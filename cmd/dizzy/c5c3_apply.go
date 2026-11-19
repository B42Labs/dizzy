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

	c5c3exec "github.com/B42Labs/dizzy/internal/c5c3/executor"
	c5c3plan "github.com/B42Labs/dizzy/internal/c5c3/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
)

// newC5C3ApplyCmd builds "c5c3 apply". With --dry-run it expands the scenario
// into a plan and prints its summary without building a Kubernetes client,
// reading a clouds.yaml or setting up telemetry. Without --dry-run it resolves
// the Keystone endpoint override of --os-cloud, so a missing clouds.yaml or an
// entry without auth_url fails before the first order, builds the Kubernetes
// client, and lists each of the five order kinds in the scenario's namespace:
// a kind the cluster does not serve, or a listing it forbids, stops the
// command before anything is created and before any run record is written.
// It then creates the orders, waits for the operator to fulfil or refuse each,
// logs in to Keystone with the credentials the operator delivers, repairs and
// rotates them in the seven stages of the c5c3 executor, prints the collected
// timing metrics, and writes the run record. On Ctrl-C / SIGTERM the run
// record is written and the orders created so far are deleted by their run
// label (--keep-on-abort leaves them in place with the cleanup hint); a second
// signal aborts hard, leaving the record for manual cleanup.
func newC5C3ApplyCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		dryRun       bool
		sets         []string
		keepOnAbort  bool
		kube         c5c3KubeFlags
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Order the Keystone resources, wait for the operator, check the delivered credentials in Keystone, and record a run",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := buildC5C3PlanFromFlags(scenarioPath, sets)
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
			// kills the process, so there is always a hard way out mid-teardown.
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

			runID, err := newRunID()
			if err != nil {
				return err
			}

			cluster, err := connectC5C3(opts, kube)
			if err != nil {
				return err
			}

			collector := metrics.NewCollector()
			client := cluster.client(p.Namespace, runID, collector, tel)
			client.SetPlan(p)

			// The pre-check runs before any create, so a cluster without the
			// operator's CRDs, or a process without the rights, leaves nothing in
			// the namespace and no run record. The client bounds each of its
			// lists by --timeout.
			if err := client.Precheck(ctx); err != nil {
				return err
			}

			slog.Info("applying plan", "run", runID, "scenario", p.Scenario, "namespace", p.Namespace,
				"orders", len(p.Orders), "refusals", len(p.Refusals), "secretRepair", p.SecretRepair,
				"concurrency", opts.concurrency)

			start := time.Now()
			hb := startHeartbeat(ctx, "apply in progress", collectorSnapshot(collector, start))
			res, applyErr := c5c3exec.Apply(ctx, client, p, opts.concurrency, opts.timeout)
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

			rec := c5c3RunRecord(runID, p, start, finished, res.Created, agg, applyErr)

			// An interrupted run tears itself down by default: write the record first
			// (so a teardown that fails partway stays reclaimable), then delete the
			// orders created so far. A successful apply keeps its orders for the
			// status/report/cleanup workflow, so this only fires on abort.
			if runAborted(ctx, applyErr) {
				return finishAbortedC5C3Apply(ctx, cmd.OutOrStdout(), client, p, runID, res.Created, keepOnAbort, applyErr, opts.concurrency, opts.timeout,
					func() (string, error) { return writeAbortedRunRecord(cmd.OutOrStdout(), rec) })
			}

			recordPath, werr := run.Write(".", rec)
			if werr != nil {
				slog.Error("writing run record failed; clean up by run id", "run", runID, "namespace", p.Namespace, "error", werr)
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
	flags.BoolVar(&dryRun, "dry-run", false, "validate the scenario and print the plan summary without building a Kubernetes client or making API calls")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set role=reader (repeatable)")
	flags.BoolVar(&keepOnAbort, "keep-on-abort", false, "on interrupt, leave already-created orders in place and print the cleanup hint instead of tearing them down")
	kube.register(flags)
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// c5c3RunRecord builds the run record of a c5c3 apply or monitor iteration
// that ran plan p as runID from start to finished: service c5c3, seed 0
// because a c5c3 plan has no seed, and created, the orders the run created
// and did not delete itself, each with its <namespace>/<name> id. A non-nil
// err becomes the record's error text.
func c5c3RunRecord(runID string, p *c5c3plan.Plan, start, finished time.Time, created []resource.Resource, agg metrics.Aggregate, err error) *run.Record {
	rec := &run.Record{
		RunID:      runID,
		Service:    "c5c3",
		Scenario:   p.Scenario,
		Seed:       0,
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

// finishAbortedC5C3Apply tears down what an interrupted c5c3 apply of plan p
// created, or, with keepOnAbort, leaves it in place with the reclaim hint.
// writeRecord runs first so a teardown that fails partway, or a second-signal
// hard abort during it, still leaves a record to reclaim from. The teardown
// runs c5c3exec.Cleanup on a context.WithoutCancel of ctx (the signal context
// is already cancelled), with each request bounded by opTimeout and the waits
// for the operator by the plan's ReadyTimeout and GoneTimeout. The partial
// created list is unioned with the run label's listing, so an order whose
// create answer was lost is still reclaimed. Every return is a non-nil error
// naming the run id, so the command exits non-zero.
func finishAbortedC5C3Apply(ctx context.Context, out io.Writer, c c5c3exec.Cleaner, p *c5c3plan.Plan, runID string, created []resource.Resource, keepOnAbort bool, applyErr error, concurrency int, opTimeout time.Duration, writeRecord func() (string, error)) error {
	recordPath, _ := writeRecord()
	hint := "--run-id " + runID + " --namespace " + p.Namespace
	if recordPath != "" {
		hint = "--run " + recordPath
	}

	if keepOnAbort {
		if _, err := fmt.Fprintf(out, "apply interrupted; orders left in place; reclaim with: c5c3 cleanup %s\n", hint); err != nil {
			slog.Warn("writing interrupt hint to output failed", "run", runID, "error", err)
		}
		return fmt.Errorf("applying plan (run %s): %w", runID, applyErr)
	}

	deleted, cleanupErr := c5c3exec.Cleanup(context.WithoutCancel(ctx), c, runID, created, concurrency, opTimeout, p.ReadyTimeout, p.GoneTimeout)
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		slog.Warn("writing teardown count to output failed", "run", runID, "error", err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("applying plan (run %s): %w; teardown incomplete after deleting %d resource(s): %w; reclaim the rest with: c5c3 cleanup %s", runID, applyErr, deleted, cleanupErr, hint)
	}
	return fmt.Errorf("applying plan (run %s): %w (interrupted; %d created resource(s) torn down)", runID, applyErr, deleted)
}
