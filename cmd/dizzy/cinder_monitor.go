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
	"github.com/B42Labs/dizzy/internal/cinder"
	cinderexec "github.com/B42Labs/dizzy/internal/cinder/executor"
	cinderplan "github.com/B42Labs/dizzy/internal/cinder/plan"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// newCinderMonitorCmd builds "cinder monitor": the block-storage counterpart to
// "neutron monitor". It drives the same service-agnostic loop (runMonitorLoop,
// runIteration) with a Cinder runOnce that composes the single-shot pipeline —
// metadata-based pre-flight sweep → apply → cleanup — continuously (the default)
// or on a fixed cadence, unattended, for days or weeks, exporting the same
// per-operation and per-iteration metrics (via --otel, tagged service=cinder) so
// a single installation's volume/snapshot latencies and error rates become
// observable over time. It survives individual iteration failures.
func newCinderMonitorCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath   string
		sets           []string
		volumeType     string
		keepRunRecords bool
		cfg            monitorConfig
	)

	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Run sweep→apply→cleanup volume iterations continuously or on a cadence and export metrics",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildCinderPlanFromFlags(cmd, opts, scenarioPath, sets)
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
				Enabled: opts.otel, Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "cinder",
			})
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)

			// One startup client resolves the volume type and pre-checks quota so a
			// misconfiguration fails fast; each iteration authenticates its own
			// client (see the runOnce closure) so a multi-day loop is not at the
			// mercy of token expiry and an unhealthy Keystone fails one iteration
			// rather than dead-looping.
			gc, err := config.NewBlockStorageClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating block storage client: %w", err)
			}
			// Like Neutron's external network, the volume type is a property of the
			// target cloud, not of the cloud-independent plan: resolve it once and
			// normalize to the resolved name so the per-type quota keys and the run
			// records use it. A named type that does not exist is an error; unset
			// means the cloud's default type.
			if volumeType != "" {
				vt, err := cinder.FindVolumeType(ctx, gc, volumeType)
				if err != nil {
					return err
				}
				volumeType = vt.Name
				slog.Info("using volume type", "name", vt.Name, "id", vt.ID)
			}
			if err := cinder.PrecheckQuota(ctx, gc, p, volumeType); err != nil {
				return err
			}

			// 0 means continuous; log it as such so the startup line explains itself.
			interval := cfg.interval.String()
			if cfg.interval == 0 {
				interval = "continuous"
			}
			slog.Info("starting monitor", "scenario", p.Scenario, "interval", interval,
				"iterations", cfg.iterations, "errorWait", cfg.errorWait,
				"volumeType", volumeType, "otel", opts.otel)

			// The plan is expanded once at startup, so every iteration reuses the
			// same seed and topology: comparable across time (the issue's default).
			runOnce := cinderMonitorRunOnce(opts, p, tel, volumeType, keepRunRecords)

			iterations, failures := runMonitorLoop(ctx, cfg, chaos.RealClock{}, runOnce)
			slog.Info("monitor finished", "iterations", iterations, "failures", failures)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.volumes=20 (repeatable)")
	flags.StringVar(&volumeType, "volume-type", "", "name of the Cinder volume type to create volumes with (default: the cloud's default type)")
	flags.DurationVar(&cfg.interval, "interval", 0, "target cadence between iteration starts, e.g. 15m; a longer iteration starts the next immediately (default 0 = continuous: iterations run back-to-back)")
	flags.IntVar(&cfg.iterations, "iterations", 0, "stop after this many iterations (0 = run forever)")
	flags.DurationVar(&cfg.errorWait, "error-wait", 0, "extra pause after a failed iteration before the next starts (0 = off)")
	flags.BoolVar(&keepRunRecords, "keep-run-records", false, "write a run-<id>.json per iteration (off by default: in monitor mode they accumulate unboundedly)")
	// MarkFlagRequired only fails for an unknown flag; scenario was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// cinderMonitorRunOnce builds the production per-iteration closure the loop
// drives. Each iteration gets a fresh run id, a fresh metrics collector, and its
// own authenticated client; it runs the metadata-based pre-flight sweep
// (reclaiming any tester leftovers via the type metadata), applies the plan, and
// cleans up, then records the per-iteration summary metrics and logs a one-line
// summary. The existing per-operation and heartbeat logging keeps working per
// iteration.
func cinderMonitorRunOnce(opts *globalOptions, p *cinderplan.Plan, tel *telemetry.Telemetry, volumeType string, keepRunRecords bool) func(ctx context.Context, iter int) bool {
	return func(ctx context.Context, iter int) bool {
		runID, err := newRunID()
		if err != nil {
			slog.Error("generating run id failed; skipping iteration", "iteration", iter, "error", err)
			return false
		}
		// A fresh Keystone auth per iteration sidesteps token expiry over a
		// multi-day loop and turns an unhealthy Keystone into a failed iteration
		// rather than a dead loop.
		gc, err := config.NewBlockStorageClient(ctx, opts.osCloud)
		if err != nil {
			slog.Error("iteration authentication failed", "iteration", iter, "run", runID, "error", err)
			return false
		}
		collector := metrics.NewCollector()
		client := cinder.New(gc, runID, collector)
		client.SetTelemetry(tel)

		start := time.Now()
		hb := startHeartbeat(ctx, "monitor iteration in progress",
			collectorSnapshot(collector, start, "iteration", iter, "run", runID))
		res := runIteration(ctx, iterationDeps{
			preflight: func(ctx context.Context) (int, error) {
				// The fresh run id satisfies Cleanup's non-empty guard; the orphan
				// adapter ignores it and discovers by the ostester:type metadata.
				return cinderexec.Cleanup(ctx, cinderTimeoutCleaner{cinderOrphanCleaner{client}, opts.timeout}, runID, nil, opts.timeout)
			},
			apply: func(ctx context.Context) ([]resource.Resource, error) {
				r, err := cinderexec.Apply(ctx, client, p, opts.concurrency, opts.timeout, volumeType)
				return r.Created, err
			},
			cleanup: func(ctx context.Context, created []resource.Resource) (int, error) {
				return cinderexec.Cleanup(ctx, cinderTimeoutCleaner{client, opts.timeout}, runID, created, opts.timeout)
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
				Service:    "cinder",
				Scenario:   p.Scenario,
				Seed:       p.Seed,
				StartedAt:  start,
				FinishedAt: start.Add(wall),
				Created:    res.created,
				Metrics:    agg,
				VolumeType: volumeType,
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

// cinderOrphanCleaner adapts a Cinder client to the cinderexec.Cleaner seam for
// the pre-flight sweep, discovering leftovers by the ostester:type metadata (any
// tester run) instead of one run's ostester:run metadata. It is the metadata
// analog of Neutron's orphanCleaner, so the sweep reuses cinderexec.Cleanup's
// exact snapshots-before-volumes ordering unchanged. Delete and WaitForGone
// promote from the embedded client.
type cinderOrphanCleaner struct{ *cinder.Client }

// ListVolumesByMetadata ignores the run id and lists by the type metadata, so
// the sweep reclaims volumes from any previous crashed or interrupted iteration
// whose run id is no longer known.
func (o cinderOrphanCleaner) ListVolumesByMetadata(ctx context.Context, _ string) ([]resource.Resource, error) {
	return o.ListByTypeMetadata(ctx, cinder.KindVolume)
}

// ListSnapshotsByMetadata ignores the run id and lists by the type metadata, the
// snapshot counterpart of ListVolumesByMetadata above.
func (o cinderOrphanCleaner) ListSnapshotsByMetadata(ctx context.Context, _ string) ([]resource.Resource, error) {
	return o.ListByTypeMetadata(ctx, cinder.KindSnapshot)
}

// cinderTimeoutCleaner wraps a cinderexec.Cleaner so every cloud operation
// Cleanup performs is bounded by opTimeout. Cleanup runs each List/Delete/
// WaitForGone call on the context it is handed, but the monitor loop's context
// carries no deadline and teardown runs on a context.WithoutCancel that strips
// one anyway; the gophercloud client sets no HTTP timeout of its own. Without
// this a wedged Cinder call would hang the iteration — and so the whole loop —
// indefinitely. Each call gets its own timeout, so a large teardown is bounded
// per operation, the same way apply bounds each create. Cleanup additionally
// bounds WaitForGone with the same opTimeout; the double bound is equivalent and
// harmless.
type cinderTimeoutCleaner struct {
	inner     cinderexec.Cleaner
	opTimeout time.Duration
}

func (t cinderTimeoutCleaner) ListVolumesByMetadata(ctx context.Context, runID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListVolumesByMetadata(ctx, runID)
}

func (t cinderTimeoutCleaner) ListSnapshotsByMetadata(ctx context.Context, runID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListSnapshotsByMetadata(ctx, runID)
}

func (t cinderTimeoutCleaner) Delete(ctx context.Context, r resource.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.Delete(ctx, r)
}

func (t cinderTimeoutCleaner) WaitForGone(ctx context.Context, r resource.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.WaitForGone(ctx, r)
}
