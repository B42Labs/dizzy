package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/octavia"
	octaviaexec "github.com/B42Labs/dizzy/internal/octavia/executor"
	octaviaplan "github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// errOctaviaMonitorNoProject is returned when the startup auth result names no
// project. The pre-flight sweep is scoped to the token's project, so a monitor
// without one could only sweep unscoped, which it never does.
var errOctaviaMonitorNoProject = errors.New("octavia monitor needs a project-scoped credential: the auth result carries no project id, and the pre-flight sweep never lists beyond the token's project")

// newOctaviaMonitorCmd builds "octavia monitor": the load balancer counterpart
// to the other monitors. It drives the same service-agnostic loop
// (runMonitorLoop, runIteration) with an Octavia runOnce that composes the
// single-shot pipeline — project-scoped pre-flight sweep → quota pre-check and
// apply → cleanup — continuously (the default) or on a fixed cadence,
// unattended, for days or weeks, exporting the same per-operation and
// per-iteration metrics (via --otel, tagged service=octavia) so a single
// installation's load balancer latencies, time-to-ready, and error rates
// become observable over time. Octavia and Neutron list every project's
// objects for an administrator, so the sweep lists by type tag within the
// token's project only, and the command refuses to start when the auth result
// names no project. The quota pre-check runs once at startup, so an oversized
// plan fails before the loop begins, and again in every iteration's apply.
func newOctaviaMonitorCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath   string
		sets           []string
		keepRunRecords bool
		cfg            monitorConfig
	)

	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Run sweep→apply→cleanup load balancer iterations continuously or on a cadence and export metrics",
		Long: "Run sweep→apply→cleanup load balancer iterations continuously or on a cadence and export metrics.\n\n" +
			"Run only one dizzy process per project: the pre-flight sweep is always on and, before every " +
			"iteration, deletes every load balancer tagged dizzy:type=loadbalancer in the token's project with " +
			"cascade, then every subnet and network tagged dizzy:type=subnet or dizzy:type=network there, those " +
			"of Neutron and Nova runs included, so a concurrent apply, chaos, or monitor in the same project is " +
			"torn down mid-flight.",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildOctaviaPlanFromFlags(cmd, opts, scenarioPath, sets)
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
				Enabled: opts.otel, Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "octavia",
			})
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)

			// One startup stack checks the project scope and pre-checks quota so a
			// misconfiguration fails fast; each iteration authenticates its own
			// stack (see the runOnce closure) so a multi-day loop is not at the
			// mercy of token expiry and an unhealthy Keystone fails one iteration
			// rather than dead-looping.
			stack, err := config.NewLoadBalancerStack(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating load balancer clients: %w", err)
			}
			if _, ok := octavia.ProjectIDFromAuth(stack.LoadBalancer); !ok {
				return errOctaviaMonitorNoProject
			}
			if err := octavia.PrecheckQuota(ctx, stack.LoadBalancer, p); err != nil {
				return err
			}

			// 0 means continuous; log it as such so the startup line explains itself.
			interval := cfg.interval.String()
			if cfg.interval == 0 {
				interval = "continuous"
			}
			slog.Info("starting monitor", "scenario", p.Scenario, "interval", interval,
				"iterations", cfg.iterations, "errorWait", cfg.errorWait, "otel", opts.otel)

			runOnce := octaviaMonitorRunOnce(opts, p, tel, keepRunRecords)

			iterations, failures := runMonitorLoop(ctx, cfg, chaos.RealClock{}, runOnce)
			slog.Info("monitor finished", "iterations", iterations, "failures", failures)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set provider=ovn (repeatable)")
	flags.DurationVar(&cfg.interval, "interval", 0, "target cadence between iteration starts, e.g. 15m; a longer iteration starts the next immediately (default 0 = continuous: iterations run back-to-back)")
	flags.IntVar(&cfg.iterations, "iterations", 0, "stop after this many iterations (0 = run forever)")
	flags.DurationVar(&cfg.errorWait, "error-wait", 0, "extra pause after a failed iteration before the next starts (0 = off)")
	flags.BoolVar(&keepRunRecords, "keep-run-records", false, "write a run-<id>.json per iteration (off by default: in monitor mode they accumulate unboundedly)")
	// MarkFlagRequired only fails for an unknown flag; scenario was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// octaviaMonitorRunOnce builds the production per-iteration closure the loop
// drives. Each iteration gets a fresh run id, a fresh metrics collector, and
// its own authenticated stack; it runs the project-scoped pre-flight sweep
// (reclaiming any tester leftovers by the dizzy:type identity), pre-checks
// quota and applies the plan, and cleans up, then records the per-iteration
// summary metrics and logs a one-line summary.
func octaviaMonitorRunOnce(opts *globalOptions, p *octaviaplan.Plan, tel *telemetry.Telemetry, keepRunRecords bool) func(ctx context.Context, iter int) bool {
	return func(ctx context.Context, iter int) bool {
		runID, err := newRunID()
		if err != nil {
			slog.Error("generating run id failed; skipping iteration", "iteration", iter, "error", err)
			return false
		}
		// A fresh Keystone auth per iteration sidesteps token expiry over a
		// multi-day loop and turns an unhealthy Keystone into a failed iteration
		// rather than a dead loop.
		stack, err := config.NewLoadBalancerStack(ctx, opts.osCloud)
		if err != nil {
			slog.Error("iteration authentication failed", "iteration", iter, "run", runID, "error", err)
			return false
		}
		collector := metrics.NewCollector()
		client := octavia.New(stack.LoadBalancer, stack.Network, runID, collector)
		client.SetTelemetry(tel)

		start := time.Now()
		hb := startHeartbeat(ctx, "monitor iteration in progress",
			collectorSnapshot(collector, start, "iteration", iter, "run", runID))
		res := runIteration(ctx, octaviaIterationDeps(stack.LoadBalancer, client, p, runID, opts.concurrency, opts.timeout))
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
				Service:    "octavia",
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

// octaviaIterationDeps builds one iteration's three phases over lb and client,
// both bound to runID. The pre-flight sweep tears down every load balancer,
// VIP subnet, and VIP network the dizzy:type identity finds in the token's
// project, whatever run created it; apply runs the quota pre-check on this
// iteration's own client before anything is created, since octaviaexec.Apply
// does not run it, and then the plan; cleanup tears down this run by its run
// tag, unioned with what apply created. They are built here rather than inline
// in the runOnce closure so a test can drive them against a fake cloud.
func octaviaIterationDeps(lb *gophercloud.ServiceClient, client *octavia.Client, p *octaviaplan.Plan, runID string, concurrency int, opTimeout time.Duration) iterationDeps {
	return iterationDeps{
		preflight: func(ctx context.Context) (int, error) {
			// The fresh run id satisfies Cleanup's non-empty guard; the orphan
			// adapter ignores it and discovers by the dizzy:type identity.
			return octaviaexec.Cleanup(ctx, octaviaTimeoutCleaner{octaviaOrphanCleaner{client}, opTimeout}, runID, nil, concurrency, opTimeout)
		},
		apply: func(ctx context.Context) ([]resource.Resource, error) {
			if err := octavia.PrecheckQuota(ctx, lb, p); err != nil {
				return nil, err
			}
			r, err := octaviaexec.Apply(ctx, client, p, concurrency, opTimeout)
			return r.Created, err
		},
		cleanup: func(ctx context.Context, created []resource.Resource) (int, error) {
			return octaviaexec.Cleanup(ctx, octaviaTimeoutCleaner{client, opTimeout}, runID, created, concurrency, opTimeout)
		},
	}
}

// octaviaOrphanCleaner adapts an Octavia client to the octaviaexec.Cleaner
// seam for the pre-flight sweep, discovering leftovers by the dizzy:type
// identity within the token's project (any tester run) instead of one run's
// dizzy:run tag. It is the load balancer analog of the other services' orphan
// cleaners, so the sweep reuses octaviaexec.Cleanup's exact load balancers,
// subnets, networks ordering and its cascade delete unchanged. Delete and
// WaitForGone promote from the embedded client.
type octaviaOrphanCleaner struct{ *octavia.Client }

// The sweep adapter must satisfy the same seam as the client it wraps.
var _ octaviaexec.Cleaner = octaviaOrphanCleaner{}

// ListByTag ignores the run id and lists by the type tag within the token's
// project, so the sweep reclaims leftovers from any previous crashed or
// interrupted iteration whose run id is no longer known.
func (o octaviaOrphanCleaner) ListByTag(ctx context.Context, kind resource.Kind, _ string) ([]resource.Resource, error) {
	return o.ListByTypeTag(ctx, kind)
}
