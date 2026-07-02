// Package scenario defines the human-authored YAML scenario format for Cinder
// (counts, ranges, ratios, seed) and the deterministic generator that expands a
// scenario plus its seed into a fully-enumerated plan. The same scenario and
// seed always yield a byte-identical plan. Cinder gets its own scenario schema
// rather than sharing the Neutron one: the two services share no resources, and
// separate schemas keep yaml.UnmarshalStrict failing loudly on a typo in either.
package scenario

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v2"
)

// Scenario is the parametrized description of a desired Cinder workload. It is
// parsed from YAML, validated, optionally overridden via Set, and expanded into
// a plan by Generate.
type Scenario struct {
	Name         string       `yaml:"name"`
	Seed         int64        `yaml:"seed"`
	Resources    Resources    `yaml:"resources"`
	Distribution Distribution `yaml:"distribution"`
}

// Resources holds the fixed counts of top-level resources to create.
type Resources struct {
	Volumes int `yaml:"volumes"`
}

// Distribution holds the per-volume ranges and the ratio that shape volume
// sizes, which volumes are resized, how much they grow, and how many snapshots
// each volume gets.
type Distribution struct {
	// VolumeSizeGiB is the initial size drawn per volume.
	VolumeSizeGiB Range `yaml:"volume_size_gib"`
	// VolumeResizedRatio is the fraction of volumes grown after creation.
	VolumeResizedRatio float64 `yaml:"volume_resized_ratio"`
	// ResizeGrowthGiB is the extend delta drawn per resized volume.
	ResizeGrowthGiB Range `yaml:"resize_growth_gib"`
	// SnapshotsPerVolume is the snapshot count drawn per volume.
	SnapshotsPerVolume Range `yaml:"snapshots_per_volume"`
}

// Range is an inclusive integer interval drawn from during generation.
type Range struct {
	Min int `yaml:"min"`
	Max int `yaml:"max"`
}

// Parse decodes a scenario from YAML. Unknown keys are rejected so that a typo
// in a scenario file fails loudly instead of being silently ignored. It does no
// semantic validation; call Validate for that.
func Parse(data []byte) (Scenario, error) {
	var s Scenario
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return Scenario{}, fmt.Errorf("parsing scenario: %w", err)
	}
	return s, nil
}

// maxCount caps the volume count and every range maximum. It bounds each
// individual slice preallocation and guards randRange's interval arithmetic
// against integer overflow, mirroring the neutron scenario's cap. It does not by
// itself bound the snapshot slice, whose length is volumes × snapshots_per_volume
// — Validate caps that product separately.
const maxCount = 1_000_000

// Validate checks the scenario for semantic consistency, returning an
// actionable error that names the offending field.
func (s Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name must not be empty")
	}

	if s.Resources.Volumes < 0 {
		return fmt.Errorf("resources.volumes must not be negative, got %d", s.Resources.Volumes)
	}
	if s.Resources.Volumes > maxCount {
		return fmt.Errorf("resources.volumes exceeds the limit of %d, got %d", maxCount, s.Resources.Volumes)
	}

	for _, c := range []struct {
		key string
		r   Range
	}{
		{"distribution.volume_size_gib", s.Distribution.VolumeSizeGiB},
		{"distribution.resize_growth_gib", s.Distribution.ResizeGrowthGiB},
		{"distribution.snapshots_per_volume", s.Distribution.SnapshotsPerVolume},
	} {
		if err := validateRange(c.key, c.r); err != nil {
			return err
		}
	}

	if s.Distribution.VolumeResizedRatio < 0 || s.Distribution.VolumeResizedRatio > 1 {
		return fmt.Errorf("distribution.volume_resized_ratio must be between 0 and 1, got %v", s.Distribution.VolumeResizedRatio)
	}

	// The snapshot slice is preallocated to volumes × snapshots_per_volume.max, a
	// product neither per-dimension cap bounds; cap it too so a large-but-legal
	// pair cannot demand a terabyte-scale allocation. Compute in int64 so the
	// product cannot overflow int on a 32-bit platform.
	if total := int64(s.Resources.Volumes) * int64(s.Distribution.SnapshotsPerVolume.Max); total > maxCount {
		return fmt.Errorf("resources.volumes (%d) × distribution.snapshots_per_volume.max (%d) = %d exceeds the total snapshot limit of %d",
			s.Resources.Volumes, s.Distribution.SnapshotsPerVolume.Max, total, maxCount)
	}

	// A volume must have a positive size, so the size range must start at 1 when
	// any volume is created.
	if s.Resources.Volumes > 0 && s.Distribution.VolumeSizeGiB.Min < 1 {
		return fmt.Errorf("distribution.volume_size_gib.min must be at least 1 when resources.volumes > 0, got %d", s.Distribution.VolumeSizeGiB.Min)
	}

	// A resized volume grows by at least 1 GiB so its target is strictly larger
	// than its initial size; require growth.min >= 1 whenever resizing happens.
	if s.Distribution.VolumeResizedRatio > 0 && s.Distribution.ResizeGrowthGiB.Min < 1 {
		return fmt.Errorf("distribution.resize_growth_gib.min must be at least 1 when volume_resized_ratio > 0, got %d", s.Distribution.ResizeGrowthGiB.Min)
	}

	return nil
}

// Set applies a single dotted-key override of the form key=value, matching the
// documented scenario fields. It returns an error for an unknown key or a value
// that does not parse to the field's type.
func (s *Scenario) Set(key, value string) error {
	switch key {
	case "seed":
		return setInt64(&s.Seed, key, value)
	case "resources.volumes":
		return setInt(&s.Resources.Volumes, key, value)
	case "distribution.volume_size_gib.min":
		return setInt(&s.Distribution.VolumeSizeGiB.Min, key, value)
	case "distribution.volume_size_gib.max":
		return setInt(&s.Distribution.VolumeSizeGiB.Max, key, value)
	case "distribution.volume_resized_ratio":
		return setFloat(&s.Distribution.VolumeResizedRatio, key, value)
	case "distribution.resize_growth_gib.min":
		return setInt(&s.Distribution.ResizeGrowthGiB.Min, key, value)
	case "distribution.resize_growth_gib.max":
		return setInt(&s.Distribution.ResizeGrowthGiB.Max, key, value)
	case "distribution.snapshots_per_volume.min":
		return setInt(&s.Distribution.SnapshotsPerVolume.Min, key, value)
	case "distribution.snapshots_per_volume.max":
		return setInt(&s.Distribution.SnapshotsPerVolume.Max, key, value)
	default:
		return fmt.Errorf("unknown override key %q", key)
	}
}

// validateRange enforces 0 <= Min <= Max <= maxCount for a named range.
func validateRange(key string, r Range) error {
	if r.Min < 0 {
		return fmt.Errorf("%s.min must not be negative, got %d", key, r.Min)
	}
	if r.Min > r.Max {
		return fmt.Errorf("%s.min (%d) must not exceed %s.max (%d)", key, r.Min, key, r.Max)
	}
	if r.Max > maxCount {
		return fmt.Errorf("%s.max (%d) exceeds the limit of %d", key, r.Max, maxCount)
	}
	return nil
}

// setInt parses value as an int into dst, wrapping a parse failure with the key.
func setInt(dst *int, key, value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("override %s: %q is not an integer", key, value)
	}
	*dst = n
	return nil
}

// setInt64 parses value as an int64 into dst, wrapping a parse failure with the
// key.
func setInt64(dst *int64, key, value string) error {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return fmt.Errorf("override %s: %q is not an integer", key, value)
	}
	*dst = n
	return nil
}

// setFloat parses value as a float64 into dst, wrapping a parse failure with the
// key.
func setFloat(dst *float64, key, value string) error {
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return fmt.Errorf("override %s: %q is not a number", key, value)
	}
	*dst = f
	return nil
}
