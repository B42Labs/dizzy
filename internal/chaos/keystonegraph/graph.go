// Package keystonegraph builds the Keystone churn graph the service-neutral
// chaos engine schedules. The domains and roles are the stable scaffold —
// provisioned once at start and torn down at end — so they are not nodes; they
// live in the bindings the closures capture. The churning population is
// projects, users, and role assignments: a project node and a user node per
// planned project/user (both parentless), and an assignment node per planned
// grant, parented on its user (and its project, for a project-scoped grant). The
// engine's parents-outlive-children invariant gives the right lifecycle for
// free: an assignment only lives while its user, project, and role live. A token
// issue is modeled as the engine's at-most-once mutate action on the matching
// assignment node — a mutation of a live grant, drawn with the token ratio,
// that records latency without changing the population. Keeping the Keystone
// coupling here leaves the chaos engine free of any service-specific import.
package keystonegraph

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneexec "github.com/B42Labs/dizzy/internal/keystone/executor"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Keystone is the create/assign/issue/delete surface the chaos engine drives
// through the nodes this package builds. It is the consumer-defined
// ports-and-adapters seam to the cloud — *keystone.Client satisfies it in
// production and a fake satisfies it in tests.
type Keystone interface {
	CreateProject(ctx context.Context, p keystoneplan.Project, domainID string) (resource.Resource, error)
	CreateUser(ctx context.Context, u keystoneplan.User, domainID, password string) (resource.Resource, error)
	AssignRole(ctx context.Context, a keystoneplan.Assignment, userID, projectID, domainID, roleID string) (resource.Resource, error)
	IssueToken(ctx context.Context, t keystoneplan.TokenIssue, userDomainID, password, projectID string) error
	Delete(ctx context.Context, r resource.Resource) error
}

// The production *keystone.Client must satisfy the seam.
var _ Keystone = (*keystone.Client)(nil)

// Build turns a Keystone plan into the churn graph over the stable domain/role
// scaffold in b: one project node and one user node per plan project/user (both
// parentless), and one assignment node per grant, parented on its user (and its
// project when project-scoped) so the engine never grants a role on an absent
// user or project and never deletes a user or project with a live grant. An
// assignment whose (user, project) matches a planned token issue carries the
// engine's mutate action, issuing a scoped token as that live, assigned user.
// Every closure captures c and runs through the keystone executor's retry
// policy, bounded by opTimeout. The plan is validated first so a dangling
// reference fails loudly instead of yielding a node that can never be created.
func Build(p *keystoneplan.Plan, c Keystone, b keystoneexec.Bindings, opTimeout time.Duration) ([]chaos.Node, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid plan: %w", err)
	}

	// The user's domain cloud id, for domain-scoped grants and token auth.
	userDomainID := make(map[string]string, len(p.Users))
	for _, u := range p.Users {
		userDomainID[u.Name] = b.Domains[u.Domain]
	}

	// A fresh random password per user, generated once and captured by the user's
	// Create closure and the matching token-issue Mutate closure, so every
	// incarnation of a churned user and its token issue share one credential. It
	// is never derived from the user's cloud-visible name or the seed, so a
	// leftover user's password cannot be recomputed.
	passwords := make(map[string]string, len(p.Users))
	for _, u := range p.Users {
		pw, err := keystone.RandomPassword()
		if err != nil {
			return nil, fmt.Errorf("generating password for user %q: %w", u.Name, err)
		}
		passwords[u.Name] = pw
	}

	// The (user, project) pairs that issue a token: the assignments those match
	// carry the mutate action.
	tokenPair := make(map[string]bool, len(p.Tokens))
	for _, t := range p.Tokens {
		tokenPair[t.User+"\x00"+t.Project] = true
	}

	var nodes []chaos.Node

	for i := range p.Projects {
		pr := p.Projects[i]
		domainID := b.Domains[pr.Domain]
		nodes = append(nodes, chaos.Node{
			Key: pr.Name, Kind: keystone.KindProject,
			Create: func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
				return createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
					return c.CreateProject(ctx, pr, domainID)
				})
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteRetry(ctx, opTimeout, c, res)
			},
		})
	}

	for i := range p.Users {
		u := p.Users[i]
		domainID := b.Domains[u.Domain]
		password := passwords[u.Name]
		nodes = append(nodes, chaos.Node{
			Key: u.Name, Kind: keystone.KindUser,
			Create: func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
				return createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
					return c.CreateUser(ctx, u, domainID, password)
				})
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteRetry(ctx, opTimeout, c, res)
			},
		})
	}

	for i := range p.Assignments {
		a := p.Assignments[i]
		roleID := b.Roles[a.Role]
		domainID := userDomainID[a.User]
		parents := []string{a.User}
		if a.Project != "" {
			parents = append(parents, a.Project)
		}
		node := chaos.Node{
			Key: fmt.Sprintf("assign-%04d", i+1), Kind: keystone.KindAssignment, Parents: parents,
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				projectID := ""
				if a.Project != "" {
					projectID = ids[a.Project]
				}
				return createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
					return c.AssignRole(ctx, a, ids[a.User], projectID, domainID, roleID)
				})
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteRetry(ctx, opTimeout, c, res)
			},
		}
		// An assignment that backs a planned token issue can be mutated: issue a
		// scoped token as the live, assigned user, at most once per grant lifetime.
		if a.Project != "" && tokenPair[a.User+"\x00"+a.Project] {
			issue := keystoneplan.TokenIssue{User: a.User, Project: a.Project}
			password := passwords[a.User]
			node.Mutate = func(ctx context.Context, ids map[string]string, _ resource.Resource) error {
				return keystoneexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
					return c.IssueToken(ctx, issue, domainID, password, ids[a.Project])
				})
			}
		}
		nodes = append(nodes, node)
	}

	return nodes, nil
}

// createRetry runs a create through the retry policy, returning the created
// resource. Keystone creates are synchronous, so there is no readiness fold.
func createRetry(ctx context.Context, opTimeout time.Duration, create func(context.Context) (resource.Resource, error)) (resource.Resource, error) {
	var res resource.Resource
	err := keystoneexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		r, err := create(ctx)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return resource.Resource{}, err
	}
	return res, nil
}

// deleteRetry deletes res through the retry policy, folding an already-gone
// (404) into success. Keystone deletes are synchronous, so there is no
// wait-for-gone.
func deleteRetry(ctx context.Context, opTimeout time.Duration, c Keystone, res resource.Resource) error {
	err := keystoneexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		return c.Delete(ctx, res)
	})
	if err != nil && !keystone.IsNotFound(err) {
		return err
	}
	return nil
}

// Classify labels an operation error for the churn engine's per-bucket error
// breakdown, reusing the keystone classification helpers so the labels match the
// kinds operators already see in the metrics report. It is wired into the engine
// via chaos.Config.Classify. forbidden takes the slot Cinder's quota label uses.
func Classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case keystone.IsForbidden(err):
		return "forbidden"
	case keystone.IsNotFound(err):
		return "not-found"
	case keystone.IsRetryable(err):
		return "transient"
	default:
		return "other"
	}
}
