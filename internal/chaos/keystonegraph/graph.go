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
// that records latency without changing the population. Application
// credentials and trusts churn too: a credential node is parented on its user,
// its project and the assignment node of its grant, and a trust node on its
// trustor, its trustee, its project and the assignment node of the trustor's
// grant, so neither outlives the grant Keystone needs to authenticate with it.
// Each is created and deleted as its owner and carries a mutate action that
// authenticates with it, drawn with the token ratio like the token issue.
// Keeping the Keystone coupling here leaves the chaos engine free of any
// service-specific import.
package keystonegraph

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneexec "github.com/B42Labs/dizzy/internal/keystone/executor"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Keystone is the create/assign/issue/authenticate/delete surface the chaos
// engine drives through the nodes this package builds. It is the
// consumer-defined ports-and-adapters seam to the cloud — *keystone.Client
// satisfies it in production and a fake satisfies it in tests.
type Keystone interface {
	CreateProject(ctx context.Context, p keystoneplan.Project, domainID string) (resource.Resource, error)
	CreateUser(ctx context.Context, u keystoneplan.User, domainID, password string) (resource.Resource, error)
	AssignRole(ctx context.Context, a keystoneplan.Assignment, userID, projectID, domainID, roleID string) (resource.Resource, error)
	IssueToken(ctx context.Context, t keystoneplan.TokenIssue, userDomainID, password, projectID string) error
	Delete(ctx context.Context, r resource.Resource) error

	keystoneexec.Delegations
	DeleteApplicationCredential(ctx context.Context, ac keystoneplan.ApplicationCredential, res resource.Resource, userDomainID, password, projectID string) error
	DeleteTrust(ctx context.Context, t keystoneplan.Trust, res resource.Resource, trustorDomainID, trustorPassword, projectID string) error
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
// After the assignments come one node per application credential and one per
// trust, each parented on the assignment node of its grant (see the package
// comment) and mutated by authenticating with it.
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

	// A fresh random secret per application credential, generated once and
	// captured by the credential's Create and Mutate closures, so every
	// incarnation of a churned credential is created and authenticated with one
	// secret. Like the passwords it is never derived and never persisted.
	secrets := make(map[string]string, len(p.ApplicationCredentials))
	for _, ac := range p.ApplicationCredentials {
		secret, err := keystone.RandomPassword()
		if err != nil {
			return nil, fmt.Errorf("generating secret for application credential %q: %w", ac.Name, err)
		}
		secrets[ac.Name] = secret
	}

	// The assignment node key of each (user, project, role) grant, the parent
	// that keeps a credential or trust on a live grant.
	grantNode := make(map[string]string, len(p.Assignments))
	for i, a := range p.Assignments {
		grantNode[a.User+"\x00"+a.Project+"\x00"+a.Role] = assignKey(i)
	}

	live := &liveInstances{ids: make(map[string]string, len(p.Projects)+len(p.Users))}

	var nodes []chaos.Node

	for i := range p.Projects {
		pr := p.Projects[i]
		domainID := b.Domains[pr.Domain]
		nodes = append(nodes, chaos.Node{
			Key: pr.Name, Kind: keystone.KindProject,
			Create: func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
				return live.created(pr.Name)(createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
					return c.CreateProject(ctx, pr, domainID)
				}))
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return live.deleted(pr.Name, res, deleteRetry(ctx, opTimeout, c, res))
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
				return live.created(u.Name)(createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
					return c.CreateUser(ctx, u, domainID, password)
				}))
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return live.deleted(u.Name, res, deleteRetry(ctx, opTimeout, c, res))
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
			Key: assignKey(i), Kind: keystone.KindAssignment, Parents: parents,
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

	for i := range p.ApplicationCredentials {
		ac := p.ApplicationCredentials[i]
		domainID := userDomainID[ac.User]
		password := passwords[ac.User]
		roleID := b.Roles[ac.Role]
		secret := secrets[ac.Name]
		nodes = append(nodes, chaos.Node{
			Key: ac.Name, Kind: keystone.KindApplicationCredential,
			Parents: []string{ac.User, ac.Project, grantNode[ac.User+"\x00"+ac.Project+"\x00"+ac.Role]},
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
					return c.CreateApplicationCredential(ctx, ac, ids[ac.User], domainID, password, ids[ac.Project], roleID, secret)
				})
			},
			Mutate: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return keystoneexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
					return c.AuthenticateApplicationCredential(ctx, ac, res, secret)
				})
			},
			Delete: func(ctx context.Context, ids map[string]string, res resource.Resource) error {
				if !live.current(ids, ac.User, ac.Project) {
					return nil // gone with its grant or user; see liveInstances
				}
				return goneOK(keystoneexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
					return c.DeleteApplicationCredential(ctx, ac, res, domainID, password, ids[ac.Project])
				}))
			},
		})
	}

	for i := range p.Trusts {
		t := p.Trusts[i]
		trustorDomainID, trusteeDomainID := userDomainID[t.Trustor], userDomainID[t.Trustee]
		trustorPassword, trusteePassword := passwords[t.Trustor], passwords[t.Trustee]
		roleID := b.Roles[t.Role]
		nodes = append(nodes, chaos.Node{
			Key: t.Name, Kind: keystone.KindTrust,
			Parents: []string{t.Trustor, t.Trustee, t.Project, grantNode[t.Trustor+"\x00"+t.Project+"\x00"+t.Role]},
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
					return c.CreateTrust(ctx, t, ids[t.Trustor], trustorDomainID, trustorPassword, ids[t.Trustee], ids[t.Project], roleID)
				})
			},
			Mutate: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return keystoneexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
					return c.AuthenticateTrust(ctx, t, res, trusteeDomainID, trusteePassword)
				})
			},
			Delete: func(ctx context.Context, ids map[string]string, res resource.Resource) error {
				if !live.current(ids, t.Trustor, t.Trustee, t.Project) {
					return nil // gone with its users or out of its trustor's reach; see liveInstances
				}
				return goneOK(keystoneexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
					return c.DeleteTrust(ctx, t, res, trustorDomainID, trustorPassword, ids[t.Project])
				}))
			},
		})
	}

	return nodes, nil
}

// liveInstances records the cloud id of the current instance of each user and
// project node, so the delete of an application credential or trust can tell
// whether the parents it was created under still exist. The engine retries a
// failed delete before the node's next create, with the parent ids of the
// first attempt, and by then a parent may have been churned. Such a retry
// cannot succeed as the owner: a token of a re-created user of the same name
// is another user's (Keystone answers 403), and a token scoped to a deleted
// project is refused (401). It has nothing left to do either. Keystone deletes
// a user's application credentials on a project when the user loses its role
// there, which the engine does before it deletes the user or the project, and
// deletes every trust of a user when it deletes the user. A trust whose
// project alone was deleted stays in Keystone, out of its trustor's reach,
// until its users are deleted, which the teardown does. So a delete whose
// parents are not all current counts as done without a request.
type liveInstances struct {
	mu  sync.Mutex
	ids map[string]string // node key -> cloud id of its current instance
}

// created returns a function that records a successful create of the node key
// and passes the create's result through, so a Create closure can wrap its
// createRetry call.
func (l *liveInstances) created(key string) func(resource.Resource, error) (resource.Resource, error) {
	return func(res resource.Resource, err error) (resource.Resource, error) {
		if err == nil && res.ID != "" {
			l.mu.Lock()
			l.ids[key] = res.ID
			l.mu.Unlock()
		}
		return res, err
	}
}

// deleted forgets the instance res of the node key once its delete succeeded
// (err is nil, a 404 included), and passes err through.
func (l *liveInstances) deleted(key string, res resource.Resource, err error) error {
	if err == nil {
		l.mu.Lock()
		if l.ids[key] == res.ID {
			delete(l.ids, key)
		}
		l.mu.Unlock()
	}
	return err
}

// current reports whether every parent key names, in ids, the current
// instance of that parent.
func (l *liveInstances) current(ids map[string]string, keys ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if id := ids[k]; id == "" || l.ids[k] != id {
			return false
		}
	}
	return true
}

// assignKey is the node key of the plan's i-th (0-based) assignment.
func assignKey(i int) string {
	return fmt.Sprintf("assign-%04d", i+1)
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
	return goneOK(keystoneexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		return c.Delete(ctx, res)
	}))
}

// goneOK folds an already-gone (404) delete error into success.
func goneOK(err error) error {
	if err != nil && !keystone.IsNotFound(err) {
		return err
	}
	return nil
}

// Classify labels an operation error for the churn engine's per-bucket error
// breakdown with keystone.ClassifyOperationError, the classifier the pressure
// operations share, so a Keystone failure carries the same label in either
// report. It is wired into the engine via chaos.Config.Classify. forbidden
// takes the slot Cinder's quota label uses.
func Classify(err error) string {
	return keystone.ClassifyOperationError(err)
}
