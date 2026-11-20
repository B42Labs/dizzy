package scenario

import (
	"math"
	"strings"
	"testing"
	"time"
)

// smallScenario is the fixture scenarios/horizon/small.yaml must equal; the
// golden plan is generated from it.
func smallScenario() Scenario {
	browse := 0.3
	return Scenario{
		Name:      "small",
		Seed:      42,
		Resources: Resources{Sessions: 5, Users: 0, Projects: 2},
		Role:      "member",
		Pages: []Page{
			{Name: "api_access", Path: "/project/api_access/"},
			{Name: "identity", Path: "/identity/"},
		},
		Distribution: Distribution{
			PagesPerSession: Range{Min: 1, Max: 2},
			SwitchRatio:     1.0,
		},
		Chaos: &Chaos{
			Duration:    Duration(5 * time.Minute),
			Interval:    Interval{Min: Duration(time.Second), Max: Duration(5 * time.Second)},
			Parallel:    Parallel{Max: 2},
			ChurnRatio:  0.5,
			TargetFill:  0.8,
			BrowseRatio: &browse,
		},
	}
}

// TestParseRejectsUnknownKey asserts a typo in a scenario fails the parse.
func TestParseRejectsUnknownKey(t *testing.T) {
	_, err := Parse([]byte("name: x\nresources:\n  sesions: 3\n"))
	if err == nil || !strings.Contains(err.Error(), "sesions") {
		t.Fatalf("Parse() = %v, want an error naming the unknown key", err)
	}
}

// TestEmptyDocumentFailsValidateOnName asserts an empty document parses and
// fails Validate with an error naming name.
func TestEmptyDocumentFailsValidateOnName(t *testing.T) {
	s, err := Parse([]byte(""))
	if err != nil {
		t.Fatalf("Parse(\"\") = %v, want nil", err)
	}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("Validate() = %v, want an error naming name", err)
	}
}

// TestValidateRules asserts one failing case per rule, each error naming its
// key.
func TestValidateRules(t *testing.T) {
	ratio := func(v float64) *float64 { return &v }
	withUsers := func(s *Scenario) { s.Resources.Users = 2 }
	tests := []struct {
		name   string
		mutate func(*Scenario)
		key    string
	}{
		{"empty name", func(s *Scenario) { s.Name = "" }, "name"},
		{"no sessions", func(s *Scenario) { s.Resources.Sessions = 0 }, "resources.sessions"},
		{"negative users", func(s *Scenario) { s.Resources.Users = -1 }, "resources.users"},
		{"negative projects", func(s *Scenario) { s.Resources.Projects = -1 }, "resources.projects"},
		{"users above sessions", func(s *Scenario) { s.Resources.Users = 6 }, "resources.users"},
		{"users without projects", func(s *Scenario) { withUsers(s); s.Resources.Projects = 0 }, "resources.projects"},
		{"users without role", func(s *Scenario) { withUsers(s); s.Role = "" }, "role"},
		{"no pages", func(s *Scenario) { s.Pages = nil }, "pages"},
		{"page name with upper case", func(s *Scenario) { s.Pages[0].Name = "API" }, "pages[0].name"},
		{"page name too long", func(s *Scenario) { s.Pages[0].Name = strings.Repeat("a", 33) }, "pages[0].name"},
		{"page name repeats", func(s *Scenario) { s.Pages[1].Name = s.Pages[0].Name }, "pages[1].name"},
		{"path without slash", func(s *Scenario) { s.Pages[0].Path = "project/" }, "pages[0].path"},
		{"path with two slashes", func(s *Scenario) { s.Pages[0].Path = "//evil.example/" }, "pages[0].path"},
		{"path under auth", func(s *Scenario) { s.Pages[0].Path = "/auth/logout/" }, "pages[0].path"},
		{"path with query", func(s *Scenario) { s.Pages[0].Path = "/project/?a=b" }, "pages[0].path"},
		{"path with fragment", func(s *Scenario) { s.Pages[0].Path = "/project/#x" }, "pages[0].path"},
		{"path with whitespace", func(s *Scenario) { s.Pages[0].Path = "/pro ject/" }, "pages[0].path"},
		{"pages min negative", func(s *Scenario) { s.Distribution.PagesPerSession.Min = -1 }, "distribution.pages_per_session.min"},
		{"pages min above max", func(s *Scenario) { s.Distribution.PagesPerSession = Range{Min: 2, Max: 1} }, "distribution.pages_per_session.min"},
		{"pages max above page count", func(s *Scenario) { s.Distribution.PagesPerSession.Max = 3 }, "distribution.pages_per_session.max"},
		{"switch ratio above 1", func(s *Scenario) { s.Distribution.SwitchRatio = 1.5 }, "distribution.switch_ratio"},
		{"switch ratio negative", func(s *Scenario) { s.Distribution.SwitchRatio = -0.1 }, "distribution.switch_ratio"},
		{"switch ratio NaN", func(s *Scenario) { s.Distribution.SwitchRatio = math.NaN() }, "distribution.switch_ratio"},
		{"chaos negative duration", func(s *Scenario) { s.Chaos.Duration = Duration(-time.Second) }, "chaos.duration"},
		{"chaos negative bucket width", func(s *Scenario) { s.Chaos.BucketWidth = Duration(-time.Second) }, "chaos.bucket_width"},
		{"chaos interval min above max", func(s *Scenario) { s.Chaos.Interval.Min = Duration(10 * time.Second) }, "chaos.interval.min"},
		{"chaos negative parallel", func(s *Scenario) { s.Chaos.Parallel.Max = -1 }, "chaos.parallel.max"},
		{"chaos churn ratio", func(s *Scenario) { s.Chaos.ChurnRatio = 2 }, "chaos.churn_ratio"},
		{"chaos target fill", func(s *Scenario) { s.Chaos.TargetFill = -1 }, "chaos.target_fill"},
		{"chaos browse ratio", func(s *Scenario) { s.Chaos.BrowseRatio = ratio(1.1) }, "chaos.browse_ratio"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			tc.mutate(&s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error naming %s", tc.key)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("Validate() = %q, want it to name %s", err, tc.key)
			}
		})
	}
}

// TestValidateIgnoresProjectsAndRoleWithoutUsers asserts resources.projects
// and role are not checked when the sessions log in as the clouds.yaml user.
func TestValidateIgnoresProjectsAndRoleWithoutUsers(t *testing.T) {
	s := smallScenario()
	s.Resources = Resources{Sessions: 5, Users: 0, Projects: 0}
	s.Role = ""
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// TestValidateAcceptsShippedFixture asserts the fixture itself is valid, so
// every failure above comes from its one mutation.
func TestValidateAcceptsShippedFixture(t *testing.T) {
	if err := smallScenario().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// TestSet asserts every documented key is applied, and an unknown key and a
// non-numeric value for a numeric key are rejected with an error naming
// them.
func TestSet(t *testing.T) {
	s := smallScenario()
	for _, kv := range [][2]string{
		{"seed", "7"},
		{"resources.sessions", "9"},
		{"resources.users", "3"},
		{"resources.projects", "4"},
		{"role", "reader"},
		{"distribution.pages_per_session.min", "0"},
		{"distribution.pages_per_session.max", "1"},
		{"distribution.switch_ratio", "0.25"},
	} {
		if err := s.Set(kv[0], kv[1]); err != nil {
			t.Fatalf("Set(%q, %q) = %v, want nil", kv[0], kv[1], err)
		}
	}
	want := Resources{Sessions: 9, Users: 3, Projects: 4}
	if s.Seed != 7 || s.Resources != want || s.Role != "reader" ||
		s.Distribution.PagesPerSession != (Range{Min: 0, Max: 1}) || s.Distribution.SwitchRatio != 0.25 {
		t.Errorf("after Set: %+v", s)
	}

	if err := s.Set("pages", "x"); err == nil || !strings.Contains(err.Error(), `"pages"`) {
		t.Errorf("Set(pages) = %v, want an unknown-key error naming it", err)
	}
	if err := s.Set("resources.sessions", "many"); err == nil ||
		!strings.Contains(err.Error(), "resources.sessions") || !strings.Contains(err.Error(), `"many"`) {
		t.Errorf("Set(resources.sessions=many) = %v, want an error naming the key and the value", err)
	}
	if err := s.Set("distribution.switch_ratio", "half"); err == nil || !strings.Contains(err.Error(), "distribution.switch_ratio") {
		t.Errorf("Set(distribution.switch_ratio=half) = %v, want an error naming the key", err)
	}
}

// TestParseBrowseRatio asserts an absent browse_ratio stays nil and an
// explicit 0 is kept, so the command can tell "default" from "off".
func TestParseBrowseRatio(t *testing.T) {
	absent, err := Parse([]byte("name: x\nchaos:\n  duration: 1m\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if absent.Chaos == nil || absent.Chaos.BrowseRatio != nil {
		t.Errorf("absent browse_ratio = %v, want nil", absent.Chaos)
	}
	zero, err := Parse([]byte("name: x\nchaos:\n  browse_ratio: 0\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if zero.Chaos.BrowseRatio == nil || *zero.Chaos.BrowseRatio != 0 {
		t.Errorf("browse_ratio: 0 = %v, want a pointer to 0", zero.Chaos.BrowseRatio)
	}
}
