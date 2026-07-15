package scenario

import (
	"fmt"
	"math/rand"

	"github.com/B42Labs/dizzy/internal/glance/plan"
)

// Generate expands the scenario and its seed into a fully-enumerated plan. The
// same scenario and seed always produce a byte-identical plan: the generator
// draws from math/rand v1 (whose sequence is frozen for compatibility) in a
// fixed order and emits the image collection in a fixed order. The returned plan
// is validated before it is handed back.
//
// Each image img-%04d is emitted in order, and each decision is drawn only when
// its governing ratio is non-degenerate — drawing a decision only when its ratio
// is set keeps a ratio-0 plan byte-identical to one generated before that
// decision existed, mirroring the nova generator's ratio-0 guard. The fixed
// per-image draw order is: payload size, metadata churn, shared (then, only if
// shared is drawn, member accept and member remove), community flip, public
// flip, deactivate cycle, delete.
func (s Scenario) Generate() (*plan.Plan, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid scenario: %w", err)
	}

	rng := rand.New(rand.NewSource(s.Seed))
	p := &plan.Plan{Scenario: s.Name, Seed: s.Seed}

	p.Images = make([]plan.Image, 0, s.Resources.Images)
	for i := 0; i < s.Resources.Images; i++ {
		img := plan.Image{
			Name:    fmt.Sprintf("img-%04d", i+1),
			SizeMiB: randRange(rng, s.Distribution.ImageSizeMiB),
		}

		if s.Distribution.MetadataUpdateRatio > 0 && rng.Float64() < s.Distribution.MetadataUpdateRatio {
			img.MetadataUpdate = true
		}

		if s.Distribution.SharedRatio > 0 && rng.Float64() < s.Distribution.SharedRatio {
			img.Shared = true
			if s.Distribution.MemberAcceptRatio > 0 && rng.Float64() < s.Distribution.MemberAcceptRatio {
				img.MemberAccept = true
			}
			if s.Distribution.MemberRemoveRatio > 0 && rng.Float64() < s.Distribution.MemberRemoveRatio {
				img.MemberRemove = true
			}
		}

		if s.Distribution.CommunityRatio > 0 && rng.Float64() < s.Distribution.CommunityRatio {
			img.Community = true
		}

		if s.Distribution.PublicRatio > 0 && rng.Float64() < s.Distribution.PublicRatio {
			img.Public = true
		}

		if s.Distribution.DeactivateRatio > 0 && rng.Float64() < s.Distribution.DeactivateRatio {
			img.Deactivate = true
		}

		if s.Distribution.DeletedRatio > 0 && rng.Float64() < s.Distribution.DeletedRatio {
			img.Delete = true
		}

		p.Images = append(p.Images, img)
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
