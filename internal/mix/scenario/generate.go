package scenario

import (
	"fmt"
	"hash/fnv"

	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	novascenario "github.com/B42Labs/dizzy/internal/nova/scenario"
)

// persona is one workload persona of a scenario: its name, its share as the
// scenario states it, and the builder of its compute scenario for a given
// number of servers.
type persona struct {
	name  string
	share float64
	nova  func(servers int) novascenario.Scenario
}

// personas returns the scenario's personas in canonical order, alphabetical
// by name, which fixes each persona's index in the apportionment.
func (s Scenario) personas() []persona {
	return []persona{
		{name: "ci", share: s.Personas.CI.Share, nova: s.ciNova},
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
// compute plan. The returned plan is validated before it is handed back, so a
// scenario with no server to divide fails here.
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
		p.Personas = append(p.Personas, mixplan.Persona{
			Name:    ps.name,
			Share:   ps.share / sum,
			Servers: servers[i],
			Seed:    ns.Seed,
			Nova:    np,
		})
	}

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("generated plan failed validation: %w", err)
	}
	return p, nil
}
