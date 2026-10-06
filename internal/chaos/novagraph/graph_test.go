package novagraph

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/nova"
	novaexec "github.com/B42Labs/dizzy/internal/nova/executor"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

func nodeByKey(t *testing.T, nodes []chaos.Node, key string) chaos.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Key == key {
			return n
		}
	}
	t.Fatalf("no node with key %q in %d nodes", key, len(nodes))
	return chaos.Node{}
}

// TestBuildShape confirms the graph is one node per network, server, volume, and
// port; that a server is parented on its networks, a volume on its server, and a
// port on its server and network; that Mutate is set exactly for the servers
// with a planned lifecycle operation; and that a server and its volume/port
// children share one per-server gate.
func TestBuildShape(t *testing.T) {
	p := churnPlan()
	nodes, err := Build(p, newFakeNova(), novaexec.Resolved{LiveMigration: true}, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := len(p.Networks) + len(p.Servers) + len(p.Volumes) + len(p.Ports)
	if len(nodes) != want {
		t.Fatalf("built %d nodes, want %d", len(nodes), want)
	}

	for _, n := range p.Networks {
		node := nodeByKey(t, nodes, n.Name)
		if node.Kind != nova.KindNetwork || len(node.Parents) != 0 || node.Mutate != nil {
			t.Errorf("network node %q: kind=%q parents=%v mutable=%v, want network/none/immutable", n.Name, node.Kind, node.Parents, node.Mutate != nil)
		}
	}

	for _, s := range p.Servers {
		node := nodeByKey(t, nodes, s.Name)
		if node.Kind != nova.KindServer {
			t.Errorf("server node %q kind = %q, want server", s.Name, node.Kind)
		}
		got := append([]string(nil), node.Parents...)
		sort.Strings(got)
		wantParents := append([]string(nil), s.Networks...)
		sort.Strings(wantParents)
		if !reflect.DeepEqual(got, wantParents) {
			t.Errorf("server node %q parents = %v, want %v", s.Name, got, wantParents)
		}
		wantMutable := s.StopStart != "" || s.Resize || s.LiveMigrate
		if (node.Mutate != nil) != wantMutable {
			t.Errorf("server node %q mutable=%v, want %v", s.Name, node.Mutate != nil, wantMutable)
		}
	}

	for _, v := range p.Volumes {
		node := nodeByKey(t, nodes, v.Name)
		if node.Kind != nova.KindVolume || len(node.Parents) != 1 || node.Parents[0] != v.Server {
			t.Errorf("volume node %q: kind=%q parents=%v, want volume/[%q]", v.Name, node.Kind, node.Parents, v.Server)
		}
		// The volume shares its server's gate.
		if node.Gate == nil || node.Gate != nodeByKey(t, nodes, v.Server).Gate {
			t.Errorf("volume node %q does not share server %q's gate", v.Name, v.Server)
		}
	}

	for _, pt := range p.Ports {
		node := nodeByKey(t, nodes, pt.Name)
		got := append([]string(nil), node.Parents...)
		sort.Strings(got)
		wantParents := []string{pt.Network, pt.Server}
		sort.Strings(wantParents)
		if node.Kind != nova.KindPort || !reflect.DeepEqual(got, wantParents) {
			t.Errorf("port node %q: kind=%q parents=%v, want port/%v", pt.Name, node.Kind, got, wantParents)
		}
		if node.Gate == nil || node.Gate != nodeByKey(t, nodes, pt.Server).Gate {
			t.Errorf("port node %q does not share server %q's gate", pt.Name, pt.Server)
		}
	}
}

// TestBuildRejectsInvalidPlan confirms Build surfaces a plan validation error
// (here a server referencing an unknown network) rather than emitting a node
// whose parent can never become present.
func TestBuildRejectsInvalidPlan(t *testing.T) {
	p := &novaplan.Plan{
		Networks: []novaplan.Network{{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"}},
		Servers:  []novaplan.Server{{Name: "srv-0001", Networks: []string{"ghost"}}},
	}
	if _, err := Build(p, newFakeNova(), novaexec.Resolved{}, time.Minute); err == nil {
		t.Fatal("Build of an invalid plan: expected an error, got nil")
	}
}

// TestMutateSkippedWhenLiveMigrationDisabledAndNoOtherOps confirms a server whose
// only lifecycle op is a disabled live migration is not mutable.
func TestMutateSkippedWhenLiveMigrationDisabledAndNoOtherOps(t *testing.T) {
	p := &novaplan.Plan{
		Networks: []novaplan.Network{{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"}},
		Servers:  []novaplan.Server{{Name: "srv-0001", Networks: []string{"net-0001"}, LiveMigrate: true}},
	}
	nodes, err := Build(p, newFakeNova(), novaexec.Resolved{LiveMigration: false}, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if node := nodeByKey(t, nodes, "srv-0001"); node.Mutate != nil {
		t.Error("server with only a disabled live migration should not be mutable")
	}
}

// TestBuildLongLivedShape confirms the long-lived graph has Build's nodes,
// parents and gates, pins every node, and makes a node repeatable exactly when
// it has a mutation: a server when the run enabled one of its operations, a
// volume or port when it is marked for detach, and never a network.
func TestBuildLongLivedShape(t *testing.T) {
	coldOff := resolvedAll()
	coldOff.ColdMigration = false
	tests := []struct {
		name     string
		resolved novaexec.Resolved
		mutable  map[string]bool // keys absent here have no mutation
	}{
		{"every operation enabled", resolvedAll(), map[string]bool{"srv-0001": true, "srv-0002": true, "srv-0003": true}},
		{"cold migration disabled", coldOff, map[string]bool{"srv-0001": true, "srv-0002": true}},
		{"nothing resolved", novaexec.Resolved{}, map[string]bool{"srv-0001": true, "srv-0002": true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := longLivedPlan()
			built, err := Build(p, newFakeNova(), tc.resolved, time.Minute)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			nodes, err := BuildLongLived(p, newFakeNova(), tc.resolved, time.Minute)
			if err != nil {
				t.Fatalf("BuildLongLived: %v", err)
			}
			if len(nodes) != len(built) {
				t.Fatalf("built %d nodes, want Build's %d", len(nodes), len(built))
			}
			for _, k := range []string{"vol-0001", "vol-0002", "port-0001", "port-0002"} {
				tc.mutable[k] = true
			}
			for i, n := range nodes {
				b := built[i]
				if n.Key != b.Key || n.Kind != b.Kind || !reflect.DeepEqual(n.Parents, b.Parents) || (n.Gate == nil) != (b.Gate == nil) {
					t.Errorf("node %d = %s/%s parents %v, want Build's %s/%s parents %v", i, n.Kind, n.Key, n.Parents, b.Kind, b.Key, b.Parents)
				}
				if !n.Pinned {
					t.Errorf("node %q is not pinned", n.Key)
				}
				if got := n.Mutate != nil; got != tc.mutable[n.Key] {
					t.Errorf("node %q mutable=%v, want %v", n.Key, got, tc.mutable[n.Key])
				}
			}
			for _, v := range p.Volumes {
				if nodeByKey(t, nodes, v.Name).Gate != nodeByKey(t, nodes, v.Server).Gate {
					t.Errorf("volume node %q does not share server %q's gate", v.Name, v.Server)
				}
			}
			for _, pt := range p.Ports {
				if nodeByKey(t, nodes, pt.Name).Gate != nodeByKey(t, nodes, pt.Server).Gate {
					t.Errorf("port node %q does not share server %q's gate", pt.Name, pt.Server)
				}
			}
		})
	}
}

// TestBuildLongLivedEdges covers a plan without servers, which yields its
// pinned networks only, and an invalid plan, which fails as Build fails.
func TestBuildLongLivedEdges(t *testing.T) {
	t.Run("no servers", func(t *testing.T) {
		p := longLivedPlan()
		p.Servers, p.Volumes, p.Ports = nil, nil, nil
		nodes, err := BuildLongLived(p, newFakeNova(), resolvedAll(), time.Minute)
		if err != nil {
			t.Fatalf("BuildLongLived: %v", err)
		}
		if len(nodes) != len(p.Networks) {
			t.Fatalf("built %d nodes, want the %d networks", len(nodes), len(p.Networks))
		}
		for _, n := range nodes {
			if n.Kind != nova.KindNetwork || !n.Pinned || n.Mutate != nil {
				t.Errorf("node %q = %s pinned=%v mutable=%v, want a pinned network without mutation", n.Key, n.Kind, n.Pinned, n.Mutate != nil)
			}
		}
	})

	t.Run("unknown network", func(t *testing.T) {
		p := longLivedPlan()
		p.Servers[0].Networks = []string{"ghost"}
		_, err := BuildLongLived(p, newFakeNova(), resolvedAll(), time.Minute)
		if err == nil || !strings.HasPrefix(err.Error(), "invalid plan:") {
			t.Errorf("BuildLongLived = %v, want an error starting with %q", err, "invalid plan:")
		}
	})
}

// TestBuildIgnoresColdMigrate confirms the churn graph of nova chaos never
// cold-migrates: a server whose only operation is a cold migration has no
// mutation, even when the run enabled cold migration.
func TestBuildIgnoresColdMigrate(t *testing.T) {
	p := longLivedPlan()
	nodes, err := Build(p, newFakeNova(), resolvedAll(), time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if node := nodeByKey(t, nodes, "srv-0003"); node.Mutate != nil {
		t.Error("a server with only a cold migration is mutable in the nova chaos graph")
	}
}

// keptServer is a plan of the one server srv-0001 with the operations of s,
// a detachable volume vol-0001 and a detachable port port-0001.
func keptServer(s novaplan.Server) *novaplan.Plan {
	s.Name, s.Networks = "srv-0001", []string{"net-0001"}
	return &novaplan.Plan{
		Seed: 7, Image: "cirros", Flavor: "m1.tiny", ResizeFlavor: "m1.small",
		Networks: []novaplan.Network{{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"}},
		Servers:  []novaplan.Server{s},
		Volumes:  []novaplan.Volume{{Name: "vol-0001", SizeGiB: 1, Server: "srv-0001", Detach: true}},
		Ports:    []novaplan.Port{{Name: "port-0001", Network: "net-0001", Server: "srv-0001", Detach: true}},
	}
}

// keptGraph builds the long-lived graph of p on f and creates every node in
// build order, which puts each parent before its children. It returns a
// function that runs the mutation of the node with a key once, as the engine
// would, with its parents' cloud ids.
func keptGraph(t *testing.T, p *novaplan.Plan, f *fakeNova, r novaexec.Resolved) func(key string) error {
	t.Helper()
	nodes, err := BuildLongLived(p, f, r, time.Minute)
	if err != nil {
		t.Fatalf("BuildLongLived: %v", err)
	}
	created := make(map[string]resource.Resource, len(nodes))
	idsOf := func(n chaos.Node) map[string]string {
		ids := make(map[string]string, len(n.Parents))
		for _, pk := range n.Parents {
			ids[pk] = created[pk].ID
		}
		return ids
	}
	for _, n := range nodes {
		res, err := n.Create(context.Background(), idsOf(n))
		if err != nil {
			t.Fatalf("creating %s: %v", n.Key, err)
		}
		created[n.Key] = res
	}
	return func(key string) error {
		n := nodeByKey(t, nodes, key)
		if n.Mutate == nil {
			t.Fatalf("node %q has no mutation", key)
		}
		return n.Mutate(context.Background(), idsOf(n), created[key])
	}
}

// logOf returns a copy of f's log entry for key in log.
func logOf(f *fakeNova, log map[string][]string, key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), log[key]...)
}

// TestLongLivedResizeAlternatesFlavors confirms a kept server resizes to the
// resize flavor and back to the boot flavor in turn, that a resize whose
// confirm failed targets the same flavor again, and that a resize Nova rejects
// because the server already has the target flavor resizes the other way.
func TestLongLivedResizeAlternatesFlavors(t *testing.T) {
	t.Run("alternates starting with the resize flavor", func(t *testing.T) {
		f := newFakeNova()
		mutate := keptGraph(t, keptServer(novaplan.Server{Resize: true}), f, resolvedAll())
		for i := 0; i < 3; i++ {
			if err := mutate("srv-0001"); err != nil {
				t.Fatalf("mutation %d: %v", i, err)
			}
		}
		if got, want := logOf(f, f.flavorsBySrv, "srv-0001"), []string{"flv-resize", "flv-boot", "flv-resize"}; !reflect.DeepEqual(got, want) {
			t.Errorf("resize flavors = %v, want %v", got, want)
		}
	})

	t.Run("a failed confirm retries the same flavor", func(t *testing.T) {
		f := newFakeNova()
		mutate := keptGraph(t, keptServer(novaplan.Server{Resize: true}), f, resolvedAll())
		boom := errors.New("confirm failed")
		f.failNext["ConfirmResizeServer"] = []error{boom}
		if err := mutate("srv-0001"); err != boom {
			t.Fatalf("mutation with a failing confirm = %v, want %v unchanged", err, boom)
		}
		for i := 0; i < 2; i++ {
			if err := mutate("srv-0001"); err != nil {
				t.Fatalf("mutation %d after the failure: %v", i, err)
			}
		}
		if got, want := logOf(f, f.flavorsBySrv, "srv-0001"), []string{"flv-resize", "flv-resize", "flv-boot"}; !reflect.DeepEqual(got, want) {
			t.Errorf("resize flavors = %v, want %v", got, want)
		}
	})

	t.Run("a resize finished after its wait gave up resyncs", func(t *testing.T) {
		f := newFakeNova()
		mutate := keptGraph(t, keptServer(novaplan.Server{Resize: true}), f, resolvedAll())
		// The first resize lands on the resize flavor but its ACTIVE wait gives
		// up, so Nova rejects the next resize to that flavor.
		sameFlavor := gophercloud.ErrUnexpectedResponseCode{
			Actual: http.StatusBadRequest,
			Body:   []byte(`{"badRequest":{"code":400,"message":"When resizing, instances must change flavor!"}}`),
		}
		f.failNext["WaitForServerStatus:ACTIVE"] = []error{context.DeadlineExceeded}
		f.failNext["ResizeServer"] = []error{nil, sameFlavor}
		if err := mutate("srv-0001"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("mutation with a timed-out wait = %v, want %v", err, context.DeadlineExceeded)
		}
		for i := 0; i < 2; i++ {
			if err := mutate("srv-0001"); err != nil {
				t.Fatalf("mutation %d after the timeout: %v", i, err)
			}
		}
		if got, want := logOf(f, f.flavorsBySrv, "srv-0001"), []string{"flv-resize", "flv-resize", "flv-boot", "flv-resize"}; !reflect.DeepEqual(got, want) {
			t.Errorf("resize flavors = %v, want %v", got, want)
		}
	})
}

// TestLongLivedVolumeToggle confirms a kept volume is detached and attached in
// turn, starting with a detach, without being created again; that a failed
// detach or attach is tried again; that a detach answered with 404 counts as
// done; and that an attach whose in-use wait gave up attaches again, which the
// client accepts for a volume that is already attached.
func TestLongLivedVolumeToggle(t *testing.T) {
	boom := errors.New("detach failed")
	refused := errors.New("attach failed")
	notFound := gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusNotFound}
	tests := []struct {
		name      string
		failNext  map[string][]error // the errors of the next calls, per fake method
		wantErrs  []error            // what the three mutations return
		wantCalls []string
	}{
		{"alternates", nil, []error{nil, nil, nil}, []string{"attach", "detach", "attach", "detach"}},
		{"a failed detach is tried again", map[string][]error{"DetachVolume": {boom}}, []error{boom, nil, nil}, []string{"attach", "detach", "detach", "attach"}},
		{"a 404 counts as detached", map[string][]error{"DetachVolume": {notFound}}, []error{nil, nil, nil}, []string{"attach", "detach", "attach", "detach"}},
		{"a failed attach is tried again", map[string][]error{"AttachVolume": {refused}}, []error{nil, refused, nil}, []string{"attach", "detach", "attach", "attach"}},
		{"a timed-out in-use wait attaches again", map[string][]error{"WaitForVolumeStatus:in-use": {context.DeadlineExceeded}}, []error{nil, context.DeadlineExceeded, nil}, []string{"attach", "detach", "attach", "attach"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeNova()
			mutate := keptGraph(t, keptServer(novaplan.Server{}), f, resolvedAll())
			for method, errs := range tc.failNext {
				f.failNext[method] = errs
			}
			for i, want := range tc.wantErrs {
				if err := mutate("vol-0001"); err != want {
					t.Errorf("mutation %d = %v, want %v", i, err, want)
				}
			}
			if got := logOf(f, f.attachLog, "vol-0001"); !reflect.DeepEqual(got, tc.wantCalls) {
				t.Errorf("attachment calls = %v, want %v", got, tc.wantCalls)
			}
			if f.createVol != 1 {
				t.Errorf("CreateVolume called %d times, want once", f.createVol)
			}
		})
	}
}

// TestLongLivedPortToggle confirms a kept port's detach waits until the server
// no longer has it before the mutation returns, its attach does not wait, a
// failed wait makes the next mutation detach again, and a failed attach makes
// it attach again.
func TestLongLivedPortToggle(t *testing.T) {
	t.Run("detach waits, attach does not", func(t *testing.T) {
		f := newFakeNova()
		mutate := keptGraph(t, keptServer(novaplan.Server{}), f, resolvedAll())
		if err := mutate("port-0001"); err != nil {
			t.Fatalf("detach: %v", err)
		}
		if got, want := logOf(f, f.attachLog, "port-0001"), []string{"attach", "detach", "wait-detached"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("calls after the detach = %v, want %v", got, want)
		}
		if err := mutate("port-0001"); err != nil {
			t.Fatalf("attach: %v", err)
		}
		if got, want := logOf(f, f.attachLog, "port-0001"), []string{"attach", "detach", "wait-detached", "attach"}; !reflect.DeepEqual(got, want) {
			t.Errorf("calls after the attach = %v, want %v", got, want)
		}
	})

	t.Run("a failed wait detaches again", func(t *testing.T) {
		f := newFakeNova()
		mutate := keptGraph(t, keptServer(novaplan.Server{}), f, resolvedAll())
		f.failNext["WaitForPortDetached"] = []error{context.DeadlineExceeded}
		if err := mutate("port-0001"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("detach with a failing wait = %v, want %v", err, context.DeadlineExceeded)
		}
		if err := mutate("port-0001"); err != nil {
			t.Fatalf("detach after the failure: %v", err)
		}
		if got, want := logOf(f, f.attachLog, "port-0001"), []string{"attach", "detach", "wait-detached", "detach", "wait-detached"}; !reflect.DeepEqual(got, want) {
			t.Errorf("calls = %v, want %v", got, want)
		}
	})

	t.Run("a failed attach attaches again", func(t *testing.T) {
		f := newFakeNova()
		mutate := keptGraph(t, keptServer(novaplan.Server{}), f, resolvedAll())
		refused := errors.New("attach failed")
		f.failNext["AttachPort"] = []error{refused}
		for i, want := range []error{nil, refused, nil} {
			if err := mutate("port-0001"); err != want {
				t.Errorf("mutation %d = %v, want %v", i, err, want)
			}
		}
		if got, want := logOf(f, f.attachLog, "port-0001"), []string{"attach", "detach", "wait-detached", "attach", "attach"}; !reflect.DeepEqual(got, want) {
			t.Errorf("calls = %v, want %v", got, want)
		}
	})
}

// TestLongLivedSkipsMigrationsWithoutVerdict confirms a kept server never
// migrates when the pre-check enabled neither kind, and that stop and start
// still runs.
func TestLongLivedSkipsMigrationsWithoutVerdict(t *testing.T) {
	f := newFakeNova()
	s := novaplan.Server{StopStart: novaplan.StopStartSoft, Resize: true, LiveMigrate: true, ColdMigrate: true}
	mutate := keptGraph(t, keptServer(s), f, novaexec.Resolved{})
	for i := 0; i < 5; i++ {
		if err := mutate("srv-0001"); err != nil {
			t.Fatalf("mutation %d: %v", i, err)
		}
	}
	want := []string{"stop-start", "stop-start", "stop-start", "stop-start", "stop-start"}
	if got := logOf(f, f.opsBySrv, "srv-0001"); !reflect.DeepEqual(got, want) {
		t.Errorf("operations = %v, want %v", got, want)
	}
}

// TestLongLivedColdMigrateErrorSurfaces confirms a failed cold migration comes
// back from the mutation unchanged and the server's next mutation runs.
func TestLongLivedColdMigrateErrorSurfaces(t *testing.T) {
	f := newFakeNova()
	mutate := keptGraph(t, keptServer(novaplan.Server{ColdMigrate: true}), f, resolvedAll())
	boom := errors.New("no valid host")
	f.failNext["ColdMigrateServer"] = []error{boom}
	if err := mutate("srv-0001"); err != boom {
		t.Fatalf("mutation with a failing cold migration = %v, want %v unchanged", err, boom)
	}
	if err := mutate("srv-0001"); err != nil {
		t.Fatalf("mutation after the failure: %v", err)
	}
	if got, want := logOf(f, f.opsBySrv, "srv-0001"), []string{"cold-migrate", "cold-migrate"}; !reflect.DeepEqual(got, want) {
		t.Errorf("operations = %v, want %v", got, want)
	}
}

// TestBuildServerGroupShape confirms a planned server group becomes a
// parentless server_group node ahead of the networks, and that a server in it
// is parented on its network and then its group.
func TestBuildServerGroupShape(t *testing.T) {
	p := &novaplan.Plan{
		Networks:     []novaplan.Network{{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"}},
		ServerGroups: []novaplan.ServerGroup{{Name: "grp-0001", Policy: novaplan.PolicyAntiAffinity}},
		Servers: []novaplan.Server{
			{Name: "srv-0001", Networks: []string{"net-0001"}, Group: "grp-0001"},
			{Name: "srv-0002", Networks: []string{"net-0001"}, Group: "grp-0001"},
		},
	}
	nodes, err := Build(p, newFakeNova(), novaexec.Resolved{}, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(nodes) != 4 || nodes[0].Key != "grp-0001" {
		t.Fatalf("built %d nodes starting with %q, want 4 starting with grp-0001", len(nodes), nodes[0].Key)
	}
	if g := nodes[0]; g.Kind != nova.KindServerGroup || len(g.Parents) != 0 || g.Gate != nil || g.Mutate != nil {
		t.Errorf("group node: kind=%q parents=%v gated=%v mutable=%v, want server_group/none/ungated/immutable",
			g.Kind, g.Parents, g.Gate != nil, g.Mutate != nil)
	}
	for _, s := range p.Servers {
		if got, want := nodeByKey(t, nodes, s.Name).Parents, []string{"net-0001", "grp-0001"}; !reflect.DeepEqual(got, want) {
			t.Errorf("server node %q parents = %v, want %v", s.Name, got, want)
		}
	}
}

// TestBuildRollingShape confirms the rolling graph pins the server groups and
// networks, gives every server and the volumes and ports of that server its
// group as the Roll value, and mutates nothing.
func TestBuildRollingShape(t *testing.T) {
	p := rollingPlan()
	p.Servers[0].StopStart = novaplan.StopStartSoft // Build would make it mutable
	p.Ports = []novaplan.Port{{Name: "port-0001", Network: "net-0001", Server: "srv-0001"}}
	nodes, err := BuildRolling(p, newFakeNova(), resolvedAll(), time.Minute)
	if err != nil {
		t.Fatalf("BuildRolling: %v", err)
	}
	if want := len(p.ServerGroups) + len(p.Networks) + len(p.Servers) + len(p.Volumes) + len(p.Ports); len(nodes) != want {
		t.Fatalf("built %d nodes, want %d", len(nodes), want)
	}
	group := map[string]string{"port-0001": "grp-0001"}
	for _, s := range p.Servers {
		group[s.Name] = s.Group
	}
	for _, v := range p.Volumes {
		group[v.Name] = group[v.Server]
	}
	for _, nd := range nodes {
		if nd.Mutate != nil {
			t.Errorf("node %q is mutable, want no mutation", nd.Key)
		}
		switch nd.Kind {
		case nova.KindServerGroup, nova.KindNetwork:
			if !nd.Pinned || nd.Roll != "" {
				t.Errorf("node %q pinned=%v roll=%q, want pinned and no roll", nd.Key, nd.Pinned, nd.Roll)
			}
		default:
			if nd.Pinned || nd.Roll != group[nd.Key] {
				t.Errorf("node %q pinned=%v roll=%q, want unpinned and roll %q", nd.Key, nd.Pinned, nd.Roll, group[nd.Key])
			}
		}
	}
}

// TestBuildRollingRejects confirms the rolling graph refuses a server without
// a group, fails an invalid plan as Build does, and yields no node for an
// empty plan.
func TestBuildRollingRejects(t *testing.T) {
	net := []novaplan.Network{{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"}}

	p := &novaplan.Plan{Networks: net, Servers: []novaplan.Server{{Name: "srv-0001", Networks: []string{"net-0001"}}}}
	_, err := BuildRolling(p, newFakeNova(), novaexec.Resolved{}, time.Minute)
	if want := `server "srv-0001" has no server group, which a rolling graph needs`; err == nil || err.Error() != want {
		t.Errorf("BuildRolling without a group = %v, want %q", err, want)
	}

	p = &novaplan.Plan{Networks: net, Servers: []novaplan.Server{{Name: "srv-0001", Networks: []string{"ghost"}}}}
	if _, err := BuildRolling(p, newFakeNova(), novaexec.Resolved{}, time.Minute); err == nil || !strings.HasPrefix(err.Error(), "invalid plan:") {
		t.Errorf("BuildRolling of an invalid plan = %v, want an error starting with \"invalid plan:\"", err)
	}

	nodes, err := BuildRolling(&novaplan.Plan{}, newFakeNova(), novaexec.Resolved{}, time.Minute)
	if err != nil || len(nodes) != 0 {
		t.Errorf("BuildRolling of an empty plan = %d nodes, %v, want none and nil", len(nodes), err)
	}
}
