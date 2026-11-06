package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/octavia"
	"github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// shrinkDelays makes the retry backoff and the 409 re-send interval of the
// package a millisecond for the rest of the test, so a test that drives a retry
// runs in milliseconds, and restores them when the test ends.
func shrinkDelays(t *testing.T) {
	t.Helper()
	base, maxDelay, conflict := retryBaseDelay, retryMaxDelay, conflictRetryInterval
	retryBaseDelay, retryMaxDelay, conflictRetryInterval = time.Millisecond, time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		retryBaseDelay, retryMaxDelay, conflictRetryInterval = base, maxDelay, conflict
	})
}

// conflict is a non-quota 409, what Octavia answers a write to a load balancer
// that is not ACTIVE with; IsRetryable treats it as transient.
var conflict = gophercloud.ErrUnexpectedResponseCode{
	Actual: 409,
	Body:   []byte(`{"faultstring": "Load Balancer lb is immutable and cannot be updated."}`),
}

// errListenerQuota is a create error as the client wraps an Octavia quota rejection.
var errListenerQuota = fmt.Errorf("creating listener %q: %w: %w", "lsn-0001", octavia.ErrQuota,
	gophercloud.ErrUnexpectedResponseCode{Actual: 403, Body: []byte(`{"faultstring": "Quota has been met."}`)})

// lbCall, poolCall, and memberCall are the arguments the fake received for one
// create, keyed by the created item's logical name.
type (
	lbCall     struct{ subnetID, provider string }
	poolCall   struct{ lbID, listenerID, algorithm string }
	memberCall struct{ poolID, subnetID string }
)

// gate holds the first listener create of each load balancer until n load
// balancers have one in flight, so n load balancers are certain to overlap
// rather than left to the scheduler to overlap.
type gate struct {
	n       int
	arrived map[string]bool
	open    chan struct{}
}

// fakeOctavia records every call the executor drives, in order, so tests can
// assert on call order, arguments, and coverage. It is the in-test
// implementation of the executor's Octavia seam. Like the real client it
// answers a call on a done context with the context's error and records
// nothing. It also tracks, per load balancer, the writes and waits in flight:
// any second call on a load balancer while one is in flight is an overlap.
type fakeOctavia struct {
	mu     sync.Mutex
	events []string

	providers  map[string]string        // load balancer logical → provider CreateLoadBalancer reports
	createErrs map[string][]error       // logical → errors its successive creates return before one succeeds
	waitErrs   map[resource.Kind]error  // WaitForActive after a write of kind returns this
	blockWait  resource.Kind            // WaitForActive after a write of this kind blocks until its ctx is done
	onWait     func(kind resource.Kind) // called as every WaitForActive starts
	readyErr   error                    // WaitForReady returns this
	gate       *gate                    // when set, the first listener create of each load balancer waits at it

	subnetArgs   map[string]string // subnet logical → network id
	lbArgs       map[string]lbCall
	listenerArgs map[string]string // listener logical → load balancer id
	poolArgs     map[string]poolCall
	memberArgs   map[string]memberCall
	monitorArgs  map[string]string // health monitor logical → pool id

	lbOf       map[string]string   // pool id → its load balancer id
	busy       map[string]int      // load balancer id → writes and waits in flight
	writing    map[string]int      // load balancer id → writes in flight
	maxWriting int                 // most load balancers ever with a write in flight at once
	overlaps   []string            // every call that started while another was in flight on its load balancer
	perLB      map[string][]string // load balancer id → the start and end of each call on it, in order
}

// newFakeOctavia returns a fakeOctavia that creates everything and reports
// an empty provider for every load balancer.
func newFakeOctavia() *fakeOctavia {
	return &fakeOctavia{
		providers:    map[string]string{},
		createErrs:   map[string][]error{},
		waitErrs:     map[resource.Kind]error{},
		subnetArgs:   map[string]string{},
		lbArgs:       map[string]lbCall{},
		listenerArgs: map[string]string{},
		poolArgs:     map[string]poolCall{},
		memberArgs:   map[string]memberCall{},
		monitorArgs:  map[string]string{},
		lbOf:         map[string]string{},
		busy:         map[string]int{},
		writing:      map[string]int{},
		perLB:        map[string][]string{},
	}
}

// res returns the Resource the fake creates for kind and logical, whose id is
// "<kind>-<logical>".
func res(kind resource.Kind, logical string) resource.Resource {
	return resource.Resource{Kind: kind, Logical: logical, Name: "dizzy-run0-" + logical, ID: string(kind) + "-" + logical}
}

// id returns the id the fake gives the resource of kind and logical.
func id(kind resource.Kind, logical string) string { return res(kind, logical).ID }

func (f *fakeOctavia) record(ev string) {
	f.mu.Lock()
	f.events = append(f.events, ev)
	f.mu.Unlock()
}

// begin marks a call on the load balancer lbID as in flight: a write when
// write is set, otherwise a wait. It records the call as an overlap when
// another is already in flight on lbID.
func (f *fakeOctavia) begin(lbID, what string, write bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy[lbID]++
	if f.busy[lbID] > 1 {
		f.overlaps = append(f.overlaps, fmt.Sprintf("%s on %s started with %d other call(s) in flight", what, lbID, f.busy[lbID]-1))
	}
	if write {
		f.writing[lbID]++
		var n int
		for _, w := range f.writing {
			if w > 0 {
				n++
			}
		}
		f.maxWriting = max(f.maxWriting, n)
	}
	f.perLB[lbID] = append(f.perLB[lbID], "start "+what)
}

// end marks the call begin started as finished.
func (f *fakeOctavia) end(lbID, what string, write bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy[lbID]--
	if write {
		f.writing[lbID]--
	}
	f.perLB[lbID] = append(f.perLB[lbID], "end "+what)
}

// createErr returns the next error queued for a create of logical, or nil.
func (f *fakeOctavia) createErr(logical string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	errs := f.createErrs[logical]
	if len(errs) == 0 {
		return nil
	}
	f.createErrs[logical] = errs[1:]
	return errs[0]
}

// await holds the first listener create of lbID at the gate until the gate
// opens, or returns ctx's error when ctx ends first.
func (f *fakeOctavia) await(ctx context.Context, lbID string) error {
	f.mu.Lock()
	g := f.gate
	first := g != nil && !g.arrived[lbID]
	if first {
		g.arrived[lbID] = true
		if len(g.arrived) == g.n {
			close(g.open)
		}
	}
	f.mu.Unlock()
	if !first {
		return nil
	}
	select {
	case <-g.open:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("gate: fewer than %d load balancers wrote at once: %w", g.n, ctx.Err())
	}
}

func (f *fakeOctavia) CreateNetwork(ctx context.Context, n plan.Network) (resource.Resource, error) {
	if err := ctx.Err(); err != nil {
		return resource.Resource{}, err
	}
	f.record("createNetwork:" + n.Name)
	if err := f.createErr(n.Name); err != nil {
		return resource.Resource{}, err
	}
	return res(octavia.KindNetwork, n.Name), nil
}

func (f *fakeOctavia) CreateSubnet(ctx context.Context, n plan.Network, networkID string) (resource.Resource, error) {
	if err := ctx.Err(); err != nil {
		return resource.Resource{}, err
	}
	f.record("createSubnet:" + n.Subnet)
	f.mu.Lock()
	f.subnetArgs[n.Subnet] = networkID
	f.mu.Unlock()
	if err := f.createErr(n.Subnet); err != nil {
		return resource.Resource{}, err
	}
	return res(octavia.KindSubnet, n.Subnet), nil
}

func (f *fakeOctavia) WaitForReady(ctx context.Context, r resource.Resource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.record("waitReady:" + r.Logical)
	return f.readyErr
}

func (f *fakeOctavia) CreateLoadBalancer(ctx context.Context, lb plan.LoadBalancer, subnetID, provider string) (resource.Resource, string, error) {
	if err := ctx.Err(); err != nil {
		return resource.Resource{}, "", err
	}
	lbID, what := id(octavia.KindLoadBalancer, lb.Name), "create loadbalancer "+lb.Name
	f.begin(lbID, what, true)
	defer f.end(lbID, what, true)
	f.record("createLoadBalancer:" + lb.Name)
	f.mu.Lock()
	f.lbArgs[lb.Name] = lbCall{subnetID: subnetID, provider: provider}
	reported := f.providers[lb.Name]
	f.mu.Unlock()
	if err := f.createErr(lb.Name); err != nil {
		return resource.Resource{}, "", err
	}
	return res(octavia.KindLoadBalancer, lb.Name), reported, nil
}

func (f *fakeOctavia) CreateListener(ctx context.Context, l plan.Listener, lbID string) (resource.Resource, error) {
	if err := ctx.Err(); err != nil {
		return resource.Resource{}, err
	}
	what := "create listener " + l.Name
	f.begin(lbID, what, true)
	defer f.end(lbID, what, true)
	f.record("createListener:" + l.Name)
	f.mu.Lock()
	f.listenerArgs[l.Name] = lbID
	f.mu.Unlock()
	if err := f.await(ctx, lbID); err != nil {
		return resource.Resource{}, err
	}
	if err := f.createErr(l.Name); err != nil {
		return resource.Resource{}, err
	}
	return res(octavia.KindListener, l.Name), nil
}

func (f *fakeOctavia) CreatePool(ctx context.Context, p plan.Pool, lbID, listenerID, algorithm string) (resource.Resource, error) {
	if err := ctx.Err(); err != nil {
		return resource.Resource{}, err
	}
	what := "create pool " + p.Name
	f.begin(lbID, what, true)
	defer f.end(lbID, what, true)
	f.record("createPool:" + p.Name)
	f.mu.Lock()
	f.poolArgs[p.Name] = poolCall{lbID: lbID, listenerID: listenerID, algorithm: algorithm}
	f.lbOf[id(octavia.KindPool, p.Name)] = lbID
	f.mu.Unlock()
	if err := f.createErr(p.Name); err != nil {
		return resource.Resource{}, err
	}
	return res(octavia.KindPool, p.Name), nil
}

func (f *fakeOctavia) CreateMember(ctx context.Context, m plan.Member, poolID, subnetID string) (resource.Resource, error) {
	if err := ctx.Err(); err != nil {
		return resource.Resource{}, err
	}
	f.mu.Lock()
	lbID := f.lbOf[poolID]
	f.mu.Unlock()
	what := "create member " + m.Name
	f.begin(lbID, what, true)
	defer f.end(lbID, what, true)
	f.record("createMember:" + m.Name)
	f.mu.Lock()
	f.memberArgs[m.Name] = memberCall{poolID: poolID, subnetID: subnetID}
	f.mu.Unlock()
	if err := f.createErr(m.Name); err != nil {
		return resource.Resource{}, err
	}
	return res(octavia.KindMember, m.Name), nil
}

func (f *fakeOctavia) CreateHealthMonitor(ctx context.Context, h plan.HealthMonitor, poolID string) (resource.Resource, error) {
	if err := ctx.Err(); err != nil {
		return resource.Resource{}, err
	}
	f.mu.Lock()
	lbID := f.lbOf[poolID]
	f.mu.Unlock()
	what := "create healthmonitor " + h.Name
	f.begin(lbID, what, true)
	defer f.end(lbID, what, true)
	f.record("createHealthMonitor:" + h.Name)
	f.mu.Lock()
	f.monitorArgs[h.Name] = poolID
	f.mu.Unlock()
	if err := f.createErr(h.Name); err != nil {
		return resource.Resource{}, err
	}
	return res(octavia.KindHealthMonitor, h.Name), nil
}

func (f *fakeOctavia) WaitForActive(ctx context.Context, lb resource.Resource, kind resource.Kind) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	what := "wait " + string(kind)
	f.begin(lb.ID, what, false)
	defer f.end(lb.ID, what, false)
	f.record("waitActive:" + lb.Logical + "/" + string(kind))
	if f.onWait != nil {
		f.onWait(kind)
	}
	if kind == f.blockWait {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.waitErrs[kind]
}

func (f *fakeOctavia) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// count returns how many events equal ev.
func count(events []string, ev string) int {
	var n int
	for _, e := range events {
		if e == ev {
			n++
		}
	}
	return n
}

// withPrefix returns the events that start with any of prefixes.
func withPrefix(events []string, prefixes ...string) []string {
	var out []string
	for _, e := range events {
		for _, p := range prefixes {
			if strings.HasPrefix(e, p) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// roundRobinPlan builds a valid plan the way the generator assigns it, with
// fixed ports: load balancer i on network i % networks, listener i on load
// balancer i % lbs, pool i as the default pool of listener i, member m in pool
// m % listeners at the next upper-half address of its network, and a health
// monitor on each of the first monitors pools.
func roundRobinPlan(t *testing.T, networks, lbs, listeners, members, monitors int) *plan.Plan {
	t.Helper()
	p := &plan.Plan{Scenario: "t", Seed: 42}
	for i := range networks {
		p.Networks = append(p.Networks, plan.Network{
			Name: fmt.Sprintf("net-%04d", i+1), Subnet: fmt.Sprintf("sub-%04d", i+1), CIDR: fmt.Sprintf("10.0.%d.0/24", i+1),
		})
	}
	for i := range lbs {
		p.LoadBalancers = append(p.LoadBalancers, plan.LoadBalancer{Name: fmt.Sprintf("lb-%04d", i+1), Network: p.Networks[i%networks].Name})
	}
	for i := range listeners {
		p.Listeners = append(p.Listeners, plan.Listener{Name: fmt.Sprintf("lsn-%04d", i+1), LoadBalancer: p.LoadBalancers[i%lbs].Name, Port: 1024 + i})
		p.Pools = append(p.Pools, plan.Pool{Name: fmt.Sprintf("pool-%04d", i+1), Listener: p.Listeners[i].Name})
	}
	perPool := make([]int, listeners)
	perNet := make([]int, networks)
	for m := range members {
		pool := m % listeners
		perPool[pool]++
		net := (pool % lbs) % networks
		p.Members = append(p.Members, plan.Member{
			Name:    plan.MemberName(p.Pools[pool].Name, perPool[pool]),
			Pool:    p.Pools[pool].Name,
			Address: fmt.Sprintf("10.0.%d.%d", net+1, 128+perNet[net]),
			Port:    8080,
		})
		perNet[net]++
	}
	for i := range monitors {
		p.HealthMonitors = append(p.HealthMonitors, plan.HealthMonitor{Name: fmt.Sprintf("hm-%04d", i+1), Pool: p.Pools[i].Name})
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("test plan is invalid: %v", err)
	}
	return p
}

// lbOfListener returns the logical name of the load balancer of listener in
// p, resolved independently of the executor.
func lbOfListener(p *plan.Plan, listener string) string {
	for _, l := range p.Listeners {
		if l.Name == listener {
			return l.LoadBalancer
		}
	}
	return ""
}

// lbOfPool returns the logical name of the load balancer of pool in p,
// through its listener.
func lbOfPool(p *plan.Plan, pool string) string {
	for _, pl := range p.Pools {
		if pl.Name == pool {
			return lbOfListener(p, pl.Listener)
		}
	}
	return ""
}

// networkOfLB returns the VIP network the load balancer lb of p sits on.
func networkOfLB(p *plan.Plan, lb string) plan.Network {
	for _, b := range p.LoadBalancers {
		if b.Name == lb {
			for _, n := range p.Networks {
				if n.Name == b.Network {
					return n
				}
			}
		}
	}
	return plan.Network{}
}

func TestApplyRunsTheSixStagesInOrder(t *testing.T) {
	f := newFakeOctavia()
	p := roundRobinPlan(t, 2, 3, 6, 12, 3)
	if _, err := Apply(context.Background(), f, p, 4, time.Second); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	e := f.snapshot()

	// Each stage's calls, waits included; every one of a stage must come after
	// every one of the stage before it.
	stages := [][]string{
		{"createNetwork:", "waitReady:", "createSubnet:"},
		{"createLoadBalancer:", "waitActive:lb-0001/loadbalancer", "waitActive:lb-0002/loadbalancer", "waitActive:lb-0003/loadbalancer"},
		{"createListener:", "waitActive:lb-0001/listener", "waitActive:lb-0002/listener", "waitActive:lb-0003/listener"},
		{"createPool:", "waitActive:lb-0001/pool", "waitActive:lb-0002/pool", "waitActive:lb-0003/pool"},
		{"createMember:", "waitActive:lb-0001/member", "waitActive:lb-0002/member", "waitActive:lb-0003/member"},
		{"createHealthMonitor:", "waitActive:lb-0001/healthmonitor", "waitActive:lb-0002/healthmonitor", "waitActive:lb-0003/healthmonitor"},
	}
	stageOf := func(ev string) int {
		for s, prefixes := range stages {
			for _, p := range prefixes {
				if strings.HasPrefix(ev, p) {
					return s
				}
			}
		}
		return -1
	}
	prev := 0
	for i, ev := range e {
		s := stageOf(ev)
		if s < 0 {
			t.Fatalf("event %q belongs to no stage", ev)
		}
		if s < prev {
			t.Errorf("event %d %q of stage %d came after stage %d had started: %v", i, ev, s+1, prev+1, e)
		}
		prev = max(prev, s)
	}
	if prev != len(stages)-1 {
		t.Errorf("the run ended in stage %d, want all six stages: %v", prev+1, e)
	}

	firstNet := slices.Index(e, "createNetwork:net-0001")
	firstLB := slices.Index(e, "createLoadBalancer:lb-0001")
	if firstNet < 0 || firstLB < 0 || firstNet > firstLB {
		t.Errorf("first network create at %d, first load balancer create at %d; want the network first: %v", firstNet, firstLB, e)
	}

	for prefix, want := range map[string]int{
		"createNetwork:": 2, "createSubnet:": 2, "waitReady:": 2, "createLoadBalancer:": 3,
		"createListener:": 6, "createPool:": 6, "createMember:": 12, "createHealthMonitor:": 3,
	} {
		if got := len(withPrefix(e, prefix)); got != want {
			t.Errorf("%s calls = %d, want %d", prefix, got, want)
		}
	}
}

func TestApplySerializesWritesPerLoadBalancerWhileLoadBalancersOverlap(t *testing.T) {
	f := newFakeOctavia()
	// The first listener create of each load balancer waits until both load
	// balancers have one in flight, so the two must overlap; the generous
	// timeout only bounds a broken executor that never lets them.
	f.gate = &gate{n: 2, arrived: map[string]bool{}, open: make(chan struct{})}
	p := roundRobinPlan(t, 1, 2, 4, 8, 2)
	if _, err := Apply(context.Background(), f, p, 4, 5*time.Second); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.overlaps) > 0 {
		t.Errorf("calls overlapped on one load balancer:\n%s", strings.Join(f.overlaps, "\n"))
	}
	if f.maxWriting < 2 {
		t.Errorf("at most %d load balancer(s) had a write in flight at once, want 2", f.maxWriting)
	}
	for lbID, n := range f.busy {
		if n != 0 {
			t.Errorf("load balancer %s ended with %d call(s) in flight, want 0", lbID, n)
		}
	}
}

func TestApplyWaitsForActiveAfterEveryWriteBeforeTheNext(t *testing.T) {
	f := newFakeOctavia()
	p := roundRobinPlan(t, 1, 2, 4, 8, 2)
	if _, err := Apply(context.Background(), f, p, 4, time.Second); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}

	// Per load balancer: its own create and every child create, in stage and
	// plan order, each finishing and then followed by a wait of its kind that
	// finishes before the next write starts.
	for _, lb := range p.LoadBalancers {
		var want []string
		write := func(kind resource.Kind, name string) {
			what := "create " + string(kind) + " " + name
			want = append(want, "start "+what, "end "+what, "start wait "+string(kind), "end wait "+string(kind))
		}
		write(octavia.KindLoadBalancer, lb.Name)
		for _, l := range p.Listeners {
			if l.LoadBalancer == lb.Name {
				write(octavia.KindListener, l.Name)
			}
		}
		for _, pl := range p.Pools {
			if lbOfPool(p, pl.Name) == lb.Name {
				write(octavia.KindPool, pl.Name)
			}
		}
		for _, m := range p.Members {
			if lbOfPool(p, m.Pool) == lb.Name {
				write(octavia.KindMember, m.Name)
			}
		}
		for _, h := range p.HealthMonitors {
			if lbOfPool(p, h.Pool) == lb.Name {
				write(octavia.KindHealthMonitor, h.Name)
			}
		}

		f.mu.Lock()
		got := f.perLB[id(octavia.KindLoadBalancer, lb.Name)]
		f.mu.Unlock()
		if !slices.Equal(got, want) {
			t.Errorf("calls on %s:\n got %v\nwant %v", lb.Name, got, want)
		}
	}
}

func TestApplyBindsPoolAlgorithmToEffectiveProvider(t *testing.T) {
	f := newFakeOctavia()
	f.providers["lb-0001"] = "ovn"
	f.providers["lb-0002"] = "amphora"
	p := roundRobinPlan(t, 1, 2, 4, 4, 0)
	p.Provider = "requested"
	if _, err := Apply(context.Background(), f, p, 4, time.Second); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}

	for _, lb := range p.LoadBalancers {
		want := lbCall{subnetID: id(octavia.KindSubnet, "sub-0001"), provider: "requested"}
		if got := f.lbArgs[lb.Name]; got != want {
			t.Errorf("CreateLoadBalancer(%s) got %+v, want %+v", lb.Name, got, want)
		}
	}
	wantAlgorithm := map[string]string{"lb-0001": "SOURCE_IP_PORT", "lb-0002": "ROUND_ROBIN"}
	for _, pl := range p.Pools {
		lb := lbOfPool(p, pl.Name)
		want := poolCall{
			lbID:       id(octavia.KindLoadBalancer, lb),
			listenerID: id(octavia.KindListener, pl.Listener),
			algorithm:  wantAlgorithm[lb],
		}
		if got := f.poolArgs[pl.Name]; got != want {
			t.Errorf("CreatePool(%s) on %s got %+v, want %+v", pl.Name, lb, got, want)
		}
	}
}

func TestApplyLogsTheBoundProvider(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newFakeOctavia()
	f.providers["lb-0001"] = "ovn"
	if _, err := Apply(context.Background(), f, roundRobinPlan(t, 1, 1, 1, 1, 0), 1, time.Second); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	want := `"msg":"load balancer provider bound","logical":"lb-0001","provider":"ovn","algorithm":"SOURCE_IP_PORT"`
	if !strings.Contains(buf.String(), want) {
		t.Errorf("log does not contain %s:\n%s", want, buf.String())
	}
	for _, kind := range []resource.Kind{octavia.KindNetwork, octavia.KindSubnet, octavia.KindLoadBalancer, octavia.KindListener, octavia.KindPool, octavia.KindMember} {
		line := fmt.Sprintf(`"msg":"created resource","kind":%q`, kind)
		if !strings.Contains(buf.String(), line) {
			t.Errorf("log has no %s line", line)
		}
	}
}

func TestApplyMemberReceivesItsLoadBalancersSubnetAndItsPoolsID(t *testing.T) {
	f := newFakeOctavia()
	// Two networks, so the two load balancers sit on different subnets.
	p := roundRobinPlan(t, 2, 2, 4, 8, 0)
	if _, err := Apply(context.Background(), f, p, 4, time.Second); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	for _, m := range p.Members {
		want := memberCall{
			poolID:   id(octavia.KindPool, m.Pool),
			subnetID: id(octavia.KindSubnet, networkOfLB(p, lbOfPool(p, m.Pool)).Subnet),
		}
		if got := f.memberArgs[m.Name]; got != want {
			t.Errorf("CreateMember(%s) got %+v, want %+v", m.Name, got, want)
		}
	}
	if got, want := f.memberArgs["pool-0002-member-01"].subnetID, id(octavia.KindSubnet, "sub-0002"); got != want {
		t.Errorf("member of lb-0002 got subnet %q, want %q", got, want)
	}
}

func TestApplyChildrenReceiveTheirParentsIDs(t *testing.T) {
	f := newFakeOctavia()
	p := roundRobinPlan(t, 2, 2, 4, 4, 4)
	if _, err := Apply(context.Background(), f, p, 4, time.Second); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	for _, n := range p.Networks {
		if got, want := f.subnetArgs[n.Subnet], id(octavia.KindNetwork, n.Name); got != want {
			t.Errorf("CreateSubnet(%s) got network %q, want %q", n.Subnet, got, want)
		}
	}
	for _, lb := range p.LoadBalancers {
		if got, want := f.lbArgs[lb.Name].subnetID, id(octavia.KindSubnet, networkOfLB(p, lb.Name).Subnet); got != want {
			t.Errorf("CreateLoadBalancer(%s) got subnet %q, want %q", lb.Name, got, want)
		}
	}
	for _, l := range p.Listeners {
		if got, want := f.listenerArgs[l.Name], id(octavia.KindLoadBalancer, l.LoadBalancer); got != want {
			t.Errorf("CreateListener(%s) got load balancer %q, want %q", l.Name, got, want)
		}
	}
	for _, h := range p.HealthMonitors {
		if got, want := f.monitorArgs[h.Name], id(octavia.KindPool, h.Pool); got != want {
			t.Errorf("CreateHealthMonitor(%s) got pool %q, want %q", h.Name, got, want)
		}
	}
}

// applyWithListenerProvisioningError applies a two-load-balancer plan one work
// item at a time, with the load balancer reaching ERROR after the first
// listener write.
func applyWithListenerProvisioningError(t *testing.T) (*fakeOctavia, *Result, error) {
	t.Helper()
	f := newFakeOctavia()
	f.waitErrs[octavia.KindListener] = fmt.Errorf("%w: load balancer %s reached ERROR after a listener write",
		octavia.ErrProvisioning, id(octavia.KindLoadBalancer, "lb-0001"))
	result, err := Apply(context.Background(), f, roundRobinPlan(t, 1, 2, 4, 8, 2), 1, time.Second)
	return f, result, err
}

func TestApplyProvisioningErrorAfterListenerStopsTheRun(t *testing.T) {
	f, _, err := applyWithListenerProvisioningError(t)
	if !errors.Is(err, octavia.ErrProvisioning) {
		t.Fatalf("Apply() = %v, want an error wrapping octavia.ErrProvisioning", err)
	}
	if want := `load balancer loadbalancer-lb-0001 did not return to ACTIVE after creating listener "lsn-0001": `; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Apply() error = %q, want it to start with %q", err, want)
	}
	e := f.snapshot()
	if later := withPrefix(e, "createPool:", "createMember:", "createHealthMonitor:"); len(later) > 0 {
		t.Errorf("a later stage started after the provisioning error: %v", later)
	}
	if n := count(e, "waitActive:lb-0001/listener"); n != 1 {
		t.Errorf("the failed wait ran %d times, want once (a provisioning error is not retried)", n)
	}
	if n := count(e, "createListener:lsn-0003"); n != 0 {
		t.Errorf("lb-0001 got another listener write after reaching ERROR: %v", e)
	}
}

func TestApplyProvisioningErrorKeepsWhatWasCreated(t *testing.T) {
	_, result, err := applyWithListenerProvisioningError(t)
	if err == nil {
		t.Fatal("Apply() = nil, want the provisioning error")
	}
	want := []resource.Resource{
		res(octavia.KindNetwork, "net-0001"),
		res(octavia.KindSubnet, "sub-0001"),
		res(octavia.KindLoadBalancer, "lb-0001"),
		res(octavia.KindLoadBalancer, "lb-0002"),
		res(octavia.KindListener, "lsn-0001"),
	}
	if !slices.Equal(result.Created, want) {
		t.Errorf("Created = %v, want %v", result.Created, want)
	}
}

func TestApplyLoadBalancerDeadlineAsksToRaiseTimeout(t *testing.T) {
	f := newFakeOctavia()
	f.blockWait = octavia.KindLoadBalancer
	result, err := Apply(context.Background(), f, roundRobinPlan(t, 1, 1, 1, 1, 0), 1, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Apply() = %v, want an error wrapping context.DeadlineExceeded", err)
	}
	want := `load balancer loadbalancer-lb-0001 did not return to ACTIVE after creating loadbalancer "lb-0001": ` +
		`context deadline exceeded; an amphora load balancer can take minutes to build, raise --timeout`
	if err.Error() != want {
		t.Errorf("Apply() error = %q, want %q", err, want)
	}
	if !strings.HasSuffix(err.Error(), "raise --timeout") {
		t.Errorf("Apply() error = %q, want it to end in %q", err, "raise --timeout")
	}
	if !slices.Contains(result.Created, res(octavia.KindLoadBalancer, "lb-0001")) {
		t.Errorf("Created = %v, want it to keep the load balancer that missed its deadline", result.Created)
	}
	if later := withPrefix(f.snapshot(), "createListener:"); len(later) > 0 {
		t.Errorf("a listener was written to a load balancer that never became ACTIVE: %v", later)
	}
}

func TestApplyChildDeadlineIsFatalWithoutTheAmphoraHint(t *testing.T) {
	f := newFakeOctavia()
	f.blockWait = octavia.KindListener
	_, err := Apply(context.Background(), f, roundRobinPlan(t, 1, 1, 1, 1, 0), 1, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Apply() = %v, want an error wrapping context.DeadlineExceeded", err)
	}
	if want := `did not return to ACTIVE after creating listener "lsn-0001": context deadline exceeded`; !strings.HasSuffix(err.Error(), want) {
		t.Errorf("Apply() error = %q, want it to end in %q", err, want)
	}
	if later := withPrefix(f.snapshot(), "createPool:"); len(later) > 0 {
		t.Errorf("a pool was written after the listener wait missed its deadline: %v", later)
	}
}

func TestApplyParentCancellationDuringWaitReturnsCtxErr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFakeOctavia()
	f.blockWait = octavia.KindListener
	f.onWait = func(kind resource.Kind) {
		if kind == octavia.KindListener {
			cancel()
		}
	}
	_, err := Apply(ctx, f, roundRobinPlan(t, 1, 1, 1, 1, 0), 1, time.Minute)
	if err != context.Canceled {
		t.Errorf("Apply() = %v, want context.Canceled unwrapped", err)
	}
}

func TestApplyRetriesListenerCreateAfterNonQuotaConflicts(t *testing.T) {
	shrinkDelays(t)
	f := newFakeOctavia()
	f.createErrs["lsn-0001"] = []error{conflict, conflict}
	result, err := Apply(context.Background(), f, roundRobinPlan(t, 1, 1, 2, 2, 1), 1, time.Second)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil after the retried 409s", err)
	}
	e := f.snapshot()
	if n := count(e, "createListener:lsn-0001"); n != 3 {
		t.Errorf("lsn-0001 was created %d times, want 3 (two 409s, then success)", n)
	}
	if n := len(withPrefix(e, "createHealthMonitor:")); n != 1 {
		t.Errorf("health monitor creates = %d, want 1 (the run completes): %v", n, e)
	}
	if !slices.Contains(result.Created, res(octavia.KindListener, "lsn-0001")) {
		t.Errorf("Created = %v, want it to hold the retried listener", result.Created)
	}
}

func TestApplyQuotaStopsTheRunOnFirstAttempt(t *testing.T) {
	f := newFakeOctavia()
	f.createErrs["lsn-0001"] = []error{errListenerQuota}
	_, err := Apply(context.Background(), f, roundRobinPlan(t, 1, 2, 4, 4, 0), 4, time.Second)
	if !errors.Is(err, octavia.ErrQuota) {
		t.Fatalf("Apply() = %v, want an error wrapping octavia.ErrQuota", err)
	}
	e := f.snapshot()
	if n := count(e, "createListener:lsn-0001"); n != 1 {
		t.Errorf("lsn-0001 was created %d times, want 1 (a quota error is not retried)", n)
	}
	if later := withPrefix(e, "createPool:", "createMember:"); len(later) > 0 {
		t.Errorf("a later stage started after the quota error: %v", later)
	}
}

func TestApplyEmptyPlanMakesNoCall(t *testing.T) {
	for name, p := range map[string]*plan.Plan{
		"nil collections": {Scenario: "empty"},
		"empty collections": {
			Scenario: "empty", Networks: []plan.Network{}, LoadBalancers: []plan.LoadBalancer{}, Listeners: []plan.Listener{},
			Pools: []plan.Pool{}, Members: []plan.Member{}, HealthMonitors: []plan.HealthMonitor{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOctavia()
			result, err := Apply(context.Background(), f, p, 4, time.Second)
			if err != nil {
				t.Fatalf("Apply() = %v, want nil", err)
			}
			if len(result.Created) != 0 {
				t.Errorf("Created = %v, want empty", result.Created)
			}
			if e := f.snapshot(); len(e) != 0 {
				t.Errorf("calls = %v, want none", e)
			}
		})
	}
}

func TestApplyWithoutChildrenMakesNoChildCall(t *testing.T) {
	f := newFakeOctavia()
	result, err := Apply(context.Background(), f, roundRobinPlan(t, 1, 2, 0, 0, 0), 4, time.Second)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	e := f.snapshot()
	if children := withPrefix(e, "createListener:", "createPool:", "createMember:", "createHealthMonitor:"); len(children) > 0 {
		t.Errorf("child calls = %v, want none", children)
	}
	if len(result.Created) != 4 {
		t.Errorf("Created = %v, want the network, subnet, and two load balancers", result.Created)
	}
}

func TestApplyCreatedListsResourcesInStageAndLoadBalancerOrder(t *testing.T) {
	f := newFakeOctavia()
	p := roundRobinPlan(t, 2, 3, 6, 12, 3)
	result, err := Apply(context.Background(), f, p, 4, time.Second)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}

	var want []resource.Resource
	for _, n := range p.Networks {
		want = append(want, res(octavia.KindNetwork, n.Name), res(octavia.KindSubnet, n.Subnet))
	}
	for _, lb := range p.LoadBalancers {
		want = append(want, res(octavia.KindLoadBalancer, lb.Name))
	}
	// Every later stage is grouped by load balancer, in load balancer plan
	// order, with each group in plan order.
	for _, lb := range p.LoadBalancers {
		for _, l := range p.Listeners {
			if l.LoadBalancer == lb.Name {
				want = append(want, res(octavia.KindListener, l.Name))
			}
		}
	}
	for _, lb := range p.LoadBalancers {
		for _, pl := range p.Pools {
			if lbOfPool(p, pl.Name) == lb.Name {
				want = append(want, res(octavia.KindPool, pl.Name))
			}
		}
	}
	for _, lb := range p.LoadBalancers {
		for _, m := range p.Members {
			if lbOfPool(p, m.Pool) == lb.Name {
				want = append(want, res(octavia.KindMember, m.Name))
			}
		}
	}
	for _, lb := range p.LoadBalancers {
		for _, h := range p.HealthMonitors {
			if lbOfPool(p, h.Pool) == lb.Name {
				want = append(want, res(octavia.KindHealthMonitor, h.Name))
			}
		}
	}
	if !slices.Equal(result.Created, want) {
		t.Errorf("Created =\n%v\nwant\n%v", result.Created, want)
	}
}

func TestApplyToleratesNetworkReadinessDeadline(t *testing.T) {
	f := newFakeOctavia()
	f.readyErr = context.DeadlineExceeded
	result, err := Apply(context.Background(), f, roundRobinPlan(t, 1, 1, 1, 1, 1), 1, time.Second)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil: a network readiness deadline is tolerated", err)
	}
	if len(result.Created) != 7 {
		t.Errorf("Created = %v, want all seven resources", result.Created)
	}
}

func TestApplyFailedSubnetKeepsItsNetwork(t *testing.T) {
	f := newFakeOctavia()
	f.createErrs["sub-0001"] = []error{gophercloud.ErrUnexpectedResponseCode{Actual: 400}}
	result, err := Apply(context.Background(), f, roundRobinPlan(t, 1, 1, 1, 1, 0), 1, time.Second)
	if err == nil {
		t.Fatal("Apply() = nil, want the subnet create error")
	}
	if want := []resource.Resource{res(octavia.KindNetwork, "net-0001")}; !slices.Equal(result.Created, want) {
		t.Errorf("Created = %v, want %v", result.Created, want)
	}
	if lbs := withPrefix(f.snapshot(), "createLoadBalancer:"); len(lbs) > 0 {
		t.Errorf("load balancers were created after the network stage failed: %v", lbs)
	}
}

func TestRunStagePrefersRealFailureOverCanceledSibling(t *testing.T) {
	// One item fails with a real error, cancelling the stage; a sibling still in
	// flight then returns context.Canceled at an earlier index. runStage must
	// surface the real failure, not the arbitrary cancelled sibling.
	realErr := errors.New("invalid provider")
	started := make(chan struct{})
	_, err := runStage(context.Background(), []int{0, 1}, 2, func(ctx context.Context, i int) (int, error) {
		if i == 0 {
			close(started) // signal the sibling is running before the failure fires
			<-ctx.Done()   // block until the real failure cancels the stage
			return 0, ctx.Err()
		}
		<-started // ensure the sibling is mid-flight so its error is context.Canceled
		return 0, realErr
	})
	if !errors.Is(err, realErr) {
		t.Fatalf("runStage() = %v, want the real failure %v masked by a cancelled sibling", err, realErr)
	}
}
