package scenario

import (
	"reflect"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/scenarios"
)

// profileNames are the built-in Glance scenario profiles shipped under
// scenarios/glance/.
var profileNames = []string{"small", "medium", "large"}

// readProfile reads and parses a shipped Glance profile by name from the
// embedded scenarios filesystem, so the test does not depend on the process
// working directory.
func readProfile(t *testing.T, name string) Scenario {
	t.Helper()
	data, err := scenarios.Files.ReadFile("glance/" + name + ".yaml")
	if err != nil {
		t.Fatalf("reading profile glance/%s.yaml: %v", name, err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing profile glance/%s.yaml: %v", name, err)
	}
	return s
}

// TestProfilesGenerateValidPlans locks the core acceptance criterion: every
// shipped Glance profile parses, names itself after its file, validates, and
// expands into a valid plan.
func TestProfilesGenerateValidPlans(t *testing.T) {
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := readProfile(t, name)
			if s.Name != name {
				t.Errorf("profile glance/%s.yaml has name %q, want %q", name, s.Name, name)
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

// TestSmallProfileMatchesFixture asserts the shipped small profile equals the
// smallScenario fixture, tying the YAML to the fixture the other tests build on.
// It uses reflect.DeepEqual rather than == because Scenario carries a *Chaos
// pointer, which == would compare by identity.
func TestSmallProfileMatchesFixture(t *testing.T) {
	if got, want := readProfile(t, "small"), smallScenario(); !reflect.DeepEqual(got, want) {
		t.Errorf("scenarios/glance/small.yaml = %+v, want %+v", got, want)
	}
}

// TestProfilesShipChaosBlock asserts every shipped Glance profile carries a
// chaos block with a positive duration, so `glance chaos --scenario
// scenarios/glance/<profile>.yaml` runs without a --duration flag.
func TestProfilesShipChaosBlock(t *testing.T) {
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			s := readProfile(t, name)
			if s.Chaos == nil {
				t.Fatalf("profile glance/%s.yaml has no chaos block", name)
			}
			if s.Chaos.Duration <= 0 {
				t.Errorf("profile glance/%s.yaml chaos.duration = %s, want positive", name, time.Duration(s.Chaos.Duration))
			}
		})
	}
}

// TestProfilesShipZeroPublicRatio locks the admin-only publicize_image default
// off: every shipped profile keeps public_ratio at 0, so a run against a
// non-admin cloud never schedules a visibility flip the cloud would reject.
func TestProfilesShipZeroPublicRatio(t *testing.T) {
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			if got := readProfile(t, name).Distribution.PublicRatio; got != 0 {
				t.Errorf("profile glance/%s.yaml public_ratio = %v, want 0 (publicize_image is admin-only)", name, got)
			}
		})
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
	if len(large.Images) <= len(medium.Images) {
		t.Errorf("large images (%d) must exceed medium images (%d)", len(large.Images), len(medium.Images))
	}
}
