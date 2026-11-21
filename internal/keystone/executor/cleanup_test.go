package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/keystone"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeCleaner serves a run's resources by prefix/tag, records disable and delete
// events in call order, tolerates already-gone (404) deletes, and can fail a
// specific listing, disable, or delete. It lets Cleanup's ordering,
// idempotency, dedup, prefix guard, disable-before-delete, and
// continue-past-failure be exercised without a cloud.
type fakeCleaner struct {
	users, projects, roles, domains []resource.Resource
	userAssignments                 map[string][]resource.Resource
	gone                            map[string]bool
	failDel                         map[string]error
	failList                        map[string]error // "users", "projects", "roles", "domains", "assignments:<userID>"
	failDisable                     map[string]error
	attempts                        map[string]int // Delete calls per id
	events                          []string       // "del:<id>" / "fail:<id>" / "disable:<id>" in call order
}

func newFakeCleaner() *fakeCleaner {
	return &fakeCleaner{
		userAssignments: make(map[string][]resource.Resource),
		gone:            make(map[string]bool),
		failDel:         make(map[string]error),
		failList:        make(map[string]error),
		failDisable:     make(map[string]error),
		attempts:        make(map[string]int),
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

// list returns the live resources of rs, or the failList error set for key.
func (f *fakeCleaner) list(key string, rs []resource.Resource) ([]resource.Resource, error) {
	if err := f.failList[key]; err != nil {
		return nil, err
	}
	return f.live(rs), nil
}

func (f *fakeCleaner) ListUsersByPrefix(context.Context, string) ([]resource.Resource, error) {
	return f.list("users", f.users)
}

func (f *fakeCleaner) ListProjectsByTag(context.Context, string) ([]resource.Resource, error) {
	return f.list("projects", f.projects)
}

func (f *fakeCleaner) ListRolesByPrefix(context.Context, string) ([]resource.Resource, error) {
	return f.list("roles", f.roles)
}

func (f *fakeCleaner) ListDomainsByPrefix(context.Context, string) ([]resource.Resource, error) {
	return f.list("domains", f.domains)
}

func (f *fakeCleaner) ListAssignmentsForUser(_ context.Context, userID string) ([]resource.Resource, error) {
	return f.list("assignments:"+userID, f.userAssignments[userID])
}

func (f *fakeCleaner) DisableDomain(_ context.Context, r resource.Resource) error {
	if err := f.failDisable[r.ID]; err != nil {
		return err
	}
	if f.gone[r.ID] {
		return gophercloud.ErrUnexpectedResponseCode{Actual: 404}
	}
	f.events = append(f.events, "disable:"+r.ID)
	return nil
}

func (f *fakeCleaner) Delete(_ context.Context, r resource.Resource) error {
	f.attempts[r.ID]++
	if err := f.failDel[r.ID]; err != nil {
		f.events = append(f.events, "fail:"+r.ID)
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
	f.domains = []resource.Resource{{Kind: keystone.KindDomain, ID: "d1", Name: "dizzy-run0-dom-0001"}}
	f.roles = []resource.Resource{{Kind: keystone.KindRole, ID: "r1", Name: "dizzy-run0-role-0001"}}
	f.projects = []resource.Resource{{Kind: keystone.KindProject, ID: "p1", Name: "dizzy-run0-proj-0001"}}
	f.users = []resource.Resource{{Kind: keystone.KindUser, ID: "u1", Name: "dizzy-run0-user-0001"}}
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
	f.users = []resource.Resource{{Kind: keystone.KindUser, ID: "u1", Name: "dizzy-run0-user-0001"}}
	recorded := []resource.Resource{
		{Kind: keystone.KindProject, ID: "p2", Name: "dizzy-run0-proj-0002"},
		{Kind: keystone.KindUser, ID: "u1", Name: "dizzy-run0-user-0001"},
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
// acceptance criterion: a reused domain and role (carrying no dizzy- prefix)
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
	f.domains = []resource.Resource{{Kind: keystone.KindDomain, ID: "d1", Name: "dizzy-run0-dom-0001"}}

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

// TestCleanupContinuesPastFailedDelete confirms a non-404 delete error no
// longer stops the sweep: the user's delete fails, every other resource is
// still deleted, and the returned error names the user.
func TestCleanupContinuesPastFailedDelete(t *testing.T) {
	f := seedRun()
	f.failDel["u1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 500}

	deleted, err := Cleanup(context.Background(), f, "run0", nil, time.Minute)
	if err == nil || !strings.HasPrefix(err.Error(), "deleting user u1: ") {
		t.Fatalf("Cleanup error = %v, want one starting with %q", err, "deleting user u1: ")
	}
	// assignment + project + role + domain; the user is left.
	if deleted != 4 {
		t.Errorf("deleted %d resources, want 4", deleted)
	}
	for _, e := range []string{"del:u1:project:p1:r1", "del:p1", "del:r1", "disable:d1", "del:d1"} {
		if !slices.Contains(f.events, e) {
			t.Errorf("event %q never happened after the failed user delete; log=%v", e, f.events)
		}
	}
}

// TestCleanupJoinsEveryFailure confirms every failed delete is reported, in
// call order, and that a terminal error is attempted once.
func TestCleanupJoinsEveryFailure(t *testing.T) {
	f := seedRun()
	errU, errP := errors.New("user boom"), errors.New("project boom")
	f.failDel["u1"] = errU
	f.failDel["p1"] = errP

	_, err := Cleanup(context.Background(), f, "run0", nil, time.Minute)
	if !errors.Is(err, errU) || !errors.Is(err, errP) {
		t.Fatalf("Cleanup error = %v, want one wrapping both delete errors", err)
	}
	msg := err.Error()
	u, p := strings.Index(msg, "deleting user u1: "), strings.Index(msg, "deleting project p1: ")
	if u < 0 || p < 0 || u > p {
		t.Errorf("error %q does not name the user before the project", msg)
	}
	if f.attempts["u1"] != 1 || f.attempts["p1"] != 1 {
		t.Errorf("attempts = %v, want one per failed resource", f.attempts)
	}
}

// TestCleanupContinuesPastFailedListing confirms a failed user listing is
// reported while the sweep goes on with the recorded user: its live
// assignment and the user are deleted, then the project, role, and domain.
func TestCleanupContinuesPastFailedListing(t *testing.T) {
	f := seedRun()
	listErr := fmt.Errorf("listing users by prefix: %w", gophercloud.ErrUnexpectedResponseCode{Actual: 503})
	f.failList["users"] = listErr

	deleted, err := Cleanup(context.Background(), f, "run0", []resource.Resource{f.users[0]}, time.Minute)
	if !errors.Is(err, listErr) {
		t.Fatalf("Cleanup error = %v, want one wrapping the listing error", err)
	}
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

// TestCleanupContinuesPastFailedAssignmentListing confirms a failed listing
// of one user's assignments is reported while the next user's grants are
// still revoked and both users are deleted.
func TestCleanupContinuesPastFailedAssignmentListing(t *testing.T) {
	f := newFakeCleaner()
	f.users = []resource.Resource{
		{Kind: keystone.KindUser, ID: "u1", Name: "dizzy-run0-user-0001"},
		{Kind: keystone.KindUser, ID: "u2", Name: "dizzy-run0-user-0002"},
	}
	f.userAssignments["u1"] = []resource.Resource{{Kind: keystone.KindAssignment, ID: "u1:project:p1:r1"}}
	f.userAssignments["u2"] = []resource.Resource{{Kind: keystone.KindAssignment, ID: "u2:project:p1:r1"}}
	listErr := fmt.Errorf("listing role assignments for user u1: %w", gophercloud.ErrUnexpectedResponseCode{Actual: 503})
	f.failList["assignments:u1"] = listErr

	_, err := Cleanup(context.Background(), f, "run0", nil, time.Minute)
	if !errors.Is(err, listErr) {
		t.Fatalf("Cleanup error = %v, want one wrapping the assignment-listing error", err)
	}
	for _, e := range []string{"del:u2:project:p1:r1", "del:u1", "del:u2"} {
		if !slices.Contains(f.events, e) {
			t.Errorf("event %q never happened; log=%v", e, f.events)
		}
	}
}

// TestCleanupSkipsDeleteWhenDisableFails confirms a domain whose disable fails
// is not deleted (Keystone refuses to delete an enabled domain), while the
// next domain is still disabled and deleted.
func TestCleanupSkipsDeleteWhenDisableFails(t *testing.T) {
	f := newFakeCleaner()
	f.domains = []resource.Resource{
		{Kind: keystone.KindDomain, ID: "d1", Name: "dizzy-run0-dom-0001"},
		{Kind: keystone.KindDomain, ID: "d2", Name: "dizzy-run0-dom-0002"},
	}
	f.failDisable["d1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 500}

	_, err := Cleanup(context.Background(), f, "run0", nil, time.Minute)
	if err == nil || !strings.HasPrefix(err.Error(), "disabling domain d1: ") {
		t.Fatalf("Cleanup error = %v, want one starting with %q", err, "disabling domain d1: ")
	}
	if slices.Contains(f.events, "del:d1") {
		t.Errorf("the domain whose disable failed was deleted; log=%v", f.events)
	}
	for _, e := range []string{"disable:d2", "del:d2"} {
		if !slices.Contains(f.events, e) {
			t.Errorf("event %q never happened; log=%v", e, f.events)
		}
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

// recordedRule is an implied-role rule between the seeded role r1 and a second
// role r2, as a run record lists it.
var recordedRule = resource.Resource{Kind: keystone.KindImpliedRole, Logical: "role-0001->role-0002", ID: "r1:r2"}

// TestCleanupDeletesImpliedRolesBeforeRoles confirms a recorded implied-role
// rule is deleted after every project and before every role, and counts. The
// record lists the role, the rule and the project in the order a run creates
// them, so a rule step that took more than the record's rules would delete
// the role before the rule.
func TestCleanupDeletesImpliedRolesBeforeRoles(t *testing.T) {
	f := seedRun()
	recorded := []resource.Resource{f.roles[0], recordedRule, f.projects[0]}
	deleted, err := Cleanup(context.Background(), f, "run0", recorded, time.Minute)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	// assignment + user + project + rule + role + domain.
	if deleted != 6 {
		t.Errorf("deleted %d resources, want 6", deleted)
	}
	order := []string{"del:p1", "del:r1:r2", "del:r1"}
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

// TestCleanupImpliedRoleAlreadyGone confirms a rule Keystone answers 404 for,
// gone with its role or deleted before, counts as success but not as a
// deletion, so the sweep goes on to the roles.
func TestCleanupImpliedRoleAlreadyGone(t *testing.T) {
	f := seedRun()
	f.gone[recordedRule.ID] = true
	deleted, err := Cleanup(context.Background(), f, "run0", []resource.Resource{recordedRule}, time.Minute)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if deleted != 5 {
		t.Errorf("deleted %d resources, want 5 (the gone rule is not counted)", deleted)
	}
	if idx(f.events, "del:r1") < 0 {
		t.Errorf("the role was not deleted after the gone rule; log=%v", f.events)
	}
}

// TestCleanupImpliedRoleDeleteFails confirms a non-404 failure deleting a rule
// names the rule and no longer stops the sweep: the role is still deleted,
// after the failed rule.
func TestCleanupImpliedRoleDeleteFails(t *testing.T) {
	f := seedRun()
	f.failDel[recordedRule.ID] = gophercloud.ErrUnexpectedResponseCode{Actual: 500}
	_, err := Cleanup(context.Background(), f, "run0", []resource.Resource{recordedRule}, time.Minute)
	if err == nil || !strings.HasPrefix(err.Error(), "deleting implied_role r1:r2: ") {
		t.Fatalf("Cleanup error = %v, want one starting with %q", err, "deleting implied_role r1:r2: ")
	}
	failed, role := idx(f.events, "fail:"+recordedRule.ID), idx(f.events, "del:r1")
	if failed < 0 || role < failed {
		t.Errorf("the role was not deleted after the failed rule; log=%v", f.events)
	}
}

// TestCleanupWithoutRecordedImpliedRoles confirms a sweep without a record,
// as keystone cleanup --run-id makes, attempts no rule delete: it lists no
// rules, and Keystone removes them with their roles.
func TestCleanupWithoutRecordedImpliedRoles(t *testing.T) {
	f := seedRun()
	if _, err := Cleanup(context.Background(), f, "run0", nil, time.Minute); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	want := []string{"del:u1:project:p1:r1", "del:u1", "del:p1", "del:r1", "disable:d1", "del:d1"}
	if !slices.Equal(f.events, want) {
		t.Errorf("events = %v, want %v", f.events, want)
	}
}
