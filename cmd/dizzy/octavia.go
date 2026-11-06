package main

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	octaviaexec "github.com/B42Labs/dizzy/internal/octavia/executor"
	"github.com/B42Labs/dizzy/internal/resource"
)

// newOctaviaCmd builds the "octavia" command namespace and attaches its
// subcommands: generate, apply, chaos, status, report, and cleanup. report is
// the same service-agnostic builder the other namespaces use. The namespace has
// no monitor command yet; that driver builds on this one's plan, client, and
// cleanup.
func newOctaviaCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "octavia",
		Short: "Octavia (load balancer) lifecycle load and consistency commands",
	}

	cmd.AddCommand(
		newOctaviaGenerateCmd(opts),
		newOctaviaApplyCmd(opts),
		newOctaviaChaosCmd(opts),
		newOctaviaStatusCmd(opts),
		newReportCmd(opts),
		newOctaviaCleanupCmd(opts),
	)

	return cmd
}

// octaviaTimeoutCleaner wraps an octaviaexec.Cleaner so every cloud operation
// Cleanup performs is bounded by opTimeout. Cleanup runs each call on the
// context it is handed, but the abort teardown runs on a context.WithoutCancel
// that carries no deadline, and the gophercloud client sets no HTTP timeout of
// its own. Without this a wedged load balancer or network call would hang the
// teardown indefinitely. Each call gets its own timeout, so a large teardown is
// bounded per operation, the same way apply bounds each create; Cleanup still
// bounds a load balancer's whole deletion wait by opTimeout on its own. It is
// the load balancer twin of novaTimeoutCleaner.
type octaviaTimeoutCleaner struct {
	inner     octaviaexec.Cleaner
	opTimeout time.Duration
}

// ListByTag discovers the run's resources of kind, bounded by opTimeout.
func (t octaviaTimeoutCleaner) ListByTag(ctx context.Context, kind resource.Kind, runID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListByTag(ctx, kind, runID)
}

// Delete sends one delete of r, bounded by opTimeout.
func (t octaviaTimeoutCleaner) Delete(ctx context.Context, r resource.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.Delete(ctx, r)
}

// WaitForGone waits for the load balancer r to be deleted, bounded by
// opTimeout.
func (t octaviaTimeoutCleaner) WaitForGone(ctx context.Context, r resource.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.WaitForGone(ctx, r)
}
