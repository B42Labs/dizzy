package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/openstack-tester/internal/config"
	"github.com/B42Labs/openstack-tester/internal/executor"
	"github.com/B42Labs/openstack-tester/internal/metrics"
	"github.com/B42Labs/openstack-tester/internal/neutron"
	"github.com/B42Labs/openstack-tester/internal/run"
)

// newCleanupCmd builds "neutron cleanup", which deletes every resource a run
// created, identified strictly by the run's ostester:run=<id> tag, in reverse
// dependency order. It is idempotent: a second run deletes nothing. The run is
// identified either by its record (--run) or directly by id (--run-id); exactly
// one is required.
func newCleanupCmd(opts *globalOptions) *cobra.Command {
	var (
		runPath string
		runID   string
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete all resources belonging to a run, by tag",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, rec, err := resolveRun(runPath, runID)
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
			collector := metrics.NewCollector()
			client := neutron.New(gc, id, collector)

			// Address scopes cannot be discovered by tag and are reclaimed only
			// from a run record. Cleaning up from a bare id leaves any behind.
			if rec == nil {
				slog.Warn("cleaning up by id without a run record; resources that cannot be discovered by tag (e.g. address scopes) will not be reclaimed — pass --run to reclaim them", "run", id)
			}

			hb := startHeartbeat(ctx, "cleanup in progress", collectorSnapshot(collector, time.Now()))
			deleted, cleanupErr := executor.Cleanup(ctx, client, id, recordedFrom(rec))
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

// resolveRun derives the run id and, when available, the run record, from
// exactly one of a run-record path or a literal id. A literal id (--run-id)
// carries no record, so rec is nil and kinds that cannot be discovered by
// tag/metadata cannot be reclaimed; a record (--run) supplies both. It errors
// when neither or both are supplied. Returning the whole record lets the caller
// read the service field (via requireService) as well as the created list.
func resolveRun(runPath, runID string) (id string, rec *run.Record, err error) {
	if (runPath == "") == (runID == "") {
		return "", nil, errors.New("exactly one of --run or --run-id is required")
	}
	if runID != "" {
		return runID, nil, nil
	}
	rec, err = run.Load(runPath)
	if err != nil {
		return "", nil, err
	}
	return rec.RunID, rec, nil
}

// recordedFrom returns a record's created list, or nil when there is no record
// (the --run-id path).
func recordedFrom(rec *run.Record) []neutron.Resource {
	if rec == nil {
		return nil
	}
	return rec.Created
}

// requireService rejects a run record whose service does not match want. A nil
// record (the --run-id path, no record to check) and a record with an empty
// service field (a pre-Cinder record, read as neutron) both pass when want is
// "neutron", so old records keep working. It guards a service's commands from
// operating on another service's record, whose resource kinds its client cannot
// handle.
func requireService(rec *run.Record, want string) error {
	if rec == nil {
		return nil
	}
	got := rec.Service
	if got == "" {
		got = "neutron"
	}
	if got != want {
		return fmt.Errorf("run record is for service %q, not %q", got, want)
	}
	return nil
}
