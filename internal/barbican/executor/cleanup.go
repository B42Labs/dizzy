package executor

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/B42Labs/dizzy/internal/barbican"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Cleaner is the identity-scoped teardown surface Cleanup drives: discover a
// run's containers, orders, and secrets, and delete one. Containers and secrets
// are found by their dizzy-<id>- name prefix, a secret only once its dizzy_run
// metadata confirms the run, and orders by the same prefix on their meta.name.
// ListOrdersByRun also returns the secret each discovered order's secret_ref
// names, the order's generated secret, whether or not it was ever stamped.
// Like Barbican it is the single ports-and-adapters seam to the cloud;
// *barbican.Client satisfies it in production and a fake satisfies it in tests.
type Cleaner interface {
	ListContainersByRun(ctx context.Context, runID string) ([]resource.Resource, error)
	ListOrdersByRun(ctx context.Context, runID string) (orders, secrets []resource.Resource, err error)
	ListSecretsByRun(ctx context.Context, runID string) ([]resource.Resource, error)
	Delete(ctx context.Context, r resource.Resource) error
}

// The production *barbican.Client must satisfy the seam.
var _ Cleaner = (*barbican.Client)(nil)

// Cleanup deletes every resource a run created, strictly by run identity,
// returning the number deleted. It works kind by kind in dependency order:
// containers first, since they reference secrets, then orders, then secrets,
// both stored and generated. Each kind is discovered through its listing and
// unioned (deduplicated by id) with the run record's resources of that kind as a
// belt-and-suspenders handle, so it never touches resources the tool did not
// create and still reclaims one discovery missed.
//
// The order listing also yields each discovered order's generated secret.
// Deleting an order leaves that secret behind, and for a secret that was never
// stamped (its order turned ACTIVE after the order wait gave up, or a retried
// create left a duplicate order) the order is the only handle. So the orders
// phase deletes those generated secrets first and the orders only after them:
// a cleanup that stops in between leaves the order in place for a rerun to find
// the secret through again. A secret is deleted when it is recorded, carries
// dizzy_run=<run id> metadata, or is the secret_ref of an order whose meta.name
// carries the run prefix, and in no other case: a stored secret whose metadata
// stamp never landed is reached through the record alone, while a secret that
// only matches the name prefix is never discovered and so never deleted.
//
// Each batch of deletions runs through the same bounded worker pool Apply uses
// (up to concurrency workers). Barbican deletes synchronously, so nothing is
// waited on. An already-gone resource (a 404) counts as success and is not
// counted, so running Cleanup twice is a no-op. The first listing error or
// non-404 delete error stops the cleanup before any later batch is touched and
// is returned with the count deleted so far.
//
// An empty runID is refused before any listing or deletion: secret discovery
// compares the dizzy_run metadata with the run id, and a missing key reads as
// "", so an empty run id would confirm secrets the tool never stamped. The
// "never touches resources the tool did not create" invariant depends on a
// non-empty run id, so guard it here.
func Cleanup(ctx context.Context, c Cleaner, runID string, recorded []resource.Resource, concurrency int) (int, error) {
	if runID == "" {
		return 0, fmt.Errorf("cleanup: empty run id; refusing to delete by empty identity")
	}

	var deleted int
	del := func(targets []resource.Resource) error {
		n, err := deleteAll(ctx, c, targets, concurrency)
		deleted += n
		return err
	}

	containers, err := c.ListContainersByRun(ctx, runID)
	if err != nil {
		return deleted, err
	}
	if err := del(union(containers, recordedOfKind(recorded, barbican.KindContainer))); err != nil {
		return deleted, err
	}

	orders, generated, err := c.ListOrdersByRun(ctx, runID)
	if err != nil {
		return deleted, err
	}
	linked := union(generated)
	if err := del(linked); err != nil {
		return deleted, err
	}
	if err := del(union(orders, recordedOfKind(recorded, barbican.KindOrder))); err != nil {
		return deleted, err
	}

	secrets, err := c.ListSecretsByRun(ctx, runID)
	if err != nil {
		return deleted, err
	}
	return deleted, del(without(union(secrets, recordedOfKind(recorded, barbican.KindSecret)), linked))
}

// without returns the resources of list whose id is not in drop, so a generated
// secret the orders phase already deleted is not deleted a second time.
func without(list, drop []resource.Resource) []resource.Resource {
	skip := make(map[string]bool, len(drop))
	for _, r := range drop {
		skip[r.ID] = true
	}
	var out []resource.Resource
	for _, r := range list {
		if !skip[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

// deleteAll deletes targets concurrently through runStage, up to concurrency at
// a time, and returns the number actually deleted. The first non-404 error
// cancels the stage and is returned.
func deleteAll(ctx context.Context, c Cleaner, targets []resource.Resource, concurrency int) (int, error) {
	gone, err := runStage(ctx, targets, concurrency, func(ctx context.Context, r resource.Resource) (bool, error) {
		return deleteOne(ctx, c, r)
	})
	var deleted int
	for _, g := range gone {
		if g {
			deleted++
		}
	}
	return deleted, err
}

// deleteOne deletes a single resource, treating an already-gone resource (a 404)
// as success. It returns whether the resource was deleted and any non-404 error.
func deleteOne(ctx context.Context, c Cleaner, r resource.Resource) (bool, error) {
	slog.Info("deleting resource", "kind", r.Kind, "id", r.ID)
	if err := c.Delete(ctx, r); err != nil {
		if barbican.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("deleting %s %s: %w", r.Kind, r.ID, err)
	}
	return true, nil
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
