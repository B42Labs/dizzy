// Package plan defines the expanded, fully-enumerated set of Octavia resources
// (VIP networks with their subnets, load balancers, listeners, pools, members,
// and health monitors): the expected-state source of truth produced
// deterministically from a scenario plus a seed. Like the other services' plans
// it is pure data: every collection is a slice (never a map) so that encoding
// the plan to JSON yields byte-identical output for the same input. The VIP
// networks and their subnets are created by dizzy itself; a member is a
// synthetic address in the upper half of its network's /24, and no backend
// answers on it.
package plan

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Plan is the fully-expanded expected state for one Octavia run. Scenario and
// Seed record the provenance that produced it; Provider names the Octavia
// provider driver every load balancer requests, empty for the cloud's default.
// The slices enumerate every VIP network, load balancer, listener, pool,
// member, and health monitor. Cross-resource references are by logical name,
// resolved by Validate.
type Plan struct {
	Scenario       string          `json:"scenario"`
	Seed           int64           `json:"seed"`
	Provider       string          `json:"provider,omitempty"`
	Networks       []Network       `json:"networks"`
	LoadBalancers  []LoadBalancer  `json:"loadBalancers"`
	Listeners      []Listener      `json:"listeners"`
	Pools          []Pool          `json:"pools"`
	Members        []Member        `json:"members"`
	HealthMonitors []HealthMonitor `json:"healthMonitors"`
}

// Network is a VIP network dizzy creates for its load balancers, together with
// the single subnet it carries. Subnet is the subnet's logical name and CIDR its
// IPv4 /24: the lower half holds the gateway and the addresses Neutron
// allocates, the upper half (.128 to .254) the plan's member addresses.
type Network struct {
	Name   string `json:"name"`
	Subnet string `json:"subnet"`
	CIDR   string `json:"cidr"`
}

// LoadBalancer is one load balancer whose VIP sits on the subnet of Network.
type LoadBalancer struct {
	Name    string `json:"name"`
	Network string `json:"network"`
}

// Listener is one TCP listener of LoadBalancer on Port. No two listeners of one
// load balancer share a port.
type Listener struct {
	Name         string `json:"name"`
	LoadBalancer string `json:"loadBalancer"`
	Port         int    `json:"port"`
}

// Pool is the default pool of Listener. A listener has at most one pool, so the
// pool's load balancer is its listener's.
type Pool struct {
	Name     string `json:"name"`
	Listener string `json:"listener"`
}

// Member is one synthetic member of Pool at Address and Port. Its name is
// MemberName(Pool, n), so MemberPool recovers the pool from the name alone.
// Address lies in .128 to .254 of the CIDR of the network its pool's load
// balancer sits on, and no other member on that network uses it.
type Member struct {
	Name    string `json:"name"`
	Pool    string `json:"pool"`
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// HealthMonitor is the TCP health monitor of Pool. A pool has at most one.
type HealthMonitor struct {
	Name string `json:"name"`
	Pool string `json:"pool"`
}

// memberSeparator joins a pool's logical name and a member number in a
// member's logical name.
const memberSeparator = "-member-"

// Inclusive bounds of a listener or member port.
const (
	minPort = 1
	maxPort = 65535
)

// Inclusive bounds of the last octet of a member address: the upper half of the
// /24 without its broadcast address.
const (
	minMemberOctet = 128
	maxMemberOctet = 254
)

// MemberName returns the logical name of the n-th member of pool, counted from
// 1: the pool's name, "-member-", and n with at least two digits.
func MemberName(pool string, n int) string {
	return fmt.Sprintf("%s-member-%02d", pool, n)
}

// MemberPool returns the logical name of the pool a member's logical name
// belongs to: the part before the last "-member-". It returns false when the
// name holds no "-member-" or nothing precedes it. A run record does not carry
// a member's pool, so status recovers it from the name this way.
func MemberPool(member string) (string, bool) {
	i := strings.LastIndex(member, memberSeparator)
	if i <= 0 {
		return "", false
	}
	return member[:i], true
}

// Validate checks the plan graph for well-formedness: every network, subnet,
// load balancer, listener, pool, member, and health monitor has a non-empty
// name unique within its collection; every network's CIDR is a masked IPv4
// /24; every load balancer, listener, pool, member, and health monitor
// references a known network, load balancer, listener, pool, and pool
// respectively; every listener and member port lies in 1 to 65535, and no two
// listeners of one load balancer share a port; no listener has a second pool
// and no pool a second health monitor; every member is named MemberName(<its
// pool>, n) for an n of at least 1; and every member address lies in .128 to
// .254 of the network its pool's listener's load balancer sits on, used once on
// that network. Empty collections are valid. It returns an error naming the
// first offender.
func (p *Plan) Validate() error {
	v := validator{
		cidrs:        make(map[string]netip.Prefix, len(p.Networks)),
		lbNetwork:    make(map[string]string, len(p.LoadBalancers)),
		listenerLB:   make(map[string]string, len(p.Listeners)),
		poolListener: make(map[string]string, len(p.Pools)),
	}
	for _, check := range []func(*Plan) error{
		v.networks,
		v.loadBalancers,
		v.listeners,
		v.pools,
		v.members,
		v.healthMonitors,
	} {
		if err := check(p); err != nil {
			return err
		}
	}
	return nil
}

// validator carries the lookups Validate builds collection by collection, in
// plan order, so that each later collection resolves its references and
// reaches the network a member's address must lie on.
type validator struct {
	cidrs        map[string]netip.Prefix // network name to its parsed CIDR
	lbNetwork    map[string]string       // load balancer name to its network
	listenerLB   map[string]string       // listener name to its load balancer
	poolListener map[string]string       // pool name to its listener
}

// networks checks every network's and subnet's name and every network's CIDR,
// and records each network's parsed CIDR.
func (v *validator) networks(p *Plan) error {
	subnets := make(map[string]bool, len(p.Networks))
	for i, n := range p.Networks {
		if err := claimName("network", i, n.Name, v.cidrs); err != nil {
			return err
		}
		if n.Subnet == "" {
			return fmt.Errorf("network %q has an empty subnet name", n.Name)
		}
		if subnets[n.Subnet] {
			return fmt.Errorf("duplicate subnet name %q", n.Subnet)
		}
		subnets[n.Subnet] = true
		prefix, ok := parseVIPCIDR(n.CIDR)
		if !ok {
			return fmt.Errorf("network %q has CIDR %q, want a masked IPv4 /24 such as 10.0.1.0/24", n.Name, n.CIDR)
		}
		v.cidrs[n.Name] = prefix
	}
	return nil
}

// loadBalancers checks every load balancer's name and network reference, and
// records each load balancer's network.
func (v *validator) loadBalancers(p *Plan) error {
	for i, lb := range p.LoadBalancers {
		if err := claimName("load balancer", i, lb.Name, v.lbNetwork); err != nil {
			return err
		}
		if _, ok := v.cidrs[lb.Network]; !ok {
			return fmt.Errorf("load balancer %q references unknown network %q", lb.Name, lb.Network)
		}
		v.lbNetwork[lb.Name] = lb.Network
	}
	return nil
}

// listeners checks every listener's name, load balancer reference, and port,
// rejects a port another listener of the same load balancer already uses, and
// records each listener's load balancer.
func (v *validator) listeners(p *Plan) error {
	taken := make(map[string]map[int]string, len(p.LoadBalancers)) // load balancer to port to listener
	for i, l := range p.Listeners {
		if err := claimName("listener", i, l.Name, v.listenerLB); err != nil {
			return err
		}
		if _, ok := v.lbNetwork[l.LoadBalancer]; !ok {
			return fmt.Errorf("listener %q references unknown load balancer %q", l.Name, l.LoadBalancer)
		}
		if !validPort(l.Port) {
			return fmt.Errorf("listener %q has port %d, want %d to %d", l.Name, l.Port, minPort, maxPort)
		}
		if other, ok := taken[l.LoadBalancer][l.Port]; ok {
			return fmt.Errorf("listener %q uses port %d of load balancer %q, already taken by listener %q", l.Name, l.Port, l.LoadBalancer, other)
		}
		if taken[l.LoadBalancer] == nil {
			taken[l.LoadBalancer] = make(map[int]string)
		}
		taken[l.LoadBalancer][l.Port] = l.Name
		v.listenerLB[l.Name] = l.LoadBalancer
	}
	return nil
}

// pools checks every pool's name and listener reference, rejects a second pool
// on one listener, and records each pool's listener.
func (v *validator) pools(p *Plan) error {
	listenerPool := make(map[string]string, len(p.Pools))
	for i, pl := range p.Pools {
		if err := claimName("pool", i, pl.Name, v.poolListener); err != nil {
			return err
		}
		if _, ok := v.listenerLB[pl.Listener]; !ok {
			return fmt.Errorf("pool %q references unknown listener %q", pl.Name, pl.Listener)
		}
		if other, ok := listenerPool[pl.Listener]; ok {
			return fmt.Errorf("pool %q is a second pool on listener %q, which already has pool %q", pl.Name, pl.Listener, other)
		}
		listenerPool[pl.Listener] = pl.Name
		v.poolListener[pl.Name] = pl.Listener
	}
	return nil
}

// members checks every member's name, pool reference, port, and address. The
// address must lie in .128 to .254 of the network reached through the
// member's pool, listener, and load balancer, and be used once on it.
func (v *validator) members(p *Plan) error {
	names := make(map[string]bool, len(p.Members))
	used := make(map[string]map[netip.Addr]string, len(v.cidrs)) // network to address to member
	for i, m := range p.Members {
		if err := claimName("member", i, m.Name, names); err != nil {
			return err
		}
		names[m.Name] = true
		if _, ok := v.poolListener[m.Pool]; !ok {
			return fmt.Errorf("member %q references unknown pool %q", m.Name, m.Pool)
		}
		if pool, ok := MemberPool(m.Name); ok && pool != m.Pool {
			return fmt.Errorf("member %q is named for pool %q, not its pool %q", m.Name, pool, m.Pool)
		}
		if !isMemberName(m.Name, m.Pool) {
			return fmt.Errorf("member %q of pool %q is not named %s-member-<nn>", m.Name, m.Pool, m.Pool)
		}
		if !validPort(m.Port) {
			return fmt.Errorf("member %q has port %d, want %d to %d", m.Name, m.Port, minPort, maxPort)
		}
		addr, err := netip.ParseAddr(m.Address)
		if err != nil || !addr.Is4() {
			return fmt.Errorf("member %q has address %q, want an IPv4 address", m.Name, m.Address)
		}
		network := v.lbNetwork[v.listenerLB[v.poolListener[m.Pool]]]
		prefix := v.cidrs[network]
		if octet := addr.As4()[3]; !prefix.Contains(addr) || octet < minMemberOctet || octet > maxMemberOctet {
			return fmt.Errorf("member %q has address %s, want .%d to .%d of network %q (%s)", m.Name, m.Address, minMemberOctet, maxMemberOctet, network, prefix)
		}
		if other, ok := used[network][addr]; ok {
			return fmt.Errorf("member %q has address %s on network %q, already used by member %q", m.Name, m.Address, network, other)
		}
		if used[network] == nil {
			used[network] = make(map[netip.Addr]string)
		}
		used[network][addr] = m.Name
	}
	return nil
}

// healthMonitors checks every health monitor's name and pool reference, and
// rejects a second health monitor on one pool.
func (v *validator) healthMonitors(p *Plan) error {
	names := make(map[string]bool, len(p.HealthMonitors))
	poolMonitor := make(map[string]string, len(p.HealthMonitors))
	for i, h := range p.HealthMonitors {
		if err := claimName("health monitor", i, h.Name, names); err != nil {
			return err
		}
		names[h.Name] = true
		if _, ok := v.poolListener[h.Pool]; !ok {
			return fmt.Errorf("health monitor %q references unknown pool %q", h.Name, h.Pool)
		}
		if other, ok := poolMonitor[h.Pool]; ok {
			return fmt.Errorf("health monitor %q is a second health monitor on pool %q, which already has health monitor %q", h.Name, h.Pool, other)
		}
		poolMonitor[h.Pool] = h.Name
	}
	return nil
}

// claimName returns an error when name, the logical name of the i-th (0-based)
// entry of a collection of kind, is empty or already a key of seen, the names
// of the entries before it. The caller adds name to seen afterwards.
func claimName[V any](kind string, i int, name string, seen map[string]V) error {
	if name == "" {
		return fmt.Errorf("%s at index %d has an empty name", kind, i)
	}
	if _, ok := seen[name]; ok {
		return fmt.Errorf("duplicate %s name %q", kind, name)
	}
	return nil
}

// parseVIPCIDR parses cidr as a VIP network's CIDR, which must be an IPv4 /24
// in its masked form (10.0.1.0/24, not 10.0.1.5/24), and reports whether it is.
func parseVIPCIDR(cidr string) (netip.Prefix, bool) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 24 || prefix != prefix.Masked() {
		return netip.Prefix{}, false
	}
	return prefix, true
}

// isMemberName reports whether member is MemberName(pool, n) for a member
// number n of at least 1. Comparing against MemberName rejects every other
// spelling of the number, such as "1" or "001" for n = 1.
func isMemberName(member, pool string) bool {
	number, ok := strings.CutPrefix(member, pool+memberSeparator)
	if !ok {
		return false
	}
	n, err := strconv.Atoi(number)
	return err == nil && n >= 1 && MemberName(pool, n) == member
}

// validPort reports whether port is a TCP port a listener or member may use.
func validPort(port int) bool {
	return port >= minPort && port <= maxPort
}

// Summary returns a deterministic, human-readable count of the plan, used by
// "octavia apply --dry-run" to preview a scenario without touching a cloud. An
// empty provider reads "cloud default", the provider Octavia then selects.
func (p *Plan) Summary() string {
	provider := p.Provider
	if provider == "" {
		provider = "cloud default"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Plan for scenario %q (seed %d)\n", p.Scenario, p.Seed)
	fmt.Fprintf(&b, "  provider:         %s\n", provider)
	fmt.Fprintf(&b, "  networks:         %d\n", len(p.Networks))
	fmt.Fprintf(&b, "  load balancers:   %d\n", len(p.LoadBalancers))
	fmt.Fprintf(&b, "  listeners:        %d\n", len(p.Listeners))
	fmt.Fprintf(&b, "  pools:            %d\n", len(p.Pools))
	fmt.Fprintf(&b, "  members:          %d\n", len(p.Members))
	fmt.Fprintf(&b, "  health monitors:  %d\n", len(p.HealthMonitors))
	return b.String()
}
