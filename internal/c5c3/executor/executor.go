// Package executor turns a c5c3 plan into orders the c5c3-operator fulfils and
// checks in Keystone what it made of them. It runs seven strictly ordered
// stages: order the user and the project and log in as the user; order the two
// refusals and wait for the reasons the operator must report; order the role
// assignment and the catalog entry and find both in a scoped token; order the
// application credential and log in with it; edit and delete the delivered
// Secrets and wait for the operator to restore them; rotate the user's
// password; and rotate the application credential while a prober keeps logging
// in with it. The creates within a stage run concurrently up to a
// configurable limit; single requests are retried on transient errors with
// exponential backoff, each bounded by its own timeout, and the waits for the
// operator are bounded by the plan's timeouts. A refused order, a mismatch and
// an accepted credential are never retried, so each fails the run. The created
// orders are the hand-off surface a later run record and cleanup consume.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/B42Labs/dizzy/internal/c5c3"
	"github.com/B42Labs/dizzy/internal/c5c3/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// retryBaseDelay, retryMaxDelay, and maxAttempts bound the per-operation retry
// of transient errors. They duplicate the other executors' policy rather than
// sharing it: each package's WithRetry hardcodes its own service's classifier,
// and this one uses c5c3.IsRetryable, under which a refusal, a mismatch and an
// accepted credential are never retried.
const (
	retryBaseDelay = 250 * time.Millisecond
	retryMaxDelay  = 5 * time.Second
	maxAttempts    = 5
)

// graceMargin is how far ahead the superseded application credential's
// deletion must lie for stage 7 to log in with it once more. A deletion closer
// than that could fall between the status read and the login, and the login
// would then fail for a reason that is not the operator's.
const graceMargin = 5 * time.Second

// proberInterval is the interval at which stage 7's prober logs in with the
// application credential's delivered Secret. It is a variable so tests can
// shorten it.
var proberInterval = 5 * time.Second

// Orders is the order, Secret and Keystone surface the executor drives. It is
// the single ports-and-adapters seam to the cluster and to Keystone:
// *c5c3.Client satisfies it in production and a fake satisfies it in tests.
type Orders interface {
	CreateOrder(ctx context.Context, o plan.Order) (resource.Resource, error)
	WaitReady(ctx context.Context, r resource.Resource) error
	WaitReason(ctx context.Context, r resource.Resource, reason string) error
	WaitGone(ctx context.Context, r resource.Resource) error
	Delete(ctx context.Context, r resource.Resource) error
	Status(ctx context.Context, r resource.Resource) (c5c3.OrderStatus, error)
	RaiseGeneration(ctx context.Context, r resource.Resource) (int64, error)
	WaitRotated(ctx context.Context, r resource.Resource, generation int64, previous c5c3.Credential) (c5c3.Credential, error)
	WaitPreviousGone(ctx context.Context, r resource.Resource) error
	ReadCredential(ctx context.Context, order resource.Resource) (c5c3.Credential, error)
	EditSecret(ctx context.Context, order resource.Resource) error
	DeleteSecret(ctx context.Context, order resource.Resource) error
	WaitSecretRestored(ctx context.Context, order resource.Resource, want c5c3.Credential) error
	Authenticate(ctx context.Context, kind resource.Kind, cred c5c3.Credential, wantProject string) error
	VerifyRole(ctx context.Context, cred c5c3.Credential, projectName, domainName, role string) error
	VerifyCatalog(ctx context.Context, cred c5c3.Credential, projectName, domainName, serviceType, serviceName, endpointURL string) error
	Reject(ctx context.Context, kind resource.Kind, cred c5c3.Credential) error
}

// Result is the outcome of an apply: every order Apply created and did not
// delete itself, in creation order. A full run holds the user, the project, the
// role assignment, the catalog entry and the application credential. A refusal
// is held from its create until its wait for the order to be gone succeeds, so
// a run that fails in stage 2 still records it for cleanup.
type Result struct {
	Created []resource.Resource
}

// layout is the plan's five orders, each found by its kind. The role
// assignment and the application credential reference the user and the
// project by logical name.
type layout struct {
	user, project, role, catalog, appcred plan.Order
}

// resolve finds the five orders Apply needs in p.Orders, one of each kind, and
// checks that the role assignment and the application credential reference
// the user and the project among them. It returns an error naming the first
// problem it finds.
func resolve(p *plan.Plan) (layout, error) {
	var l layout
	for _, want := range []struct {
		kind string
		dst  *plan.Order
	}{
		{plan.KindUser, &l.user},
		{plan.KindProject, &l.project},
		{plan.KindRoleAssignment, &l.role},
		{plan.KindCatalogEntry, &l.catalog},
		{plan.KindApplicationCredential, &l.appcred},
	} {
		n := 0
		for _, o := range p.Orders {
			if o.Kind == want.kind {
				*want.dst = o
				n++
			}
		}
		if n != 1 {
			return layout{}, fmt.Errorf("plan holds %d %s orders, want 1", n, want.kind)
		}
	}
	for _, o := range []plan.Order{l.role, l.appcred} {
		if o.User != l.user.Name || o.Project != l.project.Name {
			return layout{}, fmt.Errorf("order %q references user %q and project %q, want %q and %q",
				o.Name, o.User, o.Project, l.user.Name, l.project.Name)
		}
	}
	return l, nil
}

// Apply orders the plan's user, project, role assignment, catalog entry and
// application credential and its refusals, and checks each in Keystone
// with the credentials the operator delivers, in the seven strictly ordered
// stages of the package comment. The creates of a stage run concurrently up to
// concurrency. Every single request is bounded by opTimeout and retried on
// transient errors, except VerifyCatalog and Reject, which run once under
// opTimeout. A wait for an order to be gone is bounded by p.GoneTimeout, the
// wait for the superseded application credential to go by p.GracePeriod plus
// p.GoneTimeout, and every other wait for the operator by p.ReadyTimeout.
//
// The first error stops the run and is returned, wrapped with the stage it
// failed in, together with the orders created so far; cancelling ctx returns
// an error wrapping ctx.Err(). A plan without exactly one order of each kind,
// or whose role assignment or application credential references another user
// or project, is refused before the first request.
func Apply(ctx context.Context, c Orders, p *plan.Plan, concurrency int, opTimeout time.Duration) (*Result, error) {
	l, err := resolve(p)
	if err != nil {
		return &Result{}, err
	}
	e := &applier{c: c, p: p, l: l, concurrency: concurrency, opTimeout: opTimeout}

	stages := []struct {
		name string
		run  func(context.Context) error
		skip bool
	}{
		{name: "user and project", run: e.userAndProject},
		{name: "refusals", run: e.refusals},
		{name: "role and catalog", run: e.roleAndCatalog},
		{name: "application credential", run: e.applicationCredential},
		{name: "secret repair", run: e.secretRepair, skip: !p.SecretRepair},
		{name: "password rotation", run: e.passwordRotation},
		{name: "application credential rotation", run: e.credentialRotation},
	}
	for i, s := range stages {
		if s.skip {
			slog.Info("skipping stage", "stage", i+1, "name", s.name)
			continue
		}
		slog.Info("starting stage", "stage", i+1, "name", s.name)
		if err := s.run(ctx); err != nil {
			// Report an interrupt as one even when the call it cut short answered
			// with something else, as runStage does.
			if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
				err = ctxErr
			}
			return &Result{Created: e.created}, fmt.Errorf("stage %d (%s): %w", i+1, s.name, err)
		}
	}
	return &Result{Created: e.created}, nil
}

// applier carries the apply-wide configuration and what each stage hands the
// next: the created orders, the credentials read from their Secrets and the
// project's Keystone names.
type applier struct {
	c           Orders
	p           *plan.Plan
	l           layout
	concurrency int
	opTimeout   time.Duration

	// created is Result.Created as the run goes. Only the goroutine running the
	// stages touches it.
	created []resource.Resource

	user, project, role, catalog, appcred resource.Resource

	// userCred and appcredCred are the credentials last read from the user's
	// and the application credential's Secrets.
	userCred, appcredCred c5c3.Credential

	// projectName and domainName are the Keystone names of the project order,
	// read from its status in stage 3.
	projectName, domainName string
}

// userAndProject is stage 1: it creates the user and the project in parallel,
// waits for each to turn ready, reads the user's delivered credential and logs
// in with it unscoped.
func (e *applier) userAndProject(ctx context.Context) error {
	res, err := runStage(ctx, []plan.Order{e.l.user, e.l.project}, e.concurrency, e.createReady)
	e.created = appendCreated(e.created, res)
	if err != nil {
		return err
	}
	e.user, e.project = res[0], res[1]

	if e.userCred, err = e.readCredential(ctx, e.user); err != nil {
		return err
	}
	return e.authenticate(ctx, e.user, e.userCred, "")
}

// refusals is stage 2: it creates each refusal in turn and waits for the
// reason the operator must report, then deletes them all and waits for each to
// be gone. A refusal that turns ready instead fails the stage with the
// ErrMismatch WaitReason returns. A refusal leaves Created once it is gone.
func (e *applier) refusals(ctx context.Context) error {
	refused := make([]resource.Resource, 0, len(e.p.Refusals))
	for _, rf := range e.p.Refusals {
		r, err := e.create(ctx, rf.Order)
		if err != nil {
			return err
		}
		e.created = append(e.created, r)
		refused = append(refused, r)

		err = bounded(ctx, e.p.ReadyTimeout, func(ctx context.Context) error {
			return e.c.WaitReason(ctx, r, rf.Reason)
		})
		if err != nil {
			return fmt.Errorf("waiting for order %s to report %s: %w", r.ID, rf.Reason, err)
		}
	}
	for _, r := range refused {
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.Delete(ctx, r) }); err != nil {
			return err
		}
	}
	for _, r := range refused {
		err := bounded(ctx, e.p.GoneTimeout, func(ctx context.Context) error { return e.c.WaitGone(ctx, r) })
		if err != nil {
			return fmt.Errorf("waiting for order %s to be gone: %w", r.ID, err)
		}
		e.created = slices.DeleteFunc(e.created, func(c resource.Resource) bool { return c.ID == r.ID })
	}
	return nil
}

// roleAndCatalog is stage 3: it creates the role assignment and the catalog
// entry in parallel and waits for each to turn ready, reads the Keystone names
// of the project and of the catalog entry's service, and checks in a token
// scoped to the project that the user holds the role and that the catalog
// lists the entry.
func (e *applier) roleAndCatalog(ctx context.Context) error {
	res, err := runStage(ctx, []plan.Order{e.l.role, e.l.catalog}, e.concurrency, e.createReady)
	e.created = appendCreated(e.created, res)
	if err != nil {
		return err
	}
	e.role, e.catalog = res[0], res[1]

	project, err := e.status(ctx, e.project)
	if err != nil {
		return err
	}
	catalog, err := e.status(ctx, e.catalog)
	if err != nil {
		return err
	}
	e.projectName, e.domainName = project.ProjectName, project.DomainName

	err = WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
		return e.c.VerifyRole(ctx, e.userCred, e.projectName, e.domainName, e.l.role.Role)
	})
	if err != nil {
		return fmt.Errorf("checking role %q of order %s in project %q: %w", e.l.role.Role, e.role.ID, e.projectName, err)
	}

	err = once(ctx, e.opTimeout, func(ctx context.Context) error {
		return e.c.VerifyCatalog(ctx, e.userCred, e.projectName, e.domainName,
			e.l.catalog.ServiceType, catalog.ServiceName, e.l.catalog.EndpointURL)
	})
	if err != nil {
		return fmt.Errorf("checking the catalog for the entry of order %s: %w", e.catalog.ID, err)
	}
	return nil
}

// applicationCredential is stage 4: it creates the application credential,
// waits for it to turn ready, reads its delivered credential and logs in with
// it, which Keystone must scope to the project.
func (e *applier) applicationCredential(ctx context.Context) error {
	r, err := e.createReady(ctx, e.l.appcred)
	if r.ID != "" {
		e.created = append(e.created, r)
	}
	if err != nil {
		return err
	}
	e.appcred = r

	if e.appcredCred, err = e.readCredential(ctx, e.appcred); err != nil {
		return err
	}
	return e.authenticate(ctx, e.appcred, e.appcredCred, e.projectName)
}

// secretRepair is stage 5: for the user and then the application credential
// it edits a key of the delivered Secret and waits for the operator to write
// the credential back, deletes the Secret and waits for it again, and logs in
// with the restored Secret. The restored credential replaces the one kept.
func (e *applier) secretRepair(ctx context.Context) error {
	var err error
	if e.userCred, err = e.repair(ctx, e.user, e.userCred, ""); err != nil {
		return err
	}
	e.appcredCred, err = e.repair(ctx, e.appcred, e.appcredCred, e.projectName)
	return err
}

// repair runs stage 5 for the order r, whose Secret last carried want, and
// returns the credential read from the restored Secret. wantProject is the
// project a login with it must be scoped to, empty for a user.
func (e *applier) repair(ctx context.Context, r resource.Resource, want c5c3.Credential, wantProject string) (c5c3.Credential, error) {
	restored := func(after string) error {
		err := bounded(ctx, e.p.ReadyTimeout, func(ctx context.Context) error {
			return e.c.WaitSecretRestored(ctx, r, want)
		})
		if err != nil {
			return fmt.Errorf("waiting for the secret of order %s to be restored after %s: %w", r.ID, after, err)
		}
		return nil
	}

	if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.EditSecret(ctx, r) }); err != nil {
		return c5c3.Credential{}, err
	}
	if err := restored("the edit"); err != nil {
		return c5c3.Credential{}, err
	}
	if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.DeleteSecret(ctx, r) }); err != nil {
		return c5c3.Credential{}, err
	}
	if err := restored("the delete"); err != nil {
		return c5c3.Credential{}, err
	}

	cred, err := e.readCredential(ctx, r)
	if err != nil {
		return c5c3.Credential{}, err
	}
	if err := e.authenticate(ctx, r, cred, wantProject); err != nil {
		return c5c3.Credential{}, err
	}
	return cred, nil
}

// passwordRotation is stage 6: it raises the user's password generation and
// waits for the order to deliver a new password, sending no login with the
// user's credential until then, because the old password stops working before
// the Secret is rewritten. The new password must then log in, and the old one,
// P0, must be rejected.
func (e *applier) passwordRotation(ctx context.Context) error {
	p0 := e.userCred
	next, err := e.rotate(ctx, e.user, p0)
	if err != nil {
		return err
	}
	if err := e.authenticate(ctx, e.user, next, ""); err != nil {
		return err
	}
	return e.reject(ctx, e.user, p0)
}

// credentialRotation is stage 7: while a prober logs in with the delivered
// Secret every proberInterval, it raises the application credential's
// generation and waits for the order to deliver a new credential. While the
// superseded credential, C0, is still more than graceMargin from its deletion,
// it must still log in; once the operator has deleted it, it must be rejected.
// The prober's first error ends the stage and is returned in preference to the
// cancellation it causes; the prober has stopped when the stage returns.
func (e *applier) credentialRotation(ctx context.Context) error {
	stageCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr := e.startProber(stageCtx, cancel, e.appcred, e.projectName)
	err := e.rotateCredential(stageCtx)
	cancel()
	if perr := pr.wait(); perr != nil {
		return perr
	}
	return err
}

// rotateCredential is the sequence of stage 7 the prober runs beside.
func (e *applier) rotateCredential(ctx context.Context) error {
	c0 := e.appcredCred
	if _, err := e.rotate(ctx, e.appcred, c0); err != nil {
		return err
	}

	st, err := e.status(ctx, e.appcred)
	if err != nil {
		return err
	}
	if left := time.Until(st.PreviousCredentialDeleteAt); st.PreviousCredentialID == c0.ApplicationCredentialID && left > graceMargin {
		if err := e.authenticate(ctx, e.appcred, c0, e.projectName); err != nil {
			return fmt.Errorf("superseded credential %s in its grace period: %w", c0.ApplicationCredentialID, err)
		}
	} else {
		slog.Info("the grace period of the superseded application credential was already over; skipping its in-grace login",
			"order", e.appcred.ID,
			"superseded", c0.ApplicationCredentialID,
			"previousCredentialID", st.PreviousCredentialID,
			"previousCredentialDeleteAt", st.PreviousCredentialDeleteAt)
	}

	err = bounded(ctx, e.p.GracePeriod+e.p.GoneTimeout, func(ctx context.Context) error {
		return e.c.WaitPreviousGone(ctx, e.appcred)
	})
	if err != nil {
		return fmt.Errorf("waiting for order %s to delete its superseded credential: %w", e.appcred.ID, err)
	}
	return e.reject(ctx, e.appcred, c0)
}

// rotate raises the credential generation of the user or application
// credential order r and waits for the order to deliver the credential of the
// new generation, which must differ from previous, and returns it.
func (e *applier) rotate(ctx context.Context, r resource.Resource, previous c5c3.Credential) (c5c3.Credential, error) {
	var generation int64
	err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
		var err error
		generation, err = e.c.RaiseGeneration(ctx, r)
		return err
	})
	if err != nil {
		return c5c3.Credential{}, err
	}

	var next c5c3.Credential
	err = bounded(ctx, e.p.ReadyTimeout, func(ctx context.Context) error {
		var err error
		next, err = e.c.WaitRotated(ctx, r, generation, previous)
		return err
	})
	if err != nil {
		return c5c3.Credential{}, fmt.Errorf("waiting for order %s to rotate to generation %d: %w", r.ID, generation, err)
	}
	return next, nil
}

// prober is stage 7's background login with the application credential's
// delivered Secret.
type prober struct {
	done chan struct{}
	// err is the error that ended the stage, written before done is closed.
	err error
}

// startProber starts a goroutine that, every proberInterval until ctx ends,
// reads the delivered Secret of the application credential order r and logs
// in with it, scoped to project. Its first error that is not caused by ctx
// ending is kept for wait and cancels ctx through cancel, which ends the
// stage.
func (e *applier) startProber(ctx context.Context, cancel context.CancelFunc, r resource.Resource, project string) *prober {
	pr := &prober{done: make(chan struct{})}
	interval := proberInterval
	go func() {
		defer close(pr.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			err := e.probe(ctx, r, project)
			if err == nil {
				continue
			}
			if ctx.Err() == nil {
				pr.err = fmt.Errorf("probing the credential of order %s during its rotation: %w", r.ID, err)
				cancel()
			}
			return
		}
	}()
	return pr
}

// probe is one login of the prober: it reads the delivered Secret of r and
// logs in with it, scoped to project.
func (e *applier) probe(ctx context.Context, r resource.Resource, project string) error {
	cred, err := e.readCredential(ctx, r)
	if err != nil {
		return err
	}
	return e.authenticate(ctx, r, cred, project)
}

// wait blocks until the prober has stopped and returns the error that ended
// the stage, or nil when the stage ended for another reason.
func (pr *prober) wait() error {
	<-pr.done
	return pr.err
}

// createReady creates o and waits for it to turn ready. The created order is
// returned with a failed wait's error, so the caller still records it.
func (e *applier) createReady(ctx context.Context, o plan.Order) (resource.Resource, error) {
	r, err := e.create(ctx, o)
	if err != nil {
		return resource.Resource{}, err
	}
	err = bounded(ctx, e.p.ReadyTimeout, func(ctx context.Context) error { return e.c.WaitReady(ctx, r) })
	if err != nil {
		return r, fmt.Errorf("waiting for order %s to turn ready: %w", r.ID, err)
	}
	return r, nil
}

// create creates o, retrying transient errors.
func (e *applier) create(ctx context.Context, o plan.Order) (resource.Resource, error) {
	var r resource.Resource
	if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return e.c.CreateOrder(ctx, o)
	}, &r); err != nil {
		return resource.Resource{}, err
	}
	return r, nil
}

// status reads the status of r, retrying transient errors.
func (e *applier) status(ctx context.Context, r resource.Resource) (c5c3.OrderStatus, error) {
	var st c5c3.OrderStatus
	err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
		var err error
		st, err = e.c.Status(ctx, r)
		return err
	})
	return st, err
}

// readCredential reads the credential delivered beside r, retrying transient
// errors.
func (e *applier) readCredential(ctx context.Context, r resource.Resource) (c5c3.Credential, error) {
	var cred c5c3.Credential
	err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
		var err error
		cred, err = e.c.ReadCredential(ctx, r)
		return err
	})
	return cred, err
}

// authenticate logs in with cred, the credential delivered beside r, retrying
// transient errors. wantProject is the project the token must be scoped to,
// empty for a user's unscoped login.
func (e *applier) authenticate(ctx context.Context, r resource.Resource, cred c5c3.Credential, wantProject string) error {
	err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
		return e.c.Authenticate(ctx, r.Kind, cred, wantProject)
	})
	if err != nil {
		return fmt.Errorf("logging in with the credential of order %s: %w", r.ID, err)
	}
	return nil
}

// reject sends the one login with cred, a superseded credential of r, that
// Keystone must refuse. It is never retried: a second failed password login
// would count toward Keystone's lockout.
func (e *applier) reject(ctx context.Context, r resource.Resource, cred c5c3.Credential) error {
	err := once(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.Reject(ctx, r.Kind, cred) })
	if err != nil {
		return fmt.Errorf("checking that Keystone rejects the superseded credential of order %s: %w", r.ID, err)
	}
	return nil
}

// once runs fn a single time, bounded by opTimeout.
func once(ctx context.Context, opTimeout time.Duration, fn func(context.Context) error) error {
	return bounded(ctx, opTimeout, fn)
}

// bounded runs fn under a context that ends after bound.
func bounded(ctx context.Context, bound time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	return fn(ctx)
}

// withCreate runs a create through the retry policy, storing the created
// resource into out on success.
func withCreate(ctx context.Context, opTimeout time.Duration, create func(context.Context) (resource.Resource, error), out *resource.Resource) error {
	return WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		r, err := create(ctx)
		if err != nil {
			return err
		}
		*out = r
		return nil
	})
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

// runStage runs work over items using a fixed pool of at most concurrency workers
// reading from a job channel: a bounded pool rather than one goroutine per item,
// so a large plan cannot exhaust resources. Results are returned in item order
// (populated even for a failing item, so a caller can record what already
// exists). The first error cancels the stage, stops dispatching and starting
// work, and is returned, with a parent-context cancellation reported as
// ctx.Err().
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
				// The dispatcher can still hand out an item after the stage has
				// failed, when the job channel and stageCtx are ready at once; such
				// an item is not started.
				if stageCtx.Err() != nil {
					continue
				}
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

	if err := ctx.Err(); err != nil {
		return results, err
	}
	// Prefer a real failure over the context.Canceled that in-flight siblings
	// return once the first error triggers cancel(); otherwise the root cause is
	// masked by an arbitrary cancelled sibling.
	var canceled error
	for _, err := range errs {
		switch {
		case err == nil:
			continue
		case errors.Is(err, context.Canceled):
			canceled = err
		default:
			return results, err
		}
	}
	if canceled != nil {
		return results, canceled
	}
	return results, nil
}

// WithRetry runs fn, bounding each attempt with opTimeout, and retries transient
// errors with exponential backoff up to maxAttempts. It returns immediately on
// success or on any error c5c3.IsRetryable rejects, which includes every c5c3
// sentinel: a refusal, a mismatch, an accepted credential, a kind the cluster
// does not serve and a missing right. Backoff sleeps honor the parent context.
// It is exported, as the other executors' WithRetry is, so a caller outside
// the apply path can drive c5c3 calls through the same backoff policy.
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
		case !c5c3.IsRetryable(err):
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
