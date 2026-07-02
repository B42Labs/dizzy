package executor

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/openstack-tester/internal/cinder"
	"github.com/B42Labs/openstack-tester/internal/resource"
)

// fakeCleaner is an in-process Cleaner that serves volumes and snapshots by
// metadata, records delete and wait-gone events in call order, and can fail a
// specific delete. It lets Cleanup's ordering, idempotency, dedup, and 404
// handling be exercised without a cloud.
type fakeCleaner struct {
	volumes   []resource.Resource
	snapshots []resource.Resource
	gone      map[string]bool  // ids already deleted (a re-delete 404s)
	failDel   map[string]error // id -> error Delete returns
	events    []string         // "del:<id>" / "gone:<id>" in call order
}

func newFakeCleaner() *fakeCleaner {
	return &fakeCleaner{
		gone:    make(map[string]bool),
		failDel: make(map[string]error),
	}
}

func (f *fakeCleaner) ListVolumesByMetadata(ctx context.Context, runID string) ([]resource.Resource, error) {
	return f.live(f.volumes), nil
}

func (f *fakeCleaner) ListSnapshotsByMetadata(ctx context.Context, runID string) ([]resource.Resource, error) {
	return f.live(f.snapshots), nil
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

func (f *fakeCleaner) Delete(ctx context.Context, r resource.Resource) error {
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

func (f *fakeCleaner) WaitForGone(ctx context.Context, r resource.Resource) error {
	f.events = append(f.events, "gone:"+r.ID)
	return nil
}

// seedRun stocks one volume with two snapshots (all carrying the run metadata).
func seedRun() *fakeCleaner {
	f := newFakeCleaner()
	f.volumes = []resource.Resource{{Kind: cinder.KindVolume, ID: "v1"}}
	f.snapshots = []resource.Resource{
		{Kind: cinder.KindSnapshot, ID: "s1"},
		{Kind: cinder.KindSnapshot, ID: "s2"},
	}
	return f
}

func idx(events []string, event string) int {
	return slices.Index(events, event)
}

// TestCleanupSnapshotsBeforeVolumes confirms every snapshot is deleted and
// observed gone strictly before any volume is deleted.
func TestCleanupSnapshotsBeforeVolumes(t *testing.T) {
	f := seedRun()
	deleted, err := Cleanup(context.Background(), f, "run0", nil, time.Minute)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted %d resources, want 3 (2 snapshots + 1 volume)", deleted)
	}

	vol := idx(f.events, "del:v1")
	for _, e := range []string{"del:s1", "gone:s1", "del:s2", "gone:s2", "del:v1"} {
		if idx(f.events, e) < 0 {
			t.Fatalf("event %q never happened; log=%v", e, f.events)
		}
	}
	for _, snapEvent := range []string{"del:s1", "gone:s1", "del:s2", "gone:s2"} {
		if idx(f.events, snapEvent) >= vol {
			t.Errorf("snapshot event %q must precede the volume delete; log=%v", snapEvent, f.events)
		}
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
	if first != 3 {
		t.Fatalf("first Cleanup deleted %d, want 3", first)
	}

	second, err := Cleanup(context.Background(), f, "run0", nil, time.Minute)
	if err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
	if second != 0 {
		t.Errorf("second Cleanup deleted %d resources, want 0 (a no-op)", second)
	}
}

// TestCleanupRefusesEmptyRunID confirms an empty run id is rejected before any
// resource is listed or deleted. Snapshot discovery filters client-side on the
// run metadata, and a missing metadata key reads as "", so an empty run id would
// otherwise match every untagged snapshot in the project and delete it.
func TestCleanupRefusesEmptyRunID(t *testing.T) {
	f := seedRun()
	deleted, err := Cleanup(context.Background(), f, "", nil, time.Minute)
	if err == nil {
		t.Fatal("Cleanup with an empty run id: expected an error, got nil")
	}
	if deleted != 0 {
		t.Errorf("deleted %d resources with an empty run id, want 0", deleted)
	}
	if len(f.events) != 0 {
		t.Errorf("Cleanup touched resources with an empty run id: %v", f.events)
	}
}

// TestCleanupUnionsRecordedAndDedups confirms a resource present only in the run
// record (missed by the metadata listing) is still deleted, and a resource
// present in both is deleted once.
func TestCleanupUnionsRecordedAndDedups(t *testing.T) {
	f := newFakeCleaner()
	// Metadata discovery finds only s1 and v1; the record additionally holds s2
	// (missed by discovery) and v1 again (a duplicate to dedup).
	f.snapshots = []resource.Resource{{Kind: cinder.KindSnapshot, ID: "s1"}}
	f.volumes = []resource.Resource{{Kind: cinder.KindVolume, ID: "v1"}}
	recorded := []resource.Resource{
		{Kind: cinder.KindSnapshot, ID: "s2"},
		{Kind: cinder.KindVolume, ID: "v1"},
	}

	deleted, err := Cleanup(context.Background(), f, "run0", recorded, time.Minute)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	// s1 + s2 + v1, each once.
	if deleted != 3 {
		t.Errorf("deleted %d resources, want 3 (s1, s2, v1 each once)", deleted)
	}
	if got := strCount(f.events, "del:v1"); got != 1 {
		t.Errorf("v1 deleted %d times, want 1 (the duplicate must be deduplicated)", got)
	}
	if idx(f.events, "del:s2") < 0 {
		t.Error("the record-only snapshot s2 was never deleted")
	}
}

// TestCleanupIgnoresNotFound confirms a 404 on delete (a resource removed out of
// band) is treated as success rather than failing the sweep.
func TestCleanupIgnoresNotFound(t *testing.T) {
	f := seedRun()
	f.failDel["s1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 404}

	deleted, err := Cleanup(context.Background(), f, "run0", nil, time.Minute)
	if err != nil {
		t.Fatalf("Cleanup must ignore a 404, got %v", err)
	}
	// s2 and v1 deleted; s1 404'd and is not counted.
	if deleted != 2 {
		t.Errorf("deleted %d resources, want 2 (the 404 snapshot is not counted)", deleted)
	}
	if slices.Contains(f.events, "del:s1") {
		t.Error("the 404 snapshot must not be recorded as deleted")
	}
}

// TestCleanupPropagatesError confirms a non-404 delete error stops the sweep and
// is returned with the count deleted so far.
func TestCleanupPropagatesError(t *testing.T) {
	f := seedRun()
	f.failDel["s1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 500}

	if _, err := Cleanup(context.Background(), f, "run0", nil, time.Minute); err == nil {
		t.Fatal("expected the 500 delete error to propagate")
	}
	// The volume must not be deleted while a snapshot delete is failing.
	if slices.Contains(f.events, "del:v1") {
		t.Error("the volume was deleted despite a failing snapshot delete")
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
