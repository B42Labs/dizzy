package plan

import (
	"strings"
	"testing"
)

// validPlan is a small but complete, self-consistent plan: two domains and two
// roles, projects and users in each domain, a project-scoped and a
// domain-scoped grant, and a token issue backed by the project grant.
func validPlan() *Plan {
	return &Plan{
		Scenario: "test",
		Seed:     1,
		Domains:  []Domain{{Name: "dom-0001"}, {Name: "dom-0002"}},
		Roles:    []Role{{Name: "role-0001"}, {Name: "role-0002"}},
		Projects: []Project{
			{Name: "proj-0001", Domain: "dom-0001"},
			{Name: "proj-0002", Domain: "dom-0002"},
		},
		Users: []User{
			{Name: "user-0001", Domain: "dom-0001"},
			{Name: "user-0002", Domain: "dom-0002"},
		},
		Assignments: []Assignment{
			{User: "user-0001", Role: "role-0001", Project: "proj-0001"},
			{User: "user-0001", Role: "role-0002"}, // domain-scoped
			{User: "user-0002", Role: "role-0001", Project: "proj-0002"},
		},
		Tokens: []TokenIssue{
			{User: "user-0001", Project: "proj-0001"},
		},
	}
}

func TestValidateAcceptsValidPlan(t *testing.T) {
	if err := validPlan().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for a well-formed plan", err)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Plan)
		wantMsg string
	}{
		{
			name:    "duplicate domain name",
			mutate:  func(p *Plan) { p.Domains = append(p.Domains, Domain{Name: "dom-0001"}) },
			wantMsg: "duplicate domain",
		},
		{
			name:    "duplicate role name",
			mutate:  func(p *Plan) { p.Roles = append(p.Roles, Role{Name: "role-0001"}) },
			wantMsg: "duplicate role",
		},
		{
			name:    "project references unknown domain",
			mutate:  func(p *Plan) { p.Projects[0].Domain = "ghost" },
			wantMsg: "unknown domain",
		},
		{
			name:    "user references unknown domain",
			mutate:  func(p *Plan) { p.Users[0].Domain = "ghost" },
			wantMsg: "unknown domain",
		},
		{
			name:    "assignment references unknown user",
			mutate:  func(p *Plan) { p.Assignments[0].User = "ghost" },
			wantMsg: "unknown user",
		},
		{
			name:    "assignment references unknown role",
			mutate:  func(p *Plan) { p.Assignments[0].Role = "ghost" },
			wantMsg: "unknown role",
		},
		{
			name:    "assignment references unknown project",
			mutate:  func(p *Plan) { p.Assignments[0].Project = "ghost" },
			wantMsg: "unknown project",
		},
		{
			name: "duplicate grant triple",
			mutate: func(p *Plan) {
				p.Assignments = append(p.Assignments, Assignment{User: "user-0001", Role: "role-0001", Project: "proj-0001"})
			},
			wantMsg: "duplicate assignment",
		},
		{
			name:    "token references unknown user",
			mutate:  func(p *Plan) { p.Tokens[0].User = "ghost" },
			wantMsg: "unknown user",
		},
		{
			name:    "token references unknown project",
			mutate:  func(p *Plan) { p.Tokens[0].Project = "ghost" },
			wantMsg: "unknown project",
		},
		{
			name: "token without a matching project grant",
			mutate: func(p *Plan) {
				// user-0002 has a grant on proj-0002, not proj-0001.
				p.Tokens = []TokenIssue{{User: "user-0002", Project: "proj-0001"}}
			},
			wantMsg: "no matching project-scoped assignment",
		},
		{
			name:    "implied role rule references unknown prior role",
			mutate:  func(p *Plan) { p.ImpliedRoles = []ImpliedRole{{Prior: "ghost", Implied: "role-0002"}} },
			wantMsg: `implied role rule references unknown role "ghost"`,
		},
		{
			name:    "implied role rule references unknown implied role",
			mutate:  func(p *Plan) { p.ImpliedRoles = []ImpliedRole{{Prior: "role-0001", Implied: "ghost"}} },
			wantMsg: `implied role rule references unknown role "ghost"`,
		},
		{
			name:    "implied role rule implies its own prior",
			mutate:  func(p *Plan) { p.ImpliedRoles = []ImpliedRole{{Prior: "role-0001", Implied: "role-0001"}} },
			wantMsg: `implied role rule lets role "role-0001" imply itself`,
		},
		{
			name: "duplicate implied role rule",
			mutate: func(p *Plan) {
				p.ImpliedRoles = []ImpliedRole{{Prior: "role-0001", Implied: "role-0002"}, {Prior: "role-0001", Implied: "role-0002"}}
			},
			wantMsg: `duplicate implied role rule ("role-0001" implies "role-0002")`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validPlan()
			tc.mutate(p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error mentioning %q", tc.wantMsg)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("Validate() = %q, want it to mention %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestDomainScopedGrantMatchesUserDomain confirms a token whose project grant is
// domain-scoped only (no project grant) is rejected: the user has a domain grant
// but no scoped-token target.
func TestSummaryCounts(t *testing.T) {
	got := validPlan().Summary()
	for _, want := range []string{
		`scenario "test"`,
		"domains:         2",
		"roles:           2",
		"projects:        2",
		"users:           2",
		"assignments:     3 (1 domain-scoped)",
		"token issues:    1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary() missing %q:\n%s", want, got)
		}
	}
}

// TestSummaryFanUsers checks the fan users line: a plan with a fan user
// prints its count right after the users line, which counts the fan user too,
// and a plan without fan users prints no such line.
func TestSummaryFanUsers(t *testing.T) {
	p := validPlan()
	p.Users = append(p.Users, User{Name: "fan-0001", Domain: "dom-0001", Fan: true})
	p.Assignments = append(p.Assignments,
		Assignment{User: "fan-0001", Role: "role-0001", Project: "proj-0001"},
		Assignment{User: "fan-0001", Role: "role-0002", Project: "proj-0002"},
	)
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for a plan with a fan user", err)
	}
	if got := p.FanUsers(); got != 1 {
		t.Errorf("FanUsers() = %d, want 1", got)
	}
	got := p.Summary()
	if want := "  users:           3\n  fan users:       1\n"; !strings.Contains(got, want) {
		t.Errorf("Summary() missing %q:\n%s", want, got)
	}

	if got := validPlan().Summary(); strings.Contains(got, "fan users") {
		t.Errorf("Summary() of a plan without fan users has a fan users line:\n%s", got)
	}
}

func TestDomainScopedAssignments(t *testing.T) {
	if got := validPlan().DomainScopedAssignments(); got != 1 {
		t.Errorf("DomainScopedAssignments() = %d, want 1", got)
	}
}

// TestSummaryImpliedRoles checks the implied roles line: a plan with rules
// that validates prints their count right after the roles line, and a plan
// without rules prints no such line.
func TestSummaryImpliedRoles(t *testing.T) {
	p := validPlan()
	p.ImpliedRoles = []ImpliedRole{
		{Prior: "role-0001", Implied: "role-0002"},
		{Prior: "role-0002", Implied: "role-0001"},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for a plan with implied-role rules", err)
	}
	got := p.Summary()
	if want := "  roles:           2\n  implied roles:   2\n  projects:"; !strings.Contains(got, want) {
		t.Errorf("Summary() missing %q:\n%s", want, got)
	}

	if got := validPlan().Summary(); strings.Contains(got, "implied roles") {
		t.Errorf("Summary() of a plan without implied-role rules has an implied roles line:\n%s", got)
	}
}

// delegatedPlan is validPlan with one application credential and one trust,
// both on user-0001's project-scoped grant of role-0001 on proj-0001, the
// trust delegating it to user-0002.
func delegatedPlan() *Plan {
	p := validPlan()
	p.ApplicationCredentials = []ApplicationCredential{
		{Name: "appcred-0001", User: "user-0001", Project: "proj-0001", Role: "role-0001"},
	}
	p.Trusts = []Trust{
		{Name: "trust-0001", Trustor: "user-0001", Trustee: "user-0002", Project: "proj-0001", Role: "role-0001"},
	}
	return p
}

// TestValidateAcceptsDelegations confirms a plan whose application credential
// and trust each rest on a project-scoped grant of their owner validates, and
// that a plan whose two slices are nil validates as well.
func TestValidateAcceptsDelegations(t *testing.T) {
	if err := delegatedPlan().Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for a plan with an application credential and a trust", err)
	}
	p := validPlan()
	if p.ApplicationCredentials != nil || p.Trusts != nil {
		t.Fatalf("validPlan() has application credentials %+v and trusts %+v, want both nil", p.ApplicationCredentials, p.Trusts)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for a plan whose application credentials and trusts are nil", err)
	}
}

// TestValidateRejectsDelegations covers each way an application credential or
// a trust can break the plan, one violated rule at a time with its exact
// message, an unknown user, project or role falling under the one missing
// grant check, and the order in which Validate reports two failures.
func TestValidateRejectsDelegations(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Plan)
		wantMsg string
	}{
		{
			name: "duplicate application credential name",
			mutate: func(p *Plan) {
				p.ApplicationCredentials = append(p.ApplicationCredentials, p.ApplicationCredentials[0])
			},
			wantMsg: `duplicate application credential name "appcred-0001"`,
		},
		{
			name: "duplicate trust name",
			mutate: func(p *Plan) {
				p.Trusts = append(p.Trusts, Trust{Name: "trust-0001", Trustor: "user-0002", Trustee: "user-0001", Project: "proj-0002", Role: "role-0001"})
			},
			wantMsg: `duplicate trust name "trust-0001"`,
		},
		{
			// user-0001 holds role-0002 on its domain, which carries no
			// application credential.
			name: "application credential on a domain-scoped grant",
			mutate: func(p *Plan) {
				p.ApplicationCredentials[0].Project = ""
				p.ApplicationCredentials[0].Role = "role-0002"
			},
			wantMsg: `application credential "appcred-0001" has no matching project-scoped assignment (user "user-0001", project "", role "role-0002")`,
		},
		{
			// user-0002 holds role-0001 on proj-0002, not on proj-0001.
			name:    "application credential on another user's grant",
			mutate:  func(p *Plan) { p.ApplicationCredentials[0].User = "user-0002" },
			wantMsg: `application credential "appcred-0001" has no matching project-scoped assignment (user "user-0002", project "proj-0001", role "role-0001")`,
		},
		{
			name:    "application credential of an unknown user",
			mutate:  func(p *Plan) { p.ApplicationCredentials[0].User = "ghost" },
			wantMsg: `application credential "appcred-0001" has no matching project-scoped assignment (user "ghost", project "proj-0001", role "role-0001")`,
		},
		{
			name:    "application credential on an unknown project",
			mutate:  func(p *Plan) { p.ApplicationCredentials[0].Project = "ghost" },
			wantMsg: `application credential "appcred-0001" has no matching project-scoped assignment (user "user-0001", project "ghost", role "role-0001")`,
		},
		{
			name:    "application credential with an unknown role",
			mutate:  func(p *Plan) { p.ApplicationCredentials[0].Role = "ghost" },
			wantMsg: `application credential "appcred-0001" has no matching project-scoped assignment (user "user-0001", project "proj-0001", role "ghost")`,
		},
		{
			name: "trust on a domain-scoped grant",
			mutate: func(p *Plan) {
				p.Trusts[0].Project = ""
				p.Trusts[0].Role = "role-0002"
			},
			wantMsg: `trust "trust-0001" has no matching project-scoped assignment (trustor "user-0001", project "", role "role-0002")`,
		},
		{
			name: "trust from a trustor without the grant",
			mutate: func(p *Plan) {
				p.Trusts[0].Trustor = "user-0002"
				p.Trusts[0].Trustee = "user-0001"
			},
			wantMsg: `trust "trust-0001" has no matching project-scoped assignment (trustor "user-0002", project "proj-0001", role "role-0001")`,
		},
		{
			name:    "trust from an unknown trustor",
			mutate:  func(p *Plan) { p.Trusts[0].Trustor = "ghost" },
			wantMsg: `trust "trust-0001" has no matching project-scoped assignment (trustor "ghost", project "proj-0001", role "role-0001")`,
		},
		{
			name:    "trust to an unknown trustee",
			mutate:  func(p *Plan) { p.Trusts[0].Trustee = "ghost" },
			wantMsg: `trust "trust-0001" references unknown trustee "ghost"`,
		},
		{
			name:    "trust to its own trustor",
			mutate:  func(p *Plan) { p.Trusts[0].Trustee = "user-0001" },
			wantMsg: `trust "trust-0001" names "user-0001" as trustor and trustee`,
		},
		{
			// The second trust delegates another role on the same project, so
			// only the (trustor, trustee, project) triple repeats.
			name: "duplicate trust",
			mutate: func(p *Plan) {
				p.Assignments = append(p.Assignments, Assignment{User: "user-0001", Role: "role-0002", Project: "proj-0001"})
				p.Trusts = append(p.Trusts, Trust{Name: "trust-0002", Trustor: "user-0001", Trustee: "user-0002", Project: "proj-0001", Role: "role-0002"})
			},
			wantMsg: `duplicate trust (trustor "user-0001", trustee "user-0002", project "proj-0001")`,
		},
		{
			name: "token issues are checked before the delegations",
			mutate: func(p *Plan) {
				p.Tokens[0].User = "ghost"
				p.ApplicationCredentials[0].User = "ghost"
			},
			wantMsg: `token issue references unknown user "ghost"`,
		},
		{
			name: "names are checked before the grants",
			mutate: func(p *Plan) {
				p.ApplicationCredentials[0].User = "ghost"
				p.Trusts = append(p.Trusts, p.Trusts[0])
			},
			wantMsg: `duplicate trust name "trust-0001"`,
		},
		{
			name: "application credentials are checked before trusts",
			mutate: func(p *Plan) {
				p.Trusts[0].Trustor = "ghost"
				p.ApplicationCredentials[0].Role = "ghost"
			},
			wantMsg: `application credential "appcred-0001" has no matching project-scoped assignment (user "user-0001", project "proj-0001", role "ghost")`,
		},
		{
			name: "the trustor's grant is checked before the trustee",
			mutate: func(p *Plan) {
				p.Trusts[0].Project = ""
				p.Trusts[0].Trustee = "ghost"
			},
			wantMsg: `trust "trust-0001" has no matching project-scoped assignment (trustor "user-0001", project "", role "role-0001")`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := delegatedPlan()
			tc.mutate(p)
			err := p.Validate()
			if err == nil || err.Error() != tc.wantMsg {
				t.Fatalf("Validate() = %v, want %q", err, tc.wantMsg)
			}
		})
	}
}

// TestSummaryDelegations checks the application credentials and trusts lines:
// a plan with 2 credentials and 3 trusts that validates prints both counts
// right after the token issues line, and a plan with neither prints neither
// line.
func TestSummaryDelegations(t *testing.T) {
	p := validPlan()
	p.Users = append(p.Users, User{Name: "user-0003", Domain: "dom-0001"})
	p.ApplicationCredentials = []ApplicationCredential{
		{Name: "appcred-0001", User: "user-0001", Project: "proj-0001", Role: "role-0001"},
		{Name: "appcred-0002", User: "user-0002", Project: "proj-0002", Role: "role-0001"},
	}
	p.Trusts = []Trust{
		{Name: "trust-0001", Trustor: "user-0001", Trustee: "user-0002", Project: "proj-0001", Role: "role-0001"},
		{Name: "trust-0002", Trustor: "user-0001", Trustee: "user-0003", Project: "proj-0001", Role: "role-0001"},
		{Name: "trust-0003", Trustor: "user-0002", Trustee: "user-0001", Project: "proj-0002", Role: "role-0001"},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for a plan with application credentials and trusts", err)
	}
	got := p.Summary()
	if want := "  token issues:    1\n  app credentials: 2\n  trusts:          3\n"; !strings.Contains(got, want) {
		t.Errorf("Summary() missing %q:\n%s", want, got)
	}

	got = validPlan().Summary()
	for _, line := range []string{"app credentials:", "trusts:"} {
		if strings.Contains(got, line) {
			t.Errorf("Summary() of a plan without application credentials or trusts has a %q line:\n%s", line, got)
		}
	}
}
