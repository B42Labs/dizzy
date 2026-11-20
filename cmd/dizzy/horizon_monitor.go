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
	"github.com/B42Labs/dizzy/internal/horizon"
	horizonexec "github.com/B42Labs/dizzy/internal/horizon/executor"
	horizonplan "github.com/B42Labs/dizzy/internal/horizon/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// newHorizonMonitorCmd builds "horizon monitor": the dashboard counterpart
// of "keystone monitor". It drives the shared loop (runMonitorLoop,
// runIteration) with a Horizon runOnce: an opt-in pre-flight sweep
// (--reclaim-orphans), apply, and cleanup, continuously or on a cadence,
// exporting the same per-operation and per-iteration metrics tagged
// service=horizon. connectHorizon runs once at startup, so a wrong URL, a
// wrong password or a missing role fails before the loop, and again in
// every iteration, so a loop of days never depends on one token.
func newHorizonMonitorCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath   string
		sets           []string
		keepRunRecords bool
		reclaimOrphans bool
		cfg            monitorConfig
		horizonURL     string
	)

	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Run sweep→apply→cleanup dashboard iterations continuously or on a cadence and export metrics",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildHorizonPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}
			if err := cfg.validate(); err != nil {
				return err
			}
			base, err := parseHorizonURL(horizonURL)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()

			tel, err := setupTelemetry(ctx, opts, horizonTelemetryConfig(opts, p))
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)
			cfg.health = opts.health()

			connect := func(ctx context.Context) (horizonTarget, error) { return connectHorizon(ctx, opts, base, p) }
			if _, err := connect(ctx); err != nil {
				return err
			}

			interval := cfg.interval.String()
			if cfg.interval == 0 {
				interval = "continuous"
			}
			slog.Info("starting monitor", "scenario", p.Scenario, "interval", interval,
				"iterations", cfg.iterations, "errorWait", cfg.errorWait, "otel", opts.otel)

			runOnce := horizonMonitorRunOnce(opts, p, tel, connect, keepRunRecords, reclaimOrphans)
			iterations, failures := runMonitorLoop(ctx, cfg, chaos.RealClock{}, runOnce)
			slog.Info("monitor finished", "iterations", iterations, "failures", failures)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.users=5 (repeatable)")
	flags.DurationVar(&cfg.interval, "interval", 0, "target cadence between iteration starts, e.g. 15m; a longer iteration starts the next immediately (default 0 = continuous: iterations run back-to-back)")
	flags.IntVar(&cfg.iterations, "iterations", 0, "stop after this many iterations (0 = run forever)")
	flags.DurationVar(&cfg.errorWait, "error-wait", 0, "extra pause after a failed iteration before the next starts (0 = off)")
	flags.BoolVar(&keepRunRecords, "keep-run-records", false, "write a run-<id>.json per iteration (off by default: in monitor mode they accumulate unboundedly)")
	flags.BoolVar(&reclaimOrphans, "reclaim-orphans", false, "before each iteration of a plan with users, delete the dizzyhorizon- users and projects of every run on the cloud (off by default); safe only when no other dizzy horizon process uses that cloud")
	flags.StringVar(&horizonURL, "horizon-url", "", horizonURLFlagHelp)
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// horizonMonitorRunOnce builds the per-iteration closure the loop drives.
// Each iteration takes a fresh run id, a fresh collector and a fresh
// connect, whose failure fails the iteration and not the loop. It then runs
// the iteration's three phases, records the per-iteration metrics, writes
// the run record with keepRunRecords, and logs a one-line summary.
func horizonMonitorRunOnce(opts *globalOptions, p *horizonplan.Plan, tel *telemetry.Telemetry, connect func(context.Context) (horizonTarget, error), keepRunRecords, reclaimOrphans bool) func(ctx context.Context, iter int) bool {
	return func(ctx context.Context, iter int) bool {
		runID, err := newRunID()
		if err != nil {
			slog.Error("generating run id failed; skipping iteration", "iteration", iter, "error", err)
			return false
		}
		target, err := connect(ctx)
		if err != nil {
			slog.Error("iteration authentication failed", "iteration", iter, "run", runID, "error", err)
			return false
		}
		collector := metrics.NewCollector()
		client := target.client(runID, collector, tel)

		start := time.Now()
		hb := startHeartbeat(ctx, "monitor iteration in progress",
			collectorSnapshot(collector, start, "iteration", iter, "run", runID))
		res := runIteration(ctx, horizonIterationDeps(target, client, p, runID, opts.concurrency, opts.timeout, reclaimOrphans))
		hb.stop()

		wall := time.Since(start)
		agg := collector.Aggregate(wall)
		tel.RecordIteration(context.WithoutCancel(ctx), wall, res.ok)
		tel.RecordIterationOperations(context.WithoutCancel(ctx),
			agg.Overall.Attempted, agg.Overall.Succeeded, agg.Overall.Failed)

		if keepRunRecords {
			writeIterationRecord(horizonRunRecord(runID, p, start, start.Add(wall), res.created, agg, res.err))
		}

		attrs := []any{
			"iteration", iter, "run", runID,
			"duration", wall.Round(time.Millisecond),
			"ok", res.ok, "ops", agg.Overall.Attempted, "failed", agg.Overall.Failed,
			"swept", res.swept, "deleted", res.deleted,
		}
		if res.err != nil {
			attrs = append(attrs, "error", res.err.Error())
		}
		slog.Info("iteration complete", attrs...)
		return res.ok
	}
}

// horizonIterationDeps returns one iteration's three phases. The pre-flight
// sweep runs only with reclaimOrphans and a plan with users: it deletes the
// dizzyhorizon- users and projects of every run through
// horizonOrphanCleaner. apply runs horizonexec.Apply. cleanup deletes the
// iteration's own users and projects; a plan without users created nothing,
// so it sends nothing.
func horizonIterationDeps(target horizonTarget, client *horizon.Client, p *horizonplan.Plan, runID string, concurrency int, opTimeout time.Duration, reclaimOrphans bool) iterationDeps {
	withUsers := len(p.Users) > 0
	return iterationDeps{
		preflight: func(ctx context.Context) (int, error) {
			if !reclaimOrphans || !withUsers {
				return 0, nil
			}
			// The fresh run id satisfies Cleanup's non-empty guard; the adapter
			// ignores it and lists every run's users and projects.
			return horizonexec.Cleanup(ctx, horizonOrphanCleaner{client}, runID, nil, concurrency, opTimeout)
		},
		apply: func(ctx context.Context) ([]resource.Resource, error) {
			res, err := horizonexec.Apply(ctx, target.execTarget(client), p, concurrency, opTimeout)
			return res.Created, err
		},
		cleanup: func(ctx context.Context, created []resource.Resource) (int, error) {
			if !withUsers {
				return 0, nil
			}
			return horizonexec.Cleanup(ctx, client, runID, created, concurrency, opTimeout)
		},
	}
}

// horizonOrphanCleaner adapts a Horizon client to the horizonexec.Cleaner
// seam for the pre-flight sweep: its listings find the users and projects of
// every Horizon run, not one run's. Delete promotes from the client.
type horizonOrphanCleaner struct{ *horizon.Client }

// ListUsersByRun ignores the run id and lists every dizzyhorizon- user.
func (o horizonOrphanCleaner) ListUsersByRun(ctx context.Context, _ string) ([]resource.Resource, error) {
	return o.ListUsersByAnyRun(ctx)
}

// ListProjectsByRun ignores the run id and lists every project tagged
// dizzyhorizon:type=project.
func (o horizonOrphanCleaner) ListProjectsByRun(ctx context.Context, _ string) ([]resource.Resource, error) {
	return o.ListProjectsByAnyRun(ctx)
}
