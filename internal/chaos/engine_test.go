package chaos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/B42Labs/openstack-tester/internal/neutron"
	"github.com/B42Labs/openstack-tester/internal/plan"
)

// fakeNeutron is an in-process Neutron that tracks the live cloud population and
// the parent/child relationships of every created resource, so the engine's
// ordering (no create before its parents, no delete before its dependents),
// envelope ceiling, and concurrency bound can be checked without a cloud.
type fakeNeutron struct {
	mu     sync.Mutex
	nextID int

	live     map[string]bool         // cloud ids currently existing
	deps     map[string][]string     // id -> parent cloud ids
	kind     map[string]neutron.Kind // id -> kind
	iface    map[string]ifaceRec     // router-interface id -> attachment
	logicals []string                // logical names of resources created

	badRefs    []string // create referencing a non-live parent
	badDeletes []string // delete of a resource with a live dependent

	inFlight    int
	maxInFlight int
	liveByKind  map[neutron.Kind]int
	maxByKind   map[neutron.Kind]int

	createCalls  map[neutron.Kind]int   // create attempts that reached the fake, per kind
	emptyDeletes int                    // Delete calls made with an empty resource id
	failKinds    map[neutron.Kind]error // creates of these kinds fail with the given error
	panicKinds   map[neutron.Kind]bool  // creates of these kinds panic (a misbehaving cloud)

	workDelay       time.Duration
	holdUntilCancel bool
	started         chan struct{}
	startedOnce     sync.Once
}

type ifaceRec struct{ router, subnet, port string }

func newFake() *fakeNeutron {
	return &fakeNeutron{
		live:        make(map[string]bool),
		deps:        make(map[string][]string),
		kind:        make(map[string]neutron.Kind),
		iface:       make(map[string]ifaceRec),
		liveByKind:  make(map[neutron.Kind]int),
		maxByKind:   make(map[neutron.Kind]int),
		createCalls: make(map[neutron.Kind]int),
	}
}

// enter marks the start of an operation: it bumps the in-flight gauge, runs the
// reference checks under refCheck, then (outside the lock) blocks or delays to
// expose concurrency. It returns false if the context is cancelled while held.
func (f *fakeNeutron) enter(ctx context.Context, refCheck func()) bool {
	if f.started != nil {
		f.startedOnce.Do(func() { close(f.started) })
	}
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	refCheck()
	f.mu.Unlock()

	if f.holdUntilCancel {
		<-ctx.Done()
		f.leave()
		return false
	}
	if f.workDelay > 0 {
		time.Sleep(f.workDelay)
	}
	return true
}

func (f *fakeNeutron) leave() {
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
}

// create records a new resource of kind with the given parent ids, flagging any
// parent that is not currently live as an ordering violation.
func (f *fakeNeutron) create(ctx context.Context, kind neutron.Kind, logical string, parents ...string) (neutron.Resource, error) {
	f.mu.Lock()
	f.createCalls[kind]++
	f.mu.Unlock()

	if !f.enter(ctx, func() {
		for _, p := range parents {
			if p != "" && !f.live[p] {
				f.badRefs = append(f.badRefs, fmt.Sprintf("%s %s: parent %q not live", kind, logical, p))
			}
		}
	}) {
		return neutron.Resource{}, ctx.Err()
	}
	defer f.leave()

	if f.panicKinds[kind] {
		panic("simulated cloud panic creating " + string(kind))
	}
	if err := f.failKinds[kind]; err != nil {
		return neutron.Resource{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("id-%d", f.nextID)
	f.live[id] = true
	f.kind[id] = kind
	f.deps[id] = filterNonEmpty(parents)
	f.logicals = append(f.logicals, logical)
	f.bumpKind(kind)
	return neutron.Resource{Kind: kind, Logical: logical, ID: id}, nil
}

// bumpKind increments the live count for kind and tracks its high-water mark.
// The caller holds f.mu.
func (f *fakeNeutron) bumpKind(kind neutron.Kind) {
	f.liveByKind[kind]++
	if f.liveByKind[kind] > f.maxByKind[kind] {
		f.maxByKind[kind] = f.liveByKind[kind]
	}
}

func (f *fakeNeutron) CreateAddressScope(ctx context.Context, as plan.AddressScope) (neutron.Resource, error) {
	return f.create(ctx, neutron.KindAddressScope, as.Name)
}

func (f *fakeNeutron) CreateSubnetPool(ctx context.Context, sp plan.SubnetPool, addressScopeID string) (neutron.Resource, error) {
	return f.create(ctx, neutron.KindSubnetPool, sp.Name, addressScopeID)
}

func (f *fakeNeutron) CreateNetwork(ctx context.Context, n plan.Network) (neutron.Resource, error) {
	return f.create(ctx, neutron.KindNetwork, n.Name)
}

func (f *fakeNeutron) CreateSubnet(ctx context.Context, s plan.Subnet, networkID, subnetPoolID string) (neutron.Resource, error) {
	return f.create(ctx, neutron.KindSubnet, s.Name, networkID, subnetPoolID)
}

func (f *fakeNeutron) CreateRouter(ctx context.Context, r plan.Router, externalNetworkID string) (neutron.Resource, error) {
	return f.create(ctx, neutron.KindRouter, r.Name)
}

func (f *fakeNeutron) CreateSecurityGroup(ctx context.Context, sg plan.SecurityGroup) (neutron.Resource, error) {
	return f.create(ctx, neutron.KindSecurityGroup, sg.Name)
}

func (f *fakeNeutron) CreateSecurityGroupRule(ctx context.Context, rule plan.SecurityGroupRule, sgID, remoteGroupID string) (neutron.Resource, error) {
	return f.create(ctx, neutron.KindSecurityGroupRule, "rule@"+sgID, sgID, remoteGroupID)
}

func (f *fakeNeutron) CreatePort(ctx context.Context, p plan.Port, networkID string, subnetIDByLogical map[string]string, sgIDs []string) (neutron.Resource, error) {
	refs := []string{networkID}
	refs = append(refs, sgIDs...)
	for _, id := range subnetIDByLogical {
		refs = append(refs, id)
	}
	return f.create(ctx, neutron.KindPort, p.Name, refs...)
}

func (f *fakeNeutron) CreateFloatingIP(ctx context.Context, fip plan.FloatingIP, externalNetworkID, portID string) (neutron.Resource, error) {
	return f.create(ctx, neutron.KindFloatingIP, fip.Name, portID)
}

func (f *fakeNeutron) CreateRouterInterface(ctx context.Context, ri plan.RouterInterface, routerID, subnetID, portID string) (neutron.Resource, error) {
	target := subnetID
	if target == "" {
		target = portID
	}
	res, err := f.create(ctx, neutron.KindRouterInterface, ri.Name, routerID, target)
	if err == nil {
		f.mu.Lock()
		f.iface[res.ID] = ifaceRec{router: routerID, subnet: subnetID, port: portID}
		f.mu.Unlock()
	}
	return res, err
}

func (f *fakeNeutron) Delete(ctx context.Context, r neutron.Resource) error {
	if r.ID == "" {
		f.mu.Lock()
		f.emptyDeletes++
		f.mu.Unlock()
	}
	if !f.enter(ctx, func() {
		if !f.live[r.ID] {
			return
		}
		for id, parents := range f.deps {
			if id == r.ID || !f.live[id] {
				continue
			}
			for _, p := range parents {
				if p == r.ID {
					f.badDeletes = append(f.badDeletes, fmt.Sprintf("delete %s %s: dependent %s still live", r.Kind, r.ID, id))
				}
			}
		}
	}) {
		return ctx.Err()
	}
	defer f.leave()

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.live[r.ID] {
		f.live[r.ID] = false
		f.liveByKind[r.Kind]--
	}
	return nil
}

func (f *fakeNeutron) RemoveRouterInterface(ctx context.Context, routerID, subnetID, portID string) error {
	if !f.enter(ctx, func() {}) {
		return ctx.Err()
	}
	defer f.leave()

	f.mu.Lock()
	defer f.mu.Unlock()
	for id, rec := range f.iface {
		if f.live[id] && rec.router == routerID && rec.subnet == subnetID && rec.port == portID {
			f.live[id] = false
			f.liveByKind[neutron.KindRouterInterface]--
			break
		}
	}
	return nil
}

func filterNonEmpty(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}

// fakeClock is a virtual clock: Sleep advances time instantly and records the
// requested delay, so the schedule is deterministic and the drawn delays can be
// inspected. Only the scheduler goroutine touches it.
type fakeClock struct {
	cur    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{cur: time.Unix(0, 0)} }

func (c *fakeClock) Now() time.Time { return c.cur }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.sleeps = append(c.sleeps, d)
	c.cur = c.cur.Add(d)
	return nil
}

// churnPlan is a small dependency-rich plan: pool->subnet, network->subnet,
// router+subnet->interface, group->rule, network+subnet+group->port.
func churnPlan() *plan.Plan {
	return &plan.Plan{
		Scenario:    "churn",
		Seed:        7,
		SubnetPools: []plan.SubnetPool{{Name: "pool-1", IPVersion: 4, Prefixes: []string{"172.16.0.0/16"}, DefaultPrefixLen: 26}},
		Networks:    []plan.Network{{Name: "net-1"}, {Name: "net-2"}},
		Subnets: []plan.Subnet{
			{Name: "subnet-1", Network: "net-1", IPVersion: 4, CIDR: "10.0.0.0/24"},
			{Name: "subnet-2", Network: "net-2", IPVersion: 4, SubnetPool: "pool-1", PrefixLen: 26},
		},
		Routers:          []plan.Router{{Name: "router-1"}},
		RouterInterfaces: []plan.RouterInterface{{Name: "rif-1", Router: "router-1", Subnet: "subnet-1"}},
		SecurityGroups: []plan.SecurityGroup{{Name: "sg-1", Rules: []plan.SecurityGroupRule{
			{Direction: "ingress", EtherType: "IPv4", Protocol: "tcp"},
		}}},
		Ports: []plan.Port{{Name: "port-1", Network: "net-1", FixedIPs: []plan.FixedIP{{Subnet: "subnet-1"}}, SecurityGroups: []string{"sg-1"}}},
	}
}

func testConfig() Config {
	return Config{
		Duration:    2 * time.Second,
		MinInterval: 10 * time.Millisecond,
		MaxInterval: 40 * time.Millisecond,
		MaxParallel: 4,
		ChurnRatio:  0.5,
		TargetFill:  0.7,
		Concurrency: 8,
		OpTimeout:   time.Minute,
	}
}

func planLogicals(p *plan.Plan) map[string]bool {
	set := map[string]bool{}
	for _, n := range p.Networks {
		set[n.Name] = true
	}
	for _, s := range p.Subnets {
		set[s.Name] = true
	}
	for _, sp := range p.SubnetPools {
		set[sp.Name] = true
	}
	for _, r := range p.Routers {
		set[r.Name] = true
	}
	for _, ri := range p.RouterInterfaces {
		set[ri.Name] = true
	}
	for _, sg := range p.SecurityGroups {
		set[sg.Name] = true
	}
	for _, pt := range p.Ports {
		set[pt.Name] = true
	}
	return set
}

func TestRunDeterministicSchedule(t *testing.T) {
	cfg := testConfig()
	p := churnPlan()

	r1, err := Run(context.Background(), newFake(), p, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	r2, err := Run(context.Background(), newFake(), p, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run #2: %v", err)
	}

	if len(r1.Decisions) == 0 {
		t.Fatal("no decisions were scheduled")
	}
	if !reflect.DeepEqual(r1.Decisions, r2.Decisions) {
		t.Errorf("decision schedules differ for the same seed/config:\n #1 (%d) vs #2 (%d)", len(r1.Decisions), len(r2.Decisions))
	}
}

func TestRunRespectsEnvelopeAndOrdering(t *testing.T) {
	f := newFake()
	p := churnPlan()
	if _, err := Run(context.Background(), f, p, testConfig(), newFakeClock()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(f.badRefs) != 0 {
		t.Errorf("create-before-parent violations: %v", f.badRefs)
	}
	if len(f.badDeletes) != 0 {
		t.Errorf("delete-before-dependent violations: %v", f.badDeletes)
	}

	// The live population per kind never exceeds the plan's count for that kind.
	want := map[neutron.Kind]int{
		neutron.KindSubnetPool:        len(p.SubnetPools),
		neutron.KindNetwork:           len(p.Networks),
		neutron.KindSubnet:            len(p.Subnets),
		neutron.KindRouter:            len(p.Routers),
		neutron.KindRouterInterface:   len(p.RouterInterfaces),
		neutron.KindSecurityGroup:     len(p.SecurityGroups),
		neutron.KindSecurityGroupRule: 1,
		neutron.KindPort:              len(p.Ports),
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for kind, ceiling := range want {
		if f.maxByKind[kind] > ceiling {
			t.Errorf("kind %s peaked at %d live, exceeding the envelope ceiling %d", kind, f.maxByKind[kind], ceiling)
		}
	}

	// Only planned logicals are ever created.
	allowed := planLogicals(p)
	for _, l := range f.logicals {
		if _, ok := allowed[l]; ok {
			continue
		}
		// Security-group rules use a synthetic "rule@<sgID>" logical.
		if len(l) >= 5 && l[:5] == "rule@" {
			continue
		}
		t.Errorf("created an unplanned logical resource %q", l)
	}
}

func TestRunParallelismAndIntervalBounds(t *testing.T) {
	f := newFake()
	f.workDelay = 2 * time.Millisecond // make operations overlap
	cfg := testConfig()
	clk := newFakeClock()

	if _, err := Run(context.Background(), f, churnPlan(), cfg, clk); err != nil {
		t.Fatalf("Run: %v", err)
	}

	limit := cfg.MaxParallel
	if cfg.Concurrency < limit {
		limit = cfg.Concurrency
	}
	f.mu.Lock()
	maxInFlight := f.maxInFlight
	f.mu.Unlock()
	if maxInFlight > limit {
		t.Errorf("max in-flight %d exceeded the bound %d", maxInFlight, limit)
	}
	if maxInFlight == 0 {
		t.Error("no operations ran")
	}

	for _, d := range clk.sleeps {
		if d < cfg.MinInterval || d > cfg.MaxInterval {
			t.Errorf("drawn delay %s outside [%s, %s]", d, cfg.MinInterval, cfg.MaxInterval)
		}
	}
}

func TestRunControllerTracksTargetFill(t *testing.T) {
	// A long run over a wider plan so the steady-state mean is meaningful.
	p := &plan.Plan{Networks: make([]plan.Network, 40)}
	for i := range p.Networks {
		p.Networks[i] = plan.Network{Name: fmt.Sprintf("net-%02d", i)}
	}

	run := func(target float64) *Result {
		cfg := testConfig()
		cfg.Duration = 10 * time.Second
		cfg.ChurnRatio = 0.5
		cfg.TargetFill = target
		r, err := Run(context.Background(), newFake(), p, cfg, newFakeClock())
		if err != nil {
			t.Fatalf("Run(target=%v): %v", target, err)
		}
		return r
	}

	low := run(0.2)
	high := run(0.8)
	envelope := float64(len(p.Networks))

	// With churn_ratio > 0 the population stays well clear of empty.
	if high.PopMean < 0.4*envelope {
		t.Errorf("high target mean population %.1f is far below target 0.8*%v", high.PopMean, envelope)
	}
	// A higher target fill yields a higher steady-state population.
	if high.PopMean <= low.PopMean {
		t.Errorf("target fill is not monotone: mean(0.8)=%.1f <= mean(0.2)=%.1f", high.PopMean, low.PopMean)
	}
}

func TestRunDrainsBeforeReturning(t *testing.T) {
	f := newFake()
	f.workDelay = time.Millisecond
	if _, err := Run(context.Background(), f, churnPlan(), testConfig(), newFakeClock()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inFlight != 0 {
		t.Errorf("Run returned with %d operations still in flight", f.inFlight)
	}
}

func TestRunCancellationReturnsPromptly(t *testing.T) {
	f := newFake()
	f.holdUntilCancel = true
	f.started = make(chan struct{})

	cfg := testConfig()
	cfg.Duration = time.Hour // long enough that only cancellation stops the run

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// A real clock so the scheduler's inter-tick sleep is interruptible.
		_, _ = Run(ctx, f, churnPlan(), cfg, RealClock{})
		close(done)
	}()

	<-f.started // an operation is in flight, blocked until cancellation
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return promptly after cancellation")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inFlight != 0 {
		t.Errorf("cancellation orphaned %d in-flight operations", f.inFlight)
	}
}

func TestRunCountsChurnStats(t *testing.T) {
	f := newFake()
	r, err := Run(context.Background(), f, churnPlan(), testConfig(), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if r.Creates == 0 {
		t.Fatal("no creates were scheduled")
	}
	// Every delete is preceded by a create of the same instance, so cycles (a
	// successful create+delete round trip on the always-succeeding fake) equals
	// the number of completed deletes.
	if r.Cycles != r.Deletes {
		t.Errorf("cycles = %d, want %d (one per completed delete on a fake that never fails)", r.Cycles, r.Deletes)
	}
	if r.Creates < r.Deletes {
		t.Errorf("creates (%d) < deletes (%d): cannot delete more than was created", r.Creates, r.Deletes)
	}
	// Live resources at the end equal creates minus deletes.
	if got := len(r.Created); got != r.Creates-r.Deletes {
		t.Errorf("live resources at end = %d, want creates-deletes = %d", got, r.Creates-r.Deletes)
	}
	if r.PopMax < r.PopMin || r.PopMin < 0 {
		t.Errorf("population summary inconsistent: min=%d max=%d", r.PopMin, r.PopMax)
	}
	if r.TargetFill != testConfig().TargetFill {
		t.Errorf("result target fill = %v, want %v", r.TargetFill, testConfig().TargetFill)
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	// Each case violates exactly one rule of Config.Validate, including the upper
	// ceilings that keep absurd-but-typed operator input from driving the
	// scheduler into runaway fan-out or an overflowed interval span.
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"zero duration", func(c *Config) { c.Duration = 0 }},
		{"non-positive min-interval", func(c *Config) { c.MinInterval = 0 }},
		{"min-interval above max-interval", func(c *Config) { c.MinInterval = c.MaxInterval + time.Millisecond }},
		{"max-interval above ceiling", func(c *Config) { c.MaxInterval = maxIntervalCeiling + time.Minute }},
		{"zero max-parallel", func(c *Config) { c.MaxParallel = 0 }},
		{"max-parallel above ceiling", func(c *Config) { c.MaxParallel = maxParallelCeiling + 1 }},
		{"zero concurrency", func(c *Config) { c.Concurrency = 0 }},
		{"churn-ratio above one", func(c *Config) { c.ChurnRatio = 1.5 }},
		{"target-fill below zero", func(c *Config) { c.TargetFill = -0.1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			tc.mutate(&cfg)
			if _, err := Run(context.Background(), newFake(), churnPlan(), cfg, newFakeClock()); err == nil {
				t.Fatal("expected Run to reject the config, got nil error")
			}
		})
	}
}

// TestRunSkipsDoomedDescendantsOfFailedCreate fails every network create, then
// checks the engine does not pile bookkeeping-artifact failures on top: a child
// whose parent create yielded no cloud id is skipped before it reaches the cloud
// (missingParentID), and a logically-deleted node that never produced a resource
// is never deleted with an empty id.
func TestRunSkipsDoomedDescendantsOfFailedCreate(t *testing.T) {
	f := newFake()
	// Networks always fail, so their create publishes no cloud id even though the
	// scheduler still marks them logically present and schedules their children.
	f.failKinds = map[neutron.Kind]error{neutron.KindNetwork: errors.New("simulated create failure")}

	if _, err := Run(context.Background(), f, churnPlan(), testConfig(), newFakeClock()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.createCalls[neutron.KindNetwork] == 0 {
		t.Fatal("no network creates were attempted; the test exercises nothing")
	}
	// Every descendant resolves a network parent's empty id, so its create is
	// skipped before reaching the cloud rather than attempted with a dangling ref.
	for _, kind := range []neutron.Kind{neutron.KindSubnet, neutron.KindPort, neutron.KindRouterInterface} {
		if f.createCalls[kind] != 0 {
			t.Errorf("kind %s: %d doomed child creates reached the cloud, want 0", kind, f.createCalls[kind])
		}
	}
	if len(f.badRefs) != 0 {
		t.Errorf("doomed child creates produced ordering violations: %v", f.badRefs)
	}
	// A node deleted after a create that produced no resource is never deleted in
	// the cloud: dispatchDelete returns before issuing an empty-id delete.
	if f.emptyDeletes != 0 {
		t.Errorf("%d deletes were issued with an empty resource id, want 0", f.emptyDeletes)
	}
}

// TestRunRecoversFromOperationPanic makes every network create panic (a
// misbehaving cloud client) and checks the run survives: the goroutine recovers,
// the operation is recorded as a "panic" failure, and the engine still drains.
func TestRunRecoversFromOperationPanic(t *testing.T) {
	// Silence the panic log the engine emits on recovery so the test output stays
	// readable; the behavior under test is that the run survives, not the log line.
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	f := newFake()
	f.panicKinds = map[neutron.Kind]bool{neutron.KindNetwork: true}

	r, err := Run(context.Background(), f, churnPlan(), testConfig(), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	f.mu.Lock()
	inFlight := f.inFlight
	attempts := f.createCalls[neutron.KindNetwork]
	f.mu.Unlock()

	if attempts == 0 {
		t.Fatal("no network creates were attempted; the test exercises nothing")
	}
	if inFlight != 0 {
		t.Errorf("Run returned with %d operations still in flight after a panic", inFlight)
	}

	var panics int
	for _, b := range r.Buckets {
		for _, ec := range b.Errors {
			if ec.Kind == "panic" {
				panics += ec.Count
			}
		}
	}
	if panics == 0 {
		t.Error("a recovered panic was not recorded as a failed operation")
	}
}
