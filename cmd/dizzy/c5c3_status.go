package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/run"
)

// newC5C3StatusCmd builds "c5c3 status", which loads a c5c3 run record and
// re-reads every order the run created by its <namespace>/<name> id, printing
// a table of logical name, kind, id, and current state: Ready for a ready
// order, otherwise the reason of its first condition with status False,
// otherwise Pending, and gone for an order that no longer exists. Each GET is
// bounded by --timeout on its own, so an API server that does not answer
// fails the order's row instead of hanging the command. It sends no Keystone
// request, so it reads no clouds.yaml.
func newC5C3StatusCmd(opts *globalOptions) *cobra.Command {
	var (
		runPath string
		kube    c5c3KubeFlags
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Re-query the current state of a c5c3 run's orders",
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := run.Load(runPath)
			if err != nil {
				return err
			}
			if err := requireService(rec, "c5c3"); err != nil {
				return err
			}

			// Stop cleanly on Ctrl-C / SIGTERM, like apply.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			dyn, err := config.NewKubeClient(kube.kubeconfig, kube.kubeContext)
			if err != nil {
				return err
			}
			// Every order is read by the namespace its id names; the client's own
			// namespace only scopes creates and listings, which status never sends.
			// The cluster has no Keystone endpoint override, since status logs in
			// nowhere, and bounds each GET by --timeout.
			cluster := c5c3Cluster{dyn: dyn, requestTimeout: opts.timeout}
			client := cluster.client("", rec.RunID, metrics.NewCollector(), nil)

			return writeStatusTable(ctx, cmd, client, rec.Created)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) to re-query (required)")
	kube.register(flags)
	// MarkFlagRequired only fails for an unknown flag; "run" was just added.
	_ = cmd.MarkFlagRequired("run")

	return cmd
}
