package neutron

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"

	"github.com/B42Labs/openstack-tester/internal/plan"
)

// CreateFloatingIP allocates a tagged floating IP from externalNetworkID. When
// portID is non-empty the floating IP is associated with that internal port at
// creation time; otherwise it is allocated but left unassociated. The caller
// (the executor) only reaches this path when an external network is available,
// so externalNetworkID is always set here.
func (c *Client) CreateFloatingIP(ctx context.Context, fip plan.FloatingIP, externalNetworkID, portID string) (Resource, error) {
	return c.createTagged(ctx, KindFloatingIP, fip.Name, func(ctx context.Context, name string) (string, error) {
		opts := floatingips.CreateOpts{
			Description:       name,
			FloatingNetworkID: externalNetworkID,
			PortID:            portID,
		}
		created, err := floatingips.Create(ctx, c.gc, opts).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}
