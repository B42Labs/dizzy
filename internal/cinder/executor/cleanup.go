package executor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/B42Labs/dizzy/internal/cinder"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Cleaner is the metadata-scoped teardown surface Cleanup drives: discover a
// run's volumes and snapshots by their ostester:run metadata, delete a
// resource, and wait for a resource to be gone. Like Cinder it is the single
// ports-and-adapters seam to the cloud; *cinder.Client satisfies it in
// production and a fake satisfies it in tests.
type Cleaner interface {
	ListVolumesByMetadata(ctx context.Context, runID string) ([]resource.Resource, error)
	ListSnapshotsByMetadata(ctx context.Context, runID string) ([]resource.Resource, error)
	Delete(ctx context.Context, r resource.Resource) error
	WaitForGone(ctx context.Context, r resource.Resource) error
}

// Cleanup deletes every resource a run created in reverse dependency order —
// snapshots first, then the volumes they were taken from, since Cinder rejects
// deleting a volume whose snapshots still exist — returning the number deleted.
// Each kind is discovered by the run's ostester:run=<id> metadata and unioned
// (deduplicated by id) with the run record's created list as a belt-and-
// suspenders handle, so it never touches resources the tool did not create and
// still reclaims a resource whose metadata list missed it. Snapshot deletion is
// asynchronous, so each deleted snapshot is waited on until it is gone (bounded
// by opTimeout) before the volumes are deleted. An already-gone resource (a 404)
// counts as success, so running Cleanup twice is a no-op. The first non-404
// error stops the run and is returned with the count deleted so far.
//
// An empty runID is refused: snapshot discovery filters client-side on the run
// metadata, and the zero value of a missing metadata key is also "", so an empty
// runID would match every snapshot the tool never tagged and delete them all.
// The "never touches resources the tool did not create" invariant depends on a
// non-empty run id, so guard it here before any listing or deletion.
func Cleanup(ctx context.Context, c Cleaner, runID string, recorded []resource.Resource, opTimeout time.Duration) (int, error) {
	if runID == "" {
		return 0, fmt.Errorf("cleanup: empty run id; refusing to delete by empty metadata")
	}
	var deleted int

	// Snapshots first: a volume with snapshots cannot be deleted.
	discoveredSnaps, err := c.ListSnapshotsByMetadata(ctx, runID)
	if err != nil {
		return deleted, err
	}
	for _, s := range union(discoveredSnaps, recordedOfKind(recorded, cinder.KindSnapshot)) {
		gone, err := deleteAndWaitGone(ctx, c, s, opTimeout)
		if gone {
			deleted++
		}
		if err != nil {
			return deleted, err
		}
	}

	// Volumes next, now that their snapshots are gone.
	discoveredVols, err := c.ListVolumesByMetadata(ctx, runID)
	if err != nil {
		return deleted, err
	}
	n, err := deleteResources(ctx, c, union(discoveredVols, recordedOfKind(recorded, cinder.KindVolume)))
	deleted += n
	if err != nil {
		return deleted, err
	}

	return deleted, nil
}

// deleteAndWaitGone deletes a snapshot and waits for it to be fully gone,
// bounded by opTimeout, so the volume delete that follows is not blocked by a
// lingering snapshot. It returns whether the resource was actually deleted (a
// 404 is a no-op success) and any non-404 error.
func deleteAndWaitGone(ctx context.Context, c Cleaner, r resource.Resource, opTimeout time.Duration) (bool, error) {
	slog.Info("deleting resource", "kind", r.Kind, "id", r.ID)
	if err := c.Delete(ctx, r); err != nil {
		if cinder.IsNotFound(err) {
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

// deleteResources deletes each resource, treating an already-gone resource (a
// 404) as success so cleanup is idempotent. It returns the number actually
// deleted, so a no-op second sweep returns zero.
func deleteResources(ctx context.Context, c Cleaner, resources []resource.Resource) (int, error) {
	var deleted int
	for _, r := range resources {
		slog.Info("deleting resource", "kind", r.Kind, "id", r.ID)
		if err := c.Delete(ctx, r); err != nil {
			if cinder.IsNotFound(err) {
				continue
			}
			return deleted, fmt.Errorf("deleting %s %s: %w", r.Kind, r.ID, err)
		}
		deleted++
	}
	return deleted, nil
}

// union concatenates the resource lists, dropping entries with an empty id and
// deduplicating by id, so a resource found both by metadata and in the run
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
// the fallback discovery handle unioned with the metadata listing.
func recordedOfKind(recorded []resource.Resource, kind resource.Kind) []resource.Resource {
	var out []resource.Resource
	for _, r := range recorded {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}
