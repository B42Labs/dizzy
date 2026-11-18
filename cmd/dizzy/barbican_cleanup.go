package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	barbicanexec "github.com/B42Labs/dizzy/internal/barbican/executor"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
)

// newBarbicanCleanupCmd builds "barbican cleanup", which deletes every
// container, order, and secret a run created, in that order, identified
// strictly by the run's identity. It is idempotent: a second run deletes
// nothing. The run is identified either by its record (--run) or directly by id
// (--run-id); exactly one is required. Containers are discovered by their
// dizzy-<id>- name prefix, orders by the same prefix on their meta.name, and
// secrets by the prefix confirmed by their dizzy_run=<id> metadata, as the
// secret_ref of a discovered order, or, for a secret without any metadata, by
// a creator_id equal to the user of this command's token. --run-id alone
// therefore reaches every container and order, every stamped secret, every
// secret a run order generated, stamped or not, and every unstamped secret of
// the token's user: one whose stamp failed, one Barbican kept although its
// store failed or was canceled, and the first secret of a retried store. Run as
// another user, or with a token that names no user, it leaves the unstamped
// ones, and only the record reaches a stamp-failed secret then. Secret ACLs and
// container consumers are not deleted on their own: Barbican deletes them with
// their secret or container, and the deleted count leaves them out.
func newBarbicanCleanupCmd(opts *globalOptions) *cobra.Command {
	var (
		runPath string
		runID   string
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete all containers, orders, and secrets belonging to a run, by identity, and with them their consumers and ACLs",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, rec, err := resolveRun(runPath, runID)
			if err != nil {
				return err
			}
			if err := requireService(rec, "barbican"); err != nil {
				return err
			}

			// Stop cleanly on Ctrl-C / SIGTERM, like apply.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			kv, err := config.NewKeyManagerClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating key manager client: %w", err)
			}
			collector := metrics.NewCollector()
			// Secret discovery is bounded per request by the client, not by
			// barbicanTimeoutCleaner, so a large run's discovery fits.
			client := newBarbicanClient(kv, id, collector, opts.timeout)

			hb := startHeartbeat(ctx, "cleanup in progress", collectorSnapshot(collector, time.Now()))
			deleted, cleanupErr := barbicanexec.Cleanup(ctx, barbicanTimeoutCleaner{client, opts.timeout}, id, recordedFrom(rec), opts.concurrency)
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
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) whose secrets, orders, and containers to delete")
	flags.StringVar(&runID, "run-id", "", "delete secrets, orders, and containers for this run id directly, without a run record")

	return cmd
}
