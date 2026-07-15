package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/glance"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/run"
)

// newGlanceStatusCmd builds "glance status", which loads a glance run record,
// authenticates against the cloud, and re-queries the live state of every image
// the run created, printing a table of logical name, kind, id, and current
// state. An image that no longer exists shows as "gone".
func newGlanceStatusCmd(opts *globalOptions) *cobra.Command {
	var runPath string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Re-query the current state of a glance run's images",
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := run.Load(runPath)
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
			client := glance.New(gc, rec.RunID, metrics.NewCollector())

			return writeStatusTable(ctx, cmd, client, rec.Created)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) to re-query (required)")
	// MarkFlagRequired only fails for an unknown flag; "run" was just added.
	_ = cmd.MarkFlagRequired("run")

	return cmd
}
