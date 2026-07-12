package scenario

import (
	"testing"
	"time"
)

// floatPtr returns a pointer to f, for building the *float64 chaos fields
// (LifecycleRatio) in test fixtures.
func floatPtr(f float64) *float64 { return &f }

// smallScenario is the compact scenario backing the profile test; it mirrors the
// shipped scenarios/nova/small.yaml so the profile test can tie to it.
func smallScenario() Scenario {
	return Scenario{
		Name:         "small",
		Seed:         42,
		Image:        "cirros",
		Flavor:       "m1.tiny",
		ResizeFlavor: "m1.small",
		Resources: Resources{
			Servers:  3,
			Networks: 2,
		},
		Distribution: Distribution{
			NetworksPerServer:   Range{Min: 1, Max: 2},
			VolumesPerServer:    Range{Min: 0, Max: 2},
			PortsPerServer:      Range{Min: 0, Max: 1},
			AttachedVolumeGiB:   Range{Min: 1, Max: 3},
			RootVolumeGiB:       Range{Min: 2, Max: 4},
			BootFromVolumeRatio: 0.34,
			UserDataRatio:       0.5,
			StopStartRatio:      0.5,
			StopStartHardRatio:  0.5,
			ResizedRatio:        0.34,
			LiveMigratedRatio:   0.34,
			DeletedRatio:        0.34,
			VolumeDetachRatio:   0.5,
			PortDetachRatio:     0.5,
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
	// A stray key (here a cinder-only "volume_size_gib" leaf) must fail strict
	// unmarshal loudly rather than being silently ignored.
	_, err := Parse([]byte("name: x\nresources:\n  servers: 1\nvolume_size_gib: 4\n"))
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
		{name: "negative servers", mutate: func(s *Scenario) { s.Resources.Servers = -1 }, wantErr: true},
		{name: "networks range min above max", mutate: func(s *Scenario) { s.Distribution.NetworksPerServer = Range{Min: 3, Max: 1} }, wantErr: true},
		{name: "ratio above one", mutate: func(s *Scenario) { s.Distribution.DeletedRatio = 1.5 }, wantErr: true},
		{name: "ratio below zero", mutate: func(s *Scenario) { s.Distribution.UserDataRatio = -0.1 }, wantErr: true},
		{name: "missing image with servers", mutate: func(s *Scenario) { s.Image = "" }, wantErr: true},
		{name: "missing flavor with servers", mutate: func(s *Scenario) { s.Flavor = "" }, wantErr: true},
		{name: "networks_per_server.min zero with servers", mutate: func(s *Scenario) { s.Distribution.NetworksPerServer.Min = 0 }, wantErr: true},
		{name: "networks_per_server.max above resources.networks", mutate: func(s *Scenario) { s.Distribution.NetworksPerServer.Max = 3 }, wantErr: true},
		{name: "resized without resize flavor", mutate: func(s *Scenario) { s.ResizeFlavor = "" }, wantErr: true},
		{name: "resize flavor equals flavor", mutate: func(s *Scenario) { s.ResizeFlavor = "m1.tiny" }, wantErr: true},
		{name: "boot from volume with zero root min", mutate: func(s *Scenario) { s.Distribution.RootVolumeGiB.Min = 0 }, wantErr: true},
		{name: "volumes with zero attached min", mutate: func(s *Scenario) { s.Distribution.AttachedVolumeGiB.Min = 0 }, wantErr: true},
		{name: "volumes product cap", mutate: func(s *Scenario) { s.Resources.Servers = 1000; s.Distribution.VolumesPerServer.Max = 2000 }, wantErr: true},
		{name: "networks product cap", mutate: func(s *Scenario) { s.Resources.Servers = 1000; s.Resources.Networks = 2000 }, wantErr: true},
		{name: "negative chaos duration", mutate: func(s *Scenario) { s.Chaos.Duration = Duration(-1) }, wantErr: true},
		{name: "chaos lifecycle ratio above one", mutate: func(s *Scenario) { s.Chaos.LifecycleRatio = floatPtr(1.5) }, wantErr: true},
		{name: "no servers relaxes image/flavor", mutate: func(s *Scenario) {
			s.Resources.Servers = 0
			s.Image = ""
			s.Flavor = ""
			s.ResizeFlavor = ""
			s.Distribution.ResizedRatio = 0
			s.Distribution.BootFromVolumeRatio = 0
			s.Distribution.VolumesPerServer.Max = 0
			s.Distribution.NetworksPerServer.Min = 0
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
		"seed":                                 "99",
		"image":                                "ubuntu",
		"flavor":                               "m1.medium",
		"resize_flavor":                        "m1.large",
		"resources.servers":                    "10",
		"resources.networks":                   "5",
		"distribution.networks_per_server.min": "1",
		"distribution.networks_per_server.max": "3",
		"distribution.volumes_per_server.min":  "0",
		"distribution.volumes_per_server.max":  "4",
		"distribution.ports_per_server.min":    "0",
		"distribution.ports_per_server.max":    "2",
		"distribution.attached_volume_gib.min": "1",
		"distribution.attached_volume_gib.max": "8",
		"distribution.root_volume_gib.min":     "2",
		"distribution.root_volume_gib.max":     "6",
		"distribution.boot_from_volume_ratio":  "0.2",
		"distribution.user_data_ratio":         "0.3",
		"distribution.stop_start_ratio":        "0.4",
		"distribution.stop_start_hard_ratio":   "0.5",
		"distribution.resized_ratio":           "0.1",
		"distribution.live_migrated_ratio":     "0.2",
		"distribution.deleted_ratio":           "0.3",
		"distribution.volume_detach_ratio":     "0.4",
		"distribution.port_detach_ratio":       "0.5",
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
	if err := s.Set("resources.servers", "notanint"); err == nil {
		t.Error("Set of a non-integer value: expected an error, got nil")
	}
}
