package novagraph

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/nova"
	novaexec "github.com/B42Labs/dizzy/internal/nova/executor"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
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
