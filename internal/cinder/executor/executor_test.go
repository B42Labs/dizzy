package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/openstack-tester/internal/cinder"
	"github.com/B42Labs/openstack-tester/internal/cinder/plan"
	"github.com/B42Labs/openstack-tester/internal/resource"
)

// fakeCinder is an in-process Cinder implementation that records call order and
// per-volume snapshot concurrency, can inject transient, quota, and terminal
// failures, and can block until cancelled. It lets the executor's stage
// ordering, snapshot serialization, retry, fail-fast, and cancellation logic be
// exercised without a cloud.
type fakeCinder struct {
	mu     sync.Mutex
	nextID int

	opOrder  []string       // "vol" / "ext" / "snap" in call-start order
	extended map[string]int // volume logical -> new size passed to ExtendVolume
	attempts map[string]int // create attempts per logical name

	snapBusy         map[string]bool // volume logical -> a snapshot is in progress
	distinctBusy     int             // volumes currently snapshotting
	maxDistinctBusy  int             // high-water mark of concurrent volumes
	sameVolViolation bool            // two snapshots of one volume overlapped

	createDelay     time.Duration    // sleep inside each create to expose concurrency
	failuresLeft    map[string]int   // logical -> remaining transient failures
	quotaKind       resource.Kind    // kind to reject with a quota error ("" = none)
	terminalWait    map[string]error // logical -> error WaitForReady returns
	holdUntilCancel bool             // block each create until ctx is cancelled

	started     chan struct{}
	startedOnce sync.Once
}

func newFake() *fakeCinder {
	return &fakeCinder{
		extended:     make(map[string]int),
		attempts:     make(map[string]int),
		snapBusy:     make(map[string]bool),
		failuresLeft: make(map[string]int),
		terminalWait: make(map[string]error),
	}
}

func (f *fakeCinder) CreateVolume(ctx context.Context, v plan.Volume, volumeType string) (resource.Resource, error) {
	if f.started != nil {
		f.startedOnce.Do(func() { close(f.started) })
	}
	f.mu.Lock()
	f.opOrder = append(f.opOrder, "vol")
	f.attempts[v.Name]++
	quota := f.quotaKind == cinder.KindVolume
	fail := f.failuresLeft[v.Name] > 0
	if fail {
		f.failuresLeft[v.Name]--
	}
	f.mu.Unlock()

	switch {
	case quota:
		return resource.Resource{}, fmt.Errorf("fake quota: %w", cinder.ErrQuota)
	case fail:
		return resource.Resource{}, gophercloud.ErrUnexpectedResponseCode{Actual: 503}
	case f.holdUntilCancel:
		<-ctx.Done()
		return resource.Resource{}, ctx.Err()
	}
	if f.createDelay > 0 {
		time.Sleep(f.createDelay)
	}
	return f.record(cinder.KindVolume, v.Name), nil
}

func (f *fakeCinder) ExtendVolume(ctx context.Context, r resource.Resource, newSizeGiB int) error {
	f.mu.Lock()
	f.opOrder = append(f.opOrder, "ext")
	f.extended[r.Logical] = newSizeGiB
	f.mu.Unlock()
	return nil
}

func (f *fakeCinder) CreateSnapshot(ctx context.Context, s plan.Snapshot, volumeID string) (resource.Resource, error) {
	f.mu.Lock()
	f.opOrder = append(f.opOrder, "snap")
	if f.snapBusy[s.Volume] {
		f.sameVolViolation = true
	}
	f.snapBusy[s.Volume] = true
	f.distinctBusy++
	if f.distinctBusy > f.maxDistinctBusy {
		f.maxDistinctBusy = f.distinctBusy
	}
	quota := f.quotaKind == cinder.KindSnapshot
	f.mu.Unlock()

	if quota {
		f.clearSnap(s.Volume)
		return resource.Resource{}, fmt.Errorf("fake quota: %w", cinder.ErrQuota)
	}
	if f.createDelay > 0 {
		time.Sleep(f.createDelay)
	}
	return f.record(cinder.KindSnapshot, s.Volume), nil
}

// WaitForReady clears a snapshot's busy window (closing the per-volume serial
// window that began at CreateSnapshot) and returns any injected terminal error
// for the resource.
func (f *fakeCinder) WaitForReady(ctx context.Context, r resource.Resource) error {
	f.mu.Lock()
	if r.Kind == cinder.KindSnapshot {
		f.snapBusy[r.Logical] = false
		f.distinctBusy--
	}
	err := f.terminalWait[r.Logical]
	f.mu.Unlock()
	return err
}

func (f *fakeCinder) clearSnap(volume string) {
	f.mu.Lock()
	f.snapBusy[volume] = false
	f.distinctBusy--
	f.mu.Unlock()
}

func (f *fakeCinder) record(kind resource.Kind, logical string) resource.Resource {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	return resource.Resource{Kind: kind, Logical: logical, ID: fmt.Sprintf("id-%d", f.nextID)}
}

// fullPlan mixes resized and unresized volumes plus snapshots on several of them.
func fullPlan() *plan.Plan {
	return &plan.Plan{
		Scenario: "test",
		Seed:     1,
		Volumes: []plan.Volume{
			{Name: "vol-0001", SizeGiB: 2, ResizeToGiB: 5},
			{Name: "vol-0002", SizeGiB: 3},
			{Name: "vol-0003", SizeGiB: 1, ResizeToGiB: 4},
		},
		Snapshots: []plan.Snapshot{
			{Name: "snap-0001", Volume: "vol-0001"},
			{Name: "snap-0002", Volume: "vol-0001"},
			{Name: "snap-0003", Volume: "vol-0003"},
		},
	}
}

// TestApplyStageOrdering confirms the three stages are strictly ordered: every
// volume create precedes every extend, which precedes every snapshot.
func TestApplyStageOrdering(t *testing.T) {
	f := newFake()
	res, err := Apply(context.Background(), f, fullPlan(), 4, time.Minute, "")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 3 volumes + 3 snapshots created (extends create no resource).
	if len(res.Created) != 6 {
		t.Errorf("created %d resources, want 6", len(res.Created))
	}

	// Once an extend has happened, no volume create may follow; once a snapshot
	// has happened, no extend or volume create may follow.
	phase := 0
	rank := map[string]int{"vol": 0, "ext": 1, "snap": 2}
	for i, op := range f.opOrder {
		if rank[op] < phase {
			t.Fatalf("op %q at index %d violates stage order %v", op, i, f.opOrder)
		}
		phase = rank[op]
	}
}

// TestApplyExtendsOnlyPlanned confirms only volumes with a resize target are
// extended, each to exactly its ResizeToGiB.
func TestApplyExtendsOnlyPlanned(t *testing.T) {
	f := newFake()
	if _, err := Apply(context.Background(), f, fullPlan(), 4, time.Minute, ""); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := map[string]int{"vol-0001": 5, "vol-0003": 4}
	if len(f.extended) != len(want) {
		t.Fatalf("extended %v, want %v", f.extended, want)
	}
	for name, size := range want {
		if f.extended[name] != size {
			t.Errorf("extended[%q] = %d, want %d", name, f.extended[name], size)
		}
	}
}

// TestApplySnapshotSerialization confirms snapshots of the same volume never
// overlap while snapshots of different volumes do run concurrently.
func TestApplySnapshotSerialization(t *testing.T) {
	// Four volumes, each with two snapshots, so cross-volume concurrency is
	// possible and same-volume serialization is exercised.
	p := &plan.Plan{Volumes: nil}
	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("vol-%04d", i)
		p.Volumes = append(p.Volumes, plan.Volume{Name: name, SizeGiB: 1})
		p.Snapshots = append(p.Snapshots,
			plan.Snapshot{Name: fmt.Sprintf("snap-%04da", i), Volume: name},
			plan.Snapshot{Name: fmt.Sprintf("snap-%04db", i), Volume: name},
		)
	}

	f := newFake()
	f.createDelay = 10 * time.Millisecond // widen the window so groups overlap

	if _, err := Apply(context.Background(), f, p, 4, time.Minute, ""); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.sameVolViolation {
		t.Error("two snapshots of the same volume overlapped; they must be serialized")
	}
	if f.maxDistinctBusy < 2 {
		t.Errorf("max concurrent volumes snapshotting = %d, want > 1 (snapshots of different volumes must run concurrently)", f.maxDistinctBusy)
	}
}

// TestApplyFailFastQuota confirms a quota error on volume create stops the run
// immediately with a quota-mentioning error and returns the partial Created.
func TestApplyFailFastQuota(t *testing.T) {
	f := newFake()
	f.quotaKind = cinder.KindVolume

	res, err := Apply(context.Background(), f, fullPlan(), 4, time.Minute, "")
	if err == nil {
		t.Fatal("expected a quota error, got nil")
	}
	if !errors.Is(err, cinder.ErrQuota) {
		t.Errorf("error %v does not match ErrQuota", err)
	}
	if len(res.Created) != 0 {
		t.Errorf("created %d resources after a quota failure on every volume, want 0", len(res.Created))
	}
	// A quota error must not be retried.
	for name, n := range f.attempts {
		if n > 1 {
			t.Errorf("quota error was retried: %s attempted %d times", name, n)
		}
	}
}

// TestApplyTerminalVolumeErrorFails confirms a volume reaching a terminal error
// status fails the run while its resource is still recorded for cleanup.
func TestApplyTerminalVolumeErrorFails(t *testing.T) {
	f := newFake()
	f.terminalWait["vol-0002"] = errors.New("volume vol-0002 reached terminal status \"error\"")

	res, err := Apply(context.Background(), f, fullPlan(), 4, time.Minute, "")
	if err == nil {
		t.Fatal("expected the terminal volume error to fail the run")
	}
	// The errored volume exists and must be recorded so cleanup can remove it.
	var found bool
	for _, r := range res.Created {
		if r.Logical == "vol-0002" {
			found = true
		}
	}
	if !found {
		t.Error("the terminally-errored volume was not recorded in Created")
	}
	// No snapshots should have been created after the stage-1 failure.
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, op := range f.opOrder {
		if op == "snap" {
			t.Error("a snapshot was created after a stage-1 volume failure")
		}
	}
}

// TestApplyRetriesTransient confirms a transient error is retried with backoff
// and the create ultimately succeeds.
func TestApplyRetriesTransient(t *testing.T) {
	f := newFake()
	f.failuresLeft["vol-0001"] = 2 // fail twice, succeed on the third attempt

	p := &plan.Plan{Volumes: []plan.Volume{{Name: "vol-0001", SizeGiB: 1}}}
	res, err := Apply(context.Background(), f, p, 1, time.Minute, "")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Created) != 1 {
		t.Fatalf("created %d resources, want 1", len(res.Created))
	}
	if f.attempts["vol-0001"] != 3 {
		t.Errorf("vol-0001 attempted %d times, want 3", f.attempts["vol-0001"])
	}
}

// TestApplyCancellation confirms cancelling mid-run stops promptly and returns a
// context error.
func TestApplyCancellation(t *testing.T) {
	f := newFake()
	f.holdUntilCancel = true
	f.started = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := Apply(ctx, f, &plan.Plan{Volumes: []plan.Volume{{Name: "vol-0001", SizeGiB: 1}}}, 1, time.Minute, "")
		done <- result{err}
	}()

	<-f.started
	cancel()

	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("Apply returned %v, want context.Canceled", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Apply did not return after cancellation")
	}
}

// TestApplyConcurrencyBoundVolumes confirms no more than concurrency volume
// creates run at once while still saturating the limit.
func TestApplyConcurrencyBoundVolumes(t *testing.T) {
	const concurrency = 5
	vols := make([]plan.Volume, 30)
	for i := range vols {
		vols[i] = plan.Volume{Name: fmt.Sprintf("vol-%04d", i), SizeGiB: 1}
	}
	f := &countingFake{fakeCinder: newFake()}
	f.createDelay = 10 * time.Millisecond

	if _, err := Apply(context.Background(), f, &plan.Plan{Volumes: vols}, concurrency, time.Minute, ""); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.maxInFlight > concurrency {
		t.Errorf("max in-flight %d exceeded concurrency %d", f.maxInFlight, concurrency)
	}
	if f.maxInFlight != concurrency {
		t.Errorf("max in-flight %d did not reach concurrency %d", f.maxInFlight, concurrency)
	}
}

// countingFake wraps fakeCinder to track create concurrency without disturbing
// the base fake's snapshot bookkeeping.
type countingFake struct {
	*fakeCinder
	cmu         sync.Mutex
	inFlight    int
	maxInFlight int
}

func (c *countingFake) CreateVolume(ctx context.Context, v plan.Volume, volumeType string) (resource.Resource, error) {
	c.cmu.Lock()
	c.inFlight++
	if c.inFlight > c.maxInFlight {
		c.maxInFlight = c.inFlight
	}
	c.cmu.Unlock()
	defer func() {
		c.cmu.Lock()
		c.inFlight--
		c.cmu.Unlock()
	}()
	return c.fakeCinder.CreateVolume(ctx, v, volumeType)
}
