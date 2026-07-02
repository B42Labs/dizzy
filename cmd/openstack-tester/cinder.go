package main

import (
	"github.com/spf13/cobra"
)

// newCinderCmd builds the "cinder" command namespace and attaches its
// subcommands. generate, apply, status, report, and cleanup are implemented;
// monitor and chaos are follow-ups (#31, #32) and stay Neutron-only in this
// slice. report is the same service-agnostic builder the neutron namespace uses.
func newCinderCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cinder",
		Short: "Cinder (block storage) load and consistency commands",
	}

	cmd.AddCommand(
		newCinderGenerateCmd(opts),
		newCinderApplyCmd(opts),
		newCinderStatusCmd(opts),
		newReportCmd(opts),
		newCinderCleanupCmd(opts),
	)

	return cmd
}
