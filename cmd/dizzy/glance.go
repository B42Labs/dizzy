package main

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	glanceexec "github.com/B42Labs/dizzy/internal/glance/executor"
	"github.com/B42Labs/dizzy/internal/resource"
)

// newGlanceCmd builds the "glance" command namespace and attaches its
// subcommands. generate, apply, chaos, monitor, status, report, and cleanup are
// implemented. report is the same service-agnostic builder the other namespaces
// use.
func newGlanceCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "glance",
		Short: "Glance (image) lifecycle load and consistency commands",
	}

	cmd.AddCommand(
		newGlanceGenerateCmd(opts),
		newGlanceApplyCmd(opts),
		newGlanceChaosCmd(opts),
		newGlanceMonitorCmd(opts),
		newGlanceStatusCmd(opts),
		newReportCmd(opts),
		newGlanceCleanupCmd(opts),
	)

	return cmd
}

// glanceTimeoutCleaner wraps a glanceexec.Cleaner so every cloud operation
// Cleanup performs is bounded by opTimeout. Cleanup runs each call on the context
// it is handed, but the monitor loop's context carries no deadline and teardown
// runs on a context.WithoutCancel that strips one anyway; the gophercloud client
// sets no HTTP timeout of its own. Without this a wedged image call would hang
// the iteration — and so the whole loop — indefinitely. Each call gets its own
// timeout, so a large teardown is bounded per operation, the same way apply
// bounds each create. It is the image twin of novaTimeoutCleaner.
type glanceTimeoutCleaner struct {
	inner     glanceexec.Cleaner
	opTimeout time.Duration
}

func (t glanceTimeoutCleaner) ListImagesByTag(ctx context.Context, runID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListImagesByTag(ctx, runID)
}

func (t glanceTimeoutCleaner) Delete(ctx context.Context, r resource.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.Delete(ctx, r)
}

func (t glanceTimeoutCleaner) WaitForGone(ctx context.Context, r resource.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.WaitForGone(ctx, r)
}
