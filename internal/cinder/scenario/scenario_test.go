package scenario

import (
	"testing"
)

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
