package neutron

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/subnetpools"

	"github.com/B42Labs/dizzy/internal/plan"
)

// CreateSubnetPool creates a tagged subnet pool. addressScopeID is empty when
// the pool does not belong to an address scope.
func (c *Client) CreateSubnetPool(ctx context.Context, sp plan.SubnetPool, addressScopeID string) (Resource, error) {
	return c.createTagged(ctx, KindSubnetPool, sp.Name, func(ctx context.Context, name string) (string, error) {
		created, err := subnetpools.Create(ctx, c.gc, subnetpools.CreateOpts{
			Name:             name,
			Prefixes:         sp.Prefixes,
			DefaultPrefixLen: sp.DefaultPrefixLen,
			AddressScopeID:   addressScopeID,
		}).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}
