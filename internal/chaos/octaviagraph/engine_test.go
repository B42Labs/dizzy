package octaviagraph

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/octavia"
	"github.com/B42Labs/dizzy/internal/octavia/plan"
	octaviascenario "github.com/B42Labs/dizzy/internal/octavia/scenario"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/scenarios"
)

// update, when set via `go test -update`, rewrites the golden decision-schedule
// file instead of comparing against it.
var update = flag.Bool("update", false, "rewrite golden files instead of comparing")

// object is one cloud object the fake holds: its kind, the load balancer whose
// family it belongs to (empty for a network or subnet), the ids of the objects
// that must stay live while it is, and whether it is live.
type object struct {
	kind    resource.Kind
	logical string
	lb      string
	parents []string
	live    bool
}

// fakeOctavia is an in-process Octavia that tracks the live objects and checks
// the invariants the churn graph must never violate: no object is created on an
// absent parent, none is deleted while a live object depends on it, every
// toggle and delete targets a live object, and no load balancer ever has two
// calls or waits in flight at once. It is safe for concurrent use by the
// engine's operation tasks.
type fakeOctavia struct {
	mu       sync.Mutex
	nextID   int
	objects  map[string]*object
	provider string

	delay        time.Duration // real time every call holds its load balancer
	failListener string        // logical name of a listener whose waits report ERROR

	inFlight     map[string]int    // load balancer id -> calls in flight on it
	maxBusyLBs   int               // most load balancers with a call in flight at once
	lastListener map[string]string // load balancer id -> logical of its last created listener
	toggles      int
	cur, max     map[resource.Kind]int
	violations   []string
}

func newFakeOctavia() *fakeOctavia {
	return &fakeOctavia{
		objects:      make(map[string]*object),
		provider:     "amphora",
		inFlight:     make(map[string]int),
		lastListener: make(map[string]string),
		cur:          make(map[resource.Kind]int),
		max:          make(map[resource.Kind]int),
	}
}

// violate records an invariant violation. The caller holds mu.
func (f *fakeOctavia) violate(format string, args ...any) {
	f.violations = append(f.violations, fmt.Sprintf(format, args...))
}

// enter marks a call on the load balancer lb as in flight, flagging a second
// concurrent one, and holds it for the fake's delay. exit undoes it.
func (f *fakeOctavia) enter(lb string) {
	f.mu.Lock()
	f.inFlight[lb]++
	if f.inFlight[lb] > 1 {
		f.violate("two calls in flight on load balancer %s", lb)
	}
	busy := 0
	for _, n := range f.inFlight {
		if n > 0 {
			busy++
		}
	}
	if busy > f.maxBusyLBs {
		f.maxBusyLBs = busy
	}
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
}

func (f *fakeOctavia) exit(lb string) {
	f.mu.Lock()
	f.inFlight[lb]--
	f.mu.Unlock()
}

// add creates a live object of kind on parents, which must all be live. The
// caller holds mu.
func (f *fakeOctavia) add(kind resource.Kind, logical, lb string, parents ...string) resource.Resource {
	for _, p := range parents {
		if o := f.objects[p]; o == nil || !o.live {
			f.violate("%s %s created on absent parent %s", kind, logical, p)
		}
	}
	f.nextID++
	id := fmt.Sprintf("%s-%d", kind, f.nextID)
	if kind == octavia.KindLoadBalancer {
		lb = id
	}
	f.objects[id] = &object{kind: kind, logical: logical, lb: lb, parents: parents, live: true}
	f.cur[kind]++
	if f.cur[kind] > f.max[kind] {
		f.max[kind] = f.cur[kind]
	}
	return resource.Resource{Kind: kind, Logical: logical, Name: "dizzy-run0-" + logical, ID: id}
}

// remove deletes the live object id, flagging a live dependent. The caller
// holds mu.
func (f *fakeOctavia) remove(id string) {
	o := f.objects[id]
	if o == nil || !o.live {
		f.violate("delete of absent object %s", id)
		return
	}
	for childID, child := range f.objects {
		if child.live && contains(child.parents, id) {
			f.violate("%s %s deleted while %s %s (%s) depends on it", o.kind, o.logical, child.kind, child.logical, childID)
		}
	}
	o.live = false
	f.cur[o.kind]--
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// lbOf returns the load balancer of the object id.
func (f *fakeOctavia) lbOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o := f.objects[id]; o != nil {
		return o.lb
	}
	return ""
}

func (f *fakeOctavia) CreateNetwork(_ context.Context, n plan.Network) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.add(octavia.KindNetwork, n.Name, ""), nil
}

func (f *fakeOctavia) CreateSubnet(_ context.Context, n plan.Network, networkID string) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.add(octavia.KindSubnet, n.Subnet, "", networkID), nil
}

func (f *fakeOctavia) WaitForReady(context.Context, resource.Resource) error { return nil }

func (f *fakeOctavia) CreateLoadBalancer(_ context.Context, lb plan.LoadBalancer, subnetID, _ string) (resource.Resource, string, error) {
	f.mu.Lock()
	res := f.add(octavia.KindLoadBalancer, lb.Name, "", subnetID)
	f.mu.Unlock()
	f.enter(res.ID)
	defer f.exit(res.ID)
	return res, f.provider, nil
}

func (f *fakeOctavia) CreateListener(_ context.Context, l plan.Listener, lbID string) (resource.Resource, error) {
	f.enter(lbID)
	defer f.exit(lbID)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastListener[lbID] = l.Name
	return f.add(octavia.KindListener, l.Name, lbID, lbID), nil
}

func (f *fakeOctavia) CreatePool(_ context.Context, p plan.Pool, lbID, listenerID, algorithm string) (resource.Resource, error) {
	f.enter(lbID)
	defer f.exit(lbID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if algorithm == "" {
		f.violate("pool %s created without an algorithm", p.Name)
	}
	return f.add(octavia.KindPool, p.Name, lbID, listenerID, lbID), nil
}

func (f *fakeOctavia) CreateMember(_ context.Context, m plan.Member, poolID, subnetID string) (resource.Resource, error) {
	lb := f.lbOf(poolID)
	f.enter(lb)
	defer f.exit(lb)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.add(octavia.KindMember, m.Name, lb, poolID, subnetID), nil
}

func (f *fakeOctavia) CreateHealthMonitor(_ context.Context, h plan.HealthMonitor, poolID string) (resource.Resource, error) {
	lb := f.lbOf(poolID)
	f.enter(lb)
	defer f.exit(lb)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.add(octavia.KindHealthMonitor, h.Name, lb, poolID), nil
}

func (f *fakeOctavia) WaitForActive(_ context.Context, lb resource.Resource, kind resource.Kind) error {
	f.enter(lb.ID)
	defer f.exit(lb.ID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if kind == octavia.KindListener && f.failListener != "" && f.lastListener[lb.ID] == f.failListener {
		return fmt.Errorf("%w: load balancer %s reached ERROR after a listener write", octavia.ErrProvisioning, lb.ID)
	}
	return nil
}

func (f *fakeOctavia) WaitForActiveAfterDelete(_ context.Context, lb resource.Resource, _ resource.Kind) error {
	f.enter(lb.ID)
	defer f.exit(lb.ID)
	return nil
}

func (f *fakeOctavia) WaitForGone(_ context.Context, lb resource.Resource) error {
	f.enter(lb.ID)
	defer f.exit(lb.ID)
	return nil
}

func (f *fakeOctavia) Delete(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Kind != octavia.KindNetwork && r.Kind != octavia.KindSubnet {
		f.violate("Delete of %s %s; only networks and subnets go through Delete", r.Kind, r.Logical)
	}
	f.remove(r.ID)
	return nil
}

func (f *fakeOctavia) DeleteSingle(_ context.Context, r resource.Resource) error {
	lb := f.lbOf(r.ID)
	f.enter(lb)
	defer f.exit(lb)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remove(r.ID)
	return nil
}

func (f *fakeOctavia) DeleteMember(_ context.Context, r resource.Resource, poolID string) error {
	lb := f.lbOf(r.ID)
	f.enter(lb)
	defer f.exit(lb)
	f.mu.Lock()
	defer f.mu.Unlock()
	if o := f.objects[r.ID]; o != nil && !contains(o.parents, poolID) {
		f.violate("member %s deleted through pool %s, not its own", r.Logical, poolID)
	}
	f.remove(r.ID)
	return nil
}

func (f *fakeOctavia) toggle(r resource.Resource) {
	lb := f.lbOf(r.ID)
	f.enter(lb)
	defer f.exit(lb)
	f.mu.Lock()
	defer f.mu.Unlock()
	if o := f.objects[r.ID]; o == nil || !o.live {
		f.violate("toggle of absent %s %s", r.Kind, r.Logical)
	}
	f.toggles++
}

func (f *fakeOctavia) SetListenerAdminState(_ context.Context, r resource.Resource, _ bool) error {
	f.toggle(r)
	return nil
}

func (f *fakeOctavia) SetMemberAdminState(_ context.Context, r resource.Resource, _ string, _ bool) error {
	f.toggle(r)
	return nil
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

// profileConfig returns the churn config the shipped small profile's chaos
// block sets, with the command's default concurrency and Classify wired as the
// command wires it.
func profileConfig(t *testing.T) chaos.Config {
	t.Helper()
	data, err := scenarios.Files.ReadFile("octavia/small.yaml")
	if err != nil {
		t.Fatalf("reading the small profile: %v", err)
	}
	s, err := octaviascenario.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c := s.Chaos
	if c == nil || c.UpdateRatio == nil {
		t.Fatal("the small profile has no chaos block with an update_ratio")
	}
	return chaos.Config{
		Duration:    time.Duration(c.Duration),
		MinInterval: time.Duration(c.Interval.Min),
		MaxInterval: time.Duration(c.Interval.Max),
		MaxParallel: c.Parallel.Max,
		ChurnRatio:  c.ChurnRatio,
		TargetFill:  c.TargetFill,
		ResizeRatio: *c.UpdateRatio,
		Concurrency: 8,
		Classify:    Classify,
	}
}

func run(t *testing.T, p *plan.Plan, f *fakeOctavia, cfg chaos.Config) *chaos.Result {
	t.Helper()
	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return r
}

// checkInvariants fails the test on any recorded violation and on a live
// population above the plan's counts.
func checkInvariants(t *testing.T, p *plan.Plan, f *fakeOctavia) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.violations {
		t.Error(v)
	}
	limits := map[resource.Kind]int{
		octavia.KindNetwork: len(p.Networks), octavia.KindSubnet: len(p.Networks),
		octavia.KindLoadBalancer: len(p.LoadBalancers), octavia.KindListener: len(p.Listeners),
		octavia.KindPool: len(p.Pools), octavia.KindMember: len(p.Members),
		octavia.KindHealthMonitor: len(p.HealthMonitors),
	}
	for kind, limit := range limits {
		if f.max[kind] > limit {
			t.Errorf("%d live %s objects at once, more than the plan's %d", f.max[kind], kind, limit)
		}
	}
}

// TestRunLifecycleInvariants drives the churn graph of the small profile under
// its own chaos config and checks that no object was created on an absent
// parent or deleted under a live dependent, that no load balancer ever had two
// calls or waits in flight at once, and that the live objects stayed within
// the plan's envelope.
func TestRunLifecycleInvariants(t *testing.T) {
	p := smallPlan(t)
	f := newFakeOctavia()
	f.delay = 100 * time.Microsecond
	r := run(t, p, f, profileConfig(t))

	checkInvariants(t, p, f)
	if r.Creates == 0 || r.Deletes == 0 || r.Mutates == 0 {
		t.Errorf("creates/deletes/mutates = %d/%d/%d, want all positive", r.Creates, r.Deletes, r.Mutates)
	}
	if f.toggles == 0 {
		t.Error("no admin-state toggle reached the fake")
	}
}

// TestRunLoadBalancersOverlap confirms the gate is per load balancer, not
// global: with concurrency 4 and every call holding its load balancer for 2ms,
// calls on the two load balancers run at the same time.
func TestRunLoadBalancersOverlap(t *testing.T) {
	p := smallPlan(t)
	f := newFakeOctavia()
	f.delay = 2 * time.Millisecond
	cfg := profileConfig(t)
	cfg.Duration = 30 * time.Second
	cfg.Concurrency = 4
	run(t, p, f, cfg)

	checkInvariants(t, p, f)
	if f.maxBusyLBs < 2 {
		t.Errorf("at most %d load balancer(s) had a call in flight at once, want 2", f.maxBusyLBs)
	}
}

// formatDecisions renders a decision schedule as one tab-separated line per
// decision (offset, action, kind, key), no-ops included, for the golden file.
func formatDecisions(ds []chaos.Decision) string {
	var b strings.Builder
	for _, d := range ds {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", d.Offset, d.Action, d.Kind, d.Key)
	}
	return b.String()
}

func TestRunDeterministicSchedule(t *testing.T) {
	p := smallPlan(t)
	cfg := profileConfig(t)
	r1 := run(t, p, newFakeOctavia(), cfg)
	r2 := run(t, p, newFakeOctavia(), cfg)
	if len(r1.Decisions) == 0 {
		t.Fatal("no decisions were scheduled")
	}
	if !reflect.DeepEqual(r1.Decisions, r2.Decisions) {
		t.Errorf("decision schedules differ for the same seed and config: %d vs %d decisions", len(r1.Decisions), len(r2.Decisions))
	}
}

// TestGoldenDecisionSchedule locks the exact decision schedule the engine draws
// for the small profile under its own chaos config and a virtual clock. It also
// confirms the schedule holds an admin-state toggle, and that members, the most
// numerous leaves, are drawn more often than any other kind.
func TestGoldenDecisionSchedule(t *testing.T) {
	p := smallPlan(t)
	r := run(t, p, newFakeOctavia(), profileConfig(t))
	got := formatDecisions(r.Decisions)

	golden := filepath.Join("testdata", "golden", "decisions.txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatalf("creating golden dir: %v", err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading golden (run with -update to create it): %v", err)
	}
	if got != string(want) {
		t.Errorf("decision schedule differs from golden %s; rerun with -update only if the change is intended", golden)
	}

	perKind := map[resource.Kind]int{}
	mutates := 0
	for _, d := range r.Decisions {
		if d.Action == "noop" {
			continue
		}
		perKind[d.Kind]++
		if d.Action == "mutate" {
			mutates++
		}
	}
	if mutates == 0 {
		t.Error("the schedule holds no mutate decision; the admin-state toggle is not covered")
	}
	for kind, n := range perKind {
		if kind != octavia.KindMember && n >= perKind[octavia.KindMember] {
			t.Errorf("%d %s decisions, not fewer than the %d member decisions", n, kind, perKind[octavia.KindMember])
		}
	}
}

func TestRunUpdateRatioZeroNeverToggles(t *testing.T) {
	p := smallPlan(t)
	f := newFakeOctavia()
	cfg := profileConfig(t)
	cfg.ResizeRatio = 0
	r := run(t, p, f, cfg)

	for _, d := range r.Decisions {
		if d.Action == "mutate" {
			t.Fatalf("update ratio 0 scheduled a mutate decision: %+v", d)
		}
	}
	if f.toggles != 0 {
		t.Errorf("update ratio 0 sent %d admin-state calls, want none", f.toggles)
	}
}

// TestRunProvisioningErrorIsAFailedOutcome confirms a load balancer that
// reaches ERROR after one listener's writes records failed outcomes labeled
// provisioning_error without stopping the run.
func TestRunProvisioningErrorIsAFailedOutcome(t *testing.T) {
	p := smallPlan(t)
	f := newFakeOctavia()
	f.failListener = "lsn-0001"
	r := run(t, p, f, profileConfig(t))

	if len(r.Decisions) == 0 {
		t.Fatal("no decisions were scheduled")
	}
	failed := 0
	for _, b := range r.Buckets {
		for _, e := range b.Errors {
			if e.Kind == "provisioning_error" {
				failed += e.Count
			}
		}
	}
	if failed == 0 {
		t.Error("no failed outcome labeled provisioning_error")
	}
	checkInvariants(t, p, f)
}
