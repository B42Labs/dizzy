package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	c5c3exec "github.com/B42Labs/dizzy/internal/c5c3/executor"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/run"
)

// c5c3CleanupWaitBound bounds both kinds of wait c5c3 cleanup makes for the
// operator: for a user order's password generation to settle, and for each
// deleted order to be gone. A monitor iteration and an interrupted apply take
// these bounds from the scenario's timeouts.ready and timeouts.gone; cleanup
// has no scenario, so it uses their defaults.
const c5c3CleanupWaitBound = 15 * time.Minute

// errC5C3CleanupNamespace is what c5c3 cleanup fails with when nothing names
// the namespace of the run's orders.
var errC5C3CleanupNamespace = errors.New("--namespace is required with --run-id, or with a run record that lists no order")

// newC5C3CleanupCmd builds "c5c3 cleanup", which deletes every order a run
// created, identified strictly by the run's dizzy/run label in one Kubernetes
// namespace and unioned with the run record's orders, and then checks that
// every credential the deleted user and application credential orders
// delivered stops working. It deletes kind by kind in the order the
// operator's holds require: application credentials, role assignments,
// catalog entries, projects, users. It is idempotent: a second run deletes
// nothing. The run is identified either by its record (--run) or directly by
// id (--run-id); exactly one is required. The namespace is --namespace when
// given, otherwise, with --run, the namespace of the record's first order.
func newC5C3CleanupCmd(opts *globalOptions) *cobra.Command {
	var (
		runPath   string
		runID     string
		namespace string
		kube      c5c3KubeFlags
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete all orders belonging to a run, by label, and check that their credentials stop working",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, rec, err := resolveRun(runPath, runID)
			if err != nil {
				return err
			}
			if err := requireService(rec, "c5c3"); err != nil {
				return err
			}
			ns, err := c5c3CleanupNamespace(namespace, rec)
			if err != nil {
				return err
			}

			// Stop cleanly on Ctrl-C / SIGTERM, like apply.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			cluster, err := connectC5C3(opts, kube)
			if err != nil {
				return err
			}
			collector := metrics.NewCollector()
			client := cluster.client(ns, id, collector, nil)

			hb := startHeartbeat(ctx, "cleanup in progress", collectorSnapshot(collector, time.Now()))
			deleted, cleanupErr := c5c3exec.Cleanup(ctx, client, id, recordedFrom(rec), opts.concurrency, opts.timeout, c5c3CleanupWaitBound, c5c3CleanupWaitBound)
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
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) whose orders to delete")
	flags.StringVar(&runID, "run-id", "", "delete the orders of this run id directly, without a run record (needs --namespace)")
	flags.StringVar(&namespace, "namespace", "", "Kubernetes namespace of the run's orders (default with --run: the namespace of the record's first order)")
	kube.register(flags)

	return cmd
}

// c5c3CleanupNamespace returns the Kubernetes namespace c5c3 cleanup acts in:
// namespace, the --namespace value, when it is set; otherwise the namespace
// half of the first created id of rec, the --run record. Without either it
// returns errC5C3CleanupNamespace, and a first id that is not
// <namespace>/<name> is an error naming it.
func c5c3CleanupNamespace(namespace string, rec *run.Record) (string, error) {
	if namespace != "" {
		return namespace, nil
	}
	if rec == nil || len(rec.Created) == 0 {
		return "", errC5C3CleanupNamespace
	}
	first := rec.Created[0].ID
	ns, name, ok := strings.Cut(first, "/")
	if !ok || ns == "" || name == "" {
		return "", fmt.Errorf("the run record's first order id %q is not <namespace>/<name>; pass --namespace", first)
	}
	return ns, nil
}
