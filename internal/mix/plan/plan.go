// Package plan defines the expanded plan of a combined (mix) run: one entry per
// workload persona that received at least one server, each carrying its share
// of the server envelope, its derived seed and the compute plan it churns.
// Like the per-service plans it is pure data, so the same scenario and seed
// encode to byte-identical JSON.
package plan

import (
	"fmt"

	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
)

// Plan is the expanded state of one mix run. Scenario and Seed record the
// provenance that produced it, Services lists the opt-in services bound to the
// personas, and Personas lists the personas in canonical (alphabetical) order.
type Plan struct {
	Scenario string    `json:"scenario"`
	Seed     int64     `json:"seed"`
	Services []string  `json:"services"`
	Personas []Persona `json:"personas"`
}

// Persona is one workload persona of a mix run. Share is its normalized share,
// its scenario share divided by the sum of all shares, and Servers its part of
// the scenario's server envelope. Seed is derived from the plan seed and the
// persona name. LongLived marks a persona whose resources are kept until the
// run's teardown and mutated repeatedly. Rolling marks a persona whose servers
// are replaced one at a time within their server group. Nova is the compute
// plan the persona churns.
type Persona struct {
	Name      string         `json:"name"`
	Share     float64        `json:"share"`
	Servers   int            `json:"servers"`
	Seed      int64          `json:"seed"`
	LongLived bool           `json:"longLived,omitempty"`
	Rolling   bool           `json:"rolling,omitempty"`
	Nova      *novaplan.Plan `json:"nova"`
}

// Validate checks the plan for well-formedness: at least one persona has a
// server, no persona appears twice, and every persona has a valid compute plan
// with exactly its share of servers. It returns an error naming the first
// offending persona.
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
	}
	return nil
}
