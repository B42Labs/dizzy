package neutron

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/addressscopes"

	"github.com/B42Labs/dizzy/internal/plan"
)

// CreateAddressScope creates a tagged address scope. Tagging is best-effort for
// this kind (see createTagged).
func (c *Client) CreateAddressScope(ctx context.Context, as plan.AddressScope) (Resource, error) {
	return c.createTagged(ctx, KindAddressScope, as.Name, func(ctx context.Context, name string) (string, error) {
		created, err := addressscopes.Create(ctx, c.gc, addressscopes.CreateOpts{
			Name:      name,
			IPVersion: as.IPVersion,
		}).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}
