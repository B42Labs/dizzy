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
	"github.com/B42Labs/openstack-tester/internal/resource"
	"github.com/B42Labs/openstack-tester/internal/run"
)

// observer is the narrow re-query surface the status table drives: report a
// resource's live state. Both *neutron.Client and *cinder.Client satisfy it, so
// the neutron and cinder status commands share one table loop.
type observer interface {
	Observe(ctx context.Context, r resource.Resource) (string, bool, error)
}

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
			if err := requireService(rec, "neutron"); err != nil {
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

			return writeStatusTable(cmd, ctx, client, rec.Created)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) to re-query (required)")
	// MarkFlagRequired only fails for an unknown flag; "run" was just added.
	_ = cmd.MarkFlagRequired("run")

	return cmd
}

// writeStatusTable re-queries every resource through obs and prints a table of
// logical name, kind, id, and current state. It returns an error when any
// resource failed to re-query. It is shared by the neutron and cinder status
// commands.
func writeStatusTable(cmd *cobra.Command, ctx context.Context, obs observer, resources []resource.Resource) error {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "LOGICAL\tKIND\tID\tSTATE"); err != nil {
		return fmt.Errorf("writing status table: %w", err)
	}

	var failed int
	for _, r := range resources {
		state, err := observeState(ctx, obs, r)
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
		return fmt.Errorf("re-querying %d of %d resources failed", failed, len(resources))
	}
	return nil
}

// observeState renders one resource's live state for the status table: its
// status when it reports one, "present" when it exists without a status, or
// "gone" when it no longer exists. The error is the caller's to surface.
func observeState(ctx context.Context, obs observer, r resource.Resource) (string, error) {
	status, exists, err := obs.Observe(ctx, r)
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
