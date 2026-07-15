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
	"github.com/B42Labs/dizzy/internal/chaos/glancegraph"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/glance"
	glanceexec "github.com/B42Labs/dizzy/internal/glance/executor"
	glancescenario "github.com/B42Labs/dizzy/internal/glance/scenario"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// newGlanceChaosCmd builds "glance chaos": a random churn/soak run that, for a
// configured duration, continuously creates and deletes images (each with its
// synthetic uploaded payload) and occasionally drives a live image through its
// planned lifecycle — a deactivate/reactivate cycle, visibility flips, member
// add/accept/remove, and metadata churn — bounded by the scenario as the spatial
// envelope. It authenticates, runs the churn, records the run, and — whether it
// completed or was interrupted, unless --no-cleanup — tears the images down by
// identity and reports any leak. It mirrors "nova chaos" with the image-specific
// --lifecycle-ratio.
func newGlanceChaosCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath   string
		sets           []string
		noCleanup      bool
		f              chaosFlags
		lifecycleRatio float64
	)

	cmd := &cobra.Command{
		Use:   "chaos",
		Short: "Run continuous randomized image churn within a scenario envelope",
		Long: "Run continuous randomized image churn within a scenario envelope.\n\n" +
			"Run only one dizzy process per project: a concurrent monitor's always-on pre-flight sweep reclaims " +
			"every dizzy-tagged image in the project, so this run's images would be torn down mid-flight.",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, p, err := buildGlancePlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			cfg := mergeGlanceChaosConfig(cmd, opts, s, f, lifecycleRatio)
			cfg.Classify = glancegraph.Classify
			if err := cfg.Validate(); err != nil {
				return err
			}

			// Two-phase shutdown: the first Ctrl-C / SIGTERM cancels the run so the
			// churn stops and its resources unwind instead of being killed, then the
			// teardown below tears them down. Unregistering the handler right after
			// means a second signal takes the default disposition and kills the
			// process — there is always a hard way out mid-teardown.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()

			tel, err := telemetry.Setup(ctx, telemetry.Config{
				Enabled: opts.otel, Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "glance",
			})
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)

			runID, err := newRunID()
			if err != nil {
				return err
			}

			gc, err := config.NewImageClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating image client: %w", err)
			}

			collector := metrics.NewCollector()
			client := glance.New(gc, runID, collector)
			client.SetTelemetry(tel)

			nodes, err := glancegraph.Build(p, client, p.Seed, opts.timeout)
			if err != nil {
				return fmt.Errorf("building churn graph: %w", err)
			}

			slog.Info("starting churn run", "run", runID, "scenario", p.Scenario,
				"duration", cfg.Duration, "minInterval", cfg.MinInterval, "maxInterval", cfg.MaxInterval,
				"maxParallel", cfg.MaxParallel, "lifecycleRatio", cfg.ResizeRatio, "concurrency", cfg.Concurrency)

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
			// summary metrics from the pre-teardown aggregate. An interrupted run
			// counts as a failed iteration.
			tel.RecordIteration(ctx, wall, ctx.Err() == nil)
			tel.RecordIterationOperations(ctx, agg.Overall.Attempted, agg.Overall.Succeeded, agg.Overall.Failed)

			if _, err := fmt.Fprint(cmd.OutOrStdout(), agg.Summary()); err != nil {
				return fmt.Errorf("writing metrics: %w", err)
			}

			rec := &run.Record{
				RunID:      runID,
				Service:    "glance",
				Scenario:   p.Scenario,
				Seed:       p.Seed,
				StartedAt:  start,
				FinishedAt: finished,
				Created:    result.Created,
				Metrics:    agg,
				Chaos:      chaosStats(result),
			}
			recordPath, werr := run.Write(".", rec)
			if werr != nil {
				slog.Error("writing run record failed; clean up by run id", "run", runID, "error", werr)
			} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "run record written to %s\n", recordPath); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}

			return finishGlanceChurn(ctx, cmd, client, runID, recordPath, result.Created, ctx.Err() != nil, noCleanup, opts.concurrency, opts.timeout)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.images=20 (repeatable)")
	flags.DurationVar(&f.duration, "duration", 0, "total wall-clock runtime of the churn (required via flag or the scenario chaos block)")
	flags.DurationVar(&f.minInterval, "min-interval", defaultChaosMinInterval, "minimum random delay between scheduled actions")
	flags.DurationVar(&f.maxInterval, "max-interval", defaultChaosMaxInterval, "maximum random delay between scheduled actions")
	flags.IntVar(&f.maxParallel, "max-parallel", 0, "maximum concurrent in-flight churn operations (default: --concurrency)")
	flags.Float64Var(&f.churnRatio, "churn-ratio", defaultChaosChurnRatio, "create bias at steady state, between 0 and 1")
	flags.Float64Var(&f.targetFill, "target-fill", defaultChaosTargetFill, "fraction of the envelope to keep populated on average, between 0 and 1")
	flags.Float64Var(&lifecycleRatio, "lifecycle-ratio", defaultChaosLifecycleRatio, "probability per churn step of mutating a live image (deactivate/reactivate, visibility flip, member add/remove, or metadata churn), between 0 and 1")
	flags.BoolVar(&noCleanup, "no-cleanup", false, "leave the images in place — at the end of the run or on interrupt — instead of tearing them down by identity")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// mergeGlanceChaosConfig builds the churn config from three layers, lowest
// precedence first: built-in defaults, the scenario's chaos block (each non-zero
// field), and the dedicated flags (each one explicitly set). A zero field in the
// chaos block falls back to the default; to set a field to zero use the flag —
// except lifecycle_ratio, a pointer whose explicit 0 disables mutations while an
// omitted key falls back to the default. The lifecycle ratio maps onto the
// engine's mutate probability (chaos.Config.ResizeRatio). It is the image
// counterpart of mergeNovaChaosConfig.
func mergeGlanceChaosConfig(cmd *cobra.Command, opts *globalOptions, s glancescenario.Scenario, f chaosFlags, lifecycleRatio float64) chaos.Config {
	cfg := chaos.Config{
		MinInterval: defaultChaosMinInterval,
		MaxInterval: defaultChaosMaxInterval,
		MaxParallel: opts.concurrency,
		ChurnRatio:  defaultChaosChurnRatio,
		TargetFill:  defaultChaosTargetFill,
		ResizeRatio: defaultChaosLifecycleRatio,
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
		if c.LifecycleRatio != nil {
			cfg.ResizeRatio = *c.LifecycleRatio
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
	if cmd.Flags().Changed("lifecycle-ratio") {
		cfg.ResizeRatio = lifecycleRatio
	}
	return cfg
}

// finishGlanceChurn applies the teardown policy. Unless --no-cleanup is set, the
// run — whether it completed or was interrupted — tears the images down by
// identity (each operation bounded by opTimeout) and runs a leak check; teardown
// and the leak check run on a context.WithoutCancel of ctx so a first-signal
// interrupt does not kill the teardown it triggered. With --no-cleanup the images
// are left in place and the cleanup hint is printed.
func finishGlanceChurn(ctx context.Context, cmd *cobra.Command, c glanceexec.Cleaner, runID, recordPath string, created []resource.Resource, interrupted, noCleanup bool, concurrency int, opTimeout time.Duration) error {
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
		_, err := fmt.Fprintf(out, "%s; resources left in place — reclaim with: glance cleanup %s\n", reason, hint)
		return err
	}

	tctx := context.WithoutCancel(ctx)
	cleaner := glanceTimeoutCleaner{c, opTimeout}

	deleted, cleanupErr := glanceexec.Cleanup(tctx, cleaner, runID, created, concurrency, opTimeout)
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("tearing down run %s: %w", runID, cleanupErr)
	}

	leaked, err := glanceLeakCheck(tctx, cleaner, runID)
	if err != nil {
		return err
	}
	if leaked > 0 {
		_, err = fmt.Fprintf(out, "leak check: %d run-tagged image(s) still present after teardown\n", leaked)
	} else {
		_, err = fmt.Fprintf(out, "leak check: no run-tagged images remain\n")
	}
	return err
}

// glanceLeakCheck counts the images still carrying the run's identity tag after
// teardown. It takes the glanceexec.Cleaner seam so the listing shares the
// teardown's per-op timeout bound. On a deployment configured with delayed_delete
// a just-deleted image lingers in pending_delete while still tag-listed, so it
// can be counted here even though teardown treated it as gone.
func glanceLeakCheck(ctx context.Context, c glanceexec.Cleaner, runID string) (int, error) {
	found, err := c.ListImagesByTag(ctx, runID)
	if err != nil {
		return 0, fmt.Errorf("leak check listing images: %w", err)
	}
	return len(found), nil
}
