package scenario

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	novascenario "github.com/B42Labs/dizzy/internal/nova/scenario"
)

func TestParseRejectsUnknownKey(t *testing.T) {
	for key, data := range map[string]string{
		"personas.nope":           "name: x\npersonas:\n  nope:\n    share: 1\n",
		"personas.legacy.nope":    "name: x\npersonas:\n  legacy:\n    nope: 1\n",
		"personas.gardener.nope":  "name: x\npersonas:\n  gardener:\n    nope: 1\n",
		"lanes.cinder.volumetype": "name: x\nlanes:\n  cinder:\n    volumetype: x\n",
		"lanes.swift":             "name: x\nlanes:\n  swift: {}\n",
	} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(data))
			if err == nil {
				t.Fatalf("Parse accepted the unknown key %s", key)
			}
			if !strings.HasPrefix(err.Error(), "parsing scenario:") {
				t.Errorf("error %q does not start with %q", err, "parsing scenario:")
			}
		})
	}
}

// TestParseLegacyBlock confirms every key of the legacy block decodes into its
// field.
func TestParseLegacyBlock(t *testing.T) {
	s, err := Parse([]byte(`
personas:
  legacy:
    share: 0.2
    cloud: tenant-legacy
    networks: 3
    resize_flavor: m1.small
    volumes_per_server: { min: 1, max: 2 }
    volume_gib: { min: 3, max: 4 }
    ports_per_server: { min: 0, max: 5 }
    interval: { min: 10s, max: 1m }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Legacy{
		Share: 0.2, Cloud: "tenant-legacy", Networks: 3, ResizeFlavor: "m1.small",
		VolumesPerServer: novascenario.Range{Min: 1, Max: 2},
		VolumeGiB:        novascenario.Range{Min: 3, Max: 4},
		PortsPerServer:   novascenario.Range{Min: 0, Max: 5},
		Interval:         novascenario.Interval{Min: novascenario.Duration(10 * time.Second), Max: novascenario.Duration(time.Minute)},
	}
	if s.Personas.Legacy != want {
		t.Errorf("personas.legacy = %+v, want %+v", s.Personas.Legacy, want)
	}
}

// TestParseWithoutLegacy confirms a scenario without a legacy block has a zero
// Legacy and generates a plan with the CI persona alone.
func TestParseWithoutLegacy(t *testing.T) {
	s, err := Parse([]byte(`
name: ci-only
image: cirros
flavor: m1.tiny
resources: { servers: 3 }
personas:
  ci: { share: 1, networks: 1 }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Personas.Legacy != (Legacy{}) {
		t.Errorf("personas.legacy = %+v, want the zero block", s.Personas.Legacy)
	}
	p, err := s.Generate(LaneScenarios{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(p.Personas) != 1 || p.Personas[0].Name != "ci" || p.Personas[0].Servers != 3 {
		t.Errorf("personas = %+v, want ci with 3 servers alone", p.Personas)
	}
}

// TestParseGardenerBlock confirms every key of the gardener block decodes into
// its field.
func TestParseGardenerBlock(t *testing.T) {
	s, err := Parse([]byte(`
personas:
  gardener:
    share: 0.3
    cloud: tenant-gardener
    clusters: 2
    policy: anti-affinity
    volume_gib: { min: 1, max: 2 }
    interval: { min: 10s, max: 1m }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Gardener{
		Share: 0.3, Cloud: "tenant-gardener", Clusters: 2, Policy: "anti-affinity",
		VolumeGiB: novascenario.Range{Min: 1, Max: 2},
		Interval:  novascenario.Interval{Min: novascenario.Duration(10 * time.Second), Max: novascenario.Duration(time.Minute)},
	}
	if s.Personas.Gardener != want {
		t.Errorf("personas.gardener = %+v, want %+v", s.Personas.Gardener, want)
	}
}

// beforeGardenerSHA256 is the SHA-256 of testdata/golden/small.plan.json as
// it was before the Gardener persona existed: the plan of the small profile
// with the CI and the Legacy persona alone.
const beforeGardenerSHA256 = "f912c7098069ff5725e457139106febb21e971cdfce52b6117430c46334f95d7"

// TestParseWithoutGardener confirms a scenario without a gardener block has a
// zero Gardener, validates, and generates byte for byte the plan it generated
// before the Gardener persona existed.
func TestParseWithoutGardener(t *testing.T) {
	s, err := Parse([]byte(`
name: small
seed: 42
image: cirros
flavor: m1.tiny
services: []
resources: { servers: 6 }
personas:
  ci:
    share: 0.8
    networks: 2
    volumes_per_server: { min: 0, max: 1 }
    volume_gib: { min: 1, max: 2 }
    interval: { min: 100ms, max: 1s }
    churn_ratio: 0.5
    target_fill: 0.6
  legacy:
    share: 0.2
    networks: 1
    resize_flavor: m1.small
    volumes_per_server: { min: 1, max: 2 }
    volume_gib: { min: 1, max: 2 }
    ports_per_server: { min: 0, max: 1 }
    interval: { min: 10s, max: 1m }
chaos: { duration: 5m, parallel: { max: 4 } }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Personas.Gardener != (Gardener{}) {
		t.Errorf("personas.gardener = %+v, want the zero block", s.Personas.Gardener)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	p, err := s.Generate(LaneScenarios{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if sum := sha256.Sum256(marshal(t, p)); hex.EncodeToString(sum[:]) != beforeGardenerSHA256 {
		t.Errorf("plan without a gardener block differs from the plan generated before the persona existed:\n%s", marshal(t, p))
	}
}

// TestParseWithoutLanes confirms a scenario without a lanes key parses to four
// disabled lanes.
func TestParseWithoutLanes(t *testing.T) {
	s, err := Parse([]byte("name: x\npersonas:\n  ci: { share: 1 }\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Lanes != (Lanes{}) {
		t.Errorf("lanes = %+v, want four disabled lanes", s.Lanes)
	}
}

// TestParseLanesBlock confirms every key of the lanes block decodes into its
// field, the shared keys through the embedded Lane.
func TestParseLanesBlock(t *testing.T) {
	s, err := Parse([]byte(`
lanes:
  cinder:   { enabled: true, cloud: tenant-cinder, profile: small, volume_type: ssd }
  glance:   { enabled: true, cloud: tenant-glance, scenario: lanes/glance.yaml }
  keystone: { enabled: false, profile: medium, privilege: domain-manager, domain: soak, roles: "member,reader" }
  neutron:  { enabled: true, profile: large, external_network: public }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Lanes{
		Cinder:   CinderLane{Lane: Lane{Enabled: true, Cloud: "tenant-cinder", Profile: "small"}, VolumeType: "ssd"},
		Glance:   Lane{Enabled: true, Cloud: "tenant-glance", Scenario: "lanes/glance.yaml"},
		Keystone: KeystoneLane{Lane: Lane{Profile: "medium"}, Privilege: "domain-manager", Domain: "soak", Roles: "member,reader"},
		Neutron:  NeutronLane{Lane: Lane{Enabled: true, Profile: "large"}, ExternalNetwork: "public"},
	}
	if s.Lanes != want {
		t.Errorf("lanes = %+v, want %+v", s.Lanes, want)
	}
}

// TestLanesEnabled confirms Enabled names every enabled lane with its keys, in
// canonical order, and skips a disabled one.
func TestLanesEnabled(t *testing.T) {
	l := Lanes{
		Cinder:   CinderLane{Lane: Lane{Enabled: true, Cloud: "tenant-cinder", Scenario: "cinder.yaml"}, VolumeType: "ssd"},
		Keystone: KeystoneLane{Lane: Lane{Cloud: "admin", Profile: "small"}},
		Neutron:  NeutronLane{Lane: Lane{Enabled: true, Profile: "large"}},
	}
	want := []NamedLane{
		{Name: "cinder", Lane: Lane{Enabled: true, Cloud: "tenant-cinder", Scenario: "cinder.yaml"}},
		{Name: "neutron", Lane: Lane{Enabled: true, Profile: "large"}},
	}
	if got := l.Enabled(); !reflect.DeepEqual(got, want) {
		t.Errorf("Enabled() = %+v, want %+v", got, want)
	}
	if got := (&Lanes{}).Enabled(); len(got) != 0 {
		t.Errorf("Enabled() without an enabled lane = %+v, want none", got)
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
		{"zero shares", func(s *Scenario) { s.Personas.CI.Share = 0; s.Personas.Gardener.Share = 0; s.Personas.Legacy.Share = 0 },
			"at least one persona must have a share above 0"},
		{"negative legacy share", func(s *Scenario) { s.Personas.Legacy.Share = -1 }, "personas.legacy.share must be a finite number of at least 0, got -1"},
		{"NaN legacy share", func(s *Scenario) { s.Personas.Legacy.Share = math.NaN() }, "personas.legacy.share must be a finite number of at least 0, got NaN"},
		{"legacy alone", func(s *Scenario) {
			s.Personas.CI.Share = 0
			s.Personas.Gardener.Share = 0
			s.Personas.Legacy = legacyBlock()
		}, ""},
		{"negative interval", func(s *Scenario) { s.Personas.CI.Interval.Min = d(-time.Second) }, "personas.ci.interval.min must not be negative, got -1s"},
		{"inverted interval", func(s *Scenario) { s.Personas.CI.Interval.Min = d(2 * time.Second) }, "personas.ci.interval.min (2s) must not exceed personas.ci.interval.max (1s)"},
		{"churn ratio above 1", func(s *Scenario) { s.Personas.CI.ChurnRatio = 1.5 }, "personas.ci.churn_ratio must be between 0 and 1, got 1.5"},
		{"NaN churn ratio", func(s *Scenario) { s.Personas.CI.ChurnRatio = math.NaN() }, "personas.ci.churn_ratio must be between 0 and 1, got NaN"},
		{"negative churn ratio", func(s *Scenario) { s.Personas.CI.ChurnRatio = -0.1 }, "personas.ci.churn_ratio must be between 0 and 1, got -0.1"},
		{"negative target fill", func(s *Scenario) { s.Personas.CI.TargetFill = -0.1 }, "personas.ci.target_fill must be between 0 and 1, got -0.1"},
		{"target fill above 1", func(s *Scenario) { s.Personas.CI.TargetFill = 1.5 }, "personas.ci.target_fill must be between 0 and 1, got 1.5"},
		{"NaN target fill", func(s *Scenario) { s.Personas.CI.TargetFill = math.NaN() }, "personas.ci.target_fill must be between 0 and 1, got NaN"},
		{"negative legacy interval", func(s *Scenario) { s.Personas.Legacy.Interval.Min = d(-time.Second) }, "personas.legacy.interval.min must not be negative, got -1s"},
		{"inverted legacy interval", func(s *Scenario) {
			s.Personas.Legacy.Interval = novascenario.Interval{Min: d(2 * time.Second), Max: d(time.Second)}
		}, "personas.legacy.interval.min (2s) must not exceed personas.legacy.interval.max (1s)"},
		{"negative duration", func(s *Scenario) { s.Chaos.Duration = d(-time.Minute) }, "chaos.duration must not be negative, got -1m0s"},
		{"negative bucket width", func(s *Scenario) { s.Chaos.BucketWidth = d(-time.Minute) }, "chaos.bucket_width must not be negative, got -1m0s"},
		{"negative parallel", func(s *Scenario) { s.Chaos.Parallel.Max = -1 }, "chaos.parallel.max must not be negative, got -1"},
		{"no chaos block", func(s *Scenario) { s.Chaos = nil }, ""},
		{"empty service", func(s *Scenario) { s.Services = []string{"octavia", ""} }, "services[1] must not be empty"},
		{"duplicate service", func(s *Scenario) { s.Services = []string{"octavia", "octavia"} }, `services lists "octavia" twice`},
		{"no networks", func(s *Scenario) { s.Personas.CI.Networks = 0 }, "personas.ci: distribution.networks_per_server.max (1) must not exceed resources.networks (0)"},
		{"no image", func(s *Scenario) { s.Image = "" }, "personas.ci: image must be set when resources.servers > 0"},
		{"no networks without servers", func(s *Scenario) { s.Personas.CI.Networks = 0; s.Resources.Servers = 0 }, ""},
		{"legacy resize flavor equals flavor", func(s *Scenario) { s.Personas.Legacy = legacyBlock(); s.Personas.Legacy.ResizeFlavor = "m1.tiny" },
			`personas.legacy: resize_flavor ("m1.tiny") must differ from flavor ("m1.tiny")`},
		{"legacy without networks", func(s *Scenario) { s.Personas.Legacy = legacyBlock(); s.Personas.Legacy.Networks = 0 },
			"personas.legacy: distribution.networks_per_server.max (1) must not exceed resources.networks (0)"},
		{"inactive legacy block", func(s *Scenario) { s.Personas.Legacy = Legacy{Networks: 0, ResizeFlavor: "m1.tiny"} }, ""},
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

// TestValidateLanes covers the checks of an enabled lane, in canonical order,
// and confirms a disabled lane with the same values passes.
func TestValidateLanes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Lanes)
		want   string
	}{
		{"profile and scenario", func(l *Lanes) { l.Glance = Lane{Enabled: true, Profile: "small", Scenario: "glance.yaml"} },
			"lanes.glance: set profile or scenario, not both"},
		{"neither profile nor scenario", func(l *Lanes) { l.Glance = Lane{Enabled: true} },
			"lanes.glance: an enabled lane needs a profile or a scenario"},
		{"unknown profile", func(l *Lanes) { l.Glance = Lane{Enabled: true, Profile: "huge"} },
			`lanes.glance.profile must be small, medium or large, got "huge"`},
		{"unknown privilege", func(l *Lanes) {
			l.Keystone = KeystoneLane{Lane: Lane{Enabled: true, Profile: "small"}, Privilege: "root"}
		},
			`lanes.keystone.privilege must be auto, admin or domain-manager, got "root"`},
		{"two bad lanes", func(l *Lanes) {
			l.Cinder.Lane = Lane{Enabled: true}
			l.Neutron.Lane = Lane{Enabled: true, Profile: "huge"}
		},
			"lanes.cinder: an enabled lane needs a profile or a scenario"},
		{"empty privilege", func(l *Lanes) { l.Keystone = KeystoneLane{Lane: Lane{Enabled: true, Profile: "small"}} }, ""},
		{"every lane on a profile or a scenario", func(l *Lanes) {
			l.Cinder.Lane = Lane{Enabled: true, Profile: "small"}
			l.Glance = Lane{Enabled: true, Scenario: "glance.yaml"}
			l.Keystone = KeystoneLane{Lane: Lane{Enabled: true, Profile: "medium"}, Privilege: "domain-manager"}
			l.Neutron.Lane = Lane{Enabled: true, Profile: "large"}
		}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := smallScenario()
			tc.mutate(&s.Lanes)
			err := s.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("Validate() = %v, want %q", err, tc.want)
			}

			// The same values on a disabled lane are not checked.
			for _, b := range s.Lanes.blocks() {
				b.lane.Enabled = false
			}
			if err := s.Validate(); err != nil {
				t.Errorf("Validate() with every lane disabled = %v, want nil", err)
			}
		})
	}
}

// TestValidateGardener covers the gardener block: its share, its interval,
// its policy and cluster count, which are checked only when the persona has a
// share, and the compute scenario it builds.
func TestValidateGardener(t *testing.T) {
	d := func(v time.Duration) novascenario.Duration { return novascenario.Duration(v) }
	tests := []struct {
		name   string
		mutate func(*Scenario)
		want   string
	}{
		{"valid", func(*Scenario) {}, ""},
		{"hard policy", func(s *Scenario) { s.Personas.Gardener.Policy = "anti-affinity" }, ""},
		{"gardener alone", func(s *Scenario) { s.Personas.CI.Share = 0; s.Personas.Legacy.Share = 0 }, ""},
		{"zero servers", func(s *Scenario) { s.Resources.Servers = 0 }, ""},
		{"negative share", func(s *Scenario) { s.Personas.Gardener.Share = -1 }, "personas.gardener.share must be a finite number of at least 0, got -1"},
		{"negative interval", func(s *Scenario) { s.Personas.Gardener.Interval.Min = d(-time.Second) }, "personas.gardener.interval.min must not be negative, got -1s"},
		{"inverted interval", func(s *Scenario) {
			s.Personas.Gardener.Interval = novascenario.Interval{Min: d(2 * time.Second), Max: d(time.Second)}
		}, "personas.gardener.interval.min (2s) must not exceed personas.gardener.interval.max (1s)"},
		{"empty policy", func(s *Scenario) { s.Personas.Gardener.Policy = "" },
			`personas.gardener.policy must be "anti-affinity" or "soft-anti-affinity", got ""`},
		{"affinity policy", func(s *Scenario) { s.Personas.Gardener.Policy = "affinity" },
			`personas.gardener.policy must be "anti-affinity" or "soft-anti-affinity", got "affinity"`},
		{"inactive block without policy", func(s *Scenario) { s.Personas.Gardener = Gardener{} }, ""},
		{"no cluster", func(s *Scenario) { s.Personas.Gardener.Clusters = 0 }, "personas.gardener.clusters must be at least 1, got 0"},
		{"more clusters than servers", func(s *Scenario) { s.Personas.Gardener.Clusters = 3 }, "personas.gardener.clusters (3) exceeds the persona's 2 server(s)"},
		{"zero volume size", func(s *Scenario) { s.Personas.Gardener.VolumeGiB = novascenario.Range{} },
			"personas.gardener: distribution.attached_volume_gib.min must be at least 1 when volumes_per_server.max > 0, got 0"},
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
		{"personas.gardener.share", "0.4", func(s Scenario) bool { return s.Personas.Gardener.Share == 0.4 }},
		{"personas.gardener.cloud", "tenant-gardener", func(s Scenario) bool { return s.Personas.Gardener.Cloud == "tenant-gardener" }},
		{"personas.gardener.clusters", "3", func(s Scenario) bool { return s.Personas.Gardener.Clusters == 3 }},
		{"personas.gardener.policy", "anti-affinity", func(s Scenario) bool { return s.Personas.Gardener.Policy == "anti-affinity" }},
		{"personas.gardener.volume_gib.min", "2", func(s Scenario) bool { return s.Personas.Gardener.VolumeGiB.Min == 2 }},
		{"personas.gardener.volume_gib.max", "4", func(s Scenario) bool { return s.Personas.Gardener.VolumeGiB.Max == 4 }},
		{"personas.gardener.interval.min", "5s", func(s Scenario) bool {
			return s.Personas.Gardener.Interval.Min == novascenario.Duration(5*time.Second)
		}},
		{"personas.gardener.interval.max", "2m", func(s Scenario) bool {
			return s.Personas.Gardener.Interval.Max == novascenario.Duration(2*time.Minute)
		}},
		{"personas.legacy.share", "0.3", func(s Scenario) bool { return s.Personas.Legacy.Share == 0.3 }},
		{"personas.legacy.cloud", "tenant-legacy", func(s Scenario) bool { return s.Personas.Legacy.Cloud == "tenant-legacy" }},
		{"personas.legacy.networks", "2", func(s Scenario) bool { return s.Personas.Legacy.Networks == 2 }},
		{"personas.legacy.resize_flavor", "m1.medium", func(s Scenario) bool { return s.Personas.Legacy.ResizeFlavor == "m1.medium" }},
		{"personas.legacy.resize_flavor", "", func(s Scenario) bool { return s.Personas.Legacy.ResizeFlavor == "" }},
		{"personas.legacy.volumes_per_server.min", "1", func(s Scenario) bool { return s.Personas.Legacy.VolumesPerServer.Min == 1 }},
		{"personas.legacy.volumes_per_server.max", "3", func(s Scenario) bool { return s.Personas.Legacy.VolumesPerServer.Max == 3 }},
		{"personas.legacy.volume_gib.min", "2", func(s Scenario) bool { return s.Personas.Legacy.VolumeGiB.Min == 2 }},
		{"personas.legacy.volume_gib.max", "4", func(s Scenario) bool { return s.Personas.Legacy.VolumeGiB.Max == 4 }},
		{"personas.legacy.ports_per_server.min", "1", func(s Scenario) bool { return s.Personas.Legacy.PortsPerServer.Min == 1 }},
		{"personas.legacy.ports_per_server.max", "2", func(s Scenario) bool { return s.Personas.Legacy.PortsPerServer.Max == 2 }},
		{"personas.legacy.interval.min", "5s", func(s Scenario) bool {
			return s.Personas.Legacy.Interval.Min == novascenario.Duration(5*time.Second)
		}},
		{"personas.legacy.interval.max", "2m", func(s Scenario) bool {
			return s.Personas.Legacy.Interval.Max == novascenario.Duration(2*time.Minute)
		}},
		{"lanes.cinder.enabled", "true", func(s Scenario) bool { return s.Lanes.Cinder.Enabled }},
		{"lanes.cinder.cloud", "tenant-cinder", func(s Scenario) bool { return s.Lanes.Cinder.Cloud == "tenant-cinder" }},
		{"lanes.cinder.profile", "medium", func(s Scenario) bool { return s.Lanes.Cinder.Profile == "medium" }},
		{"lanes.cinder.scenario", "lanes/cinder.yaml", func(s Scenario) bool { return s.Lanes.Cinder.Scenario == "lanes/cinder.yaml" }},
		{"lanes.cinder.volume_type", "ssd", func(s Scenario) bool { return s.Lanes.Cinder.VolumeType == "ssd" }},
		{"lanes.glance.enabled", " 1 ", func(s Scenario) bool { return s.Lanes.Glance.Enabled }},
		{"lanes.glance.cloud", "tenant-glance", func(s Scenario) bool { return s.Lanes.Glance.Cloud == "tenant-glance" }},
		{"lanes.glance.profile", "large", func(s Scenario) bool { return s.Lanes.Glance.Profile == "large" }},
		{"lanes.glance.scenario", "lanes/glance.yaml", func(s Scenario) bool { return s.Lanes.Glance.Scenario == "lanes/glance.yaml" }},
		{"lanes.keystone.enabled", "true", func(s Scenario) bool { return s.Lanes.Keystone.Enabled }},
		{"lanes.keystone.cloud", "admin", func(s Scenario) bool { return s.Lanes.Keystone.Cloud == "admin" }},
		{"lanes.keystone.profile", "", func(s Scenario) bool { return s.Lanes.Keystone.Profile == "" }},
		{"lanes.keystone.scenario", "lanes/keystone.yaml", func(s Scenario) bool { return s.Lanes.Keystone.Scenario == "lanes/keystone.yaml" }},
		{"lanes.keystone.privilege", "domain-manager", func(s Scenario) bool { return s.Lanes.Keystone.Privilege == "domain-manager" }},
		{"lanes.keystone.domain", "soak", func(s Scenario) bool { return s.Lanes.Keystone.Domain == "soak" }},
		{"lanes.keystone.roles", "member", func(s Scenario) bool { return s.Lanes.Keystone.Roles == "member" }},
		{"lanes.neutron.enabled", "false", func(s Scenario) bool { return !s.Lanes.Neutron.Enabled }},
		{"lanes.neutron.cloud", "tenant-net", func(s Scenario) bool { return s.Lanes.Neutron.Cloud == "tenant-net" }},
		{"lanes.neutron.profile", "small", func(s Scenario) bool { return s.Lanes.Neutron.Profile == "small" }},
		{"lanes.neutron.scenario", "lanes/neutron.yaml", func(s Scenario) bool { return s.Lanes.Neutron.Scenario == "lanes/neutron.yaml" }},
		{"lanes.neutron.external_network", "public", func(s Scenario) bool { return s.Lanes.Neutron.ExternalNetwork == "public" }},
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
		{"personas.legacy.nope", "1", `unknown override key "personas.legacy.nope"`},
		{"personas.legacy.interval.min", "x", `override personas.legacy.interval.min: "x" is not a duration`},
		{"personas.legacy.networks", "two", `override personas.legacy.networks: "two" is not an integer`},
		{"personas.gardener.nope", "1", `unknown override key "personas.gardener.nope"`},
		{"personas.gardener.clusters", "x", `override personas.gardener.clusters: "x" is not an integer`},
		{"lanes.cinder.enabled", "maybe", `override lanes.cinder.enabled: "maybe" is not a boolean`},
		{"lanes.swift.enabled", "true", `unknown override key "lanes.swift.enabled"`},
		{"lanes.glance.volume_type", "ssd", `unknown override key "lanes.glance.volume_type"`},
		{"enabled", "true", `unknown override key "enabled"`},
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
	f.Add([]byte("name: x\nresources: { servers: 2 }\npersonas:\n  legacy: { share: 1, networks: 1, resize_flavor: m1.small, ports_per_server: { min: 0, max: 1 } }\n"))
	f.Add([]byte("name: x\nresources: { servers: 3 }\npersonas:\n  gardener: { share: 1, clusters: 2, policy: anti-affinity, volume_gib: { min: 1, max: 1 } }\n"))
	f.Add([]byte("name: x\nlanes:\n  cinder: { enabled: true, profile: small, scenario: c.yaml }\n  keystone: { enabled: true, profile: small, privilege: root }\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := Parse(data)
		if err != nil {
			return
		}
		_ = s.Validate()
	})
}
