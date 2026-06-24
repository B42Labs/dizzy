package main

import (
	"errors"

	"github.com/spf13/cobra"
)

// newNeutronCmd builds the "neutron" command namespace and attaches its
// subcommands. generate, apply, chaos, status, report, and cleanup are
// implemented; verify is a Phase 2 stub, and list-networks is a working
// auth/connectivity smoke test.
func newNeutronCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "neutron",
		Short: "Neutron (networking) load and consistency commands",
	}

	stub := func(use, short string, runErr error) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			RunE: func(cmd *cobra.Command, args []string) error {
				return runErr
			},
		}
	}

	cmd.AddCommand(
		newGenerateCmd(opts),
		newApplyCmd(opts),
		newChaosCmd(opts),
		newStatusCmd(opts),
		newReportCmd(opts),
		newCleanupCmd(opts),
		stub("verify", "Compare a run against OVN/OVS (Phase 2)", errors.New("not implemented yet (Phase 2)")),
		newListNetworksCmd(opts),
	)

	return cmd
}
