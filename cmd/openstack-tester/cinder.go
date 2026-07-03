package main

import (
	"github.com/spf13/cobra"
)

// newCinderCmd builds the "cinder" command namespace and attaches its
// subcommands. generate, apply, chaos, monitor, status, report, and cleanup are
// implemented. report is the same service-agnostic builder the neutron namespace
// uses.
func newCinderCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cinder",
		Short: "Cinder (block storage) load and consistency commands",
	}

	cmd.AddCommand(
		newCinderGenerateCmd(opts),
		newCinderApplyCmd(opts),
		newCinderChaosCmd(opts),
		newCinderMonitorCmd(opts),
		newCinderStatusCmd(opts),
		newReportCmd(opts),
		newCinderCleanupCmd(opts),
	)

	return cmd
}
