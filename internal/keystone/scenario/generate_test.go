package scenario

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// fanScenario is smallScenario with four regular users and two fan users
// holding exactly three grants each over its five projects.
func fanScenario() Scenario {
	s := smallScenario()
	s.Resources.Users = 4
	s.Distribution.FanUsers = 2
	s.Distribution.FanAssignments = Range{Min: 3, Max: 3}
	return s
}

// TestGenerateFanUsers checks step 7 of the generator: the fan users follow
// the regular users, live in the first domain, are marked in the plan JSON,
// hold exactly fan_assignments grants on distinct projects and no token
// issue, and leave everything the earlier steps drew as it is without them.
// A fan user holding every project spans every domain, since the own-domain
// rule of the regular grants does not apply to it.
func TestGenerateFanUsers(t *testing.T) {
	s := fanScenario()
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}

	var names []string
	for _, u := range p.Users {
		names = append(names, u.Name)
	}
	wantNames := []string{"user-0001", "user-0002", "user-0003", "user-0004", "fan-0001", "fan-0002"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("users = %v, want %v", names, wantNames)
	}
	for _, u := range p.Users {
		if wantFan := strings.HasPrefix(u.Name, "fan-"); u.Fan != wantFan {
			t.Errorf("user %q has fan %v, want %v", u.Name, u.Fan, wantFan)
		}
		if u.Fan && u.Domain != "dom-0001" {
			t.Errorf("fan user %q lives in %q, want dom-0001", u.Name, u.Domain)
		}
	}
	if got := p.FanUsers(); got != 2 {
		t.Errorf("FanUsers() = %d, want 2", got)
	}
	if n := bytes.Count(marshal(t, p), []byte(`"fan": true`)); n != 2 {
		t.Errorf("the plan JSON carries %d \"fan\": true keys, want one per fan user", n)
	}

	fanProjects := make(map[string]map[string]bool)
	fanGrants := make(map[string]int)
	for _, a := range p.Assignments {
		if !strings.HasPrefix(a.User, "fan-") {
			continue
		}
		if a.Project == "" {
			t.Errorf("fan user %q holds a domain-scoped grant", a.User)
		}
		if fanProjects[a.User] == nil {
			fanProjects[a.User] = make(map[string]bool)
		}
		fanProjects[a.User][a.Project] = true
		fanGrants[a.User]++
	}
	for _, name := range []string{"fan-0001", "fan-0002"} {
		if fanGrants[name] != 3 || len(fanProjects[name]) != 3 {
			t.Errorf("fan user %q holds %d grants on %d distinct projects, want 3 on 3", name, fanGrants[name], len(fanProjects[name]))
		}
	}
	for _, tok := range p.Tokens {
		if strings.HasPrefix(tok.User, "fan-") {
			t.Errorf("fan user %q issues a plan token", tok.User)
		}
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Plan.Validate() = %v, want nil", err)
	}

	// The fan users draw after every other step, so what precedes them is the
	// plan of the same scenario without fan users.
	regular := s
	regular.Distribution.FanUsers = 0
	rp, err := regular.Generate()
	if err != nil {
		t.Fatalf("Generate() without fan users: %v", err)
	}
	if !reflect.DeepEqual(p.Projects, rp.Projects) || !reflect.DeepEqual(p.Users[:4], rp.Users) ||
		!reflect.DeepEqual(p.Assignments[:len(rp.Assignments)], rp.Assignments) || !reflect.DeepEqual(p.Tokens, rp.Tokens) {
		t.Error("the fan users changed what the earlier steps drew")
	}

	spread := smallScenario()
	spread.Resources.Domains = 2
	spread.Distribution.FanUsers = 1
	spread.Distribution.FanAssignments = Range{Min: 5, Max: 5}
	sp, err := spread.Generate()
	if err != nil {
		t.Fatalf("Generate() over two domains: %v", err)
	}
	projectDomain := make(map[string]string, len(sp.Projects))
	for _, pr := range sp.Projects {
		projectDomain[pr.Name] = pr.Domain
	}
	domains := make(map[string]bool)
	for _, a := range sp.Assignments {
		if a.User == "fan-0001" {
			domains[projectDomain[a.Project]] = true
		}
	}
	if len(domains) != 2 {
		t.Errorf("a fan user holding every project spans domains %v, want both", domains)
	}
}

// TestGenerateFanDeterministic confirms the fan users' draws are as
// reproducible as the rest of the plan: two generations of the same scenario
// with fan users marshal byte-identically.
func TestGenerateFanDeterministic(t *testing.T) {
	s := fanScenario()
	p1, err := s.Generate()
	if err != nil {
		t.Fatalf("first Generate(): %v", err)
	}
	p2, err := s.Generate()
	if err != nil {
		t.Fatalf("second Generate(): %v", err)
	}
	if got, want := marshal(t, p1), marshal(t, p2); !bytes.Equal(got, want) {
		t.Error("two generations of the same scenario with fan users differ")
	}
}

// TestGenerateFanZeroLeavesPlanUnchanged confirms a scenario without fan
// users draws nothing for them: the small scenario with a fan_assignments
// range but fan_users 0 still generates the golden plan byte for byte.
func TestGenerateFanZeroLeavesPlanUnchanged(t *testing.T) {
	s := smallScenario()
	s.Distribution.FanUsers = 0
	s.Distribution.FanAssignments = Range{Min: 2, Max: 3}
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "golden", "small.plan.json"))
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if !bytes.Equal(marshal(t, p), want) {
		t.Error("a fan_assignments range without fan users changed the generated plan")
	}
}

// TestGenerateImpliedRoles confirms every implied role of a rule becomes one
// (prior, implied) pair of the plan, rule by rule and entry by entry in
// declaration order.
func TestGenerateImpliedRoles(t *testing.T) {
	s := smallScenario()
	s.Resources.Roles = 3
	s.Resources.ImpliedRoles = []ImpliedRole{
		{Prior: "role-0001", Implies: []string{"role-0002", "role-0003"}},
		{Prior: "role-0002", Implies: []string{"role-0003"}},
	}
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	want := []plan.ImpliedRole{
		{Prior: "role-0001", Implied: "role-0002"},
		{Prior: "role-0001", Implied: "role-0003"},
		{Prior: "role-0002", Implied: "role-0003"},
	}
	if !reflect.DeepEqual(p.ImpliedRoles, want) {
		t.Errorf("ImpliedRoles = %+v, want %+v", p.ImpliedRoles, want)
	}
}

// TestGenerateImpliedRolesDrawNothing confirms the rules take no draw: the
// small scenario with a rule generates the golden plan byte for byte once its
// rules are set aside, so a scenario's rules change nothing else of its plan.
func TestGenerateImpliedRolesDrawNothing(t *testing.T) {
	s := smallScenario()
	s.Resources.ImpliedRoles = []ImpliedRole{{Prior: "role-0001", Implies: []string{"role-0002"}}}
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	if len(p.ImpliedRoles) != 1 {
		t.Fatalf("ImpliedRoles = %+v, want one rule", p.ImpliedRoles)
	}
	p.ImpliedRoles = nil
	want, err := os.ReadFile(filepath.Join("testdata", "golden", "small.plan.json"))
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if !bytes.Equal(marshal(t, p), want) {
		t.Error("an implied-role rule changed the rest of the generated plan")
	}
}
