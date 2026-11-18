package keystonegraph

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneexec "github.com/B42Labs/dizzy/internal/keystone/executor"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/resource"
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

// delegationPlan is churnPlan with application credentials and trusts on top.
// A fourth grant gives user-1 a second role on proj-1 (assign-0004), so two
// credentials of one user on one project rest on different assignment nodes
// told apart only by role, beside user-1's domain-scoped grant of that role.
func delegationPlan() *keystoneplan.Plan {
	p := churnPlan()
	p.Scenario = "delegation"
	p.Assignments = append(p.Assignments, keystoneplan.Assignment{User: "user-1", Role: "role-2", Project: "proj-1"})
	p.ApplicationCredentials = []keystoneplan.ApplicationCredential{
		{Name: "appcred-0001", User: "user-1", Project: "proj-1", Role: "role-1"},
		{Name: "appcred-0002", User: "user-1", Project: "proj-1", Role: "role-2"},
		{Name: "appcred-0003", User: "user-2", Project: "proj-2", Role: "role-1"},
	}
	p.Trusts = []keystoneplan.Trust{
		{Name: "trust-0001", Trustor: "user-1", Trustee: "user-2", Project: "proj-1", Role: "role-2"},
		{Name: "trust-0002", Trustor: "user-2", Trustee: "user-1", Project: "proj-2", Role: "role-1"},
	}
	return p
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

// TestBuildDelegationShape confirms Build appends one node per application
// credential, parented exactly on its user, its project and the assignment node
// of its (user, project, role) grant, and one node per trust, parented exactly
// on its trustor, its trustee, its project and the assignment node of the
// trustor's grant, each of its own kind and each carrying a Mutate.
func TestBuildDelegationShape(t *testing.T) {
	p := delegationPlan()
	nodes, err := Build(p, newFakeKeystone(p), testBindings(), time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// 2 projects + 2 users + 4 assignments + 3 credentials + 2 trusts = 13.
	if len(nodes) != 13 {
		t.Fatalf("built %d nodes, want 13 (projects + users + assignments + credentials + trusts)", len(nodes))
	}

	tests := []struct {
		key     string
		kind    resource.Kind
		parents []string
	}{
		{"appcred-0001", keystone.KindApplicationCredential, []string{"user-1", "proj-1", "assign-0001"}},
		{"appcred-0002", keystone.KindApplicationCredential, []string{"user-1", "proj-1", "assign-0004"}},
		{"appcred-0003", keystone.KindApplicationCredential, []string{"user-2", "proj-2", "assign-0003"}},
		{"trust-0001", keystone.KindTrust, []string{"user-1", "user-2", "proj-1", "assign-0004"}},
		{"trust-0002", keystone.KindTrust, []string{"user-2", "user-1", "proj-2", "assign-0003"}},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			n := nodeByKey(t, nodes, tc.key)
			if n.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", n.Kind, tc.kind)
			}
			if !slices.Equal(n.Parents, tc.parents) {
				t.Errorf("parents = %v, want %v", n.Parents, tc.parents)
			}
			if n.Mutate == nil {
				t.Error("Mutate is nil, want the authentication")
			}
		})
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

// TestDelegationDeleteAfterParentChurn confirms the delete of an application
// credential or trust, retried with the parent ids of its first attempt after
// a parent was churned, counts as done without a request: Keystone removed the
// credential with its user's grant and keeps the trust out of its trustor's
// reach, and a request as the owner could only fail. With its parents current
// the same delete still reaches the cloud.
func TestDelegationDeleteAfterParentChurn(t *testing.T) {
	ctx := context.Background()
	p := delegationPlan()
	f := newFakeKeystone(p)
	nodes, err := Build(p, f, testBindings(), time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	create := func(key string, ids map[string]string) resource.Resource {
		t.Helper()
		res, err := nodeByKey(t, nodes, key).Create(ctx, ids)
		if err != nil {
			t.Fatalf("creating %s: %v", key, err)
		}
		return res
	}
	remove := func(key string, ids map[string]string, res resource.Resource) {
		t.Helper()
		if err := nodeByKey(t, nodes, key).Delete(ctx, ids, res); err != nil {
			t.Fatalf("deleting %s: %v", key, err)
		}
	}

	// user-2 holds role-1 on proj-2 (assign-0003); appcred-0003 and trust-0002
	// (user-2 to user-1) rest on that grant.
	user1, user2, proj2 := create("user-1", nil), create("user-2", nil), create("proj-2", nil)
	grantIDs := map[string]string{"user-2": user2.ID, "proj-2": proj2.ID}
	grant := create("assign-0003", grantIDs)
	credIDs := map[string]string{"user-2": user2.ID, "proj-2": proj2.ID, "assign-0003": grant.ID}
	trustIDs := map[string]string{"user-2": user2.ID, "user-1": user1.ID, "proj-2": proj2.ID, "assign-0003": grant.ID}
	cred := create("appcred-0003", credIDs)
	trust := create("trust-0002", trustIDs)

	// With every parent current, both deletes reach the cloud.
	remove("appcred-0003", credIDs, cred)
	remove("trust-0002", trustIDs, trust)
	if f.deletes[keystone.KindApplicationCredential] != 1 || f.deletes[keystone.KindTrust] != 1 {
		t.Fatalf("deletes with current parents = %v, want one of each kind", f.deletes)
	}

	// Churn user-2: its grant is revoked, the user deleted and created again.
	cred, trust = create("appcred-0003", credIDs), create("trust-0002", trustIDs)
	remove("assign-0003", grantIDs, grant)
	remove("user-2", nil, user2)
	user2 = create("user-2", nil)
	remove("appcred-0003", credIDs, cred)
	remove("trust-0002", trustIDs, trust)
	if f.deletes[keystone.KindApplicationCredential] != 1 || f.deletes[keystone.KindTrust] != 1 {
		t.Errorf("deletes after the user churned = %v, want no further request", f.deletes)
	}

	// Churn proj-2 alone: the trust's trustor and trustee stay, its project
	// goes.
	grantIDs = map[string]string{"user-2": user2.ID, "proj-2": proj2.ID}
	grant = create("assign-0003", grantIDs)
	trustIDs = map[string]string{"user-2": user2.ID, "user-1": user1.ID, "proj-2": proj2.ID, "assign-0003": grant.ID}
	trust = create("trust-0002", trustIDs)
	remove("assign-0003", grantIDs, grant)
	remove("proj-2", nil, proj2)
	create("proj-2", nil)
	remove("trust-0002", trustIDs, trust)
	if f.deletes[keystone.KindTrust] != 1 {
		t.Errorf("trust deletes after its project churned = %d, want no further request", f.deletes[keystone.KindTrust])
	}
}
