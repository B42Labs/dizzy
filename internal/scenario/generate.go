package scenario

import (
	"fmt"
	"math/rand"
	"net/netip"

	"github.com/B42Labs/dizzy/internal/plan"
)

// IP allocation ranges. The three ranges do not overlap, so explicit IPv4
// subnets, subnet pools, and IPv6 subnets never collide.
const (
	ipv4Base       = uint32(10) << 24                  // 10.0.0.0, explicit IPv4 subnets as /24
	ipv4BlockCount = 1 << 16                           // number of /24 blocks in 10.0.0.0/8
	poolBase       = uint32(172)<<24 | uint32(16)<<16  // 172.16.0.0, subnet pools as /16
	poolBlockCount = 16                                // number of /16 blocks in 172.16.0.0/12
	poolPrefixLen  = 26                                // prefix length pools hand out to subnets
	ipv6BlockCount = 1 << 16                           // number of /64 blocks enumerated under fd00::/16
	linkBase       = uint32(192)<<24 | uint32(168)<<16 // 192.168.0.0, router-to-router transit subnets as /30
	linkBlockCount = 1 << 14                           // number of /30 blocks in 192.168.0.0/16
	linkPrefixLen  = 30                                // point-to-point transit subnets
)

// Generate expands the scenario and its seed into a fully-enumerated plan. The
// same scenario and seed always produce a byte-identical plan: the generator
// draws from math/rand v1 (whose sequence is frozen for compatibility) in a
// fixed order and emits every collection in a fixed order. The returned plan is
// validated before it is handed back.
func (s Scenario) Generate() (*plan.Plan, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid scenario: %w", err)
	}

	g := &generator{rng: rand.New(rand.NewSource(s.Seed))}
	p := &plan.Plan{Scenario: s.Name, Seed: s.Seed}

	p.AddressScopes = make([]plan.AddressScope, 0, s.Resources.AddressScopes)
	for i := 0; i < s.Resources.AddressScopes; i++ {
		p.AddressScopes = append(p.AddressScopes, plan.AddressScope{
			Name:      fmt.Sprintf("as-%04d", i+1),
			IPVersion: 4,
		})
	}

	p.SubnetPools = make([]plan.SubnetPool, 0, s.Resources.SubnetPools)
	for i := 0; i < s.Resources.SubnetPools; i++ {
		prefix, err := g.nextPoolPrefix()
		if err != nil {
			return nil, err
		}
		p.SubnetPools = append(p.SubnetPools, plan.SubnetPool{
			Name:             fmt.Sprintf("pool-%04d", i+1),
			IPVersion:        4,
			Prefixes:         []string{prefix},
			DefaultPrefixLen: poolPrefixLen,
		})
	}

	p.Networks = make([]plan.Network, 0, s.Resources.Networks)
	for i := 0; i < s.Resources.Networks; i++ {
		p.Networks = append(p.Networks, plan.Network{Name: fmt.Sprintf("net-%04d", i+1)})
	}

	// Subnets are grouped by network so ports can later pin a fixed IP to a
	// subnet of their own network.
	p.Subnets = make([]plan.Subnet, 0, len(p.Networks))
	subnetsByNetwork := make(map[string][]string, len(p.Networks))
	subnetCount := 0
	for _, n := range p.Networks {
		for j := 0; j < randRange(g.rng, s.Distribution.SubnetsPerNetwork); j++ {
			subnetCount++
			subnet := plan.Subnet{
				Name:    fmt.Sprintf("subnet-%04d", subnetCount),
				Network: n.Name,
			}
			switch {
			case g.rng.Float64() < s.Distribution.IPv6Ratio:
				cidr, err := g.nextIPv6CIDR()
				if err != nil {
					return nil, err
				}
				subnet.IPVersion = 6
				subnet.CIDR = cidr
				subnet.IPv6AddressMode = "slaac"
				subnet.IPv6RAMode = "slaac"
			case len(p.SubnetPools) > 0 && g.rng.Float64() < s.Distribution.SubnetFromPoolRatio:
				pool := p.SubnetPools[g.rng.Intn(len(p.SubnetPools))]
				subnet.IPVersion = 4
				subnet.SubnetPool = pool.Name
				subnet.PrefixLen = pool.DefaultPrefixLen
			default:
				cidr, err := g.nextIPv4CIDR()
				if err != nil {
					return nil, err
				}
				subnet.IPVersion = 4
				subnet.CIDR = cidr
			}
			p.Subnets = append(p.Subnets, subnet)
			subnetsByNetwork[n.Name] = append(subnetsByNetwork[n.Name], subnet.Name)
		}
	}

	p.Routers = make([]plan.Router, 0, s.Resources.Routers)
	for i := 0; i < s.Resources.Routers; i++ {
		router := plan.Router{Name: fmt.Sprintf("router-%04d", i+1)}
		// Draw the external-gateway intent only when the ratio is set, so plans
		// that do not use external connectivity stay byte-identical to before the
		// feature existed (the RNG sequence is not disturbed).
		if s.Distribution.RoutersWithExternalGatewayRatio > 0 {
			router.ExternalGateway = g.rng.Float64() < s.Distribution.RoutersWithExternalGatewayRatio
		}
		p.Routers = append(p.Routers, router)
	}

	// Each subnet is considered once and attached to a random router with the
	// configured probability, so a subnet attaches to at most one router.
	p.RouterInterfaces = make([]plan.RouterInterface, 0, len(p.Subnets))
	rifCount := 0
	for _, subnet := range p.Subnets {
		if len(p.Routers) == 0 || g.rng.Float64() >= s.Distribution.SubnetsAttachedToRouterRatio {
			continue
		}
		router := p.Routers[g.rng.Intn(len(p.Routers))]
		rifCount++
		p.RouterInterfaces = append(p.RouterInterfaces, plan.RouterInterface{
			Name:   fmt.Sprintf("rif-%04d", rifCount),
			Router: router.Name,
			Subnet: subnet.Name,
		})
	}

	p.SecurityGroups = make([]plan.SecurityGroup, 0, s.Resources.SecurityGroups)
	for i := 0; i < s.Resources.SecurityGroups; i++ {
		ruleCount := randRange(g.rng, s.Distribution.RulesPerSecurityGroup)
		rules := make([]plan.SecurityGroupRule, 0, ruleCount)
		for j := 0; j < ruleCount; j++ {
			rules = append(rules, g.randomRule(s.Resources.SecurityGroups))
		}
		p.SecurityGroups = append(p.SecurityGroups, plan.SecurityGroup{
			Name:  fmt.Sprintf("sg-%04d", i+1),
			Rules: rules,
		})
	}

	// Each network gets a random number of ports, each with a random number of
	// distinct security groups and one auto-allocated fixed IP on a subnet of
	// its own network (when the network has any subnets).
	p.Ports = make([]plan.Port, 0, len(p.Networks))
	portCount := 0
	for _, n := range p.Networks {
		for j := 0; j < randRange(g.rng, s.Distribution.PortsPerNetwork); j++ {
			portCount++
			port := plan.Port{
				Name:           fmt.Sprintf("port-%04d", portCount),
				Network:        n.Name,
				FixedIPs:       []plan.FixedIP{},
				SecurityGroups: []string{},
			}
			sgCount := randRange(g.rng, s.Topology.PortSecurityGroupCount)
			if numSGs := len(p.SecurityGroups); numSGs > 0 {
				if sgCount > numSGs {
					sgCount = numSGs
				}
				for _, idx := range g.rng.Perm(numSGs)[:sgCount] {
					port.SecurityGroups = append(port.SecurityGroups, p.SecurityGroups[idx].Name)
				}
			}
			if subnets := subnetsByNetwork[n.Name]; len(subnets) > 0 {
				port.FixedIPs = append(port.FixedIPs, plan.FixedIP{Subnet: subnets[g.rng.Intn(len(subnets))]})
			}
			p.Ports = append(p.Ports, port)
		}
	}

	// Router-to-router links. Each link adds a dedicated transit network, a /30
	// transit subnet, and a port, then wires two distinct routers together: one
	// router attaches to the subnet (owning the gateway address) and the other
	// attaches through the explicit port. The underlying resources are appended
	// to the existing slices so they create, quota-check, and clean up through
	// the same paths as any other network/subnet/port. Generated only when
	// requested, so link-free plans are byte-identical to before this feature.
	if s.Resources.RouterLinks > 0 && len(p.Routers) >= 2 {
		if err := g.appendRouterLinks(p, s.Resources.RouterLinks); err != nil {
			return nil, err
		}
	}

	// Floating IPs, allocated from the external network resolved at apply time.
	// A fraction target an internal port that is reachable through an
	// external-gateway router; the rest stay unassociated. Each eligible port is
	// targeted at most once. Generated only when requested.
	if s.Resources.FloatingIPs > 0 {
		g.appendFloatingIPs(p, s.Resources.FloatingIPs, s.Distribution.FloatingIPAssociatedRatio)
	}

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("generated plan failed validation: %w", err)
	}
	return p, nil
}

// appendRouterLinks adds count router-to-router interconnects to p. For each
// link it picks two distinct routers, allocates a transit subnet, and appends a
// transit network, subnet, and port plus the two router interfaces that wire the
// routers together. The caller guarantees len(p.Routers) >= 2.
func (g *generator) appendRouterLinks(p *plan.Plan, count int) error {
	for i := 0; i < count; i++ {
		ia := g.rng.Intn(len(p.Routers))
		ib := g.rng.Intn(len(p.Routers) - 1)
		if ib >= ia {
			ib++ // map [0,n-1) onto the routers other than ia, so a != b
		}
		routerA := p.Routers[ia].Name
		routerB := p.Routers[ib].Name

		cidr, portIP, err := g.nextLinkSubnet()
		if err != nil {
			return err
		}

		netName := fmt.Sprintf("link-net-%04d", i+1)
		subnetName := fmt.Sprintf("link-subnet-%04d", i+1)
		portName := fmt.Sprintf("link-port-%04d", i+1)

		p.Networks = append(p.Networks, plan.Network{Name: netName})
		p.Subnets = append(p.Subnets, plan.Subnet{
			Name:      subnetName,
			Network:   netName,
			IPVersion: 4,
			CIDR:      cidr,
			// A /30 transit subnet has exactly one host address (the link port's);
			// leaving DHCP on would let Neutron's DHCP port grab it first and make
			// the link port fail with IpAddressAlreadyAllocated.
			DisableDHCP: true,
		})
		p.Ports = append(p.Ports, plan.Port{
			Name:           portName,
			Network:        netName,
			FixedIPs:       []plan.FixedIP{{Subnet: subnetName, IPAddress: portIP}},
			SecurityGroups: []string{},
		})
		p.RouterInterfaces = append(p.RouterInterfaces,
			plan.RouterInterface{Name: fmt.Sprintf("link-rif-a-%04d", i+1), Router: routerA, Subnet: subnetName},
			plan.RouterInterface{Name: fmt.Sprintf("link-rif-b-%04d", i+1), Router: routerB, Port: portName},
		)
	}
	return nil
}

// appendFloatingIPs adds count floating IPs to p, associating a fraction
// (associatedRatio) of them with an internal port that is reachable through an
// external-gateway router. Eligible ports are those whose fixed IP sits on a
// subnet attached to a gateway router and that are not themselves a router
// interface; each is targeted at most once so an association never collides.
func (g *generator) appendFloatingIPs(p *plan.Plan, count int, associatedRatio float64) {
	eligible := eligibleFloatingIPPorts(p)
	order := g.rng.Perm(len(eligible))
	used := 0

	p.FloatingIPs = make([]plan.FloatingIP, 0, count)
	for i := 0; i < count; i++ {
		fip := plan.FloatingIP{Name: fmt.Sprintf("fip-%04d", i+1)}
		if used < len(eligible) && g.rng.Float64() < associatedRatio {
			fip.Port = eligible[order[used]]
			used++
		}
		p.FloatingIPs = append(p.FloatingIPs, fip)
	}
}

// eligibleFloatingIPPorts returns, in plan order, the ports a floating IP may be
// associated with: a port whose fixed IP is on an IPv4 subnet attached to a
// router that has an external gateway, excluding ports that are themselves
// consumed as a router interface. The IPv4 requirement matters because a
// floating IP is an IPv4 resource — Neutron rejects associating one with a port
// that has no fixed IPv4 address (e.g. a port on an IPv6 subnet).
func eligibleFloatingIPPorts(p *plan.Plan) []string {
	gatewayRouter := make(map[string]bool, len(p.Routers))
	for _, r := range p.Routers {
		if r.ExternalGateway {
			gatewayRouter[r.Name] = true
		}
	}

	ipv4Subnet := make(map[string]bool, len(p.Subnets))
	for _, s := range p.Subnets {
		if s.IPVersion == 4 {
			ipv4Subnet[s.Name] = true
		}
	}

	externalSubnet := make(map[string]bool)
	interfacePort := make(map[string]bool)
	for _, ri := range p.RouterInterfaces {
		if ri.Subnet != "" && gatewayRouter[ri.Router] && ipv4Subnet[ri.Subnet] {
			externalSubnet[ri.Subnet] = true
		}
		if ri.Port != "" {
			interfacePort[ri.Port] = true
		}
	}

	var eligible []string
	for _, pt := range p.Ports {
		if interfacePort[pt.Name] {
			continue
		}
		for _, fip := range pt.FixedIPs {
			if externalSubnet[fip.Subnet] {
				eligible = append(eligible, pt.Name)
				break
			}
		}
	}
	return eligible
}

// generator carries the RNG and the monotonic IP-block cursors used while
// expanding a scenario.
type generator struct {
	rng      *rand.Rand
	ipv4Next uint32
	ipv6Next uint32
	poolNext int
	linkNext uint32
}

// randomRule draws a single valid security-group rule. numSGs is the total
// number of security groups in the plan, used so a remote-group reference
// resolves to a generated group.
func (g *generator) randomRule(numSGs int) plan.SecurityGroupRule {
	etherType := []string{"IPv4", "IPv6"}[g.rng.Intn(2)]
	rule := plan.SecurityGroupRule{
		Direction: []string{"ingress", "egress"}[g.rng.Intn(2)],
		EtherType: etherType,
		Protocol:  []string{"tcp", "udp", "icmp"}[g.rng.Intn(3)],
	}
	if rule.Protocol != "icmp" {
		port := g.rng.Intn(65535) + 1
		rule.PortRangeMin = port
		rule.PortRangeMax = port
	}
	switch {
	case g.rng.Float64() < 0.5:
		rule.RemoteGroup = fmt.Sprintf("sg-%04d", g.rng.Intn(numSGs)+1)
	case etherType == "IPv6":
		rule.RemoteIPPrefix = "::/0"
	default:
		rule.RemoteIPPrefix = "0.0.0.0/0"
	}
	return rule
}

// nextIPv4CIDR returns the next /24 block from 10.0.0.0/8.
func (g *generator) nextIPv4CIDR() (string, error) {
	if g.ipv4Next >= ipv4BlockCount {
		return "", fmt.Errorf("exhausted IPv4 /24 blocks in 10.0.0.0/8")
	}
	addr := ipv4Base + g.ipv4Next*256
	g.ipv4Next++
	a := netip.AddrFrom4([4]byte{byte(addr >> 24), byte(addr >> 16), byte(addr >> 8), byte(addr)})
	return netip.PrefixFrom(a, 24).String(), nil
}

// nextPoolPrefix returns the next /16 prefix from 172.16.0.0/12.
func (g *generator) nextPoolPrefix() (string, error) {
	if g.poolNext >= poolBlockCount {
		return "", fmt.Errorf("exhausted /16 prefixes in 172.16.0.0/12")
	}
	addr := poolBase + uint32(g.poolNext)*65536
	g.poolNext++
	a := netip.AddrFrom4([4]byte{byte(addr >> 24), byte(addr >> 16), byte(addr >> 8), byte(addr)})
	return netip.PrefixFrom(a, 16).String(), nil
}

// nextLinkSubnet returns the next /30 transit block from 192.168.0.0/16 as a
// CIDR together with the second usable address in that block. The first usable
// address is the subnet's default gateway (taken by the router attached to the
// subnet); the returned address is assigned to the port the peer router
// attaches through.
func (g *generator) nextLinkSubnet() (cidr, portIP string, err error) {
	if g.linkNext >= linkBlockCount {
		return "", "", fmt.Errorf("exhausted /30 transit blocks in 192.168.0.0/16")
	}
	base := linkBase + g.linkNext*4
	g.linkNext++
	network := netip.AddrFrom4([4]byte{byte(base >> 24), byte(base >> 16), byte(base >> 8), byte(base)})
	port := base + 2
	portAddr := netip.AddrFrom4([4]byte{byte(port >> 24), byte(port >> 16), byte(port >> 8), byte(port)})
	return netip.PrefixFrom(network, linkPrefixLen).String(), portAddr.String(), nil
}

// nextIPv6CIDR returns the next /64 block from fd00::/16.
func (g *generator) nextIPv6CIDR() (string, error) {
	if g.ipv6Next >= ipv6BlockCount {
		return "", fmt.Errorf("exhausted IPv6 /64 blocks in fd00::/16")
	}
	var b [16]byte
	b[0] = 0xfd
	b[6] = byte(g.ipv6Next >> 8)
	b[7] = byte(g.ipv6Next)
	g.ipv6Next++
	return netip.PrefixFrom(netip.AddrFrom16(b), 64).String(), nil
}

// randRange returns a uniformly random integer in the inclusive interval
// [r.Min, r.Max]. The caller guarantees r.Min <= r.Max via Scenario.Validate.
func randRange(rng *rand.Rand, r Range) int {
	return r.Min + rng.Intn(r.Max-r.Min+1)
}
