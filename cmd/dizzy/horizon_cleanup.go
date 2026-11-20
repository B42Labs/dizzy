package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	horizonexec "github.com/B42Labs/dizzy/internal/horizon/executor"
	"github.com/B42Labs/dizzy/internal/metrics"
)

// newHorizonCleanupCmd builds "horizon cleanup", which deletes the users and
// then the projects a horizon run created, found by their
// dizzyhorizon-<id>- name prefix and run tag and unioned with the run
// record. It is idempotent: a second run deletes nothing. The run is
// identified by its record (--run) or by id (--run-id); exactly one is
// required. It sends no Horizon request: a session needs no cleanup.
func newHorizonCleanupCmd(opts *globalOptions) *cobra.Command {
	var (
		runPath string
		runID   string
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete the users and projects belonging to a horizon run, by name prefix and tag",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, rec, err := resolveRun(runPath, runID)
			if err != nil {
				return err
			}
			if err := requireService(rec, "horizon"); err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			collector := metrics.NewCollector()
			client, err := newHorizonIdentityClient(ctx, opts, id, collector)
			if err != nil {
				return err
			}

			hb := startHeartbeat(ctx, "cleanup in progress", collectorSnapshot(collector, time.Now()))
			deleted, cleanupErr := horizonexec.Cleanup(ctx, client, id, recordedFrom(rec), opts.concurrency, opts.timeout)
			hb.stop()
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
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) whose users and projects to delete")
	flags.StringVar(&runID, "run-id", "", "delete the users and projects of this run id directly, without a run record")

	return cmd
}
