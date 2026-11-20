package scenario

import (
	"reflect"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/scenarios"
)

// profileNames are the built-in Horizon scenario profiles shipped under
// scenarios/horizon/, smallest first.
var profileNames = []string{"small", "medium", "large"}

// readProfile reads and parses a shipped Horizon profile by name from the
// embedded scenarios filesystem.
func readProfile(t *testing.T, name string) Scenario {
	t.Helper()
	data, err := scenarios.Files.ReadFile("horizon/" + name + ".yaml")
	if err != nil {
		t.Fatalf("reading profile horizon/%s.yaml: %v", name, err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing profile horizon/%s.yaml: %v", name, err)
	}
	return s
}

// TestProfilesGenerateValidPlans asserts every shipped profile parses, names
// itself after its file, validates and generates.
func TestProfilesGenerateValidPlans(t *testing.T) {
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			s := readProfile(t, name)
			if s.Name != name {
				t.Errorf("profile horizon/%s.yaml has name %q, want %q", name, s.Name, name)
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

// TestSmallProfileMatchesGoldenFixture ties the shipped small profile to the
// fixture the golden plan is generated from.
func TestSmallProfileMatchesGoldenFixture(t *testing.T) {
	if got, want := readProfile(t, "small"), smallScenario(); !reflect.DeepEqual(got, want) {
		t.Errorf("scenarios/horizon/small.yaml = %+v, want %+v", got, want)
	}
}

// TestProfilesDifferOnlyInSize asserts the three profiles log in as the
// clouds.yaml user and differ only in the session count and the chaos
// duration, interval and fan-out.
func TestProfilesDifferOnlyInSize(t *testing.T) {
	want := map[string]struct {
		sessions int
		duration time.Duration
		min, max time.Duration
		parallel int
	}{
		"small":  {5, 5 * time.Minute, time.Second, 5 * time.Second, 2},
		"medium": {25, 30 * time.Minute, 500 * time.Millisecond, 3 * time.Second, 4},
		"large":  {100, time.Hour, 200 * time.Millisecond, 2 * time.Second, 8},
	}
	small := readProfile(t, "small")
	for _, name := range profileNames {
		t.Run(name, func(t *testing.T) {
			s := readProfile(t, name)
			w := want[name]
			if s.Resources.Sessions != w.sessions || s.Resources.Users != 0 || s.Resources.Projects != 2 {
				t.Errorf("resources = %+v, want %d sessions, 0 users, 2 projects", s.Resources, w.sessions)
			}
			c := s.Chaos
			if c == nil {
				t.Fatal("profile has no chaos block")
			}
			if time.Duration(c.Duration) != w.duration || time.Duration(c.Interval.Min) != w.min ||
				time.Duration(c.Interval.Max) != w.max || c.Parallel.Max != w.parallel {
				t.Errorf("chaos = %+v, want duration %s, interval %s to %s, parallel %d", c, w.duration, w.min, w.max, w.parallel)
			}

			// Everything else equals small.
			s.Name, s.Resources.Sessions = small.Name, small.Resources.Sessions
			sc := *c
			sc.Duration, sc.Interval, sc.Parallel = small.Chaos.Duration, small.Chaos.Interval, small.Chaos.Parallel
			s.Chaos = &sc
			if !reflect.DeepEqual(s, small) {
				t.Errorf("profile differs from small beyond its size: %+v", s)
			}
		})
	}
}
