package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/horizon"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
)

// newHorizonStatusCmd builds "horizon status", which loads a horizon run
// record and re-queries in Keystone every user and project the run created,
// printing a table of logical name, kind, id, and state: enabled, disabled,
// or gone. A record without created resources, which every run of the
// clouds.yaml user writes, prints the header alone and reads no clouds.yaml.
// It sends no Horizon request: sessions are not recorded.
func newHorizonStatusCmd(opts *globalOptions) *cobra.Command {
	var runPath string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Re-query in Keystone the users and projects a horizon run created",
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := run.Load(runPath)
			if err != nil {
				return err
			}
			if err := requireService(rec, "horizon"); err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			if len(rec.Created) == 0 {
				return writeStatusTable(ctx, cmd, nil, nil)
			}

			client, err := newHorizonIdentityClient(ctx, opts, rec.RunID, metrics.NewCollector())
			if err != nil {
				return err
			}
			return writeStatusTable(ctx, cmd, horizonTimeoutObserver{client, opts.timeout}, rec.Created)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) to re-query (required)")
	// MarkFlagRequired only fails for an unknown flag; "run" was just added.
	_ = cmd.MarkFlagRequired("run")

	return cmd
}

// newHorizonIdentityClient authenticates the clouds.yaml entry and returns a
// Horizon client for runID that only speaks to Keystone, as status and
// cleanup need, recording into m.
func newHorizonIdentityClient(ctx context.Context, opts *globalOptions, runID string, m *metrics.Collector) (*horizon.Client, error) {
	gc, err := config.NewIdentityClient(ctx, opts.osCloud)
	if err != nil {
		return nil, fmt.Errorf("creating identity client: %w", err)
	}
	c := horizon.New(&url.URL{}, nil, runID, m)
	c.SetIdentity(gc)
	return c, nil
}

// horizonTimeoutObserver bounds each Observe by its own timeout, so a
// Keystone that does not answer cannot hang horizon status.
type horizonTimeoutObserver struct {
	c *horizon.Client
	d time.Duration
}

func (o horizonTimeoutObserver) Observe(ctx context.Context, r resource.Resource) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, o.d)
	defer cancel()
	return o.c.Observe(ctx, r)
}
