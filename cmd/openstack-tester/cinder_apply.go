package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/openstack-tester/internal/cinder"
	cinderexec "github.com/B42Labs/openstack-tester/internal/cinder/executor"
	"github.com/B42Labs/openstack-tester/internal/config"
	"github.com/B42Labs/openstack-tester/internal/metrics"
	"github.com/B42Labs/openstack-tester/internal/run"
	"github.com/B42Labs/openstack-tester/internal/telemetry"
)

// newCinderApplyCmd builds "cinder apply". With --dry-run it expands the
// scenario into a plan and prints a summary without making any API calls.
// Without --dry-run it authenticates against the cloud, creates the volumes,
// extends the planned fraction, snapshots them, and prints the collected timing
// metrics. The cloud client is constructed only on the non-dry-run path, after
// the early return, so --dry-run never reaches a cloud.
func newCinderApplyCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		dryRun       bool
		sets         []string
		volumeType   string
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Create volumes, extend and snapshot them, poll states, and record a run",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := buildCinderPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			if dryRun {
				if _, err := fmt.Fprint(cmd.OutOrStdout(), p.Summary()); err != nil {
					return fmt.Errorf("writing summary: %w", err)
				}
				return nil
			}

			// Stop cleanly on Ctrl-C / SIGTERM: the derived context cancels the
			// run so in-flight operations unwind instead of being killed.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// Set up OTEL export (a no-op unless --otel is set) and flush it on
			// exit so an ad-hoc apply lands in the same database as monitor runs.
			tel, err := telemetry.Setup(ctx, telemetry.Config{
				Enabled: opts.otel, Cloud: opts.cloudName(), Scenario: p.Scenario,
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
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}
