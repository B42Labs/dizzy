package executor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
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
	failDelTimes                    map[string]int   // fail only the first N deletes of an id (0 = every one)
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
		failDelTimes:    make(map[string]int),
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
	if err := f.failDel[r.ID]; err != nil && (f.failDelTimes[r.ID] == 0 || f.attempts[r.ID] <= f.failDelTimes[r.ID]) {
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

	deleted, remaining, err := CleanupWithRetry(context.Background(), f, "", nil, time.Minute, 0)
	if want := "cleanup: empty run id; refusing to delete by an empty prefix"; err == nil || err.Error() != want {
		t.Errorf("CleanupWithRetry with an empty run id: error = %v, want %q", err, want)
	}
	if deleted != 0 || remaining != nil || len(f.events) != 0 {
		t.Errorf("CleanupWithRetry with an empty run id: deleted=%d remaining=%v events=%v, want 0, nil, none", deleted, remaining, f.events)
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

// errUnavailable is the Keystone 503 the retry tests inject.
var errUnavailable = gophercloud.ErrUnexpectedResponseCode{Actual: 503}

// The retry tests run inside a synctest bubble, so the backoff passes on the
// bubble's clock and costs no wall time, and time.Now reads that clock.

// TestCleanupWithRetryRetriesTransient confirms a delete that fails twice with
// a 503 is retried until it succeeds, and the sweep reports no error.
func TestCleanupWithRetryRetriesTransient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := seedRun()
		f.failDel["u1"] = errUnavailable
		f.failDelTimes["u1"] = 2

		deleted, remaining, err := CleanupWithRetry(t.Context(), f, "run0", nil, time.Minute, time.Minute)
		if err != nil {
			t.Fatalf("CleanupWithRetry: %v", err)
		}
		if deleted != 5 {
			t.Errorf("deleted %d resources, want 5", deleted)
		}
		if len(remaining) != 0 {
			t.Errorf("remaining = %v, want none once the retried delete succeeded", remaining)
		}
		if got := f.attempts["u1"]; got != 3 {
			t.Errorf("u1 attempts = %d, want 3", got)
		}
	})
}

// TestCleanupWithRetryRetriesTimeoutAndConnectionErrors confirms an attempt
// that timed out, a refused connection, and a 429 are each retried.
func TestCleanupWithRetryRetriesTimeoutAndConnectionErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"timeout", context.DeadlineExceeded},
		{"connection refused", &url.Error{Op: "Delete", URL: "http://keystone/v3/users/u1", Err: syscall.ECONNREFUSED}},
		{"rate limited", gophercloud.ErrUnexpectedResponseCode{Actual: 429}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := seedRun()
				f.failDel["u1"] = tc.err
				f.failDelTimes["u1"] = 1

				if _, _, err := CleanupWithRetry(t.Context(), f, "run0", nil, time.Minute, time.Minute); err != nil {
					t.Fatalf("CleanupWithRetry: %v", err)
				}
				if got := f.attempts["u1"]; got != 2 {
					t.Errorf("u1 attempts = %d, want 2", got)
				}
			})
		})
	}
}

// TestCleanupWithRetryNeverRetriesTerminal confirms a 400, 403, or 409 is
// attempted once and reported, and a 404 is attempted once and counts as
// already gone.
func TestCleanupWithRetryNeverRetriesTerminal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"bad request", 400, true},
		{"forbidden", 403, true},
		{"conflict", 409, true},
		{"not found", 404, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := seedRun()
				f.failDel["u1"] = gophercloud.ErrUnexpectedResponseCode{Actual: tc.status}

				deleted, _, err := CleanupWithRetry(t.Context(), f, "run0", nil, time.Minute, time.Minute)
				if got := f.attempts["u1"]; got != 1 {
					t.Errorf("u1 attempts = %d, want 1", got)
				}
				if tc.wantErr {
					if err == nil || !strings.HasPrefix(err.Error(), "deleting user u1: ") {
						t.Errorf("CleanupWithRetry error = %v, want one starting with %q", err, "deleting user u1: ")
					}
					return
				}
				if err != nil {
					t.Errorf("CleanupWithRetry: %v", err)
				}
				// assignment + project + role + domain; the gone user is not counted.
				if deleted != 4 {
					t.Errorf("deleted %d resources, want 4", deleted)
				}
			})
		})
	}
}

// TestCleanupWithRetryStopsAtBudget confirms the retries of a call stop once
// the next attempt would start past the budget, that the backoff is capped at
// cleanupRetryMaxDelay, and that the rest of the sweep still runs.
func TestCleanupWithRetryStopsAtBudget(t *testing.T) {
	for _, tc := range []struct {
		budget time.Duration
		want   int
	}{
		// Attempts at 0, 0.25, 0.75, 1.75, 3.75, and 7.75s; the next would
		// start at 15.75s.
		{10 * time.Second, 6},
		// The backoff is capped at 30s; without the cap it would be 10.
		{3 * time.Minute, 12},
	} {
		t.Run(tc.budget.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := seedRun()
				f.failDel["u1"] = errUnavailable

				_, _, err := CleanupWithRetry(t.Context(), f, "run0", nil, time.Minute, tc.budget)
				if got := f.attempts["u1"]; got != tc.want {
					t.Errorf("u1 attempts = %d, want %d", got, tc.want)
				}
				if err == nil || !strings.HasPrefix(err.Error(), "deleting user u1: ") {
					t.Errorf("CleanupWithRetry error = %v, want one starting with %q", err, "deleting user u1: ")
				}
				for _, e := range []string{"del:p1", "del:r1", "del:d1"} {
					if !slices.Contains(f.events, e) {
						t.Errorf("event %q never happened after the budget ran out; log=%v", e, f.events)
					}
				}
			})
		})
	}
}

// TestCleanupZeroBudgetMakesOneAttempt confirms Cleanup, a zero budget, never
// retries a transient failure.
func TestCleanupZeroBudgetMakesOneAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := seedRun()
		f.failDel["u1"] = errUnavailable

		if _, err := Cleanup(t.Context(), f, "run0", nil, time.Minute); err == nil {
			t.Fatal("Cleanup: expected the 503 to be reported")
		}
		if got := f.attempts["u1"]; got != 1 {
			t.Errorf("u1 attempts = %d, want 1", got)
		}
	})
}

// TestCleanupWithRetryStopsOnCancel confirms a parent context cancelled during
// a backoff ends that call's retries with the cancellation.
func TestCleanupWithRetryStopsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := seedRun()
		f.failDel["u1"] = errUnavailable
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		done := make(chan error, 1)
		go func() {
			_, _, err := CleanupWithRetry(ctx, f, "run0", nil, time.Minute, time.Minute)
			done <- err
		}()
		synctest.Wait() // the sweep now waits out u1's first backoff
		cancel()
		err := <-done

		if got := f.attempts["u1"]; got != 1 {
			t.Errorf("u1 attempts = %d, want 1", got)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("CleanupWithRetry error = %v, want one wrapping context.Canceled", err)
		}
	})
}

// remainingIDs returns the ids of rs, in order.
func remainingIDs(rs []resource.Resource) []string {
	var ids []string
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestCleanupWithRetryReportsRemaining confirms CleanupWithRetry returns what
// the sweep left behind: each resource whose delete or disable failed with
// anything but a 404, in call order, and nothing for a 404 or a failed
// listing. Every case passes a budget of 0, so a 500 or a 503 gets one attempt.
func TestCleanupWithRetryReportsRemaining(t *testing.T) {
	errProject := errors.New("project boom")
	listErr := fmt.Errorf("listing users by prefix: %w", errUnavailable)
	for _, tc := range []struct {
		name        string
		setup       func() (*fakeCleaner, []resource.Resource)
		wantIDs     []string
		wantDeleted int
		wantErr     func(error) bool // nil: the error must be nil
	}{
		{
			name: "failed user and project deletes",
			setup: func() (*fakeCleaner, []resource.Resource) {
				f := seedRun()
				f.failDel["u1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 500}
				f.failDel["p1"] = errProject
				return f, nil
			},
			wantIDs:     []string{"u1", "p1"},
			wantDeleted: 3, // assignment + role + domain
			wantErr: func(err error) bool {
				return gophercloud.ResponseCodeIs(err, 500) && errors.Is(err, errProject)
			},
		},
		{
			name:        "nothing fails",
			setup:       func() (*fakeCleaner, []resource.Resource) { return seedRun(), nil },
			wantDeleted: 5,
		},
		{
			name:  "nothing to delete",
			setup: func() (*fakeCleaner, []resource.Resource) { return newFakeCleaner(), nil },
		},
		{
			name: "recorded user already gone",
			setup: func() (*fakeCleaner, []resource.Resource) {
				f := newFakeCleaner()
				f.gone["u1"] = true
				return f, []resource.Resource{{Kind: keystone.KindUser, ID: "u1", Name: "dizzy-run0-user-0001"}}
			},
		},
		{
			name: "delete timed out",
			setup: func() (*fakeCleaner, []resource.Resource) {
				f := seedRun()
				f.failDel["u1"] = context.DeadlineExceeded
				return f, nil
			},
			wantIDs:     []string{"u1"},
			wantDeleted: 4, // assignment + project + role + domain
			wantErr:     func(err error) bool { return errors.Is(err, context.DeadlineExceeded) },
		},
		{
			name: "delete canceled",
			setup: func() (*fakeCleaner, []resource.Resource) {
				f := seedRun()
				f.failDel["u1"] = context.Canceled
				return f, nil
			},
			wantIDs:     []string{"u1"},
			wantDeleted: 4, // assignment + project + role + domain
			wantErr:     func(err error) bool { return errors.Is(err, context.Canceled) },
		},
		{
			name: "failed domain disable",
			setup: func() (*fakeCleaner, []resource.Resource) {
				f := newFakeCleaner()
				f.domains = []resource.Resource{
					{Kind: keystone.KindDomain, ID: "d1", Name: "dizzy-run0-dom-0001"},
					{Kind: keystone.KindDomain, ID: "d2", Name: "dizzy-run0-dom-0002"},
				}
				f.failDisable["d1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 500}
				return f, nil
			},
			wantIDs:     []string{"d1"},
			wantDeleted: 1, // d2
			wantErr:     func(err error) bool { return gophercloud.ResponseCodeIs(err, 500) },
		},
		{
			// The unlisted user is neither deleted nor reported, which is why
			// the monitor still queues the run: its next sweep lists it again.
			name: "failed user listing",
			setup: func() (*fakeCleaner, []resource.Resource) {
				f := seedRun()
				f.failList["users"] = listErr
				return f, nil
			},
			wantDeleted: 3, // project + role + domain
			wantErr:     func(err error) bool { return errors.Is(err, listErr) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, recorded := tc.setup()
			deleted, remaining, err := CleanupWithRetry(context.Background(), f, "run0", recorded, time.Minute, 0)
			if got := remainingIDs(remaining); !slices.Equal(got, tc.wantIDs) {
				t.Errorf("remaining ids = %v, want %v", got, tc.wantIDs)
			}
			if deleted != tc.wantDeleted {
				t.Errorf("deleted %d resources, want %d", deleted, tc.wantDeleted)
			}
			switch {
			case tc.wantErr == nil && err != nil:
				t.Errorf("CleanupWithRetry: %v", err)
			case tc.wantErr != nil && !tc.wantErr(err):
				t.Errorf("CleanupWithRetry error = %v, want one wrapping the injected failure", err)
			}
		})
	}
}

// TestCleanupOfRemainingDeletesNothingTwice is the regression guard for a
// later cleanup deleting again what a failed one already deleted: given what
// the first sweep left behind, the second sends a delete for that leftover
// and none for a resource the first sweep deleted.
func TestCleanupOfRemainingDeletesNothingTwice(t *testing.T) {
	f := seedRun()
	recorded := []resource.Resource{f.userAssignments["u1"][0], f.users[0], f.projects[0], f.roles[0], f.domains[0]}
	f.failDel["u1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 500}
	f.failDelTimes["u1"] = 1

	_, remaining, err := CleanupWithRetry(context.Background(), f, "run0", recorded, time.Minute, 0)
	if err == nil {
		t.Fatal("first CleanupWithRetry: expected the user's 500 to be reported")
	}
	if got := remainingIDs(remaining); !slices.Equal(got, []string{"u1"}) {
		t.Fatalf("remaining ids = %v, want [u1]", got)
	}

	deleted, err := Cleanup(context.Background(), f, "run0", remaining, time.Minute)
	if err != nil || deleted != 1 {
		t.Fatalf("second Cleanup = %d, %v; want 1, nil", deleted, err)
	}
	if got := f.attempts["u1"]; got != 2 {
		t.Errorf("u1 attempts = %d, want 2", got)
	}
	for _, id := range []string{"u1:project:p1:r1", "p1", "r1", "d1"} {
		if got := f.attempts[id]; got != 1 {
			t.Errorf("%s attempts = %d, want 1 (deleted by the first sweep only)", id, got)
		}
	}
}
