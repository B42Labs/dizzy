package main

import (
	"context"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/barbican"
	barbicanexec "github.com/B42Labs/dizzy/internal/barbican/executor"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// newBarbicanCmd builds the "barbican" command namespace and attaches its
// subcommands: generate, apply, status, report, and cleanup. report is the same
// service-agnostic builder the other namespaces use.
func newBarbicanCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "barbican",
		Short: "Barbican (key manager) secret lifecycle load and consistency commands",
	}

	cmd.AddCommand(
		newBarbicanGenerateCmd(opts),
		newBarbicanApplyCmd(opts),
		newBarbicanStatusCmd(opts),
		newReportCmd(opts),
		newBarbicanCleanupCmd(opts),
	)

	return cmd
}

// barbicanTimeoutCleaner wraps a barbicanexec.Cleaner so every cloud operation
// Cleanup performs is bounded by opTimeout. Cleanup runs each call on the context
// it is handed, but the abort teardown runs on a context.WithoutCancel that
// carries no deadline, and the gophercloud client sets no HTTP timeout of its
// own. Without this a wedged key manager call would hang the teardown
// indefinitely. Each call gets its own timeout, so a large teardown is bounded
// per operation, the same way apply bounds each create. Secret discovery is the
// exception, bounded per request by the client instead (see ListSecretsByRun).
// It is the key manager twin of glanceTimeoutCleaner.
type barbicanTimeoutCleaner struct {
	inner     barbicanexec.Cleaner
	opTimeout time.Duration
}

func (t barbicanTimeoutCleaner) ListContainersByRun(ctx context.Context, runID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListContainersByRun(ctx, runID)
}

func (t barbicanTimeoutCleaner) ListOrdersByRun(ctx context.Context, runID string) (orders, secrets []resource.Resource, err error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListOrdersByRun(ctx, runID)
}

// newBarbicanClient builds the Barbican client a command drives, with every
// discovery listing and secret metadata read bounded by opTimeout
// (barbican.Client.SetRequestTimeout). barbicanTimeoutCleaner leaves secret
// discovery to that bound, so every command builds its client here rather than
// with barbican.New.
func newBarbicanClient(kv *gophercloud.ServiceClient, runID string, m *metrics.Collector, opTimeout time.Duration) *barbican.Client {
	c := barbican.New(kv, runID, m)
	c.SetRequestTimeout(opTimeout)
	return c
}

// ListSecretsByRun passes ctx through without a timeout of its own. Secret
// discovery is one listing plus one metadata read per prefix-matching secret,
// so its duration grows with the run, and a single opTimeout over the whole
// call would fail a large run's discovery, stopping cleanup before any secret
// is deleted. Every command builds its client with newBarbicanClient, whose
// request timeout is the same opTimeout and bounds the listing and each
// metadata read on its own, so a wedged request still cannot hang the teardown.
func (t barbicanTimeoutCleaner) ListSecretsByRun(ctx context.Context, runID string) ([]resource.Resource, error) {
	return t.inner.ListSecretsByRun(ctx, runID)
}

func (t barbicanTimeoutCleaner) Delete(ctx context.Context, r resource.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.Delete(ctx, r)
}
