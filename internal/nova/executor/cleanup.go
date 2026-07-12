package executor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/B42Labs/dizzy/internal/nova"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Cleaner is the identity-scoped teardown surface Cleanup drives: discover a
// run's servers and volumes by their dizzy:run metadata and its ports, networks,
// and subnets by their dizzy:run tag, sweep a network's stray ports, delete a
// resource, and wait for one to be gone. Like Nova it is the single
// ports-and-adapters seam to the cloud; *nova.Client satisfies it in production
// and a fake satisfies it in tests.
type Cleaner interface {
	ListServersByMetadata(ctx context.Context, runID string) ([]resource.Resource, error)
	ListVolumesByMetadata(ctx context.Context, runID string) ([]resource.Resource, error)
	ListByTag(ctx context.Context, kind resource.Kind, runID string) ([]resource.Resource, error)
	DeleteNetworkPorts(ctx context.Context, networkID string) (int, error)
	Delete(ctx context.Context, r resource.Resource) error
	WaitForGone(ctx context.Context, r resource.Resource) error
}

// The production *nova.Client must satisfy the seam.
var _ Cleaner = (*nova.Client)(nil)

// Cleanup deletes every resource a run created, strictly by run identity, in
// dependency order — servers first (so their volume and port attachments
// release), then ports, then volumes, then the companion networks (each preceded
// by a stray-port sweep, since a network delete cascades its subnet but is
// blocked by a leftover port), then any subnet the network delete did not
// cascade — returning the number deleted. Servers and volumes are discovered by
// the run's dizzy:run=<id> metadata; ports, networks, and subnets by the
// dizzy:run=<id> tag; each is unioned (deduplicated by id) with the run record's
// created list as a belt-and-suspenders handle, so it never touches resources
// the tool did not create and still reclaims a resource whose discovery missed
// it. Each deleted server is waited on until it is gone (bounded by opTimeout)
// before the ports and volumes are deleted. An already-gone resource (a 404)
// counts as success, so running Cleanup twice is a no-op. The first non-404
// error stops the run and is returned with the count deleted so far.
//
// An empty runID is refused: metadata and tag discovery both filter on the run
// id, and the zero value of a missing key is also "", so an empty runID would
// match every resource the tool never tagged and delete them all. The "never
// touches resources the tool did not create" invariant depends on a non-empty
// run id, so guard it here before any listing or deletion.
func Cleanup(ctx context.Context, c Cleaner, runID string, recorded []resource.Resource, opTimeout time.Duration) (int, error) {
	if runID == "" {
		return 0, fmt.Errorf("cleanup: empty run id; refusing to delete by empty identity")
	}
	var deleted int

	// Servers first: each is waited on to gone so its attachments release before
	// the ports and volumes are deleted.
	discoveredServers, err := c.ListServersByMetadata(ctx, runID)
	if err != nil {
		return deleted, err
	}
	for _, s := range union(discoveredServers, recordedOfKind(recorded, nova.KindServer)) {
		gone, err := deleteAndWaitGone(ctx, c, s, opTimeout)
		if gone {
			deleted++
		}
		if err != nil {
			return deleted, err
		}
	}

	// Ports next, now detached from their (deleted) servers.
	discoveredPorts, err := c.ListByTag(ctx, nova.KindPort, runID)
	if err != nil {
		return deleted, err
	}
	n, err := deleteResources(ctx, c, union(discoveredPorts, recordedOfKind(recorded, nova.KindPort)))
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Volumes next, now detached.
	discoveredVols, err := c.ListVolumesByMetadata(ctx, runID)
	if err != nil {
		return deleted, err
	}
	n, err = deleteResources(ctx, c, union(discoveredVols, recordedOfKind(recorded, nova.KindVolume)))
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Networks next. Each is preceded by a stray-port sweep so a leftover untagged
	// port cannot block the delete; the network delete cascades its subnet.
	discoveredNets, err := c.ListByTag(ctx, nova.KindNetwork, runID)
	if err != nil {
		return deleted, err
	}
	for _, net := range union(discoveredNets, recordedOfKind(recorded, nova.KindNetwork)) {
		if _, err := c.DeleteNetworkPorts(ctx, net.ID); err != nil {
			return deleted, err
		}
		gone, err := deleteOne(ctx, c, net)
		if gone {
			deleted++
		}
		if err != nil {
			return deleted, err
		}
	}

	// Any subnet the network delete did not cascade (its network was already gone
	// or was never discovered) is reclaimed by tag; a 404 is tolerated.
	discoveredSubnets, err := c.ListByTag(ctx, nova.KindSubnet, runID)
	if err != nil {
		return deleted, err
	}
	n, err = deleteResources(ctx, c, union(discoveredSubnets, recordedOfKind(recorded, nova.KindSubnet)))
	deleted += n
	if err != nil {
		return deleted, err
	}

	return deleted, nil
}

// deleteAndWaitGone deletes a resource and waits for it to be fully gone,
// bounded by opTimeout, so the deletes that follow are not blocked by a
// lingering attachment. It returns whether the resource was actually deleted (a
// 404 is a no-op success) and any non-404 error.
func deleteAndWaitGone(ctx context.Context, c Cleaner, r resource.Resource, opTimeout time.Duration) (bool, error) {
	slog.Info("deleting resource", "kind", r.Kind, "id", r.ID)
	if err := c.Delete(ctx, r); err != nil {
		if nova.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("deleting %s %s: %w", r.Kind, r.ID, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	if err := c.WaitForGone(waitCtx, r); err != nil {
		return true, fmt.Errorf("waiting for %s %s to be deleted: %w", r.Kind, r.ID, err)
	}
	return true, nil
}

// deleteOne deletes a single resource, treating an already-gone resource (a 404)
// as success. It returns whether the resource was deleted and any non-404 error.
func deleteOne(ctx context.Context, c Cleaner, r resource.Resource) (bool, error) {
	slog.Info("deleting resource", "kind", r.Kind, "id", r.ID)
	if err := c.Delete(ctx, r); err != nil {
		if nova.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("deleting %s %s: %w", r.Kind, r.ID, err)
	}
	return true, nil
}

// deleteResources deletes each resource, treating an already-gone resource (a
// 404) as success so cleanup is idempotent. It returns the number actually
// deleted, so a no-op second sweep returns zero.
func deleteResources(ctx context.Context, c Cleaner, resources []resource.Resource) (int, error) {
	var deleted int
	for _, r := range resources {
		gone, err := deleteOne(ctx, c, r)
		if gone {
			deleted++
		}
		if err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// union concatenates the resource lists, dropping entries with an empty id and
// deduplicating by id, so a resource found both by discovery and in the run
// record is deleted once.
func union(lists ...[]resource.Resource) []resource.Resource {
	seen := make(map[string]bool)
	var out []resource.Resource
	for _, list := range lists {
		for _, r := range list {
			if r.ID == "" || seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	return out
}

// recordedOfKind returns the resources of kind from a run record's created list,
// the fallback discovery handle unioned with the identity listing.
func recordedOfKind(recorded []resource.Resource, kind resource.Kind) []resource.Resource {
	var out []resource.Resource
	for _, r := range recorded {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}
