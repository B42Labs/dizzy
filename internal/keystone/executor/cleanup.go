package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/B42Labs/dizzy/internal/keystone"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Cleaner is the prefix/tag-scoped teardown surface Cleanup drives: discover a
// run's projects (by tag), users, roles, and domains (by name prefix), list a
// user's assignments, disable a domain, and delete a resource. Like Keystone it
// is the single ports-and-adapters seam to the cloud; *keystone.Client satisfies
// it in production and a fake satisfies it in tests.
type Cleaner interface {
	ListProjectsByTag(ctx context.Context, runID string) ([]resource.Resource, error)
	ListUsersByPrefix(ctx context.Context, runID string) ([]resource.Resource, error)
	ListRolesByPrefix(ctx context.Context, runID string) ([]resource.Resource, error)
	ListDomainsByPrefix(ctx context.Context, runID string) ([]resource.Resource, error)
	ListAssignmentsForUser(ctx context.Context, userID string) ([]resource.Resource, error)
	DisableDomain(ctx context.Context, r resource.Resource) error
	Delete(ctx context.Context, r resource.Resource) error
}

// The production *keystone.Client must satisfy the seam.
var _ Cleaner = (*keystone.Client)(nil)

// cleanupRetryMaxDelay caps the backoff between two attempts of one cleanup
// call. It is longer than WithRetry's retryMaxDelay because a cleanup rides
// out an outage within its retry budget rather than failing an apply fast.
const cleanupRetryMaxDelay = 30 * time.Second

// Cleanup deletes every resource a run created in reverse dependency order —
// unassign role assignments, delete users, delete projects, delete implied-role
// rules, delete roles (admin-created only), then disable-then-delete domains
// (admin-created only) — returning the number deleted. Each kind is discovered
// by the run's name prefix (projects additionally by tag) and unioned
// (deduplicated by id) with the run record's created list as a
// belt-and-suspenders handle, so it never touches resources the tool did not
// create and still reclaims a resource whose listing missed it. Assignments are
// additionally discovered from each surviving user's live grants. Implied-role
// rules have no name or tag and are deleted from the record only: a sweep
// without a record reaches them through their roles, since Keystone deletes a
// role's rules when it deletes the role. Application credentials and trusts are
// deleted with their users: Keystone deletes a user's application credentials
// and every trust the user is trustor or trustee of when it deletes the user,
// so their recorded entries match no kind picked here and the count leaves
// them out. Keystone refuses to delete an enabled
// domain, so each domain is disabled before it is deleted (a cascade removes
// its remaining contents). An already-gone resource (a 404) counts as success,
// so running Cleanup twice is a no-op; each call is bounded by opTimeout.
//
// A failed call does not stop the run. A failed listing is recorded and its
// stage goes on with the record's resources of that kind (for users, live
// assignments are then listed for the recorded users only); a failed
// assignment listing is recorded and the next user is listed; a failed delete
// is recorded and the next resource is deleted; a failed disable is recorded
// and that domain's delete is skipped. Every stage runs, since Keystone deletes
// a user's and a project's role assignments with them, a role's implied-role
// rules with the role, and a disabled domain's contents with the domain. The
// error joins every recorded failure in call order (nil when none failed) and
// is returned with the count actually deleted.
//
// Cleanup makes one attempt per call; it is CleanupWithRetry with a zero
// budget. CleanupWithRetry retries a call that failed with a 5xx, a 429, its
// own opTimeout, or a connection error (retryableCleanupError) while the next
// attempt would start before the budget, measured from the start of the run,
// runs out. Each call backs off from 250ms, doubling up to
// cleanupRetryMaxDelay. A 404, a 403, and every other error are never
// retried, and no call is retried once ctx is done. Once the budget is spent,
// each remaining call still gets its one attempt.
//
// Roles and domains are additionally guarded by the dizzy- name prefix
// (HasNamePrefix): in domain-manager mode the run shares a pre-existing domain
// and reuses pre-existing roles that carry no prefix and must never be deleted —
// they are absent from both the listing and the record, and this guard is the
// final backstop.
//
// An empty runID is refused: prefix discovery would degenerate to the bare
// dizzy- prefix and match every tester run in scope, so the
// "never touches resources the tool did not create" invariant depends on a
// non-empty run id.
func Cleanup(ctx context.Context, c Cleaner, runID string, recorded []resource.Resource, opTimeout time.Duration) (int, error) {
	return CleanupWithRetry(ctx, c, runID, recorded, opTimeout, 0)
}

// CleanupWithRetry is Cleanup with a retry budget for transient failures: see
// Cleanup for the teardown and the retry policy. A budget of 0 never retries.
func CleanupWithRetry(ctx context.Context, c Cleaner, runID string, recorded []resource.Resource, opTimeout, retryBudget time.Duration) (int, error) {
	if runID == "" {
		return 0, fmt.Errorf("cleanup: empty run id; refusing to delete by an empty prefix")
	}
	s := &sweeper{c: c, opTimeout: opTimeout, retryUntil: time.Now().Add(retryBudget)}

	// Discover this run's users first: they are both a delete target and the
	// handle for discovering their live assignments.
	users := union(s.list(ctx, func(ctx context.Context) ([]resource.Resource, error) {
		return c.ListUsersByPrefix(ctx, runID)
	}), recordedOfKind(recorded, keystone.KindUser))

	// Revoke assignments: the recorded synthetic-id grants plus every live grant
	// held by a surviving user. Tokens need no teardown (they expire).
	assignments := recordedOfKind(recorded, keystone.KindAssignment)
	for _, u := range users {
		assignments = union(assignments, s.list(ctx, func(ctx context.Context) ([]resource.Resource, error) {
			return c.ListAssignmentsForUser(ctx, u.ID)
		}))
	}
	s.deleteAll(ctx, assignments)

	// Delete the users, now that their assignments are gone.
	s.deleteAll(ctx, users)

	// Delete the projects (discovered by tag, unioned with the record).
	s.deleteAll(ctx, union(s.list(ctx, func(ctx context.Context) ([]resource.Resource, error) {
		return c.ListProjectsByTag(ctx, runID)
	}), recordedOfKind(recorded, keystone.KindProject)))

	// Delete the implied-role rules before their roles — admin-created only, as
	// no domain-manager run records one. They are discovered from the record
	// alone; deleting a role removes any rule the record missed.
	s.deleteAll(ctx, union(recordedOfKind(recorded, keystone.KindImpliedRole)))

	// Delete the roles — admin-created only. A reused role carries no dizzy-
	// prefix, so ownedOnly drops it even if it somehow reached the record.
	s.deleteAll(ctx, ownedOnly(union(s.list(ctx, func(ctx context.Context) ([]resource.Resource, error) {
		return c.ListRolesByPrefix(ctx, runID)
	}), recordedOfKind(recorded, keystone.KindRole))))

	// Disable then delete the domains — admin-created only, guarded by the prefix
	// so a reused in-scope domain is never touched.
	s.deleteDomains(ctx, ownedOnly(union(s.list(ctx, func(ctx context.Context) ([]resource.Resource, error) {
		return c.ListDomainsByPrefix(ctx, runID)
	}), recordedOfKind(recorded, keystone.KindDomain))))

	return s.deleted, errors.Join(s.errs...)
}

// sweeper carries one Cleanup run's state: the cleaner, the per-attempt bound,
// the end of the retry budget, the count deleted so far, and every failure
// recorded so far, in call order.
type sweeper struct {
	c          Cleaner
	opTimeout  time.Duration
	retryUntil time.Time
	deleted    int
	errs       []error
}

// call runs fn, bounding each attempt with opTimeout so a wedged Keystone call
// bounds one attempt rather than the whole sweep. Every Cleaner call goes
// through it. A failed attempt is retried after a backoff unless ctx is done,
// the error is not retryableCleanupError, or the next attempt would not start
// before retryUntil; the backoff starts at retryBaseDelay for each call and
// doubles up to cleanupRetryMaxDelay.
func (s *sweeper) call(ctx context.Context, fn func(context.Context) error) error {
	backoff := retryBaseDelay
	for attempt := 1; ; attempt++ {
		opCtx, cancel := context.WithTimeout(ctx, s.opTimeout)
		err := fn(opCtx)
		cancel()
		if err == nil || ctx.Err() != nil || !retryableCleanupError(err) || !time.Now().Add(backoff).Before(s.retryUntil) {
			return err
		}
		slog.Warn("retrying cleanup call", "attempt", attempt, "backoff", backoff, "error", err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = min(backoff*2, cleanupRetryMaxDelay)
	}
}

// retryableCleanupError reports whether a failed cleanup call is worth another
// attempt: a 5xx or a 429 (keystone.IsRetryable), the attempt's own opTimeout
// expiring, or a connection error, which gophercloud returns as the net/http
// client's *url.Error for a refused, reset, or closed connection. A 404 (the
// resource is already gone) and a 403 (a policy denial no retry changes) are
// not, and neither is anything else, so a 400 or a 409 is terminal.
func retryableCleanupError(err error) bool {
	switch {
	case keystone.IsNotFound(err), keystone.IsForbidden(err):
		return false
	case keystone.IsRetryable(err), errors.Is(err, context.DeadlineExceeded):
		return true
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// list runs a discovery listing through call. A failed listing is recorded and
// returns no resources, so its stage goes on with the record alone.
func (s *sweeper) list(ctx context.Context, fn func(context.Context) ([]resource.Resource, error)) []resource.Resource {
	var found []resource.Resource
	err := s.call(ctx, func(ctx context.Context) error {
		var err error
		found, err = fn(ctx)
		return err
	})
	if err != nil {
		s.errs = append(s.errs, err)
		return nil
	}
	return found
}

// deleteAll deletes each resource, treating an already-gone resource (a 404)
// as success so cleanup is idempotent. A failed delete is recorded and the
// next resource is deleted. Only actual deletions count, so a no-op second
// sweep counts zero.
func (s *sweeper) deleteAll(ctx context.Context, resources []resource.Resource) {
	for _, r := range resources {
		s.delete(ctx, r)
	}
}

// deleteDomains disables each domain before deleting it, since Keystone refuses
// to delete an enabled domain. A 404 on either step counts as success. A failed
// disable is recorded and that domain's delete is skipped.
func (s *sweeper) deleteDomains(ctx context.Context, domains []resource.Resource) {
	for _, d := range domains {
		if err := s.call(ctx, func(ctx context.Context) error { return s.c.DisableDomain(ctx, d) }); err != nil {
			if !keystone.IsNotFound(err) {
				s.errs = append(s.errs, fmt.Errorf("disabling domain %s: %w", d.ID, err))
			}
			continue // already gone, or still enabled
		}
		s.delete(ctx, d)
	}
}

// delete deletes one resource through call, counting it on success and
// recording a non-404 failure.
func (s *sweeper) delete(ctx context.Context, r resource.Resource) {
	slog.Info("deleting resource", "kind", r.Kind, "id", r.ID)
	if err := s.call(ctx, func(ctx context.Context) error { return s.c.Delete(ctx, r) }); err != nil {
		if !keystone.IsNotFound(err) {
			s.errs = append(s.errs, fmt.Errorf("deleting %s %s: %w", r.Kind, r.ID, err))
		}
		return
	}
	s.deleted++
}

// ownedOnly keeps only resources whose name carries the dizzy- prefix — the
// reused-root protection for domain-manager mode, where a shared domain and
// reused roles must never be deleted.
func ownedOnly(resources []resource.Resource) []resource.Resource {
	var out []resource.Resource
	for _, r := range resources {
		if keystone.HasNamePrefix(r.Name) {
			out = append(out, r)
		}
	}
	return out
}

// union concatenates the resource lists, dropping entries with an empty id and
// deduplicating by id, so a resource found both by listing and in the run record
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
// the fallback discovery handle unioned with the listing.
func recordedOfKind(recorded []resource.Resource, kind resource.Kind) []resource.Resource {
	var out []resource.Resource
	for _, r := range recorded {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}
