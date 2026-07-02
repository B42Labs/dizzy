// Package plan defines the expanded, fully-enumerated set of Cinder resources —
// the expected-state source of truth produced deterministically from a scenario
// plus a seed. Like the neutron plan it is pure data: every collection is a
// slice (never a map) so that encoding the plan to JSON yields byte-identical
// output for the same input.
package plan

import (
	"fmt"
	"strings"
)

// Plan is the fully-expanded expected state for one Cinder run. Scenario and
// Seed record the provenance that produced it; the slices enumerate every
// volume and snapshot. Cross-resource references are by logical name, resolved
// by Validate.
type Plan struct {
	Scenario  string     `json:"scenario"`
	Seed      int64      `json:"seed"`
	Volumes   []Volume   `json:"volumes"`
	Snapshots []Snapshot `json:"snapshots"`
}

// Volume is a blank volume to create. SizeGiB is its initial size. ResizeToGiB
// is the target size after the extend stage; 0 means the volume is never
// resized, and any non-zero value is always strictly greater than SizeGiB.
type Volume struct {
	Name        string `json:"name"`
	SizeGiB     int    `json:"sizeGiB"`
	ResizeToGiB int    `json:"resizeToGiB,omitempty"`
}

// Snapshot is a snapshot to create of its source volume, referenced by logical
// name.
type Snapshot struct {
	Name   string `json:"name"`
	Volume string `json:"volume"`
}

// Validate checks the plan graph for well-formedness: every volume has a
// positive initial size and a resize target (when set) strictly larger than it,
// and every snapshot references a known volume. It returns an error naming the
// first offending resource.
func (p *Plan) Validate() error {
	volumes := make(map[string]bool, len(p.Volumes))
	for _, v := range p.Volumes {
		if v.SizeGiB < 1 {
			return fmt.Errorf("volume %q has size %d GiB, want at least 1", v.Name, v.SizeGiB)
		}
		if v.ResizeToGiB != 0 && v.ResizeToGiB <= v.SizeGiB {
			return fmt.Errorf("volume %q resize target %d GiB must exceed its size %d GiB", v.Name, v.ResizeToGiB, v.SizeGiB)
		}
		volumes[v.Name] = true
	}

	for _, s := range p.Snapshots {
		if !volumes[s.Volume] {
			return fmt.Errorf("snapshot %q references unknown volume %q", s.Name, s.Volume)
		}
	}

	return nil
}

// ResizedVolumes counts the volumes with a resize target (ResizeToGiB > 0).
func (p *Plan) ResizedVolumes() int {
	var n int
	for _, v := range p.Volumes {
		if v.ResizeToGiB > 0 {
			n++
		}
	}
	return n
}

// finalSizeGiB returns a volume's size after the extend stage: its resize
// target when set, otherwise its initial size.
func finalSizeGiB(v Volume) int {
	if v.ResizeToGiB > 0 {
		return v.ResizeToGiB
	}
	return v.SizeGiB
}

// TotalGiB is the total gigabytes the plan consumes against Cinder's shared
// gigabytes quota: the sum of every volume's final (post-resize) size plus the
// sum of every snapshot's size, which is its source volume's final size. It is
// shared by Summary and the quota pre-check. A max-size plan can sum past a
// 32-bit int, so the total is accumulated in int64 — the same guard Scenario
// bounds the snapshot-count product with — so it never wraps to a small or
// negative value that would let an over-quota plan slip past the pre-check.
func (p *Plan) TotalGiB() int64 {
	finalByName := make(map[string]int, len(p.Volumes))
	var total int64
	for _, v := range p.Volumes {
		final := finalSizeGiB(v)
		finalByName[v.Name] = final
		total += int64(final)
	}
	for _, s := range p.Snapshots {
		total += int64(finalByName[s.Volume])
	}
	return total
}

// Summary returns a deterministic, human-readable count of the plan, used by
// "cinder apply --dry-run" to preview a scenario without touching a cloud.
func (p *Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan for scenario %q (seed %d)\n", p.Scenario, p.Seed)
	fmt.Fprintf(&b, "  volumes:         %d (%d resized)\n", len(p.Volumes), p.ResizedVolumes())
	fmt.Fprintf(&b, "  snapshots:       %d\n", len(p.Snapshots))
	fmt.Fprintf(&b, "  total GiB:       %d\n", p.TotalGiB())
	return b.String()
}
