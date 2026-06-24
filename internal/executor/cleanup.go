package executor

import (
	"context"
	"fmt"

	"github.com/B42Labs/openstack-tester/internal/neutron"
)

// Cleaner is the tag-scoped teardown surface Cleanup drives: discover a run's
// resources by tag, detach a router's interfaces, and delete a resource. Like
// Neutron it is the single ports-and-adapters seam to the cloud; *neutron.Client
// satisfies it in production and a fake satisfies it in tests.
type Cleaner interface {
	ListByTag(ctx context.Context, kind neutron.Kind, runID string) ([]neutron.Resource, error)
	DetachRouterInterfaces(ctx context.Context, routerID string) (int, error)
	Delete(ctx context.Context, r neutron.Resource) error
}

// Cleanup deletes every resource tagged for runID in reverse dependency order,
// returning the number deleted. It removes only tag-matched resources, so it
// never touches resources the tool did not create, and it treats an
// already-gone resource as success — so running it twice is a no-op. Router
// interfaces are detached (not deleted) before the routers and subnets they
// attach can be removed; security-group rules are not handled because they
// cascade with their group. The first non-404 error stops the run and is
// returned with the count deleted so far.
func Cleanup(ctx context.Context, c Cleaner, runID string) (int, error) {
	var deleted int

	// Ports first: they pin subnet IPs and belong to networks.
	n, err := deleteKind(ctx, c, neutron.KindPort, runID)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Detach every tagged router's interfaces before its subnets or the router
	// itself can be deleted. The router list is reused to delete the routers
	// below, avoiding a second tag query.
	routers, err := c.ListByTag(ctx, neutron.KindRouter, runID)
	if err != nil {
		return deleted, err
	}
	for _, r := range routers {
		if _, err := c.DetachRouterInterfaces(ctx, r.ID); err != nil {
			return deleted, err
		}
	}

	// Subnets (now interface-free) before their networks and subnet pools.
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

	// Routers (now interface-free).
	n, err = deleteResources(ctx, c, routers)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Networks after their subnets and ports.
	n, err = deleteKind(ctx, c, neutron.KindNetwork, runID)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Subnet pools last: subnets allocate their CIDRs from them.
	n, err = deleteKind(ctx, c, neutron.KindSubnetPool, runID)
	deleted += n
	if err != nil {
		return deleted, err
	}

	return deleted, nil
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
