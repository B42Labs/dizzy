package glancegraph

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/glance"
	glanceplan "github.com/B42Labs/dizzy/internal/glance/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeGlance is an in-process Glance that tracks the live population, upload
// coverage, and per-instance mutation counts so the glance churn graph's
// lifecycle and once-per-lifetime guarantees can be checked without a cloud. It
// is safe for concurrent use by the engine's operation tasks.
type fakeGlance struct {
	mu     sync.Mutex
	nextID int

	live              map[string]bool // id -> live
	liveByLogicalName map[string]bool // logical -> a live instance exists

	uploaded    map[string]bool // id -> its payload was uploaded before it went live
	metaAddByID map[string]int  // image id -> metadata-add calls (once per instance)
	deactByID   map[string]int  // image id -> deactivate calls (once per instance)

	doubleLive   bool // a create ran while a live instance of its logical existed
	uploadAbsent bool // an upload referenced an image that was not live-in-progress
}

func newFakeGlance() *fakeGlance {
	return &fakeGlance{
		live:              map[string]bool{},
		liveByLogicalName: map[string]bool{},
		uploaded:          map[string]bool{},
		metaAddByID:       map[string]int{},
		deactByID:         map[string]int{},
	}
}

func (f *fakeGlance) CreateImage(_ context.Context, img glanceplan.Image) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.liveByLogicalName[img.Name] {
		f.doubleLive = true
	}
	f.nextID++
	id := fmt.Sprintf("img-%d", f.nextID)
	f.live[id] = true
	f.liveByLogicalName[img.Name] = true
	return resource.Resource{Kind: glance.KindImage, Logical: img.Name, Name: "dizzy-run0-" + img.Name, ID: id}, nil
}

func (f *fakeGlance) UploadImageData(_ context.Context, r resource.Resource, _ int, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.live[r.ID] {
		f.uploadAbsent = true
	}
	f.uploaded[r.ID] = true
	return nil
}

func (f *fakeGlance) ImageOwner(context.Context, resource.Resource) (string, error) {
	return "project-owner", nil
}
func (f *fakeGlance) AddImageProperties(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	f.metaAddByID[r.ID]++
	f.mu.Unlock()
	return nil
}
func (f *fakeGlance) ChurnImageProperties(context.Context, resource.Resource) error { return nil }
func (f *fakeGlance) SetImageVisibility(context.Context, resource.Resource, images.ImageVisibility) error {
	return nil
}
func (f *fakeGlance) AddImageMember(context.Context, resource.Resource, string) error    { return nil }
func (f *fakeGlance) AcceptImageMember(context.Context, resource.Resource, string) error { return nil }
func (f *fakeGlance) RemoveImageMember(context.Context, resource.Resource, string) error { return nil }
func (f *fakeGlance) DeactivateImage(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	f.deactByID[r.ID]++
	f.mu.Unlock()
	return nil
}
func (f *fakeGlance) ReactivateImage(context.Context, resource.Resource) error { return nil }
func (f *fakeGlance) Delete(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, r.ID)
	f.liveByLogicalName[r.Logical] = false
	return nil
}
func (f *fakeGlance) WaitForReady(context.Context, resource.Resource) error { return nil }
func (f *fakeGlance) WaitForGone(context.Context, resource.Resource) error  { return nil }

// churnPlan is a plan with a mix of lifecycle operations across several images.
func churnPlan() *glanceplan.Plan {
	return &glanceplan.Plan{
		Scenario: "churn", Seed: 7,
		Images: []glanceplan.Image{
			{Name: "img-0001", SizeMiB: 1, MetadataUpdate: true, Deactivate: true},
			{Name: "img-0002", SizeMiB: 2, Shared: true, MemberAccept: true, MemberRemove: true},
			{Name: "img-0003", SizeMiB: 4, Community: true},
			{Name: "img-0004", SizeMiB: 8}, // no lifecycle op: immutable
		},
	}
}

// fakeClock is a virtual clock: Sleep advances time instantly, so the scheduler
// emits a deterministic number of ticks while the operation tasks still run
// concurrently on real goroutines.
type fakeClock struct{ cur time.Time }

func newFakeClock() *fakeClock      { return &fakeClock{cur: time.Unix(0, 0)} }
func (c *fakeClock) Now() time.Time { return c.cur }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.cur = c.cur.Add(d)
	return nil
}

func glanceCfg() chaos.Config {
	return chaos.Config{
		Duration:    2 * time.Second,
		MinInterval: 10 * time.Millisecond,
		MaxInterval: 40 * time.Millisecond,
		MaxParallel: 4,
		ChurnRatio:  0.5,
		TargetFill:  0.7,
		ResizeRatio: 0.4, // the engine's mutate probability
		Concurrency: 8,
		Classify:    Classify,
	}
}

func mustBuild(t *testing.T, p *glanceplan.Plan, c Glance) []chaos.Node {
	t.Helper()
	nodes, err := Build(p, c, p.Seed, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return nodes
}

// TestRunLifecycleInvariants drives the full churn graph and checks the image
// lifecycle invariants: every live image had its payload uploaded, no upload
// referenced an absent image, no logical was doubly live, and each image instance
// was metadata-churned and deactivated at most once per lifetime.
func TestRunLifecycleInvariants(t *testing.T) {
	f := newFakeGlance()
	p := churnPlan()

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, glanceCfg(), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Creates == 0 {
		t.Fatal("no creates were scheduled; the test exercises nothing")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploadAbsent {
		t.Error("an upload referenced an image that was not live")
	}
	if f.doubleLive {
		t.Error("a logical was doubly live")
	}
	for id := range f.live {
		if !f.uploaded[id] {
			t.Errorf("image %s is live but was never uploaded", id)
		}
	}
	for id, n := range f.metaAddByID {
		if n > 1 {
			t.Errorf("image instance %s metadata-churned %d times, want at most once per lifetime", id, n)
		}
	}
	for id, n := range f.deactByID {
		if n > 1 {
			t.Errorf("image instance %s deactivated %d times, want at most once per lifetime", id, n)
		}
	}
}

// TestRunDeterministicSchedule confirms the decision schedule is reproducible for
// a given seed and config, independent of the concurrent cloud completions.
func TestRunDeterministicSchedule(t *testing.T) {
	p := churnPlan()
	cfg := glanceCfg()

	r1, err := chaos.Run(context.Background(), mustBuild(t, p, newFakeGlance()), p.Seed, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	r2, err := chaos.Run(context.Background(), mustBuild(t, p, newFakeGlance()), p.Seed, cfg, newFakeClock())
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
