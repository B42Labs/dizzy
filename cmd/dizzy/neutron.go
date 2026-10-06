package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/neutron"
	"github.com/B42Labs/dizzy/internal/plan"
)

// newNeutronCmd builds the "neutron" command namespace and attaches its
// subcommands. generate, apply, chaos, monitor, status, report, and cleanup are
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
		newMonitorCmd(opts),
		newStatusCmd(opts),
		newReportCmd(opts),
		newCleanupCmd(opts),
		stub("verify", "Compare a run against OVN/OVS (Phase 2)", errors.New("not implemented yet (Phase 2)")),
		newListNetworksCmd(opts),
	)

	return cmd
}

// precheckNeutron runs the read-only pre-checks of a Neutron run against p,
// before anything is created. It resolves the external network the run uses
// for router gateways and floating IPs: externalNetwork when named, which is
// an error when it does not exist, else the first one. With no external
// network, external connectivity is skipped, with a warning when p wants it.
// It then pre-checks the network quota, turning a late, messy quota failure
// into an early, clear one; external gateway ports and floating IPs count only
// when a network is available. It returns the network's ID, empty when there
// is none.
func precheckNeutron(ctx context.Context, gc *gophercloud.ServiceClient, externalNetwork string, p *plan.Plan) (string, error) {
	extNet, haveExternal, err := neutron.FindExternalNetwork(ctx, gc, externalNetwork)
	if err != nil {
		return "", err
	}
	switch {
	case haveExternal:
		slog.Info("using external network for gateways and floating IPs", "id", extNet.ID, "name", extNet.Name)
	case p.RoutersWithExternalGateway() > 0 || len(p.FloatingIPs) > 0:
		slog.Warn("plan wants external connectivity but no external network was found; gateways and floating IPs will be skipped",
			"externalGatewayRouters", p.RoutersWithExternalGateway(), "floatingIPs", len(p.FloatingIPs))
	}
	if err := neutron.PrecheckQuota(ctx, gc, p, haveExternal); err != nil {
		return "", err
	}
	return extNet.ID, nil
}
