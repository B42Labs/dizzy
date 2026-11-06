package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/octavia"
	octaviaplan "github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
)

// newOctaviaStatusCmd builds "octavia status", which loads an octavia run
// record, authenticates against the cloud, and re-queries the live state of
// every VIP network, subnet, load balancer, listener, pool, member, and health
// monitor the run created, printing a table of logical name, kind, id, and
// current state. A resource that no longer exists shows as "gone". A member is
// read below its pool, whose id the record's pool entries supply.
func newOctaviaStatusCmd(opts *globalOptions) *cobra.Command {
	var runPath string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Re-query the current state of an octavia run's resources",
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := run.Load(runPath)
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
			client := octavia.New(stack.LoadBalancer, stack.Network, rec.RunID, metrics.NewCollector())

			return writeStatusTable(ctx, cmd, octaviaObserver{c: client, poolIDs: recordedPoolIDs(rec.Created)}, rec.Created)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) to re-query (required)")
	// MarkFlagRequired only fails for an unknown flag; "run" was just added.
	_ = cmd.MarkFlagRequired("run")

	return cmd
}

// octaviaObserver is the observer the octavia status table drives. Octavia
// reads a member below its pool, and the run record does not store a member's
// pool, so the observer recovers it from the member's logical name
// (<pool>-member-<nn>, octaviaplan.MemberPool) and looks its id up in poolIDs,
// the record's pool entries by logical name. Every other kind goes through
// Client.Observe.
type octaviaObserver struct {
	c       *octavia.Client
	poolIDs map[string]string
}

// Observe reports r's live state. A member whose pool the record lacks returns
// an error without a request, which the status table renders as state "error".
func (o octaviaObserver) Observe(ctx context.Context, r resource.Resource) (string, bool, error) {
	if r.Kind != octavia.KindMember {
		return o.c.Observe(ctx, r)
	}
	pool, ok := octaviaplan.MemberPool(r.Logical)
	if !ok {
		return "", false, fmt.Errorf("member %q: its logical name names no pool", r.Logical)
	}
	poolID, ok := o.poolIDs[pool]
	if !ok {
		return "", false, fmt.Errorf("member %q: its pool %q is not in the run record", r.Logical, pool)
	}
	return o.c.ObserveMember(ctx, r, poolID)
}

// recordedPoolIDs maps the logical name of every pool in created to its cloud
// id, the lookup octaviaObserver resolves a member's pool through.
func recordedPoolIDs(created []resource.Resource) map[string]string {
	ids := make(map[string]string)
	for _, r := range created {
		if r.Kind == octavia.KindPool {
			ids[r.Logical] = r.ID
		}
	}
	return ids
}
