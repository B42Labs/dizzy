package scenario

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/B42Labs/dizzy/internal/horizon/plan"
)

var update = flag.Bool("update", false, "update golden files")

// marshal renders a plan exactly as the generate command does: indented JSON
// with a trailing newline.
func marshal(t *testing.T, p *plan.Plan) []byte {
	t.Helper()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("marshalling plan: %v", err)
	}
	return append(data, '\n')
}

// generate expands s, failing the test on an error.
func generate(t *testing.T, s Scenario) *plan.Plan {
	t.Helper()
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}
	return p
}

func TestGenerateInvalidScenario(t *testing.T) {
	s := smallScenario()
	s.Name = ""
	_, err := s.Generate()
	if want := "invalid scenario: name must not be empty"; err == nil || err.Error() != want {
		t.Fatalf("Generate() = %v, want %q", err, want)
	}
}

func TestGenerateDeterministic(t *testing.T) {
	s := smallScenario()
	if first, second := marshal(t, generate(t, s)), marshal(t, generate(t, s)); !bytes.Equal(first, second) {
		t.Errorf("two generations of the same scenario+seed differ:\n%s\n---\n%s", first, second)
	}
}

func TestGenerateSeedChangesPlan(t *testing.T) {
	s7 := smallScenario()
	s7.Seed = 7
	if bytes.Equal(marshal(t, generate(t, smallScenario())), marshal(t, generate(t, s7))) {
		t.Error("seeds 42 and 7 produced identical plans")
	}
}

func TestGenerateGolden(t *testing.T) {
	got := marshal(t, generate(t, smallScenario()))

	path := filepath.Join("testdata", "golden", "small.plan.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("generated plan differs from golden file %s; run with -update if the change is intended", path)
	}
}

// TestGenerateWithoutUsersEncodesEmptyArrays asserts a plan of the
// clouds.yaml user has no role, and its projects and users read [].
func TestGenerateWithoutUsersEncodesEmptyArrays(t *testing.T) {
	p := generate(t, smallScenario())
	if p.Role != "" {
		t.Errorf("role = %q, want empty without users", p.Role)
	}
	got := string(marshal(t, p))
	for _, want := range []string{`"projects": []`, `"users": []`} {
		if !strings.Contains(got, want) {
			t.Errorf("plan JSON lacks %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"role"`) || strings.Contains(got, `"user"`) {
		t.Errorf("plan JSON names a role or a session user without users:\n%s", got)
	}
}

// TestGenerateZeroPagesPerSession asserts pages_per_session {0, 0} gives
// every session an empty, non-nil page list.
func TestGenerateZeroPagesPerSession(t *testing.T) {
	s := smallScenario()
	s.Distribution.PagesPerSession = Range{Min: 0, Max: 0}
	p := generate(t, s)
	for _, sess := range p.Sessions {
		if sess.Pages == nil || len(sess.Pages) != 0 {
			t.Errorf("session %s pages = %#v, want an empty, non-nil list", sess.Name, sess.Pages)
		}
	}
	if !strings.Contains(string(marshal(t, p)), `"pages": []`) {
		t.Error(`plan JSON lacks a session with "pages": []`)
	}
}

// TestGenerateUsersInTurn asserts resources.users 2 creates two users and
// the scenario's projects and assigns the users to the sessions in turn.
func TestGenerateUsersInTurn(t *testing.T) {
	s := smallScenario()
	s.Resources.Users = 2
	p := generate(t, s)
	if p.Role != "member" {
		t.Errorf("role = %q, want member", p.Role)
	}
	if len(p.Projects) != 2 || p.Projects[0].Name != "project-0001" || p.Projects[1].Name != "project-0002" {
		t.Errorf("projects = %+v, want project-0001 and project-0002", p.Projects)
	}
	want := []string{"user-0001", "user-0002", "user-0001", "user-0002", "user-0001"}
	for i, sess := range p.Sessions {
		if sess.User != want[i] {
			t.Errorf("session %s user = %q, want %q", sess.Name, sess.User, want[i])
		}
	}
}

// TestGenerateUsersDoNotChangeDraws asserts the identity does not move any
// draw: the pages, switches and switch draws of a plan with users equal
// those without.
func TestGenerateUsersDoNotChangeDraws(t *testing.T) {
	without := generate(t, smallScenario())
	s := smallScenario()
	s.Resources.Users = 2
	with := generate(t, s)
	for i := range without.Sessions {
		a, b := without.Sessions[i], with.Sessions[i]
		b.User = ""
		if !equalSession(a, b) {
			t.Errorf("session %d differs: %+v vs %+v", i, a, b)
		}
	}
}

// TestGeneratePagesKeepScenarioOrder asserts a session's pages follow the
// order of the scenario's pages list.
func TestGeneratePagesKeepScenarioOrder(t *testing.T) {
	s := smallScenario()
	s.Resources.Sessions = 50
	order := map[string]int{"api_access": 0, "identity": 1}
	for _, sess := range generate(t, s).Sessions {
		for i := 1; i < len(sess.Pages); i++ {
			if order[sess.Pages[i-1]] >= order[sess.Pages[i]] {
				t.Fatalf("session %s pages %v are out of scenario order", sess.Name, sess.Pages)
			}
		}
	}
}

func equalSession(a, b plan.Session) bool {
	if a.Name != b.Name || a.User != b.User || a.Switch != b.Switch || a.SwitchDraw != b.SwitchDraw || len(a.Pages) != len(b.Pages) {
		return false
	}
	for i := range a.Pages {
		if a.Pages[i] != b.Pages[i] {
			return false
		}
	}
	return true
}
