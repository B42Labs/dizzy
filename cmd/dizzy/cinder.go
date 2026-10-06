package main

import (
	"context"
	"log/slog"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/cinder"
	cinderplan "github.com/B42Labs/dizzy/internal/cinder/plan"
)

// newCinderCmd builds the "cinder" command namespace and attaches its
// subcommands. generate, apply, chaos, monitor, status, report, and cleanup are
// implemented. report is the same service-agnostic builder the neutron namespace
// uses.
func newCinderCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cinder",
		Short: "Cinder (block storage) load and consistency commands",
	}

	cmd.AddCommand(
		newCinderGenerateCmd(opts),
		newCinderApplyCmd(opts),
		newCinderChaosCmd(opts),
		newCinderMonitorCmd(opts),
		newCinderStatusCmd(opts),
		newReportCmd(opts),
		newCinderCleanupCmd(opts),
	)

	return cmd
}

// precheckCinder runs the read-only pre-checks of a Cinder run against p,
// before anything is created. Like Neutron's external network, the volume type
// is a property of the target cloud, not of the cloud-independent plan: a
// named volumeType is resolved, an error when it does not exist, and
// normalized to its resolved name so the per-type quota keys (volumes_<name>)
// and the run record use it; empty means the cloud's default type. It then
// pre-checks the volume quota, the per-type quotas too when a type is set,
// turning a late, messy quota failure into an early, clear one. It returns the
// volume type to create volumes with.
func precheckCinder(ctx context.Context, gc *gophercloud.ServiceClient, volumeType string, p *cinderplan.Plan) (string, error) {
	if volumeType != "" {
		vt, err := cinder.FindVolumeType(ctx, gc, volumeType)
		if err != nil {
			return "", err
		}
		volumeType = vt.Name
		slog.Info("using volume type", "name", vt.Name, "id", vt.ID)
	}
	if err := cinder.PrecheckQuota(ctx, gc, p, volumeType); err != nil {
		return "", err
	}
	return volumeType, nil
}
