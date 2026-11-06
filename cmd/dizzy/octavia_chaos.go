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
	"github.com/B42Labs/dizzy/internal/chaos/octaviagraph"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/octavia"
	octaviaexec "github.com/B42Labs/dizzy/internal/octavia/executor"
	octaviascenario "github.com/B42Labs/dizzy/internal/octavia/scenario"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// defaultChaosUpdateRatio is the built-in per-step probability of toggling the
// admin state of a live listener or member, used when neither the scenario
// chaos block nor the --update-ratio flag supplies one. The temporal defaults it
// joins live in chaos.go, shared with the other chaos commands.
const defaultChaosUpdateRatio = 0.3

// newOctaviaChaosCmd builds "octavia chaos": a random churn/soak run that, for
// a configured duration or until it is stopped (--duration 0), continuously
// creates and deletes VIP networks, subnets, load balancers, listeners, pools,
// members, and health monitors, one object per call and one call per load
// balancer at a time, and occasionally sets a live listener or member to
// admin_state_up=false and back to true, bounded by the scenario as the spatial
// envelope. It authenticates, pre-checks quota against the whole plan, runs the
// churn, rewrites its run record every chaosCheckpointInterval while it runs,
// records the run, and — whether it completed or was interrupted, unless
// --no-cleanup — tears the run down by its tag with the cascade delete and
// reports any leak. It mirrors "barbican chaos" with the load-balancer-specific
// --update-ratio.
func newOctaviaChaosCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		sets         []string
		noCleanup    bool
		f            chaosFlags
		updateRatio  float64
	)

	cmd := &cobra.Command{
		Use:   "chaos",
		Short: "Run continuous randomized load balancer churn with admin-state toggles within a scenario envelope",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, p, err := buildOctaviaPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			cfg := mergeOctaviaChaosConfig(cmd, opts, s, f, updateRatio)
			cfg.Classify = octaviagraph.Classify
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
				Enabled: opts.otel, Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "octavia",
			})
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)

			runID, err := newRunID()
			if err != nil {
				return err
			}

			stack, err := config.NewLoadBalancerStack(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating load balancer clients: %w", err)
			}

			// The envelope is the population's worst case, and the churn populates
			// every kind of the plan, so quota is pre-checked against the whole plan
			// exactly as apply does. The pre-check runs before the collector exists
			// and before any write, so an oversized plan leaves nothing in the cloud
			// and no run record.
			if err := octavia.PrecheckQuota(ctx, stack.LoadBalancer, p); err != nil {
				return err
			}

			collector := metrics.NewCollector()
			client := octavia.New(stack.LoadBalancer, stack.Network, runID, collector)
			client.SetTelemetry(tel)

			nodes, err := octaviagraph.Build(p, client, opts.timeout)
			if err != nil {
				return fmt.Errorf("building churn graph: %w", err)
			}

			slog.Info("starting churn run", "run", runID, "scenario", p.Scenario, "provider", p.Provider,
				"networks", len(p.Networks), "loadbalancers", len(p.LoadBalancers),
				"listeners", len(p.Listeners), "pools", len(p.Pools), "members", len(p.Members),
				"healthmonitors", len(p.HealthMonitors),
				"duration", chaosDurationLabel(cfg), "minInterval", cfg.MinInterval, "maxInterval", cfg.MaxInterval,
				"maxParallel", cfg.MaxParallel, "updateRatio", cfg.ResizeRatio, "concurrency", cfg.Concurrency)

			start := time.Now()
			// buildRecord builds the run record as of finished, for the
			// checkpoints written while the churn runs and for the final record.
			buildRecord := func(r *chaos.Result, finished time.Time) *run.Record {
				return &run.Record{
					RunID:      runID,
					Service:    "octavia",
					Scenario:   p.Scenario,
					Seed:       p.Seed,
					StartedAt:  start,
					FinishedAt: finished,
					Created:    r.Created,
					Metrics:    collector.Aggregate(finished.Sub(start)),
					Chaos:      chaosStats(r),
				}
			}
			cfg.CheckpointInterval = chaosCheckpointInterval
			cfg.OnCheckpoint = chaosCheckpoint(".", buildRecord)

			hb := startHeartbeat(ctx, "churn in progress", collectorSnapshot(collector, start, "duration", chaosDurationLabel(cfg)))
			result, runErr := chaos.Run(ctx, nodes, p.Seed, cfg, chaos.RealClock{})
			hb.stop()
			finished := time.Now()
			if runErr != nil {
				return fmt.Errorf("running churn (run %s): %w", runID, runErr)
			}
			rec := buildRecord(result, finished)

			// A churn run is a single iteration: export the same per-iteration
			// summary metrics from the pre-teardown aggregate. An interrupted run
			// counts as a failed iteration.
			interrupted := chaosInterrupted(ctx, cfg)
			tel.RecordIteration(ctx, rec.Metrics.Wall, !interrupted)
			tel.RecordIterationOperations(ctx, rec.Metrics.Overall.Attempted, rec.Metrics.Overall.Succeeded, rec.Metrics.Overall.Failed)

			if _, err := fmt.Fprint(cmd.OutOrStdout(), rec.Metrics.Summary()); err != nil {
				return fmt.Errorf("writing metrics: %w", err)
			}

			recordPath, werr := run.Write(".", rec)
			if werr != nil {
				slog.Error("writing run record failed; clean up by run id", "run", runID, "error", werr)
			} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "run record written to %s\n", recordPath); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}

			return finishOctaviaChurn(ctx, cmd, client, runID, recordPath, rec.Created, interrupted, noCleanup, opts.concurrency, opts.timeout)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.members=20 (repeatable)")
	flags.DurationVar(&f.duration, "duration", 0, "total wall-clock runtime of the churn; 0 runs until interrupted (required via flag or the scenario chaos block)")
	flags.DurationVar(&f.bucketWidth, "bucket-width", defaultChaosBucketWidth, "width of one time bucket in an unbounded run's time series, at least 1m")
	flags.DurationVar(&f.minInterval, "min-interval", defaultChaosMinInterval, "minimum random delay between scheduled actions")
	flags.DurationVar(&f.maxInterval, "max-interval", defaultChaosMaxInterval, "maximum random delay between scheduled actions")
	flags.IntVar(&f.maxParallel, "max-parallel", 0, "maximum concurrent in-flight churn operations (default: --concurrency)")
	flags.Float64Var(&f.churnRatio, "churn-ratio", defaultChaosChurnRatio, "create bias at steady state, between 0 and 1")
	flags.Float64Var(&f.targetFill, "target-fill", defaultChaosTargetFill, "fraction of the envelope to keep populated on average, between 0 and 1")
	flags.Float64Var(&updateRatio, "update-ratio", defaultChaosUpdateRatio, "probability per churn step of setting a live listener or member to admin_state_up=false and back to true, between 0 and 1")
	flags.BoolVar(&noCleanup, "no-cleanup", false, "leave the load balancers and their VIP networks in place — at the end of the run or on interrupt — instead of tearing them down by tag")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// mergeOctaviaChaosConfig builds the churn config from three layers, lowest
// precedence first: built-in defaults, the scenario's chaos block (each non-zero
// field), and the dedicated flags (each one explicitly set). A zero field in the
// chaos block falls back to the default; to set a field to zero use the flag —
// except update_ratio, a pointer whose explicit 0 disables the admin-state
// toggle while an omitted key falls back to the default. The update ratio maps
// onto the engine's mutate probability (chaos.Config.ResizeRatio). It is the
// load balancer counterpart of mergeBarbicanChaosConfig.
func mergeOctaviaChaosConfig(cmd *cobra.Command, opts *globalOptions, s octaviascenario.Scenario, f chaosFlags, updateRatio float64) chaos.Config {
	cfg := chaos.Config{
		MinInterval: defaultChaosMinInterval,
		MaxInterval: defaultChaosMaxInterval,
		MaxParallel: opts.concurrency,
		ChurnRatio:  defaultChaosChurnRatio,
		TargetFill:  defaultChaosTargetFill,
		ResizeRatio: defaultChaosUpdateRatio,
		Concurrency: opts.concurrency,
		BucketWidth: defaultChaosBucketWidth,
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
		if c.BucketWidth > 0 {
			cfg.BucketWidth = time.Duration(c.BucketWidth)
		}
		if c.UpdateRatio != nil {
			cfg.ResizeRatio = *c.UpdateRatio
		}
	}

	if cmd.Flags().Changed("duration") {
		cfg.Duration = f.duration
		cfg.Unbounded = f.duration == 0
	}
	if cmd.Flags().Changed("bucket-width") {
		cfg.BucketWidth = f.bucketWidth
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
	if cmd.Flags().Changed("update-ratio") {
		cfg.ResizeRatio = updateRatio
	}
	return cfg
}

// finishOctaviaChurn applies the teardown policy. Unless --no-cleanup is set,
// the run — whether it completed or was interrupted — tears its resources down
// by its tag (load balancers with the cascade, each operation bounded by
// opTimeout, then subnets and networks) and runs a leak check; teardown and the
// leak check run on a context.WithoutCancel of ctx so a first-signal interrupt
// does not kill the teardown it triggered. With --no-cleanup the resources are
// left in place and the cleanup hint is printed.
func finishOctaviaChurn(ctx context.Context, cmd *cobra.Command, c octaviaexec.Cleaner, runID, recordPath string, created []resource.Resource, interrupted, noCleanup bool, concurrency int, opTimeout time.Duration) error {
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
		_, err := fmt.Fprintf(out, "%s; resources left in place — reclaim with: octavia cleanup %s\n", reason, hint)
		return err
	}

	tctx := context.WithoutCancel(ctx)
	cleaner := octaviaTimeoutCleaner{c, opTimeout}

	deleted, cleanupErr := octaviaexec.Cleanup(tctx, cleaner, runID, created, concurrency, opTimeout)
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("tearing down run %s: %w", runID, cleanupErr)
	}

	leaked, err := octaviaLeakCheck(tctx, cleaner, runID)
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

// octaviaLeakCheck counts the load balancers, subnets, and networks still
// carrying the run's tag after teardown. Listeners, pools, members, and health
// monitors cannot outlive the cascade of their load balancer, so a leaked one
// shows as its load balancer. It takes the octaviaexec.Cleaner seam so the
// listings share the teardown's per-op timeout bound.
func octaviaLeakCheck(ctx context.Context, c octaviaexec.Cleaner, runID string) (int, error) {
	total := 0
	for _, kind := range []resource.Kind{octavia.KindLoadBalancer, octavia.KindSubnet, octavia.KindNetwork} {
		found, err := c.ListByTag(ctx, kind, runID)
		if err != nil {
			return total, fmt.Errorf("leak check listing %s: %w", kind, err)
		}
		total += len(found)
	}
	return total, nil
}
