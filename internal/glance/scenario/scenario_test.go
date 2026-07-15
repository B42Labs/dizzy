package scenario

import (
	"testing"
	"time"
)

// floatPtr returns a pointer to f, for building the *float64 chaos fields
// (LifecycleRatio) in test fixtures.
func floatPtr(f float64) *float64 { return &f }

// smallScenario is the compact scenario backing the profile test; it mirrors the
// shipped scenarios/glance/small.yaml so the profile test can tie to it.
func smallScenario() Scenario {
	return Scenario{
		Name: "small",
		Seed: 42,
		Resources: Resources{
			Images: 5,
		},
		Distribution: Distribution{
			ImageSizeMiB:        Range{Min: 1, Max: 8},
			MetadataUpdateRatio: 0.5,
			SharedRatio:         0.5,
			MemberAcceptRatio:   0.5,
			MemberRemoveRatio:   0.5,
			CommunityRatio:      0.25,
			PublicRatio:         0.0,
			DeactivateRatio:     0.34,
			DeletedRatio:        0.34,
		},
		Chaos: &Chaos{
			Duration:       Duration(5 * time.Minute),
			Interval:       Interval{Min: Duration(200 * time.Millisecond), Max: Duration(3 * time.Second)},
			Parallel:       Parallel{Max: 4},
			ChurnRatio:     0.5,
			TargetFill:     0.8,
			LifecycleRatio: floatPtr(0.3),
		},
	}
}

func TestParseRejectsUnknownKey(t *testing.T) {
	// A stray key (here a nova-only "resources.servers" leaf) must fail strict
	// unmarshal loudly rather than being silently ignored.
	_, err := Parse([]byte("name: x\nresources:\n  servers: 1\n"))
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
		{name: "negative images", mutate: func(s *Scenario) { s.Resources.Images = -1 }, wantErr: true},
		{name: "size range min above max", mutate: func(s *Scenario) { s.Distribution.ImageSizeMiB = Range{Min: 8, Max: 1} }, wantErr: true},
		{name: "size min zero with images", mutate: func(s *Scenario) { s.Distribution.ImageSizeMiB.Min = 0 }, wantErr: true},
		{name: "metadata ratio above one", mutate: func(s *Scenario) { s.Distribution.MetadataUpdateRatio = 1.5 }, wantErr: true},
		{name: "shared ratio below zero", mutate: func(s *Scenario) { s.Distribution.SharedRatio = -0.1 }, wantErr: true},
		{name: "member accept ratio above one", mutate: func(s *Scenario) { s.Distribution.MemberAcceptRatio = 2 }, wantErr: true},
		{name: "public ratio above one", mutate: func(s *Scenario) { s.Distribution.PublicRatio = 1.1 }, wantErr: true},
		{name: "deleted ratio above one", mutate: func(s *Scenario) { s.Distribution.DeletedRatio = 1.5 }, wantErr: true},
		{name: "negative chaos duration", mutate: func(s *Scenario) { s.Chaos.Duration = Duration(-1) }, wantErr: true},
		{name: "chaos interval min above max", mutate: func(s *Scenario) { s.Chaos.Interval = Interval{Min: Duration(time.Second), Max: Duration(0)} }, wantErr: true},
		{name: "chaos lifecycle ratio above one", mutate: func(s *Scenario) { s.Chaos.LifecycleRatio = floatPtr(1.5) }, wantErr: true},
		{name: "chaos lifecycle ratio zero is valid", mutate: func(s *Scenario) { s.Chaos.LifecycleRatio = floatPtr(0) }},
		{name: "no images relaxes size min", mutate: func(s *Scenario) {
			s.Resources.Images = 0
			s.Distribution.ImageSizeMiB.Min = 0
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			tc.mutate(&s)
			err := s.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestSet(t *testing.T) {
	keys := map[string]string{
		"seed":                               "99",
		"resources.images":                   "10",
		"distribution.image_size_mib.min":    "2",
		"distribution.image_size_mib.max":    "16",
		"distribution.metadata_update_ratio": "0.3",
		"distribution.shared_ratio":          "0.4",
		"distribution.member_accept_ratio":   "0.5",
		"distribution.member_remove_ratio":   "0.2",
		"distribution.community_ratio":       "0.1",
		"distribution.public_ratio":          "0.0",
		"distribution.deactivate_ratio":      "0.3",
		"distribution.deleted_ratio":         "0.4",
	}
	for key, value := range keys {
		var s Scenario
		if err := s.Set(key, value); err != nil {
			t.Errorf("Set(%q, %q) = %v, want nil", key, value, err)
		}
	}

	var s Scenario
	if err := s.Set("distribution.nonexistent", "1"); err == nil {
		t.Error("Set of an unknown key: expected an error, got nil")
	}
	if err := s.Set("resources.images", "notanint"); err == nil {
		t.Error("Set of a non-integer value: expected an error, got nil")
	}
	if err := s.Set("distribution.shared_ratio", "notafloat"); err == nil {
		t.Error("Set of a non-numeric ratio: expected an error, got nil")
	}
}
