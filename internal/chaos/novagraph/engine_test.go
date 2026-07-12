package novagraph

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/nova"
	novaexec "github.com/B42Labs/dizzy/internal/nova/executor"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeNova is an in-process Nova that tracks the live population, per-server
// serialization, and attach state so the nova churn graph's lifecycle,
// attach/detach, and once-per-lifetime guarantees can be checked without a
// cloud. It is safe for concurrent use by the engine's operation tasks.
type fakeNova struct {
	mu     sync.Mutex
	nextID int

	live map[string]bool // resource id -> live

	attachedVol  map[string]bool // volume id -> attached to a server
	attachedPort map[string]bool // port id -> attached to a server

	// observation
	createVol   int
	attachVol   int
	detachVol   int
	attachPort  int
	detachPort  int
	stopByID    map[string]int // server id -> stop calls (once per instance)
	resizeByID  map[string]int // server id -> resize calls
	migrateByID map[string]int // server id -> live-migrate calls

	// invariants
	doubleLive        bool // a resource created while a live instance of its logical exists
	attachAbsentSrv   bool // an attach referenced a server that is not live
	familyBusy        map[string]bool
	familyViolation   bool // two ops on one server's family overlapped
	opDelay           time.Duration
	liveByLogicalName map[string]bool // logical -> a live instance exists
}

func newFakeNova() *fakeNova {
	return &fakeNova{
		live:              map[string]bool{},
		attachedVol:       map[string]bool{},
		attachedPort:      map[string]bool{},
		stopByID:          map[string]int{},
		resizeByID:        map[string]int{},
		migrateByID:       map[string]int{},
		familyBusy:        map[string]bool{},
		liveByLogicalName: map[string]bool{},
	}
}

// id returns a fresh unique id. The caller must hold f.mu.
func (f *fakeNova) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

// enterFamily opens a per-server serial window, flagging an overlap if another
// family op is already inside one for the same server. The graph's per-server
// gate serializes the family, so a correct build never overlaps.
func (f *fakeNova) enterFamily(server string) {
	f.mu.Lock()
	if f.familyBusy[server] {
		f.familyViolation = true
	}
	f.familyBusy[server] = true
	f.mu.Unlock()
	if f.opDelay > 0 {
		time.Sleep(f.opDelay)
	}
	f.mu.Lock()
	f.familyBusy[server] = false
	f.mu.Unlock()
}

func (f *fakeNova) create(kind resource.Kind, logical, prefix string) resource.Resource {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.liveByLogicalName[logical] {
		f.doubleLive = true
	}
	id := f.id(prefix)
	f.live[id] = true
	f.liveByLogicalName[logical] = true
	return resource.Resource{Kind: kind, Logical: logical, ID: id}
}

func (f *fakeNova) CreateNetwork(_ context.Context, n novaplan.Network) (resource.Resource, error) {
	return f.create(nova.KindNetwork, n.Name, "net"), nil
}
func (f *fakeNova) CreateSubnet(_ context.Context, n novaplan.Network, _ string) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return resource.Resource{Kind: nova.KindSubnet, Logical: n.Subnet, ID: f.id("sub")}, nil
}
func (f *fakeNova) DeleteNetworkPorts(context.Context, string) (int, error) { return 0, nil }

func (f *fakeNova) CreateServer(_ context.Context, s novaplan.Server, _ nova.BootSpec) (resource.Resource, error) {
	return f.create(nova.KindServer, s.Name, "srv"), nil
}
func (f *fakeNova) CreateVolume(_ context.Context, v novaplan.Volume) (resource.Resource, error) {
	f.mu.Lock()
	f.createVol++
	f.mu.Unlock()
	return f.create(nova.KindVolume, v.Name, "vol"), nil
}
func (f *fakeNova) CreatePort(_ context.Context, pt novaplan.Port, _ string) (resource.Resource, error) {
	return f.create(nova.KindPort, pt.Name, "port"), nil
}

func (f *fakeNova) AttachVolume(_ context.Context, server, volume resource.Resource) error {
	f.enterFamily(server.Logical)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachVol++
	if !f.live[server.ID] {
		f.attachAbsentSrv = true
	}
	f.attachedVol[volume.ID] = true
	return nil
}
func (f *fakeNova) DetachVolume(_ context.Context, server, volume resource.Resource) error {
	f.enterFamily(server.Logical)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detachVol++
	f.attachedVol[volume.ID] = false
	return nil
}
func (f *fakeNova) AttachPort(_ context.Context, server, port resource.Resource) error {
	f.enterFamily(server.Logical)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachPort++
	if !f.live[server.ID] {
		f.attachAbsentSrv = true
	}
	f.attachedPort[port.ID] = true
	return nil
}
func (f *fakeNova) DetachPort(_ context.Context, server, port resource.Resource) error {
	f.enterFamily(server.Logical)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detachPort++
	f.attachedPort[port.ID] = false
	return nil
}

func (f *fakeNova) StopServer(_ context.Context, r resource.Resource) error {
	f.enterFamily(r.Logical)
	f.mu.Lock()
	f.stopByID[r.ID]++
	f.mu.Unlock()
	return nil
}
func (f *fakeNova) StartServer(_ context.Context, r resource.Resource) error {
	f.enterFamily(r.Logical)
	return nil
}
func (f *fakeNova) RebootServerHard(_ context.Context, r resource.Resource) error {
	f.enterFamily(r.Logical)
	return nil
}
func (f *fakeNova) ResizeServer(_ context.Context, r resource.Resource, _ string) error {
	f.enterFamily(r.Logical)
	f.mu.Lock()
	f.resizeByID[r.ID]++
	f.mu.Unlock()
	return nil
}
func (f *fakeNova) ConfirmResizeServer(_ context.Context, r resource.Resource) error {
	f.enterFamily(r.Logical)
	return nil
}
func (f *fakeNova) LiveMigrateServer(_ context.Context, r resource.Resource) error {
	f.enterFamily(r.Logical)
	f.mu.Lock()
	f.migrateByID[r.ID]++
	f.mu.Unlock()
	return nil
}

func (f *fakeNova) Delete(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, r.ID)
	f.liveByLogicalName[r.Logical] = false
	return nil
}
func (f *fakeNova) WaitForReady(context.Context, resource.Resource) error                { return nil }
func (f *fakeNova) WaitForServerStatus(context.Context, resource.Resource, string) error { return nil }
func (f *fakeNova) WaitForVolumeStatus(context.Context, resource.Resource, string) error { return nil }
func (f *fakeNova) WaitForGone(context.Context, resource.Resource) error                 { return nil }

// churnPlan is a dependency-rich plan: two networks, three servers with a mix of
// lifecycle operations, and volumes and ports across them.
func churnPlan() *novaplan.Plan {
	return &novaplan.Plan{
		Scenario: "churn", Seed: 7,
		Image: "cirros", Flavor: "m1.tiny", ResizeFlavor: "m1.small",
		Networks: []novaplan.Network{
			{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"},
			{Name: "net-0002", Subnet: "sub-0002", CIDR: "10.0.2.0/24"},
		},
		Servers: []novaplan.Server{
			{Name: "srv-0001", Networks: []string{"net-0001"}, StopStart: novaplan.StopStartSoft},
			{Name: "srv-0002", Networks: []string{"net-0001", "net-0002"}, Resize: true, LiveMigrate: true},
			{Name: "srv-0003", Networks: []string{"net-0002"}},
		},
		Volumes: []novaplan.Volume{
			{Name: "vol-0001", SizeGiB: 1, Server: "srv-0001", Detach: true},
			{Name: "vol-0002", SizeGiB: 2, Server: "srv-0002"},
		},
		Ports: []novaplan.Port{
			{Name: "port-0001", Network: "net-0001", Server: "srv-0001"},
			{Name: "port-0002", Network: "net-0002", Server: "srv-0002", Detach: true},
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

func novaCfg() chaos.Config {
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

func mustBuild(t *testing.T, p *novaplan.Plan, c Nova) []chaos.Node {
	t.Helper()
	nodes, err := Build(p, c, novaexec.Resolved{LiveMigration: true}, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return nodes
}

// TestRunLifecycleInvariants drives the full churn graph and checks the compute
// lifecycle invariants: every created volume and port was attached to its
// server, no attach referenced an absent server, the per-server family never ran
// concurrent operations, no logical was doubly live, and each server instance
// was stop/started, resized, and migrated at most once per lifetime.
func TestRunLifecycleInvariants(t *testing.T) {
	f := newFakeNova()
	f.opDelay = time.Millisecond // widen the serial window so a race would show
	p := churnPlan()

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, novaCfg(), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Creates == 0 {
		t.Fatal("no creates were scheduled; the test exercises nothing")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createVol > 0 && f.attachVol == 0 {
		t.Error("volumes were created but never attached")
	}
	if f.attachVol != f.createVol {
		t.Errorf("attachVol=%d != createVol=%d: every created volume must be attached on create", f.attachVol, f.createVol)
	}
	if f.attachAbsentSrv {
		t.Error("an attach referenced a server that was not live")
	}
	if f.familyViolation {
		t.Error("two operations on one server's family ran concurrently")
	}
	if f.doubleLive {
		t.Error("a logical was doubly live")
	}
	for id, n := range f.stopByID {
		if n > 1 {
			t.Errorf("server instance %s stopped %d times, want at most once per lifetime", id, n)
		}
	}
	for id, n := range f.resizeByID {
		if n > 1 {
			t.Errorf("server instance %s resized %d times, want at most once per lifetime", id, n)
		}
	}
	for id, n := range f.migrateByID {
		if n > 1 {
			t.Errorf("server instance %s live-migrated %d times, want at most once per lifetime", id, n)
		}
	}
}

// TestRunDeterministicSchedule confirms the decision schedule is reproducible
// for a given seed and config, independent of the concurrent cloud completions.
func TestRunDeterministicSchedule(t *testing.T) {
	p := churnPlan()
	cfg := novaCfg()

	r1, err := chaos.Run(context.Background(), mustBuild(t, p, newFakeNova()), p.Seed, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	r2, err := chaos.Run(context.Background(), mustBuild(t, p, newFakeNova()), p.Seed, cfg, newFakeClock())
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
