// Package executor turns a Keystone plan into real domains, roles, projects,
// users, role assignments, and issued tokens. It runs strictly ordered stages —
// bind the roots (create domains and roles in admin mode; map onto the in-scope
// domain and reused roles in domain-manager mode), create projects and users
// concurrently, grant role assignments, then issue tokens — with independent
// work within a stage run concurrently up to a configurable limit, transient
// failures retried with exponential backoff, a 403 policy denial failing fast
// (the privilege backstop), and per-operation timeouts. Keystone creates are
// synchronous, so unlike the Neutron and Cinder executors there is no
// status-polling / time-to-ready stage. The created resources and the timing the
// Keystone wrappers record are the hand-off surface a later run record and
// cleanup consume.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/B42Labs/openstack-tester/internal/keystone"
	"github.com/B42Labs/openstack-tester/internal/keystone/plan"
	"github.com/B42Labs/openstack-tester/internal/resource"
)

// retryBaseDelay, retryMaxDelay, and maxAttempts bound the per-operation retry
// of transient errors. They duplicate the Neutron/Cinder policy rather than
// sharing it: this WithRetry uses the keystone classifiers (a 403 fails fast, a
// 409 is terminal) and backs both the apply path and the keystone chaos graph.
const (
	retryBaseDelay = 250 * time.Millisecond
	retryMaxDelay  = 5 * time.Second
	maxAttempts    = 5
)

// Keystone is the create/assign/issue surface the executor drives. It is the
// single ports-and-adapters seam to the cloud: *keystone.Client satisfies it in
// production and a fake satisfies it in tests.
type Keystone interface {
	CreateDomain(ctx context.Context, d plan.Domain) (resource.Resource, error)
	CreateRole(ctx context.Context, r plan.Role) (resource.Resource, error)
	CreateProject(ctx context.Context, p plan.Project, domainID string) (resource.Resource, error)
	CreateUser(ctx context.Context, u plan.User, domainID, password string) (resource.Resource, error)
	AssignRole(ctx context.Context, a plan.Assignment, userID, projectID, domainID, roleID string) (resource.Resource, error)
	IssueToken(ctx context.Context, t plan.TokenIssue, userDomainID, password, projectID string) error
}

// The production *keystone.Client must satisfy the seam.
var _ Keystone = (*keystone.Client)(nil)

// Bindings maps a plan's logical domain and role names to their cloud ids: the
// created roots in admin mode, the in-scope domain and reused roles in
// domain-manager mode.
type Bindings struct {
	Domains map[string]string
	Roles   map[string]string
}

// Result is the outcome of an apply: every resource that was created, in
// dependency order (roots, then projects and users, then assignments). Tokens
// create no resource and never appear here.
type Result struct {
	Created []resource.Resource
}

// BindRoots resolves the plan's domains and roles to cloud ids. In admin mode it
// creates the domains then the roles (two ordered stages) and returns them as
// created resources. In domain-manager mode it writes nothing: every logical
// domain binds onto the single in-scope domain and logical role i binds onto the
// reused role i mod len(reused), so nothing reused enters the created list. The
// created roots are returned so the caller can record and later tear them down.
func BindRoots(ctx context.Context, c Keystone, p *plan.Plan, tier keystone.Tier, res keystone.Resolution, concurrency int, opTimeout time.Duration) (Bindings, []resource.Resource, error) {
	b := Bindings{
		Domains: make(map[string]string, len(p.Domains)),
		Roles:   make(map[string]string, len(p.Roles)),
	}

	if tier != keystone.TierAdmin {
		// Domain-manager mode: pure mapping, no writes. Reused roots never enter
		// the created list, so cleanup can never delete them.
		for _, d := range p.Domains {
			b.Domains[d.Name] = res.DomainID
		}
		for i, r := range p.Roles {
			if len(res.RoleIDs) == 0 {
				break
			}
			b.Roles[r.Name] = res.RoleIDs[i%len(res.RoleIDs)]
		}
		return b, nil, nil
	}

	e := &applier{c: c, opTimeout: opTimeout}
	var created []resource.Resource

	domRes, err := runStage(ctx, p.Domains, concurrency, func(ctx context.Context, d plan.Domain) (resource.Resource, error) {
		return e.create(ctx, func(ctx context.Context) (resource.Resource, error) { return c.CreateDomain(ctx, d) })
	})
	created = appendCreated(created, domRes)
	for i, d := range p.Domains {
		if domRes[i].ID != "" {
			b.Domains[d.Name] = domRes[i].ID
		}
	}
	if err != nil {
		return b, created, err
	}

	roleRes, err := runStage(ctx, p.Roles, concurrency, func(ctx context.Context, r plan.Role) (resource.Resource, error) {
		return e.create(ctx, func(ctx context.Context) (resource.Resource, error) { return c.CreateRole(ctx, r) })
	})
	created = appendCreated(created, roleRes)
	for i, r := range p.Roles {
		if roleRes[i].ID != "" {
			b.Roles[r.Name] = roleRes[i].ID
		}
	}
	if err != nil {
		return b, created, err
	}

	return b, created, nil
}

// Apply binds the plan's roots, creates its projects and users concurrently,
// grants its role assignments, and issues its tokens, in strictly ordered
// stages. Within a stage independent work runs concurrently up to concurrency,
// each operation retried on transient errors and bounded by opTimeout. A 403
// policy denial, or any non-retryable error, stops the run and is returned along
// with the resources created so far; ctx cancellation returns ctx.Err(). tier
// and res select admin (create roots) or domain-manager (bind roots) mode.
func Apply(ctx context.Context, c Keystone, p *plan.Plan, tier keystone.Tier, res keystone.Resolution, concurrency int, opTimeout time.Duration) (*Result, error) {
	e := &applier{c: c, opTimeout: opTimeout}
	result := &Result{}

	// Stage 1: bind the roots (create in admin mode, map in domain-manager mode).
	bindings, roots, err := BindRoots(ctx, c, p, tier, res, concurrency, opTimeout)
	result.Created = append(result.Created, roots...)
	if err != nil {
		return result, err
	}

	// passwords holds a fresh random password per user, generated once here
	// (single-threaded) and only read within the concurrent stages that create
	// the users and later issue their tokens — like resByLogical and userDomain
	// below — so no lock is needed. It is never derived from the user's
	// cloud-visible name or the seed, and never persisted.
	passwords := make(map[string]string, len(p.Users))
	for _, u := range p.Users {
		pw, err := keystone.RandomPassword()
		if err != nil {
			return result, fmt.Errorf("generating password for user %q: %w", u.Name, err)
		}
		passwords[u.Name] = pw
	}

	// Stage 2: create projects and users concurrently in one pool.
	children := make([]rootChild, 0, len(p.Projects)+len(p.Users))
	for i := range p.Projects {
		children = append(children, rootChild{project: &p.Projects[i]})
	}
	for i := range p.Users {
		children = append(children, rootChild{user: &p.Users[i]})
	}
	childRes, err := runStage(ctx, children, concurrency, func(ctx context.Context, ch rootChild) (resource.Resource, error) {
		return e.provisionChild(ctx, ch, bindings, passwords)
	})
	result.Created = appendCreated(result.Created, childRes)

	// resByLogical maps a project/user logical name to its created resource,
	// built between stages (single-threaded) and read concurrently within the
	// next stages, so no lock is needed.
	resByLogical := make(map[string]resource.Resource, len(children))
	for i, ch := range children {
		if childRes[i].ID == "" {
			continue
		}
		resByLogical[ch.logical()] = childRes[i]
	}
	if err != nil {
		return result, err
	}

	// userDomain maps a user's logical name to its domain's cloud id, for
	// domain-scoped grants and token issues.
	userDomain := make(map[string]string, len(p.Users))
	for _, u := range p.Users {
		userDomain[u.Name] = bindings.Domains[u.Domain]
	}

	// Stage 3: grant each (user, target, role) assignment.
	assignRes, err := runStage(ctx, p.Assignments, concurrency, func(ctx context.Context, a plan.Assignment) (resource.Resource, error) {
		projectID := ""
		if a.Project != "" {
			projectID = resByLogical[a.Project].ID
		}
		return e.create(ctx, func(ctx context.Context) (resource.Resource, error) {
			return c.AssignRole(ctx, a, resByLogical[a.User].ID, projectID, userDomain[a.User], bindings.Roles[a.Role])
		})
	})
	result.Created = appendCreated(result.Created, assignRes)
	if err != nil {
		return result, err
	}

	// Stage 4: issue a token for each selected user, scoped to its planned
	// project. A failed issue is a failed operation and fails the run — the point
	// of including it as an end-to-end consistency check. Tokens create no
	// resource.
	_, err = runStage(ctx, p.Tokens, concurrency, func(ctx context.Context, t plan.TokenIssue) (resource.Resource, error) {
		return resource.Resource{}, e.issueToken(ctx, t, userDomain[t.User], passwords[t.User], resByLogical[t.Project].ID)
	})
	if err != nil {
		return result, err
	}

	return result, nil
}

// applier carries the apply-wide configuration shared by the stage helpers.
type applier struct {
	c         Keystone
	opTimeout time.Duration
}

// rootChild is one stage-2 item: exactly one of a project or a user, so both
// kinds share a single concurrent pool.
type rootChild struct {
	project *plan.Project
	user    *plan.User
}

// logical returns the child's logical plan name.
func (ch rootChild) logical() string {
	if ch.project != nil {
		return ch.project.Name
	}
	return ch.user.Name
}

// create runs a create through the retry policy and logs the created resource.
func (e *applier) create(ctx context.Context, fn func(context.Context) (resource.Resource, error)) (resource.Resource, error) {
	var res resource.Resource
	err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
		r, err := fn(ctx)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return resource.Resource{}, err
	}
	slog.Info("created resource", "kind", res.Kind, "logical", res.Logical, "id", res.ID)
	return res, nil
}

// provisionChild creates one project or user (with its generated password) in
// its bound domain.
func (e *applier) provisionChild(ctx context.Context, ch rootChild, b Bindings, passwords map[string]string) (resource.Resource, error) {
	if ch.project != nil {
		p := *ch.project
		return e.create(ctx, func(ctx context.Context) (resource.Resource, error) {
			return e.c.CreateProject(ctx, p, b.Domains[p.Domain])
		})
	}
	u := *ch.user
	password := passwords[u.Name]
	return e.create(ctx, func(ctx context.Context) (resource.Resource, error) {
		return e.c.CreateUser(ctx, u, b.Domains[u.Domain], password)
	})
}

// issueToken authenticates as the user and obtains a scoped token, through the
// retry policy. A non-retryable failure (e.g. the assignment did not really take
// and the scope is denied) fails the run.
func (e *applier) issueToken(ctx context.Context, t plan.TokenIssue, userDomainID, password, projectID string) error {
	err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
		return e.c.IssueToken(ctx, t, userDomainID, password, projectID)
	})
	if err != nil {
		return err
	}
	slog.Info("issued token", "user", t.User, "project", t.Project)
	return nil
}

// appendCreated appends the populated resources from a stage to dst, skipping
// the zero Resource{} slots a partially-failed stage leaves for items that
// failed or were never dispatched (identified by an empty ID). It keeps the run
// record honest about what actually exists when a stage fails partway.
func appendCreated(dst, stageRes []resource.Resource) []resource.Resource {
	for _, r := range stageRes {
		if r.ID != "" {
			dst = append(dst, r)
		}
	}
	return dst
}

// runStage runs work over items using a fixed pool of at most concurrency
// workers reading from a job channel — a bounded pool rather than one goroutine
// per item, so a large plan cannot exhaust resources. Results are returned in
// item order (populated even for a failing item, so a caller can record what
// already exists). The first error cancels the stage, stops dispatching, and is
// returned, with a 403 policy denial (ErrForbidden) taking priority and a
// parent-context cancellation reported as ctx.Err().
func runStage[T, R any](ctx context.Context, items []T, concurrency int, work func(context.Context, T) (R, error)) ([]R, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stageCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := concurrency
	if workers < 1 {
		workers = 1
	}
	if workers > len(items) {
		workers = len(items)
	}

	results := make([]R, len(items))
	errs := make([]error, len(items))
	jobs := make(chan int)

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				res, err := work(stageCtx, items[i])
				results[i] = res
				if err != nil {
					errs[i] = err
					cancel()
				}
			}
		}()
	}

dispatch:
	for i := range items {
		select {
		case jobs <- i:
		case <-stageCtx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()

	for _, err := range errs {
		if errors.Is(err, keystone.ErrForbidden) {
			return results, err
		}
	}
	if err := ctx.Err(); err != nil {
		return results, err
	}
	for _, err := range errs {
		if err != nil {
			return results, err
		}
	}
	return results, nil
}

// WithRetry runs fn, bounding each attempt with opTimeout, and retries transient
// errors with exponential backoff up to maxAttempts. It returns immediately on
// success, on a 403 policy denial (so the run fails fast), or on any
// non-retryable error (including a 409 conflict, terminal for Keystone). Backoff
// sleeps honor the parent context. It is exported so the keystone chaos graph
// drives its create/delete operations through the same transient/backoff policy
// the apply path uses.
func WithRetry(ctx context.Context, opTimeout time.Duration, fn func(context.Context) error) error {
	backoff := retryBaseDelay
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		opCtx, cancel := context.WithTimeout(ctx, opTimeout)
		err = fn(opCtx)
		cancel()

		switch {
		case err == nil:
			return nil
		case errors.Is(err, keystone.ErrForbidden):
			return err
		case !keystone.IsRetryable(err):
			return err
		case attempt == maxAttempts:
			return err
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff *= 2; backoff > retryMaxDelay {
			backoff = retryMaxDelay
		}
	}
	return err
}
