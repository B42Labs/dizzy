package main

import (
	"errors"

	"github.com/spf13/cobra"
)

// errNotImplemented is returned by the subcommand stubs whose behavior is
// implemented in later work packages.
var errNotImplemented = errors.New("not implemented yet")

// newNeutronCmd builds the "neutron" command namespace and attaches its
// subcommands. The Phase 1 subcommands are stubs until their owning work
// packages land; list-networks is a working auth/connectivity smoke test.
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
		stub("apply", "Create resources from a plan, poll states, and record a run", errNotImplemented),
		stub("status", "Re-query the current state of a run's resources", errNotImplemented),
		stub("report", "Render metrics from a run record", errNotImplemented),
		stub("cleanup", "Delete all resources belonging to a run, by tag", errNotImplemented),
		stub("verify", "Compare a run against OVN/OVS (Phase 2)", errors.New("not implemented yet (Phase 2)")),
		newListNetworksCmd(opts),
	)

	return cmd
}
