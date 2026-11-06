package octaviagraph

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/octavia"
	"github.com/B42Labs/dizzy/internal/octavia/plan"
	octaviascenario "github.com/B42Labs/dizzy/internal/octavia/scenario"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/scenarios"
)

// recordingOctavia is an Octavia that records every call as one line and
// answers with the error configured for its method, so a node closure's calls
// can be checked one by one. The closure tests call it from a single
// goroutine, so it takes no lock. A created resource's id is "id-<logical>".
type recordingOctavia struct {
	provider string           // the effective provider CreateLoadBalancer reports
	errs     map[string]error // method name -> the error every call of it returns
	waitErrs []error          // the n-th WaitForActive returns waitErrs[n-1]; nil or absent succeeds

	calls []string
	waits int
}

func (f *recordingOctavia) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func created(kind resource.Kind, logical string) resource.Resource {
	return resource.Resource{Kind: kind, Logical: logical, Name: "dizzy-run0-" + logical, ID: "id-" + logical}
}

func (f *recordingOctavia) create(method string, kind resource.Kind, logical string) (resource.Resource, error) {
	if err := f.errs[method]; err != nil {
		return resource.Resource{}, err
	}
	return created(kind, logical), nil
}

func (f *recordingOctavia) CreateNetwork(_ context.Context, n plan.Network) (resource.Resource, error) {
	f.record("CreateNetwork %s", n.Name)
	return f.create("CreateNetwork", octavia.KindNetwork, n.Name)
}

func (f *recordingOctavia) CreateSubnet(_ context.Context, n plan.Network, networkID string) (resource.Resource, error) {
	f.record("CreateSubnet %s network=%s", n.Subnet, networkID)
	return f.create("CreateSubnet", octavia.KindSubnet, n.Subnet)
}

func (f *recordingOctavia) WaitForReady(_ context.Context, r resource.Resource) error {
	f.record("WaitForReady %s", r.ID)
	return f.errs["WaitForReady"]
}

func (f *recordingOctavia) CreateLoadBalancer(_ context.Context, lb plan.LoadBalancer, subnetID, provider string) (resource.Resource, string, error) {
	f.record("CreateLoadBalancer %s subnet=%s provider=%s", lb.Name, subnetID, provider)
	res, err := f.create("CreateLoadBalancer", octavia.KindLoadBalancer, lb.Name)
	if err != nil {
		return resource.Resource{}, "", err
	}
	return res, f.provider, nil
}

func (f *recordingOctavia) CreateListener(_ context.Context, l plan.Listener, lbID string) (resource.Resource, error) {
	f.record("CreateListener %s lb=%s", l.Name, lbID)
	return f.create("CreateListener", octavia.KindListener, l.Name)
}

func (f *recordingOctavia) CreatePool(_ context.Context, p plan.Pool, lbID, listenerID, algorithm string) (resource.Resource, error) {
	f.record("CreatePool %s lb=%s listener=%s algorithm=%s", p.Name, lbID, listenerID, algorithm)
	return f.create("CreatePool", octavia.KindPool, p.Name)
}

func (f *recordingOctavia) CreateMember(_ context.Context, m plan.Member, poolID, subnetID string) (resource.Resource, error) {
	f.record("CreateMember %s pool=%s subnet=%s", m.Name, poolID, subnetID)
	return f.create("CreateMember", octavia.KindMember, m.Name)
}

func (f *recordingOctavia) CreateHealthMonitor(_ context.Context, h plan.HealthMonitor, poolID string) (resource.Resource, error) {
	f.record("CreateHealthMonitor %s pool=%s", h.Name, poolID)
	return f.create("CreateHealthMonitor", octavia.KindHealthMonitor, h.Name)
}

func (f *recordingOctavia) WaitForActive(_ context.Context, lb resource.Resource, kind resource.Kind) error {
	f.record("WaitForActive %s %s", lb.ID, kind)
	f.waits++
	if f.waits <= len(f.waitErrs) {
		return f.waitErrs[f.waits-1]
	}
	return nil
}

func (f *recordingOctavia) WaitForActiveAfterDelete(_ context.Context, lb resource.Resource, kind resource.Kind) error {
	f.record("WaitForActiveAfterDelete %s %s", lb.ID, kind)
	return f.errs["WaitForActiveAfterDelete"]
}

func (f *recordingOctavia) WaitForGone(_ context.Context, lb resource.Resource) error {
	f.record("WaitForGone %s", lb.ID)
	return f.errs["WaitForGone"]
}

func (f *recordingOctavia) Delete(_ context.Context, r resource.Resource) error {
	f.record("Delete %s", r.ID)
	return f.errs["Delete"]
}

func (f *recordingOctavia) DeleteSingle(_ context.Context, r resource.Resource) error {
	f.record("DeleteSingle %s", r.ID)
	return f.errs["DeleteSingle"]
}

func (f *recordingOctavia) DeleteMember(_ context.Context, r resource.Resource, poolID string) error {
	f.record("DeleteMember %s pool=%s", r.ID, poolID)
	return f.errs["DeleteMember"]
}

func (f *recordingOctavia) SetListenerAdminState(_ context.Context, r resource.Resource, up bool) error {
	f.record("SetListenerAdminState %s up=%t", r.ID, up)
	return f.errs["SetListenerAdminState"]
}

func (f *recordingOctavia) SetMemberAdminState(_ context.Context, r resource.Resource, poolID string, up bool) error {
	f.record("SetMemberAdminState %s pool=%s up=%t", r.ID, poolID, up)
	if up {
		return f.errs["SetMemberAdminState up"]
	}
	return f.errs["SetMemberAdminState down"]
}

// wantCalls fails the test unless the fake received exactly want, in order.
func (f *recordingOctavia) wantCalls(t *testing.T, want ...string) {
	t.Helper()
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls =\n  %s\nwant\n  %s", strings.Join(f.calls, "\n  "), strings.Join(want, "\n  "))
	}
}

// httpErr returns an error wrapping a gophercloud error that carries the HTTP
// status, the shape the octavia classifiers branch on. The wrapper is a
// pointer, so a test can match the exact error with errors.Is, which the bare
// gophercloud value, holding a byte slice, does not allow.
func httpErr(status int) error {
	return fmt.Errorf("answered %d: %w", status, gophercloud.ErrUnexpectedResponseCode{Actual: status, Body: []byte(`{}`)})
}

// smallPlan generates the plan of the shipped small profile.
func smallPlan(t *testing.T) *plan.Plan {
	t.Helper()
	data, err := scenarios.Files.ReadFile("octavia/small.yaml")
	if err != nil {
		t.Fatalf("reading the small profile: %v", err)
	}
	s, err := octaviascenario.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return p
}

func mustBuild(t *testing.T, p *plan.Plan, c Octavia) []chaos.Node {
	t.Helper()
	nodes, err := Build(p, c, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return nodes
}

// node returns the node keyed key.
func node(t *testing.T, nodes []chaos.Node, key string) chaos.Node {
	t.Helper()
	for _, nd := range nodes {
		if nd.Key == key {
			return nd
		}
	}
	t.Fatalf("no node %q", key)
	return chaos.Node{}
}

// smallIDs is the parent-id map the engine would hand a closure of the small
// plan once every node was created by a recordingOctavia.
func smallIDs(p *plan.Plan) map[string]string {
	ids := map[string]string{}
	add := func(name string) { ids[name] = "id-" + name }
	for _, n := range p.Networks {
		add(n.Name)
		add(n.Subnet)
	}
	for _, lb := range p.LoadBalancers {
		add(lb.Name)
	}
	for _, l := range p.Listeners {
		add(l.Name)
	}
	for _, pl := range p.Pools {
		add(pl.Name)
	}
	for _, m := range p.Members {
		add(m.Name)
	}
	for _, h := range p.HealthMonitors {
		add(h.Name)
	}
	return ids
}

// TestBuildShape confirms the small plan yields one node per cloud object, in
// the order network, subnet, load balancers, listeners, pools, members, health
// monitors, each with its kind and the parents its create needs.
func TestBuildShape(t *testing.T) {
	p := smallPlan(t)
	nodes := mustBuild(t, p, &recordingOctavia{})

	type shape struct {
		key     string
		kind    resource.Kind
		parents []string
	}
	listenerLB := map[string]string{}
	for _, l := range p.Listeners {
		listenerLB[l.Name] = l.LoadBalancer
	}
	poolLB := map[string]string{}
	for _, pl := range p.Pools {
		poolLB[pl.Name] = listenerLB[pl.Listener]
	}
	var want []shape
	for _, n := range p.Networks {
		want = append(want, shape{n.Name, octavia.KindNetwork, nil})
	}
	for _, n := range p.Networks {
		want = append(want, shape{n.Subnet, octavia.KindSubnet, []string{n.Name}})
	}
	for _, lb := range p.LoadBalancers {
		want = append(want, shape{lb.Name, octavia.KindLoadBalancer, []string{"sub-0001"}})
	}
	for _, l := range p.Listeners {
		want = append(want, shape{l.Name, octavia.KindListener, []string{l.LoadBalancer}})
	}
	for _, pl := range p.Pools {
		want = append(want, shape{pl.Name, octavia.KindPool, []string{pl.Listener, poolLB[pl.Name]}})
	}
	for _, m := range p.Members {
		want = append(want, shape{m.Name, octavia.KindMember, []string{m.Pool, poolLB[m.Pool], "sub-0001"}})
	}
	for _, h := range p.HealthMonitors {
		want = append(want, shape{h.Name, octavia.KindHealthMonitor, []string{h.Pool, poolLB[h.Pool]}})
	}

	if len(nodes) != 22 || len(want) != 22 {
		t.Fatalf("Build returned %d nodes (plan implies %d), want 22", len(nodes), len(want))
	}
	counts := map[resource.Kind]int{}
	for i, nd := range nodes {
		counts[nd.Kind]++
		w := want[i]
		if nd.Key != w.key || nd.Kind != w.kind || !slices.Equal(nd.Parents, w.parents) {
			t.Errorf("node %d = {%s %s %v}, want {%s %s %v}", i, nd.Key, nd.Kind, nd.Parents, w.key, w.kind, w.parents)
		}
	}
	wantCounts := map[resource.Kind]int{
		octavia.KindNetwork: 1, octavia.KindSubnet: 1, octavia.KindLoadBalancer: 2, octavia.KindListener: 4,
		octavia.KindPool: 4, octavia.KindMember: 8, octavia.KindHealthMonitor: 2,
	}
	for kind, n := range wantCounts {
		if counts[kind] != n {
			t.Errorf("%d %s nodes, want %d", counts[kind], kind, n)
		}
	}
}

// TestBuildGates confirms every node of one load balancer's family shares that
// load balancer's gate, the two load balancers have different gates, and the
// network and subnet nodes have none.
func TestBuildGates(t *testing.T) {
	p := smallPlan(t)
	nodes := mustBuild(t, p, &recordingOctavia{})

	family := map[string]string{} // node key -> its load balancer
	for _, lb := range p.LoadBalancers {
		family[lb.Name] = lb.Name
	}
	for _, l := range p.Listeners {
		family[l.Name] = l.LoadBalancer
	}
	for _, pl := range p.Pools {
		family[pl.Name] = family[pl.Listener]
	}
	for _, m := range p.Members {
		family[m.Name] = family[m.Pool]
	}
	for _, h := range p.HealthMonitors {
		family[h.Name] = family[h.Pool]
	}

	gates := map[string]chan struct{}{}
	for _, nd := range nodes {
		lb, inFamily := family[nd.Key]
		if !inFamily {
			if nd.Gate != nil {
				t.Errorf("%s node %s has a gate, want none", nd.Kind, nd.Key)
			}
			continue
		}
		if nd.Gate == nil {
			t.Errorf("%s node %s has no gate", nd.Kind, nd.Key)
			continue
		}
		if cap(nd.Gate) != 1 {
			t.Errorf("%s node %s gate capacity = %d, want 1", nd.Kind, nd.Key, cap(nd.Gate))
		}
		if g, ok := gates[lb]; !ok {
			gates[lb] = nd.Gate
		} else if g != nd.Gate {
			t.Errorf("%s node %s does not share the gate of load balancer %s", nd.Kind, nd.Key, lb)
		}
	}
	if len(gates) != 2 || gates["lb-0001"] == gates["lb-0002"] {
		t.Errorf("load balancer gates = %v, want two distinct gates", gates)
	}
}

// TestBuildMutate confirms exactly the listener and member nodes carry the
// admin-state toggle.
func TestBuildMutate(t *testing.T) {
	for _, nd := range mustBuild(t, smallPlan(t), &recordingOctavia{}) {
		want := nd.Kind == octavia.KindListener || nd.Kind == octavia.KindMember
		if (nd.Mutate != nil) != want {
			t.Errorf("%s node %s has Mutate = %t, want %t", nd.Kind, nd.Key, nd.Mutate != nil, want)
		}
	}
}

func TestBuildEmptyPlan(t *testing.T) {
	nodes, err := Build(&plan.Plan{Scenario: "empty"}, &recordingOctavia{}, time.Minute)
	if err != nil {
		t.Fatalf("Build(empty plan) = %v, want nil", err)
	}
	if len(nodes) != 0 {
		t.Errorf("Build(empty plan) returned %d nodes, want none", len(nodes))
	}
}

func TestBuildRejectsInvalidPlan(t *testing.T) {
	p := smallPlan(t)
	p.Listeners[0].LoadBalancer = "lb-9999"
	_, err := Build(p, &recordingOctavia{}, time.Minute)
	if err == nil || !strings.HasPrefix(err.Error(), "invalid plan: ") {
		t.Fatalf("Build(dangling listener) = %v, want an error starting %q", err, "invalid plan: ")
	}
}

// TestNetworkAndSubnetCreate confirms a network create waits for the network to
// be ready, and a subnet create passes its network's id.
func TestNetworkAndSubnetCreate(t *testing.T) {
	p := smallPlan(t)
	f := &recordingOctavia{}
	nodes := mustBuild(t, p, f)
	ids := smallIDs(p)

	if res, err := node(t, nodes, "net-0001").Create(context.Background(), nil); err != nil || res.ID != "id-net-0001" {
		t.Fatalf("network create = (%+v, %v), want id-net-0001", res, err)
	}
	if res, err := node(t, nodes, "sub-0001").Create(context.Background(), ids); err != nil || res.ID != "id-sub-0001" {
		t.Fatalf("subnet create = (%+v, %v), want id-sub-0001", res, err)
	}
	f.wantCalls(t, "CreateNetwork net-0001", "WaitForReady id-net-0001", "CreateSubnet sub-0001 network=id-net-0001")
}

// TestLoadBalancerCreate confirms a load balancer create passes its subnet
// node's id and the plan's provider, then waits for itself with kind
// loadbalancer.
func TestLoadBalancerCreate(t *testing.T) {
	p := smallPlan(t)
	p.Provider = "ovn"
	f := &recordingOctavia{provider: "ovn"}
	nodes := mustBuild(t, p, f)

	res, err := node(t, nodes, "lb-0001").Create(context.Background(), smallIDs(p))
	if err != nil || res.ID != "id-lb-0001" {
		t.Fatalf("load balancer create = (%+v, %v), want id-lb-0001", res, err)
	}
	f.wantCalls(t,
		"CreateLoadBalancer lb-0001 subnet=id-sub-0001 provider=ovn",
		"WaitForActive id-lb-0001 loadbalancer")
}

// TestPoolAlgorithmBinding confirms a pool create gets the algorithm of the
// provider Octavia reported for its load balancer, not the plan's provider.
func TestPoolAlgorithmBinding(t *testing.T) {
	for _, tc := range []struct{ provider, want string }{
		{"ovn", "SOURCE_IP_PORT"},
		{"amphora", "ROUND_ROBIN"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			p := smallPlan(t)
			f := &recordingOctavia{provider: tc.provider}
			nodes := mustBuild(t, p, f)
			ids := smallIDs(p)

			if _, err := node(t, nodes, "lb-0001").Create(context.Background(), ids); err != nil {
				t.Fatalf("load balancer create: %v", err)
			}
			if _, err := node(t, nodes, "pool-0001").Create(context.Background(), ids); err != nil {
				t.Fatalf("pool create: %v", err)
			}
			want := "CreatePool pool-0001 lb=id-lb-0001 listener=id-lsn-0001 algorithm=" + tc.want
			if !slices.Contains(f.calls, want) {
				t.Errorf("calls = %q, want one %q", f.calls, want)
			}
			if last := f.calls[len(f.calls)-1]; last != "WaitForActive id-lb-0001 pool" {
				t.Errorf("last call = %q, want the wait for the pool", last)
			}
		})
	}
}

func TestPoolCreateWithoutBinding(t *testing.T) {
	p := smallPlan(t)
	f := &recordingOctavia{}
	nodes := mustBuild(t, p, f)

	res, err := node(t, nodes, "pool-0001").Create(context.Background(), smallIDs(p))
	if want := `no pool algorithm bound for load balancer "lb-0001"`; err == nil || err.Error() != want {
		t.Fatalf("pool create without binding = %v, want %q", err, want)
	}
	if res != (resource.Resource{}) {
		t.Errorf("pool create without binding returned %+v, want the zero resource", res)
	}
	f.wantCalls(t)
}

// TestMemberCreate confirms a member create passes its pool's id and its load
// balancer's subnet id, then waits with kind member.
func TestMemberCreate(t *testing.T) {
	p := smallPlan(t)
	f := &recordingOctavia{}
	nodes := mustBuild(t, p, f)
	m := p.Members[0]
	lb := node(t, nodes, m.Name).Parents[1]

	if _, err := node(t, nodes, m.Name).Create(context.Background(), smallIDs(p)); err != nil {
		t.Fatalf("member create: %v", err)
	}
	f.wantCalls(t,
		"CreateMember "+m.Name+" pool=id-"+m.Pool+" subnet=id-sub-0001",
		"WaitForActive id-"+lb+" member")
}

func TestListenerCreateError(t *testing.T) {
	p := smallPlan(t)
	boom := httpErr(400)
	f := &recordingOctavia{errs: map[string]error{"CreateListener": boom}}
	nodes := mustBuild(t, p, f)

	res, err := node(t, nodes, "lsn-0001").Create(context.Background(), smallIDs(p))
	if !errors.Is(err, boom) {
		t.Fatalf("listener create = %v, want %v", err, boom)
	}
	if res != (resource.Resource{}) {
		t.Errorf("listener create returned %+v, want the zero resource", res)
	}
	f.wantCalls(t, "CreateListener lsn-0001 lb=id-lb-0001")
}

// TestListenerCreateWaitError confirms a listener whose wait fails is returned
// with the wait's error, so the engine keeps it deletable.
func TestListenerCreateWaitError(t *testing.T) {
	p := smallPlan(t)
	failed := fmt.Errorf("%w: load balancer id-lb-0001 reached ERROR after a listener write", octavia.ErrProvisioning)
	f := &recordingOctavia{waitErrs: []error{failed}}
	nodes := mustBuild(t, p, f)

	res, err := node(t, nodes, "lsn-0001").Create(context.Background(), smallIDs(p))
	if !errors.Is(err, octavia.ErrProvisioning) {
		t.Fatalf("listener create = %v, want ErrProvisioning", err)
	}
	if res.ID != "id-lsn-0001" {
		t.Errorf("listener create returned %+v, want the created listener", res)
	}
}

func TestMemberDelete(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantErr  bool
		wantWait bool
	}{
		{"deleted then waited", nil, false, true},
		{"already gone still waits", httpErr(404), false, true},
		{"refused returns without wait", httpErr(400), true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := smallPlan(t)
			f := &recordingOctavia{errs: map[string]error{"DeleteMember": tc.err}}
			nodes := mustBuild(t, p, f)
			m := p.Members[0]
			lb := node(t, nodes, m.Name).Parents[1]

			err := node(t, nodes, m.Name).Delete(context.Background(), smallIDs(p), created(octavia.KindMember, m.Name))
			if (err != nil) != tc.wantErr {
				t.Fatalf("member delete = %v, want error %t", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, tc.err) {
				t.Errorf("member delete = %v, want %v", err, tc.err)
			}
			want := []string{"DeleteMember id-" + m.Name + " pool=id-" + m.Pool}
			if tc.wantWait {
				want = append(want, "WaitForActiveAfterDelete id-"+lb+" member")
			}
			f.wantCalls(t, want...)
		})
	}
}

// TestChildDeleteOfGoneLoadBalancer confirms a child delete whose load balancer
// is gone counts as deleted. The engine retries a failed delete with the parent
// ids it first used, so the retry can name a load balancer instance the churn
// has deleted since; a child cannot outlive its load balancer, so the 404 the
// wait then finds proves the child gone too.
func TestChildDeleteOfGoneLoadBalancer(t *testing.T) {
	p := smallPlan(t)
	gone := fmt.Errorf("load balancer id-lb-0001 is gone while waiting for ACTIVE: %w", httpErr(404))
	f := &recordingOctavia{errs: map[string]error{"DeleteMember": httpErr(404), "WaitForActiveAfterDelete": gone}}
	nodes := mustBuild(t, p, f)
	m := p.Members[0]

	if err := node(t, nodes, m.Name).Delete(context.Background(), smallIDs(p), created(octavia.KindMember, m.Name)); err != nil {
		t.Fatalf("member delete under a gone load balancer = %v, want nil", err)
	}
}

// TestChildDeleteWaitError confirms any other failure of the wait after a child
// delete fails the delete.
func TestChildDeleteWaitError(t *testing.T) {
	p := smallPlan(t)
	failed := fmt.Errorf("%w: load balancer id-lb-0001 reached ERROR after a listener write", octavia.ErrProvisioning)
	f := &recordingOctavia{errs: map[string]error{"WaitForActiveAfterDelete": failed}}
	nodes := mustBuild(t, p, f)

	err := node(t, nodes, "lsn-0001").Delete(context.Background(), smallIDs(p), created(octavia.KindListener, "lsn-0001"))
	if !errors.Is(err, octavia.ErrProvisioning) {
		t.Fatalf("listener delete = %v, want ErrProvisioning", err)
	}
}

// TestChildDeletesUseSingleDeletes confirms a listener, pool, or health monitor
// is deleted through DeleteSingle and waited on without a time-to-ready.
func TestChildDeletesUseSingleDeletes(t *testing.T) {
	p := smallPlan(t)
	for _, tc := range []struct {
		key  string
		kind resource.Kind
	}{
		{"lsn-0001", octavia.KindListener},
		{"pool-0001", octavia.KindPool},
		{p.HealthMonitors[0].Name, octavia.KindHealthMonitor},
	} {
		f := &recordingOctavia{}
		nodes := mustBuild(t, p, f)
		nd := node(t, nodes, tc.key)
		lb := nd.Parents[len(nd.Parents)-1]
		if err := nd.Delete(context.Background(), smallIDs(p), created(tc.kind, tc.key)); err != nil {
			t.Fatalf("%s delete: %v", tc.kind, err)
		}
		f.wantCalls(t, "DeleteSingle id-"+tc.key, "WaitForActiveAfterDelete id-"+lb+" "+string(tc.kind))
	}
}

func TestLoadBalancerDelete(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr bool
		want    []string
	}{
		{"deleted then waited until gone", nil, false, []string{"DeleteSingle id-lb-0001", "WaitForGone id-lb-0001"}},
		{"refused while it has children", httpErr(400), true, []string{"DeleteSingle id-lb-0001"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := smallPlan(t)
			f := &recordingOctavia{errs: map[string]error{"DeleteSingle": tc.err}}
			nodes := mustBuild(t, p, f)

			err := node(t, nodes, "lb-0001").Delete(context.Background(), smallIDs(p), created(octavia.KindLoadBalancer, "lb-0001"))
			if (err != nil) != tc.wantErr {
				t.Fatalf("load balancer delete = %v, want error %t", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, tc.err) {
				t.Errorf("load balancer delete = %v, want %v", err, tc.err)
			}
			f.wantCalls(t, tc.want...)
		})
	}
}

// TestNeutronDeletes confirms a subnet and a network are deleted through
// Delete, without a wait, and a 404 there counts as deleted.
func TestNeutronDeletes(t *testing.T) {
	for _, delErr := range []error{nil, httpErr(404)} {
		p := smallPlan(t)
		f := &recordingOctavia{errs: map[string]error{"Delete": delErr}}
		nodes := mustBuild(t, p, f)

		if err := node(t, nodes, "sub-0001").Delete(context.Background(), smallIDs(p), created(octavia.KindSubnet, "sub-0001")); err != nil {
			t.Errorf("subnet delete with %v = %v, want nil", delErr, err)
		}
		if err := node(t, nodes, "net-0001").Delete(context.Background(), nil, created(octavia.KindNetwork, "net-0001")); err != nil {
			t.Errorf("network delete with %v = %v, want nil", delErr, err)
		}
		f.wantCalls(t, "Delete id-sub-0001", "Delete id-net-0001")
	}
}

func TestMemberToggle(t *testing.T) {
	p := smallPlan(t)
	f := &recordingOctavia{}
	nodes := mustBuild(t, p, f)
	m := p.Members[0]
	lb := node(t, nodes, m.Name).Parents[1]

	if err := node(t, nodes, m.Name).Mutate(context.Background(), smallIDs(p), created(octavia.KindMember, m.Name)); err != nil {
		t.Fatalf("member toggle: %v", err)
	}
	f.wantCalls(t,
		"SetMemberAdminState id-"+m.Name+" pool=id-"+m.Pool+" up=false",
		"WaitForActive id-"+lb+" member",
		"SetMemberAdminState id-"+m.Name+" pool=id-"+m.Pool+" up=true",
		"WaitForActive id-"+lb+" member")
}

func TestMemberToggleDisableFails(t *testing.T) {
	p := smallPlan(t)
	boom := httpErr(400)
	f := &recordingOctavia{errs: map[string]error{"SetMemberAdminState down": boom}}
	nodes := mustBuild(t, p, f)
	m := p.Members[0]

	err := node(t, nodes, m.Name).Mutate(context.Background(), smallIDs(p), created(octavia.KindMember, m.Name))
	if !errors.Is(err, boom) {
		t.Fatalf("member toggle = %v, want %v", err, boom)
	}
	f.wantCalls(t, "SetMemberAdminState id-"+m.Name+" pool=id-"+m.Pool+" up=false")
}

func TestListenerToggleSecondWaitFails(t *testing.T) {
	p := smallPlan(t)
	failed := fmt.Errorf("%w: load balancer id-lb-0001 reached ERROR after a listener write", octavia.ErrProvisioning)
	f := &recordingOctavia{waitErrs: []error{nil, failed}}
	nodes := mustBuild(t, p, f)

	err := node(t, nodes, "lsn-0001").Mutate(context.Background(), smallIDs(p), created(octavia.KindListener, "lsn-0001"))
	if !errors.Is(err, octavia.ErrProvisioning) {
		t.Fatalf("listener toggle = %v, want ErrProvisioning", err)
	}
	f.wantCalls(t,
		"SetListenerAdminState id-lsn-0001 up=false",
		"WaitForActive id-lb-0001 listener",
		"SetListenerAdminState id-lsn-0001 up=true",
		"WaitForActive id-lb-0001 listener")
}

func TestClassify(t *testing.T) {
	provisioningTimeout := fmt.Errorf("%w: %w", octavia.ErrProvisioning, context.DeadlineExceeded)
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"provisioning error over a deadline", provisioningTimeout, "provisioning_error"},
		{"canceled", context.Canceled, "canceled"},
		{"deadline", fmt.Errorf("waiting: %w", context.DeadlineExceeded), "timeout"},
		{"quota", fmt.Errorf("creating listener %q: %w: %w", "lsn-0001", octavia.ErrQuota, httpErr(403)), "quota"},
		{"not found", httpErr(404), "not-found"},
		{"conflict", httpErr(409), "transient"},
		{"bad request", httpErr(400), "other"},
	}
	for _, tc := range tests {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("%s: Classify(%v) = %q, want %q", tc.name, tc.err, got, tc.want)
		}
	}
}
