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

	"github.com/B42Labs/dizzy/internal/c5c3"
	c5c3exec "github.com/B42Labs/dizzy/internal/c5c3/executor"
	c5c3plan "github.com/B42Labs/dizzy/internal/c5c3/plan"
	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// newC5C3MonitorCmd builds "c5c3 monitor": the order counterpart to the other
// monitors. It drives the same service-agnostic loop (runMonitorLoop,
// runIteration) with a c5c3 runOnce that composes the single-shot pipeline,
// pre-flight sweep of every dizzy order in the namespace → pre-check and apply
// → cleanup of the iteration's run, continuously (the default) or on a fixed
// cadence, unattended, for days or weeks, exporting the same per-operation and
// per-iteration metrics (tagged service=c5c3). The Keystone endpoint override
// and the Kubernetes client are resolved once at startup, where the pre-check
// also runs once, so a missing clouds.yaml, a missing CRD or a missing right
// fails before the loop begins. A refusal, a mismatch or an accepted
// credential fails its iteration like any other apply error; the loop
// survives it, as it survives every individual iteration failure.
func newC5C3MonitorCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath   string
		sets           []string
		keepRunRecords bool
		kube           c5c3KubeFlags
		cfg            monitorConfig
	)

	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Run sweep→apply→cleanup order iterations continuously or on a cadence and export metrics",
		Long: "Run sweep→apply→cleanup order iterations continuously or on a cadence and export metrics.\n\n" +
			"Run only one dizzy process per Kubernetes namespace: the pre-flight sweep is always on and, before " +
			"every iteration, deletes every order in the namespace that carries the dizzy/type label, whatever " +
			"run created it, so a concurrent apply or monitor in the same namespace (including orders an earlier " +
			"apply left in place) is torn down mid-flight.",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := buildC5C3PlanFromFlags(scenarioPath, sets)
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

			tel, err := setupTelemetry(ctx, opts, c5c3TelemetryConfig(opts, p))
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)
			cfg.health = opts.health()

			// The Keystone endpoint and the Kubernetes client are resolved once:
			// neither holds a token that could expire over a multi-day loop. The
			// startup pre-check records into a collector of its own and exports
			// nothing, so the iterations' metrics hold only their own requests.
			// Its client bounds each of its lists by --timeout.
			cluster, err := connectC5C3(opts, kube)
			if err != nil {
				return err
			}
			if err := cluster.client(p.Namespace, "", metrics.NewCollector(), nil).Precheck(ctx); err != nil {
				return err
			}

			// 0 means continuous; log it as such so the startup line explains itself.
			interval := cfg.interval.String()
			if cfg.interval == 0 {
				interval = "continuous"
			}
			slog.Info("starting monitor", "scenario", p.Scenario, "namespace", p.Namespace, "interval", interval,
				"iterations", cfg.iterations, "errorWait", cfg.errorWait, "otel", opts.otel)

			runOnce := c5c3MonitorRunOnce(opts, cluster, p, tel, keepRunRecords)

			iterations, failures := runMonitorLoop(ctx, cfg, chaos.RealClock{}, runOnce)
			slog.Info("monitor finished", "iterations", iterations, "failures", failures)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set role=reader (repeatable)")
	flags.DurationVar(&cfg.interval, "interval", 0, "target cadence between iteration starts, e.g. 15m; a longer iteration starts the next immediately (default 0 = continuous: iterations run back-to-back)")
	flags.IntVar(&cfg.iterations, "iterations", 0, "stop after this many iterations (0 = run forever)")
	flags.DurationVar(&cfg.errorWait, "error-wait", 0, "extra pause after a failed iteration before the next starts (0 = off)")
	flags.BoolVar(&keepRunRecords, "keep-run-records", false, "write a run-<id>.json per iteration (off by default: in monitor mode they accumulate unboundedly)")
	kube.register(flags)
	// MarkFlagRequired only fails for an unknown flag; scenario was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// c5c3MonitorRunOnce builds the production per-iteration closure the loop
// drives. Each iteration gets a fresh run id, a fresh metrics collector, and
// the clients c5c3IterationDeps builds over the cluster resolved at startup;
// it runs the pre-flight sweep, the pre-check and the apply, and the cleanup,
// then records the per-iteration summary metrics and logs a one-line summary.
func c5c3MonitorRunOnce(opts *globalOptions, cluster c5c3Cluster, p *c5c3plan.Plan, tel *telemetry.Telemetry, keepRunRecords bool) func(ctx context.Context, iter int) bool {
	return func(ctx context.Context, iter int) bool {
		runID, err := newRunID()
		if err != nil {
			slog.Error("generating run id failed; skipping iteration", "iteration", iter, "error", err)
			return false
		}
		collector := metrics.NewCollector()

		start := time.Now()
		hb := startHeartbeat(ctx, "monitor iteration in progress",
			collectorSnapshot(collector, start, "iteration", iter, "run", runID))
		res := runIteration(ctx, c5c3IterationDeps(cluster, p, runID, collector, tel, opts.concurrency, opts.timeout))
		hb.stop()

		wall := time.Since(start)
		agg := collector.Aggregate(wall)
		// Record on a context that survives a first-signal cancel so the final
		// iteration's metrics still make it into the export.
		tel.RecordIteration(context.WithoutCancel(ctx), wall, res.ok)
		tel.RecordIterationOperations(context.WithoutCancel(ctx),
			agg.Overall.Attempted, agg.Overall.Succeeded, agg.Overall.Failed)

		if keepRunRecords {
			writeIterationRecord(c5c3RunRecord(runID, p, start, start.Add(wall), res.created, agg, res.err))
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

// c5c3IterationDeps builds the three phases of the iteration that runs p as
// runID over cluster, and their two clients, both in p's namespace, bound to
// runID and recording into collector and tel. The pre-flight sweep runs
// c5c3exec.Cleanup over every order in the namespace that carries the
// dizzy/type label, whatever its run; apply runs the pre-check on the
// iteration's client before anything is created, since c5c3exec.Apply does
// not run it, and then the plan; cleanup deletes this run's orders by their
// dizzy/run label, unioned with what apply created. The sweep and the cleanup
// bound their waits for the operator by the plan's ReadyTimeout and
// GoneTimeout. They are built here rather than inline in the runOnce closure
// so a test can drive them against a fake cluster.
//
// The apply and the cleanup share the iteration's client, which carries p.
// The sweep has a client of its own: a client stops sending password logins
// once Keystone has rejected five of them, and the sweep sends one login that
// Keystone must reject for every user order of another run it deletes. On a
// shared client, a sweep that reclaims five leftover users would leave the
// iteration no password login of its own.
func c5c3IterationDeps(cluster c5c3Cluster, p *c5c3plan.Plan, runID string, collector *metrics.Collector, tel *telemetry.Telemetry, concurrency int, opTimeout time.Duration) iterationDeps {
	sweep := cluster.client(p.Namespace, runID, collector, tel)
	client := cluster.client(p.Namespace, runID, collector, tel)
	client.SetPlan(p)
	return iterationDeps{
		preflight: func(ctx context.Context) (int, error) {
			// The fresh run id satisfies Cleanup's non-empty guard; the orphan
			// adapter ignores it and discovers by the dizzy/type label.
			return c5c3exec.Cleanup(ctx, c5c3OrphanCleaner{sweep}, runID, nil, concurrency, opTimeout, p.ReadyTimeout, p.GoneTimeout)
		},
		apply: func(ctx context.Context) ([]resource.Resource, error) {
			if err := client.Precheck(ctx); err != nil {
				return nil, err
			}
			r, err := c5c3exec.Apply(ctx, client, p, concurrency, opTimeout)
			return r.Created, err
		},
		cleanup: func(ctx context.Context, created []resource.Resource) (int, error) {
			return c5c3exec.Cleanup(ctx, client, runID, created, concurrency, opTimeout, p.ReadyTimeout, p.GoneTimeout)
		},
	}
}

// c5c3OrphanCleaner adapts a c5c3 client to the c5c3exec.Cleaner seam for the
// pre-flight sweep, discovering every order in the client's namespace that
// carries the dizzy/type label, whatever run created it, instead of one run's
// dizzy/run label. The sweep thus reuses c5c3exec.Cleanup's six steps
// unchanged, deletion order and credential checks included, over the orders
// of any killed iteration whose run id is lost. Every other method promotes
// from the embedded client.
type c5c3OrphanCleaner struct{ *c5c3.Client }

// The sweep adapter must satisfy the same seam as the client it wraps.
var _ c5c3exec.Cleaner = c5c3OrphanCleaner{}

// ListByRun ignores the run id and lists the orders of kind by the presence
// of the dizzy/type label.
func (o c5c3OrphanCleaner) ListByRun(ctx context.Context, kind resource.Kind, _ string) ([]resource.Resource, error) {
	return o.ListByAnyRun(ctx, kind)
}
