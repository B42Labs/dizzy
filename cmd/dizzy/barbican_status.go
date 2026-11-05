package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/run"
)

// newBarbicanStatusCmd builds "barbican status", which loads a barbican run
// record, authenticates against the cloud, and re-queries the live state of
// every secret, order, and container the run created, printing a table of
// logical name, kind, id, and current state. A resource that no longer exists
// shows as "gone".
func newBarbicanStatusCmd(opts *globalOptions) *cobra.Command {
	var runPath string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Re-query the current state of a barbican run's secrets, orders, and containers",
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := run.Load(runPath)
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
			client := newBarbicanClient(kv, rec.RunID, metrics.NewCollector(), opts.timeout)

			return writeStatusTable(ctx, cmd, client, rec.Created)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) to re-query (required)")
	// MarkFlagRequired only fails for an unknown flag; "run" was just added.
	_ = cmd.MarkFlagRequired("run")

	return cmd
}
