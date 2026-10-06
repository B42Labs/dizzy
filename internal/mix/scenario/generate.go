package scenario

import (
	"fmt"
	"hash/fnv"

	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	novascenario "github.com/B42Labs/dizzy/internal/nova/scenario"
)

// persona is one workload persona of a scenario: its name, its share as the
// scenario states it, whether its resources are long-lived, whether its
// servers cold-migrate, whether its servers are replaced one at a time within
// their server group, and the builder of its compute scenario for a given
// number of servers. check, when set, validates what the compute scenario
// cannot for a given number of servers, and shape, when set, rewrites the
// generated compute plan.
type persona struct {
	name        string
	share       float64
	longLived   bool
	coldMigrate bool
	rolling     bool
	nova        func(servers int) novascenario.Scenario
	check       func(servers int) error
	shape       func(np *novaplan.Plan)
}

// personas returns the scenario's personas in canonical order, alphabetical
// by name, which fixes each persona's index in the apportionment.
func (s Scenario) personas() []persona {
	return []persona{
		{name: "ci", share: s.Personas.CI.Share, nova: s.ciNova},
		{name: "gardener", share: s.Personas.Gardener.Share, rolling: true, nova: s.gardenerNova, check: s.gardenerCheck, shape: s.gardenerShape},
		{name: "legacy", share: s.Personas.Legacy.Share, longLived: true, coldMigrate: true, nova: s.legacyNova},
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
// least one cluster, and no cluster is left without a worker.
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
	return nil
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

// legacyNova builds the compute scenario of the Legacy persona: servers
// servers on one network each, spread over the persona's networks, with its
// data volumes and extra ports. Every server is stop/started (soft),
// live-migrated and, when the block names a resize flavor, resized, and every
// volume and port is detached. Generate marks every server for cold migration
// on top, an operation the compute scenario has no ratio for.
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
// name: the seed XOR the FNV-64a hash of the name, the derivation
// glance.PayloadSeed uses. Two personas of one run draw from distinct seeds
// while the whole run stays reproducible from its single seed.
func PersonaSeed(seed int64, name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return seed ^ int64(h.Sum64())
}

// Generate expands the scenario and its seed into a mix plan. It validates the
// scenario, divides resources.servers among the personas by their shares, and
// emits, in canonical order, one plan persona for every persona that received
// at least one server, with its normalized share, its seed and its generated
// compute plan. Every server of a persona that cold-migrates is marked for cold
// migration, a persona with a shape has its compute plan rewritten, and a
// long-lived or rolling persona is marked so. The returned plan
// is validated before it is handed back, so a scenario with no server to
// divide fails here.
func (s Scenario) Generate() (*mixplan.Plan, error) {
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
		p.Personas = append(p.Personas, mixplan.Persona{
			Name:      ps.name,
			Share:     ps.share / sum,
			Servers:   servers[i],
			Seed:      ns.Seed,
			LongLived: ps.longLived,
			Rolling:   ps.rolling,
			Nova:      np,
		})
	}

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("generated plan failed validation: %w", err)
	}
	return p, nil
}
