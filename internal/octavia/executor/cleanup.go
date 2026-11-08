package executor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/octavia"
	"github.com/B42Labs/dizzy/internal/resource"
)

// conflictRetryInterval is how long a delete answered with a 409 waits before
// it is sent again: a load balancer still in a PENDING status refuses its
// delete, and so does a subnet or network a port remains on. It is a variable
// so tests can shrink it.
var conflictRetryInterval = 2 * time.Second

// Cleaner is the identity-scoped teardown surface Cleanup and
// CleanupLoadBalancers drive: discover a run's load balancers, VIP subnets,
// and VIP networks by their dizzy:run tag, delete one, and wait for a load
// balancer to be gone. Like Octavia it is the single ports-and-adapters seam
// to the cloud; *octavia.Client satisfies it in production and a fake
// satisfies it in tests.
type Cleaner interface {
	ListByTag(ctx context.Context, kind resource.Kind, runID string) ([]resource.Resource, error)
	Delete(ctx context.Context, r resource.Resource) error
	WaitForGone(ctx context.Context, r resource.Resource) error
}

// The production *octavia.Client must satisfy the seam.
var _ Cleaner = (*octavia.Client)(nil)

// Cleanup deletes every resource a run created, strictly by run identity, in
// dependency order, returning the number deleted. Load balancers go first,
// deleted with cascade, which removes their listeners, pools, members, and
// health monitors and the VIP port Octavia owns; then the VIP subnets, then
// the VIP networks. Each kind is discovered by the run's dizzy:run=<id> tag and
// unioned (deduplicated by id) with the run record's resources of that kind as
// a belt-and-suspenders handle, so it never touches resources the tool did not
// create and still reclaims one discovery missed. The recorded listeners,
// pools, members, and health monitors are never deleted on their own: the
// cascade removes them.
//
// The load balancers are deleted by CleanupLoadBalancers, through the same
// bounded worker pool Apply uses (up to concurrency at once), each delete
// re-sent while the load balancer answers 409 because it is still in a PENDING
// status, and each accepted delete waited on until the load balancer is gone
// (bounded by opTimeout), so its VIP port is released before the subnets are
// deleted. The subnets and networks are deleted one at a time, each re-sent
// while it answers 409. A subnet or network still refused at opTimeout holds a
// port dizzy did not create, a leak in the cloud that Cleanup reports and
// never deletes. An already-gone resource (a 404) counts as success and is not
// counted, so running Cleanup twice is a no-op. The first listing error or
// other delete error stops the cleanup before any later kind is touched and is
// returned with the count deleted so far.
//
// An empty runID is refused before any listing or deletion, as every other
// service's cleanup refuses it: the run id is the identity each discovery is
// scoped by, and an empty one names no run. The "never touches resources the
// tool did not create" invariant depends on a non-empty run id, so guard it
// here.
func Cleanup(ctx context.Context, c Cleaner, runID string, recorded []resource.Resource, concurrency int, opTimeout time.Duration) (int, error) {
	if runID == "" {
		return 0, fmt.Errorf("cleanup: empty run id; refusing to delete by empty identity")
	}

	// Load balancers first: the cascade removes every child and the VIP port, so
	// the subnets and networks are free once each load balancer is gone.
	deleted, err := CleanupLoadBalancers(ctx, c, runID, recorded, concurrency, opTimeout)
	if err != nil {
		return deleted, err
	}

	// Subnets, then networks, one at a time.
	for _, kind := range []resource.Kind{octavia.KindSubnet, octavia.KindNetwork} {
		discovered, err := c.ListByTag(ctx, kind, runID)
		if err != nil {
			return deleted, err
		}
		for _, r := range union(discovered, recordedOfKind(recorded, kind)) {
			gone, err := deleteResending(ctx, c, r, opTimeout, func(conflict error) error {
				return fmt.Errorf("deleting %s %s: still in use after %s; a port dizzy did not create remains on it: %w",
					r.Kind, r.ID, opTimeout, conflict)
			})
			if gone {
				deleted++
			}
			if err != nil {
				return deleted, err
			}
		}
	}

	return deleted, nil
}

// CleanupLoadBalancers deletes every load balancer a run created, strictly by
// run identity, and returns the number deleted. The load balancers are
// discovered by the run's dizzy:run=<id> tag and unioned (deduplicated by id)
// with the run record's load balancers, so one discovery missed is still
// reclaimed. Each is deleted with cascade, which removes its listeners, pools,
// members, and health monitors and the VIP port Octavia owns. The deletes run
// through runStage (up to concurrency at once); each is re-sent while the load
// balancer answers 409 because it is still in a PENDING status, and each
// accepted delete is waited on until the load balancer is gone (bounded by
// opTimeout), so every VIP port is released when CleanupLoadBalancers returns.
// An already-gone load balancer (a 404) counts as success and is not counted.
// A listing error is returned unchanged with a count of 0 before any delete;
// the first delete error stops the stage and is returned with the count
// deleted so far.
//
// It never lists or deletes a subnet or a network. Cleanup runs it as its
// first stage; it exists on its own for a run whose networks belong to another
// service's teardown. The mix Gardener persona's load balancers sit on Nova's
// cluster networks, which carry the same run tag, so Cleanup's subnet and
// network stages would delete what the Nova teardown owns, while this stage
// frees their VIP ports for it.
//
// An empty runID is refused before any listing or deletion, as Cleanup refuses
// it: the run id is the identity the discovery is scoped by, and an empty one
// names no run.
func CleanupLoadBalancers(ctx context.Context, c Cleaner, runID string, recorded []resource.Resource, concurrency int, opTimeout time.Duration) (int, error) {
	if runID == "" {
		return 0, fmt.Errorf("cleanup: empty run id; refusing to delete by empty identity")
	}
	discovered, err := c.ListByTag(ctx, octavia.KindLoadBalancer, runID)
	if err != nil {
		return 0, err
	}
	gone, err := runStage(ctx, union(discovered, recordedOfKind(recorded, octavia.KindLoadBalancer)), concurrency,
		func(ctx context.Context, lb resource.Resource) (bool, error) {
			return deleteLoadBalancer(ctx, c, lb, opTimeout)
		})
	var deleted int
	for _, g := range gone {
		if g {
			deleted++
		}
	}
	return deleted, err
}

// deleteLoadBalancer deletes a load balancer with cascade, re-sending the
// delete while it is still in a PENDING status, then waits for it to be fully
// gone, bounded by opTimeout. It returns whether the load balancer was
// actually deleted (a 404 is a no-op success) and any non-404 error.
func deleteLoadBalancer(ctx context.Context, c Cleaner, lb resource.Resource, opTimeout time.Duration) (bool, error) {
	accepted, err := deleteResending(ctx, c, lb, opTimeout, func(conflict error) error {
		return fmt.Errorf("deleting %s %s: still in a PENDING status after %s: %w", lb.Kind, lb.ID, opTimeout, conflict)
	})
	if !accepted || err != nil {
		return accepted, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	if err := c.WaitForGone(waitCtx, lb); err != nil {
		return true, fmt.Errorf("waiting for %s %s to be deleted: %w", lb.Kind, lb.ID, err)
	}
	return true, nil
}

// deleteResending sends the delete of r and, while it is answered with a 409,
// sends it again every conflictRetryInterval until it is accepted, a 404 says r
// is gone, or opTimeout elapses. The last case returns the error stuck builds
// from the final 409, which names why r is still refusing its delete. It
// returns whether the delete was accepted (false for a 404) and any other
// error, a parent cancellation included.
func deleteResending(ctx context.Context, c Cleaner, r resource.Resource, opTimeout time.Duration, stuck func(conflict error) error) (bool, error) {
	slog.Info("deleting resource", "kind", r.Kind, "id", r.ID)
	conflictCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	for {
		err := c.Delete(ctx, r)
		switch {
		case err == nil:
			return true, nil
		case octavia.IsNotFound(err):
			return false, nil
		case !gophercloud.ResponseCodeIs(err, http.StatusConflict):
			return false, fmt.Errorf("deleting %s %s: %w", r.Kind, r.ID, err)
		}

		slog.Info("delete refused with a conflict; sending it again", "kind", r.Kind, "id", r.ID, "error", err)
		select {
		case <-time.After(conflictRetryInterval):
		case <-conflictCtx.Done():
			if ctx.Err() != nil {
				return false, fmt.Errorf("deleting %s %s: %w", r.Kind, r.ID, ctx.Err())
			}
			return false, stuck(err)
		}
	}
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
