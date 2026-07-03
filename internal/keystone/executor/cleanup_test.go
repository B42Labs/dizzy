package executor

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/openstack-tester/internal/keystone"
	"github.com/B42Labs/openstack-tester/internal/resource"
)

// fakeCleaner serves a run's resources by prefix/tag, records disable and delete
// events in call order, tolerates already-gone (404) deletes, and can fail a
// specific delete. It lets Cleanup's ordering, idempotency, dedup, prefix guard,
// and disable-before-delete be exercised without a cloud.
type fakeCleaner struct {
	users, projects, roles, domains []resource.Resource
	userAssignments                 map[string][]resource.Resource
	gone                            map[string]bool
	failDel                         map[string]error
	events                          []string // "del:<id>" / "disable:<id>" in call order
}

func newFakeCleaner() *fakeCleaner {
	return &fakeCleaner{
		userAssignments: make(map[string][]resource.Resource),
		gone:            make(map[string]bool),
		failDel:         make(map[string]error),
	}
}

func (f *fakeCleaner) live(rs []resource.Resource) []resource.Resource {
	var out []resource.Resource
	for _, r := range rs {
		if !f.gone[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeCleaner) ListUsersByPrefix(context.Context, string) ([]resource.Resource, error) {
	return f.live(f.users), nil
}

func (f *fakeCleaner) ListProjectsByTag(context.Context, string) ([]resource.Resource, error) {
	return f.live(f.projects), nil
}

func (f *fakeCleaner) ListRolesByPrefix(context.Context, string) ([]resource.Resource, error) {
	return f.live(f.roles), nil
}

func (f *fakeCleaner) ListDomainsByPrefix(context.Context, string) ([]resource.Resource, error) {
	return f.live(f.domains), nil
}

func (f *fakeCleaner) ListAssignmentsForUser(_ context.Context, userID string) ([]resource.Resource, error) {
	return f.live(f.userAssignments[userID]), nil
}

func (f *fakeCleaner) DisableDomain(_ context.Context, r resource.Resource) error {
	if f.gone[r.ID] {
		return gophercloud.ErrUnexpectedResponseCode{Actual: 404}
	}
	f.events = append(f.events, "disable:"+r.ID)
	return nil
}

func (f *fakeCleaner) Delete(_ context.Context, r resource.Resource) error {
	if err := f.failDel[r.ID]; err != nil {
		return err
	}
	if f.gone[r.ID] {
		return gophercloud.ErrUnexpectedResponseCode{Actual: 404}
	}
	f.gone[r.ID] = true
	f.events = append(f.events, "del:"+r.ID)
	return nil
}

// seedRun stocks one domain, one role, one project, and one user (all prefixed),
// with the user holding one project-scoped grant.
func seedRun() *fakeCleaner {
	f := newFakeCleaner()
	f.domains = []resource.Resource{{Kind: keystone.KindDomain, ID: "d1", Name: "ostester-run0-dom-0001"}}
	f.roles = []resource.Resource{{Kind: keystone.KindRole, ID: "r1", Name: "ostester-run0-role-0001"}}
	f.projects = []resource.Resource{{Kind: keystone.KindProject, ID: "p1", Name: "ostester-run0-proj-0001"}}
	f.users = []resource.Resource{{Kind: keystone.KindUser, ID: "u1", Name: "ostester-run0-user-0001"}}
	f.userAssignments["u1"] = []resource.Resource{{Kind: keystone.KindAssignment, ID: "u1:project:p1:r1"}}
	return f
}

func idx(events []string, event string) int { return slices.Index(events, event) }

// TestCleanupReverseOrder confirms the teardown order: unassign -> users ->
// projects -> roles -> disable+delete domains.
func TestCleanupReverseOrder(t *testing.T) {
	f := seedRun()
	deleted, err := Cleanup(context.Background(), f, "run0", nil, time.Minute)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	// assignment + user + project + role + domain.
	if deleted != 5 {
		t.Errorf("deleted %d resources, want 5", deleted)
	}

	order := []string{"del:u1:project:p1:r1", "del:u1", "del:p1", "del:r1", "disable:d1", "del:d1"}
	prev := -1
	for _, e := range order {
		at := idx(f.events, e)
		if at < 0 {
			t.Fatalf("event %q never happened; log=%v", e, f.events)
		}
		if at < prev {
			t.Fatalf("event %q out of order; log=%v", e, f.events)
		}
		prev = at
	}
}

// TestCleanupIdempotent covers the "running cleanup twice is a no-op" acceptance
// criterion: the second sweep finds every resource gone and deletes nothing.
func TestCleanupIdempotent(t *testing.T) {
	f := seedRun()
	first, err := Cleanup(context.Background(), f, "run0", nil, time.Minute)
	if err != nil {
		t.Fatalf("first Cleanup: %v", err)
	}
	if first != 5 {
		t.Fatalf("first Cleanup deleted %d, want 5", first)
	}
	second, err := Cleanup(context.Background(), f, "run0", nil, time.Minute)
	if err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
	if second != 0 {
		t.Errorf("second Cleanup deleted %d resources, want 0 (a no-op)", second)
	}
}

func TestCleanupRefusesEmptyRunID(t *testing.T) {
	f := seedRun()
	deleted, err := Cleanup(context.Background(), f, "", nil, time.Minute)
	if err == nil {
		t.Fatal("Cleanup with an empty run id: expected an error, got nil")
	}
	if deleted != 0 || len(f.events) != 0 {
		t.Errorf("Cleanup touched resources with an empty run id: deleted=%d events=%v", deleted, f.events)
	}
}

// TestCleanupUnionsRecordedAndDedups confirms a resource present only in the run
// record (missed by discovery) is still deleted, and one present in both is
// deleted once.
func TestCleanupUnionsRecordedAndDedups(t *testing.T) {
	f := newFakeCleaner()
	// Discovery finds only the user u1; the record additionally holds project p2
	// (missed by discovery) and u1 again (a duplicate to dedup).
	f.users = []resource.Resource{{Kind: keystone.KindUser, ID: "u1", Name: "ostester-run0-user-0001"}}
	recorded := []resource.Resource{
		{Kind: keystone.KindProject, ID: "p2", Name: "ostester-run0-proj-0002"},
		{Kind: keystone.KindUser, ID: "u1", Name: "ostester-run0-user-0001"},
	}

	deleted, err := Cleanup(context.Background(), f, "run0", recorded, time.Minute)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if deleted != 2 { // u1 + p2, each once
		t.Errorf("deleted %d resources, want 2 (u1 and p2 each once)", deleted)
	}
	if got := strCount(f.events, "del:u1"); got != 1 {
		t.Errorf("u1 deleted %d times, want 1 (the duplicate must be deduplicated)", got)
	}
	if idx(f.events, "del:p2") < 0 {
		t.Error("the record-only project p2 was never deleted")
	}
}

// TestCleanupNeverDeletesUnprefixedRoots locks the domain-manager reused-root
// acceptance criterion: a reused domain and role (carrying no ostester- prefix)
// are never disabled or deleted, even when they appear in the record.
func TestCleanupNeverDeletesUnprefixedRoots(t *testing.T) {
	f := newFakeCleaner()
	recorded := []resource.Resource{
		{Kind: keystone.KindDomain, ID: "shared", Name: "managed"}, // reused, no prefix
		{Kind: keystone.KindRole, ID: "reused", Name: "member"},    // reused, no prefix
	}

	deleted, err := Cleanup(context.Background(), f, "run0", recorded, time.Minute)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted %d resources, want 0 (reused roots must never be deleted)", deleted)
	}
	if len(f.events) != 0 {
		t.Errorf("Cleanup touched a reused root: %v", f.events)
	}
}

// TestCleanupDisablesDomainBeforeDelete confirms a domain is disabled before it
// is deleted (Keystone refuses to delete an enabled domain).
func TestCleanupDisablesDomainBeforeDelete(t *testing.T) {
	f := newFakeCleaner()
	f.domains = []resource.Resource{{Kind: keystone.KindDomain, ID: "d1", Name: "ostester-run0-dom-0001"}}

	if _, err := Cleanup(context.Background(), f, "run0", nil, time.Minute); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	dis, del := idx(f.events, "disable:d1"), idx(f.events, "del:d1")
	if dis < 0 || del < 0 {
		t.Fatalf("missing disable/delete for the domain; log=%v", f.events)
	}
	if dis >= del {
		t.Errorf("domain deleted before it was disabled; log=%v", f.events)
	}
}

// TestCleanupPropagatesError confirms a non-404 delete error stops the sweep and
// is returned. A failing user delete must halt before any project is deleted.
func TestCleanupPropagatesError(t *testing.T) {
	f := seedRun()
	f.failDel["u1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 500}

	if _, err := Cleanup(context.Background(), f, "run0", nil, time.Minute); err == nil {
		t.Fatal("expected the 500 delete error to propagate")
	}
	if slices.Contains(f.events, "del:p1") {
		t.Error("a project was deleted despite a failing user delete")
	}
}

// TestCleanupToleratesForbiddenListing confirms a nil listing (the client's
// fail-open on a 403) is handled: no crash, nothing to delete from that kind.
func TestCleanupToleratesForbiddenListing(t *testing.T) {
	f := newFakeCleaner() // every listing empty, as a fail-open 403 would produce
	deleted, err := Cleanup(context.Background(), f, "run0", nil, time.Minute)
	if err != nil {
		t.Fatalf("Cleanup with empty listings: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted %d, want 0 for empty listings", deleted)
	}
}

// strCount counts occurrences of s in events.
func strCount(events []string, s string) int {
	var n int
	for _, e := range events {
		if e == s {
			n++
		}
	}
	return n
}

// errForbidden is a small sanity check that the executor's fail-fast sentinel is
// wired: an ErrForbidden wrap is recognised by the shared classifier.
func TestForbiddenSentinelWired(t *testing.T) {
	if !errors.Is(wrapForbidden(), keystone.ErrForbidden) {
		t.Error("the test forbidden wrapper does not match keystone.ErrForbidden")
	}
}
