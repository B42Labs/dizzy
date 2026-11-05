package scenario

import (
	"fmt"
	"math/rand"
	"slices"

	"github.com/B42Labs/dizzy/internal/barbican/plan"
)

// Generate expands the scenario and its seed into a fully-enumerated plan. The
// same scenario and seed always produce a byte-identical plan: the generator
// draws from math/rand v1 (whose sequence is frozen for compatibility) in a
// fixed order and emits every collection in a fixed order. The returned plan is
// validated before it is handed back.
//
// The draw order is fixed. Each stored secret sec-%04d draws its payload size
// and then, only when retrieve_ratio > 0, its read-back decision; drawing the
// decision only when the ratio is set keeps a ratio-0 plan's sizes identical to
// one with no read-backs at all, mirroring the other generators' ratio-0 guard.
// Each order ord-%04d draws nothing. Each container ctr-%04d draws its reference
// count and then, only when that count is positive, a permutation of the stored
// secrets whose first count indices, sorted ascending, name the secrets it
// references. Every slice is allocated non-nil, so an empty collection encodes
// as [] rather than null.
func (s Scenario) Generate() (*plan.Plan, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid scenario: %w", err)
	}

	rng := rand.New(rand.NewSource(s.Seed))
	p := &plan.Plan{Scenario: s.Name, Seed: s.Seed}

	p.Secrets = make([]plan.Secret, 0, s.Resources.Secrets)
	for i := 0; i < s.Resources.Secrets; i++ {
		sec := plan.Secret{
			Name:         fmt.Sprintf("sec-%04d", i+1),
			PayloadBytes: randRange(rng, s.Distribution.PayloadBytes),
		}
		if s.Distribution.RetrieveRatio > 0 && rng.Float64() < s.Distribution.RetrieveRatio {
			sec.Retrieve = true
		}
		p.Secrets = append(p.Secrets, sec)
	}

	p.Orders = make([]plan.Order, 0, s.Resources.Orders)
	for i := 0; i < s.Resources.Orders; i++ {
		p.Orders = append(p.Orders, plan.Order{Name: fmt.Sprintf("ord-%04d", i+1)})
	}

	p.Containers = make([]plan.Container, 0, s.Resources.Containers)
	for i := 0; i < s.Resources.Containers; i++ {
		k := randRange(rng, s.Distribution.SecretsPerContainer)
		ct := plan.Container{
			Name:    fmt.Sprintf("ctr-%04d", i+1),
			Secrets: make([]string, 0, k),
		}
		if k > 0 {
			// Validate caps secrets_per_container.max at the stored-secret count, so
			// the permutation always holds at least k indices.
			idx := rng.Perm(len(p.Secrets))[:k]
			slices.Sort(idx)
			for _, j := range idx {
				ct.Secrets = append(ct.Secrets, p.Secrets[j].Name)
			}
		}
		p.Containers = append(p.Containers, ct)
	}

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("generated plan failed validation: %w", err)
	}
	return p, nil
}

// randRange returns a uniformly random integer in the inclusive interval
// [r.Min, r.Max]. The caller guarantees r.Min <= r.Max via Scenario.Validate.
func randRange(rng *rand.Rand, r Range) int {
	return r.Min + rng.Intn(r.Max-r.Min+1)
}
