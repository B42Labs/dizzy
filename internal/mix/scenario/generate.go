package scenario

import (
	"fmt"
	"hash/fnv"
	"math"
	"slices"

	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	novascenario "github.com/B42Labs/dizzy/internal/nova/scenario"
	octaviaplan "github.com/B42Labs/dizzy/internal/octavia/plan"
)

// maxGardenerListeners caps personas.gardener.octavia.listeners, the
// listeners of one load balancer.
const maxGardenerListeners = 10

// The Kubernetes NodePort range a Gardener pool's members listen on, and the
// number of ports in it. Every pool of the persona takes one port of the range,
// so the range caps the persona's pools.
const (
	firstNodePort = 30000
	lastNodePort  = 32767
	nodePorts     = lastNodePort - firstNodePort + 1
)

// The ports of a Gardener load balancer's first two listeners, the HTTP and
// HTTPS ports a Kubernetes Service of type LoadBalancer exposes most often,
// and the first port of the listeners after them.
const (
	firstListenerPort  = 80
	secondListenerPort = 443
	extraListenerPort  = 8080
)

// persona is one workload persona of a scenario: its name, its share as the
// scenario states it, whether its resources are long-lived, whether its
// servers cold-migrate, whether its servers are replaced one at a time within
// their server group, and the builder of its compute scenario for a given
// number of servers. check, when set, validates what the compute scenario
// cannot for a given number of servers, and shape, when set, rewrites the
// generated compute plan. frontend, when set, builds the Octavia frontend in
// front of the shaped compute plan, which Generate calls only when services
// lists octavia.
type persona struct {
	name        string
	share       float64
	longLived   bool
	coldMigrate bool
	rolling     bool
	nova        func(servers int) novascenario.Scenario
	check       func(servers int) error
	shape       func(np *novaplan.Plan)
	frontend    func(np *novaplan.Plan) *octaviaplan.Frontend
}

// personas returns the scenario's personas in canonical order, alphabetical
// by name, which fixes each persona's index in the apportionment.
func (s Scenario) personas() []persona {
	return []persona{
		{name: "ci", share: s.Personas.CI.Share, nova: s.ciNova},
		{name: "gardener", share: s.Personas.Gardener.Share, rolling: true, nova: s.gardenerNova, check: s.gardenerCheck, shape: s.gardenerShape, frontend: s.gardenerFrontend},
		{name: "legacy", share: s.Personas.Legacy.Share, longLived: true, coldMigrate: s.Personas.Legacy.ColdMigrates(), nova: s.legacyNova},
	}
}

// shares returns the shares of personas, in their order.
func shares(personas []persona) []float64 {
	out := make([]float64, len(personas))
	for i, p := range personas {
		out[i] = p.share
	}
	return out
}

// ciNova builds the compute scenario of the CI persona: servers servers on one
// network each, spread over the persona's networks, with its data volumes and
// no extra port or lifecycle operation, so the churn engine never mutates a CI
// server.
func (s Scenario) ciNova(servers int) novascenario.Scenario {
	ci := s.Personas.CI
	return novascenario.Scenario{
		Name:      s.Name + "/ci",
		Seed:      PersonaSeed(s.Seed, "ci"),
		Image:     s.Image,
		Flavor:    s.Flavor,
		Resources: novascenario.Resources{Servers: servers, Networks: ci.Networks},
		Distribution: novascenario.Distribution{
			NetworksPerServer: novascenario.Range{Min: 1, Max: 1},
			VolumesPerServer:  ci.VolumesPerServer,
			AttachedVolumeGiB: ci.VolumeGiB,
		},
	}
}

// gardenerNova builds the compute scenario of the Gardener persona: servers
// servers on one network each, one network per cluster, with one data volume
// each and no extra port or lifecycle operation. gardenerShape then spreads
// the servers over the clusters.
func (s Scenario) gardenerNova(servers int) novascenario.Scenario {
	g := s.Personas.Gardener
	return novascenario.Scenario{
		Name:      s.Name + "/gardener",
		Seed:      PersonaSeed(s.Seed, "gardener"),
		Image:     s.Image,
		Flavor:    s.Flavor,
		Resources: novascenario.Resources{Servers: servers, Networks: g.Clusters},
		Distribution: novascenario.Distribution{
			NetworksPerServer: novascenario.Range{Min: 1, Max: 1},
			VolumesPerServer:  novascenario.Range{Min: 1, Max: 1},
			AttachedVolumeGiB: g.VolumeGiB,
		},
	}
}

// gardenerCheck validates the Gardener block for a persona of servers
// servers: the policy is one of the two anti-affinity policies, there is at
// least one cluster, and no cluster is left without a worker. When services
// lists octavia it also validates the octavia block: every cluster has at
// least one load balancer, every load balancer 1 to maxGardenerListeners
// listeners, and the persona's pools fit the NodePort range, one port each.
// Without the opt-in the octavia block is never read, so it is not checked.
func (s Scenario) gardenerCheck(servers int) error {
	g := s.Personas.Gardener
	if !novaplan.ValidPolicy(g.Policy) {
		return fmt.Errorf("personas.gardener.policy must be %q or %q, got %q", novaplan.PolicyAntiAffinity, novaplan.PolicySoftAntiAffinity, g.Policy)
	}
	if g.Clusters < 1 {
		return fmt.Errorf("personas.gardener.clusters must be at least 1, got %d", g.Clusters)
	}
	if servers >= 1 && g.Clusters > servers {
		return fmt.Errorf("personas.gardener.clusters (%d) exceeds the persona's %d server(s)", g.Clusters, servers)
	}
	if !slices.Contains(s.Services, mixplan.ServiceOctavia) {
		return nil
	}
	o := g.Octavia
	if o.LoadBalancers < 1 {
		return fmt.Errorf("personas.gardener.octavia.loadbalancers must be at least 1, got %d", o.LoadBalancers)
	}
	if o.Listeners < 1 || o.Listeners > maxGardenerListeners {
		return fmt.Errorf("personas.gardener.octavia.listeners must be between 1 and %d, got %d", maxGardenerListeners, o.Listeners)
	}
	if pools := product(g.Clusters, o.LoadBalancers, o.Listeners); pools > nodePorts {
		return fmt.Errorf("personas.gardener.octavia: %d pools (clusters x loadbalancers x listeners) exceed the %d node ports %d-%d",
			pools, nodePorts, firstNodePort, lastNodePort)
	}
	return nil
}

// product returns the product of factors, each at least 1, saturated at
// math.MaxInt. A product that overflowed could wrap below the NodePort range's
// size and let gardenerFrontend build a frontend too large to hold.
func product(factors ...int) int {
	n := 1
	for _, f := range factors {
		if n > math.MaxInt/f {
			return math.MaxInt
		}
		n *= f
	}
	return n
}

// gardenerShape turns the Gardener persona's compute plan into clusters: with
// K networks it adds the server groups grp-0001 to grp-%04d of K, each with
// the block's policy, and gives the server at index i the one network
// Networks[i%K] and the group ServerGroups[i%K]. Network k and group k form
// cluster k, so cluster sizes differ by at most one.
func (s Scenario) gardenerShape(np *novaplan.Plan) {
	k := len(np.Networks)
	np.ServerGroups = make([]novaplan.ServerGroup, k)
	for i := range np.ServerGroups {
		np.ServerGroups[i] = novaplan.ServerGroup{Name: fmt.Sprintf("grp-%04d", i+1), Policy: s.Personas.Gardener.Policy}
	}
	for i := range np.Servers {
		np.Servers[i].Networks = []string{np.Networks[i%k].Name}
		np.Servers[i].Group = np.ServerGroups[i%k].Name
	}
}

// gardenerFrontend builds the Octavia frontend of the Gardener persona's
// shaped compute plan np, as the OpenStack cloud controller manager keeps it
// for a cluster's Services of type LoadBalancer. Cluster k (0-based) is
// np.Networks[k] with the workers in group np.ServerGroups[k], in plan order.
// With B load balancers and L listeners from the octavia block, load balancer
// b of cluster k is lb-%04d numbered k*B+b+1, with its VIP on the cluster's
// network, which the members reach without a router. Listener l (0-based) of
// load balancer i (1-based) is lsn-%04d numbered (i-1)*L+l+1, on the port
// listenerPort returns, and its default pool pool-%04d and, with
// health_monitors, the pool's hm-%04d carry the same number. Pool n has one
// member per worker of its cluster, MemberName(pool, w) for the w-th worker,
// on NodePort firstNodePort+n-1: every worker of a pool shares the port, as
// every node of a Kubernetes cluster serves a Service's NodePort.
//
// It draws nothing, so the frontend is a pure function of np and the block,
// and the plan stays byte-identical for the same scenario and seed. Every
// collection is non-nil, so an empty one encodes as [], not null.
func (s Scenario) gardenerFrontend(np *novaplan.Plan) *octaviaplan.Frontend {
	o := s.Personas.Gardener.Octavia
	f := &octaviaplan.Frontend{
		Provider:       o.Provider,
		LoadBalancers:  []octaviaplan.LoadBalancer{},
		Listeners:      []octaviaplan.Listener{},
		Pools:          []octaviaplan.Pool{},
		Members:        []octaviaplan.ServerMember{},
		HealthMonitors: []octaviaplan.HealthMonitor{},
	}
	for k, network := range np.Networks {
		var workers []string
		for _, srv := range np.Servers {
			if srv.Group == np.ServerGroups[k].Name {
				workers = append(workers, srv.Name)
			}
		}
		for b := range o.LoadBalancers {
			i := k*o.LoadBalancers + b + 1
			lb := fmt.Sprintf("lb-%04d", i)
			f.LoadBalancers = append(f.LoadBalancers, octaviaplan.LoadBalancer{Name: lb, Network: network.Name})
			for l := range o.Listeners {
				n := (i-1)*o.Listeners + l + 1
				listener := fmt.Sprintf("lsn-%04d", n)
				pool := fmt.Sprintf("pool-%04d", n)
				f.Listeners = append(f.Listeners, octaviaplan.Listener{Name: listener, LoadBalancer: lb, Port: listenerPort(l)})
				f.Pools = append(f.Pools, octaviaplan.Pool{Name: pool, Listener: listener})
				for w, worker := range workers {
					f.Members = append(f.Members, octaviaplan.ServerMember{
						Name:   octaviaplan.MemberName(pool, w+1),
						Pool:   pool,
						Server: worker,
						Port:   firstNodePort + n - 1,
					})
				}
				if o.HealthMonitors {
					f.HealthMonitors = append(f.HealthMonitors, octaviaplan.HealthMonitor{Name: fmt.Sprintf("hm-%04d", n), Pool: pool})
				}
			}
		}
	}
	return f
}

// listenerPort returns the port of listener l (0-based) of a Gardener load
// balancer: firstListenerPort, then secondListenerPort, then consecutive ports
// from extraListenerPort on, so no two listeners of a load balancer share one.
func listenerPort(l int) int {
	switch l {
	case 0:
		return firstListenerPort
	case 1:
		return secondListenerPort
	default:
		return extraListenerPort + l - 2
	}
}

// legacyNova builds the compute scenario of the Legacy persona: servers
// servers on one network each, spread over the persona's networks, with its
// data volumes and extra ports. Every server is stop/started (soft),
// live-migrated and, when the block names a resize flavor, resized, and every
// volume and port is detached. Unless the block turns cold migration off,
// Generate marks every server for it on top, an operation the compute
// scenario has no ratio for.
func (s Scenario) legacyNova(servers int) novascenario.Scenario {
	legacy := s.Personas.Legacy
	d := novascenario.Distribution{
		NetworksPerServer: novascenario.Range{Min: 1, Max: 1},
		VolumesPerServer:  legacy.VolumesPerServer,
		PortsPerServer:    legacy.PortsPerServer,
		AttachedVolumeGiB: legacy.VolumeGiB,
		StopStartRatio:    1,
		LiveMigratedRatio: 1,
		VolumeDetachRatio: 1,
		PortDetachRatio:   1,
	}
	if legacy.ResizeFlavor != "" {
		d.ResizedRatio = 1
	}
	return novascenario.Scenario{
		Name:         s.Name + "/legacy",
		Seed:         PersonaSeed(s.Seed, "legacy"),
		Image:        s.Image,
		Flavor:       s.Flavor,
		ResizeFlavor: legacy.ResizeFlavor,
		Resources:    novascenario.Resources{Servers: servers, Networks: legacy.Networks},
		Distribution: d,
	}
}

// PersonaSeed derives a persona's seed from the scenario seed and the persona
// name: the seed XOR the FNV-64a hash of the name, the derivation payload.Seed
// uses. Two personas of one run draw from distinct seeds while the whole run
// stays reproducible from its single seed. A background lane derives its seed
// the same way from its name.
func PersonaSeed(seed int64, name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return seed ^ int64(h.Sum64())
}

// Generate expands the scenario, its seed and the lane scenarios
// LoadLanes read into a mix plan. It validates the scenario, divides
// resources.servers among the personas by their shares, and emits, in
// canonical order, one plan persona for every persona that received at least
// one server, with its normalized share, its seed and its generated compute
// plan. Every server of a persona that cold-migrates is marked for cold
// migration, a persona with a shape has its compute plan rewritten, and a
// long-lived or rolling persona is marked so. When services lists octavia, a
// persona with a frontend gets the Octavia frontend of its shaped compute
// plan, and Generate fails when no persona received a server to put behind
// one, so a run never claims Octavia load it cannot produce. It then emits, in
// canonical order, one plan lane for every enabled lane, with the seed and the
// generated plan of its scenario in ls; the scenario of a disabled lane is
// ignored. The returned plan is validated before it is handed back, so a
// scenario with no server to divide fails here.
func (s Scenario) Generate(ls LaneScenarios) (*mixplan.Plan, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid scenario: %w", err)
	}

	personas := s.personas()
	sh := shares(personas)
	var sum float64
	for _, v := range sh {
		sum += v
	}
	servers := Apportion(s.Resources.Servers, sh)

	p := &mixplan.Plan{
		Scenario: s.Name,
		Seed:     s.Seed,
		Services: append([]string{}, s.Services...),
		Personas: []mixplan.Persona{},
	}
	octavia := slices.Contains(s.Services, mixplan.ServiceOctavia)
	frontends := 0
	for i, ps := range personas {
		if servers[i] < 1 {
			continue
		}
		ns := ps.nova(servers[i])
		np, err := ns.Generate()
		if err != nil {
			return nil, fmt.Errorf("personas.%s: %w", ps.name, err)
		}
		if ps.coldMigrate {
			for j := range np.Servers {
				np.Servers[j].ColdMigrate = true
			}
		}
		if ps.shape != nil {
			ps.shape(np)
		}
		var frontend *octaviaplan.Frontend
		if octavia && ps.frontend != nil {
			frontend = ps.frontend(np)
			frontends++
		}
		p.Personas = append(p.Personas, mixplan.Persona{
			Name:      ps.name,
			Share:     ps.share / sum,
			Servers:   servers[i],
			Seed:      ns.Seed,
			LongLived: ps.longLived,
			Rolling:   ps.rolling,
			Nova:      np,
			Octavia:   frontend,
		})
	}
	if octavia && frontends == 0 {
		return nil, fmt.Errorf("opt-in service %q gives the gardener persona load balancers, but the gardener persona received no server of resources.servers (%d)",
			mixplan.ServiceOctavia, s.Resources.Servers)
	}

	for _, b := range s.Lanes.blocks() {
		if !b.lane.Enabled {
			continue
		}
		l, err := ls.generate(b.name)
		if err != nil {
			return nil, err
		}
		p.Lanes = append(p.Lanes, l)
	}

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("generated plan failed validation: %w", err)
	}
	return p, nil
}
