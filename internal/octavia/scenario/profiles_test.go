package scenario

import (
	"bytes"
	"io/fs"
	"os"
	"testing"

	"github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/scenarios"
)

// profileNames are the built-in Octavia scenario profiles shipped under
// scenarios/octavia/, smallest first.
var profileNames = []string{"small", "medium", "large"}

// readProfile reads and parses a shipped Octavia profile by name from the
// embedded scenarios filesystem, so the test does not depend on the process
// working directory.
func readProfile(t *testing.T, name string) Scenario {
	t.Helper()
	data, err := scenarios.Files.ReadFile("octavia/" + name + ".yaml")
	if err != nil {
		t.Fatalf("reading profile octavia/%s.yaml: %v", name, err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing profile octavia/%s.yaml: %v", name, err)
	}
	return s
}

// TestProfilesEmbedded asserts every Octavia profile is embedded under its
// service-scoped path, octavia/small.yaml among them, so the generate and
// apply commands and the image test find them without the source tree.
func TestProfilesEmbedded(t *testing.T) {
	for _, name := range profileNames {
		path := "octavia/" + name + ".yaml"
		if _, err := fs.Stat(scenarios.Files, path); err != nil {
			t.Errorf("scenarios.Files lacks %s: %v", path, err)
		}
	}
}

// TestProfilesGenerateValidPlans locks the core acceptance criterion: every
// shipped Octavia profile parses, names itself after its file, validates, and
// expands into a valid plan with exactly the profile's counts of networks, load
// balancers, listeners, pools, members, and health monitors.
func TestProfilesGenerateValidPlans(t *testing.T) {
	wantCounts := map[string][6]int{
		"small":  {1, 2, 4, 4, 8, 2},
		"medium": {2, 5, 10, 10, 30, 5},
		"large":  {3, 15, 30, 30, 120, 15},
	}
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want, ok := wantCounts[name]
			if !ok {
				t.Fatalf("profile octavia/%s.yaml has no expected counts in this test", name)
			}
			s := readProfile(t, name)
			if s.Name != name {
				t.Errorf("profile octavia/%s.yaml has name %q, want %q", name, s.Name, name)
			}
			if err := s.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			p, err := s.Generate()
			if err != nil {
				t.Fatalf("Generate() = %v, want nil", err)
			}
			got := [6]int{len(p.Networks), len(p.LoadBalancers), len(p.Listeners), len(p.Pools), len(p.Members), len(p.HealthMonitors)}
			if got != want {
				t.Errorf("profile octavia/%s.yaml networks/load balancers/listeners/pools/members/monitors = %v, want %v", name, got, want)
			}
		})
	}
}

// TestProfilesShipDefaults asserts every shipped Octavia profile leaves the
// provider to the cloud's default and draws listener and member ports from the
// unprivileged range 1024 to 65535 with seed 42.
func TestProfilesShipDefaults(t *testing.T) {
	ports := Range{Min: 1024, Max: 65535}
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			s := readProfile(t, name)
			if s.Provider != "" {
				t.Errorf("profile octavia/%s.yaml provider = %q, want empty (the cloud's default)", name, s.Provider)
			}
			if s.Seed != 42 {
				t.Errorf("profile octavia/%s.yaml seed = %d, want 42", name, s.Seed)
			}
			if s.Distribution.ListenerPort != ports {
				t.Errorf("profile octavia/%s.yaml listener_port = %+v, want %+v", name, s.Distribution.ListenerPort, ports)
			}
			if s.Distribution.MemberPort != ports {
				t.Errorf("profile octavia/%s.yaml member_port = %+v, want %+v", name, s.Distribution.MemberPort, ports)
			}
		})
	}
}

// TestSmallProfileMatchesFixture asserts the shipped small profile equals the
// smallScenario fixture, tying the YAML to the fixture the other tests build on.
func TestSmallProfileMatchesFixture(t *testing.T) {
	if got, want := readProfile(t, "small"), smallScenario(); got != want {
		t.Errorf("scenarios/octavia/small.yaml = %+v, want %+v", got, want)
	}
}

// TestSmallProfileMatchesGolden asserts the shipped small profile, generated
// twice, encodes byte-identically and equal to the golden plan, which is what
// "dizzy octavia generate --scenario scenarios/octavia/small.yaml" prints.
func TestSmallProfileMatchesGolden(t *testing.T) {
	s := readProfile(t, "small")
	first, second := marshal(t, generate(t, s)), marshal(t, generate(t, s))
	if !bytes.Equal(first, second) {
		t.Fatalf("two generations of scenarios/octavia/small.yaml differ:\n%s\n---\n%s", first, second)
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden file (run with -update to create it): %v", err)
	}
	if !bytes.Equal(first, want) {
		t.Errorf("scenarios/octavia/small.yaml generates a plan that differs from golden file %s", goldenPath)
	}
}

// TestProfilesGrowStrictly asserts each of the five resource counts, and so
// the pool count, grows strictly from small to medium to large, so no two
// profiles collapse into the same scale.
func TestProfilesGrowStrictly(t *testing.T) {
	plans := make([]*plan.Plan, 0, len(profileNames))
	for _, name := range profileNames {
		p, err := readProfile(t, name).Generate()
		if err != nil {
			t.Fatalf("Generate(%s): %v", name, err)
		}
		plans = append(plans, p)
	}

	for _, c := range []struct {
		resource string
		count    func(*plan.Plan) int
	}{
		{"networks", func(p *plan.Plan) int { return len(p.Networks) }},
		{"load balancers", func(p *plan.Plan) int { return len(p.LoadBalancers) }},
		{"listeners", func(p *plan.Plan) int { return len(p.Listeners) }},
		{"pools", func(p *plan.Plan) int { return len(p.Pools) }},
		{"members", func(p *plan.Plan) int { return len(p.Members) }},
		{"health monitors", func(p *plan.Plan) int { return len(p.HealthMonitors) }},
	} {
		for i := 1; i < len(plans); i++ {
			if got, prev := c.count(plans[i]), c.count(plans[i-1]); got <= prev {
				t.Errorf("%s %s (%d) must exceed %s %s (%d)", profileNames[i], c.resource, got, profileNames[i-1], c.resource, prev)
			}
		}
	}
}
