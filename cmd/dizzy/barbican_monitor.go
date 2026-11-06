package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/barbican"
	barbicanexec "github.com/B42Labs/dizzy/internal/barbican/executor"
	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// newBarbicanMonitorCmd builds "barbican monitor": the key manager counterpart
// to the other monitors. It drives the same service-agnostic loop
// (runMonitorLoop, runIteration) with a Barbican runOnce that composes the
// single-shot pipeline — identity-based pre-flight sweep → quota pre-check and
// apply → cleanup — continuously (the default) or on a fixed cadence,
// unattended, for days or weeks, exporting the same per-operation and
// per-iteration metrics (via --otel, tagged service=barbican) so a single
// installation's secret, order, and read-back latencies and error rates become
// observable over time. The quota pre-check runs once at startup, so an
// oversized plan fails before the loop begins, and again in every iteration's
// apply. A read-back mismatch fails its iteration like any other apply error;
// the loop survives it, as it survives every individual iteration failure.
func newBarbicanMonitorCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath   string
		sets           []string
		keepRunRecords bool
		cfg            monitorConfig
	)

	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Run sweep→apply→cleanup secret iterations continuously or on a cadence and export metrics",
		Long: "Run sweep→apply→cleanup secret iterations continuously or on a cadence and export metrics.\n\n" +
			"Run only one dizzy process per project: the pre-flight sweep is always on and, before every " +
			"iteration, reclaims every dizzy- container and order in the project, the secrets those orders " +
			"generated, and every dizzy- secret stamped dizzy_type=secret, so a concurrent apply, chaos, or " +
			"monitor in the same project (including a fleet an earlier apply left in place) is torn down mid-flight.",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildBarbicanPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}
			if err := cfg.validate(); err != nil {
				return err
			}

			// Two-phase shutdown: the first SIGINT/SIGTERM cancels the loop for a
			// graceful stop; a second signal takes the default disposition and kills
			// the process.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()

			tel, err := telemetry.Setup(ctx, telemetry.Config{
				Enabled: opts.otel, Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "barbican",
			})
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)

			// One startup client pre-checks quota so an oversized plan or an
			// unreadable quota fails fast; each iteration authenticates its own
			// client (see the runOnce closure) so a multi-day loop is not at the
			// mercy of token expiry and an unhealthy Keystone fails one iteration
			// rather than dead-looping.
			kv, err := config.NewKeyManagerClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating key manager client: %w", err)
			}
			if err := barbican.PrecheckQuota(ctx, kv, p); err != nil {
				return err
			}

			// 0 means continuous; log it as such so the startup line explains itself.
			interval := cfg.interval.String()
			if cfg.interval == 0 {
				interval = "continuous"
			}
			slog.Info("starting monitor", "scenario", p.Scenario, "interval", interval,
				"iterations", cfg.iterations, "errorWait", cfg.errorWait, "otel", opts.otel)

			runOnce := barbicanMonitorRunOnce(opts, p, tel, keepRunRecords)

			iterations, failures := runMonitorLoop(ctx, cfg, chaos.RealClock{}, runOnce)
			slog.Info("monitor finished", "iterations", iterations, "failures", failures)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.secrets=20 (repeatable)")
	flags.DurationVar(&cfg.interval, "interval", 0, "target cadence between iteration starts, e.g. 15m; a longer iteration starts the next immediately (default 0 = continuous: iterations run back-to-back)")
	flags.IntVar(&cfg.iterations, "iterations", 0, "stop after this many iterations (0 = run forever)")
	flags.DurationVar(&cfg.errorWait, "error-wait", 0, "extra pause after a failed iteration before the next starts (0 = off)")
	flags.BoolVar(&keepRunRecords, "keep-run-records", false, "write a run-<id>.json per iteration (off by default: in monitor mode they accumulate unboundedly)")
	// MarkFlagRequired only fails for an unknown flag; scenario was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// barbicanMonitorRunOnce builds the production per-iteration closure the loop
// drives. Each iteration gets a fresh run id, a fresh metrics collector, and
// its own authenticated client; it runs the identity-based pre-flight sweep
// (reclaiming any tester leftovers by the any-run name prefix), pre-checks
// quota and applies the plan, and cleans up, then records the per-iteration
// summary metrics and logs a one-line summary.
func barbicanMonitorRunOnce(opts *globalOptions, p *barbicanplan.Plan, tel *telemetry.Telemetry, keepRunRecords bool) func(ctx context.Context, iter int) bool {
	return func(ctx context.Context, iter int) bool {
		runID, err := newRunID()
		if err != nil {
			slog.Error("generating run id failed; skipping iteration", "iteration", iter, "error", err)
			return false
		}
		// A fresh Keystone auth per iteration sidesteps token expiry over a
		// multi-day loop and turns an unhealthy Keystone into a failed iteration
		// rather than a dead loop.
		kv, err := config.NewKeyManagerClient(ctx, opts.osCloud)
		if err != nil {
			slog.Error("iteration authentication failed", "iteration", iter, "run", runID, "error", err)
			return false
		}
		collector := metrics.NewCollector()
		// The sweep and the cleanup discover secrets through this client, so it
		// is built with a request timeout, as barbican cleanup's is.
		client := newBarbicanClient(kv, runID, collector, opts.timeout)
		client.SetTelemetry(tel)

		start := time.Now()
		hb := startHeartbeat(ctx, "monitor iteration in progress",
			collectorSnapshot(collector, start, "iteration", iter, "run", runID))
		res := runIteration(ctx, barbicanIterationDeps(kv, client, p, runID, opts.concurrency, opts.timeout))
		hb.stop()

		wall := time.Since(start)
		agg := collector.Aggregate(wall)
		// Record on a context that survives a first-signal cancel so the final
		// iteration's metrics still make it into the export.
		tel.RecordIteration(context.WithoutCancel(ctx), wall, res.ok)
		tel.RecordIterationOperations(context.WithoutCancel(ctx),
			agg.Overall.Attempted, agg.Overall.Succeeded, agg.Overall.Failed)

		if keepRunRecords {
			rec := &run.Record{
				RunID:      runID,
				Service:    "barbican",
				Scenario:   p.Scenario,
				Seed:       p.Seed,
				StartedAt:  start,
				FinishedAt: start.Add(wall),
				Created:    res.created,
				Metrics:    agg,
			}
			if res.err != nil {
				rec.Error = res.err.Error()
			}
			writeIterationRecord(rec)
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

// barbicanIterationDeps builds one iteration's three phases over kv and client,
// both bound to runID. The pre-flight sweep tears down every tester resource
// the any-run identity finds; apply runs the quota pre-check on this
// iteration's own client before anything is created, since barbicanexec.Apply
// does not run it, and then the plan; cleanup tears down this run by identity,
// unioned with what apply created. They are built here rather than inline in
// the runOnce closure so a test can drive them against a fake key manager.
func barbicanIterationDeps(kv *gophercloud.ServiceClient, client *barbican.Client, p *barbicanplan.Plan, runID string, concurrency int, opTimeout time.Duration) iterationDeps {
	return iterationDeps{
		preflight: func(ctx context.Context) (int, error) {
			// The fresh run id satisfies Cleanup's non-empty guard; the orphan
			// adapter ignores it and discovers by the any-run identity.
			return barbicanexec.Cleanup(ctx, barbicanTimeoutCleaner{barbicanOrphanCleaner{client}, opTimeout}, runID, nil, concurrency)
		},
		apply: func(ctx context.Context) ([]resource.Resource, error) {
			if err := barbican.PrecheckQuota(ctx, kv, p); err != nil {
				return nil, err
			}
			r, err := barbicanexec.Apply(ctx, client, p, concurrency, opTimeout, p.Seed)
			return r.Created, err
		},
		cleanup: func(ctx context.Context, created []resource.Resource) (int, error) {
			return barbicanexec.Cleanup(ctx, barbicanTimeoutCleaner{client, opTimeout}, runID, created, concurrency)
		},
	}
}

// barbicanOrphanCleaner adapts a Barbican client to the barbicanexec.Cleaner
// seam for the pre-flight sweep, discovering leftovers by the any-run identity
// instead of one run's: containers and orders by the bare dizzy- name prefix,
// the secrets those orders generated through their secret_ref, and any other
// secret only once its dizzy_type=secret metadata confirms the prefix. It is the
// key manager analog of the other services' orphan cleaners, so the sweep
// reuses barbicanexec.Cleanup's exact containers, orders, secrets ordering
// unchanged. Delete promotes from the embedded client.
type barbicanOrphanCleaner struct{ *barbican.Client }

// The sweep adapter must satisfy the same seam as the client it wraps.
var _ barbicanexec.Cleaner = barbicanOrphanCleaner{}

// ListContainersByRun ignores the run id and lists by the any-run name prefix,
// so the sweep reclaims containers from any previous crashed or interrupted
// iteration whose run id is no longer known.
func (o barbicanOrphanCleaner) ListContainersByRun(ctx context.Context, _ string) ([]resource.Resource, error) {
	return o.ListContainersByAnyRun(ctx)
}

// ListOrdersByRun ignores the run id and lists by the any-run name prefix on
// meta.name, returning each kept order's generated secret alongside.
func (o barbicanOrphanCleaner) ListOrdersByRun(ctx context.Context, _ string) (orders, secrets []resource.Resource, err error) {
	return o.ListOrdersByAnyRun(ctx)
}

// ListSecretsByRun ignores the run id and lists the prefixed secrets whose
// dizzy_type=secret metadata confirms them, whatever run stamped them.
func (o barbicanOrphanCleaner) ListSecretsByRun(ctx context.Context, _ string) ([]resource.Resource, error) {
	return o.ListSecretsByType(ctx)
}
