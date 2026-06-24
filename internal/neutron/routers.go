package neutron

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"

	"github.com/B42Labs/openstack-tester/internal/plan"
)

// CreateRouter creates a tagged internal router (no external gateway in
// Phase 1).
func (c *Client) CreateRouter(ctx context.Context, r plan.Router) (Resource, error) {
	return c.createTagged(ctx, KindRouter, r.Name, func(ctx context.Context, name string) (string, error) {
		created, err := routers.Create(ctx, c.gc, routers.CreateOpts{Name: name}).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}

// CreateRouterInterface attaches subnetID to routerID. The interface's port
// belongs to the router and is not independently named or tagged; the returned
// Resource records the port id so a later cleanup can detach it. The Resource's
// Name field is left empty because no separately-named cloud object exists.
func (c *Client) CreateRouterInterface(ctx context.Context, ri plan.RouterInterface, routerID, subnetID string) (Resource, error) {
	var portID string
	err := c.timed(ctx, string(KindRouterInterface), func(ctx context.Context) error {
		info, addErr := routers.AddInterface(ctx, c.gc, routerID, routers.AddInterfaceOpts{SubnetID: subnetID}).Extract()
		if addErr != nil {
			return addErr
		}
		portID = info.PortID
		return nil
	})
	if err != nil {
		return Resource{}, wrapCreate(KindRouterInterface, ri.Name, err)
	}
	return Resource{Kind: KindRouterInterface, Logical: ri.Name, ID: portID}, nil
}
