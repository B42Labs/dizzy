package keystonegraph

import (
	"slices"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneexec "github.com/B42Labs/dizzy/internal/keystone/executor"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
)

// testBindings is the stable scaffold every test churns within: one real domain
// and two reused roles.
func testBindings() keystoneexec.Bindings {
	return keystoneexec.Bindings{
		Domains: map[string]string{"dom-1": "dom-real"},
		Roles:   map[string]string{"role-1": "r-a", "role-2": "r-b"},
	}
}

// churnPlan is a dependency-rich single-domain plan: two projects, two users,
// three grants (one domain-scoped), and two token issues on the project grants.
func churnPlan() *keystoneplan.Plan {
	return &keystoneplan.Plan{
		Scenario: "churn", Seed: 7,
		Domains:  []keystoneplan.Domain{{Name: "dom-1"}},
		Roles:    []keystoneplan.Role{{Name: "role-1"}, {Name: "role-2"}},
		Projects: []keystoneplan.Project{{Name: "proj-1", Domain: "dom-1"}, {Name: "proj-2", Domain: "dom-1"}},
		Users:    []keystoneplan.User{{Name: "user-1", Domain: "dom-1"}, {Name: "user-2", Domain: "dom-1"}},
		Assignments: []keystoneplan.Assignment{
			{User: "user-1", Role: "role-1", Project: "proj-1"},
			{User: "user-1", Role: "role-2"}, // domain-scoped
			{User: "user-2", Role: "role-1", Project: "proj-2"},
		},
		Tokens: []keystoneplan.TokenIssue{
			{User: "user-1", Project: "proj-1"},
			{User: "user-2", Project: "proj-2"},
		},
	}
}

func nodeByKey(t *testing.T, nodes []chaos.Node, key string) chaos.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Key == key {
			return n
		}
	}
	t.Fatalf("no node with key %q in %d nodes", key, len(nodes))
	return chaos.Node{}
}

// TestBuildShape confirms domains and roles are not nodes (the stable scaffold),
// projects and users are parentless nodes, assignments are parented on their
// user (and project when project-scoped), and Mutate is set exactly on the
// project-scoped assignments that back a planned token issue.
func TestBuildShape(t *testing.T) {
	p := churnPlan()
	nodes, err := Build(p, newFakeKeystone(p), testBindings(), time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// 2 projects + 2 users + 3 assignments = 7. Domains and roles are not nodes.
	if len(nodes) != 7 {
		t.Fatalf("built %d nodes, want 7 (projects + users + assignments; no domain/role nodes)", len(nodes))
	}

	for _, pr := range p.Projects {
		n := nodeByKey(t, nodes, pr.Name)
		if n.Kind != keystone.KindProject || len(n.Parents) != 0 {
			t.Errorf("project node %q kind=%q parents=%v, want project and parentless", pr.Name, n.Kind, n.Parents)
		}
	}
	for _, u := range p.Users {
		n := nodeByKey(t, nodes, u.Name)
		if n.Kind != keystone.KindUser || len(n.Parents) != 0 {
			t.Errorf("user node %q kind=%q parents=%v, want user and parentless", u.Name, n.Kind, n.Parents)
		}
	}

	// assign-0001 (user-1/proj-1) backs a token -> mutable, parents [user-1, proj-1].
	a1 := nodeByKey(t, nodes, "assign-0001")
	if a1.Kind != keystone.KindAssignment {
		t.Errorf("assign-0001 kind = %q, want %q", a1.Kind, keystone.KindAssignment)
	}
	if !slices.Equal(a1.Parents, []string{"user-1", "proj-1"}) {
		t.Errorf("assign-0001 parents = %v, want [user-1 proj-1]", a1.Parents)
	}
	if a1.Mutate == nil {
		t.Error("assign-0001 backs a token issue but is not mutable")
	}

	// assign-0002 (user-1 domain-scoped) parents only on the user, no token.
	a2 := nodeByKey(t, nodes, "assign-0002")
	if !slices.Equal(a2.Parents, []string{"user-1"}) {
		t.Errorf("assign-0002 parents = %v, want [user-1] (domain-scoped)", a2.Parents)
	}
	if a2.Mutate != nil {
		t.Error("assign-0002 is domain-scoped and backs no token, but is mutable")
	}

	// assign-0003 (user-2/proj-2) backs a token -> mutable.
	a3 := nodeByKey(t, nodes, "assign-0003")
	if a3.Mutate == nil {
		t.Error("assign-0003 backs a token issue but is not mutable")
	}
}

// TestBuildRejectsInvalidPlan confirms Build surfaces a plan validation error
// (here a token issue with no matching assignment) rather than emitting a node
// graph that misrepresents the plan.
func TestBuildRejectsInvalidPlan(t *testing.T) {
	p := churnPlan()
	p.Tokens = append(p.Tokens, keystoneplan.TokenIssue{User: "user-2", Project: "proj-1"}) // user-2 has no proj-1 grant
	if _, err := Build(p, newFakeKeystone(p), testBindings(), time.Minute); err == nil {
		t.Fatal("Build of a plan with a token lacking a matching grant: expected an error, got nil")
	}
}
