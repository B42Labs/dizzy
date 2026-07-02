package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/openstack-tester/internal/cinder"
	cinderexec "github.com/B42Labs/openstack-tester/internal/cinder/executor"
	"github.com/B42Labs/openstack-tester/internal/config"
	"github.com/B42Labs/openstack-tester/internal/metrics"
)

// newCinderCleanupCmd builds "cinder cleanup", which deletes every volume and
// snapshot a run created, identified strictly by the run's ostester:run=<id>
// metadata, snapshots before volumes. It is idempotent: a second run deletes
// nothing. The run is identified either by its record (--run) or directly by id
// (--run-id); exactly one is required.
func newCinderCleanupCmd(opts *globalOptions) *cobra.Command {
	var (
		runPath string
		runID   string
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete all volumes and snapshots belonging to a run, by metadata",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, rec, err := resolveRun(runPath, runID)
			if err != nil {
				return err
			}
			if err := requireService(rec, "cinder"); err != nil {
				return err
			}

			// Stop cleanly on Ctrl-C / SIGTERM, like apply.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			gc, err := config.NewBlockStorageClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating block storage client: %w", err)
			}
			collector := metrics.NewCollector()
			client := cinder.New(gc, id, collector)

			hb := startHeartbeat(ctx, "cleanup in progress", collectorSnapshot(collector, time.Now()))
			deleted, cleanupErr := cinderexec.Cleanup(ctx, client, id, recordedFrom(rec), opts.timeout)
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
