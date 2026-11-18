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
// grant must be domain-scoped. Such a plan has no grant to create an
// application credential or a trust on, so both counts are zero here;
// TestGenerateDelegationsWithoutProjectScopedAssignment covers the error a
// positive count meets.
func TestGenerateProjectlessDomainFallsBackToDomainScoped(t *testing.T) {
	s := smallScenario()
	s.Resources.Projects = 0
	s.Resources.ApplicationCredentials = 0
	s.Resources.Trusts = 0
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

// TestGenerateDelegations pins steps 8 and 9 of the generator on the small
// fixture: its two application credentials and two trusts are exactly the
// entries the draw order yields at seed 42, named contiguously from 0001.
func TestGenerateDelegations(t *testing.T) {
	p, err := smallScenario().Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	wantCreds := []plan.ApplicationCredential{
		{Name: "appcred-0001", User: "user-0002", Project: "proj-0005", Role: "role-0001"},
		{Name: "appcred-0002", User: "user-0001", Project: "proj-0003", Role: "role-0002"},
	}
	if !reflect.DeepEqual(p.ApplicationCredentials, wantCreds) {
		t.Errorf("ApplicationCredentials = %+v, want %+v", p.ApplicationCredentials, wantCreds)
	}
	wantTrusts := []plan.Trust{
		{Name: "trust-0001", Trustor: "user-0008", Trustee: "user-0001", Project: "proj-0002", Role: "role-0001"},
		{Name: "trust-0002", Trustor: "user-0001", Trustee: "user-0004", Project: "proj-0003", Role: "role-0002"},
	}
	if !reflect.DeepEqual(p.Trusts, wantTrusts) {
		t.Errorf("Trusts = %+v, want %+v", p.Trusts, wantTrusts)
	}
}

// TestGenerateDelegationsZeroLeavesPlanUnchanged confirms steps 8 and 9 draw
// nothing at a zero count and come after every other step: the small fixture
// with both counts at 0 marshals without the application_credentials and
// trusts keys, byte for byte as the fixture's own plan with both slices set
// aside.
func TestGenerateDelegationsZeroLeavesPlanUnchanged(t *testing.T) {
	s := smallScenario()
	s.Resources.ApplicationCredentials = 0
	s.Resources.Trusts = 0
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate() with both counts 0: %v", err)
	}
	got := marshal(t, p)
	for _, key := range []string{`"application_credentials"`, `"trusts"`} {
		if bytes.Contains(got, []byte(key)) {
			t.Errorf("the plan JSON of a scenario with both counts 0 has the key %s", key)
		}
	}

	fixture, err := smallScenario().Generate()
	if err != nil {
		t.Fatalf("Generate() of the fixture: %v", err)
	}
	if len(fixture.ApplicationCredentials) == 0 || len(fixture.Trusts) == 0 {
		t.Fatalf("the fixture plan has %d application credentials and %d trusts, want both drawn",
			len(fixture.ApplicationCredentials), len(fixture.Trusts))
	}
	fixture.ApplicationCredentials = nil
	fixture.Trusts = nil
	if want := marshal(t, fixture); !bytes.Equal(got, want) {
		t.Errorf("the plan with both counts 0 differs from the fixture's plan without them:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestGenerateDelegationsWithoutProjectScopedAssignment confirms a positive
// count of either kind fails Generate with the message naming its key when
// every grant of the plan is domain-scoped, so there is no project-scoped
// assignment to create one on.
func TestGenerateDelegationsWithoutProjectScopedAssignment(t *testing.T) {
	tests := []struct {
		name    string
		creds   int
		trusts  int
		wantMsg string
	}{
		{
			name:    "application credentials",
			creds:   2,
			wantMsg: "resources.application_credentials is 2 but the plan has no project-scoped assignment to create one on",
		},
		{
			name:    "trusts",
			trusts:  2,
			wantMsg: "resources.trusts is 2 but the plan has no project-scoped assignment to create one on",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			s.Distribution.DomainScopedAssignmentRatio = 1
			s.Resources.ApplicationCredentials = tc.creds
			s.Resources.Trusts = tc.trusts
			p, err := s.Generate()
			if err == nil || err.Error() != tc.wantMsg {
				t.Fatalf("Generate() = %v, want %q", err, tc.wantMsg)
			}
			if p != nil {
				t.Errorf("Generate() returned a plan alongside the error: %+v", p)
			}
		})
	}
}

// TestGenerateTrustsSkipDuplicates confirms step 9 skips a draw whose
// (trustor, trustee, project) an earlier trust already has rather than
// redrawing it: two users holding one grant each on the one project allow two
// distinct trusts, so ten draws yield at most two, named contiguously in
// order, each delegating to the other user.
func TestGenerateTrustsSkipDuplicates(t *testing.T) {
	s := smallScenario()
	s.Resources.Domains = 1
	s.Resources.Roles = 1
	s.Resources.Projects = 1
	s.Resources.Users = 2
	s.Resources.ApplicationCredentials = 0
	s.Resources.Trusts = 10
	s.Distribution.AssignmentsPerUser = Range{Min: 1, Max: 1}
	s.Distribution.DomainScopedAssignmentRatio = 0

	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	if len(p.Trusts) > 2 {
		t.Fatalf("trusts = %d, want at most 2 distinct (trustor, trustee, project): %+v", len(p.Trusts), p.Trusts)
	}
	var names []string
	seen := make(map[string]bool, len(p.Trusts))
	for _, tr := range p.Trusts {
		names = append(names, tr.Name)
		key := tr.Trustor + "\x00" + tr.Trustee + "\x00" + tr.Project
		if seen[key] {
			t.Errorf("trust %q repeats (trustor %q, trustee %q, project %q)", tr.Name, tr.Trustor, tr.Trustee, tr.Project)
		}
		seen[key] = true
		if tr.Trustor == tr.Trustee {
			t.Errorf("trust %q names %q as trustor and trustee", tr.Name, tr.Trustor)
		}
		if tr.Project != "proj-0001" || tr.Role != "role-0001" {
			t.Errorf("trust %q is on project %q with role %q, want proj-0001 and role-0001", tr.Name, tr.Project, tr.Role)
		}
	}
	if want := []string{"trust-0001", "trust-0002"}; !reflect.DeepEqual(names, want) {
		t.Errorf("trust names = %v, want %v", names, want)
	}
	if len(p.ApplicationCredentials) != 0 {
		t.Errorf("application credentials = %d, want 0", len(p.ApplicationCredentials))
	}
}

// TestGenerateTrustNamesContiguousAfterSkip confirms a trust kept after a
// skipped draw takes the next name rather than its draw's: at seed 1 the
// second draw repeats the first (two draws yield one trust) and the third is
// kept, as trust-0002, not trust-0003.
func TestGenerateTrustNamesContiguousAfterSkip(t *testing.T) {
	gen := func(trusts int) []plan.Trust {
		t.Helper()
		s := smallScenario()
		s.Seed = 1
		s.Resources.Domains, s.Resources.Roles, s.Resources.Projects, s.Resources.Users = 1, 1, 1, 2
		s.Resources.ApplicationCredentials = 0
		s.Resources.Trusts = trusts
		s.Distribution.AssignmentsPerUser = Range{Min: 1, Max: 1}
		s.Distribution.DomainScopedAssignmentRatio = 0
		p, err := s.Generate()
		if err != nil {
			t.Fatalf("Generate(trusts=%d): %v", trusts, err)
		}
		return p.Trusts
	}
	if got := gen(2); len(got) != 1 {
		t.Fatalf("two draws yield %d trusts (%+v), want 1: the fixture needs the second draw to repeat the first", len(got), got)
	}
	got := gen(3)
	var names []string
	for _, tr := range got {
		names = append(names, tr.Name)
	}
	if want := []string{"trust-0001", "trust-0002"}; !reflect.DeepEqual(names, want) {
		t.Errorf("trust names = %v, want %v", names, want)
	}
	if len(got) == 2 && got[0].Trustor == got[1].Trustor {
		t.Errorf("trusts %+v share a trustor, want the third draw to delegate the other way", got)
	}
}

// TestGenerateDelegationsDrawFanGrants confirms steps 8 and 9 draw from the fan
// users' grants too, and that two fan users are the two users a trust needs:
// with no regular user, every application credential belongs to a fan user and
// every trust delegates from one fan user to the other.
func TestGenerateDelegationsDrawFanGrants(t *testing.T) {
	s := smallScenario()
	s.Resources.Users = 0
	s.Distribution.FanUsers = 2
	s.Distribution.FanAssignments = Range{Min: 1, Max: 1}

	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	if len(p.ApplicationCredentials) != 2 {
		t.Errorf("application credentials = %d, want 2", len(p.ApplicationCredentials))
	}
	for _, ac := range p.ApplicationCredentials {
		if !strings.HasPrefix(ac.User, "fan-") {
			t.Errorf("application credential %q belongs to %q, want a fan user", ac.Name, ac.User)
		}
	}
	if len(p.Trusts) == 0 {
		t.Fatal("no trust drawn from the fan users' grants")
	}
	for _, tr := range p.Trusts {
		if !strings.HasPrefix(tr.Trustor, "fan-") || !strings.HasPrefix(tr.Trustee, "fan-") || tr.Trustor == tr.Trustee {
			t.Errorf("trust %q delegates from %q to %q, want one fan user to the other", tr.Name, tr.Trustor, tr.Trustee)
		}
	}
}
