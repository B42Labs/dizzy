package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newApplyCmd builds "neutron apply". In Phase 1 only --dry-run is implemented:
// it expands the scenario into a plan and prints a summary without making any
// API calls. The non-dry-run executor is tracked in issue #4. This file
// deliberately imports neither internal/config nor gophercloud so the dry-run
// path cannot reach a cloud.
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

			if !dryRun {
				return fmt.Errorf("apply without --dry-run is not implemented yet (tracked in issue #4)")
			}

			if _, err := fmt.Fprint(cmd.OutOrStdout(), p.Summary()); err != nil {
				return fmt.Errorf("writing summary: %w", err)
			}
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
