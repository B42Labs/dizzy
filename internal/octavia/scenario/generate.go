package scenario

import (
	"fmt"
	"math/rand"
	"net/netip"
	"slices"

	"github.com/B42Labs/dizzy/internal/octavia/plan"
)

// ipv4Base is 10.0.0.0; each VIP network takes the next /24 block above it
// (10.0.1.0/24, 10.0.2.0/24, …), so the at most 255 networks Validate allows
// never collide and never overflow an octet.
const ipv4Base = uint32(10) << 24

// firstMemberOctet is the last octet of the first member address on every VIP
// network: members count up from .128, the start of the upper half of the /24.
const firstMemberOctet = 128

// Generate expands the scenario and its seed into a fully-enumerated plan. The
// same scenario and seed always produce a byte-identical plan: the generator
// draws from math/rand v1 (whose sequence is frozen for compatibility) in a
// fixed order and emits every collection in a fixed order. The returned plan is
// validated before it is handed back.
//
// The assignment draws nothing: networks net-%04d (each with its subnet
// sub-%04d and the /24 10.0.<i+1>.0/24) come first, then load balancers
// lb-%04d round-robin over the networks, listeners lsn-%04d round-robin over
// the load balancers, one pool pool-%04d per listener, and members round-robin
// over the pools, each taking the next unused address of its network counting
// up from .128. Only three things are drawn, in this order: each listener's port
// in listener order, stepping past a port another listener of its load balancer
// holds; each member's port in member order; and, only when health_monitors > 0,
// a permutation of the pools whose first health_monitors indices, sorted
// ascending, pick the pools that get a health monitor hm-%04d. Every slice is
// allocated non-nil, so an empty collection encodes as [] rather than null.
func (s Scenario) Generate() (*plan.Plan, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid scenario: %w", err)
	}

	r := s.Resources
	rng := rand.New(rand.NewSource(s.Seed))
	p := &plan.Plan{Scenario: s.Name, Seed: s.Seed, Provider: s.Provider}

	p.Networks = make([]plan.Network, 0, r.Networks)
	for i := 0; i < r.Networks; i++ {
		p.Networks = append(p.Networks, plan.Network{
			Name:   networkName(i),
			Subnet: fmt.Sprintf("sub-%04d", i+1),
			CIDR:   netip.PrefixFrom(addrFrom(networkBlock(i)), 24).String(),
		})
	}

	p.LoadBalancers = make([]plan.LoadBalancer, 0, r.LoadBalancers)
	for i := 0; i < r.LoadBalancers; i++ {
		p.LoadBalancers = append(p.LoadBalancers, plan.LoadBalancer{
			Name:    fmt.Sprintf("lb-%04d", i+1),
			Network: p.Networks[i%r.Networks].Name,
		})
	}

	p.Listeners = make([]plan.Listener, 0, r.Listeners)
	taken := make([]map[int]bool, r.LoadBalancers) // load balancer index to its listeners' ports
	for i := 0; i < r.Listeners; i++ {
		lb := i % r.LoadBalancers
		if taken[lb] == nil {
			taken[lb] = make(map[int]bool)
		}
		port := freePort(randRange(rng, s.Distribution.ListenerPort), s.Distribution.ListenerPort, taken[lb])
		taken[lb][port] = true
		p.Listeners = append(p.Listeners, plan.Listener{
			Name:         fmt.Sprintf("lsn-%04d", i+1),
			LoadBalancer: p.LoadBalancers[lb].Name,
			Port:         port,
		})
	}

	p.Pools = make([]plan.Pool, 0, r.Listeners)
	for i := 0; i < r.Listeners; i++ {
		p.Pools = append(p.Pools, plan.Pool{
			Name:     fmt.Sprintf("pool-%04d", i+1),
			Listener: p.Listeners[i].Name,
		})
	}

	p.Members = make([]plan.Member, 0, r.Members)
	poolMembers := make([]int, len(p.Pools)) // pool index to the members it has so far
	nextOctet := make([]int, r.Networks)     // network index to its next unused member octet
	for i := range nextOctet {
		nextOctet[i] = firstMemberOctet
	}
	for m := 0; m < r.Members; m++ {
		pool := m % len(p.Pools)
		// Pool i belongs to listener i, whose load balancer and network follow
		// the round-robins above, as Validate's membersPerNetwork counts them.
		network := (pool % r.LoadBalancers) % r.Networks
		poolMembers[pool]++
		p.Members = append(p.Members, plan.Member{
			Name:    plan.MemberName(p.Pools[pool].Name, poolMembers[pool]),
			Pool:    p.Pools[pool].Name,
			Address: addrFrom(networkBlock(network) + uint32(nextOctet[network])).String(),
			Port:    randRange(rng, s.Distribution.MemberPort),
		})
		nextOctet[network]++
	}

	p.HealthMonitors = make([]plan.HealthMonitor, 0, r.HealthMonitors)
	if r.HealthMonitors > 0 {
		// Validate caps health_monitors at the pool count, so the permutation
		// always holds at least that many indices.
		idx := rng.Perm(len(p.Pools))[:r.HealthMonitors]
		slices.Sort(idx)
		for i, j := range idx {
			p.HealthMonitors = append(p.HealthMonitors, plan.HealthMonitor{
				Name: fmt.Sprintf("hm-%04d", i+1),
				Pool: p.Pools[j].Name,
			})
		}
	}

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("generated plan failed validation: %w", err)
	}
	return p, nil
}

// freePort returns port when taken does not hold it, and otherwise the next
// port up that taken does not hold, wrapping from r.Max to r.Min. The caller
// guarantees a free port in r: Validate requires the range to span at least as
// many ports as one load balancer gets listeners.
func freePort(port int, r Range, taken map[int]bool) int {
	for taken[port] {
		port++
		if port > r.Max {
			port = r.Min
		}
	}
	return port
}

// networkBlock returns the first address of the i-th (0-based) VIP network's
// /24, 10.0.<i+1>.0, as a uint32.
func networkBlock(i int) uint32 {
	return ipv4Base + uint32(i+1)*256
}

// addrFrom returns the IPv4 address whose big-endian value is a.
func addrFrom(a uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(a >> 24), byte(a >> 16), byte(a >> 8), byte(a)})
}

// randRange returns a uniformly random integer in the inclusive interval
// [r.Min, r.Max]. The caller guarantees r.Min <= r.Max via Scenario.Validate.
func randRange(rng *rand.Rand, r Range) int {
	return r.Min + rng.Intn(r.Max-r.Min+1)
}
