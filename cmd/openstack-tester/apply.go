package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/openstack-tester/internal/config"
	"github.com/B42Labs/openstack-tester/internal/executor"
	"github.com/B42Labs/openstack-tester/internal/metrics"
	"github.com/B42Labs/openstack-tester/internal/neutron"
	"github.com/B42Labs/openstack-tester/internal/run"
	"github.com/B42Labs/openstack-tester/internal/telemetry"
)

// newApplyCmd builds "neutron apply". With --dry-run it expands the scenario
// into a plan and prints a summary without making any API calls. Without
// --dry-run it authenticates against the cloud, creates the full tagged
// topology in dependency order, and prints the collected timing metrics. The
// cloud client is constructed only on the non-dry-run path, after the early
// return, so --dry-run never reaches a cloud. On Ctrl-C / SIGTERM the run record
// is written and the partial topology is torn down in reverse dependency order
// (--keep-on-abort leaves it in place with the cleanup hint); a second signal
// aborts hard, leaving the record for manual cleanup.
func newApplyCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath    string
		dryRun          bool
		sets            []string
		externalNetwork string
		keepOnAbort     bool
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Create resources from a plan, poll states, and record a run",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildPlanFromFlags(cmd, opts, scenarioPath, sets)
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
			// exit so an ad-hoc apply lands in the same database as monitor runs. A
			// misconfigured protocol fails fast here rather than silently dropping
			// metrics later.
			tel, err := telemetry.Setup(ctx, telemetry.Config{
				Enabled: opts.otel, Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "neutron",
			})
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)

			runID, err := newRunID()
			if err != nil {
				return err
			}

			gc, err := config.NewNetworkClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating network client: %w", err)
			}

			// Resolve the external network the run will use for router gateways and
			// floating IPs. A named network that does not exist is an error; with no
			// name and no external network present, external connectivity is simply
			// skipped (the plan's intent degrades to a no-op).
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

			// Abort an oversized plan before creating anything, turning a late,
			// messy mid-apply quota failure into an early, clear one. External
			// gateway ports and floating IPs only count when a network is available.
			if err := neutron.PrecheckQuota(ctx, gc, p, haveExternal); err != nil {
				return err
			}

			collector := metrics.NewCollector()
			client := neutron.New(gc, runID, collector)
			client.SetTelemetry(tel)

			slog.Info("applying plan", "run", runID, "scenario", p.Scenario,
				"networks", len(p.Networks), "subnets", len(p.Subnets), "ports", len(p.Ports),
				"concurrency", opts.concurrency)

			start := time.Now()
			hb := startHeartbeat(ctx, "apply in progress", collectorSnapshot(collector, start))
			res, applyErr := executor.Apply(ctx, runID, client, p, opts.concurrency, opts.timeout, externalNetworkID)
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
			// created so far can be reported on and cleaned up by tag.
			rec := &run.Record{
				RunID:      runID,
				Service:    "neutron",
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

			// An interrupted run tears itself down by default: write the record
			// first (so a teardown that fails partway stays reclaimable), then
			// delete the partial topology. A successful apply keeps its resources
			// for the status/report/cleanup workflow, so this only fires on abort.
			if runAborted(ctx, applyErr) {
				return finishAbortedApply(ctx, cmd.OutOrStdout(), client, runID, res.Created, keepOnAbort, applyErr, opts.timeout,
					func() (string, error) { return writeAbortedRunRecord(cmd.OutOrStdout(), rec) })
			}

			// A failed record write must not mask a successful apply: the tagged
			// resources are live and must stay cleanable. Report the apply outcome
			// first, then surface the write failure distinctly so it is never read
			// as a failed apply nor silently dropped.
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
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.networks=200 (repeatable)")
	flags.StringVar(&externalNetwork, "external-network", "", "name of the external network for gateways and floating IPs (default: auto-detect the first external network)")
	flags.BoolVar(&keepOnAbort, "keep-on-abort", false, "on interrupt, leave already-created resources in place and print the cleanup hint instead of tearing them down")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// newRunID returns a short random run identifier (8 lowercase hex characters)
// used to name and tag every resource a run creates.
func newRunID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating run id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// runAborted reports whether an apply run was aborted by a signal and should be
// torn down: true only when the apply returned an error AND the context was
// cancelled. A run that finished successfully keeps its resources even when the
// signal lands just after the last operation (the issue's non-goal), and a run
// that failed without a signal also keeps them for inspection, exactly as
// today. executor.Apply returns ctx.Err() on cancellation, so both conditions
// coincide in every real interrupt.
func runAborted(ctx context.Context, applyErr error) bool {
	return applyErr != nil && ctx.Err() != nil
}

// writeAbortedRunRecord persists an interrupted run's record before teardown so
// the resources still live stay reclaimable, mirroring the non-abort path's
// write. It returns the path written (empty on failure). A write failure is
// logged and returned but not fatal to teardown — cleanup by tag can still
// proceed without a record; a failed status print is logged, never fatal.
func writeAbortedRunRecord(out io.Writer, rec *run.Record) (string, error) {
	recordPath, err := run.Write(".", rec)
	if err != nil {
		slog.Error("writing run record failed; clean up by run id", "run", rec.RunID, "error", err)
		return "", err
	}
	if _, err := fmt.Fprintf(out, "run record written to %s\n", recordPath); err != nil {
		slog.Warn("writing run-record path to output failed", "run", rec.RunID, "error", err)
	}
	return recordPath, nil
}

// finishAbortedApply tears down what an interrupted neutron apply created, or —
// with keepOnAbort — leaves it in place with the reclaim hint. writeRecord is
// called first so a teardown that fails partway, or a second-signal hard abort
// during it, still leaves a record to reclaim from. Teardown runs on a
// context.WithoutCancel of ctx because the signal context is already cancelled
// and must not kill the teardown it triggered, and each op is bounded by
// opTimeout so a wedged call cannot hang shutdown. The partial Created list is
// passed so address scopes — which carry no tag — are reclaimed by id. Every
// return is a non-nil error naming the run id, so the command exits non-zero and
// the run is never read as a clean apply.
func finishAbortedApply(ctx context.Context, out io.Writer, c executor.Cleaner, runID string, created []neutron.Resource, keepOnAbort bool, applyErr error, opTimeout time.Duration, writeRecord func() (string, error)) error {
	recordPath, _ := writeRecord()
	hint := "--run-id " + runID
	if recordPath != "" {
		hint = "--run " + recordPath
	}

	if keepOnAbort {
		if _, err := fmt.Fprintf(out, "apply interrupted; resources left in place — reclaim with: neutron cleanup %s\n", hint); err != nil {
			slog.Warn("writing interrupt hint to output failed", "run", runID, "error", err)
		}
		return fmt.Errorf("applying plan (run %s): %w", runID, applyErr)
	}

	deleted, cleanupErr := executor.Cleanup(context.WithoutCancel(ctx), timeoutCleaner{c, opTimeout}, runID, created)
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		slog.Warn("writing teardown count to output failed", "run", runID, "error", err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("applying plan (run %s): %w; teardown incomplete after deleting %d resource(s): %w — reclaim the rest with: neutron cleanup %s", runID, applyErr, deleted, cleanupErr, hint)
	}
	return fmt.Errorf("applying plan (run %s): %w (interrupted; %d created resource(s) torn down)", runID, applyErr, deleted)
}
