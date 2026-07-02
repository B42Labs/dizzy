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

	"github.com/B42Labs/openstack-tester/internal/chaos"
	"github.com/B42Labs/openstack-tester/internal/config"
	"github.com/B42Labs/openstack-tester/internal/executor"
	"github.com/B42Labs/openstack-tester/internal/metrics"
	"github.com/B42Labs/openstack-tester/internal/neutron"
	"github.com/B42Labs/openstack-tester/internal/plan"
	"github.com/B42Labs/openstack-tester/internal/run"
	"github.com/B42Labs/openstack-tester/internal/telemetry"
)

// newMonitorCmd builds "neutron monitor": a loop driver that runs the existing
// single-shot pipeline — pre-flight orphan sweep → apply → cleanup — on a fixed
// cadence, unattended, for days or weeks, exporting the same per-operation and
// per-iteration metrics (via --otel) so a single installation becomes observable
// over time. It composes the existing executor, metrics collector, and cleanup
// code paths unchanged and survives individual iteration failures.
func newMonitorCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath    string
		sets            []string
		externalNetwork string
		keepRunRecords  bool
		cfg             monitorConfig
	)

	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Run apply→cleanup iterations on a fixed cadence and export metrics",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}
			if err := cfg.validate(); err != nil {
				return err
			}

			// Two-phase shutdown: the first SIGINT/SIGTERM cancels the loop for a
			// graceful stop (the current iteration finishes or aborts, then cleans
			// up and the exporter flushes). Unregistering the handler right after
			// means a second signal takes the default disposition and kills the
			// process — the issue's "second signal aborts hard".
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()

			tel, err := telemetry.Setup(ctx, telemetry.Config{
				Enabled: opts.otel, Cloud: opts.cloudName(), Scenario: p.Scenario,
			})
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)

			// One startup client resolves the external network and pre-checks quota
			// so a misconfiguration fails fast; each iteration authenticates its own
			// client (see the runOnce closure) so a multi-day loop is not at the
			// mercy of token expiry and an unhealthy Keystone fails one iteration
			// rather than dead-looping.
			gc, err := config.NewNetworkClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating network client: %w", err)
			}
			extNet, haveExternal, err := neutron.FindExternalNetwork(ctx, gc, externalNetwork)
			if err != nil {
				return err
			}
			externalNetworkID := ""
			switch {
			case haveExternal:
				externalNetworkID = extNet.ID
				slog.Info("using external network for gateways and floating IPs", "id", extNet.ID, "name", extNet.Name)
			case p.RoutersWithExternalGateway() > 0 || len(p.FloatingIPs) > 0:
				slog.Warn("plan wants external connectivity but no external network was found; gateways and floating IPs will be skipped",
					"externalGatewayRouters", p.RoutersWithExternalGateway(), "floatingIPs", len(p.FloatingIPs))
			}
			if err := neutron.PrecheckQuota(ctx, gc, p, haveExternal); err != nil {
				return err
			}

			slog.Info("starting monitor", "scenario", p.Scenario, "interval", cfg.interval,
				"iterations", cfg.iterations, "errorWait", cfg.errorWait, "otel", opts.otel)

			// The plan is expanded once at startup, so every iteration reuses the
			// same seed and topology: comparable across time (the issue's default).
			// Rotating the seed per iteration would broaden coverage at the cost of
			// comparability; that trade-off is documented in the README.
			runOnce := monitorRunOnce(opts, p, tel, externalNetworkID, keepRunRecords)

			iterations, failures := runMonitorLoop(ctx, cfg, chaos.RealClock{}, runOnce)
			slog.Info("monitor finished", "iterations", iterations, "failures", failures)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.networks=200 (repeatable)")
	flags.StringVar(&externalNetwork, "external-network", "", "name of the external network for gateways and floating IPs (default: auto-detect the first external network)")
	flags.DurationVar(&cfg.interval, "interval", 0, "target cadence between iteration starts, e.g. 15m (required); a longer iteration starts the next immediately")
	flags.IntVar(&cfg.iterations, "iterations", 0, "stop after this many iterations (0 = run forever)")
	flags.DurationVar(&cfg.errorWait, "error-wait", 0, "extra pause after a failed iteration before the next starts (0 = off)")
	flags.BoolVar(&keepRunRecords, "keep-run-records", false, "write a run-<id>.json per iteration (off by default: in monitor mode they accumulate unboundedly)")
	// MarkFlagRequired only fails for an unknown flag; both were just added.
	_ = cmd.MarkFlagRequired("scenario")
	_ = cmd.MarkFlagRequired("interval")

	return cmd
}

// monitorConfig holds the loop's pacing knobs, validated before any cloud call.
type monitorConfig struct {
	interval   time.Duration
	errorWait  time.Duration
	iterations int
}

// validate rejects a non-positive interval and negative counts before the loop
// touches the cloud, so a misconfiguration fails fast with a clear message.
func (c monitorConfig) validate() error {
	if c.interval <= 0 {
		return fmt.Errorf("--interval must be positive, got %s", c.interval)
	}
	if c.iterations < 0 {
		return fmt.Errorf("--iterations must be zero (run forever) or positive, got %d", c.iterations)
	}
	if c.errorWait < 0 {
		return fmt.Errorf("--error-wait must not be negative, got %s", c.errorWait)
	}
	return nil
}

// runMonitorLoop drives runOnce on cfg's cadence using clk, returning how many
// iterations ran and how many failed. It paces on the target interval between
// iteration starts: an iteration shorter than the interval sleeps the remainder;
// one that overruns starts the next immediately (no overlap, no backlog). A
// failed iteration additionally waits at least errorWait before the next start.
// iterations == 0 runs forever; a cancelled context ends the loop between or
// during iterations. It is a pure function of the injected clock and runOnce, so
// pacing and shutdown are tested on a fake clock without a cloud.
func runMonitorLoop(ctx context.Context, cfg monitorConfig, clk chaos.Clock, runOnce func(ctx context.Context, iter int) bool) (iterations, failures int) {
	for i := 1; cfg.iterations == 0 || i <= cfg.iterations; i++ {
		if ctx.Err() != nil {
			break
		}
		start := clk.Now()
		ok := runOnce(ctx, i)
		iterations++
		if !ok {
			failures++
		}
		// No trailing sleep after the final capped iteration.
		if cfg.iterations != 0 && i >= cfg.iterations {
			break
		}
		delay := start.Add(cfg.interval).Sub(clk.Now())
		if delay < 0 {
			delay = 0 // the iteration overran the interval: start the next now
		}
		if !ok && delay < cfg.errorWait {
			delay = cfg.errorWait
		}
		if err := clk.Sleep(ctx, delay); err != nil {
			break
		}
	}
	return iterations, failures
}

// iterationDeps are the three cloud-touching phases of one iteration, injected
// so the iteration policy is testable without a cloud.
type iterationDeps struct {
	preflight func(ctx context.Context) (swept int, err error)
	apply     func(ctx context.Context) (created []neutron.Resource, err error)
	cleanup   func(ctx context.Context, created []neutron.Resource) (deleted int, err error)
}

// iterationResult is one iteration's outcome, used for the summary line, the
// per-iteration metrics, and an optional run record.
type iterationResult struct {
	ok      bool
	swept   int
	created []neutron.Resource
	deleted int
	err     error
}

// runIteration runs one iteration's pre-flight sweep, apply, and cleanup. A
// pre-flight error skips apply — a half-swept project must not be filled again —
// but cleanup still runs, always on a context.WithoutCancel of ctx so a
// first-signal cancel tears the iteration down instead of leaking resources
// (best-effort, per the issue's clean-exit criterion). The iteration is ok only
// when all reached phases returned nil and the context was not cancelled; err
// keeps the first phase error for the run record and summary.
func runIteration(ctx context.Context, d iterationDeps) iterationResult {
	var res iterationResult

	res.swept, res.err = d.preflight(ctx)
	if res.err != nil {
		res.err = fmt.Errorf("pre-flight cleanup: %w", res.err)
	} else {
		created, applyErr := d.apply(ctx)
		res.created = created
		if applyErr != nil {
			res.err = fmt.Errorf("apply: %w", applyErr)
		}
	}

	deleted, cleanupErr := d.cleanup(context.WithoutCancel(ctx), res.created)
	res.deleted = deleted
	if cleanupErr != nil && res.err == nil {
		res.err = fmt.Errorf("cleanup: %w", cleanupErr)
	}

	res.ok = res.err == nil && ctx.Err() == nil
	return res
}

// monitorRunOnce builds the production per-iteration closure the loop drives.
// Each iteration gets a fresh run id, a fresh metrics collector, and its own
// authenticated client; it runs the pre-flight sweep (reclaiming any tester
// leftovers via the type tag), applies the plan, and cleans up, then records the
// per-iteration summary metrics and logs a one-line summary. The existing
// per-operation and heartbeat logging keeps working per iteration.
func monitorRunOnce(opts *globalOptions, p *plan.Plan, tel *telemetry.Telemetry, externalNetworkID string, keepRunRecords bool) func(ctx context.Context, iter int) bool {
	return func(ctx context.Context, iter int) bool {
		runID, err := newRunID()
		if err != nil {
			slog.Error("generating run id failed; skipping iteration", "iteration", iter, "error", err)
			return false
		}
		// A fresh Keystone auth per iteration sidesteps token expiry over a
		// multi-day loop and turns an unhealthy Keystone into a failed iteration
		// rather than a dead loop.
		gc, err := config.NewNetworkClient(ctx, opts.osCloud)
		if err != nil {
			slog.Error("iteration authentication failed", "iteration", iter, "run", runID, "error", err)
			return false
		}
		collector := metrics.NewCollector()
		client := neutron.New(gc, runID, collector)
		client.SetTelemetry(tel)

		start := time.Now()
		hb := startHeartbeat(ctx, "monitor iteration in progress",
			collectorSnapshot(collector, start, "iteration", iter, "run", runID))
		res := runIteration(ctx, iterationDeps{
			preflight: func(ctx context.Context) (int, error) {
				return executor.Cleanup(ctx, timeoutCleaner{orphanCleaner{client}, opts.timeout}, runID, nil)
			},
			apply: func(ctx context.Context) ([]neutron.Resource, error) {
				r, err := executor.Apply(ctx, runID, client, p, opts.concurrency, opts.timeout, externalNetworkID)
				return r.Created, err
			},
			cleanup: func(ctx context.Context, created []neutron.Resource) (int, error) {
				return executor.Cleanup(ctx, timeoutCleaner{client, opts.timeout}, runID, created)
			},
		})
		hb.stop()

		wall := time.Since(start)
		agg := collector.Aggregate(wall)
		// Record on a context that survives a first-signal cancel so the final
		// iteration's metrics still make it into the export.
		tel.RecordIteration(context.WithoutCancel(ctx), wall, res.ok)
		tel.RecordIterationOperations(context.WithoutCancel(ctx),
			agg.Overall.Attempted, agg.Overall.Succeeded, agg.Overall.Failed)

		if keepRunRecords {
			writeIterationRecord(p, runID, start, wall, agg, res)
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

// writeIterationRecord persists a run record for one iteration, mirroring what
// apply writes (Error carries the first phase error). A write failure is logged,
// never fatal: it must not abort the loop.
func writeIterationRecord(p *plan.Plan, runID string, start time.Time, wall time.Duration, agg metrics.Aggregate, res iterationResult) {
	rec := &run.Record{
		RunID:      runID,
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
	path, err := run.Write(".", rec)
	if err != nil {
		slog.Error("writing iteration run record failed", "run", runID, "error", err)
		return
	}
	slog.Info("run record written", "run", runID, "path", path)
}

// timeoutCleaner wraps a Cleaner so every cloud operation executor.Cleanup
// performs is bounded by opTimeout. Cleanup runs each Delete/ListByTag/detach
// call on the context it is handed, but the monitor loop's context carries no
// deadline and teardown runs on a context.WithoutCancel that strips one anyway;
// the gophercloud client sets no HTTP timeout of its own (apply gets its
// per-operation bound from executor.WithRetry). Without this a wedged Neutron
// call would hang the iteration — and so the whole loop — indefinitely. Each
// call gets its own timeout, so a large teardown is bounded per operation, the
// same way apply bounds each create.
type timeoutCleaner struct {
	inner     executor.Cleaner
	opTimeout time.Duration
}

func (t timeoutCleaner) ListByTag(ctx context.Context, kind neutron.Kind, runID string) ([]neutron.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListByTag(ctx, kind, runID)
}

func (t timeoutCleaner) DetachRouterInterfaces(ctx context.Context, routerID string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.DetachRouterInterfaces(ctx, routerID)
}

func (t timeoutCleaner) DeleteNetworkPorts(ctx context.Context, networkID string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.DeleteNetworkPorts(ctx, networkID)
}

func (t timeoutCleaner) Delete(ctx context.Context, r neutron.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.Delete(ctx, r)
}

// orphanCleaner adapts a Neutron client to the executor.Cleaner seam for the
// pre-flight sweep, discovering leftovers by the ostester:type tag (any tester
// run) instead of one run's ostester:run tag. It is the second real
// implementation of Cleaner — the one that proves the seam — so the sweep reuses
// executor.Cleanup's exact reverse-dependency ordering unchanged. Address scopes
// cannot be discovered by tag and so are not reclaimed here, the same limitation
// cleanup --run-id has.
type orphanCleaner struct{ *neutron.Client }

// ListByTag ignores the run id and lists by the type tag, so the sweep reclaims
// leftovers from any previous crashed or interrupted iteration whose run id is
// no longer known.
func (o orphanCleaner) ListByTag(ctx context.Context, kind neutron.Kind, _ string) ([]neutron.Resource, error) {
	return o.ListByTypeTag(ctx, kind)
}
