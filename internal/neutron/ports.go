package neutron

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"

	"github.com/B42Labs/dizzy/internal/plan"
)

// CreatePort creates a tagged port on networkID. subnetIDByLogical resolves each
// of the plan's fixed-IP subnet references to a cloud id; sgIDs are the resolved
// security-group ids to attach. Empty fixed IPs or security groups are omitted
// so Neutron applies its defaults (auto-allocated address, default group).
func (c *Client) CreatePort(ctx context.Context, p plan.Port, networkID string, subnetIDByLogical map[string]string, sgIDs []string) (Resource, error) {
	return c.createTagged(ctx, KindPort, p.Name, func(ctx context.Context, name string) (string, error) {
		opts := ports.CreateOpts{NetworkID: networkID, Name: name}

		if len(p.FixedIPs) > 0 {
			fixedIPs := make([]ports.IP, 0, len(p.FixedIPs))
			for _, fip := range p.FixedIPs {
				fixedIPs = append(fixedIPs, ports.IP{
					SubnetID:  subnetIDByLogical[fip.Subnet],
					IPAddress: fip.IPAddress,
				})
			}
			opts.FixedIPs = fixedIPs
		}
		if len(sgIDs) > 0 {
			opts.SecurityGroups = &sgIDs
		}

		created, err := ports.Create(ctx, c.gc, opts).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}
