package plan

import (
	"strings"
	"testing"
)

// validPlan is a plan with two users on one project.
func validPlan() *Plan {
	return &Plan{
		Scenario: "t",
		Seed:     1,
		Role:     "member",
		Projects: []Project{{Name: "project-0001"}},
		Users:    []User{{Name: "user-0001"}, {Name: "user-0002"}},
		Pages:    []Page{{Name: "a", Path: "/a/"}, {Name: "b", Path: "/b/"}},
		Sessions: []Session{
			{Name: "session-0001", User: "user-0001", Pages: []string{"a", "b"}, Switch: true, SwitchDraw: 3},
			{Name: "session-0002", User: "user-0002", Pages: []string{}},
		},
	}
}

func TestValidateAcceptsValidPlans(t *testing.T) {
	if err := validPlan().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	p := validPlan()
	p.Role, p.Projects, p.Users = "", []Project{}, []User{}
	for i := range p.Sessions {
		p.Sessions[i].User = ""
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() of a plan without users = %v, want nil", err)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Plan)
		want   string
	}{
		{"duplicate project", func(p *Plan) { p.Projects = append(p.Projects, p.Projects[0]) }, "duplicate project"},
		{"duplicate user", func(p *Plan) { p.Users[1] = p.Users[0] }, "duplicate user"},
		{"duplicate page", func(p *Plan) { p.Pages[1].Name = "a" }, "duplicate page"},
		{"duplicate session", func(p *Plan) { p.Sessions[1].Name = "session-0001" }, "duplicate session"},
		{"empty page path", func(p *Plan) { p.Pages[0].Path = "" }, "empty path"},
		{"unknown page", func(p *Plan) { p.Sessions[0].Pages = []string{"c"} }, "unknown page"},
		{"session without user", func(p *Plan) { p.Sessions[0].User = "" }, "names no user"},
		{"unknown user", func(p *Plan) { p.Sessions[0].User = "user-0009" }, "unknown user"},
		{"user without users", func(p *Plan) { p.Users, p.Role, p.Projects = nil, "", nil }, "has none"},
		{"users without role", func(p *Plan) { p.Role = "" }, "role"},
		{"role without users", func(p *Plan) {
			p.Users, p.Projects = nil, nil
			p.Sessions[0].User, p.Sessions[1].User = "", ""
		}, "role"},
		{"users without projects", func(p *Plan) { p.Projects = nil }, "project"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validPlan()
			tc.mutate(p)
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestSummary(t *testing.T) {
	got := validPlan().Summary()
	for _, want := range []string{
		`Plan for scenario "t" (seed 1)`,
		"identity:        2 created users on 1 created projects, role member",
		"sessions:        2",
		"page a (/a/): 1 session(s)",
		"page b (/b/): 1 session(s)",
		"switches:        1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary() lacks %q:\n%s", want, got)
		}
	}

	p := validPlan()
	p.Role, p.Projects, p.Users = "", []Project{}, []User{}
	if got := p.Summary(); !strings.Contains(got, "identity:        the clouds.yaml user") {
		t.Errorf("Summary() without users lacks the clouds.yaml identity line:\n%s", got)
	}
}
