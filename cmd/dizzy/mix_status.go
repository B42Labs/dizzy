package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/mix"
	"github.com/B42Labs/dizzy/internal/run"
)

// newMixStatusCmd builds "mix status", which loads a mix run record,
// authenticates every persona and background lane against the cloud the record
// names for it, and re-queries the live state of the resources each created,
// printing one table per persona and lane. A resource that no longer exists
// shows as "gone".
func newMixStatusCmd(opts *globalOptions) *cobra.Command {
	var runPath string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Re-query the current state of a mix run's resources, per persona and lane",
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := run.Load(runPath)
			if err != nil {
				return err
			}
			if err := requireService(rec, "mix"); err != nil {
				return err
			}
			if err := mix.CheckServices(rec.Services); err != nil {
				return err
			}

			// Stop cleanly on Ctrl-C / SIGTERM, like apply.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			overall := metrics.NewCollector()
			lanes, err := buildCleanupLanes(ctx, opts, recordLaneInputs(rec, overall), recordServiceLaneInputs(rec, overall))
			if err != nil {
				return err
			}
			return writeMixStatus(ctx, cmd, rec, lanes)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&runPath, "run", "", "path to the mix run record (run-<id>.json) to re-query (required)")
	// MarkFlagRequired only fails for an unknown flag; "run" was just added.
	_ = cmd.MarkFlagRequired("run")

	return cmd
}

// writeMixStatus prints, for every lane, a "persona <name> (run <identity>)"
// heading, or "lane <name> (run <identity>)" for a background lane, and the
// status table of the record's resources that lane created, re-queried
// through the lane. It visits every lane and returns an error when any table
// failed.
func writeMixStatus(ctx context.Context, cmd *cobra.Command, rec *run.Record, lanes []*mix.Lane) error {
	out := cmd.OutOrStdout()
	var failed int
	what := "personas"
	for i, l := range lanes {
		sep := ""
		if i > 0 {
			sep = "\n"
		}
		noun := laneNoun(l)
		if l.Background() {
			what = "personas and lanes"
		}
		if _, err := fmt.Fprintf(out, "%s%s %s (run %s)\n", sep, noun, l.Name, l.RunID); err != nil {
			return fmt.Errorf("writing output: %w", err)
		}
		if err := writeStatusTable(ctx, cmd, observeFunc(l.Observe), resourcesOfLane(rec.Created, l)); err != nil {
			failed++
			slog.Warn("re-querying "+noun+" failed", noun, l.Name, "run", l.RunID, "error", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("re-querying %d of %d %s failed", failed, len(lanes), what)
	}
	return nil
}
