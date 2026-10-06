package scenario

import (
	"reflect"
	"testing"
	"time"

	novascenario "github.com/B42Labs/dizzy/internal/nova/scenario"
	"github.com/B42Labs/dizzy/scenarios"
)

// profileNames are the built-in mix scenario profiles shipped under
// scenarios/mix/.
var profileNames = []string{"small", "medium", "large"}

// readProfile reads and parses a shipped mix profile by name from the embedded
// scenarios filesystem, so the test does not depend on the process working
// directory.
func readProfile(t *testing.T, name string) Scenario {
	t.Helper()
	data, err := scenarios.Files.ReadFile("mix/" + name + ".yaml")
	if err != nil {
		t.Fatalf("reading profile mix/%s.yaml: %v", name, err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing profile mix/%s.yaml: %v", name, err)
	}
	return s
}

// TestProfilesGenerateValidPlans confirms every shipped mix profile parses,
// names itself after its file, validates, and expands into a valid plan.
func TestProfilesGenerateValidPlans(t *testing.T) {
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := readProfile(t, name)
			if s.Name != name {
				t.Errorf("profile mix/%s.yaml has name %q, want %q", name, s.Name, name)
			}
			if err := s.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if _, err := s.Generate(LaneScenarios{}); err != nil {
				t.Fatalf("Generate() = %v, want nil", err)
			}
		})
	}
}

// TestProfilesLanes confirms every shipped mix profile has four disabled
// lanes on the small profiles, and that switching each on with Set yields a
// plan with that lane.
func TestProfilesLanes(t *testing.T) {
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := readProfile(t, name)
			want := Lanes{
				Cinder:   CinderLane{Lane: Lane{Profile: "small"}},
				Glance:   Lane{Profile: "small"},
				Keystone: KeystoneLane{Lane: Lane{Profile: "small"}},
				Neutron:  NeutronLane{Lane: Lane{Profile: "small"}},
			}
			if s.Lanes != want {
				t.Errorf("lanes = %+v, want %+v", s.Lanes, want)
			}
			for _, lane := range []string{"cinder", "glance", "keystone", "neutron"} {
				s := readProfile(t, name)
				if err := s.Set("lanes."+lane+".enabled", "true"); err != nil {
					t.Fatalf("Set: %v", err)
				}
				ls, err := s.LoadLanes(noRead(t))
				if err != nil {
					t.Fatalf("LoadLanes(%s): %v", lane, err)
				}
				p, err := s.Generate(ls)
				if err != nil {
					t.Fatalf("Generate(%s): %v", lane, err)
				}
				if len(p.Lanes) != 1 || p.Lanes[0].Name != lane || p.Lanes[0].Scenario() != name+"/"+lane {
					t.Errorf("lanes with %s enabled = %+v, want that lane alone under %s/%s", lane, p.Lanes, name, lane)
				}
			}
		})
	}
}

// TestSmallProfileMatchesFixture ties the shipped small profile to the
// smallScenario fixture the golden test builds on.
func TestSmallProfileMatchesFixture(t *testing.T) {
	if got, want := readProfile(t, "small"), smallScenario(); !reflect.DeepEqual(got, want) {
		t.Errorf("scenarios/mix/small.yaml = %+v, want %+v", got, want)
	}
}

// TestProfilesMatchDocumentedSizes locks the sizes and shared values the
// scenario schema reference documents for the three profiles.
func TestProfilesMatchDocumentedSizes(t *testing.T) {
	sizes := map[string]struct {
		servers, networks, clusters, legacyNetworks int
		duration                                    time.Duration
	}{
		"small":  {6, 2, 1, 1, 5 * time.Minute},
		"medium": {20, 4, 2, 2, 30 * time.Minute},
		"large":  {60, 8, 3, 2, time.Hour},
	}
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			s := readProfile(t, name)
			want := sizes[name]
			if s.Resources.Servers != want.servers || s.Personas.CI.Networks != want.networks {
				t.Errorf("servers/networks = %d/%d, want %d/%d", s.Resources.Servers, s.Personas.CI.Networks, want.servers, want.networks)
			}
			if s.Chaos == nil || time.Duration(s.Chaos.Duration) != want.duration || s.Chaos.Parallel.Max != 4 {
				t.Errorf("chaos = %+v, want duration %s and parallel max 4", s.Chaos, want.duration)
			}
			ci := s.Personas.CI
			if s.Seed != 42 || s.Image != "cirros" || s.Flavor != "m1.tiny" || s.Services == nil || len(s.Services) != 0 {
				t.Errorf("seed/image/flavor/services = %d/%s/%s/%#v, want 42/cirros/m1.tiny/[]", s.Seed, s.Image, s.Flavor, s.Services)
			}
			wantCI := CI{
				Share: 0.5, Networks: want.networks,
				VolumesPerServer: novascenario.Range{Min: 0, Max: 1},
				VolumeGiB:        novascenario.Range{Min: 1, Max: 2},
				Interval: novascenario.Interval{
					Min: novascenario.Duration(100 * time.Millisecond),
					Max: novascenario.Duration(time.Second),
				},
				ChurnRatio: 0.5, TargetFill: 0.6,
			}
			if ci != wantCI {
				t.Errorf("personas.ci = %+v, want %+v", ci, wantCI)
			}
			wantGardener := Gardener{
				Share: 0.3, Clusters: want.clusters, Policy: "soft-anti-affinity",
				VolumeGiB: novascenario.Range{Min: 1, Max: 2},
				Interval: novascenario.Interval{
					Min: novascenario.Duration(10 * time.Second),
					Max: novascenario.Duration(time.Minute),
				},
			}
			if s.Personas.Gardener != wantGardener {
				t.Errorf("personas.gardener = %+v, want %+v", s.Personas.Gardener, wantGardener)
			}
			wantLegacy := Legacy{
				Share: 0.2, Networks: want.legacyNetworks, ResizeFlavor: "m1.small",
				VolumesPerServer: novascenario.Range{Min: 1, Max: 2},
				VolumeGiB:        novascenario.Range{Min: 1, Max: 2},
				PortsPerServer:   novascenario.Range{Min: 0, Max: 1},
				Interval: novascenario.Interval{
					Min: novascenario.Duration(10 * time.Second),
					Max: novascenario.Duration(time.Minute),
				},
			}
			if !reflect.DeepEqual(s.Personas.Legacy, wantLegacy) {
				t.Errorf("personas.legacy = %+v, want %+v", s.Personas.Legacy, wantLegacy)
			}
		})
	}
}

// TestProfilesSplitServers locks how each profile divides its servers among
// the CI, the Gardener and the Legacy persona.
func TestProfilesSplitServers(t *testing.T) {
	split := map[string][3]int{"small": {3, 2, 1}, "medium": {10, 6, 4}, "large": {30, 18, 12}}
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, err := readProfile(t, name).Generate(LaneScenarios{})
			if err != nil {
				t.Fatalf("Generate() = %v, want nil", err)
			}
			if len(p.Personas) != 3 || p.Personas[0].Name != "ci" || p.Personas[1].Name != "gardener" || p.Personas[2].Name != "legacy" {
				t.Fatalf("personas = %+v, want ci, gardener and legacy", p.Personas)
			}
			var got [3]int
			for i, ps := range p.Personas {
				got[i] = len(ps.Nova.Servers)
			}
			if got != split[name] {
				t.Errorf("ci/gardener/legacy servers = %v, want %v", got, split[name])
			}
		})
	}
}

// TestSmallProfileFitsDefaultQuotas confirms the small profile stays within
// Nova's common default quota of 10 instances and its default limits of 10
// server groups and 10 members per group.
func TestSmallProfileFitsDefaultQuotas(t *testing.T) {
	p, err := readProfile(t, "small").Generate(LaneScenarios{})
	if err != nil {
		t.Fatalf("Generate(small): %v", err)
	}
	var servers, groups int
	members := map[string]int{}
	for _, ps := range p.Personas {
		servers += len(ps.Nova.Servers)
		groups += len(ps.Nova.ServerGroups)
		for _, srv := range ps.Nova.Servers {
			if srv.Group != "" {
				members[ps.Name+"/"+srv.Group]++
			}
		}
	}
	if servers > 10 {
		t.Errorf("small servers = %d, want <= 10 (default instance quota)", servers)
	}
	if groups == 0 || groups > 10 {
		t.Errorf("small server groups = %d, want 1 to 10 (default server group quota)", groups)
	}
	for group, n := range members {
		if n > 10 {
			t.Errorf("server group %s has %d members, want <= 10 (default member quota)", group, n)
		}
	}
}
