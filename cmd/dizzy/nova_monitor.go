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
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/nova"
	novaexec "github.com/B42Labs/dizzy/internal/nova/executor"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// newNovaMonitorCmd builds "nova monitor": the compute counterpart to the other
// monitors. It drives the same service-agnostic loop (runMonitorLoop,
// runIteration) with a Nova runOnce that composes the single-shot pipeline —
// identity-based pre-flight sweep → apply → cleanup — continuously (the default)
// or on a fixed cadence, unattended, for days or weeks, exporting the same
// per-operation and per-iteration metrics (via --otel, tagged service=nova) so a
// single installation's server latencies and error rates become observable over
// time. It survives individual iteration failures.
func newNovaMonitorCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath   string
		sets           []string
		keepRunRecords bool
		cfg            monitorConfig
	)

	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Run sweep→apply→cleanup server iterations continuously or on a cadence and export metrics",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildNovaPlanFromFlags(cmd, opts, scenarioPath, sets)
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
				Enabled: opts.otel, Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "nova",
			})
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)

			// One startup stack resolves the image/flavors and runs the pre-checks so
			// a misconfiguration fails fast; each iteration authenticates its own
			// clients (see the runOnce closure) so a multi-day loop is not at the
			// mercy of token expiry and an unhealthy Keystone fails one iteration
			// rather than dead-looping. The resolved cloud ids are stable across
			// iterations, so they are resolved once here.
			cs, err := config.NewComputeStack(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating compute clients: %w", err)
			}
			resolved, err := resolveNovaRefs(ctx, cs, p)
			if err != nil {
				return err
			}

			// 0 means continuous; log it as such so the startup line explains itself.
			interval := cfg.interval.String()
			if cfg.interval == 0 {
				interval = "continuous"
			}
			slog.Info("starting monitor", "scenario", p.Scenario, "interval", interval,
				"iterations", cfg.iterations, "errorWait", cfg.errorWait, "otel", opts.otel)

			runOnce := novaMonitorRunOnce(opts, p, tel, resolved, keepRunRecords)

			iterations, failures := runMonitorLoop(ctx, cfg, chaos.RealClock{}, runOnce)
			slog.Info("monitor finished", "iterations", iterations, "failures", failures)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.servers=20 (repeatable)")
	flags.DurationVar(&cfg.interval, "interval", 0, "target cadence between iteration starts, e.g. 15m; a longer iteration starts the next immediately (default 0 = continuous: iterations run back-to-back)")
	flags.IntVar(&cfg.iterations, "iterations", 0, "stop after this many iterations (0 = run forever)")
	flags.DurationVar(&cfg.errorWait, "error-wait", 0, "extra pause after a failed iteration before the next starts (0 = off)")
	flags.BoolVar(&keepRunRecords, "keep-run-records", false, "write a run-<id>.json per iteration (off by default: in monitor mode they accumulate unboundedly)")
	// MarkFlagRequired only fails for an unknown flag; scenario was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// novaMonitorRunOnce builds the production per-iteration closure the loop
// drives. Each iteration gets a fresh run id, a fresh metrics collector, and its
// own authenticated clients; it runs the identity-based pre-flight sweep
// (reclaiming any tester leftovers via the type identity), applies the plan, and
// cleans up, then records the per-iteration summary metrics and logs a one-line
// summary.
func novaMonitorRunOnce(opts *globalOptions, p *novaplan.Plan, tel *telemetry.Telemetry, resolved novaexec.Resolved, keepRunRecords bool) func(ctx context.Context, iter int) bool {
	return func(ctx context.Context, iter int) bool {
		runID, err := newRunID()
		if err != nil {
			slog.Error("generating run id failed; skipping iteration", "iteration", iter, "error", err)
			return false
		}
		// A fresh Keystone auth per iteration sidesteps token expiry over a
		// multi-day loop and turns an unhealthy Keystone into a failed iteration
		// rather than a dead loop.
		cs, err := config.NewComputeStack(ctx, opts.osCloud)
		if err != nil {
			slog.Error("iteration authentication failed", "iteration", iter, "run", runID, "error", err)
			return false
		}
		collector := metrics.NewCollector()
		client := nova.New(cs.Compute, cs.Network, cs.BlockStorage, runID, collector)
		client.SetTelemetry(tel)

		start := time.Now()
		hb := startHeartbeat(ctx, "monitor iteration in progress",
			collectorSnapshot(collector, start, "iteration", iter, "run", runID))
		res := runIteration(ctx, iterationDeps{
			preflight: func(ctx context.Context) (int, error) {
				// The fresh run id satisfies Cleanup's non-empty guard; the orphan
				// adapter ignores it and discovers by the dizzy:type identity.
				return novaexec.Cleanup(ctx, novaTimeoutCleaner{novaOrphanCleaner{client}, opts.timeout}, runID, nil, opts.timeout)
			},
			apply: func(ctx context.Context) ([]resource.Resource, error) {
				r, err := novaexec.Apply(ctx, client, p, opts.concurrency, opts.timeout, resolved)
				return r.Created, err
			},
			cleanup: func(ctx context.Context, created []resource.Resource) (int, error) {
				return novaexec.Cleanup(ctx, novaTimeoutCleaner{client, opts.timeout}, runID, created, opts.timeout)
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
			rec := &run.Record{
				RunID:      runID,
				Service:    "nova",
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

// novaOrphanCleaner adapts a Nova client to the novaexec.Cleaner seam for the
// pre-flight sweep, discovering leftovers by the dizzy:type identity (any tester
// run) instead of one run's dizzy:run identity. It is the compute analog of the
// neutron and cinder orphan cleaners, so the sweep reuses novaexec.Cleanup's
// exact servers-before-companions ordering unchanged. DeleteNetworkPorts,
// Delete, and WaitForGone promote from the embedded client.
type novaOrphanCleaner struct{ *nova.Client }

// ListServersByMetadata ignores the run id and lists by the type metadata, so
// the sweep reclaims servers from any previous crashed or interrupted iteration
// whose run id is no longer known.
func (o novaOrphanCleaner) ListServersByMetadata(ctx context.Context, _ string) ([]resource.Resource, error) {
	return o.ListByTypeMetadata(ctx, nova.KindServer)
}

// ListVolumesByMetadata ignores the run id and lists by the type metadata, the
// volume counterpart of ListServersByMetadata above.
func (o novaOrphanCleaner) ListVolumesByMetadata(ctx context.Context, _ string) ([]resource.Resource, error) {
	return o.ListByTypeMetadata(ctx, nova.KindVolume)
}

// ListByTag ignores the run id and lists by the type tag, the tag-discoverable
// (network, subnet, port) counterpart of the metadata sweeps above.
func (o novaOrphanCleaner) ListByTag(ctx context.Context, kind resource.Kind, _ string) ([]resource.Resource, error) {
	return o.ListByTypeTag(ctx, kind)
}
