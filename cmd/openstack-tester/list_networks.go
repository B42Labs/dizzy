package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/config"
)

// newListNetworksCmd builds "neutron list-networks", a read-only diagnostic that
// authenticates against the configured cloud and issues a single NetworkV2 call.
// It exists to verify auth and connectivity end-to-end.
func newListNetworksCmd(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list-networks",
		Short: "List networks as an auth/connectivity smoke test",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
			defer cancel()

			client, err := config.NewNetworkClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating network client: %w", err)
			}

			pages, err := networks.List(client, networks.ListOpts{}).AllPages(ctx)
			if err != nil {
				return fmt.Errorf("listing networks: %w", err)
			}
			ns, err := networks.ExtractNetworks(pages)
			if err != nil {
				return fmt.Errorf("extracting networks: %w", err)
			}

			slog.Info("listed networks", "count", len(ns))
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out, "found %d network(s)\n", len(ns)); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}
			for _, n := range ns {
				if _, err := fmt.Fprintf(out, "  %s\t%s\n", n.ID, n.Name); err != nil {
					return fmt.Errorf("writing output: %w", err)
				}
			}
			return nil
		},
	}
}
