package scenario

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/B42Labs/openstack-tester/internal/plan"
)

var update = flag.Bool("update", false, "update golden files")

// smallScenario is a compact scenario that still exercises every resource kind
// (pools, IPv4/IPv6/pool subnets, router interfaces, rules, multi-SG ports). It
// backs the golden test that locks byte-stability across runs and Go versions.
func smallScenario() Scenario {
	return Scenario{
		Name: "small",
		Seed: 42,
		Resources: Resources{
			SubnetPools:    1,
			AddressScopes:  1,
			Networks:       3,
			Routers:        2,
			SecurityGroups: 2,
			RouterLinks:    1,
			FloatingIPs:    2,
		},
		Distribution: Distribution{
			SubnetsPerNetwork:               Range{Min: 1, Max: 3},
			PortsPerNetwork:                 Range{Min: 1, Max: 2},
			RulesPerSecurityGroup:           Range{Min: 1, Max: 3},
			SubnetFromPoolRatio:             0.5,
			IPv6Ratio:                       0.3,
			SubnetsAttachedToRouterRatio:    0.7,
			RoutersWithExternalGatewayRatio: 0.5,
			FloatingIPAssociatedRatio:       0.5,
		},
		Topology: Topology{
			RouterAttachStrategy:   "random",
			PortSecurityGroupCount: Range{Min: 1, Max: 2},
		},
	}
}

// marshal renders a plan exactly as the generate command does: indented JSON
// with a trailing newline.
func marshal(t *testing.T, p *plan.Plan) []byte {
	t.Helper()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("marshalling plan: %v", err)
	}
	return append(data, '\n')
}

func TestGenerateInvalidScenario(t *testing.T) {
	s := smallScenario()
	s.Name = "" // fails Scenario.Validate

	if _, err := s.Generate(); err == nil {
		t.Fatal("Generate() = nil error, want error for invalid scenario")
	}
}

func TestGenerateDeterministic(t *testing.T) {
	s := smallScenario()

	p1, err := s.Generate()
	if err != nil {
		t.Fatalf("first Generate(): %v", err)
	}
	p2, err := s.Generate()
	if err != nil {
		t.Fatalf("second Generate(): %v", err)
	}

	if got, want := marshal(t, p1), marshal(t, p2); !bytes.Equal(got, want) {
		t.Error("two generations of the same scenario+seed differ")
	}
}

func TestGenerateSeedChangesTopology(t *testing.T) {
	s1 := smallScenario()
	s2 := smallScenario()
	s2.Seed = s1.Seed + 1

	p1, err := s1.Generate()
	if err != nil {
		t.Fatalf("Generate(seed=%d): %v", s1.Seed, err)
	}
	p2, err := s2.Generate()
	if err != nil {
		t.Fatalf("Generate(seed=%d): %v", s2.Seed, err)
	}

	if bytes.Equal(marshal(t, p1), marshal(t, p2)) {
		t.Error("different seeds produced identical plans")
	}
}

func TestGenerateGolden(t *testing.T) {
	p, err := smallScenario().Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	got := marshal(t, p)

	path := filepath.Join("testdata", "golden", "small.plan.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("generated plan differs from golden file %s; run with -update if the change is intended", path)
	}
}

// TestGenerateExternalConnectivityAndLinks exercises the external-gateway
// intent, router-to-router links, and floating IPs together: it checks the link
// topology is well-formed (one subnet-side and one port-side interface per
// link over a /30 transit subnet), that gateway intent is recorded, and that
// floating IPs associate only with distinct, non-interface ports.
func TestGenerateExternalConnectivityAndLinks(t *testing.T) {
	s := Scenario{
		Name: "ext",
		Seed: 99,
		Resources: Resources{
			Networks:       6,
			Routers:        4,
			SecurityGroups: 1,
			RouterLinks:    2,
			FloatingIPs:    5,
		},
		Distribution: Distribution{
			SubnetsPerNetwork:               Range{Min: 1, Max: 2},
			PortsPerNetwork:                 Range{Min: 1, Max: 3},
			RulesPerSecurityGroup:           Range{Min: 1, Max: 2},
			IPv6Ratio:                       0.4, // mix in IPv6 subnets the FIP eligibility must skip
			SubnetsAttachedToRouterRatio:    1.0,
			RoutersWithExternalGatewayRatio: 1.0,
			FloatingIPAssociatedRatio:       1.0,
		},
		Topology: Topology{PortSecurityGroupCount: Range{Min: 0, Max: 1}},
	}

	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}

	// Every router intends an external gateway at ratio 1.0.
	if got := p.RoutersWithExternalGateway(); got != 4 {
		t.Errorf("routers with external gateway = %d, want 4", got)
	}

	// Each link adds exactly one subnet-side and one port-side interface.
	var linkNets, linkSubnets, linkPorts, subnetSide, portSide int
	for _, n := range p.Networks {
		if strings.HasPrefix(n.Name, "link-net-") {
			linkNets++
		}
	}
	transitCIDR := map[string]string{}
	for _, sub := range p.Subnets {
		if strings.HasPrefix(sub.Name, "link-subnet-") {
			linkSubnets++
			if pfx, err := netip.ParsePrefix(sub.CIDR); err != nil || pfx.Bits() != 30 || !pfx.Addr().IsPrivate() {
				t.Errorf("transit subnet %q has CIDR %q, want a private /30", sub.Name, sub.CIDR)
			}
			if !sub.DisableDHCP {
				t.Errorf("transit subnet %q must disable DHCP so its single host address is free for the link port", sub.Name)
			}
			transitCIDR[sub.Name] = sub.CIDR
		}
	}
	for _, pt := range p.Ports {
		if strings.HasPrefix(pt.Name, "link-port-") {
			linkPorts++
			if len(pt.FixedIPs) != 1 || pt.FixedIPs[0].IPAddress == "" {
				t.Errorf("transit port %q must pin exactly one explicit IP, got %+v", pt.Name, pt.FixedIPs)
			}
		}
	}
	for _, ri := range p.RouterInterfaces {
		switch {
		case strings.HasPrefix(ri.Name, "link-rif-a-"):
			subnetSide++
			if ri.Subnet == "" || ri.Port != "" {
				t.Errorf("link interface %q must be subnet-side, got subnet=%q port=%q", ri.Name, ri.Subnet, ri.Port)
			}
		case strings.HasPrefix(ri.Name, "link-rif-b-"):
			portSide++
			if ri.Port == "" || ri.Subnet != "" {
				t.Errorf("link interface %q must be port-side, got subnet=%q port=%q", ri.Name, ri.Subnet, ri.Port)
			}
		}
	}
	if linkNets != 2 || linkSubnets != 2 || linkPorts != 2 || subnetSide != 2 || portSide != 2 {
		t.Errorf("link topology = nets %d, subnets %d, ports %d, subnet-side %d, port-side %d; want 2 of each",
			linkNets, linkSubnets, linkPorts, subnetSide, portSide)
	}

	// Floating IPs: all requested are present; associations target distinct,
	// real, non-interface ports.
	if got := len(p.FloatingIPs); got != 5 {
		t.Fatalf("floating IPs = %d, want 5", got)
	}
	interfacePort := map[string]bool{}
	for _, ri := range p.RouterInterfaces {
		if ri.Port != "" {
			interfacePort[ri.Port] = true
		}
	}
	ipv4Subnet := map[string]bool{}
	for _, sub := range p.Subnets {
		if sub.IPVersion == 4 {
			ipv4Subnet[sub.Name] = true
		}
	}
	portByName := map[string]plan.Port{}
	for _, pt := range p.Ports {
		portByName[pt.Name] = pt
	}
	seen := map[string]bool{}
	var associated int
	for _, fip := range p.FloatingIPs {
		if fip.Port == "" {
			continue
		}
		associated++
		pt, ok := portByName[fip.Port]
		if !ok {
			t.Errorf("floating ip %q targets unknown port %q", fip.Name, fip.Port)
			continue
		}
		if interfacePort[fip.Port] {
			t.Errorf("floating ip %q targets a router-interface port %q", fip.Name, fip.Port)
		}
		hasIPv4 := false
		for _, f := range pt.FixedIPs {
			if ipv4Subnet[f.Subnet] {
				hasIPv4 = true
			}
		}
		if !hasIPv4 {
			t.Errorf("floating ip %q targets port %q with no IPv4 fixed IP; Neutron would reject the association", fip.Name, fip.Port)
		}
		if seen[fip.Port] {
			t.Errorf("port %q targeted by more than one floating ip", fip.Port)
		}
		seen[fip.Port] = true
	}
	if associated == 0 {
		t.Error("no floating IPs were associated despite ratio 1.0 and eligible ports")
	}

	// The feature-enabled path stays deterministic.
	if p2, err := s.Generate(); err != nil {
		t.Fatalf("second Generate(): %v", err)
	} else if !bytes.Equal(marshal(t, p), marshal(t, p2)) {
		t.Error("two generations of the same external scenario differ")
	}
}

func TestGenerateCounts(t *testing.T) {
	s := Scenario{
		Name: "counts",
		Seed: 7,
		Resources: Resources{
			SubnetPools:    2,
			AddressScopes:  1,
			Networks:       40,
			Routers:        5,
			SecurityGroups: 4,
		},
		Distribution: Distribution{
			SubnetsPerNetwork:            Range{Min: 1, Max: 3},
			PortsPerNetwork:              Range{Min: 0, Max: 4},
			RulesPerSecurityGroup:        Range{Min: 2, Max: 6},
			SubnetFromPoolRatio:          0.4,
			IPv6Ratio:                    0.3,
			SubnetsAttachedToRouterRatio: 0.6,
		},
		Topology: Topology{
			RouterAttachStrategy:   "random",
			PortSecurityGroupCount: Range{Min: 1, Max: 2},
		},
	}

	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}

	// Fixed counts match the scenario exactly.
	if got := len(p.AddressScopes); got != s.Resources.AddressScopes {
		t.Errorf("address scopes = %d, want %d", got, s.Resources.AddressScopes)
	}
	if got := len(p.SubnetPools); got != s.Resources.SubnetPools {
		t.Errorf("subnet pools = %d, want %d", got, s.Resources.SubnetPools)
	}
	if got := len(p.Networks); got != s.Resources.Networks {
		t.Errorf("networks = %d, want %d", got, s.Resources.Networks)
	}
	if got := len(p.Routers); got != s.Resources.Routers {
		t.Errorf("routers = %d, want %d", got, s.Resources.Routers)
	}
	if got := len(p.SecurityGroups); got != s.Resources.SecurityGroups {
		t.Errorf("security groups = %d, want %d", got, s.Resources.SecurityGroups)
	}

	// Per-security-group rule counts fall within their distribution bounds.
	for _, sg := range p.SecurityGroups {
		if n := len(sg.Rules); n < s.Distribution.RulesPerSecurityGroup.Min || n > s.Distribution.RulesPerSecurityGroup.Max {
			t.Errorf("security group %q has %d rules, want within [%d,%d]", sg.Name, n,
				s.Distribution.RulesPerSecurityGroup.Min, s.Distribution.RulesPerSecurityGroup.Max)
		}
	}

	// A subnet attaches to at most one router.
	attached := make(map[string]bool)
	for _, ri := range p.RouterInterfaces {
		if attached[ri.Subnet] {
			t.Errorf("subnet %q attached to more than one router", ri.Subnet)
		}
		attached[ri.Subnet] = true
	}

	// Both IPv6 and pool-allocated subnets appear when their ratios are nonzero.
	var ipv6, pool int
	for _, sub := range p.Subnets {
		switch {
		case sub.IPVersion == 6:
			ipv6++
		case sub.SubnetPool != "":
			pool++
		}
	}
	if ipv6 == 0 {
		t.Error("no IPv6 subnets generated despite ipv6_ratio > 0")
	}
	if pool == 0 {
		t.Error("no pool-allocated subnets generated despite subnet_from_pool_ratio > 0")
	}
}
