package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/B42Labs/openstack-tester/internal/config"
	"github.com/B42Labs/openstack-tester/internal/keystone"
	"github.com/B42Labs/openstack-tester/internal/metrics"
	"github.com/B42Labs/openstack-tester/internal/run"
)

// newKeystoneStatusCmd builds "keystone status", which loads a keystone run
// record, authenticates against the cloud, and re-queries the live state of
// every domain, role, project, user, and role assignment the run created,
// printing a table of logical name, kind, id, and current state. A resource that
// no longer exists shows as "gone".
func newKeystoneStatusCmd(opts *globalOptions) *cobra.Command {
	var runPath string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Re-query the current state of a keystone run's resources",
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := run.Load(runPath)
			if err != nil {
				return err
			}
			if err := requireService(rec, "keystone"); err != nil {
				return err
			}

			// Stop cleanly on Ctrl-C / SIGTERM, like apply.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			gc, err := config.NewIdentityClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating identity client: %w", err)
			}
			client := keystone.New(gc, rec.RunID, metrics.NewCollector())

			return writeStatusTable(ctx, cmd, client, rec.Created)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) to re-query (required)")
	// MarkFlagRequired only fails for an unknown flag; "run" was just added.
	_ = cmd.MarkFlagRequired("run")

	return cmd
}
