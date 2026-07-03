package scenario

import (
	"testing"
	"time"
)

// floatPtr returns a pointer to f, for building the *float64 chaos fields
// (ResizeRatio) in test fixtures.
func floatPtr(f float64) *float64 { return &f }

// smallScenario is the compact scenario backing the golden test; it mirrors the
// shipped scenarios/cinder/small.yaml so the profile test can tie to it.
func smallScenario() Scenario {
	return Scenario{
		Name: "small",
		Seed: 42,
		Resources: Resources{
			Volumes: 5,
		},
		Distribution: Distribution{
			VolumeSizeGiB:      Range{Min: 1, Max: 5},
			VolumeResizedRatio: 0.5,
			ResizeGrowthGiB:    Range{Min: 1, Max: 4},
			SnapshotsPerVolume: Range{Min: 0, Max: 2},
		},
		Chaos: &Chaos{
			Duration:    Duration(5 * time.Minute),
			Interval:    Interval{Min: Duration(200 * time.Millisecond), Max: Duration(3 * time.Second)},
			Parallel:    Parallel{Max: 4},
			ChurnRatio:  0.5,
			TargetFill:  0.8,
			ResizeRatio: floatPtr(0.3),
		},
	}
}

func TestParseRejectsUnknownKey(t *testing.T) {
	// A stray key (here a neutron-only "topology" block) must fail strict
	// unmarshal loudly rather than being silently ignored.
	_, err := Parse([]byte("name: x\nresources:\n  volumes: 1\ntopology:\n  foo: bar\n"))
	if err == nil {
		t.Fatal("Parse of a scenario with an unknown key: expected an error, got nil")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Scenario)
		wantErr bool
	}{
		{name: "valid", mutate: func(*Scenario) {}},
		{name: "empty name", mutate: func(s *Scenario) { s.Name = "" }, wantErr: true},
		{name: "negative volumes", mutate: func(s *Scenario) { s.Resources.Volumes = -1 }, wantErr: true},
		{name: "size min above max", mutate: func(s *Scenario) { s.Distribution.VolumeSizeGiB = Range{Min: 5, Max: 1} }, wantErr: true},
		{name: "negative snapshot min", mutate: func(s *Scenario) { s.Distribution.SnapshotsPerVolume.Min = -1 }, wantErr: true},
		{name: "ratio above one", mutate: func(s *Scenario) { s.Distribution.VolumeResizedRatio = 1.5 }, wantErr: true},
		{name: "ratio below zero", mutate: func(s *Scenario) { s.Distribution.VolumeResizedRatio = -0.1 }, wantErr: true},
		{
			name:    "size min zero with volumes rejected",
			mutate:  func(s *Scenario) { s.Distribution.VolumeSizeGiB.Min = 0 },
			wantErr: true,
		},
		{
			name:    "size min zero allowed without volumes",
			mutate:  func(s *Scenario) { s.Resources.Volumes = 0; s.Distribution.VolumeSizeGiB.Min = 0 },
			wantErr: false,
		},
		{
			name:    "growth min zero with resizing rejected",
			mutate:  func(s *Scenario) { s.Distribution.ResizeGrowthGiB.Min = 0 },
			wantErr: true,
		},
		{
			name:    "growth min zero allowed without resizing",
			mutate:  func(s *Scenario) { s.Distribution.VolumeResizedRatio = 0; s.Distribution.ResizeGrowthGiB.Min = 0 },
			wantErr: false,
		},
		{
			// Each dimension is at its own cap, but the volumes × snapshots_per_volume
			// product (10^12) would demand a terabyte-scale snapshot preallocation.
			name: "volumes times snapshot max exceeds total limit",
			mutate: func(s *Scenario) {
				s.Resources.Volumes = maxCount
				s.Distribution.SnapshotsPerVolume = Range{Min: 0, Max: maxCount}
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			tc.mutate(&s)
			err := s.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestSet(t *testing.T) {
	// Every documented key parses into the matching field.
	keys := []struct {
		key   string
		value string
		check func(Scenario) bool
	}{
		{"seed", "7", func(s Scenario) bool { return s.Seed == 7 }},
		{"resources.volumes", "9", func(s Scenario) bool { return s.Resources.Volumes == 9 }},
		{"distribution.volume_size_gib.min", "2", func(s Scenario) bool { return s.Distribution.VolumeSizeGiB.Min == 2 }},
		{"distribution.volume_size_gib.max", "8", func(s Scenario) bool { return s.Distribution.VolumeSizeGiB.Max == 8 }},
		{"distribution.volume_resized_ratio", "0.25", func(s Scenario) bool { return s.Distribution.VolumeResizedRatio == 0.25 }},
		{"distribution.resize_growth_gib.min", "3", func(s Scenario) bool { return s.Distribution.ResizeGrowthGiB.Min == 3 }},
		{"distribution.resize_growth_gib.max", "6", func(s Scenario) bool { return s.Distribution.ResizeGrowthGiB.Max == 6 }},
		{"distribution.snapshots_per_volume.min", "1", func(s Scenario) bool { return s.Distribution.SnapshotsPerVolume.Min == 1 }},
		{"distribution.snapshots_per_volume.max", "4", func(s Scenario) bool { return s.Distribution.SnapshotsPerVolume.Max == 4 }},
	}
	for _, k := range keys {
		t.Run(k.key, func(t *testing.T) {
			s := smallScenario()
			if err := s.Set(k.key, k.value); err != nil {
				t.Fatalf("Set(%q,%q) = %v", k.key, k.value, err)
			}
			if !k.check(s) {
				t.Errorf("Set(%q,%q) did not apply", k.key, k.value)
			}
		})
	}
}

// TestParseChaosBlock confirms a full chaos block — including the
// block-storage-specific resize_ratio — parses and validates, that a scenario
// with no chaos block is valid, and that an unknown chaos key fails strict
// unmarshal loudly.
func TestParseChaosBlock(t *testing.T) {
	yaml := `name: c
seed: 1
resources:
  volumes: 1
distribution:
  volume_size_gib: { min: 1, max: 1 }
chaos:
  duration: 10m
  interval: { min: 200ms, max: 3s }
  parallel: { max: 4 }
  churn_ratio: 0.5
  target_fill: 0.8
  resize_ratio: 0.3
`
	s, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Chaos == nil {
		t.Fatal("chaos block did not parse into a non-nil Chaos")
	}
	if s.Chaos.ResizeRatio == nil || *s.Chaos.ResizeRatio != 0.3 {
		t.Errorf("resize_ratio = %v, want 0.3", s.Chaos.ResizeRatio)
	}
	if s.Chaos.Duration != Duration(10*time.Minute) {
		t.Errorf("duration = %s, want 10m", time.Duration(s.Chaos.Duration))
	}
	if err := s.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for a well-formed chaos block", err)
	}

	// A scenario with no chaos block validates the same as before.
	if err := smallScenario().Validate(); err != nil {
		t.Errorf("Validate() with a chaos block = %v, want nil", err)
	}

	// An unknown chaos key must fail strict unmarshal, not be silently ignored.
	if _, err := Parse([]byte("name: c\nresources:\n  volumes: 1\nchaos:\n  nope: 1\n")); err == nil {
		t.Error("Parse of a chaos block with an unknown key: expected an error, got nil")
	}
}

// TestValidateChaosBlockRejects covers each way a chaos block can be
// semantically invalid, one violated rule at a time.
func TestValidateChaosBlockRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Chaos)
	}{
		{"negative duration", func(c *Chaos) { c.Duration = Duration(-time.Second) }},
		{"negative interval min", func(c *Chaos) { c.Interval.Min = Duration(-time.Millisecond) }},
		{"interval min above max", func(c *Chaos) { c.Interval.Min = c.Interval.Max + Duration(time.Second) }},
		{"negative parallel max", func(c *Chaos) { c.Parallel.Max = -1 }},
		{"churn_ratio above one", func(c *Chaos) { c.ChurnRatio = 1.5 }},
		{"target_fill below zero", func(c *Chaos) { c.TargetFill = -0.1 }},
		{"resize_ratio above one", func(c *Chaos) { c.ResizeRatio = floatPtr(1.5) }},
		{"resize_ratio below zero", func(c *Chaos) { c.ResizeRatio = floatPtr(-0.1) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			tc.mutate(s.Chaos)
			if err := s.Validate(); err == nil {
				t.Fatal("Validate() = nil, want error")
			}
		})
	}
}

func TestSetErrors(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"unknown key", "resources.nope", "1"},
		{"non-integer volumes", "resources.volumes", "many"},
		{"non-number ratio", "distribution.volume_resized_ratio", "half"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			if err := s.Set(tc.key, tc.value); err == nil {
				t.Errorf("Set(%q,%q) = nil, want error", tc.key, tc.value)
			}
		})
	}
}
