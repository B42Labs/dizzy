// Package plan defines the expanded, fully-enumerated set of Neutron resources
// and their relationships — the expected-state source of truth produced
// deterministically from a scenario plus a seed. The model is pure data: every
// collection is a slice (never a map) so that encoding the plan to JSON yields
// byte-identical output for the same input.
package plan

import (
	"fmt"
	"strings"
)

// Plan is the fully-expanded expected state for one run. Scenario and Seed
// record the provenance that produced it; the slices enumerate every resource
// in dependency order. Cross-resource references are by logical name, resolved
// by Validate.
type Plan struct {
	Scenario         string            `json:"scenario"`
	Seed             int64             `json:"seed"`
	AddressScopes    []AddressScope    `json:"addressScopes"`
	SubnetPools      []SubnetPool      `json:"subnetPools"`
	Networks         []Network         `json:"networks"`
	Subnets          []Subnet          `json:"subnets"`
	Routers          []Router          `json:"routers"`
	RouterInterfaces []RouterInterface `json:"routerInterfaces"`
	SecurityGroups   []SecurityGroup   `json:"securityGroups"`
	Ports            []Port            `json:"ports"`
}

// AddressScope is a named L3 address scope that subnet pools may belong to.
type AddressScope struct {
	Name      string `json:"name"`
	IPVersion int    `json:"ipVersion"`
}

// SubnetPool is a pool that pool-allocated subnets draw their CIDRs from.
// AddressScope, when set, references an AddressScope by name.
type SubnetPool struct {
	Name             string   `json:"name"`
	IPVersion        int      `json:"ipVersion"`
	Prefixes         []string `json:"prefixes"`
	DefaultPrefixLen int      `json:"defaultPrefixLen"`
	AddressScope     string   `json:"addressScope,omitempty"`
}

// Network is a plain tenant network (geneve/vxlan). It carries no provider
// attributes; VLAN/flat provider networks are out of scope for Phase 1.
type Network struct {
	Name string `json:"name"`
}

// Subnet belongs to a Network. Exactly one allocation source is set: either an
// explicit CIDR, or a SubnetPool reference together with PrefixLen. The IPv6
// mode fields are populated only when IPVersion is 6.
type Subnet struct {
	Name            string `json:"name"`
	Network         string `json:"network"`
	IPVersion       int    `json:"ipVersion"`
	CIDR            string `json:"cidr,omitempty"`
	SubnetPool      string `json:"subnetPool,omitempty"`
	PrefixLen       int    `json:"prefixLen,omitempty"`
	IPv6AddressMode string `json:"ipv6AddressMode,omitempty"`
	IPv6RAMode      string `json:"ipv6RAMode,omitempty"`
}

// Router is an internal router. Phase 1 routers have no external gateway.
type Router struct {
	Name string `json:"name"`
}

// RouterInterface attaches a Subnet to a Router. Both are references by name.
type RouterInterface struct {
	Name   string `json:"name"`
	Router string `json:"router"`
	Subnet string `json:"subnet"`
}

// SecurityGroup is a named group with its rules nested for locality.
type SecurityGroup struct {
	Name  string              `json:"name"`
	Rules []SecurityGroupRule `json:"rules"`
}

// SecurityGroupRule is a single rule within a SecurityGroup. At most one of
// RemoteIPPrefix or RemoteGroup is set; RemoteGroup references a SecurityGroup
// by name.
type SecurityGroupRule struct {
	Direction      string `json:"direction"`
	EtherType      string `json:"etherType"`
	Protocol       string `json:"protocol"`
	PortRangeMin   int    `json:"portRangeMin,omitempty"`
	PortRangeMax   int    `json:"portRangeMax,omitempty"`
	RemoteIPPrefix string `json:"remoteIPPrefix,omitempty"`
	RemoteGroup    string `json:"remoteGroup,omitempty"`
}

// Port belongs to a Network and references its security groups by name.
type Port struct {
	Name           string    `json:"name"`
	Network        string    `json:"network"`
	FixedIPs       []FixedIP `json:"fixedIPs"`
	SecurityGroups []string  `json:"securityGroups"`
}

// FixedIP pins a Port to a Subnet. IPAddress, when empty, means the address is
// auto-allocated at apply time.
type FixedIP struct {
	Subnet    string `json:"subnet"`
	IPAddress string `json:"ipAddress,omitempty"`
}

// Validate checks the plan graph for well-formedness: every cross-resource
// reference resolves, and each subnet is attached to at most one router (the
// Phase 1 topology invariant). It returns an error naming the first offending
// resource.
func (p *Plan) Validate() error {
	networks := nameSet(p.Networks, func(n Network) string { return n.Name })
	subnetPools := nameSet(p.SubnetPools, func(sp SubnetPool) string { return sp.Name })
	addressScopes := nameSet(p.AddressScopes, func(as AddressScope) string { return as.Name })
	routers := nameSet(p.Routers, func(r Router) string { return r.Name })
	subnets := nameSet(p.Subnets, func(s Subnet) string { return s.Name })
	securityGroups := nameSet(p.SecurityGroups, func(sg SecurityGroup) string { return sg.Name })

	for _, sp := range p.SubnetPools {
		if sp.AddressScope != "" && !addressScopes[sp.AddressScope] {
			return fmt.Errorf("subnet pool %q references unknown address scope %q", sp.Name, sp.AddressScope)
		}
	}

	for _, s := range p.Subnets {
		if !networks[s.Network] {
			return fmt.Errorf("subnet %q references unknown network %q", s.Name, s.Network)
		}
		if s.SubnetPool != "" && !subnetPools[s.SubnetPool] {
			return fmt.Errorf("subnet %q references unknown subnet pool %q", s.Name, s.SubnetPool)
		}
	}

	attached := make(map[string]bool, len(p.RouterInterfaces))
	for _, ri := range p.RouterInterfaces {
		if !routers[ri.Router] {
			return fmt.Errorf("router interface %q references unknown router %q", ri.Name, ri.Router)
		}
		if !subnets[ri.Subnet] {
			return fmt.Errorf("router interface %q references unknown subnet %q", ri.Name, ri.Subnet)
		}
		if attached[ri.Subnet] {
			return fmt.Errorf("subnet %q is attached to more than one router", ri.Subnet)
		}
		attached[ri.Subnet] = true
	}

	for _, sg := range p.SecurityGroups {
		for _, rule := range sg.Rules {
			if rule.RemoteGroup != "" && !securityGroups[rule.RemoteGroup] {
				return fmt.Errorf("security group %q has a rule referencing unknown remote group %q", sg.Name, rule.RemoteGroup)
			}
		}
	}

	for _, port := range p.Ports {
		if !networks[port.Network] {
			return fmt.Errorf("port %q references unknown network %q", port.Name, port.Network)
		}
		for _, fip := range port.FixedIPs {
			if !subnets[fip.Subnet] {
				return fmt.Errorf("port %q references unknown subnet %q", port.Name, fip.Subnet)
			}
		}
		for _, sg := range port.SecurityGroups {
			if !securityGroups[sg] {
				return fmt.Errorf("port %q references unknown security group %q", port.Name, sg)
			}
		}
	}

	return nil
}

// Summary returns a deterministic, human-readable count of every resource type
// in the plan, used by "apply --dry-run" to preview a scenario without touching
// a cloud.
func (p *Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan for scenario %q (seed %d)\n", p.Scenario, p.Seed)
	fmt.Fprintf(&b, "  address scopes:    %d\n", len(p.AddressScopes))
	fmt.Fprintf(&b, "  subnet pools:      %d\n", len(p.SubnetPools))
	fmt.Fprintf(&b, "  networks:          %d\n", len(p.Networks))
	fmt.Fprintf(&b, "  subnets:           %d\n", len(p.Subnets))
	fmt.Fprintf(&b, "  routers:           %d\n", len(p.Routers))
	fmt.Fprintf(&b, "  router interfaces: %d\n", len(p.RouterInterfaces))
	fmt.Fprintf(&b, "  security groups:   %d\n", len(p.SecurityGroups))
	fmt.Fprintf(&b, "  ports:             %d\n", len(p.Ports))
	return b.String()
}

// nameSet builds a lookup set of names from a slice using the supplied name
// accessor. It is used only for reference resolution and never affects JSON
// output.
func nameSet[T any](items []T, name func(T) string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[name(item)] = true
	}
	return set
}
