package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/glance"
	"github.com/B42Labs/dizzy/internal/resource"
)

// notFound is a gophercloud 404, the error a second delete of an already-gone
// image returns so cleanup treats it as an idempotent no-op.
var notFound = gophercloud.ErrUnexpectedResponseCode{Actual: 404}

// fakeCleaner is an in-memory Cleaner: it holds the images discoverable by tag
// and deletes from a shared live set, so a second Cleanup is a no-op. Cleanup now
// deletes concurrently, so mu guards the shared live/deleted state.
type fakeCleaner struct {
	mu           sync.Mutex
	images       []resource.Resource
	live         map[string]bool
	deleted      []string
	failDeleteID string // id whose delete returns a non-404 error
}

func newFakeCleaner(imgs ...resource.Resource) *fakeCleaner {
	f := &fakeCleaner{images: imgs, live: map[string]bool{}}
	for _, r := range imgs {
		f.live[r.ID] = true
	}
	return f
}

func (f *fakeCleaner) ListImagesByTag(_ context.Context, _ string) ([]resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []resource.Resource
	for _, r := range f.images {
		if f.live[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeCleaner) Delete(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.ID == f.failDeleteID {
		return errors.New("boom")
	}
	if !f.live[r.ID] {
		return notFound
	}
	f.live[r.ID] = false
	f.deleted = append(f.deleted, r.ID)
	return nil
}

func (f *fakeCleaner) WaitForGone(context.Context, resource.Resource) error { return nil }

func img(id string) resource.Resource {
	return resource.Resource{Kind: glance.KindImage, ID: id}
}

func TestCleanupCount(t *testing.T) {
	f := newFakeCleaner(img("i1"), img("i2"))
	deleted, err := Cleanup(context.Background(), f, "run0", nil, 4, time.Second)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
}

func TestCleanupIsIdempotent(t *testing.T) {
	f := newFakeCleaner(img("i1"))
	if _, err := Cleanup(context.Background(), f, "run0", nil, 4, time.Second); err != nil {
		t.Fatalf("first Cleanup() = %v", err)
	}
	deleted, err := Cleanup(context.Background(), f, "run0", nil, 4, time.Second)
	if err != nil {
		t.Fatalf("second Cleanup() = %v", err)
	}
	if deleted != 0 {
		t.Errorf("second Cleanup deleted = %d, want 0", deleted)
	}
}

func TestCleanupUnionsRecord(t *testing.T) {
	// Tag discovery misses the image; the run record supplies it.
	f := newFakeCleaner()
	f.live["i-rec"] = true
	recorded := []resource.Resource{img("i-rec")}
	deleted, err := Cleanup(context.Background(), f, "run0", recorded, 4, time.Second)
	if err != nil {
		t.Fatalf("Cleanup() = %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (from the record)", deleted)
	}
}

func TestCleanupRefusesEmptyRunID(t *testing.T) {
	f := newFakeCleaner(img("i1"))
	if _, err := Cleanup(context.Background(), f, "", nil, 4, time.Second); err == nil {
		t.Fatal("Cleanup with an empty run id: expected an error, got nil")
	}
	if len(f.deleted) != 0 {
		t.Errorf("Cleanup with an empty run id deleted %v; it must delete nothing", f.deleted)
	}
}

func TestCleanupStopsOnFirstError(t *testing.T) {
	f := newFakeCleaner(img("i1"), img("i2"))
	f.failDeleteID = "i1"
	deleted, err := Cleanup(context.Background(), f, "run0", nil, 4, time.Second)
	if err == nil {
		t.Fatal("Cleanup() = nil, want the delete error")
	}
	// The failing delete is never counted; a concurrent sibling may or may not be
	// reached before the stage is cancelled, but the reported count always matches
	// what was actually removed and never includes the failure.
	if deleted != len(f.deleted) {
		t.Errorf("deleted = %d, want %d (the images actually removed)", deleted, len(f.deleted))
	}
}
