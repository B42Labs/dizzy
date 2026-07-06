package scenario

import (
	"fmt"
	"math/rand"

	"github.com/B42Labs/dizzy/internal/cinder/plan"
)

// Generate expands the scenario and its seed into a fully-enumerated plan. The
// same scenario and seed always produce a byte-identical plan: the generator
// draws from math/rand v1 (whose sequence is frozen for compatibility) in a
// fixed order and emits every collection in a fixed order. The returned plan is
// validated before it is handed back.
//
// Per volume vol-%04d the draw order is fixed: the initial size, then — only
// when volume_resized_ratio > 0 — the resize decision and (for a resized
// volume) the growth delta, then the snapshot count. Drawing the resize
// decision only when the ratio is set keeps a ratio-0 plan byte-identical to
// one generated before resizing existed, mirroring the neutron generator's
// external-gateway guard.
func (s Scenario) Generate() (*plan.Plan, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid scenario: %w", err)
	}

	rng := rand.New(rand.NewSource(s.Seed))
	p := &plan.Plan{Scenario: s.Name, Seed: s.Seed}

	p.Volumes = make([]plan.Volume, 0, s.Resources.Volumes)
	snapCounts := make([]int, 0, s.Resources.Volumes)
	for i := 0; i < s.Resources.Volumes; i++ {
		v := plan.Volume{
			Name:    fmt.Sprintf("vol-%04d", i+1),
			SizeGiB: randRange(rng, s.Distribution.VolumeSizeGiB),
		}
		if s.Distribution.VolumeResizedRatio > 0 && rng.Float64() < s.Distribution.VolumeResizedRatio {
			v.ResizeToGiB = v.SizeGiB + randRange(rng, s.Distribution.ResizeGrowthGiB)
		}
		p.Volumes = append(p.Volumes, v)
		snapCounts = append(snapCounts, randRange(rng, s.Distribution.SnapshotsPerVolume))
	}

	// Snapshots are emitted after every volume's counts are drawn, with a global
	// counter so snap-%04d is contiguous across volumes while each references its
	// source volume by logical name.
	var total int
	for _, n := range snapCounts {
		total += n
	}
	p.Snapshots = make([]plan.Snapshot, 0, total)
	snapCount := 0
	for i, n := range snapCounts {
		for j := 0; j < n; j++ {
			snapCount++
			p.Snapshots = append(p.Snapshots, plan.Snapshot{
				Name:   fmt.Sprintf("snap-%04d", snapCount),
				Volume: p.Volumes[i].Name,
			})
		}
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
