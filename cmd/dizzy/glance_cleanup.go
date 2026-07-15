package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/glance"
	glanceexec "github.com/B42Labs/dizzy/internal/glance/executor"
	"github.com/B42Labs/dizzy/internal/metrics"
)

// newGlanceCleanupCmd builds "glance cleanup", which deletes every image a run
// created, identified strictly by the run's dizzy:run=<id> tag. It is idempotent:
// a second run deletes nothing. The run is identified either by its record
// (--run) or directly by id (--run-id); exactly one is required. Images are fully
// tag-discoverable, so --run-id alone reaches everything the run created.
func newGlanceCleanupCmd(opts *globalOptions) *cobra.Command {
	var (
		runPath string
		runID   string
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete all images belonging to a run, by tag",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, rec, err := resolveRun(runPath, runID)
			if err != nil {
				return err
			}
			if err := requireService(rec, "glance"); err != nil {
				return err
			}

			// Stop cleanly on Ctrl-C / SIGTERM, like apply.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			gc, err := config.NewImageClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating image client: %w", err)
			}
			collector := metrics.NewCollector()
			client := glance.New(gc, id, collector)

			hb := startHeartbeat(ctx, "cleanup in progress", collectorSnapshot(collector, time.Now()))
			deleted, cleanupErr := glanceexec.Cleanup(ctx, glanceTimeoutCleaner{client, opts.timeout}, id, recordedFrom(rec), opts.concurrency, opts.timeout)
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
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) whose images to delete")
	flags.StringVar(&runID, "run-id", "", "delete images for this run id directly, without a run record")

	return cmd
}
