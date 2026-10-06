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
// cloud. It logs the operations each server's mutations start and the
// attachment calls of each volume and port, and returns the errors queued in
// failNext. It is safe for concurrent use by the engine's operation tasks.
type fakeNova struct {
	mu     sync.Mutex
	nextID int

	live map[string]bool // resource id -> live

	attachedVol  map[string]bool // volume id -> attached to a server
	attachedPort map[string]bool // port id -> attached to a server

	// observation
	createVol    int
	attachVol    int
	detachVol    int
	attachPort   int
	detachPort   int
	stopByID     map[string]int      // server id -> stop calls (once per instance)
	resizeByID   map[string]int      // server id -> resize calls
	migrateByID  map[string]int      // server id -> live-migrate calls
	deletes      int                 // Delete calls
	opsBySrv     map[string][]string // server logical -> operations started, in call order
	flavorsBySrv map[string][]string // server logical -> flavor ids ResizeServer was called with
	attachLog    map[string][]string // volume or port logical -> attach, detach and detach-wait calls

	// failNext queues, per method name, the errors its next calls return, one
	// per call; a method with an empty queue succeeds.
	failNext map[string][]error

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
		opsBySrv:          map[string][]string{},
		flavorsBySrv:      map[string][]string{},
		attachLog:         map[string][]string{},
		failNext:          map[string][]error{},
		familyBusy:        map[string]bool{},
		liveByLogicalName: map[string]bool{},
	}
}

// id returns a fresh unique id. The caller must hold f.mu.
func (f *fakeNova) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

// injected pops the next error queued for method, nil when none is. The caller
// must hold f.mu.
func (f *fakeNova) injected(method string) error {
	errs := f.failNext[method]
	if len(errs) == 0 {
		return nil
	}
	f.failNext[method] = errs[1:]
	return errs[0]
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
	f.attachLog[volume.Logical] = append(f.attachLog[volume.Logical], "attach")
	if !f.live[server.ID] {
		f.attachAbsentSrv = true
	}
	if err := f.injected("AttachVolume"); err != nil {
		return err
	}
	f.attachedVol[volume.ID] = true
	return nil
}
func (f *fakeNova) DetachVolume(_ context.Context, server, volume resource.Resource) error {
	f.enterFamily(server.Logical)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detachVol++
	f.attachLog[volume.Logical] = append(f.attachLog[volume.Logical], "detach")
	if err := f.injected("DetachVolume"); err != nil {
		return err
	}
	f.attachedVol[volume.ID] = false
	return nil
}
func (f *fakeNova) AttachPort(_ context.Context, server, port resource.Resource) error {
	f.enterFamily(server.Logical)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachPort++
	f.attachLog[port.Logical] = append(f.attachLog[port.Logical], "attach")
	if !f.live[server.ID] {
		f.attachAbsentSrv = true
	}
	if err := f.injected("AttachPort"); err != nil {
		return err
	}
	f.attachedPort[port.ID] = true
	return nil
}
func (f *fakeNova) DetachPort(_ context.Context, server, port resource.Resource) error {
	f.enterFamily(server.Logical)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detachPort++
	f.attachLog[port.Logical] = append(f.attachLog[port.Logical], "detach")
	f.attachedPort[port.ID] = false
	return nil
}
func (f *fakeNova) WaitForPortDetached(_ context.Context, server, port resource.Resource) error {
	f.enterFamily(server.Logical)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachLog[port.Logical] = append(f.attachLog[port.Logical], "wait-detached")
	return f.injected("WaitForPortDetached")
}

func (f *fakeNova) StopServer(_ context.Context, r resource.Resource) error {
	f.enterFamily(r.Logical)
	f.mu.Lock()
	f.stopByID[r.ID]++
	f.opsBySrv[r.Logical] = append(f.opsBySrv[r.Logical], "stop-start")
	f.mu.Unlock()
	return nil
}
func (f *fakeNova) StartServer(_ context.Context, r resource.Resource) error {
	f.enterFamily(r.Logical)
	return nil
}
func (f *fakeNova) RebootServerHard(_ context.Context, r resource.Resource) error {
	f.enterFamily(r.Logical)
	f.mu.Lock()
	f.opsBySrv[r.Logical] = append(f.opsBySrv[r.Logical], "stop-start")
	f.mu.Unlock()
	return nil
}
func (f *fakeNova) ResizeServer(_ context.Context, r resource.Resource, flavorID string) error {
	f.enterFamily(r.Logical)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizeByID[r.ID]++
	f.opsBySrv[r.Logical] = append(f.opsBySrv[r.Logical], "resize")
	f.flavorsBySrv[r.Logical] = append(f.flavorsBySrv[r.Logical], flavorID)
	return f.injected("ResizeServer")
}
func (f *fakeNova) ConfirmResizeServer(_ context.Context, r resource.Resource) error {
	f.enterFamily(r.Logical)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.injected("ConfirmResizeServer")
}
func (f *fakeNova) LiveMigrateServer(_ context.Context, r resource.Resource) error {
	f.enterFamily(r.Logical)
	f.mu.Lock()
	f.migrateByID[r.ID]++
	f.opsBySrv[r.Logical] = append(f.opsBySrv[r.Logical], "live-migrate")
	f.mu.Unlock()
	return nil
}
func (f *fakeNova) ColdMigrateServer(_ context.Context, r resource.Resource) error {
	f.enterFamily(r.Logical)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opsBySrv[r.Logical] = append(f.opsBySrv[r.Logical], "cold-migrate")
	return f.injected("ColdMigrateServer")
}

func (f *fakeNova) Delete(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	delete(f.live, r.ID)
	f.liveByLogicalName[r.Logical] = false
	return nil
}
func (f *fakeNova) WaitForReady(context.Context, resource.Resource) error { return nil }
func (f *fakeNova) WaitForGone(context.Context, resource.Resource) error  { return nil }

// WaitForServerStatus and WaitForVolumeStatus return the errors queued under
// their name and the wanted status, e.g. "WaitForVolumeStatus:in-use".
func (f *fakeNova) WaitForServerStatus(_ context.Context, _ resource.Resource, want string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.injected("WaitForServerStatus:" + want)
}
func (f *fakeNova) WaitForVolumeStatus(_ context.Context, _ resource.Resource, want string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.injected("WaitForVolumeStatus:" + want)
}

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

// longLivedPlan is the plan of a long-lived persona: two servers that carry
// every operation, one with cold migration only and one with none, each of the
// first two with a detachable volume and port, and a fixed volume and port on
// the last.
func longLivedPlan() *novaplan.Plan {
	all := func(name string, networks ...string) novaplan.Server {
		return novaplan.Server{Name: name, Networks: networks, StopStart: novaplan.StopStartSoft, Resize: true, LiveMigrate: true, ColdMigrate: true}
	}
	return &novaplan.Plan{
		Scenario: "legacy", Seed: 7,
		Image: "cirros", Flavor: "m1.tiny", ResizeFlavor: "m1.small",
		Networks: []novaplan.Network{
			{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"},
			{Name: "net-0002", Subnet: "sub-0002", CIDR: "10.0.2.0/24"},
		},
		Servers: []novaplan.Server{
			all("srv-0001", "net-0001"),
			all("srv-0002", "net-0001", "net-0002"),
			{Name: "srv-0003", Networks: []string{"net-0002"}, ColdMigrate: true},
			{Name: "srv-0004", Networks: []string{"net-0002"}},
		},
		Volumes: []novaplan.Volume{
			{Name: "vol-0001", SizeGiB: 1, Server: "srv-0001", Detach: true},
			{Name: "vol-0002", SizeGiB: 2, Server: "srv-0002", Detach: true},
			{Name: "vol-0003", SizeGiB: 1, Server: "srv-0004"},
		},
		Ports: []novaplan.Port{
			{Name: "port-0001", Network: "net-0001", Server: "srv-0001", Detach: true},
			{Name: "port-0002", Network: "net-0002", Server: "srv-0002", Detach: true},
			{Name: "port-0003", Network: "net-0002", Server: "srv-0004"},
		},
	}
}

// resolvedAll resolves every reference of a plan and enables both migration
// kinds, the verdict of an admin pre-check on a cloud with two hosts.
func resolvedAll() novaexec.Resolved {
	return novaexec.Resolved{ImageID: "img-1", FlavorID: "flv-boot", ResizeFlavorID: "flv-resize", LiveMigration: true, ColdMigration: true}
}

// runLongLived runs a churn of p's long-lived graph on f in which every step
// after the creates is a mutation.
func runLongLived(t *testing.T, p *novaplan.Plan, f *fakeNova) *chaos.Result {
	t.Helper()
	nodes, err := BuildLongLived(p, f, resolvedAll(), time.Minute)
	if err != nil {
		t.Fatalf("BuildLongLived: %v", err)
	}
	cfg := novaCfg()
	cfg.ResizeRatio = 1
	r, err := chaos.Run(context.Background(), nodes, p.Seed, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return r
}

// TestRunLongLivedInvariants drives the long-lived graph and checks that it
// keeps every planned resource: nothing is deleted, each volume is created
// once, the per-server family never runs two operations at once, and no
// attach references a server that is not live, while servers, volumes and
// ports are changed in place.
func TestRunLongLivedInvariants(t *testing.T) {
	f := newFakeNova()
	f.opDelay = time.Millisecond // widen the serial window so a race would show
	p := longLivedPlan()
	r := runLongLived(t, p, f)

	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Mutates == 0 || len(f.opsBySrv["srv-0001"]) == 0 || f.detachVol == 0 || f.detachPort == 0 {
		t.Fatalf("mutates=%d srv-0001 ops=%d volume detaches=%d port detaches=%d; the run changed nothing in place",
			r.Mutates, len(f.opsBySrv["srv-0001"]), f.detachVol, f.detachPort)
	}
	if r.Deletes != 0 || f.deletes != 0 {
		t.Errorf("deletes scheduled=%d reached the cloud=%d, want 0", r.Deletes, f.deletes)
	}
	if f.createVol != len(p.Volumes) {
		t.Errorf("volumes created %d times, want once per planned volume (%d)", f.createVol, len(p.Volumes))
	}
	if want := len(p.Networks) + len(p.Servers) + len(p.Volumes) + len(p.Ports); len(r.Created) != want {
		t.Errorf("%d resources live at the end, want every planned one (%d)", len(r.Created), want)
	}
	if f.familyViolation {
		t.Error("two operations on one server's family ran concurrently")
	}
	if f.attachAbsentSrv {
		t.Error("an attach referenced a server that was not live")
	}
}

// TestRunLongLivedOperationsDeterministic confirms the operations a run gives
// each server follow from the seed alone: the same seed repeats every
// server's sequence, and another seed changes at least one.
func TestRunLongLivedOperationsDeterministic(t *testing.T) {
	opsOf := func(seed int64) map[string][]string {
		f := newFakeNova()
		p := longLivedPlan()
		p.Seed = seed
		runLongLived(t, p, f)
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.opsBySrv
	}

	first, second := opsOf(7), opsOf(7)
	if len(first["srv-0001"]) < 2 || len(first["srv-0002"]) < 2 {
		t.Fatalf("operations = %v, want several per server", first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("the same seed gave different operations:\n%v\n%v", first, second)
	}
	if other := opsOf(8); reflect.DeepEqual(first, other) {
		t.Errorf("seeds 7 and 8 gave every server the same operations: %v", first)
	}
}
