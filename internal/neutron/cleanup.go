package neutron

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/subnetpools"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

// ListByTag returns the resources of kind carrying this run's
// ostester:run=<runID> tag, the discovery step tag-based cleanup deletes from.
// It supports the tag-discoverable kinds (networks, subnets, routers, security
// groups, subnet pools, ports); other kinds return an error. The filter is
// applied server-side, so the result never includes resources the tool did not
// create. Returned Resources carry the kind, cloud name, and id needed to delete
// them.
func (c *Client) ListByTag(ctx context.Context, kind Kind, runID string) ([]Resource, error) {
	tag := "ostester:run=" + runID
	var found []Resource
	err := c.timed(ctx, string(kind), func(ctx context.Context) error {
		var listErr error
		found, listErr = c.listByTag(ctx, kind, tag)
		return listErr
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s by tag: %w", kind, err)
	}
	return found, nil
}

// listByTag performs the per-kind tagged list without recording a sample;
// ListByTag wraps it through c.timed. Each kind uses its own typed ListOpts and
// extractor, so the switch arms cannot merge; the shared AllPages/Extract/collect
// body is factored into listTagged.
func (c *Client) listByTag(ctx context.Context, kind Kind, tag string) ([]Resource, error) {
	switch kind {
	case KindNetwork:
		return listTagged(ctx, kind, networks.List(c.gc, networks.ListOpts{Tags: tag}),
			networks.ExtractNetworks, func(it networks.Network) (string, string) { return it.Name, it.ID })
	case KindSubnet:
		return listTagged(ctx, kind, subnets.List(c.gc, subnets.ListOpts{Tags: tag}),
			subnets.ExtractSubnets, func(it subnets.Subnet) (string, string) { return it.Name, it.ID })
	case KindRouter:
		return listTagged(ctx, kind, routers.List(c.gc, routers.ListOpts{Tags: tag}),
			routers.ExtractRouters, func(it routers.Router) (string, string) { return it.Name, it.ID })
	case KindSecurityGroup:
		return listTagged(ctx, kind, groups.List(c.gc, groups.ListOpts{Tags: tag}),
			groups.ExtractGroups, func(it groups.SecGroup) (string, string) { return it.Name, it.ID })
	case KindSubnetPool:
		return listTagged(ctx, kind, subnetpools.List(c.gc, subnetpools.ListOpts{Tags: tag}),
			subnetpools.ExtractSubnetPools, func(it subnetpools.SubnetPool) (string, string) { return it.Name, it.ID })
	case KindPort:
		return listTagged(ctx, kind, ports.List(c.gc, ports.ListOpts{Tags: tag}),
			ports.ExtractPorts, func(it ports.Port) (string, string) { return it.Name, it.ID })
	case KindFloatingIP:
		return listTagged(ctx, kind, floatingips.List(c.gc, floatingips.ListOpts{Tags: tag}),
			floatingips.ExtractFloatingIPs, func(it floatingips.FloatingIP) (string, string) { return it.Description, it.ID })
	default:
		return nil, fmt.Errorf("list by tag not supported for kind %q", kind)
	}
}

// listTagged runs a tagged list pager to completion and collects the results into
// Resources of kind. It performs the AllPages/Extract/allocate/append body shared
// by every arm of listByTag; nameID pulls the cloud name and id from each typed item.
func listTagged[T any](
	ctx context.Context,
	kind Kind,
	pager pagination.Pager,
	extract func(pagination.Page) ([]T, error),
	nameID func(T) (string, string),
) ([]Resource, error) {
	pages, err := pager.AllPages(ctx)
	if err != nil {
		return nil, err
	}
	items, err := extract(pages)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(items))
	for _, it := range items {
		name, id := nameID(it)
		out = append(out, Resource{Kind: kind, Name: name, ID: id})
	}
	return out, nil
}

// DeleteNetworkPorts deletes the plain ports left on networkID — those with an
// empty device_owner — and returns how many it removed. These are the ports the
// run created on its own network that tag-based discovery can miss: a cancelled
// run can create a port and then lose the context before tagging (and before the
// rollback), leaving an untagged orphan that would otherwise block the network
// delete with NetworkInUse. Ports with a device owner are left alone: router
// interface and gateway ports are detached separately, and Neutron's own service
// ports (DHCP/metadata) are removed by the network delete that follows. A port
// already gone (404) is skipped so repeated cleanup stays idempotent.
func (c *Client) DeleteNetworkPorts(ctx context.Context, networkID string) (int, error) {
	var deleted int
	err := c.timed(ctx, string(KindPort), func(ctx context.Context) error {
		pages, err := ports.List(c.gc, ports.ListOpts{NetworkID: networkID}).AllPages(ctx)
		if err != nil {
			return err
		}
		items, err := ports.ExtractPorts(pages)
		if err != nil {
			return err
		}
		for _, p := range items {
			if p.DeviceOwner != "" {
				continue
			}
			if err := ports.Delete(ctx, c.gc, p.ID).ExtractErr(); err != nil {
				if IsNotFound(err) {
					continue
				}
				return err
			}
			deleted++
		}
		return nil
	})
	if err != nil {
		return deleted, fmt.Errorf("deleting ports on network %s: %w", networkID, err)
	}
	return deleted, nil
}

// DetachRouterInterfaces detaches every interface port from routerID, returning
// the number detached. Interface ports are owned by the router and are not
// tagged, so they are found by listing the router's ports filtered to the
// interface device owner and removed with RemoveInterface (they cannot be deleted
// directly). The device-owner filter excludes a router's gateway port
// (network:router_gateway) and Neutron's internal HA ports, which RemoveInterface
// cannot detach; without it a router that ever carries such a port would abort
// cleanup at the router stage. A port that is already gone (404) is skipped so
// repeated cleanup stays idempotent.
func (c *Client) DetachRouterInterfaces(ctx context.Context, routerID string) (int, error) {
	var detached int
	err := c.timed(ctx, string(KindRouterInterface), func(ctx context.Context) error {
		pages, err := ports.List(c.gc, ports.ListOpts{DeviceID: routerID, DeviceOwner: "network:router_interface"}).AllPages(ctx)
		if err != nil {
			return err
		}
		items, err := ports.ExtractPorts(pages)
		if err != nil {
			return err
		}
		for _, p := range items {
			opts := routers.RemoveInterfaceOpts{PortID: p.ID}
			if _, err := routers.RemoveInterface(ctx, c.gc, routerID, opts).Extract(); err != nil {
				if IsNotFound(err) {
					continue
				}
				return err
			}
			detached++
		}
		return nil
	})
	if err != nil {
		return detached, fmt.Errorf("detaching interfaces from router %s: %w", routerID, err)
	}
	return detached, nil
}
