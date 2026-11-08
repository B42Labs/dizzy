// Package plan defines the expanded plan of a combined (mix) run: one entry per
// workload persona that received at least one server, each carrying its share
// of the server envelope, its derived seed and the compute plan it churns, and
// one entry per enabled background lane, each carrying its derived seed and
// the plan of the service it churns. Like the per-service plans it is pure
// data, so the same scenario and seed encode to byte-identical JSON.
package plan

import (
	"fmt"
	"slices"

	cinderplan "github.com/B42Labs/dizzy/internal/cinder/plan"
	glanceplan "github.com/B42Labs/dizzy/internal/glance/plan"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	octaviaplan "github.com/B42Labs/dizzy/internal/octavia/plan"
	neutronplan "github.com/B42Labs/dizzy/internal/plan"
)

// ServiceOctavia is the opt-in service that gives the gardener persona load
// balancers in front of its clusters. It is a name of Plan.Services, the list
// a scenario's services key and a run record share.
const ServiceOctavia = "octavia"

// Plan is the expanded state of one mix run. Scenario and Seed record the
// provenance that produced it, Services lists the opt-in services bound to the
// personas, Personas lists the personas in canonical (alphabetical) order, and
// Lanes lists the enabled background lanes in canonical order. A plan without
// lanes encodes without a lanes key.
type Plan struct {
	Scenario string    `json:"scenario"`
	Seed     int64     `json:"seed"`
	Services []string  `json:"services"`
	Personas []Persona `json:"personas"`
	Lanes    []Lane    `json:"lanes,omitempty"`
}

// Persona is one workload persona of a mix run. Share is its normalized share,
// its scenario share divided by the sum of all shares, and Servers its part of
// the scenario's server envelope. Seed is derived from the plan seed and the
// persona name. LongLived marks a persona whose resources are kept until the
// run's teardown and mutated repeatedly. Rolling marks a persona whose servers
// are replaced one at a time within their server group. Nova is the compute
// plan the persona churns. Octavia is the load-balancer frontend in front of
// the compute plan's servers; only the gardener persona of a plan whose
// services list octavia has one, so a plan without the opt-in encodes without
// an octavia key.
type Persona struct {
	Name      string                `json:"name"`
	Share     float64               `json:"share"`
	Servers   int                   `json:"servers"`
	Seed      int64                 `json:"seed"`
	LongLived bool                  `json:"longLived,omitempty"`
	Rolling   bool                  `json:"rolling,omitempty"`
	Nova      *novaplan.Plan        `json:"nova"`
	Octavia   *octaviaplan.Frontend `json:"octavia,omitempty"`
}

// Lane is one background lane of a mix run: the churn of a single service, in
// a churn engine of its own next to the personas. Name is the service, Seed is
// derived from the plan seed and the lane name, and the one plan matching the
// name is set.
type Lane struct {
	Name     string             `json:"name"`
	Seed     int64              `json:"seed"`
	Cinder   *cinderplan.Plan   `json:"cinder,omitempty"`
	Glance   *glanceplan.Plan   `json:"glance,omitempty"`
	Keystone *keystoneplan.Plan `json:"keystone,omitempty"`
	Neutron  *neutronplan.Plan  `json:"neutron,omitempty"`
}

// Scenario returns the scenario name of the lane's plan, empty when no plan is
// set.
func (l Lane) Scenario() string {
	switch {
	case l.Cinder != nil:
		return l.Cinder.Scenario
	case l.Glance != nil:
		return l.Glance.Scenario
	case l.Keystone != nil:
		return l.Keystone.Scenario
	case l.Neutron != nil:
		return l.Neutron.Scenario
	}
	return ""
}

// validate checks one lane: its name is a lane this build defines, the plan
// matching its name is set, and that plan is valid.
func (l Lane) validate() error {
	var (
		set      bool
		validate func() error
	)
	switch l.Name {
	case "cinder":
		set, validate = l.Cinder != nil, l.Cinder.Validate
	case "glance":
		set, validate = l.Glance != nil, l.Glance.Validate
	case "keystone":
		set, validate = l.Keystone != nil, l.Keystone.Validate
	case "neutron":
		set, validate = l.Neutron != nil, l.Neutron.Validate
	default:
		return fmt.Errorf("lane %q is not defined by this build of dizzy", l.Name)
	}
	if !set {
		return fmt.Errorf("lane %q has no %s plan", l.Name, l.Name)
	}
	if err := validate(); err != nil {
		return fmt.Errorf("lane %q: %w", l.Name, err)
	}
	return nil
}

// Validate checks the plan for well-formedness: at least one persona has a
// server, no persona appears twice, and every persona has a valid compute plan
// with exactly its share of servers. A persona with an octavia frontend needs
// services to list octavia, and its frontend must be valid and sit on the
// networks and servers of the persona's compute plan. No lane appears twice,
// and every lane is one this build defines with a valid plan of its service.
// It returns an error naming the first offending persona or lane.
func (p *Plan) Validate() error {
	hasServers := false
	for _, ps := range p.Personas {
		if ps.Servers >= 1 {
			hasServers = true
			break
		}
	}
	if !hasServers {
		return fmt.Errorf("plan has no persona with at least one server")
	}

	seen := make(map[string]bool, len(p.Personas))
	for _, ps := range p.Personas {
		if seen[ps.Name] {
			return fmt.Errorf("duplicate persona %q", ps.Name)
		}
		seen[ps.Name] = true
		if ps.Nova == nil {
			return fmt.Errorf("persona %q has no compute plan", ps.Name)
		}
		if got := len(ps.Nova.Servers); got != ps.Servers {
			return fmt.Errorf("persona %q plans %d servers but its share is %d", ps.Name, got, ps.Servers)
		}
		if err := ps.Nova.Validate(); err != nil {
			return fmt.Errorf("persona %q: %w", ps.Name, err)
		}
		if ps.Octavia != nil {
			if err := p.validateFrontend(ps); err != nil {
				return err
			}
		}
	}

	lanes := make(map[string]bool, len(p.Lanes))
	for _, l := range p.Lanes {
		if lanes[l.Name] {
			return fmt.Errorf("duplicate lane %q", l.Name)
		}
		lanes[l.Name] = true
		if err := l.validate(); err != nil {
			return err
		}
	}
	return nil
}

// validateFrontend checks the octavia frontend of persona ps, whose compute
// plan is valid: the plan's services list octavia, the frontend is valid on its
// own, every load balancer sits on a network of the compute plan, and every
// member is a server of the compute plan on the network of the load balancer
// it serves, reached through the member's pool and that pool's listener.
// Frontend.Validate leaves the networks and servers to this check because only
// the compute plan creates them, and a member off its load balancer's network
// would be unreachable from the VIP without a router.
func (p *Plan) validateFrontend(ps Persona) error {
	if !slices.Contains(p.Services, ServiceOctavia) {
		return fmt.Errorf("persona %q has an octavia plan, but services does not list %q", ps.Name, ServiceOctavia)
	}
	f := ps.Octavia
	if err := f.Validate(); err != nil {
		return fmt.Errorf("persona %q: octavia: %w", ps.Name, err)
	}

	networks := make(map[string]bool, len(ps.Nova.Networks))
	for _, n := range ps.Nova.Networks {
		networks[n.Name] = true
	}
	lbNetwork := make(map[string]string, len(f.LoadBalancers))
	for _, lb := range f.LoadBalancers {
		if !networks[lb.Network] {
			return fmt.Errorf("persona %q: load balancer %q sits on network %q, which the compute plan does not create", ps.Name, lb.Name, lb.Network)
		}
		lbNetwork[lb.Name] = lb.Network
	}

	// Frontend.Validate resolved every member's pool and every pool's
	// listener, so these lookups find the load balancer of each member.
	listenerLB := make(map[string]string, len(f.Listeners))
	for _, l := range f.Listeners {
		listenerLB[l.Name] = l.LoadBalancer
	}
	poolLB := make(map[string]string, len(f.Pools))
	for _, pl := range f.Pools {
		poolLB[pl.Name] = listenerLB[pl.Listener]
	}
	serverNetworks := make(map[string][]string, len(ps.Nova.Servers))
	for _, srv := range ps.Nova.Servers {
		serverNetworks[srv.Name] = srv.Networks
	}
	for _, m := range f.Members {
		on, ok := serverNetworks[m.Server]
		if !ok {
			return fmt.Errorf("persona %q: member %q references server %q, which the compute plan does not create", ps.Name, m.Name, m.Server)
		}
		lb := poolLB[m.Pool]
		if network := lbNetwork[lb]; !slices.Contains(on, network) {
			return fmt.Errorf("persona %q: member %q puts server %q behind load balancer %q, but the server is not on network %q", ps.Name, m.Name, m.Server, lb, network)
		}
	}
	return nil
}
