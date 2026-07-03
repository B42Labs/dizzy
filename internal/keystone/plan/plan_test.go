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

func TestDomainScopedAssignments(t *testing.T) {
	if got := validPlan().DomainScopedAssignments(); got != 1 {
		t.Errorf("DomainScopedAssignments() = %d, want 1", got)
	}
}
