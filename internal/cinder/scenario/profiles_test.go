package scenario

import (
	"testing"

	"github.com/B42Labs/openstack-tester/scenarios"
)

// profileNames are the built-in Cinder scenario profiles shipped under
// scenarios/cinder/.
var profileNames = []string{"small", "medium", "large"}

// readProfile reads and parses a shipped Cinder profile by name from the
// embedded scenarios filesystem, so the test does not depend on the process
// working directory.
func readProfile(t *testing.T, name string) Scenario {
	t.Helper()
	data, err := scenarios.Files.ReadFile("cinder/" + name + ".yaml")
	if err != nil {
		t.Fatalf("reading profile cinder/%s.yaml: %v", name, err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing profile cinder/%s.yaml: %v", name, err)
	}
	return s
}

// TestProfilesGenerateValidPlans locks the core acceptance criterion: every
// shipped Cinder profile parses, names itself after its file, validates, and
// expands into a valid plan.
func TestProfilesGenerateValidPlans(t *testing.T) {
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := readProfile(t, name)
			if s.Name != name {
				t.Errorf("profile cinder/%s.yaml has name %q, want %q", name, s.Name, name)
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
// TestGenerateGolden.
func TestSmallProfileMatchesGoldenFixture(t *testing.T) {
	if got, want := readProfile(t, "small"), smallScenario(); got != want {
		t.Errorf("scenarios/cinder/small.yaml = %+v, want %+v", got, want)
	}
}

// TestSmallProfileFitsDefaultQuotas asserts the small profile expands within
// Cinder's common default quotas of 10 volumes / 10 snapshots / 1000 GiB, so it
// runs against a fresh project without raising anything.
func TestSmallProfileFitsDefaultQuotas(t *testing.T) {
	p, err := readProfile(t, "small").Generate()
	if err != nil {
		t.Fatalf("Generate(small): %v", err)
	}
	if got := len(p.Volumes); got > 10 {
		t.Errorf("small volumes = %d, want <= 10 (default quota)", got)
	}
	if got := len(p.Snapshots); got > 10 {
		t.Errorf("small snapshots = %d, want <= 10 (default quota)", got)
	}
	if got := p.TotalGiB(); got > 1000 {
		t.Errorf("small total GiB = %d, want <= 1000 (default quota)", got)
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
	if len(large.Volumes) <= len(medium.Volumes) {
		t.Errorf("large volumes (%d) must exceed medium volumes (%d)", len(large.Volumes), len(medium.Volumes))
	}
}
