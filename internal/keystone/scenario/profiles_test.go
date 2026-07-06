package scenario

import (
	"reflect"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/scenarios"
)

// profileNames are the built-in Keystone scenario profiles shipped under
// scenarios/keystone/.
var profileNames = []string{"small", "medium", "large"}

// readProfile reads and parses a shipped Keystone profile by name from the
// embedded scenarios filesystem, so the test does not depend on the process
// working directory.
func readProfile(t *testing.T, name string) Scenario {
	t.Helper()
	data, err := scenarios.Files.ReadFile("keystone/" + name + ".yaml")
	if err != nil {
		t.Fatalf("reading profile keystone/%s.yaml: %v", name, err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing profile keystone/%s.yaml: %v", name, err)
	}
	return s
}

// TestProfilesGenerateValidPlans locks the core acceptance criterion: every
// shipped Keystone profile parses, names itself after its file, validates, and
// expands into a valid plan.
func TestProfilesGenerateValidPlans(t *testing.T) {
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := readProfile(t, name)
			if s.Name != name {
				t.Errorf("profile keystone/%s.yaml has name %q, want %q", name, s.Name, name)
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

// TestSmallProfileMatchesGoldenFixture asserts the shipped small profile equals
// the smallScenario fixture, tying it transitively to the golden plan locked by
// TestGenerateGolden. It uses reflect.DeepEqual rather than == because Scenario
// carries a *Chaos pointer, which == would compare by identity.
func TestSmallProfileMatchesGoldenFixture(t *testing.T) {
	if got, want := readProfile(t, "small"), smallScenario(); !reflect.DeepEqual(got, want) {
		t.Errorf("scenarios/keystone/small.yaml = %+v, want %+v", got, want)
	}
}

// TestProfilesShipChaosBlock asserts every shipped Keystone profile carries a
// chaos block with a positive duration and a token_ratio, so `keystone chaos
// --scenario scenarios/keystone/<profile>.yaml` runs without extra flags.
func TestProfilesShipChaosBlock(t *testing.T) {
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			s := readProfile(t, name)
			if s.Chaos == nil {
				t.Fatalf("profile keystone/%s.yaml has no chaos block", name)
			}
			if s.Chaos.Duration <= 0 {
				t.Errorf("profile keystone/%s.yaml chaos.duration = %s, want positive", name, time.Duration(s.Chaos.Duration))
			}
			if s.Chaos.TokenRatio == nil {
				t.Errorf("profile keystone/%s.yaml chaos block has no token_ratio", name)
			}
		})
	}
}

// TestSmallProfileSingleDomain asserts the small profile keeps a single domain,
// so it stays runnable in domain-manager mode (which requires domains <= 1).
func TestSmallProfileSingleDomain(t *testing.T) {
	if got := readProfile(t, "small").Resources.Domains; got != 1 {
		t.Errorf("small profile domains = %d, want 1 so it runs in domain-manager mode", got)
	}
}

// TestLargeProfileExceedsMedium asserts the large profile stays strictly larger
// than medium, so the two profiles never collapse into the same scale.
func TestLargeProfileExceedsMedium(t *testing.T) {
	large, err := readProfile(t, "large").Generate()
	if err != nil {
		t.Fatalf("Generate(large): %v", err)
	}
	medium, err := readProfile(t, "medium").Generate()
	if err != nil {
		t.Fatalf("Generate(medium): %v", err)
	}
	if len(large.Users) <= len(medium.Users) {
		t.Errorf("large users (%d) must exceed medium users (%d)", len(large.Users), len(medium.Users))
	}
	if len(large.Projects) <= len(medium.Projects) {
		t.Errorf("large projects (%d) must exceed medium projects (%d)", len(large.Projects), len(medium.Projects))
	}
}
