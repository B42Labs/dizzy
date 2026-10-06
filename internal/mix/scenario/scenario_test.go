package scenario

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	novascenario "github.com/B42Labs/dizzy/internal/nova/scenario"
)

func TestParseRejectsUnknownKey(t *testing.T) {
	_, err := Parse([]byte("name: x\npersonas:\n  legacy:\n    share: 1\n"))
	if err == nil {
		t.Fatal("Parse accepted the unknown key personas.legacy")
	}
	if !strings.HasPrefix(err.Error(), "parsing scenario:") {
		t.Errorf("error %q does not start with %q", err, "parsing scenario:")
	}
}

// TestParseEmptyInput confirms empty input decodes to the zero scenario, which
// Validate then rejects for its missing name.
func TestParseEmptyInput(t *testing.T) {
	s, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse(nil) = %v, want nil", err)
	}
	if !reflect.DeepEqual(s, Scenario{}) {
		t.Errorf("Parse(nil) = %+v, want the zero scenario", s)
	}
	if err := s.Validate(); err == nil || err.Error() != "name must not be empty" {
		t.Errorf("Validate() = %v, want %q", err, "name must not be empty")
	}
}

func TestValidate(t *testing.T) {
	d := func(v time.Duration) novascenario.Duration { return novascenario.Duration(v) }
	tests := []struct {
		name   string
		mutate func(*Scenario)
		want   string
	}{
		{"valid", func(*Scenario) {}, ""},
		{"zero servers is valid", func(s *Scenario) { s.Resources.Servers = 0 }, ""},
		{"empty name", func(s *Scenario) { s.Name = "" }, "name must not be empty"},
		{"negative servers", func(s *Scenario) { s.Resources.Servers = -1 }, "resources.servers must not be negative, got -1"},
		{"too many servers", func(s *Scenario) { s.Resources.Servers = 1_000_001 }, "resources.servers exceeds the limit of 1000000, got 1000001"},
		{"negative share", func(s *Scenario) { s.Personas.CI.Share = -1 }, "personas.ci.share must be a finite number of at least 0, got -1"},
		{"NaN share", func(s *Scenario) { s.Personas.CI.Share = math.NaN() }, "personas.ci.share must be a finite number of at least 0, got NaN"},
		{"infinite share", func(s *Scenario) { s.Personas.CI.Share = math.Inf(1) }, "personas.ci.share must be a finite number of at least 0, got +Inf"},
		{"zero share", func(s *Scenario) { s.Personas.CI.Share = 0 }, "at least one persona must have a share above 0"},
		{"negative interval", func(s *Scenario) { s.Personas.CI.Interval.Min = d(-time.Second) }, "personas.ci.interval.min must not be negative, got -1s"},
		{"inverted interval", func(s *Scenario) { s.Personas.CI.Interval.Min = d(2 * time.Second) }, "personas.ci.interval.min (2s) must not exceed personas.ci.interval.max (1s)"},
		{"churn ratio above 1", func(s *Scenario) { s.Personas.CI.ChurnRatio = 1.5 }, "personas.ci.churn_ratio must be between 0 and 1, got 1.5"},
		{"NaN churn ratio", func(s *Scenario) { s.Personas.CI.ChurnRatio = math.NaN() }, "personas.ci.churn_ratio must be between 0 and 1, got NaN"},
		{"negative churn ratio", func(s *Scenario) { s.Personas.CI.ChurnRatio = -0.1 }, "personas.ci.churn_ratio must be between 0 and 1, got -0.1"},
		{"negative target fill", func(s *Scenario) { s.Personas.CI.TargetFill = -0.1 }, "personas.ci.target_fill must be between 0 and 1, got -0.1"},
		{"target fill above 1", func(s *Scenario) { s.Personas.CI.TargetFill = 1.5 }, "personas.ci.target_fill must be between 0 and 1, got 1.5"},
		{"NaN target fill", func(s *Scenario) { s.Personas.CI.TargetFill = math.NaN() }, "personas.ci.target_fill must be between 0 and 1, got NaN"},
		{"negative duration", func(s *Scenario) { s.Chaos.Duration = d(-time.Minute) }, "chaos.duration must not be negative, got -1m0s"},
		{"negative bucket width", func(s *Scenario) { s.Chaos.BucketWidth = d(-time.Minute) }, "chaos.bucket_width must not be negative, got -1m0s"},
		{"negative parallel", func(s *Scenario) { s.Chaos.Parallel.Max = -1 }, "chaos.parallel.max must not be negative, got -1"},
		{"no chaos block", func(s *Scenario) { s.Chaos = nil }, ""},
		{"empty service", func(s *Scenario) { s.Services = []string{"octavia", ""} }, "services[1] must not be empty"},
		{"duplicate service", func(s *Scenario) { s.Services = []string{"octavia", "octavia"} }, `services lists "octavia" twice`},
		{"no networks", func(s *Scenario) { s.Personas.CI.Networks = 0 }, "personas.ci: distribution.networks_per_server.max (1) must not exceed resources.networks (0)"},
		{"no image", func(s *Scenario) { s.Image = "" }, "personas.ci: image must be set when resources.servers > 0"},
		{"no networks without servers", func(s *Scenario) { s.Personas.CI.Networks = 0; s.Resources.Servers = 0 }, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := smallScenario()
			tc.mutate(&s)
			err := s.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSet(t *testing.T) {
	tests := []struct {
		key, value string
		check      func(Scenario) bool
	}{
		{"seed", "7", func(s Scenario) bool { return s.Seed == 7 }},
		{"image", "ubuntu", func(s Scenario) bool { return s.Image == "ubuntu" }},
		{"flavor", "m1.small", func(s Scenario) bool { return s.Flavor == "m1.small" }},
		{"resources.servers", "3", func(s Scenario) bool { return s.Resources.Servers == 3 }},
		{"services", "octavia, barbican", func(s Scenario) bool { return reflect.DeepEqual(s.Services, []string{"octavia", "barbican"}) }},
		{"services", "", func(s Scenario) bool { return len(s.Services) == 0 }},
		{"personas.ci.share", "0.5", func(s Scenario) bool { return s.Personas.CI.Share == 0.5 }},
		{"personas.ci.cloud", "tenant-ci", func(s Scenario) bool { return s.Personas.CI.Cloud == "tenant-ci" }},
		{"personas.ci.networks", "5", func(s Scenario) bool { return s.Personas.CI.Networks == 5 }},
		{"personas.ci.volumes_per_server.min", "1", func(s Scenario) bool { return s.Personas.CI.VolumesPerServer.Min == 1 }},
		{"personas.ci.volumes_per_server.max", "3", func(s Scenario) bool { return s.Personas.CI.VolumesPerServer.Max == 3 }},
		{"personas.ci.volume_gib.min", "2", func(s Scenario) bool { return s.Personas.CI.VolumeGiB.Min == 2 }},
		{"personas.ci.volume_gib.max", "4", func(s Scenario) bool { return s.Personas.CI.VolumeGiB.Max == 4 }},
		{"personas.ci.interval.min", "50ms", func(s Scenario) bool {
			return s.Personas.CI.Interval.Min == novascenario.Duration(50*time.Millisecond)
		}},
		{"personas.ci.interval.max", "2s", func(s Scenario) bool {
			return s.Personas.CI.Interval.Max == novascenario.Duration(2*time.Second)
		}},
		{"personas.ci.churn_ratio", "0.25", func(s Scenario) bool { return s.Personas.CI.ChurnRatio == 0.25 }},
		{"personas.ci.target_fill", "0.9", func(s Scenario) bool { return s.Personas.CI.TargetFill == 0.9 }},
	}
	for _, tc := range tests {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Parallel()
			s := smallScenario()
			s.Services = []string{"old"}
			if err := s.Set(tc.key, tc.value); err != nil {
				t.Fatalf("Set(%q, %q) = %v", tc.key, tc.value, err)
			}
			if !tc.check(s) {
				t.Errorf("Set(%q, %q) left %+v", tc.key, tc.value, s)
			}
		})
	}
}

func TestSetErrors(t *testing.T) {
	tests := []struct {
		key, value, want string
	}{
		{"personas.ci.nope", "1", `unknown override key "personas.ci.nope"`},
		{"resources.servers", "x", `override resources.servers: "x" is not an integer`},
		{"seed", "1.5", `override seed: "1.5" is not an integer`},
		{"personas.ci.share", "half", `override personas.ci.share: "half" is not a number`},
		{"personas.ci.interval.min", "5", `override personas.ci.interval.min: "5" is not a duration`},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			s := smallScenario()
			if err := s.Set(tc.key, tc.value); err == nil || err.Error() != tc.want {
				t.Errorf("Set(%q, %q) = %v, want %q", tc.key, tc.value, err, tc.want)
			}
		})
	}
}

// FuzzParse checks that neither Parse nor Validate panics on arbitrary input.
func FuzzParse(f *testing.F) {
	f.Add([]byte("name: small\nresources: { servers: 6 }\npersonas:\n  ci: { share: 1, networks: 2 }\n"))
	f.Add([]byte("name: x\npersonas:\n  ci: { share: .nan }\n"))
	f.Add([]byte("chaos: { duration: 1m, parallel: { max: 4 } }\nservices: [a, a]\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := Parse(data)
		if err != nil {
			return
		}
		_ = s.Validate()
	})
}
