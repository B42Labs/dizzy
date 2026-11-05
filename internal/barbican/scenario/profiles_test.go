package scenario

import (
	"encoding/base64"
	"testing"

	"github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/scenarios"
)

// profileNames are the built-in Barbican scenario profiles shipped under
// scenarios/barbican/, smallest first.
var profileNames = []string{"small", "medium", "large"}

// readProfile reads and parses a shipped Barbican profile by name from the
// embedded scenarios filesystem, so the test does not depend on the process
// working directory.
func readProfile(t *testing.T, name string) Scenario {
	t.Helper()
	data, err := scenarios.Files.ReadFile("barbican/" + name + ".yaml")
	if err != nil {
		t.Fatalf("reading profile barbican/%s.yaml: %v", name, err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing profile barbican/%s.yaml: %v", name, err)
	}
	return s
}

// TestProfilesGenerateValidPlans locks the core acceptance criterion: every
// shipped Barbican profile parses, names itself after its file, validates, and
// expands into a valid plan.
func TestProfilesGenerateValidPlans(t *testing.T) {
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := readProfile(t, name)
			if s.Name != name {
				t.Errorf("profile barbican/%s.yaml has name %q, want %q", name, s.Name, name)
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
// TestGenerateGolden. Scenario holds no pointer, so == compares every field.
func TestSmallProfileMatchesGoldenFixture(t *testing.T) {
	if got, want := readProfile(t, "small"), smallScenario(); got != want {
		t.Errorf("scenarios/barbican/small.yaml = %+v, want %+v", got, want)
	}
}

// TestProfilesGrowStrictly asserts each resource count grows strictly from
// small to medium to large, so no two profiles collapse into the same scale.
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
		{"secrets", func(p *plan.Plan) int { return len(p.Secrets) }},
		{"orders", func(p *plan.Plan) int { return len(p.Orders) }},
		{"containers", func(p *plan.Plan) int { return len(p.Containers) }},
	} {
		for i := 1; i < len(plans); i++ {
			if got, prev := c.count(plans[i]), c.count(plans[i-1]); got <= prev {
				t.Errorf("%s %s (%d) must exceed %s %s (%d)", profileNames[i], c.resource, got, profileNames[i-1], c.resource, prev)
			}
		}
	}
}

// defaultMaxSecretChars is Barbican's default max_allowed_secret_in_bytes,
// which it checks against the base64 text of a secret's payload.
const defaultMaxSecretChars = 20000

// maxRawPayloadBytes is the largest raw payload whose base64 encoding fits
// defaultMaxSecretChars.
const maxRawPayloadBytes = 15000

// TestProfilesFitDefaultSecretSize asserts every shipped profile keeps
// payload_bytes.max at or below maxRawPayloadBytes, so no stored secret trips
// Barbican's default size cap with an HTTP 413. It first pins the bound itself
// against the base64 encoding.
func TestProfilesFitDefaultSecretSize(t *testing.T) {
	if got := base64.StdEncoding.EncodedLen(maxRawPayloadBytes); got > defaultMaxSecretChars {
		t.Fatalf("%d raw bytes encode to %d base64 characters, over %d", maxRawPayloadBytes, got, defaultMaxSecretChars)
	}
	if got := base64.StdEncoding.EncodedLen(maxRawPayloadBytes + 1); got <= defaultMaxSecretChars {
		t.Fatalf("%d raw bytes encode to %d base64 characters, within %d; %d is not the largest fitting size", maxRawPayloadBytes+1, got, defaultMaxSecretChars, maxRawPayloadBytes)
	}

	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			if got := readProfile(t, name).Distribution.PayloadBytes.Max; got > maxRawPayloadBytes {
				t.Errorf("profile barbican/%s.yaml payload_bytes.max = %d, want <= %d (Barbican's default cap of %d base64 characters)", name, got, maxRawPayloadBytes, defaultMaxSecretChars)
			}
		})
	}
}
