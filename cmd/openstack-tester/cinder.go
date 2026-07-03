package main

import (
	"github.com/spf13/cobra"
)

// newCinderCmd builds the "cinder" command namespace and attaches its
// subcommands. generate, apply, monitor, status, report, and cleanup are
// implemented; chaos is a follow-up (#32) and stays Neutron-only in this slice.
// report is the same service-agnostic builder the neutron namespace uses.
func newCinderCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cinder",
		Short: "Cinder (block storage) load and consistency commands",
	}

	cmd.AddCommand(
		newCinderGenerateCmd(opts),
		newCinderApplyCmd(opts),
		newCinderMonitorCmd(opts),
		newCinderStatusCmd(opts),
		newReportCmd(opts),
		newCinderCleanupCmd(opts),
	)

	return cmd
}
