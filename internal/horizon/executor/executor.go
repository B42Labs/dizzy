// Package executor applies a Horizon plan: it provisions the users and
// projects a plan with users logs in as, then runs every session's journey
// (log in, load the pages, switch project, log out), up to --concurrency at
// a time, and tears a run's users and projects down again. Every step runs
// through WithRetry under the per-operation timeout. A failed step does not
// stop a run: every journey runs to its end, and Apply reports how many
// failed. It drives Horizon and Keystone through the Session and Identity
// seams, which *horizon.Session and *horizon.Client satisfy in production
// and fakes satisfy in tests.
package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/B42Labs/dizzy/internal/horizon"
	"github.com/B42Labs/dizzy/internal/horizon/plan"
	"github.com/B42Labs/dizzy/internal/keystone"
	"github.com/B42Labs/dizzy/internal/resource"
)

// retryBaseDelay, retryMaxDelay and maxAttempts bound the retry of transient
// errors, as in the other executors: five attempts, the backoff doubling
// from 250 ms to at most 5 s. A sentinel (rejected, mismatch, forbidden) is
// never retried.
const (
	retryBaseDelay = 250 * time.Millisecond
	retryMaxDelay  = 5 * time.Second
	maxAttempts    = 5
)

// Session is one dashboard login: the methods of *horizon.Session.
type Session interface {
	Login(ctx context.Context, cred horizon.Credentials) error
	Page(ctx context.Context, p plan.Page) error
	Switch(ctx context.Context, projectID string, landing plan.Page) error
	Logout(ctx context.Context) error
	LoggedIn() bool
	Close()
}

// The production *horizon.Session must satisfy the seam.
var _ Session = (*horizon.Session)(nil)

// Identity creates what a plan with users logs in as: the methods of
// *horizon.Client.
type Identity interface {
	CreateProject(ctx context.Context, p plan.Project, domainID string) (resource.Resource, error)
	CreateUser(ctx context.Context, u plan.User, domainID, password string) (resource.Resource, error)
	AssignRole(ctx context.Context, userID, projectID, roleID string) error
}

// The production *horizon.Client must satisfy the seam.
var _ Identity = (*horizon.Client)(nil)

// Target is what a run logs in against. Open returns a new, logged-out
// session. Identity creates the plan's users and projects; it is nil for a
// plan without users. DomainID is the domain created users live in, and
// DomainName what every login sends as its domain. RoleID is the role each
// created user gets on each created project. Cloud are the credentials of
// the clouds.yaml user, and Projects its enabled project ids, sorted, which
// a session switches between when the plan creates no projects.
type Target struct {
	Open       func() Session
	Identity   Identity
	DomainID   string
	DomainName string
	RoleID     string
	Cloud      horizon.Credentials
	Projects   []string
}

// Provisioned is what Provision created: the users and projects, in the
// order they were created, the credentials of each created user by logical
// name, and the created project ids in plan order. Credentials never leave
// the process.
type Provisioned struct {
	Created     []resource.Resource
	Credentials map[string]horizon.Credentials
	ProjectIDs  []string
}

// Result is the outcome of Apply: the users and projects it created, and how
// many journeys ran and how many of them failed.
type Result struct {
	Created  []resource.Resource
	Journeys int
	Failed   int
}

// Provision creates a plan's projects, then its users, each with a fresh
// random password, then one role assignment per user and project, each
// stage in parallel up to concurrency and each call through WithRetry under
// opTimeout. ErrForbidden is not retried. The first error stops it and is
// returned with what was created so far. A plan without users creates
// nothing.
func Provision(ctx context.Context, t Target, p *plan.Plan, concurrency int, opTimeout time.Duration) (*Provisioned, error) {
	prov := &Provisioned{Credentials: map[string]horizon.Credentials{}}
	if len(p.Users) == 0 {
		return prov, nil
	}
	if t.Identity == nil {
		return prov, errors.New("the plan creates users, but the target has no identity client")
	}

	projects, err := runStage(ctx, p.Projects, concurrency, func(ctx context.Context, pr plan.Project) (resource.Resource, error) {
		return createWithRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
			return t.Identity.CreateProject(ctx, pr, t.DomainID)
		})
	})
	prov.Created = appendCreated(prov.Created, projects)
	if err != nil {
		return prov, err
	}
	for _, r := range projects {
		prov.ProjectIDs = append(prov.ProjectIDs, r.ID)
	}

	passwords := make([]string, len(p.Users))
	for i := range p.Users {
		pw, err := keystone.RandomPassword()
		if err != nil {
			return prov, err
		}
		passwords[i] = pw
	}
	type indexed struct {
		i int
		u plan.User
	}
	todo := make([]indexed, len(p.Users))
	for i, u := range p.Users {
		todo[i] = indexed{i, u}
	}
	users, err := runStage(ctx, todo, concurrency, func(ctx context.Context, x indexed) (resource.Resource, error) {
		return createWithRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
			return t.Identity.CreateUser(ctx, x.u, t.DomainID, passwords[x.i])
		})
	})
	prov.Created = appendCreated(prov.Created, users)
	for i, r := range users {
		if r.ID != "" {
			prov.Credentials[p.Users[i].Name] = horizon.Credentials{Username: r.Name, Password: passwords[i], Domain: t.DomainName}
		}
	}
	if err != nil {
		return prov, err
	}

	type pair struct{ user, project string }
	var pairs []pair
	for _, u := range users {
		for _, pid := range prov.ProjectIDs {
			pairs = append(pairs, pair{u.ID, pid})
		}
	}
	_, err = runStage(ctx, pairs, concurrency, func(ctx context.Context, a pair) (struct{}, error) {
		return struct{}{}, WithRetry(ctx, opTimeout, func(ctx context.Context) error {
			return t.Identity.AssignRole(ctx, a.user, a.project, t.RoleID)
		})
	})
	return prov, err
}

// appendCreated appends the resources of a stage that were created, those
// with an id, keeping their order.
func appendCreated(created, stage []resource.Resource) []resource.Resource {
	for _, r := range stage {
		if r.ID != "" {
			created = append(created, r)
		}
	}
	return created
}

// createWithRetry runs a create through WithRetry and returns what the last
// attempt created.
func createWithRetry(ctx context.Context, opTimeout time.Duration, create func(context.Context) (resource.Resource, error)) (resource.Resource, error) {
	var r resource.Resource
	err := WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		var err error
		r, err = create(ctx)
		return err
	})
	return r, err
}

// Apply provisions p and then runs every session's journey through
// RunJourney, up to concurrency at a time. A cancelled context starts no
// further journey. When a journey failed it returns
// "<failed> of <total> journeys failed" wrapping the first failure, and
// when the context was cancelled before every journey started, the
// context's error; Result.Created is filled either way.
func Apply(ctx context.Context, t Target, p *plan.Plan, concurrency int, opTimeout time.Duration) (*Result, error) {
	prov, err := Provision(ctx, t, p, concurrency, opTimeout)
	res := &Result{Created: prov.Created}
	if err != nil {
		return res, fmt.Errorf("provisioning: %w", err)
	}

	if concurrency < 1 {
		concurrency = 1
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		slots = make(chan struct{}, concurrency)
	)
	started := 0
loop:
	for _, s := range p.Sessions {
		select {
		case <-ctx.Done():
			break loop
		case slots <- struct{}{}:
		}
		if ctx.Err() != nil {
			<-slots
			break
		}
		started++
		wg.Add(1)
		go func(s plan.Session) {
			defer wg.Done()
			defer func() { <-slots }()
			err := RunJourney(ctx, t, prov, p, s, opTimeout)
			mu.Lock()
			defer mu.Unlock()
			res.Journeys++
			if err != nil {
				res.Failed++
				if first == nil {
					first = err
				}
			}
		}(s)
	}
	wg.Wait()

	if res.Failed > 0 {
		return res, fmt.Errorf("%d of %d journeys failed: %w", res.Failed, len(p.Sessions), first)
	}
	if started < len(p.Sessions) {
		return res, ctx.Err()
	}
	return res, nil
}

// CredentialsFor returns the credentials session s logs in with: its
// created user's, or the clouds.yaml user's when s names no user.
func CredentialsFor(t Target, prov *Provisioned, s plan.Session) horizon.Credentials {
	if s.User == "" || prov == nil {
		return t.Cloud
	}
	return prov.Credentials[s.User]
}

// RunJourney runs one session's journey: it opens a session and logs in
// with CredentialsFor; a failed login ends the journey. It then browses
// (Browse), logs out whenever the login succeeded, and closes the session.
// After a cancelled context the logout runs once on a context without the
// cancellation, under opTimeout, so no session stays logged in. It returns
// the first error of its steps.
func RunJourney(ctx context.Context, t Target, prov *Provisioned, p *plan.Plan, s plan.Session, opTimeout time.Duration) error {
	sess := t.Open()
	defer sess.Close()

	cred := CredentialsFor(t, prov, s)
	if err := WithRetry(ctx, opTimeout, func(ctx context.Context) error { return sess.Login(ctx, cred) }); err != nil {
		return fmt.Errorf("%s: %w", s.Name, err)
	}

	first := Browse(ctx, sess, t, prov, p, s, opTimeout)
	if err := Logout(ctx, sess, opTimeout); err != nil && first == nil {
		first = err
	}
	if first != nil {
		return fmt.Errorf("%s: %w", s.Name, first)
	}
	return nil
}

// Logout logs a logged-in session out through WithRetry under opTimeout.
// When ctx is cancelled, before or during the logout, it sends one logout on
// a context without the cancellation, under opTimeout. A session that is not
// logged in is left alone.
func Logout(ctx context.Context, sess Session, opTimeout time.Duration) error {
	if !sess.LoggedIn() {
		return nil
	}
	if ctx.Err() == nil {
		err := WithRetry(ctx, opTimeout, sess.Logout)
		if ctx.Err() == nil || !sess.LoggedIn() {
			return err
		}
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()
	return sess.Logout(lctx)
}

// Browse loads session s's pages in order and then, when the plan says so,
// switches project, landing on the plan's first page. The target is
// targets[SwitchDraw mod len(targets)], where targets are the provisioned
// project ids or, without them, the clouds.yaml user's projects. A failed
// step does not stop the later ones; Browse returns the first error after
// every step ran.
func Browse(ctx context.Context, sess Session, t Target, prov *Provisioned, p *plan.Plan, s plan.Session, opTimeout time.Duration) error {
	pages := make(map[string]plan.Page, len(p.Pages))
	for _, pg := range p.Pages {
		pages[pg.Name] = pg
	}
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for _, name := range s.Pages {
		pg := pages[name]
		keep(WithRetry(ctx, opTimeout, func(ctx context.Context) error { return sess.Page(ctx, pg) }))
	}
	if s.Switch {
		targets := t.Projects
		if prov != nil && len(prov.ProjectIDs) > 0 {
			targets = prov.ProjectIDs
		}
		switch {
		case len(targets) == 0:
			keep(errors.New("no project to switch to"))
		case len(p.Pages) == 0:
			keep(errors.New("no page to land on after the switch"))
		default:
			target := targets[s.SwitchDraw%len(targets)]
			keep(WithRetry(ctx, opTimeout, func(ctx context.Context) error { return sess.Switch(ctx, target, p.Pages[0]) }))
		}
	}
	return first
}

// WithRetry runs fn, bounding each attempt by opTimeout, and retries
// transient errors (horizon.IsRetryable) with exponential backoff up to
// maxAttempts. It returns at once on success and on any other error, a
// sentinel included. Backoff sleeps honor ctx.
func WithRetry(ctx context.Context, opTimeout time.Duration, fn func(context.Context) error) error {
	backoff := retryBaseDelay
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		opCtx, cancel := context.WithTimeout(ctx, opTimeout)
		err = fn(opCtx)
		cancel()
		if err == nil || !horizon.IsRetryable(err) || attempt == maxAttempts {
			return err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
		if backoff *= 2; backoff > retryMaxDelay {
			backoff = retryMaxDelay
		}
	}
	return err
}

// runStage runs fn over items with up to concurrency in flight and returns
// the results in item order, a zero value for an item that failed or never
// ran. The first error cancels the items not yet started and is returned; a
// real failure is preferred over the context.Canceled that items still in
// flight return once the stage is cancelled.
func runStage[T, R any](ctx context.Context, items []T, concurrency int, fn func(context.Context, T) (R, error)) ([]R, error) {
	results := make([]R, len(items))
	if len(items) == 0 {
		return results, nil
	}
	if concurrency < 1 {
		concurrency = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make([]error, len(items))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(concurrency, len(items)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				r, err := fn(ctx, items[i])
				results[i], errs[i] = r, err
				if err != nil {
					cancel()
				}
			}
		}()
	}
feed:
	for i := range items {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	var canceled error
	for _, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, context.Canceled):
			canceled = err
		default:
			return results, err
		}
	}
	if canceled != nil {
		return results, canceled
	}
	return results, ctx.Err()
}
