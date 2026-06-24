package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/B42Labs/openstack-tester/internal/config"
	"github.com/B42Labs/openstack-tester/internal/metrics"
	"github.com/B42Labs/openstack-tester/internal/neutron"
	"github.com/B42Labs/openstack-tester/internal/run"
)

// newStatusCmd builds "neutron status", which loads a run record, authenticates
// against the cloud, and re-queries the live state of every resource the run
// created, printing a table of logical name, kind, id, and current state. A
// resource that no longer exists shows as "gone".
func newStatusCmd(opts *globalOptions) *cobra.Command {
	var runPath string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Re-query the current state of a run's resources",
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := run.Load(runPath)
			if err != nil {
				return err
			}

			// Stop cleanly on Ctrl-C / SIGTERM, like apply.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			gc, err := config.NewNetworkClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating network client: %w", err)
			}
			client := neutron.New(gc, rec.RunID, metrics.NewCollector())

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			if _, err := fmt.Fprintln(tw, "LOGICAL\tKIND\tID\tSTATE"); err != nil {
				return fmt.Errorf("writing status table: %w", err)
			}

			var failed int
			for _, r := range rec.Created {
				state, err := observeState(ctx, client, r)
				if err != nil {
					failed++
					slog.Warn("re-querying resource failed", "kind", r.Kind, "id", r.ID, "error", err)
					state = "error"
				}
				if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Logical, r.Kind, r.ID, state); err != nil {
					return fmt.Errorf("writing status table: %w", err)
				}
			}
			if err := tw.Flush(); err != nil {
				return fmt.Errorf("flushing status table: %w", err)
			}

			if failed > 0 {
				return fmt.Errorf("re-querying %d of %d resources failed", failed, len(rec.Created))
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) to re-query (required)")
	// MarkFlagRequired only fails for an unknown flag; "run" was just added.
	_ = cmd.MarkFlagRequired("run")

	return cmd
}

// observeState renders one resource's live state for the status table: its
// status when it reports one, "present" when it exists without a status, or
// "gone" when it no longer exists. The error is the caller's to surface.
func observeState(ctx context.Context, client *neutron.Client, r neutron.Resource) (string, error) {
	status, exists, err := client.Observe(ctx, r)
	switch {
	case err != nil:
		return "", err
	case !exists:
		return "gone", nil
	case status == "":
		return "present", nil
	default:
		return status, nil
	}
}
