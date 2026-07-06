package executor

import (
	"context"
	"fmt"
	"log/slog"
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

// Cleanup deletes every resource a run created in reverse dependency order —
// unassign role assignments, delete users, delete projects, delete roles
// (admin-created only), then disable-then-delete domains (admin-created only) —
// returning the number deleted. Each kind is discovered by the run's name prefix
// (projects additionally by tag) and unioned (deduplicated by id) with the run
// record's created list as a belt-and-suspenders handle, so it never touches
// resources the tool did not create and still reclaims a resource whose listing
// missed it. Assignments are additionally discovered from each surviving user's
// live grants. Keystone refuses to delete an enabled domain, so each domain is
// disabled before it is deleted (a cascade removes its remaining contents). An
// already-gone resource (a 404) counts as success, so running Cleanup twice is a
// no-op; each mutating op is bounded by opTimeout. The first non-404 error stops
// the run and is returned with the count deleted so far.
//
// Roles and domains are additionally guarded by the ostester- name prefix
// (HasNamePrefix): in domain-manager mode the run shares a pre-existing domain
// and reuses pre-existing roles that carry no prefix and must never be deleted —
// they are absent from both the listing and the record, and this guard is the
// final backstop.
//
// An empty runID is refused: prefix discovery would degenerate to the bare
// ostester- prefix and match every tester run in scope, so the
// "never touches resources the tool did not create" invariant depends on a
// non-empty run id.
func Cleanup(ctx context.Context, c Cleaner, runID string, recorded []resource.Resource, opTimeout time.Duration) (int, error) {
	if runID == "" {
		return 0, fmt.Errorf("cleanup: empty run id; refusing to delete by an empty prefix")
	}
	var deleted int

	// Discover this run's users first: they are both a delete target and the
	// handle for discovering their live assignments.
	discoveredUsers, err := c.ListUsersByPrefix(ctx, runID)
	if err != nil {
		return deleted, err
	}
	users := union(discoveredUsers, recordedOfKind(recorded, keystone.KindUser))

	// Revoke assignments: the recorded synthetic-id grants plus every live grant
	// held by a surviving user. Tokens need no teardown (they expire).
	assignments := recordedOfKind(recorded, keystone.KindAssignment)
	for _, u := range users {
		live, err := c.ListAssignmentsForUser(ctx, u.ID)
		if err != nil {
			return deleted, err
		}
		assignments = union(assignments, live)
	}
	n, err := deleteResources(ctx, c, assignments, opTimeout)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Delete the users, now that their assignments are gone.
	n, err = deleteResources(ctx, c, users, opTimeout)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Delete the projects (discovered by tag, unioned with the record).
	discoveredProjects, err := c.ListProjectsByTag(ctx, runID)
	if err != nil {
		return deleted, err
	}
	n, err = deleteResources(ctx, c, union(discoveredProjects, recordedOfKind(recorded, keystone.KindProject)), opTimeout)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Delete the roles — admin-created only. A reused role carries no ostester-
	// prefix, so ownedOnly drops it even if it somehow reached the record.
	discoveredRoles, err := c.ListRolesByPrefix(ctx, runID)
	if err != nil {
		return deleted, err
	}
	n, err = deleteResources(ctx, c, ownedOnly(union(discoveredRoles, recordedOfKind(recorded, keystone.KindRole))), opTimeout)
	deleted += n
	if err != nil {
		return deleted, err
	}

	// Disable then delete the domains — admin-created only, guarded by the prefix
	// so a reused in-scope domain is never touched.
	discoveredDomains, err := c.ListDomainsByPrefix(ctx, runID)
	if err != nil {
		return deleted, err
	}
	n, err = deleteDomains(ctx, c, ownedOnly(union(discoveredDomains, recordedOfKind(recorded, keystone.KindDomain))), opTimeout)
	deleted += n
	if err != nil {
		return deleted, err
	}

	return deleted, nil
}

// deleteResources deletes each resource, treating an already-gone resource (a
// 404) as success so cleanup is idempotent. Each delete is bounded by opTimeout.
// It returns the number actually deleted, so a no-op second sweep returns zero.
func deleteResources(ctx context.Context, c Cleaner, resources []resource.Resource, opTimeout time.Duration) (int, error) {
	var deleted int
	for _, r := range resources {
		slog.Info("deleting resource", "kind", r.Kind, "id", r.ID)
		if err := bounded(ctx, opTimeout, func(ctx context.Context) error { return c.Delete(ctx, r) }); err != nil {
			if keystone.IsNotFound(err) {
				continue
			}
			return deleted, fmt.Errorf("deleting %s %s: %w", r.Kind, r.ID, err)
		}
		deleted++
	}
	return deleted, nil
}

// deleteDomains disables each domain before deleting it, since Keystone refuses
// to delete an enabled domain. A 404 on either step counts as success. It
// returns the number actually deleted.
func deleteDomains(ctx context.Context, c Cleaner, domains []resource.Resource, opTimeout time.Duration) (int, error) {
	var deleted int
	for _, d := range domains {
		if err := bounded(ctx, opTimeout, func(ctx context.Context) error { return c.DisableDomain(ctx, d) }); err != nil {
			if !keystone.IsNotFound(err) {
				return deleted, fmt.Errorf("disabling domain %s: %w", d.ID, err)
			}
			continue // already gone
		}
		slog.Info("deleting resource", "kind", d.Kind, "id", d.ID)
		if err := bounded(ctx, opTimeout, func(ctx context.Context) error { return c.Delete(ctx, d) }); err != nil {
			if keystone.IsNotFound(err) {
				continue
			}
			return deleted, fmt.Errorf("deleting %s %s: %w", d.Kind, d.ID, err)
		}
		deleted++
	}
	return deleted, nil
}

// bounded runs fn on a context bounded by opTimeout, so a wedged Keystone call
// bounds one operation rather than the whole sweep.
func bounded(ctx context.Context, opTimeout time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return fn(ctx)
}

// ownedOnly keeps only resources whose name carries the ostester- prefix — the
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
