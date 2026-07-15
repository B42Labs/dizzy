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

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/glance"
	glanceexec "github.com/B42Labs/dizzy/internal/glance/executor"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// newGlanceApplyCmd builds "glance apply". With --dry-run it expands the scenario
// into a plan and prints a summary without making any API calls. Without
// --dry-run it authenticates against the cloud, creates each image with a
// synthetic uploaded payload, drives each image through its lifecycle, and prints
// the collected timing metrics. The cloud client is constructed only on the
// non-dry-run path, after the early return, so --dry-run never reaches a cloud.
// There is no quota pre-check: Glance exposes no project-quota API (its caps are
// deployment config), so the executor's over-limit fast-fail (a 413) is the only
// backstop. On Ctrl-C / SIGTERM the run record is written and the partial fleet
// is torn down by identity (--keep-on-abort leaves it in place with the cleanup
// hint); a second signal aborts hard, leaving the record for manual cleanup.
func newGlanceApplyCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		dryRun       bool
		sets         []string
		keepOnAbort  bool
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Create images, upload payloads, drive their lifecycle, and record a run",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildGlancePlanFromFlags(cmd, opts, scenarioPath, sets)
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

			slog.Info("applying plan", "run", runID, "scenario", p.Scenario,
				"images", len(p.Images), "uploadMiB", p.TotalUploadMiB(), "concurrency", opts.concurrency)

			start := time.Now()
			hb := startHeartbeat(ctx, "apply in progress", collectorSnapshot(collector, start))
			res, applyErr := glanceexec.Apply(ctx, client, p, opts.concurrency, opts.timeout, p.Seed)
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

			rec := &run.Record{
				RunID:      runID,
				Service:    "glance",
				Scenario:   p.Scenario,
				Seed:       p.Seed,
				StartedAt:  start,
				FinishedAt: finished,
				Created:    res.Created,
				Metrics:    agg,
			}
			if applyErr != nil {
				rec.Error = applyErr.Error()
			}

			// An interrupted run tears itself down by default: write the record first
			// (so a teardown that fails partway stays reclaimable), then delete the
			// partial fleet. A successful apply keeps its resources for the
			// status/report/cleanup workflow, so this only fires on abort.
			if runAborted(ctx, applyErr) {
				return finishAbortedGlanceApply(ctx, cmd.OutOrStdout(), client, runID, res.Created, keepOnAbort, applyErr, opts.concurrency, opts.timeout,
					func() (string, error) { return writeAbortedRunRecord(cmd.OutOrStdout(), rec) })
			}

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
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.images=20 (repeatable)")
	flags.BoolVar(&keepOnAbort, "keep-on-abort", false, "on interrupt, leave already-created resources in place and print the cleanup hint instead of tearing them down")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// finishAbortedGlanceApply tears down what an interrupted glance apply created,
// or — with keepOnAbort — leaves it in place with the reclaim hint. writeRecord
// runs first so a teardown that fails partway, or a second-signal hard abort
// during it, still leaves a record to reclaim from; teardown runs on a
// context.WithoutCancel of ctx (the signal context is already cancelled) with
// each op bounded by opTimeout, deleting images by identity via glanceexec.Cleanup.
// The partial Created list is unioned with the identity listing so an image
// discovery missed is still reclaimed. Every return is a non-nil error naming the
// run id, so the command exits non-zero.
func finishAbortedGlanceApply(ctx context.Context, out io.Writer, c glanceexec.Cleaner, runID string, created []resource.Resource, keepOnAbort bool, applyErr error, concurrency int, opTimeout time.Duration, writeRecord func() (string, error)) error {
	recordPath, _ := writeRecord()
	hint := "--run-id " + runID
	if recordPath != "" {
		hint = "--run " + recordPath
	}

	if keepOnAbort {
		if _, err := fmt.Fprintf(out, "apply interrupted; resources left in place — reclaim with: glance cleanup %s\n", hint); err != nil {
			slog.Warn("writing interrupt hint to output failed", "run", runID, "error", err)
		}
		return fmt.Errorf("applying plan (run %s): %w", runID, applyErr)
	}

	deleted, cleanupErr := glanceexec.Cleanup(context.WithoutCancel(ctx), glanceTimeoutCleaner{c, opTimeout}, runID, created, concurrency, opTimeout)
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		slog.Warn("writing teardown count to output failed", "run", runID, "error", err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("applying plan (run %s): %w; teardown incomplete after deleting %d resource(s): %w — reclaim the rest with: glance cleanup %s", runID, applyErr, deleted, cleanupErr, hint)
	}
	return fmt.Errorf("applying plan (run %s): %w (interrupted; %d created resource(s) torn down)", runID, applyErr, deleted)
}
