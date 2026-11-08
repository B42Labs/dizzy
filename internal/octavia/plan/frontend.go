package plan

import "fmt"

// Frontend is the Octavia part of a plan whose backends another plan creates:
// load balancers whose VIP sits on a network of that plan, and members that
// are servers of it. Only the mix generator builds one, for the Gardener persona.
type Frontend struct {
	Provider       string          `json:"provider,omitempty"`
	LoadBalancers  []LoadBalancer  `json:"loadBalancers"`
	Listeners      []Listener      `json:"listeners"`
	Pools          []Pool          `json:"pools"`
	Members        []ServerMember  `json:"members"`
	HealthMonitors []HealthMonitor `json:"healthMonitors"`
}

// ServerMember is a member of Pool backed by the server Server (a logical
// name of the other plan) on Port. Its address is read when it is created.
type ServerMember struct {
	Name   string `json:"name"`
	Pool   string `json:"pool"`
	Server string `json:"server"`
	Port   int    `json:"port"`
}

// Validate checks the frontend for well-formedness: every load balancer has a
// non-empty name unique among them and names a network; the listeners, pools,
// and health monitors pass the checks Plan.Validate applies to them, with the
// same error texts; and every member has a non-empty name unique among them,
// references a known pool, is named MemberName(<its pool>, n) for an n of at
// least 1, names a server, has a port in 1 to 65535, and puts its server into
// its pool only once. Empty collections are valid. Whether the networks and
// servers exist is the caller's check, since only the plan that creates them
// knows. It returns an error naming the first offender.
func (f *Frontend) Validate() error {
	v := validator{
		lbNetwork:    make(map[string]string, len(f.LoadBalancers)),
		listenerLB:   make(map[string]string, len(f.Listeners)),
		poolListener: make(map[string]string, len(f.Pools)),
	}
	if err := v.frontendLoadBalancers(f); err != nil {
		return err
	}
	// The listener, pool, and health monitor checks read only these four
	// collections and the lookups filled before them, so a Plan holding just
	// those runs them unchanged.
	p := &Plan{
		LoadBalancers:  f.LoadBalancers,
		Listeners:      f.Listeners,
		Pools:          f.Pools,
		HealthMonitors: f.HealthMonitors,
	}
	for _, check := range []func(*Plan) error{
		v.listeners,
		v.pools,
		v.healthMonitors,
	} {
		if err := check(p); err != nil {
			return err
		}
	}
	return v.serverMembers(f)
}

// frontendLoadBalancers checks every load balancer's name and that it names a
// network, and records each load balancer's network. Unlike loadBalancers it
// cannot resolve the network, which another plan creates.
func (v *validator) frontendLoadBalancers(f *Frontend) error {
	for i, lb := range f.LoadBalancers {
		if err := claimName("load balancer", i, lb.Name, v.lbNetwork); err != nil {
			return err
		}
		if lb.Network == "" {
			return fmt.Errorf("load balancer %q has an empty network", lb.Name)
		}
		v.lbNetwork[lb.Name] = lb.Network
	}
	return nil
}

// serverMembers checks every member's name, pool reference, server, and port,
// and rejects a second member of one pool backed by the same server: a server
// is one backend of a pool, however many pools it serves.
func (v *validator) serverMembers(f *Frontend) error {
	names := make(map[string]bool, len(f.Members))
	placed := make(map[string]map[string]string, len(f.Pools)) // pool to server to member
	for i, m := range f.Members {
		if err := claimName("member", i, m.Name, names); err != nil {
			return err
		}
		names[m.Name] = true
		if _, ok := v.poolListener[m.Pool]; !ok {
			return fmt.Errorf("member %q references unknown pool %q", m.Name, m.Pool)
		}
		if !isMemberName(m.Name, m.Pool) {
			return fmt.Errorf("member %q of pool %q is not named %s-member-<nn>", m.Name, m.Pool, m.Pool)
		}
		if m.Server == "" {
			return fmt.Errorf("member %q has an empty server", m.Name)
		}
		if !validPort(m.Port) {
			return fmt.Errorf("member %q has port %d, want %d to %d", m.Name, m.Port, minPort, maxPort)
		}
		if other, ok := placed[m.Pool][m.Server]; ok {
			return fmt.Errorf("member %q puts server %q into pool %q a second time, after member %q", m.Name, m.Server, m.Pool, other)
		}
		if placed[m.Pool] == nil {
			placed[m.Pool] = make(map[string]string)
		}
		placed[m.Pool][m.Server] = m.Name
	}
	return nil
}
