package executor

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/B42Labs/dizzy/internal/neutron"
)

// Cleaner is the tag-scoped teardown surface Cleanup drives: discover a run's
// resources by tag, detach a router's interfaces, and delete a resource. Like
// Neutron it is the single ports-and-adapters seam to the cloud; *neutron.Client
// satisfies it in production and a fake satisfies it in tests.
type Cleaner interface {
	ListByTag(ctx context.Context, kind neutron.Kind, runID string) ([]neutron.Resource, error)
	DetachRouterInterfaces(ctx context.Context, routerID string) (int, error)
	DeleteNetworkPorts(ctx context.Context, networkID string) (int, error)
	Delete(ctx context.Context, r neutron.Resource) error
}

// Cleanup deletes every resource a run created in reverse dependency order,
// returning the number deleted. Tag-discoverable kinds are found by the run's
// dizzy:run=<id> tag, so it never touches resources the tool did not create;
// address scopes, which Neutron may not let us tag (and which gophercloud cannot
// filter by tag), are reclaimed instead from recorded — the run record's created
// list — by id. recorded is nil when cleanup runs from a bare run id (--run-id),
// in which case address scopes cannot be reclaimed. It treats an already-gone
// resource as success — so running it twice is a no-op. Floating IPs are removed
// first (an associated one pins its port and router); router interfaces are
// detached (not deleted) before the ports they attach are removed; a network is
// deleted before its subnets so its delete cascades the auto-created DHCP/service
// ports that would otherwise block an explicit subnet delete (SubnetInUse);
// security-group rules are not handled because they cascade with their group.
// The first non-404 error stops the run and is returned with the count deleted
// so far.
func Cleanup(ctx context.Context, c Cleaner, runID string, recorded []neutron.Resource) (int, error) {
	var deleted int

	// Floating IPs first: an associated floating IP pins its internal port and
	// is routed through a router's gateway, blocking both from deletion.
	n, err := deleteKind(ctx, c, neutron.KindFloatingIP, runID)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Detach every tagged router's interfaces before its ports, subnets, or the
	// router itself can be deleted. Detaching a port-based interface removes (or
	// frees) the transit port, so it must precede the port deletion below. The
	// router list is reused to delete the routers further down, avoiding a second
	// tag query.
	routers, err := c.ListByTag(ctx, neutron.KindRouter, runID)
	if err != nil {
		return deleted, err
	}
	for _, r := range routers {
		if _, err := c.DetachRouterInterfaces(ctx, r.ID); err != nil {
			return deleted, err
		}
	}

	// Ports: they pin subnet IPs and belong to networks. Any transit port a
	// port-based interface used is already detached above (a re-delete 404s and
	// is treated as success), so this removes the remaining standalone ports.
	n, err = deleteKind(ctx, c, neutron.KindPort, runID)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Networks before subnets: deleting a network cascades its subnets and the
	// service ports Neutron auto-created on them — notably the DHCP port, which
	// holds an IP allocation and so makes an explicit subnet delete fail with
	// SubnetInUse. Before each network delete, sweep any plain ports left on it
	// (untagged orphans a cancelled run can leave behind) so the delete is not
	// blocked by NetworkInUse; the cascade then takes the subnets with it.
	nets, err := c.ListByTag(ctx, neutron.KindNetwork, runID)
	if err != nil {
		return deleted, err
	}
	for _, nw := range nets {
		swept, err := c.DeleteNetworkPorts(ctx, nw.ID)
		deleted += swept
		if err != nil {
			return deleted, err
		}
	}
	n, err = deleteResources(ctx, c, nets)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Subnets: the network cascade above already removed any whose network we
	// own, so this is normally an idempotent 404 sweep. It still covers a subnet
	// whose network delete was somehow skipped, and keeps cleanup correct for
	// callers (and tests) where networks do not cascade.
	n, err = deleteKind(ctx, c, neutron.KindSubnet, runID)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Security groups: independent of the L3 chain once their ports are gone.
	n, err = deleteKind(ctx, c, neutron.KindSecurityGroup, runID)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Routers (now interface-free; any external-gateway port is removed with the
	// router, and floating IPs routed through it are already gone).
	n, err = deleteResources(ctx, c, routers)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Subnet pools: subnets allocate their CIDRs from them and are gone (via the
	// network cascade) by now, so the pools are free to delete.
	n, err = deleteKind(ctx, c, neutron.KindSubnetPool, runID)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Address scopes last, after the subnet pools that reference them. They
	// cannot be discovered by tag, so they are reclaimed from the run record by
	// id; with no record (recorded is nil) there is nothing to reclaim here.
	n, err = deleteResources(ctx, c, recordedOfKind(recorded, neutron.KindAddressScope))
	deleted += n
	if err != nil {
		return deleted, err
	}

	return deleted, nil
}

// recordedOfKind returns the resources of kind from a run record's created list.
// It is the discovery path for kinds that cannot be found by tag (address
// scopes, which Neutron may not let us tag): cleanup deletes them by the id the
// record captured at apply time.
func recordedOfKind(recorded []neutron.Resource, kind neutron.Kind) []neutron.Resource {
	var out []neutron.Resource
	for _, r := range recorded {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// deleteKind lists every resource of kind tagged for runID and deletes each.
func deleteKind(ctx context.Context, c Cleaner, kind neutron.Kind, runID string) (int, error) {
	resources, err := c.ListByTag(ctx, kind, runID)
	if err != nil {
		return 0, err
	}
	return deleteResources(ctx, c, resources)
}

// deleteResources deletes each resource, treating an already-gone resource (a
// 404) as success so cleanup is idempotent. It returns the number actually
// deleted, so a no-op second sweep returns zero.
func deleteResources(ctx context.Context, c Cleaner, resources []neutron.Resource) (int, error) {
	var deleted int
	for _, r := range resources {
		// Announce each delete so a teardown shows what it is removing instead of
		// going silent until its final count. Logged at info (per resource);
		// silence it with --log-level warn.
		slog.Info("deleting resource", "kind", r.Kind, "id", r.ID)
		if err := c.Delete(ctx, r); err != nil {
			if neutron.IsNotFound(err) {
				continue
			}
			return deleted, fmt.Errorf("deleting %s %s: %w", r.Kind, r.ID, err)
		}
		deleted++
	}
	return deleted, nil
}
