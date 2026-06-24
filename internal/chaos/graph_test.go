package chaos

import (
	"sort"
	"testing"

	"github.com/B42Labs/openstack-tester/internal/neutron"
	"github.com/B42Labs/openstack-tester/internal/plan"
)

// fullPlan is a small plan exercising every kind and cross-reference, mirroring
// the executor's test plan so the graph's parent resolution can be checked
// against the same shape the executor applies.
func fullPlan() *plan.Plan {
	return &plan.Plan{
		Scenario:    "test",
		Seed:        1,
		SubnetPools: []plan.SubnetPool{{Name: "pool-1", IPVersion: 4, Prefixes: []string{"172.16.0.0/16"}, DefaultPrefixLen: 26}},
		Networks:    []plan.Network{{Name: "net-1"}, {Name: "net-2"}},
		Subnets: []plan.Subnet{
			{Name: "subnet-1", Network: "net-1", IPVersion: 4, CIDR: "10.0.0.0/24"},
			{Name: "subnet-2", Network: "net-2", IPVersion: 4, SubnetPool: "pool-1", PrefixLen: 26},
		},
		Routers:          []plan.Router{{Name: "router-1"}},
		RouterInterfaces: []plan.RouterInterface{{Name: "rif-1", Router: "router-1", Subnet: "subnet-1"}},
		SecurityGroups: []plan.SecurityGroup{{Name: "sg-1", Rules: []plan.SecurityGroupRule{
			{Direction: "ingress", EtherType: "IPv4", Protocol: "tcp", RemoteGroup: "sg-1"},
		}}},
		Ports: []plan.Port{{Name: "port-1", Network: "net-1", FixedIPs: []plan.FixedIP{{Subnet: "subnet-1"}}, SecurityGroups: []string{"sg-1"}}},
	}
}

// externalPlan exercises a port-side router interface and a floating IP, the two
// kinds whose handling depends on the external network.
func externalPlan() *plan.Plan {
	return &plan.Plan{
		Networks: []plan.Network{{Name: "net-1"}, {Name: "link-net-1"}},
		Subnets: []plan.Subnet{
			{Name: "subnet-1", Network: "net-1", IPVersion: 4, CIDR: "10.0.0.0/24"},
			{Name: "link-subnet-1", Network: "link-net-1", IPVersion: 4, CIDR: "192.168.0.0/30"},
		},
		Routers: []plan.Router{{Name: "router-1", ExternalGateway: true}, {Name: "router-2"}},
		Ports: []plan.Port{
			{Name: "port-1", Network: "net-1", FixedIPs: []plan.FixedIP{{Subnet: "subnet-1"}}},
			{Name: "link-port-1", Network: "link-net-1", FixedIPs: []plan.FixedIP{{Subnet: "link-subnet-1", IPAddress: "192.168.0.2"}}},
		},
		RouterInterfaces: []plan.RouterInterface{
			{Name: "rif-1", Router: "router-1", Subnet: "subnet-1"},
			{Name: "link-rif-b-1", Router: "router-2", Port: "link-port-1"},
		},
		FloatingIPs: []plan.FloatingIP{
			{Name: "fip-1", Port: "port-1"},
			{Name: "fip-2"},
		},
	}
}

func nodeByKey(t *testing.T, nodes []Node, key string) Node {
	t.Helper()
	for _, n := range nodes {
		if n.Key == key {
			return n
		}
	}
	t.Fatalf("no node with key %q in %d nodes", key, len(nodes))
	return Node{}
}

func assertParents(t *testing.T, n Node, want ...string) {
	t.Helper()
	got := append([]string(nil), n.Parents...)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("node %q parents = %v, want %v", n.Key, n.Parents, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("node %q parents = %v, want %v", n.Key, n.Parents, want)
		}
	}
}

func TestBuildOneNodePerResource(t *testing.T) {
	nodes, err := Build(fullPlan(), "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// 1 pool + 2 networks + 2 subnets + 1 router + 1 interface + 1 sg + 1 rule
	// + 1 port = 10 nodes. No floating IPs are in fullPlan.
	if len(nodes) != 10 {
		t.Errorf("built %d nodes, want 10", len(nodes))
	}
	keys := make(map[string]int)
	for _, n := range nodes {
		keys[n.Key]++
	}
	for k, c := range keys {
		if c != 1 {
			t.Errorf("key %q produced %d nodes, want exactly 1", k, c)
		}
	}
}

func TestBuildParentEdges(t *testing.T) {
	nodes, err := Build(fullPlan(), "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A pool-allocated subnet depends on its network and its pool.
	assertParents(t, nodeByKey(t, nodes, "subnet-2"), "net-2", "pool-1")
	// An explicit-CIDR subnet depends only on its network.
	assertParents(t, nodeByKey(t, nodes, "subnet-1"), "net-1")
	// A port depends on its network, its fixed-IP subnets, and its groups.
	assertParents(t, nodeByKey(t, nodes, "port-1"), "net-1", "subnet-1", "sg-1")
	// A subnet-side interface depends on the router and the subnet.
	assertParents(t, nodeByKey(t, nodes, "rif-1"), "router-1", "subnet-1")
	// A rule depends on its group and, here, the remote group it references.
	assertParents(t, nodeByKey(t, nodes, "rule:sg-1:0"), "sg-1")
}

func TestBuildPortSideInterfaceAndFloatingIPWithExternal(t *testing.T) {
	nodes, err := Build(externalPlan(), "extnet")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A port-side interface depends on the router and the port (not a subnet).
	assertParents(t, nodeByKey(t, nodes, "link-rif-b-1"), "router-2", "link-port-1")
	// An associated floating IP depends on its port; an unassociated one has none.
	assertParents(t, nodeByKey(t, nodes, "fip-1"), "port-1")
	assertParents(t, nodeByKey(t, nodes, "fip-2"))
}

func TestBuildOmitsFloatingIPsWithoutExternalNetwork(t *testing.T) {
	nodes, err := Build(externalPlan(), "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, n := range nodes {
		if n.Kind == neutron.KindFloatingIP {
			t.Errorf("floating IP node %q built without an external network", n.Key)
		}
	}
}

func TestBuildRejectsInvalidPlan(t *testing.T) {
	// A subnet referencing a network that does not exist is a dangling edge the
	// plan validator rejects; Build must surface that rather than emit a node
	// whose parent can never become present.
	p := &plan.Plan{
		Networks: []plan.Network{{Name: "net-1"}},
		Subnets:  []plan.Subnet{{Name: "subnet-1", Network: "ghost", IPVersion: 4, CIDR: "10.0.0.0/24"}},
	}
	if _, err := Build(p, ""); err == nil {
		t.Fatal("Build of a plan with a dangling reference: expected an error, got nil")
	}
}
