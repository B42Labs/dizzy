package scenario

import (
	"encoding/base64"
	"reflect"
	"testing"
	"time"

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
// TestGenerateGolden. It uses reflect.DeepEqual rather than == because Scenario
// carries a *Chaos pointer, which == would compare by identity.
func TestSmallProfileMatchesGoldenFixture(t *testing.T) {
	if got, want := readProfile(t, "small"), smallScenario(); !reflect.DeepEqual(got, want) {
		t.Errorf("scenarios/barbican/small.yaml = %+v, want %+v", got, want)
	}
}

// TestProfilesShipChaosBlock asserts every shipped Barbican profile carries a
// chaos block, so `barbican chaos --scenario scenarios/barbican/<profile>.yaml`
// runs without extra flags: the duration grows from small to large, while the
// interval, fan-out, churn ratio, target fill, and retrieve ratio are the same
// in all three.
func TestProfilesShipChaosBlock(t *testing.T) {
	wantDuration := map[string]time.Duration{
		"small":  5 * time.Minute,
		"medium": 30 * time.Minute,
		"large":  time.Hour,
	}
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			want, ok := wantDuration[name]
			if !ok {
				t.Fatalf("profile barbican/%s.yaml has no expected chaos duration in this test", name)
			}
			c := readProfile(t, name).Chaos
			if c == nil {
				t.Fatalf("profile barbican/%s.yaml has no chaos block", name)
			}
			if got := time.Duration(c.Duration); got != want {
				t.Errorf("profile barbican/%s.yaml chaos.duration = %s, want %s", name, got, want)
			}
			if got, want := c.Interval, (Interval{Min: Duration(200 * time.Millisecond), Max: Duration(3 * time.Second)}); got != want {
				t.Errorf("profile barbican/%s.yaml chaos.interval = %s-%s, want %s-%s", name, time.Duration(got.Min), time.Duration(got.Max), time.Duration(want.Min), time.Duration(want.Max))
			}
			if c.Parallel.Max != 4 {
				t.Errorf("profile barbican/%s.yaml chaos.parallel.max = %d, want 4", name, c.Parallel.Max)
			}
			if c.ChurnRatio != 0.5 {
				t.Errorf("profile barbican/%s.yaml chaos.churn_ratio = %v, want 0.5", name, c.ChurnRatio)
			}
			if c.TargetFill != 0.8 {
				t.Errorf("profile barbican/%s.yaml chaos.target_fill = %v, want 0.8", name, c.TargetFill)
			}
			if c.RetrieveRatio == nil || *c.RetrieveRatio != 0.3 {
				t.Errorf("profile barbican/%s.yaml chaos.retrieve_ratio = %s, want &0.3", name, describeRatio(c.RetrieveRatio))
			}
		})
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
