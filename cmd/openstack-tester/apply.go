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
)

// newApplyCmd builds "neutron apply". With --dry-run it expands the scenario
// into a plan and prints a summary without making any API calls. Without
// --dry-run it authenticates against the cloud, creates the full tagged
// topology in dependency order, and prints the collected timing metrics. The
// cloud client is constructed only on the non-dry-run path, after the early
// return, so --dry-run never reaches a cloud.
func newApplyCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		dryRun       bool
		sets         []string
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Create resources from a plan, poll states, and record a run",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := buildPlanFromFlags(cmd, opts, scenarioPath, sets)
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

			collector := metrics.NewCollector()
			client := neutron.New(gc, runID, collector)

			slog.Info("applying plan", "run", runID, "scenario", p.Scenario,
				"networks", len(p.Networks), "subnets", len(p.Subnets), "ports", len(p.Ports),
				"concurrency", opts.concurrency)

			start := time.Now()
			res, applyErr := executor.Apply(ctx, runID, client, p, opts.concurrency, opts.timeout)
			wall := time.Since(start)

			// Print metrics even on partial failure so the run is never silent.
			if _, err := fmt.Fprint(cmd.OutOrStdout(), collector.Aggregate(wall).Summary()); err != nil {
				return fmt.Errorf("writing metrics: %w", err)
			}

			if applyErr != nil {
				return fmt.Errorf("applying plan (run %s): %w", runID, applyErr)
			}

			slog.Info("apply complete", "run", runID, "created", len(res.Created), "wall", wall)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.BoolVar(&dryRun, "dry-run", false, "validate the scenario and print the plan summary without making API calls")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.networks=200 (repeatable)")
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
