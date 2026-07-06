package neutron

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"

	"github.com/B42Labs/dizzy/internal/plan"
)

// CreateNetwork creates a tagged tenant network.
func (c *Client) CreateNetwork(ctx context.Context, n plan.Network) (Resource, error) {
	return c.createTagged(ctx, KindNetwork, n.Name, func(ctx context.Context, name string) (string, error) {
		created, err := networks.Create(ctx, c.gc, networks.CreateOpts{Name: name}).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}
