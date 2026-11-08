package octaviagraph

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/nova"
	"github.com/B42Labs/dizzy/internal/octavia"
	"github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// smallFrontend is the frontend the mix generator builds for the Gardener
// persona of the small profile: one cluster net-0001 with the workers srv-0001
// and srv-0002, one load balancer on its network with listeners on ports 80
// and 443, a pool of both workers behind each listener, and a health monitor
// per pool.
func smallFrontend() *plan.Frontend {
	return &plan.Frontend{
		LoadBalancers: []plan.LoadBalancer{{Name: "lb-0001", Network: "net-0001"}},
		Listeners: []plan.Listener{
			{Name: "lsn-0001", LoadBalancer: "lb-0001", Port: 80},
			{Name: "lsn-0002", LoadBalancer: "lb-0001", Port: 443},
		},
		Pools: []plan.Pool{
			{Name: "pool-0001", Listener: "lsn-0001"},
			{Name: "pool-0002", Listener: "lsn-0002"},
		},
		Members: []plan.ServerMember{
			{Name: "pool-0001-member-01", Pool: "pool-0001", Server: "srv-0001", Port: 30000},
			{Name: "pool-0001-member-02", Pool: "pool-0001", Server: "srv-0002", Port: 30000},
			{Name: "pool-0002-member-01", Pool: "pool-0002", Server: "srv-0001", Port: 30001},
			{Name: "pool-0002-member-02", Pool: "pool-0002", Server: "srv-0002", Port: 30001},
		},
		HealthMonitors: []plan.HealthMonitor{
			{Name: "hm-0001", Pool: "pool-0001"},
			{Name: "hm-0002", Pool: "pool-0002"},
		},
	}
}

// smallRoll maps the workers of the small frontend's cluster to their rolling
// set, as the rolling graph of the Gardener persona carries them.
func smallRoll() map[string]string {
	return map[string]string{"srv-0001": "grp-0001", "srv-0002": "grp-0001"}
}

func mustBuildFrontend(t *testing.T, f *plan.Frontend, c FrontendOctavia) []chaos.Node {
	t.Helper()
	nodes, err := BuildFrontend(f, c, smallRoll(), time.Minute)
	if err != nil {
		t.Fatalf("BuildFrontend: %v", err)
	}
	return nodes
}

// recordingFrontend is a recordingOctavia with the two lookups of
// FrontendOctavia. It answers NetworkSubnet with "id-sub-of-<network id>" and
// ServerAddress with 10.0.1.5 on that subnet, or with the error configured for
// the method, and its CreateMember records the address and port too.
type recordingFrontend struct{ *recordingOctavia }

func (f recordingFrontend) NetworkSubnet(_ context.Context, networkID string) (string, error) {
	f.record("NetworkSubnet %s", networkID)
	if err := f.errs["NetworkSubnet"]; err != nil {
		return "", err
	}
	return "id-sub-of-" + networkID, nil
}

func (f recordingFrontend) ServerAddress(_ context.Context, serverID, networkID string) (string, string, error) {
	f.record("ServerAddress %s network=%s", serverID, networkID)
	if err := f.errs["ServerAddress"]; err != nil {
		return "", "", err
	}
	return "10.0.1.5", "id-sub-of-" + networkID, nil
}

func (f recordingFrontend) CreateMember(_ context.Context, m plan.Member, poolID, subnetID string) (resource.Resource, error) {
	f.record("CreateMember %s pool=%s address=%s port=%d subnet=%s", m.Name, poolID, m.Address, m.Port, subnetID)
	return f.create("CreateMember", octavia.KindMember, m.Name)
}

// frontendIDs is the parent-id map the engine would hand a closure of the
// small frontend once its cluster and every frontend node were created by a
// recordingFrontend.
func frontendIDs(f *plan.Frontend) map[string]string {
	ids := map[string]string{"net-0001": "id-net-0001", "srv-0001": "id-srv-0001", "srv-0002": "id-srv-0002"}
	add := func(name string) { ids[name] = "id-" + name }
	for _, lb := range f.LoadBalancers {
		add(lb.Name)
	}
	for _, l := range f.Listeners {
		add(l.Name)
	}
	for _, pl := range f.Pools {
		add(pl.Name)
	}
	for _, m := range f.Members {
		add(m.Name)
	}
	for _, h := range f.HealthMonitors {
		add(h.Name)
	}
	return ids
}

// TestBuildFrontendShape confirms the small frontend yields, in this order,
// pinned load balancer, listener, pool, and health monitor nodes without a
// mutation and with the parents of their Build counterparts, the load balancer
// parented on its network, and then a rolling member node per pool and worker
// parented on its pool, load balancer, worker, and the load balancer's network,
// every node on the load balancer's gate.
func TestBuildFrontendShape(t *testing.T) {
	nodes := mustBuildFrontend(t, smallFrontend(), recordingFrontend{&recordingOctavia{}})

	type shape struct {
		key     string
		kind    resource.Kind
		parents []string
		pinned  bool
		roll    string
	}
	want := []shape{
		{"lb-0001", octavia.KindLoadBalancer, []string{"net-0001"}, true, ""},
		{"lsn-0001", octavia.KindListener, []string{"lb-0001"}, true, ""},
		{"lsn-0002", octavia.KindListener, []string{"lb-0001"}, true, ""},
		{"pool-0001", octavia.KindPool, []string{"lsn-0001", "lb-0001"}, true, ""},
		{"pool-0002", octavia.KindPool, []string{"lsn-0002", "lb-0001"}, true, ""},
		{"hm-0001", octavia.KindHealthMonitor, []string{"pool-0001", "lb-0001"}, true, ""},
		{"hm-0002", octavia.KindHealthMonitor, []string{"pool-0002", "lb-0001"}, true, ""},
		{"pool-0001-member-01", octavia.KindMember, []string{"pool-0001", "lb-0001", "srv-0001", "net-0001"}, false, "grp-0001"},
		{"pool-0001-member-02", octavia.KindMember, []string{"pool-0001", "lb-0001", "srv-0002", "net-0001"}, false, "grp-0001"},
		{"pool-0002-member-01", octavia.KindMember, []string{"pool-0002", "lb-0001", "srv-0001", "net-0001"}, false, "grp-0001"},
		{"pool-0002-member-02", octavia.KindMember, []string{"pool-0002", "lb-0001", "srv-0002", "net-0001"}, false, "grp-0001"},
	}
	if len(nodes) != len(want) {
		t.Fatalf("BuildFrontend returned %d nodes, want %d", len(nodes), len(want))
	}
	gate := nodes[0].Gate
	if gate == nil || cap(gate) != 1 {
		t.Fatalf("load balancer gate = %v, want a capacity-1 channel", gate)
	}
	for i, nd := range nodes {
		w := want[i]
		if nd.Key != w.key || nd.Kind != w.kind || !slices.Equal(nd.Parents, w.parents) || nd.Pinned != w.pinned || nd.Roll != w.roll {
			t.Errorf("node %d = {%s %s %v pinned=%t roll=%q}, want {%s %s %v pinned=%t roll=%q}",
				i, nd.Key, nd.Kind, nd.Parents, nd.Pinned, nd.Roll, w.key, w.kind, w.parents, w.pinned, w.roll)
		}
		if nd.Gate != gate {
			t.Errorf("%s node %s is not on the gate of load balancer lb-0001", nd.Kind, nd.Key)
		}
		if nd.Mutate != nil {
			t.Errorf("%s node %s has a Mutate, want none", nd.Kind, nd.Key)
		}
		if nd.Create == nil || nd.Delete == nil {
			t.Errorf("%s node %s lacks a Create or Delete", nd.Kind, nd.Key)
		}
	}
}

func TestBuildFrontendRejects(t *testing.T) {
	invalid := smallFrontend()
	invalid.LoadBalancers[0].Network = ""
	tests := []struct {
		name   string
		f      *plan.Frontend
		roll   map[string]string
		prefix string
		want   string
	}{
		{name: "invalid frontend", f: invalid, roll: smallRoll(), prefix: "invalid frontend: "},
		{
			name: "server without rolling set", f: smallFrontend(), roll: map[string]string{"srv-0002": "grp-0001"},
			want: `member "pool-0001-member-01" references server "srv-0001", which has no rolling set`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nodes, err := BuildFrontend(tc.f, recordingFrontend{&recordingOctavia{}}, tc.roll, time.Minute)
			if err == nil {
				t.Fatalf("BuildFrontend = %d nodes, want an error", len(nodes))
			}
			if tc.prefix != "" && !strings.HasPrefix(err.Error(), tc.prefix) {
				t.Errorf("BuildFrontend = %v, want an error starting %q", err, tc.prefix)
			}
			if tc.want != "" && err.Error() != tc.want {
				t.Errorf("BuildFrontend = %v, want %q", err, tc.want)
			}
			if nodes != nil {
				t.Errorf("BuildFrontend returned %d nodes with its error, want none", len(nodes))
			}
		})
	}
}

// TestBuildFrontendEmpty confirms a nil frontend and one without any object
// yield no node and no error, whatever roll holds.
func TestBuildFrontendEmpty(t *testing.T) {
	for name, f := range map[string]*plan.Frontend{
		"nil":   nil,
		"zero":  {},
		"empty": {LoadBalancers: []plan.LoadBalancer{}, Listeners: []plan.Listener{}, Pools: []plan.Pool{}, Members: []plan.ServerMember{}, HealthMonitors: []plan.HealthMonitor{}},
	} {
		nodes, err := BuildFrontend(f, recordingFrontend{&recordingOctavia{}}, nil, time.Minute)
		if err != nil || nodes != nil {
			t.Errorf("%s: BuildFrontend = (%d nodes, %v), want (nil, nil)", name, len(nodes), err)
		}
	}
}

// TestFrontendLoadBalancerCreate confirms a load balancer create reads the
// subnet of its network's cloud id first and creates the load balancer on that
// subnet with the frontend's provider, and that a failed lookup returns the
// zero resource and the lookup's error without a create.
func TestFrontendLoadBalancerCreate(t *testing.T) {
	noSubnet := errors.New("network id-net-0001 has no subnet")
	tests := []struct {
		name   string
		err    error
		wantID string
		want   []string
	}{
		{"created on the looked-up subnet", nil, "id-lb-0001", []string{
			"NetworkSubnet id-net-0001",
			"CreateLoadBalancer lb-0001 subnet=id-sub-of-id-net-0001 provider=ovn",
			"WaitForActive id-lb-0001 loadbalancer",
		}},
		{"failed lookup", noSubnet, "", []string{"NetworkSubnet id-net-0001"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fr := smallFrontend()
			fr.Provider = "ovn"
			f := &recordingOctavia{provider: "ovn", errs: map[string]error{"NetworkSubnet": tc.err}}
			nodes := mustBuildFrontend(t, fr, recordingFrontend{f})

			res, err := node(t, nodes, "lb-0001").Create(context.Background(), frontendIDs(fr))
			if !errors.Is(err, tc.err) {
				t.Fatalf("load balancer create = %v, want %v", err, tc.err)
			}
			if res.ID != tc.wantID {
				t.Errorf("load balancer create returned %+v, want id %q", res, tc.wantID)
			}
			f.wantCalls(t, tc.want...)
		})
	}
}

// TestFrontendMemberCreate confirms a member create reads the address of its
// server's cloud id on its load balancer's network first and creates the
// member at that address on the member's port and on the subnet the lookup
// returned, and that a failed lookup returns the zero resource and the
// lookup's error without a create.
func TestFrontendMemberCreate(t *testing.T) {
	noAddress := errors.New("server id-srv-0002 has no IPv4 address on network id-net-0001")
	tests := []struct {
		name   string
		err    error
		wantID string
		want   []string
	}{
		{"created at the looked-up address", nil, "id-pool-0002-member-02", []string{
			"ServerAddress id-srv-0002 network=id-net-0001",
			"CreateMember pool-0002-member-02 pool=id-pool-0002 address=10.0.1.5 port=30001 subnet=id-sub-of-id-net-0001",
			"WaitForActive id-lb-0001 member",
		}},
		{"failed lookup", noAddress, "", []string{"ServerAddress id-srv-0002 network=id-net-0001"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fr := smallFrontend()
			f := &recordingOctavia{errs: map[string]error{"ServerAddress": tc.err}}
			nodes := mustBuildFrontend(t, fr, recordingFrontend{f})

			res, err := node(t, nodes, "pool-0002-member-02").Create(context.Background(), frontendIDs(fr))
			if !errors.Is(err, tc.err) {
				t.Fatalf("member create = %v, want %v", err, tc.err)
			}
			if res.ID != tc.wantID {
				t.Errorf("member create returned %+v, want id %q", res, tc.wantID)
			}
			f.wantCalls(t, tc.want...)
		})
	}
}

// TestFrontendDeletes confirms a member is deleted through its pool and a
// listener, pool, or health monitor through DeleteSingle, each followed by the
// wait for the load balancer, and a load balancer through DeleteSingle and the
// wait until it is gone.
func TestFrontendDeletes(t *testing.T) {
	fr := smallFrontend()
	for _, tc := range []struct {
		key  string
		kind resource.Kind
		want []string
	}{
		{"pool-0001-member-01", octavia.KindMember, []string{"DeleteMember id-pool-0001-member-01 pool=id-pool-0001", "WaitForActiveAfterDelete id-lb-0001 member"}},
		{"lsn-0002", octavia.KindListener, []string{"DeleteSingle id-lsn-0002", "WaitForActiveAfterDelete id-lb-0001 listener"}},
		{"pool-0002", octavia.KindPool, []string{"DeleteSingle id-pool-0002", "WaitForActiveAfterDelete id-lb-0001 pool"}},
		{"hm-0001", octavia.KindHealthMonitor, []string{"DeleteSingle id-hm-0001", "WaitForActiveAfterDelete id-lb-0001 healthmonitor"}},
		{"lb-0001", octavia.KindLoadBalancer, []string{"DeleteSingle id-lb-0001", "WaitForGone id-lb-0001"}},
	} {
		f := &recordingOctavia{}
		nodes := mustBuildFrontend(t, fr, recordingFrontend{f})
		if err := node(t, nodes, tc.key).Delete(context.Background(), frontendIDs(fr), created(tc.kind, tc.key)); err != nil {
			t.Fatalf("%s delete: %v", tc.kind, err)
		}
		f.wantCalls(t, tc.want...)
	}
}

// clusterNodes returns the nodes of the rolling graph the small frontend
// joins, backed by f, as the rolling graph of the Gardener persona emits them:
// the pinned cluster network net-0001 and the workers srv-0001 and srv-0002 on
// it, both in the rolling set grp-0001.
func clusterNodes(f *fakeOctavia) []chaos.Node {
	nodes := []chaos.Node{{
		Key: "net-0001", Kind: nova.KindNetwork, Pinned: true,
		Create: func(context.Context, map[string]string) (resource.Resource, error) {
			return f.addNetwork("net-0001", "sub-0001"), nil
		},
		Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
			return f.Delete(ctx, res) // never runs: the network is pinned
		},
	}}
	for _, srv := range []string{"srv-0001", "srv-0002"} {
		nodes = append(nodes, chaos.Node{
			Key: srv, Kind: nova.KindServer, Parents: []string{"net-0001"}, Roll: "grp-0001",
			Create: func(_ context.Context, ids map[string]string) (resource.Resource, error) {
				return f.bootServer(srv, ids["net-0001"]), nil
			},
			Delete: func(_ context.Context, _ map[string]string, res resource.Resource) error {
				f.deleteServer(res)
				return nil
			},
		})
	}
	return nodes
}

// frontendConfig is the churn config of a rolling persona, as the mix command
// gives one: target fill 1 and no mutation, run long enough on the fake clock
// that each worker is replaced several times.
func frontendConfig() chaos.Config {
	return chaos.Config{
		Duration:    2 * time.Second,
		MinInterval: 10 * time.Millisecond,
		MaxInterval: 40 * time.Millisecond,
		MaxParallel: 4,
		ChurnRatio:  0.5,
		TargetFill:  1,
		ResizeRatio: 0,
		Concurrency: 8,
		Classify:    Classify,
	}
}

// runFrontend runs a churn over the cluster nodes and the small frontend's
// nodes on f, the cluster nodes first, as the mix command appends a frontend
// to the persona's graph.
func runFrontend(t *testing.T, f *fakeOctavia) *chaos.Result {
	t.Helper()
	nodes := append(clusterNodes(f), mustBuildFrontend(t, smallFrontend(), f)...)
	r, err := chaos.Run(context.Background(), nodes, 7, frontendConfig(), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return r
}

// membersOf maps each worker of the small frontend to its members, in plan
// order.
func membersOf() map[string][]string {
	out := map[string][]string{}
	for _, m := range smallFrontend().Members {
		out[m.Server] = append(out[m.Server], m.Name)
	}
	return out
}

// failedOutcomes tallies the failed outcomes of r by error label.
func failedOutcomes(r *chaos.Result) map[string]int {
	out := map[string]int{}
	for _, b := range r.Buckets {
		for _, e := range b.Errors {
			out[e.Kind] += e.Count
		}
	}
	return out
}

// decided counts the decisions of r with action on a node of kind.
func decided(r *chaos.Result, action string, kind resource.Kind) int {
	n := 0
	for _, d := range r.Decisions {
		if d.Action == action && d.Kind == kind {
			n++
		}
	}
	return n
}

// checkViolations fails the test on every invariant violation f recorded. The
// caller holds f.mu.
func checkViolations(t *testing.T, f *fakeOctavia) {
	t.Helper()
	for _, v := range f.violations {
		t.Error(v)
	}
}

// TestRunFrontendRollOrder confirms every roll of a worker decides the deletes
// of the worker's members directly before the worker's delete and at its
// offset, and that the next decisions create the worker again and then its
// members.
func TestRunFrontendRollOrder(t *testing.T) {
	r := runFrontend(t, newFakeOctavia())
	if r.Deletes == 0 {
		t.Fatal("the run deleted nothing; the test exercises nothing")
	}
	members := membersOf()
	rolls := map[string]int{}
	for i, d := range r.Decisions {
		if d.Action != "delete" || d.Kind != nova.KindServer {
			continue
		}
		rolls[d.Key]++
		ms := members[d.Key]
		if i < len(ms) {
			t.Errorf("decision %d deletes %s with fewer than %d decisions before it", i, d.Key, len(ms))
			continue
		}
		var deleted []string
		for _, md := range r.Decisions[i-len(ms) : i] {
			if md.Action == "delete" && md.Kind == octavia.KindMember && md.Offset == d.Offset {
				deleted = append(deleted, md.Key)
			}
		}
		slices.Sort(deleted)
		if !slices.Equal(deleted, ms) {
			t.Errorf("the roll of %s at %s deleted the members %v directly before it, want %v", d.Key, d.Offset, deleted, ms)
		}

		// The worker's create comes next and then its members' creates, in
		// either order. A roll at the very end of the run is followed by fewer
		// decisions.
		pending := map[string]bool{}
		for _, m := range ms {
			pending["create "+m] = true
		}
		for j, nd := range r.Decisions[i+1 : min(i+2+len(ms), len(r.Decisions))] {
			got := nd.Action + " " + nd.Key
			switch {
			case j == 0 && got != "create "+d.Key:
				t.Errorf("the roll of %s at %s is followed by %s, want the worker's create", d.Key, d.Offset, got)
			case j > 0 && !pending[got]:
				t.Errorf("after the worker's create, the roll of %s at %s decided %s, want the create of one of its members %v", d.Key, d.Offset, got, ms)
			}
			delete(pending, got)
		}
	}
	for _, srv := range []string{"srv-0001", "srv-0002"} {
		if rolls[srv] < 2 {
			t.Errorf("%s was rolled %d times, want several", srv, rolls[srv])
		}
	}
}

// TestRunFrontendInvariants confirms, over a churn of the small frontend
// joined to its cluster, that the fake never sees a member's address read or
// the member created before its worker's create returned, a worker deleted
// before the delete of each of its members returned, or two operations of the
// load balancer at once; that the load balancer, listeners, pools, and health
// monitors are created once and never deleted; and that every load balancer
// and member create passes the subnet and address the lookups returned for
// the cluster network and the worker's current instance.
func TestRunFrontendInvariants(t *testing.T) {
	f := newFakeOctavia()
	f.delay = 100 * time.Microsecond // widen the windows a misordered call would hit
	r := runFrontend(t, f)
	if r.Deletes == 0 {
		t.Fatal("the run deleted nothing; the test exercises nothing")
	}
	if failed := failedOutcomes(r); len(failed) != 0 {
		t.Errorf("failed outcomes = %v, want none", failed)
	}
	for _, d := range r.Decisions {
		switch d.Kind {
		case octavia.KindLoadBalancer, octavia.KindListener, octavia.KindPool, octavia.KindHealthMonitor:
			if d.Action != "create" {
				t.Errorf("decision at %s: %s %s %s, want only its create", d.Offset, d.Action, d.Kind, d.Key)
			}
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	checkViolations(t, f)

	kept := map[resource.Kind]int{}
	for id, o := range f.objects {
		switch o.kind {
		case octavia.KindLoadBalancer, octavia.KindListener, octavia.KindPool, octavia.KindHealthMonitor:
			kept[o.kind]++
			if !o.live {
				t.Errorf("%s %s (%s) was deleted", o.kind, o.logical, id)
			}
		}
	}
	wantKept := map[resource.Kind]int{
		octavia.KindLoadBalancer: 1, octavia.KindListener: 2, octavia.KindPool: 2, octavia.KindHealthMonitor: 2,
	}
	if !reflect.DeepEqual(kept, wantKept) {
		t.Errorf("frontend objects created = %v, want each once: %v", kept, wantKept)
	}

	if len(f.subnetOf) != 1 {
		t.Fatalf("the fake holds %d networks, want the one cluster network", len(f.subnetOf))
	}
	var subnet string
	for _, s := range f.subnetOf {
		subnet = s
	}
	if !slices.Equal(f.lbSubnets, []string{subnet}) {
		t.Errorf("load balancer creates got the subnets %v, want once the cluster subnet %s", f.lbSubnets, subnet)
	}

	if want := decided(r, "create", octavia.KindMember); len(f.memberCreates) != want {
		t.Errorf("%d member creates reached the fake at a server's address, want one per member create decision (%d)", len(f.memberCreates), want)
	}
	server := map[string]string{}
	for _, m := range smallFrontend().Members {
		server[m.Name] = m.Server
	}
	for _, mc := range f.memberCreates {
		if got := f.objects[mc.server].logical; got != server[mc.member] {
			t.Errorf("member %s created at %s, the address of %s (%s), want an instance of %s", mc.member, mc.address, got, mc.server, server[mc.member])
		}
		if mc.subnet != subnet {
			t.Errorf("member %s created on subnet %s, want the cluster subnet %s that ServerAddress returned", mc.member, mc.subnet, subnet)
		}
	}
}

// TestRunFrontendRetriesAddressLookup confirms a member whose address lookup
// fails once with a 500 is retried within its create and created: the run
// records no failed outcome, and every member create decision reaches the
// fake.
func TestRunFrontendRetriesAddressLookup(t *testing.T) {
	f := newFakeOctavia()
	f.addressErrs["srv-0001"] = []error{httpErr(500)}
	r := runFrontend(t, f)

	if failed := failedOutcomes(r); len(failed) != 0 {
		t.Errorf("failed outcomes = %v, want none", failed)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	checkViolations(t, f)
	i := slices.Index(f.serverLog, "address srv-0001 failed")
	if i < 0 || len(f.addressErrs["srv-0001"]) != 0 {
		t.Fatalf("server log = %v, want the first address lookup of srv-0001 to fail", f.serverLog)
	}
	if i+1 == len(f.serverLog) || f.serverLog[i+1] != "address srv-0001 ok" {
		t.Errorf("server log = %v, want the failed lookup of srv-0001 retried at once", f.serverLog)
	}
	if want := decided(r, "create", octavia.KindMember); len(f.memberCreates) != want {
		t.Errorf("%d member creates reached the fake, want one per member create decision (%d)", len(f.memberCreates), want)
	}
}

// TestRunFrontendAddressLookupFailure confirms a member whose address lookup
// finds no IPv4 address counts as one failed create, while the rolls go on:
// its worker is replaced later and the worker's members read its address
// again, and every other member create decision reaches the fake.
func TestRunFrontendAddressLookupFailure(t *testing.T) {
	f := newFakeOctavia()
	f.addressErrs["srv-0001"] = []error{fmt.Errorf("server %s has no IPv4 address on network %s", "srv-0001", "net-0001")}
	r := runFrontend(t, f)

	if failed, want := failedOutcomes(r), map[string]int{"other": 1}; !reflect.DeepEqual(failed, want) {
		t.Errorf("failed outcomes = %v, want %v", failed, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	checkViolations(t, f)
	i := slices.Index(f.serverLog, "address srv-0001 failed")
	if i < 0 {
		t.Fatalf("server log = %v, want an address lookup of srv-0001 to fail", f.serverLog)
	}
	rolled := slices.Index(f.serverLog[i:], "delete srv-0001")
	if rolled < 0 || !slices.Contains(f.serverLog[i+rolled:], "address srv-0001 ok") {
		t.Errorf("server log = %v, want srv-0001 replaced after the failed lookup and its members created again", f.serverLog)
	}
	if want := decided(r, "create", octavia.KindMember) - 1; len(f.memberCreates) != want {
		t.Errorf("%d member creates reached the fake, want one per member create decision but the failed one (%d)", len(f.memberCreates), want)
	}
}

func TestRunFrontendDeterministicSchedule(t *testing.T) {
	r1 := runFrontend(t, newFakeOctavia())
	r2 := runFrontend(t, newFakeOctavia())
	if len(r1.Decisions) == 0 {
		t.Fatal("no decisions were scheduled")
	}
	if !reflect.DeepEqual(r1.Decisions, r2.Decisions) {
		t.Errorf("decision schedules differ for the same seed and config: %d vs %d decisions", len(r1.Decisions), len(r2.Decisions))
	}
}
