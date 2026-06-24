package scenario

import (
	"testing"
	"time"

	"github.com/B42Labs/openstack-tester/scenarios"
)

// profileNames are the built-in scenario profiles shipped under scenarios/.
var profileNames = []string{"small", "medium", "large"}

// readProfile reads and parses a shipped profile by name from the embedded
// scenarios filesystem, so the test does not depend on the process working
// directory.
func readProfile(t *testing.T, name string) Scenario {
	t.Helper()
	data, err := scenarios.Files.ReadFile(name + ".yaml")
	if err != nil {
		t.Fatalf("reading profile %s.yaml: %v", name, err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing profile %s.yaml: %v", name, err)
	}
	return s
}

// TestProfilesGenerateValidPlans locks the core acceptance criterion: every
// shipped profile parses, names itself after its file, validates, and expands
// into a valid plan.
func TestProfilesGenerateValidPlans(t *testing.T) {
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := readProfile(t, name)
			if s.Name != name {
				t.Errorf("profile %s.yaml has name %q, want %q", name, s.Name, name)
			}
			if err := s.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if _, err := s.Generate(); err != nil {
				t.Fatalf("Generate() = %v, want nil", err)
			}
		})
	}
}

// TestProfilesShipRunnableChaosBlock locks the requirement that every shipped
// profile carries a chaos block with a positive duration, so `neutron chaos
// --scenario scenarios/<profile>.yaml` runs straight away without --duration or
// any other flag.
func TestProfilesShipRunnableChaosBlock(t *testing.T) {
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := readProfile(t, name)
			if s.Chaos == nil {
				t.Fatalf("profile %s.yaml has no chaos block; the chaos subcommand needs one to run without --duration", name)
			}
			if time.Duration(s.Chaos.Duration) <= 0 {
				t.Errorf("profile %s.yaml chaos.duration = %s, want positive so the chaos run has a hard stop",
					name, time.Duration(s.Chaos.Duration))
			}
			if err := s.Validate(); err != nil {
				t.Fatalf("profile %s.yaml with chaos block: Validate() = %v, want nil", name, err)
			}
		})
	}
}

// TestMediumProfileMatchesReadmeExample asserts the shipped medium profile is
// the README §6 example, by comparing its spatial envelope to the in-package
// fixture that the README-example parse test also pins. The chaos block (which
// the fixture omits) is checked separately so `neutron chaos` runs the profile
// without flags.
func TestMediumProfileMatchesReadmeExample(t *testing.T) {
	got := readProfile(t, "medium")
	assertChaos(t, "medium", got.Chaos, Chaos{
		Duration:   Duration(30 * time.Minute),
		Interval:   Interval{Min: Duration(200 * time.Millisecond), Max: Duration(2 * time.Second)},
		Parallel:   Parallel{Max: 6},
		ChurnRatio: 0.5,
		TargetFill: 0.7,
	})
	got.Chaos = nil
	if want := mediumScenario(); got != want {
		t.Errorf("scenarios/medium.yaml = %+v, want %+v", got, want)
	}
}

// TestSmallProfileMatchesGoldenFixture asserts the shipped small profile's
// spatial envelope equals the smallScenario fixture, tying it transitively to
// the golden plan locked by TestGenerateGolden. The chaos block (which the
// fixture omits) is checked separately so `neutron chaos` runs the profile
// without flags.
func TestSmallProfileMatchesGoldenFixture(t *testing.T) {
	got := readProfile(t, "small")
	assertChaos(t, "small", got.Chaos, Chaos{
		Duration:   Duration(5 * time.Minute),
		Interval:   Interval{Min: Duration(200 * time.Millisecond), Max: Duration(3 * time.Second)},
		Parallel:   Parallel{Max: 4},
		ChurnRatio: 0.5,
		TargetFill: 0.8,
	})
	got.Chaos = nil
	if want := smallScenario(); got != want {
		t.Errorf("scenarios/small.yaml = %+v, want %+v", got, want)
	}
}

// assertChaos checks a shipped profile carries the expected chaos block. Every
// profile must ship one so the chaos subcommand runs it with no flags (the
// duration is otherwise required); the values are pinned so an accidental edit
// is caught. Chaos has no pointer fields, so == is a full value comparison.
func assertChaos(t *testing.T, name string, got *Chaos, want Chaos) {
	t.Helper()
	if got == nil {
		t.Fatalf("scenarios/%s.yaml has no chaos block; the chaos subcommand needs one to run without --duration", name)
	}
	if *got != want {
		t.Errorf("scenarios/%s.yaml chaos = %+v, want %+v", name, *got, want)
	}
}

// TestLargeProfileHitsHeadlineScale asserts the large profile expands to at
// least the headline 20-routers / 100-networks / 200-subnets target and stays
// strictly larger than medium, so the two profiles never collapse into the same
// scale.
func TestLargeProfileHitsHeadlineScale(t *testing.T) {
	large, err := readProfile(t, "large").Generate()
	if err != nil {
		t.Fatalf("Generate(large): %v", err)
	}

	if got := len(large.Routers); got < 20 {
		t.Errorf("large routers = %d, want >= 20", got)
	}
	if got := len(large.Networks); got < 100 {
		t.Errorf("large networks = %d, want >= 100", got)
	}
	if got := len(large.Subnets); got < 200 {
		t.Errorf("large subnets = %d, want >= 200", got)
	}

	medium, err := readProfile(t, "medium").Generate()
	if err != nil {
		t.Fatalf("Generate(medium): %v", err)
	}
	if len(large.Networks) <= len(medium.Networks) {
		t.Errorf("large networks (%d) must exceed medium networks (%d)",
			len(large.Networks), len(medium.Networks))
	}
}
