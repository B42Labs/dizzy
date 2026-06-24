package main

import (
	"errors"

	"github.com/spf13/cobra"
)

// errNotImplemented is returned by the subcommand stubs whose behavior is
// implemented in later work packages.
var errNotImplemented = errors.New("not implemented yet")

// newNeutronCmd builds the "neutron" command namespace and attaches its
// subcommands. generate and apply are implemented; the remaining subcommands
// are stubs until their owning work packages land, and list-networks is a
// working auth/connectivity smoke test.
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
		newStatusCmd(opts),
		stub("report", "Render metrics from a run record", errNotImplemented),
		stub("cleanup", "Delete all resources belonging to a run, by tag", errNotImplemented),
		stub("verify", "Compare a run against OVN/OVS (Phase 2)", errors.New("not implemented yet (Phase 2)")),
		newListNetworksCmd(opts),
	)

	return cmd
}
