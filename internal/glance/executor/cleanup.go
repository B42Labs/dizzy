package executor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/B42Labs/dizzy/internal/glance"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Cleaner is the identity-scoped teardown surface Cleanup drives: discover a
// run's images by their dizzy:run tag, delete an image, and wait for one to be
// gone. Like Glance it is the single ports-and-adapters seam to the cloud;
// *glance.Client satisfies it in production and a fake satisfies it in tests.
type Cleaner interface {
	ListImagesByTag(ctx context.Context, runID string) ([]resource.Resource, error)
	Delete(ctx context.Context, r resource.Resource) error
	WaitForGone(ctx context.Context, r resource.Resource) error
}

// The production *glance.Client must satisfy the seam.
var _ Cleaner = (*glance.Client)(nil)

// Cleanup deletes every image a run created, strictly by run identity, returning
// the number deleted. Images are discovered by the run's dizzy:run=<id> tag and
// unioned (deduplicated by id) with the run record's created list as a
// belt-and-suspenders handle, so it never touches images the tool did not create
// and still reclaims an image whose discovery missed it. Deletions run through the
// same bounded worker pool Apply uses (up to concurrency workers), each image
// waited on until it is gone (bounded by opTimeout), so a large teardown does not
// serialize on the monitor hot path. An already-gone image (a 404) counts as
// success, so running Cleanup twice is a no-op. The first non-404 error cancels
// the stage and is returned with the count deleted so far.
//
// An empty runID is refused: tag discovery filters on the run id, and the zero
// value of a missing key is also "", so an empty runID would match every image
// the tool never tagged and delete them all. The "never touches resources the
// tool did not create" invariant depends on a non-empty run id, so guard it here
// before any listing or deletion. Images are a single kind, so — unlike the Nova
// teardown — no cross-kind deletion ordering is needed, which is what lets the
// teardown run concurrently.
func Cleanup(ctx context.Context, c Cleaner, runID string, recorded []resource.Resource, concurrency int, opTimeout time.Duration) (int, error) {
	if runID == "" {
		return 0, fmt.Errorf("cleanup: empty run id; refusing to delete by empty identity")
	}

	discovered, err := c.ListImagesByTag(ctx, runID)
	if err != nil {
		return 0, err
	}

	targets := union(discovered, recordedOfKind(recorded, glance.KindImage))
	gone, err := runStage(ctx, targets, concurrency, func(ctx context.Context, img resource.Resource) (bool, error) {
		return deleteAndWaitGone(ctx, c, img, opTimeout)
	})
	var deleted int
	for _, g := range gone {
		if g {
			deleted++
		}
	}
	return deleted, err
}

// deleteAndWaitGone deletes an image and waits for it to be fully gone, bounded
// by opTimeout. It returns whether the image was actually deleted (a 404 is a
// no-op success) and any non-404 error.
func deleteAndWaitGone(ctx context.Context, c Cleaner, r resource.Resource, opTimeout time.Duration) (bool, error) {
	slog.Info("deleting resource", "kind", r.Kind, "id", r.ID)
	if err := c.Delete(ctx, r); err != nil {
		if glance.IsNotFound(err) {
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

// union concatenates the resource lists, dropping entries with an empty id and
// deduplicating by id, so an image found both by discovery and in the run record
// is deleted once.
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
