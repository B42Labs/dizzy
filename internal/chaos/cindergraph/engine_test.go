package cindergraph

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/B42Labs/openstack-tester/internal/chaos"
	"github.com/B42Labs/openstack-tester/internal/cinder"
	cinderplan "github.com/B42Labs/openstack-tester/internal/cinder/plan"
	"github.com/B42Labs/openstack-tester/internal/resource"
)

// volInstance is a live volume in the fake cloud.
type volInstance struct {
	logical string
	status  string
	sizeGiB int
}

// snapInstance is a live snapshot in the fake cloud, carrying the gigabytes it
// occupies (its source volume's size at snapshot time).
type snapInstance struct {
	logical   string
	sourceVol string
	sizeGiB   int
}

// fakeCinder is an in-process Cinder that tracks the live population, per-volume
// family serialization, and the gigabytes consumed, so the cinder churn graph's
// lifecycle, envelope, extend-once, readiness, and serialization guarantees can
// be checked without a cloud. It is safe for concurrent use by the engine's
// operation tasks.
type fakeCinder struct {
	mu     sync.Mutex
	nextID int

	vols  map[string]*volInstance  // volume id -> instance
	snaps map[string]*snapInstance // snapshot id -> instance

	liveVol  map[string]bool // volume logical -> a live instance exists
	liveSnap map[string]bool // snapshot logical -> a live instance exists

	// config: volumes whose readiness never completes or fails terminally.
	stuck   map[string]bool // logical -> stays "creating"
	errored map[string]bool // logical -> "error" status
	opDelay time.Duration    // sleep inside family-serialized ops to expose races

	// observation
	createVolCalls     int
	createSnapByVol    map[string]int // source volume logical -> snapshot creates
	extendByVol        map[string]int // volume logical -> extend calls
	extendByID         map[string]int // volume id -> extend calls (once per lifetime)
	extendSize         map[string]int // volume logical -> last extend target
	volDeleteByLogical map[string]int // volume logical -> deletes

	// live counts and high-water marks
	liveVolCount, maxVolCount   int
	liveSnapCount, maxSnapCount int
	liveGiB, maxGiB             int64

	// invariant violations the graph must never cause
	doubleLiveVol      bool // a volume logical created while already live
	doubleLiveSnap     bool // a snapshot logical created while already live
	snapOnAbsentVol    bool // snapshot created against a gone source volume
	volDeleteWithSnaps bool // volume deleted while it still had live snapshots
	familyBusy         map[string]bool
	familyViolation    bool // two family ops on one volume overlapped
}

func newFakeCinder() *fakeCinder {
	return &fakeCinder{
		vols:               make(map[string]*volInstance),
		snaps:              make(map[string]*snapInstance),
		liveVol:            make(map[string]bool),
		liveSnap:           make(map[string]bool),
		stuck:              make(map[string]bool),
		errored:            make(map[string]bool),
		createSnapByVol:    make(map[string]int),
		extendByVol:        make(map[string]int),
		extendByID:         make(map[string]int),
		extendSize:         make(map[string]int),
		volDeleteByLogical: make(map[string]int),
		familyBusy:         make(map[string]bool),
	}
}

// enterFamily opens a per-volume serial window with a delay, flagging an overlap
// if another family operation is already inside one for the same volume. The
// cinder graph holds a per-volume mutex across its snapshot/extend operations,
// so a correct build never overlaps.
func (f *fakeCinder) enterFamily(vol string) {
	f.mu.Lock()
	if f.familyBusy[vol] {
		f.familyViolation = true
	}
	f.familyBusy[vol] = true
	f.mu.Unlock()
	if f.opDelay > 0 {
		time.Sleep(f.opDelay)
	}
	f.mu.Lock()
	f.familyBusy[vol] = false
	f.mu.Unlock()
}

func (f *fakeCinder) CreateVolume(_ context.Context, v cinderplan.Volume, _ string) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createVolCalls++
	if f.liveVol[v.Name] {
		f.doubleLiveVol = true
	}
	f.nextID++
	id := fmt.Sprintf("vol-%d", f.nextID)
	status := "available"
	switch {
	case f.stuck[v.Name]:
		status = "creating"
	case f.errored[v.Name]:
		status = "error"
	}
	f.vols[id] = &volInstance{logical: v.Name, status: status, sizeGiB: v.SizeGiB}
	f.liveVol[v.Name] = true
	f.liveVolCount++
	if f.liveVolCount > f.maxVolCount {
		f.maxVolCount = f.liveVolCount
	}
	f.addGiB(int64(v.SizeGiB))
	return resource.Resource{Kind: cinder.KindVolume, Logical: v.Name, ID: id}, nil
}

func (f *fakeCinder) ExtendVolume(_ context.Context, r resource.Resource, newSizeGiB int) error {
	f.enterFamily(r.Logical)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extendByVol[r.Logical]++
	f.extendByID[r.ID]++
	f.extendSize[r.Logical] = newSizeGiB
	if v := f.vols[r.ID]; v != nil {
		f.addGiB(int64(newSizeGiB - v.sizeGiB))
		v.sizeGiB = newSizeGiB
	}
	return nil
}

func (f *fakeCinder) CreateSnapshot(_ context.Context, s cinderplan.Snapshot, volumeID string) (resource.Resource, error) {
	f.mu.Lock()
	f.createSnapByVol[s.Volume]++
	src := f.vols[volumeID]
	if src == nil || !f.liveVol[s.Volume] {
		f.snapOnAbsentVol = true
	}
	if f.liveSnap[s.Name] {
		f.doubleLiveSnap = true
	}
	srcSize := 0
	if src != nil {
		srcSize = src.sizeGiB
	}
	f.mu.Unlock()

	f.enterFamily(s.Volume)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("snap-%d", f.nextID)
	f.snaps[id] = &snapInstance{logical: s.Name, sourceVol: s.Volume, sizeGiB: srcSize}
	f.liveSnap[s.Name] = true
	f.liveSnapCount++
	if f.liveSnapCount > f.maxSnapCount {
		f.maxSnapCount = f.liveSnapCount
	}
	f.addGiB(int64(srcSize))
	return resource.Resource{Kind: cinder.KindSnapshot, Logical: s.Name, ID: id}, nil
}

func (f *fakeCinder) Delete(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Kind {
	case cinder.KindVolume:
		f.volDeleteByLogical[r.Logical]++
		for _, s := range f.snaps {
			if s.sourceVol == r.Logical {
				f.volDeleteWithSnaps = true
			}
		}
		if v := f.vols[r.ID]; v != nil {
			f.addGiB(-int64(v.sizeGiB))
			f.liveVolCount--
			delete(f.vols, r.ID)
		}
		f.liveVol[r.Logical] = false
	case cinder.KindSnapshot:
		if s := f.snaps[r.ID]; s != nil {
			f.addGiB(-int64(s.sizeGiB))
			f.liveSnapCount--
			delete(f.snaps, r.ID)
		}
		f.liveSnap[r.Logical] = false
	}
	return nil
}

func (f *fakeCinder) WaitForReady(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	status := "available" // snapshots are always immediately available in the fake
	if r.Kind == cinder.KindVolume {
		status = ""
		if v := f.vols[r.ID]; v != nil {
			status = v.status
		}
	}
	f.mu.Unlock()

	switch status {
	case "available":
		return nil
	case "":
		return fmt.Errorf("resource %s not found for readiness", r.ID)
	case "error", "error_extending":
		return fmt.Errorf("%s %s reached terminal status %q", r.Kind, r.ID, status)
	default: // "creating" never becomes available: simulate the readiness deadline
		return context.DeadlineExceeded
	}
}

func (f *fakeCinder) WaitForGone(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Kind {
	case cinder.KindVolume:
		if _, ok := f.vols[r.ID]; ok {
			return fmt.Errorf("volume %s still present", r.ID)
		}
	case cinder.KindSnapshot:
		if _, ok := f.snaps[r.ID]; ok {
			return fmt.Errorf("snapshot %s still present", r.ID)
		}
	}
	return nil
}

// addGiB adjusts the live gigabytes and tracks the high-water mark. The caller
// holds f.mu.
func (f *fakeCinder) addGiB(delta int64) {
	f.liveGiB += delta
	if f.liveGiB > f.maxGiB {
		f.maxGiB = f.liveGiB
	}
}

// churnPlan is a dependency-rich plan: three volumes (two with a resize target)
// and three snapshots, two of them on the same volume so per-volume snapshot
// serialization is exercised.
func churnPlan() *cinderplan.Plan {
	return &cinderplan.Plan{
		Scenario: "churn", Seed: 7,
		Volumes: []cinderplan.Volume{
			{Name: "vol-1", SizeGiB: 2, ResizeToGiB: 5},
			{Name: "vol-2", SizeGiB: 3},
			{Name: "vol-3", SizeGiB: 1, ResizeToGiB: 4},
		},
		Snapshots: []cinderplan.Snapshot{
			{Name: "snap-1", Volume: "vol-1"},
			{Name: "snap-2", Volume: "vol-1"},
			{Name: "snap-3", Volume: "vol-2"},
		},
	}
}

// fakeClock is a virtual clock: Sleep advances time instantly, so the scheduler
// emits a deterministic number of ticks regardless of wall-clock jitter while
// the operation tasks still run concurrently on real goroutines.
type fakeClock struct {
	cur time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{cur: time.Unix(0, 0)} }

func (c *fakeClock) Now() time.Time { return c.cur }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.cur = c.cur.Add(d)
	return nil
}

func cinderCfg() chaos.Config {
	return chaos.Config{
		Duration:    2 * time.Second,
		MinInterval: 10 * time.Millisecond,
		MaxInterval: 40 * time.Millisecond,
		MaxParallel: 4,
		ChurnRatio:  0.5,
		TargetFill:  0.7,
		ResizeRatio: 0.4,
		Concurrency: 8,
		Classify:    Classify,
	}
}

func mustBuild(t *testing.T, p *cinderplan.Plan, c Cinder) []chaos.Node {
	t.Helper()
	nodes, err := Build(p, c, "", time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return nodes
}

func containsLogical(rs []resource.Resource, logical string) bool {
	for _, r := range rs {
		if r.Logical == logical {
			return true
		}
	}
	return false
}

func failuresIn(r *chaos.Result) int {
	var n int
	for _, b := range r.Buckets {
		n += b.Stats.Failed
	}
	return n
}

// TestRunLifecycleAndEnvelope drives the full churn graph and checks the
// block-storage lifecycle invariants: no snapshot is ever created against an
// absent volume, no volume is deleted while it has live snapshots, no logical is
// doubly live, per-kind live counts stay within the plan, the per-volume family
// never runs concurrent operations, and total live gigabytes never exceed the
// quota envelope the plan sums.
func TestRunLifecycleAndEnvelope(t *testing.T) {
	f := newFakeCinder()
	f.opDelay = time.Millisecond // widen the serial window so a race would show
	p := churnPlan()

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, cinderCfg(), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Creates == 0 {
		t.Fatal("no creates were scheduled; the test exercises nothing")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapOnAbsentVol {
		t.Error("a snapshot was created against an absent source volume")
	}
	if f.volDeleteWithSnaps {
		t.Error("a volume was deleted while it still had live snapshots")
	}
	if f.doubleLiveVol || f.doubleLiveSnap {
		t.Errorf("a logical was doubly live: volume=%v snapshot=%v", f.doubleLiveVol, f.doubleLiveSnap)
	}
	if f.familyViolation {
		t.Error("two operations on one volume's family ran concurrently")
	}
	if f.maxVolCount > len(p.Volumes) {
		t.Errorf("live volumes peaked at %d, exceeding the envelope %d", f.maxVolCount, len(p.Volumes))
	}
	if f.maxSnapCount > len(p.Snapshots) {
		t.Errorf("live snapshots peaked at %d, exceeding the envelope %d", f.maxSnapCount, len(p.Snapshots))
	}
	if f.maxGiB > p.TotalGiB() {
		t.Errorf("live gigabytes peaked at %d, exceeding the quota envelope %d", f.maxGiB, p.TotalGiB())
	}
}

// TestRunExtendSemantics confirms extends happen only for volumes with a resize
// target, always to that target, and at most once per volume instance.
func TestRunExtendSemantics(t *testing.T) {
	f := newFakeCinder()
	p := churnPlan()

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, cinderCfg(), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Mutates == 0 {
		t.Fatal("no extends were scheduled; the test exercises nothing")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Only vol-1 (->5) and vol-3 (->4) have a resize target; vol-2 has none.
	if f.extendByVol["vol-2"] != 0 {
		t.Errorf("vol-2 has no resize target but was extended %d times", f.extendByVol["vol-2"])
	}
	if got := f.extendSize["vol-1"]; got != 0 && got != 5 {
		t.Errorf("vol-1 extended to %d GiB, want its planned target 5", got)
	}
	if got := f.extendSize["vol-3"]; got != 0 && got != 4 {
		t.Errorf("vol-3 extended to %d GiB, want its planned target 4", got)
	}
	// No single volume instance (cloud id) is extended more than once.
	for id, n := range f.extendByID {
		if n > 1 {
			t.Errorf("volume instance %s extended %d times, want at most once per lifetime", id, n)
		}
	}
}

// TestRunSkipsNotReadyVolume confirms a volume stuck in creating yields a failed
// create operation without wedging the run, and that no snapshot or extend
// reaches the cloud for it while it is still reclaimable.
func TestRunSkipsNotReadyVolume(t *testing.T) {
	f := newFakeCinder()
	f.stuck["vol-1"] = true // never reaches available
	p := churnPlan()

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, cinderCfg(), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if failuresIn(r) == 0 {
		t.Error("the stuck volume's create was not recorded as a failed operation")
	}
	if f.createSnapByVol["vol-1"] != 0 {
		t.Errorf("%d snapshots reached the cloud for a never-ready volume, want 0", f.createSnapByVol["vol-1"])
	}
	if f.extendByVol["vol-1"] != 0 {
		t.Errorf("%d extends reached the cloud for a never-ready volume, want 0", f.extendByVol["vol-1"])
	}
	// The volume is reclaimable: either torn down during the run or recorded in
	// the run's created list for cleanup.
	if !containsLogical(r.Created, "vol-1") && f.volDeleteByLogical["vol-1"] == 0 {
		t.Error("a never-ready volume was neither cleaned up nor recorded for cleanup")
	}
}

// TestRunTerminalErrorIsFailedOp confirms a volume whose backend status is
// terminal (error) surfaces as a failed operation rather than wedging the run.
func TestRunTerminalErrorIsFailedOp(t *testing.T) {
	f := newFakeCinder()
	f.errored["vol-2"] = true
	p := churnPlan()

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, cinderCfg(), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if failuresIn(r) == 0 {
		t.Error("a terminal error status did not surface as a failed operation")
	}
}

// TestRunDeterministicSchedule confirms the decision schedule is reproducible
// for a given seed and config, independent of the concurrent cloud completions.
func TestRunDeterministicSchedule(t *testing.T) {
	p := churnPlan()
	cfg := cinderCfg()

	r1, err := chaos.Run(context.Background(), mustBuild(t, p, newFakeCinder()), p.Seed, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	r2, err := chaos.Run(context.Background(), mustBuild(t, p, newFakeCinder()), p.Seed, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if len(r1.Decisions) == 0 {
		t.Fatal("no decisions were scheduled")
	}
	if !reflect.DeepEqual(r1.Decisions, r2.Decisions) {
		t.Error("decision schedules differ for the same seed/config")
	}
}
