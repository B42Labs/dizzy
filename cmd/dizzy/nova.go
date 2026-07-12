package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/nova"
	novaexec "github.com/B42Labs/dizzy/internal/nova/executor"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// newNovaCmd builds the "nova" command namespace and attaches its subcommands.
// generate, apply, chaos, monitor, status, report, and cleanup are implemented.
// report is the same service-agnostic builder the other namespaces use.
func newNovaCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nova",
		Short: "Nova (compute) server lifecycle load and consistency commands",
	}

	cmd.AddCommand(
		newNovaGenerateCmd(opts),
		newNovaApplyCmd(opts),
		newNovaChaosCmd(opts),
		newNovaMonitorCmd(opts),
		newNovaStatusCmd(opts),
		newReportCmd(opts),
		newNovaCleanupCmd(opts),
	)

	return cmd
}

// resolveNovaRefs resolves the plan's by-name image and flavor references to
// cloud ids, pre-checks the compute quota, and runs the live-migration admin
// pre-check when the plan schedules any live migration. It is shared by apply,
// chaos, and monitor. The image is resolved via Glance and the flavors via Nova;
// the resize flavor is only resolved when the plan resizes anything. A false
// live-migration verdict logs a warning and leaves live migration disabled for
// the run rather than aborting it — the fail-open behavior the issue requires.
func resolveNovaRefs(ctx context.Context, cs *config.ComputeStack, p *novaplan.Plan) (novaexec.Resolved, error) {
	img, err := nova.FindImage(ctx, cs.Image, p.Image)
	if err != nil {
		return novaexec.Resolved{}, err
	}
	bootFlavor, err := nova.FindFlavor(ctx, cs.Compute, p.Flavor)
	if err != nil {
		return novaexec.Resolved{}, err
	}
	resolved := novaexec.Resolved{ImageID: img.ID, FlavorID: bootFlavor.ID}
	slog.Info("resolved image and flavor", "image", img.Name, "imageID", img.ID, "flavor", bootFlavor.Name, "flavorID", bootFlavor.ID)

	var resizeFlavor nova.Flavor
	if p.Resizes() > 0 {
		resizeFlavor, err = nova.FindFlavor(ctx, cs.Compute, p.ResizeFlavor)
		if err != nil {
			return novaexec.Resolved{}, err
		}
		resolved.ResizeFlavorID = resizeFlavor.ID
		slog.Info("resolved resize flavor", "flavor", resizeFlavor.Name, "flavorID", resizeFlavor.ID)
	}

	if err := nova.PrecheckQuota(ctx, cs.Compute, p, bootFlavor, resizeFlavor); err != nil {
		return novaexec.Resolved{}, err
	}

	if p.LiveMigrations() > 0 {
		if ok, reason := nova.PrecheckLiveMigration(ctx, cs.Compute); ok {
			resolved.LiveMigration = true
			slog.Info("live migration enabled for this run")
		} else {
			slog.Warn("live migration disabled for this run", "reason", reason)
		}
	}

	return resolved, nil
}

// novaTimeoutCleaner wraps a novaexec.Cleaner so every cloud operation Cleanup
// performs is bounded by opTimeout. Cleanup runs each call on the context it is
// handed, but the monitor loop's context carries no deadline and teardown runs
// on a context.WithoutCancel that strips one anyway; the gophercloud client sets
// no HTTP timeout of its own. Without this a wedged compute call would hang the
// iteration — and so the whole loop — indefinitely. Each call gets its own
// timeout, so a large teardown is bounded per operation, the same way apply
// bounds each create. It is the compute twin of cinderTimeoutCleaner.
type novaTimeoutCleaner struct {
	inner     novaexec.Cleaner
	opTimeout time.Duration
}

func (t novaTimeoutCleaner) ListServersByMetadata(ctx context.Context, runID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListServersByMetadata(ctx, runID)
}

func (t novaTimeoutCleaner) ListVolumesByMetadata(ctx context.Context, runID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListVolumesByMetadata(ctx, runID)
}

func (t novaTimeoutCleaner) ListByTag(ctx context.Context, kind resource.Kind, runID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListByTag(ctx, kind, runID)
}

func (t novaTimeoutCleaner) DeleteNetworkPorts(ctx context.Context, networkID string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.DeleteNetworkPorts(ctx, networkID)
}

func (t novaTimeoutCleaner) Delete(ctx context.Context, r resource.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.Delete(ctx, r)
}

func (t novaTimeoutCleaner) WaitForGone(ctx context.Context, r resource.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.WaitForGone(ctx, r)
}
