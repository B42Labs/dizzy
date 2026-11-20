package executor

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/horizon"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeCleaner lists the users and projects it holds and deletes them;
// deleteErr makes the delete of an id fail with that error every time.
type fakeCleaner struct {
	mu        sync.Mutex
	users     []resource.Resource
	projects  []resource.Resource
	deleted   []string
	deleteErr map[string]error
	lists     int
}

func (f *fakeCleaner) ListUsersByRun(context.Context, string) ([]resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return slices.Clone(f.users), nil
}

func (f *fakeCleaner) ListProjectsByRun(context.Context, string) ([]resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return slices.Clone(f.projects), nil
}

func (f *fakeCleaner) Delete(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.deleteErr[r.ID]; err != nil {
		return err
	}
	drop := func(rs []resource.Resource) ([]resource.Resource, bool) {
		for i, x := range rs {
			if x.ID == r.ID {
				return slices.Delete(rs, i, i+1), true
			}
		}
		return rs, false
	}
	var found bool
	if f.users, found = drop(f.users); !found {
		f.projects, found = drop(f.projects)
	}
	if !found {
		return gophercloud.ErrUnexpectedResponseCode{Actual: 404}
	}
	f.deleted = append(f.deleted, r.ID)
	return nil
}

func user(id string) resource.Resource {
	return resource.Resource{Kind: horizon.KindUser, Name: "dizzyhorizon-run1-" + id, ID: id}
}

func project(id string) resource.Resource {
	return resource.Resource{Kind: horizon.KindProject, Name: "dizzyhorizon-run1-" + id, ID: id}
}

// TestCleanupDeletesUsersThenProjects asserts users go before projects, the
// listing is unioned with the record, a recorded resource without the
// prefix is skipped, a recorded resource already gone counts as success,
// and a second run deletes nothing.
func TestCleanupDeletesUsersThenProjects(t *testing.T) {
	f := &fakeCleaner{
		users:    []resource.Resource{user("u1")},
		projects: []resource.Resource{project("p1")},
	}
	// u2 and p2 the listing missed; dizzy-other belongs to someone else;
	// u3 is already gone.
	f.users = append(f.users, user("u2"))
	f.projects = append(f.projects, project("p2"))
	recorded := []resource.Resource{
		user("u2"), project("p2"), user("u3"),
		{Kind: horizon.KindUser, Name: "dizzy-other", ID: "x1"},
	}
	listed := &fakeCleaner{users: f.users[:1], projects: f.projects[:1]}
	c := cleanerView{lists: listed, del: f}

	n, err := Cleanup(t.Context(), c, "run1", recorded, 1, time.Second)
	if err != nil || n != 4 {
		t.Fatalf("Cleanup() = %d, %v, want 4, nil", n, err)
	}
	if want := []string{"u1", "u2", "p1", "p2"}; !slices.Equal(f.deleted, want) {
		t.Errorf("deleted %v, want %v", f.deleted, want)
	}

	listed.users, listed.projects = nil, nil
	if n, err := Cleanup(t.Context(), c, "run1", recorded, 4, time.Second); err != nil || n != 0 {
		t.Errorf("second Cleanup() = %d, %v, want 0, nil", n, err)
	}
}

// cleanerView lists through lists and deletes through del, so a test can
// hand Cleanup a listing that misses what the record holds.
type cleanerView struct {
	lists *fakeCleaner
	del   *fakeCleaner
}

func (v cleanerView) ListUsersByRun(ctx context.Context, id string) ([]resource.Resource, error) {
	return v.lists.ListUsersByRun(ctx, id)
}

func (v cleanerView) ListProjectsByRun(ctx context.Context, id string) ([]resource.Resource, error) {
	return v.lists.ListProjectsByRun(ctx, id)
}

func (v cleanerView) Delete(ctx context.Context, r resource.Resource) error {
	return v.del.Delete(ctx, r)
}

// TestCleanupNothingFound asserts a run without users or projects returns 0,
// nil.
func TestCleanupNothingFound(t *testing.T) {
	if n, err := Cleanup(t.Context(), &fakeCleaner{}, "run1", nil, 4, time.Second); n != 0 || err != nil {
		t.Errorf("Cleanup() = %d, %v, want 0, nil", n, err)
	}
}

// TestCleanupRefusesEmptyRunID asserts an empty run id is refused before any
// listing.
func TestCleanupRefusesEmptyRunID(t *testing.T) {
	f := &fakeCleaner{users: []resource.Resource{user("u1")}}
	n, err := Cleanup(t.Context(), f, "", nil, 4, time.Second)
	if err == nil || !strings.Contains(err.Error(), "empty run id") || n != 0 || f.lists != 0 {
		t.Errorf("Cleanup(\"\") = %d, %v after %d listing(s), want a refusal before any", n, err, f.lists)
	}
}

// TestCleanupStopsOnPersistentError asserts a delete that answers 500 on
// every one of the five attempts stops the cleanup with the count so far
// and the error, before any project.
func TestCleanupStopsOnPersistentError(t *testing.T) {
	var attempts int
	f := &fakeCleaner{
		users:     []resource.Resource{user("u1"), user("u2")},
		projects:  []resource.Resource{project("p1")},
		deleteErr: map[string]error{},
	}
	f.deleteErr["u2"] = gophercloud.ErrUnexpectedResponseCode{Actual: 500}
	counting := countingCleaner{fakeCleaner: f, attempts: &attempts, id: "u2"}

	n, err := Cleanup(t.Context(), counting, "run1", nil, 1, time.Second)
	if !gophercloud.ResponseCodeIs(err, 500) || n != 1 {
		t.Fatalf("Cleanup() = %d, %v, want 1 and the 500", n, err)
	}
	if attempts != 5 {
		t.Errorf("u2 was deleted %d time(s), want 5 attempts", attempts)
	}
	if slices.Contains(f.deleted, "p1") {
		t.Error("a project was deleted after the user delete failed")
	}
}

// countingCleaner counts the delete attempts of id.
type countingCleaner struct {
	*fakeCleaner
	attempts *int
	id       string
}

func (c countingCleaner) Delete(ctx context.Context, r resource.Resource) error {
	if r.ID == c.id {
		*c.attempts++
	}
	return c.fakeCleaner.Delete(ctx, r)
}
