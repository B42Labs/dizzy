package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/octavia"
	octaviaexec "github.com/B42Labs/dizzy/internal/octavia/executor"
)

// newOctaviaCleanupCmd builds "octavia cleanup", which deletes every load
// balancer, VIP subnet, and VIP network a run created, in that order,
// identified strictly by the run's dizzy:run=<id> tag. Each load balancer is
// deleted with cascade, which removes its listeners, pools, members, health
// monitors, and the VIP port Octavia owns, and is waited on until it is gone. A
// port left on a subnet is reported, never deleted. It is idempotent: a second
// run deletes nothing. The run is identified either by its record (--run) or
// directly by id (--run-id); exactly one is required. Everything an Octavia run
// creates carries the run tag, so --run-id alone reaches all of it.
func newOctaviaCleanupCmd(opts *globalOptions) *cobra.Command {
	var (
		runPath string
		runID   string
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete all load balancers, VIP subnets, and VIP networks belonging to a run, by tag",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, rec, err := resolveRun(runPath, runID)
			if err != nil {
				return err
			}
			if err := requireService(rec, "octavia"); err != nil {
				return err
			}

			// Stop cleanly on Ctrl-C / SIGTERM, like apply.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			stack, err := config.NewLoadBalancerStack(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating load balancer clients: %w", err)
			}
			collector := metrics.NewCollector()
			client := octavia.New(stack.LoadBalancer, stack.Network, id, collector)

			hb := startHeartbeat(ctx, "cleanup in progress", collectorSnapshot(collector, time.Now()))
			deleted, cleanupErr := octaviaexec.Cleanup(ctx, octaviaTimeoutCleaner{client, opts.timeout}, id, recordedFrom(rec), opts.concurrency, opts.timeout)
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
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) whose load balancers, subnets, and networks to delete")
	flags.StringVar(&runID, "run-id", "", "delete load balancers, subnets, and networks for this run id directly, without a run record")

	return cmd
}
