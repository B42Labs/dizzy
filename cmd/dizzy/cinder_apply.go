package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/cinder"
	cinderexec "github.com/B42Labs/dizzy/internal/cinder/executor"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// newCinderApplyCmd builds "cinder apply". With --dry-run it expands the
// scenario into a plan and prints a summary without making any API calls.
// Without --dry-run it authenticates against the cloud, creates the volumes,
// extends the planned fraction, snapshots them, and prints the collected timing
// metrics. The cloud client is constructed only on the non-dry-run path, after
// the early return, so --dry-run never reaches a cloud. On Ctrl-C / SIGTERM the
// run record is written and the partial volumes and snapshots are torn down
// (snapshots before their volumes; --keep-on-abort leaves them in place with the
// cleanup hint); a second signal aborts hard, leaving the record for manual
// cleanup.
func newCinderApplyCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		dryRun       bool
		sets         []string
		volumeType   string
		keepOnAbort  bool
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Create volumes, extend and snapshot them, poll states, and record a run",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildCinderPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			if dryRun {
				if _, err := fmt.Fprint(cmd.OutOrStdout(), p.Summary()); err != nil {
					return fmt.Errorf("writing summary: %w", err)
				}
				return nil
			}

			// Two-phase shutdown: the first Ctrl-C / SIGTERM cancels the run so
			// in-flight operations unwind instead of being killed, then the abort
			// epilogue below tears down what was created. Unregistering the handler
			// right after means a second signal takes the default disposition and
			// kills the process — there is always a hard way out mid-teardown.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()

			// Set up OTEL export (a no-op unless --otel is set) and flush it on
			// exit so an ad-hoc apply lands in the same database as monitor runs.
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

			// Resolve the volume type when one was named. Like Neutron's external
			// network, the type is a property of the target cloud, not of the
			// cloud-independent plan: it is resolved at apply time and applied to
			// every volume. A named type that does not exist is an error; unset
			// means the cloud's default type. Normalize to the resolved name so the
			// per-type quota keys (volumes_<name>) and the run record use it.
			if volumeType != "" {
				vt, err := cinder.FindVolumeType(ctx, gc, volumeType)
				if err != nil {
					return err
				}
				volumeType = vt.Name
				slog.Info("using volume type", "name", vt.Name, "id", vt.ID)
			}

			// Abort an oversized plan before creating anything, turning a late,
			// messy mid-apply quota failure into an early, clear one. The per-type
			// quotas are checked too when a volume type is set.
			if err := cinder.PrecheckQuota(ctx, gc, p, volumeType); err != nil {
				return err
			}

			collector := metrics.NewCollector()
			client := cinder.New(gc, runID, collector)
			client.SetTelemetry(tel)

			slog.Info("applying plan", "run", runID, "scenario", p.Scenario,
				"volumes", len(p.Volumes), "snapshots", len(p.Snapshots),
				"concurrency", opts.concurrency)

			start := time.Now()
			hb := startHeartbeat(ctx, "apply in progress", collectorSnapshot(collector, start))
			res, applyErr := cinderexec.Apply(ctx, client, p, opts.concurrency, opts.timeout, volumeType)
			hb.stop()
			finished := time.Now()
			wall := finished.Sub(start)
			agg := collector.Aggregate(wall)

			// A one-shot apply is a single iteration: export the same per-iteration
			// summary metrics so ad-hoc and periodic runs share one schema.
			tel.RecordIteration(ctx, wall, applyErr == nil)
			tel.RecordIterationOperations(ctx, agg.Overall.Attempted, agg.Overall.Succeeded, agg.Overall.Failed)

			// Print metrics even on partial failure so the run is never silent.
			if _, err := fmt.Fprint(cmd.OutOrStdout(), agg.Summary()); err != nil {
				return fmt.Errorf("writing metrics: %w", err)
			}

			// Persist the run record even on partial failure so the resources
			// created so far can be reported on and cleaned up by metadata.
			rec := &run.Record{
				RunID:      runID,
				Service:    "cinder",
				Scenario:   p.Scenario,
				Seed:       p.Seed,
				StartedAt:  start,
				FinishedAt: finished,
				Created:    res.Created,
				Metrics:    agg,
				VolumeType: volumeType,
			}
			if applyErr != nil {
				rec.Error = applyErr.Error()
			}

			// An interrupted run tears itself down by default: write the record
			// first (so a teardown that fails partway stays reclaimable), then
			// delete the partial volumes and snapshots. A successful apply keeps its
			// resources for the status/report/cleanup workflow, so this only fires
			// on abort.
			if runAborted(ctx, applyErr) {
				return finishAbortedCinderApply(ctx, cmd.OutOrStdout(), client, runID, res.Created, keepOnAbort, applyErr, opts.timeout,
					func() (string, error) { return writeAbortedRunRecord(cmd.OutOrStdout(), rec) })
			}

			// A failed record write must not mask a successful apply: the resources
			// are live and must stay cleanable. Report the apply outcome first, then
			// surface the write failure distinctly.
			recordPath, werr := run.Write(".", rec)
			if werr != nil {
				slog.Error("writing run record failed; clean up by run id", "run", runID, "error", werr)
			} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "run record written to %s\n", recordPath); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}

			if applyErr != nil {
				return fmt.Errorf("applying plan (run %s): %w", runID, applyErr)
			}
			if werr != nil {
				return fmt.Errorf("apply succeeded but writing run record failed (run %s): %w", runID, werr)
			}

			slog.Info("apply complete", "run", runID, "created", len(res.Created), "wall", wall)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.BoolVar(&dryRun, "dry-run", false, "validate the scenario and print the plan summary without making API calls")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.volumes=20 (repeatable)")
	flags.StringVar(&volumeType, "volume-type", "", "name of the Cinder volume type to create volumes with (default: the cloud's default type)")
	flags.BoolVar(&keepOnAbort, "keep-on-abort", false, "on interrupt, leave already-created resources in place and print the cleanup hint instead of tearing them down")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// finishAbortedCinderApply tears down what an interrupted cinder apply created,
// or — with keepOnAbort — leaves it in place with the reclaim hint. It is the
// block-storage twin of finishAbortedApply: writeRecord runs first so a teardown
// that fails partway, or a second-signal hard abort during it, still leaves a
// record to reclaim from; teardown runs on a context.WithoutCancel of ctx (the
// signal context is already cancelled) with each op bounded by opTimeout, and
// deletes snapshots before their volumes via cinderexec.Cleanup. The partial
// Created list is unioned with the metadata listing so a resource the metadata
// query missed is still reclaimed. Every return is a non-nil error naming the
// run id, so the command exits non-zero.
func finishAbortedCinderApply(ctx context.Context, out io.Writer, c cinderexec.Cleaner, runID string, created []resource.Resource, keepOnAbort bool, applyErr error, opTimeout time.Duration, writeRecord func() (string, error)) error {
	recordPath, _ := writeRecord()
	hint := "--run-id " + runID
	if recordPath != "" {
		hint = "--run " + recordPath
	}

	if keepOnAbort {
		if _, err := fmt.Fprintf(out, "apply interrupted; resources left in place — reclaim with: cinder cleanup %s\n", hint); err != nil {
			slog.Warn("writing interrupt hint to output failed", "run", runID, "error", err)
		}
		return fmt.Errorf("applying plan (run %s): %w", runID, applyErr)
	}

	deleted, cleanupErr := cinderexec.Cleanup(context.WithoutCancel(ctx), cinderTimeoutCleaner{c, opTimeout}, runID, created, opTimeout)
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		slog.Warn("writing teardown count to output failed", "run", runID, "error", err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("applying plan (run %s): %w; teardown incomplete after deleting %d resource(s): %w — reclaim the rest with: cinder cleanup %s", runID, applyErr, deleted, cleanupErr, hint)
	}
	return fmt.Errorf("applying plan (run %s): %w (interrupted; %d created resource(s) torn down)", runID, applyErr, deleted)
}
