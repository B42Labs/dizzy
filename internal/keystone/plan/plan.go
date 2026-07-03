// Package plan defines the expanded, fully-enumerated set of Keystone (Identity
// v3) resources — the expected-state source of truth produced deterministically
// from a scenario plus a seed. Like the neutron and cinder plans it is pure
// data: every collection is a slice (never a map) so that encoding the plan to
// JSON yields byte-identical output for the same input. Cross-resource
// references are by logical name, resolved by Validate; the two privilege tiers
// (admin, domain-manager) share the same cloud-independent plan and bind its
// logical domains and roles onto created-or-reused reality at apply time.
package plan

import (
	"fmt"
	"strings"
)

// Plan is the fully-expanded expected state for one Keystone run. Scenario and
// Seed record the provenance that produced it; the slices enumerate every
// domain, role, project, user, role assignment, and token issue. Domains and
// roles are created in admin mode and bound to the in-scope domain / reused
// roles in domain-manager mode. User passwords are never in the plan: each is
// derived deterministically from (seed, logical name) at apply time.
type Plan struct {
	Scenario    string       `json:"scenario"`
	Seed        int64        `json:"seed"`
	Domains     []Domain     `json:"domains"`
	Roles       []Role       `json:"roles"`
	Projects    []Project    `json:"projects"`
	Users       []User       `json:"users"`
	Assignments []Assignment `json:"assignments"`
	Tokens      []TokenIssue `json:"tokens"`
}

// Domain is an identity domain, referenced elsewhere by its logical name.
type Domain struct {
	Name string `json:"name"`
}

// Role is a role, referenced by assignments by its logical name.
type Role struct {
	Name string `json:"name"`
}

// Project is a project in a domain, both referenced by logical name.
type Project struct {
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

// User is a user in a domain, both referenced by logical name. Its password is
// derived at apply time from the seed and logical name, never persisted here.
type User struct {
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

// Assignment is a (user, target, role) grant. Project is the logical name of
// the project the role is granted on, or empty for a domain-scoped grant on the
// user's own domain. User and Role reference their logical names.
type Assignment struct {
	User    string `json:"user"`
	Role    string `json:"role"`
	Project string `json:"project,omitempty"`
}

// TokenIssue authenticates as User scoped to Project (both logical names),
// recording the issue latency. It requires a matching project-scoped Assignment
// so the create→assign chain is exercised end to end.
type TokenIssue struct {
	User    string `json:"user"`
	Project string `json:"project"`
}

// Validate checks the plan graph for well-formedness: unique logical names per
// kind, every domain/role/project/user reference resolving, no duplicate
// (user, target, role) grant, and every token issue naming an existing user and
// project backed by a matching project-scoped assignment. It returns an error
// naming the first offending resource.
func (p *Plan) Validate() error {
	domains, err := uniqueNames("domain", names(p.Domains, func(d Domain) string { return d.Name }))
	if err != nil {
		return err
	}
	roles, err := uniqueNames("role", names(p.Roles, func(r Role) string { return r.Name }))
	if err != nil {
		return err
	}

	projects := make(map[string]bool, len(p.Projects))
	for _, pr := range p.Projects {
		if projects[pr.Name] {
			return fmt.Errorf("duplicate project name %q", pr.Name)
		}
		if !domains[pr.Domain] {
			return fmt.Errorf("project %q references unknown domain %q", pr.Name, pr.Domain)
		}
		projects[pr.Name] = true
	}

	users := make(map[string]string, len(p.Users))
	for _, u := range p.Users {
		if _, ok := users[u.Name]; ok {
			return fmt.Errorf("duplicate user name %q", u.Name)
		}
		if !domains[u.Domain] {
			return fmt.Errorf("user %q references unknown domain %q", u.Name, u.Domain)
		}
		users[u.Name] = u.Domain
	}

	// (user, target, role) grants, keyed for both reference-resolution and
	// duplicate detection. A domain-scoped grant uses an empty project.
	grants := make(map[string]bool, len(p.Assignments))
	for _, a := range p.Assignments {
		if _, ok := users[a.User]; !ok {
			return fmt.Errorf("assignment references unknown user %q", a.User)
		}
		if !roles[a.Role] {
			return fmt.Errorf("assignment for user %q references unknown role %q", a.User, a.Role)
		}
		if a.Project != "" && !projects[a.Project] {
			return fmt.Errorf("assignment for user %q references unknown project %q", a.User, a.Project)
		}
		key := grantKey(a.User, a.Project, a.Role)
		if grants[key] {
			return fmt.Errorf("duplicate assignment (user %q, project %q, role %q)", a.User, a.Project, a.Role)
		}
		grants[key] = true
	}

	for _, t := range p.Tokens {
		if _, ok := users[t.User]; !ok {
			return fmt.Errorf("token issue references unknown user %q", t.User)
		}
		if !projects[t.Project] {
			return fmt.Errorf("token issue for user %q references unknown project %q", t.User, t.Project)
		}
		if !p.hasProjectGrant(t.User, t.Project) {
			return fmt.Errorf("token issue for user %q on project %q has no matching project-scoped assignment", t.User, t.Project)
		}
	}

	return nil
}

// hasProjectGrant reports whether user has any project-scoped assignment on
// project, the precondition a token issue depends on to obtain a scoped token.
func (p *Plan) hasProjectGrant(user, project string) bool {
	for _, a := range p.Assignments {
		if a.User == user && a.Project == project {
			return true
		}
	}
	return false
}

// DomainScopedAssignments counts the assignments with no project target (a
// grant on the user's domain rather than a project).
func (p *Plan) DomainScopedAssignments() int {
	var n int
	for _, a := range p.Assignments {
		if a.Project == "" {
			n++
		}
	}
	return n
}

// Summary returns a deterministic, human-readable count of the plan, used by
// "keystone apply --dry-run" to preview a scenario without touching a cloud.
func (p *Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan for scenario %q (seed %d)\n", p.Scenario, p.Seed)
	fmt.Fprintf(&b, "  domains:         %d\n", len(p.Domains))
	fmt.Fprintf(&b, "  roles:           %d\n", len(p.Roles))
	fmt.Fprintf(&b, "  projects:        %d\n", len(p.Projects))
	fmt.Fprintf(&b, "  users:           %d\n", len(p.Users))
	fmt.Fprintf(&b, "  assignments:     %d (%d domain-scoped)\n", len(p.Assignments), p.DomainScopedAssignments())
	fmt.Fprintf(&b, "  token issues:    %d\n", len(p.Tokens))
	return b.String()
}

// grantKey renders the deduplication key for a (user, target, role) grant.
func grantKey(user, project, role string) string {
	return user + "\x00" + project + "\x00" + role
}

// names projects a slice of resources to their logical names in order.
func names[T any](items []T, name func(T) string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = name(it)
	}
	return out
}

// uniqueNames builds a presence set from names, returning an error naming the
// kind and the first duplicate.
func uniqueNames(kind string, in []string) (map[string]bool, error) {
	set := make(map[string]bool, len(in))
	for _, n := range in {
		if set[n] {
			return nil, fmt.Errorf("duplicate %s name %q", kind, n)
		}
		set[n] = true
	}
	return set, nil
}
