package scenario

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/B42Labs/dizzy/internal/keystone/plan"
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

func TestGenerateInvalidScenario(t *testing.T) {
	s := smallScenario()
	s.Name = "" // fails Scenario.Validate

	if _, err := s.Generate(); err == nil {
		t.Fatal("Generate() = nil error, want error for invalid scenario")
	}
}

func TestGenerateDeterministic(t *testing.T) {
	s := smallScenario()

	p1, err := s.Generate()
	if err != nil {
		t.Fatalf("first Generate(): %v", err)
	}
	p2, err := s.Generate()
	if err != nil {
		t.Fatalf("second Generate(): %v", err)
	}

	if got, want := marshal(t, p1), marshal(t, p2); !bytes.Equal(got, want) {
		t.Error("two generations of the same scenario+seed differ")
	}
}

func TestGenerateSeedChangesPlan(t *testing.T) {
	s1 := smallScenario()
	s2 := smallScenario()
	s2.Seed = s1.Seed + 1

	p1, err := s1.Generate()
	if err != nil {
		t.Fatalf("Generate(seed=%d): %v", s1.Seed, err)
	}
	p2, err := s2.Generate()
	if err != nil {
		t.Fatalf("Generate(seed=%d): %v", s2.Seed, err)
	}

	if bytes.Equal(marshal(t, p1), marshal(t, p2)) {
		t.Error("different seeds produced identical plans")
	}
}

func TestGenerateGolden(t *testing.T) {
	p, err := smallScenario().Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	got := marshal(t, p)

	path := filepath.Join("testdata", "golden", "small.plan.json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating golden dir: %v", err)
		}
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

// TestGenerateInvariants exercises the generator's structural guarantees over a
// larger scenario mixing multiple domains, projects, and users: exact project
// and user counts, projects dealt only into real domains, every grant resolving,
// and every token issue backed by a project-scoped grant.
func TestGenerateInvariants(t *testing.T) {
	s := smallScenario()
	s.Resources.Domains = 3
	s.Resources.Roles = 4
	s.Resources.Projects = 30
	s.Resources.Users = 40

	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}

	if got := len(p.Projects); got != 30 {
		t.Fatalf("projects = %d, want 30 (the deal must place exactly the fixed count)", got)
	}
	if got := len(p.Users); got != 40 {
		t.Fatalf("users = %d, want 40", got)
	}

	domains := make(map[string]bool, len(p.Domains))
	for _, d := range p.Domains {
		domains[d.Name] = true
	}
	projectDomain := make(map[string]string, len(p.Projects))
	for _, pr := range p.Projects {
		if !domains[pr.Domain] {
			t.Errorf("project %q dealt into unknown domain %q", pr.Name, pr.Domain)
		}
		projectDomain[pr.Name] = pr.Domain
	}
	userDomain := make(map[string]string, len(p.Users))
	for _, u := range p.Users {
		userDomain[u.Name] = u.Domain
	}
	// Every project-scoped grant is on a project in the user's own domain.
	for _, a := range p.Assignments {
		if a.Project != "" && projectDomain[a.Project] != userDomain[a.User] {
			t.Errorf("assignment for user %q (domain %q) targets project %q in domain %q",
				a.User, userDomain[a.User], a.Project, projectDomain[a.Project])
		}
	}
	if len(p.Tokens) == 0 {
		t.Fatal("no token issues generated despite users_issuing_tokens_ratio > 0")
	}
	if err := p.Validate(); err != nil {
		t.Errorf("generated plan failed validation: %v", err)
	}
}

// TestGenerateRatioZeroNoTokens confirms users_issuing_tokens_ratio 0 yields no
// token issues — the guard that keeps a ratio-0 plan's RNG sequence undisturbed.
func TestGenerateRatioZeroNoTokens(t *testing.T) {
	s := smallScenario()
	s.Distribution.UsersIssuingTokensRatio = 0

	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	if len(p.Tokens) != 0 {
		t.Errorf("tokens = %d, want 0 for a ratio of 0", len(p.Tokens))
	}
}

// TestGenerateRatioZeroNoDomainScoped confirms domain_scoped_assignment_ratio 0
// produces only project-scoped grants (given every user's domain has projects),
// the guard mirroring the token-ratio one.
func TestGenerateRatioZeroNoDomainScoped(t *testing.T) {
	s := smallScenario()
	s.Distribution.DomainScopedAssignmentRatio = 0

	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	for _, a := range p.Assignments {
		if a.Project == "" {
			t.Errorf("assignment for user %q is domain-scoped despite ratio 0", a.User)
		}
	}
}

// TestGenerateProjectlessDomainFallsBackToDomainScoped confirms that when a
// user's domain has no projects, its grants fall back to domain-scoped rather
// than referencing a foreign project. With one domain and zero projects, every
// grant must be domain-scoped.
func TestGenerateProjectlessDomainFallsBackToDomainScoped(t *testing.T) {
	s := smallScenario()
	s.Resources.Projects = 0
	s.Distribution.DomainScopedAssignmentRatio = 0 // force the project-scoped path

	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	if len(p.Assignments) == 0 {
		t.Fatal("no assignments generated; the fallback path is not exercised")
	}
	for _, a := range p.Assignments {
		if a.Project != "" {
			t.Errorf("assignment for user %q targets project %q despite a projectless domain", a.User, a.Project)
		}
	}
	// With no projects there can be no token issues.
	if len(p.Tokens) != 0 {
		t.Errorf("tokens = %d, want 0 when no projects exist", len(p.Tokens))
	}
}
