package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/chaos/cindergraph"
	"github.com/B42Labs/dizzy/internal/cinder"
	cinderexec "github.com/B42Labs/dizzy/internal/cinder/executor"
	cinderscenario "github.com/B42Labs/dizzy/internal/cinder/scenario"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// defaultChaosResizeRatio is the built-in per-step probability of extending a
// live, not-yet-resized volume, used when neither the scenario chaos block nor
// the --resize-ratio flag supplies one. The temporal defaults it joins live in
// chaos.go, shared with the neutron chaos command.
const defaultChaosResizeRatio = 0.3

// newCinderChaosCmd builds "cinder chaos": a random churn/soak run that, for a
// configured duration, continuously creates and deletes volumes and snapshots
// and occasionally extends a live volume to its planned target, bounded by the
// scenario as the spatial envelope. It authenticates, resolves the volume type,
// pre-checks quota against the full plan, runs the churn, records the run, and —
// whether it completed or was interrupted, unless --no-cleanup — tears the
// resources down by metadata and reports any leak. It mirrors "neutron chaos"
// plus --volume-type and the block-storage-specific --resize-ratio.
func newCinderChaosCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		sets         []string
		volumeType   string
		noCleanup    bool
		f            chaosFlags
		resizeRatio  float64
	)

	cmd := &cobra.Command{
		Use:   "chaos",
		Short: "Run continuous randomized volume/snapshot churn within a scenario envelope",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, p, err := buildCinderPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			cfg := mergeCinderChaosConfig(cmd, opts, s, f, resizeRatio)
			cfg.Classify = cindergraph.Classify
			if err := cfg.Validate(); err != nil {
				return err
			}

			// Two-phase shutdown: the first Ctrl-C / SIGTERM cancels the run so the
			// churn stops and its resources unwind instead of being killed, then the
			// teardown below tears the volumes and snapshots down. Unregistering the
			// handler right after means a second signal takes the default disposition
			// and kills the process — there is always a hard way out mid-teardown.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()

			// Set up OTEL export (a no-op unless --otel is set) and flush it on
			// exit so an ad-hoc churn run lands in the same database as monitor.
			tel, err := telemetry.Setup(ctx, telemetry.Config{
				Enabled: opts.otel, Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "cinder",
			})
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)

			runID, err := newRunID()
			if err != nil {
				return err
			}

			gc, err := config.NewBlockStorageClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating block storage client: %w", err)
			}

			// The volume type is a property of the target cloud, not of the
			// cloud-independent plan: resolve it and normalize to the resolved name
			// so the per-type quota keys and the run record use it. A named type
			// that does not exist is an error; unset means the cloud's default type.
			if volumeType != "" {
				vt, err := cinder.FindVolumeType(ctx, gc, volumeType)
				if err != nil {
					return err
				}
				volumeType = vt.Name
				slog.Info("using volume type", "name", vt.Name, "id", vt.ID)
			}

			// The envelope is the population's worst case, so quota is pre-checked
			// against the full plan exactly as apply does.
			if err := cinder.PrecheckQuota(ctx, gc, p, volumeType); err != nil {
				return err
			}

			collector := metrics.NewCollector()
			client := cinder.New(gc, runID, collector)
			client.SetTelemetry(tel)

			nodes, err := cindergraph.Build(p, client, volumeType, opts.timeout)
			if err != nil {
				return fmt.Errorf("building churn graph: %w", err)
			}

			slog.Info("starting churn run", "run", runID, "scenario", p.Scenario,
				"duration", cfg.Duration, "minInterval", cfg.MinInterval, "maxInterval", cfg.MaxInterval,
				"maxParallel", cfg.MaxParallel, "resizeRatio", cfg.ResizeRatio, "concurrency", cfg.Concurrency)

			start := time.Now()
			hb := startHeartbeat(ctx, "churn in progress", collectorSnapshot(collector, start, "duration", cfg.Duration))
			result, runErr := chaos.Run(ctx, nodes, p.Seed, cfg, chaos.RealClock{})
			hb.stop()
			finished := time.Now()
			if runErr != nil {
				return fmt.Errorf("running churn (run %s): %w", runID, runErr)
			}
			wall := finished.Sub(start)
			agg := collector.Aggregate(wall)

			// A churn run is a single iteration: export the same per-iteration
			// summary metrics from the pre-teardown aggregate, mirroring the run
			// record. An interrupted run counts as a failed iteration.
			tel.RecordIteration(ctx, wall, ctx.Err() == nil)
			tel.RecordIterationOperations(ctx, agg.Overall.Attempted, agg.Overall.Succeeded, agg.Overall.Failed)

			if _, err := fmt.Fprint(cmd.OutOrStdout(), agg.Summary()); err != nil {
				return fmt.Errorf("writing metrics: %w", err)
			}

			// Persist the run record before teardown so the resources still live
			// stay reclaimable by metadata even if teardown fails partway or the
			// operator wants to inspect them.
			rec := &run.Record{
				RunID:      runID,
				Service:    "cinder",
				Scenario:   p.Scenario,
				Seed:       p.Seed,
				StartedAt:  start,
				FinishedAt: finished,
				Created:    result.Created,
				Metrics:    agg,
				VolumeType: volumeType,
				Chaos:      chaosStats(result),
			}
			recordPath, werr := run.Write(".", rec)
			if werr != nil {
				slog.Error("writing run record failed; clean up by run id", "run", runID, "error", werr)
			} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "run record written to %s\n", recordPath); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}

			return finishCinderChurn(ctx, cmd, client, runID, recordPath, result.Created, ctx.Err() != nil, noCleanup, opts.timeout)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.volumes=20 (repeatable)")
	flags.DurationVar(&f.duration, "duration", 0, "total wall-clock runtime of the churn (required via flag or the scenario chaos block)")
	flags.DurationVar(&f.minInterval, "min-interval", defaultChaosMinInterval, "minimum random delay between scheduled actions")
	flags.DurationVar(&f.maxInterval, "max-interval", defaultChaosMaxInterval, "maximum random delay between scheduled actions")
	flags.IntVar(&f.maxParallel, "max-parallel", 0, "maximum concurrent in-flight churn operations (default: --concurrency)")
	flags.Float64Var(&f.churnRatio, "churn-ratio", defaultChaosChurnRatio, "create bias at steady state, between 0 and 1")
	flags.Float64Var(&f.targetFill, "target-fill", defaultChaosTargetFill, "fraction of the envelope to keep populated on average, between 0 and 1")
	flags.Float64Var(&resizeRatio, "resize-ratio", defaultChaosResizeRatio, "probability per churn step of extending a live, not-yet-resized volume to its planned target, between 0 and 1")
	flags.BoolVar(&noCleanup, "no-cleanup", false, "leave the volumes and snapshots in place — at the end of the run or on interrupt — instead of tearing them down by metadata")
	flags.StringVar(&volumeType, "volume-type", "", "name of the Cinder volume type to create volumes with (default: the cloud's default type)")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// mergeCinderChaosConfig builds the churn config from three layers, lowest
// precedence first: built-in defaults, the scenario's chaos block (each non-zero
// field), and the dedicated flags (each one explicitly set). A zero field in the
// chaos block falls back to the default; to set a field to zero use the flag —
// except resize_ratio, a pointer whose explicit 0 disables extends while an
// omitted key falls back to the default. It is the cinder counterpart of
// mergeChaosConfig, adding the resize_ratio knob.
func mergeCinderChaosConfig(cmd *cobra.Command, opts *globalOptions, s cinderscenario.Scenario, f chaosFlags, resizeRatio float64) chaos.Config {
	cfg := chaos.Config{
		MinInterval: defaultChaosMinInterval,
		MaxInterval: defaultChaosMaxInterval,
		MaxParallel: opts.concurrency,
		ChurnRatio:  defaultChaosChurnRatio,
		TargetFill:  defaultChaosTargetFill,
		ResizeRatio: defaultChaosResizeRatio,
		Concurrency: opts.concurrency,
	}

	if c := s.Chaos; c != nil {
		if c.Duration > 0 {
			cfg.Duration = time.Duration(c.Duration)
		}
		if c.Interval.Min > 0 {
			cfg.MinInterval = time.Duration(c.Interval.Min)
		}
		if c.Interval.Max > 0 {
			cfg.MaxInterval = time.Duration(c.Interval.Max)
		}
		if c.Parallel.Max > 0 {
			cfg.MaxParallel = c.Parallel.Max
		}
		if c.ChurnRatio > 0 {
			cfg.ChurnRatio = c.ChurnRatio
		}
		if c.TargetFill > 0 {
			cfg.TargetFill = c.TargetFill
		}
		if c.ResizeRatio != nil {
			cfg.ResizeRatio = *c.ResizeRatio
		}
	}

	if cmd.Flags().Changed("duration") {
		cfg.Duration = f.duration
	}
	if cmd.Flags().Changed("min-interval") {
		cfg.MinInterval = f.minInterval
	}
	if cmd.Flags().Changed("max-interval") {
		cfg.MaxInterval = f.maxInterval
	}
	if cmd.Flags().Changed("max-parallel") {
		cfg.MaxParallel = f.maxParallel
	}
	if cmd.Flags().Changed("churn-ratio") {
		cfg.ChurnRatio = f.churnRatio
	}
	if cmd.Flags().Changed("target-fill") {
		cfg.TargetFill = f.targetFill
	}
	if cmd.Flags().Changed("resize-ratio") {
		cfg.ResizeRatio = resizeRatio
	}
	return cfg
}

// finishCinderChurn applies the teardown policy. Unless --no-cleanup is set, the
// run — whether it completed or was interrupted — tears the volumes and
// snapshots down by metadata (snapshots before volumes, each operation bounded
// by opTimeout) and runs a leak check, exactly as a completed run always has;
// teardown and the leak check run on a context.WithoutCancel of ctx so a
// first-signal interrupt does not kill the teardown it triggered. With
// --no-cleanup the resources are left in place and the cleanup hint is printed —
// interrupt-to-inspect is now this explicit opt-out.
func finishCinderChurn(ctx context.Context, cmd *cobra.Command, c cinderexec.Cleaner, runID, recordPath string, created []resource.Resource, interrupted, noCleanup bool, opTimeout time.Duration) error {
	out := cmd.OutOrStdout()
	if noCleanup {
		reason := "churn complete"
		if interrupted {
			reason = "churn interrupted"
		}
		hint := "--run-id " + runID
		if recordPath != "" {
			hint = "--run " + recordPath
		}
		_, err := fmt.Fprintf(out, "%s; resources left in place — reclaim with: cinder cleanup %s\n", reason, hint)
		return err
	}

	// The signal context is already cancelled on interrupt; run teardown on a
	// context.WithoutCancel with a per-op bound so the first signal triggers it
	// and a second signal (the hard abort) is the only thing that kills it.
	tctx := context.WithoutCancel(ctx)
	cleaner := cinderTimeoutCleaner{c, opTimeout}

	deleted, cleanupErr := cinderexec.Cleanup(tctx, cleaner, runID, created, opTimeout)
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("tearing down run %s: %w", runID, cleanupErr)
	}

	leaked, err := cinderLeakCheck(tctx, cleaner, runID)
	if err != nil {
		return err
	}
	if leaked > 0 {
		_, err = fmt.Fprintf(out, "leak check: %d run-tagged resource(s) still present after teardown\n", leaked)
	} else {
		_, err = fmt.Fprintf(out, "leak check: no run-tagged resources remain\n")
	}
	return err
}

// cinderLeakCheck counts the volumes and snapshots still carrying the run's
// dizzy:run metadata after teardown. It takes the cinderexec.Cleaner seam so
// the listing shares the teardown's per-op timeout bound.
func cinderLeakCheck(ctx context.Context, c cinderexec.Cleaner, runID string) (int, error) {
	vols, err := c.ListVolumesByMetadata(ctx, runID)
	if err != nil {
		return 0, fmt.Errorf("leak check listing volumes: %w", err)
	}
	snaps, err := c.ListSnapshotsByMetadata(ctx, runID)
	if err != nil {
		return len(vols), fmt.Errorf("leak check listing snapshots: %w", err)
	}
	return len(vols) + len(snaps), nil
}
