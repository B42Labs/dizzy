package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/openstack-tester/internal/config"
	"github.com/B42Labs/openstack-tester/internal/keystone"
	keystoneexec "github.com/B42Labs/openstack-tester/internal/keystone/executor"
	"github.com/B42Labs/openstack-tester/internal/metrics"
)

// newKeystoneCleanupCmd builds "keystone cleanup", which deletes every domain,
// role, project, user, and role assignment a run created, identified strictly by
// the run's ostester-<id>- name prefix (projects also by tag), in reverse
// dependency order. It is idempotent: a second run deletes nothing. The run is
// identified either by its record (--run) or directly by id (--run-id); exactly
// one is required.
func newKeystoneCleanupCmd(opts *globalOptions) *cobra.Command {
	var (
		runPath string
		runID   string
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete all identity resources belonging to a run, by name prefix and tag",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, rec, err := resolveRun(runPath, runID)
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
			collector := metrics.NewCollector()
			client := keystone.New(gc, id, collector)

			// Assignments have no name or tag: they are discovered from each
			// surviving user's live grants and from the run record. Without a record,
			// the sweep still reclaims them via the users it discovers by prefix, but
			// a record is the authoritative handle.
			if rec == nil {
				slog.Warn("cleaning up by id without a run record; role assignments are best reclaimed with a record — pass --run to use it", "run", id)
			}

			hb := startHeartbeat(ctx, "cleanup in progress", collectorSnapshot(collector, time.Now()))
			deleted, cleanupErr := keystoneexec.Cleanup(ctx, client, id, recordedFrom(rec), opts.timeout)
			hb.stop()
			// Report progress even on partial failure so an interrupted sweep is
			// never silent about what it already removed.
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "deleted %d resource(s) for run %s\n", deleted, id); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}
			if cleanupErr != nil {
				return fmt.Errorf("cleaning up run %s: %w", id, cleanupErr)
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) whose resources to delete")
	flags.StringVar(&runID, "run-id", "", "delete resources for this run id directly, without a run record")

	return cmd
}
