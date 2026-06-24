package neutron

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"

	"github.com/B42Labs/openstack-tester/internal/plan"
)

// CreateRouter creates a tagged router. When the plan marks the router as
// wanting an external gateway and externalNetworkID is non-empty (an external
// network was discovered at apply time), the router is created already plugged
// into that network; otherwise it is a plain internal router. An empty
// externalNetworkID — no external network available — yields no gateway, so the
// router's external-gateway intent is silently a no-op rather than a failure.
func (c *Client) CreateRouter(ctx context.Context, r plan.Router, externalNetworkID string) (Resource, error) {
	return c.createTagged(ctx, KindRouter, r.Name, func(ctx context.Context, name string) (string, error) {
		opts := routers.CreateOpts{Name: name}
		if r.ExternalGateway && externalNetworkID != "" {
			opts.GatewayInfo = &routers.GatewayInfo{NetworkID: externalNetworkID}
		}
		created, err := routers.Create(ctx, c.gc, opts).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}

// CreateRouterInterface attaches a router to a subnet (subnetID) or to an
// existing port (portID) — exactly one is non-empty, mirroring the plan's
// RouterInterface. A subnet attachment takes the subnet's gateway address; a
// port attachment wires an already-created port into the router (used to link
// two routers over a shared transit subnet). The interface's resulting port id
// is recorded so a later cleanup can detach it. The Resource's Name field is
// left empty because no separately-named cloud object exists.
func (c *Client) CreateRouterInterface(ctx context.Context, ri plan.RouterInterface, routerID, subnetID, portID string) (Resource, error) {
	var resultPortID string
	err := c.timed(ctx, string(KindRouterInterface), func(ctx context.Context) error {
		opts := routers.AddInterfaceOpts{SubnetID: subnetID, PortID: portID}
		info, addErr := routers.AddInterface(ctx, c.gc, routerID, opts).Extract()
		if addErr != nil {
			return addErr
		}
		resultPortID = info.PortID
		return nil
	})
	if err != nil {
		return Resource{}, wrapCreate(KindRouterInterface, ri.Name, err)
	}
	return Resource{Kind: KindRouterInterface, Logical: ri.Name, ID: resultPortID}, nil
}

// RemoveRouterInterface detaches a single interface from a router, mirroring
// CreateRouterInterface: exactly one of subnetID or portID identifies the
// interface to remove. It is the per-interface counterpart to cleanup's
// DetachRouterInterfaces (which removes all of a router's interfaces); the chaos
// churn engine needs it to delete one interface at a time, since the generic
// Delete cannot remove a router interface. The call is timed under
// KindRouterInterface and returns the error unwrapped enough for IsNotFound to
// classify an already-detached interface so the caller can treat it as success.
func (c *Client) RemoveRouterInterface(ctx context.Context, routerID, subnetID, portID string) error {
	err := c.timed(ctx, string(KindRouterInterface), func(ctx context.Context) error {
		opts := routers.RemoveInterfaceOpts{SubnetID: subnetID, PortID: portID}
		_, rmErr := routers.RemoveInterface(ctx, c.gc, routerID, opts).Extract()
		return rmErr
	})
	if err != nil {
		return fmt.Errorf("removing interface from router %s: %w", routerID, err)
	}
	return nil
}
