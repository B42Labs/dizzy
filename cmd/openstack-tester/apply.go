package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
)

// newApplyCmd builds "neutron apply". With --dry-run it expands the scenario
// into a plan and prints a summary without making any API calls. Without
// --dry-run it authenticates against the cloud, creates the full tagged
// topology in dependency order, and prints the collected timing metrics. The
// cloud client is constructed only on the non-dry-run path, after the early
// return, so --dry-run never reaches a cloud.
func newApplyCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath    string
		dryRun          bool
		sets            []string
		externalNetwork string
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

			// Stop cleanly on Ctrl-C / SIGTERM: the derived context cancels the
			// run so in-flight operations unwind instead of being killed.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

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

			slog.Info("applying plan", "run", runID, "scenario", p.Scenario,
				"networks", len(p.Networks), "subnets", len(p.Subnets), "ports", len(p.Ports),
				"concurrency", opts.concurrency)

			start := time.Now()
			res, applyErr := executor.Apply(ctx, runID, client, p, opts.concurrency, opts.timeout, externalNetworkID)
			finished := time.Now()
			wall := finished.Sub(start)
			agg := collector.Aggregate(wall)

			// Print metrics even on partial failure so the run is never silent.
			if _, err := fmt.Fprint(cmd.OutOrStdout(), agg.Summary()); err != nil {
				return fmt.Errorf("writing metrics: %w", err)
			}

			// Persist the run record even on partial failure so the resources
			// created so far can be reported on and cleaned up by tag.
			rec := &run.Record{
				RunID:      runID,
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
