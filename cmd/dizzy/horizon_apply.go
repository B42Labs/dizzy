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

	horizonexec "github.com/B42Labs/dizzy/internal/horizon/executor"
	horizonplan "github.com/B42Labs/dizzy/internal/horizon/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
)

// newHorizonApplyCmd builds "horizon apply". With --dry-run it expands the
// scenario into a plan and prints its summary without reading a clouds.yaml
// or sending a request. Without --dry-run it parses --horizon-url, then
// authenticates the clouds.yaml entry at Keystone (connectHorizon), so a bad
// URL or a wrong password fails before the first dashboard request. It
// creates the plan's users and projects, if any, runs every session's
// journey, prints the collected metrics, and writes the run record. A failed
// check does not stop the run: every journey runs to its end, and the command
// exits non-zero when one failed. Created users and projects stay for status
// and cleanup. On Ctrl-C / SIGTERM the record is written and the created
// users and projects are deleted (--keep-on-abort leaves them with the
// cleanup hint); every logged-in session still logs out. A second signal
// aborts hard.
func newHorizonApplyCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		dryRun       bool
		sets         []string
		keepOnAbort  bool
		horizonURL   string
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Run every session's dashboard journey (login, pages, project switch, logout), check each response, and record a run",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildHorizonPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			if dryRun {
				if _, err := fmt.Fprint(cmd.OutOrStdout(), p.Summary()); err != nil {
					return fmt.Errorf("writing summary: %w", err)
				}
				return nil
			}

			base, err := parseHorizonURL(horizonURL)
			if err != nil {
				return err
			}

			// Two-phase shutdown: the first signal cancels the run so the
			// journeys unwind and log out, then the abort epilogue tears down
			// what was created; a second signal kills the process.
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

			runID, err := newRunID()
			if err != nil {
				return err
			}

			target, err := connectHorizon(ctx, opts, base, p)
			if err != nil {
				return err
			}

			collector := metrics.NewCollector()
			client := target.client(runID, collector, tel)

			slog.Info("applying plan", "run", runID, "scenario", p.Scenario, "seed", p.Seed,
				"sessions", len(p.Sessions), "users", len(p.Users), "projects", len(p.Projects),
				"concurrency", opts.concurrency)

			start := time.Now()
			hb := startHeartbeat(ctx, "apply in progress", collectorSnapshot(collector, start))
			res, applyErr := horizonexec.Apply(ctx, target.execTarget(client), p, opts.concurrency, opts.timeout)
			hb.stop()
			finished := time.Now()
			wall := finished.Sub(start)
			agg := collector.Aggregate(wall)

			// A one-shot apply is a single iteration: export the same
			// per-iteration summary metrics the monitor does.
			tel.RecordIteration(ctx, wall, applyErr == nil)
			tel.RecordIterationOperations(ctx, agg.Overall.Attempted, agg.Overall.Succeeded, agg.Overall.Failed)

			// Print metrics even on failure so the run is never silent.
			if _, err := fmt.Fprint(cmd.OutOrStdout(), agg.Summary()); err != nil {
				return fmt.Errorf("writing metrics: %w", err)
			}

			rec := horizonRunRecord(runID, p, start, finished, res.Created, agg, applyErr)

			if runAborted(ctx, applyErr) {
				return finishAbortedHorizonApply(ctx, cmd.OutOrStdout(), client, p, runID, res.Created, keepOnAbort, applyErr, opts.concurrency, opts.timeout,
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

			slog.Info("apply complete", "run", runID, "journeys", res.Journeys, "created", len(res.Created), "wall", wall)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.BoolVar(&dryRun, "dry-run", false, "validate the scenario and print the plan summary without reading a clouds.yaml or sending a request")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.users=5 (repeatable)")
	flags.BoolVar(&keepOnAbort, "keep-on-abort", false, "on interrupt, leave already-created users and projects in place and print the cleanup hint instead of deleting them")
	flags.StringVar(&horizonURL, "horizon-url", "", horizonURLFlagHelp+" without --dry-run")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// horizonRunRecord builds the run record of a horizon apply or monitor
// iteration that ran plan p as runID from start to finished: service
// horizon, and created, the users and projects the run created and did not
// delete itself, empty for a run of the clouds.yaml user. A non-nil err
// becomes the record's error text.
func horizonRunRecord(runID string, p *horizonplan.Plan, start, finished time.Time, created []resource.Resource, agg metrics.Aggregate, err error) *run.Record {
	if created == nil {
		created = []resource.Resource{}
	}
	rec := &run.Record{
		RunID:      runID,
		Service:    "horizon",
		Scenario:   p.Scenario,
		Seed:       p.Seed,
		StartedAt:  start,
		FinishedAt: finished,
		Created:    created,
		Metrics:    agg,
	}
	if err != nil {
		rec.Error = err.Error()
	}
	return rec
}

// finishAbortedHorizonApply tears down what an interrupted horizon apply of
// plan p created, or, with keepOnAbort, leaves it with the reclaim hint.
// writeRecord runs first so a teardown that fails partway still leaves a
// record to reclaim from. The teardown runs horizonexec.Cleanup on a
// context.WithoutCancel of ctx, each call bounded by opTimeout; a plan
// without users created nothing and sends no request. Every return is a
// non-nil error naming the run id, so the command exits non-zero.
func finishAbortedHorizonApply(ctx context.Context, out io.Writer, c horizonexec.Cleaner, p *horizonplan.Plan, runID string, created []resource.Resource, keepOnAbort bool, applyErr error, concurrency int, opTimeout time.Duration, writeRecord func() (string, error)) error {
	recordPath, _ := writeRecord()
	hint := "--run-id " + runID
	if recordPath != "" {
		hint = "--run " + recordPath
	}

	if keepOnAbort && len(p.Users) > 0 {
		if _, err := fmt.Fprintf(out, "apply interrupted; users and projects left in place; reclaim with: horizon cleanup %s\n", hint); err != nil {
			slog.Warn("writing interrupt hint to output failed", "run", runID, "error", err)
		}
		return fmt.Errorf("applying plan (run %s): %w", runID, applyErr)
	}

	var (
		deleted    int
		cleanupErr error
	)
	if len(p.Users) > 0 {
		deleted, cleanupErr = horizonexec.Cleanup(context.WithoutCancel(ctx), c, runID, created, concurrency, opTimeout)
	}
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		slog.Warn("writing teardown count to output failed", "run", runID, "error", err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("applying plan (run %s): %w; teardown incomplete after deleting %d resource(s): %w; reclaim the rest with: horizon cleanup %s", runID, applyErr, deleted, cleanupErr, hint)
	}
	return fmt.Errorf("applying plan (run %s): %w (interrupted; %d created resource(s) torn down)", runID, applyErr, deleted)
}
