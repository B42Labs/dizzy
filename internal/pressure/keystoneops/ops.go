// Package keystoneops builds the Keystone operations a pressure run draws:
// token validation, effective role listings, token revocation, role grant
// churn and token issue. They run over a pool of real project-scoped tokens
// that the package issues for the provisioned users of a plan and keeps fresh
// while the run goes on. The engine in internal/pressure stays service-neutral;
// this package supplies its operations as closures over a Keystone client and
// the pool. The pool tokens and the users' passwords live only in process
// memory: no log line, error text, run record or checkpoint carries them.
package keystoneops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand"
	"sync"
	"time"

	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneexec "github.com/B42Labs/dizzy/internal/keystone/executor"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/pressure"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Keystone is the surface the pressure operations, the pool priming and the
// refresher drive. It is the consumer-defined ports-and-adapters seam to the
// cloud: *keystone.Client satisfies it in production and a fake satisfies it
// in tests.
type Keystone interface {
	AssignRole(ctx context.Context, a keystoneplan.Assignment, userID, projectID, domainID, roleID string) (resource.Resource, error)
	Delete(ctx context.Context, r resource.Resource) error
	IssueScopedToken(ctx context.Context, t keystoneplan.TokenIssue, userDomainID, password, projectID string) (keystone.IssuedToken, error)
	ValidateToken(ctx context.Context, token string) error
	RevokeToken(ctx context.Context, token string) error
	ListEffectiveAssignments(ctx context.Context, userID string) (int, error)
	ListUserProjects(ctx context.Context, userID string) (int, error)
	ListAvailableProjects(ctx context.Context, token string) (int, error)
}

// The production *keystone.Client must satisfy the seam.
var _ Keystone = (*keystone.Client)(nil)

// The names of the five operations, as the scenario's mix keys, the --mix flag
// and the run record name them.
const (
	OpValidate   = "validate"
	OpEffective  = "effective"
	OpRevoke     = "revoke"
	OpGrantChurn = "grant_churn"
	OpIssue      = "issue"
)

// maxRefreshMargin caps how long before its expiry the refresher replaces a
// pool token. A token whose lifetime is shorter than four times the cap is
// replaced a quarter of its lifetime before it expires instead.
const maxRefreshMargin = 5 * time.Minute

// errNoChurnGrants is returned by grant_churn when the set reserved no churn
// user. Build reserves churn users whenever grant_churn has a positive weight,
// so the engine never draws the operation in that state.
var errNoChurnGrants = errors.New("keystoneops: grant_churn has no churn assignments to draw from")

// Options holds the knobs of an operation set, merged from the defaults, the
// scenario's pressure block and the flags.
type Options struct {
	// Tokens is the pool size, at least 1.
	Tokens int
	// ChurnUsers is the number of users, taken from the end of the plan,
	// reserved for grant_churn. They hold no pool token. It applies only when
	// the grant_churn weight is positive; otherwise no user is reserved.
	ChurnUsers int
	// Weights maps an operation name to its weight; a missing name is 0.
	Weights map[string]int
	// OpTimeout bounds every issue Prime and the refresher make. The
	// operations themselves run on the context the engine bounds.
	OpTimeout time.Duration
}

// pair is a (user, project) pair the pool issues tokens for, with everything
// an issue and the listings need resolved: the logical names for the token
// issue, the user's cloud id, the cloud id of the user's domain, its password
// and the project's cloud id. The password, like the tokens, lives only in
// memory.
type pair struct {
	issue     keystoneplan.TokenIssue
	userID    string
	domainID  string
	password  string
	projectID string
}

// churnGrant is one assignment of a churn user, resolved to the ids AssignRole
// and keystone.AssignmentResource take. mu is held while grant_churn removes
// and restores the grant, so the same grant is never removed twice at once.
type churnGrant struct {
	mu        sync.Mutex
	a         keystoneplan.Assignment
	userID    string
	projectID string // empty for a domain-scoped grant
	domainID  string // the user's domain
	roleID    string
}

// Set is the Keystone operation set of one pressure run: the client, the
// resolved pool pairs and churn grants, the token pool and its refresher.
// Build creates it, Prime fills the pool and starts the refresher, Operations
// hands the operations to pressure.Run, and Close stops the refresher.
type Set struct {
	c     Keystone
	o     Options
	pairs []pair
	churn []*churnGrant
	pool  pool

	// now reads the clock for the token lifetime and the refresher's due
	// check. It is time.Now; tests replace it to move the clock.
	now func() time.Time

	mu          sync.Mutex // guards the fields below
	primed      bool
	closed      bool
	concurrency int                // the most issues a refresh has in flight
	margin      time.Duration      // an entry closer than this to its expiry is replaced
	stop        context.CancelFunc // cancels the refresher; nil until it starts
	done        chan struct{}      // closed when the refresher goroutine returned
}

// Build resolves the operation set of plan p for a pressure run against c,
// with the root bindings b, the cloud ids of the plan's projects and users by
// logical name in ids, and the users' passwords in passwords, all three as
// keystoneexec.Provision returned them.
//
// The churn users are the last o.ChurnUsers users of p.Users in plan order
// when the grant_churn weight is positive, and none otherwise; grant_churn
// removes and restores their assignments only. The pool pairs are the
// distinct project-scoped (user, project) pairs of p.Assignments whose user is
// not a churn user, in first-appearance order. Build checks the plan, the
// options and the partition, and resolves every id it needs, so a run fails
// here, before any token is issued, rather than in a worker.
func Build(p *keystoneplan.Plan, c Keystone, b keystoneexec.Bindings, ids, passwords map[string]string, o Options) (*Set, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid plan: %w", err)
	}
	if o.Tokens < 1 {
		return nil, fmt.Errorf("pressure tokens must be at least 1, got %d", o.Tokens)
	}
	if o.ChurnUsers < 0 {
		return nil, fmt.Errorf("pressure churn users must not be negative, got %d", o.ChurnUsers)
	}
	if o.OpTimeout <= 0 {
		return nil, fmt.Errorf("pressure op-timeout must be positive, got %s", o.OpTimeout)
	}

	churnUsers := make(map[string]bool)
	if o.Weights[OpGrantChurn] > 0 {
		if o.ChurnUsers >= len(p.Users) {
			return nil, fmt.Errorf("pressure.churn_users (%d) must leave at least one user outside the churn set; the plan has %d users", o.ChurnUsers, len(p.Users))
		}
		for _, u := range p.Users[len(p.Users)-o.ChurnUsers:] {
			churnUsers[u.Name] = true
		}
	}

	var churnAssignments, poolAssignments []keystoneplan.Assignment
	seen := make(map[[2]string]bool)
	for _, a := range p.Assignments {
		if churnUsers[a.User] {
			churnAssignments = append(churnAssignments, a)
			continue
		}
		key := [2]string{a.User, a.Project}
		if a.Project == "" || seen[key] {
			continue
		}
		seen[key] = true
		poolAssignments = append(poolAssignments, a)
	}
	if o.Weights[OpGrantChurn] > 0 && len(churnAssignments) == 0 {
		return nil, fmt.Errorf("pressure grant_churn needs the churn users to hold at least one assignment; the last %d user(s) of the plan hold none", o.ChurnUsers)
	}
	if len(poolAssignments) == 0 {
		return nil, errors.New("pressure needs at least one project-scoped assignment outside the churn users to issue pool tokens from; the plan has none")
	}

	r := resolver{b: b, ids: ids, passwords: passwords, userDomain: make(map[string]string, len(p.Users))}
	for _, u := range p.Users {
		r.userDomain[u.Name] = u.Domain
	}
	s := &Set{c: c, o: o, now: time.Now}
	s.o.Weights = maps.Clone(o.Weights)
	for _, a := range poolAssignments {
		pr, err := r.pair(a)
		if err != nil {
			return nil, err
		}
		s.pairs = append(s.pairs, pr)
	}
	for _, a := range churnAssignments {
		g, err := r.grant(a)
		if err != nil {
			return nil, err
		}
		s.churn = append(s.churn, g)
	}
	return s, nil
}

// resolver looks up the cloud ids and passwords Build needs and fails with an
// error naming what is missing. keystoneexec.Provision fills every one of
// them, so a miss means the maps belong to another plan.
type resolver struct {
	b          keystoneexec.Bindings
	ids        map[string]string
	passwords  map[string]string
	userDomain map[string]string // user logical name to domain logical name
}

// id returns the cloud id of the project or user (kind) with logical name.
func (r resolver) id(kind, name string) (string, error) {
	id := r.ids[name]
	if id == "" {
		return "", fmt.Errorf("pressure set: no cloud id for %s %q", kind, name)
	}
	return id, nil
}

// domainID returns the cloud id of user's domain from the root bindings.
func (r resolver) domainID(user string) (string, error) {
	dom := r.userDomain[user]
	id := r.b.Domains[dom]
	if id == "" {
		return "", fmt.Errorf("pressure set: no cloud id for domain %q of user %q", dom, user)
	}
	return id, nil
}

// pair resolves the pool pair of the project-scoped assignment a.
func (r resolver) pair(a keystoneplan.Assignment) (pair, error) {
	userID, err := r.id("user", a.User)
	if err != nil {
		return pair{}, err
	}
	projectID, err := r.id("project", a.Project)
	if err != nil {
		return pair{}, err
	}
	domainID, err := r.domainID(a.User)
	if err != nil {
		return pair{}, err
	}
	password := r.passwords[a.User]
	if password == "" {
		return pair{}, fmt.Errorf("pressure set: no password for user %q", a.User)
	}
	return pair{
		issue:     keystoneplan.TokenIssue{User: a.User, Project: a.Project},
		userID:    userID,
		domainID:  domainID,
		password:  password,
		projectID: projectID,
	}, nil
}

// grant resolves the churn grant of assignment a, project- or domain-scoped.
func (r resolver) grant(a keystoneplan.Assignment) (*churnGrant, error) {
	userID, err := r.id("user", a.User)
	if err != nil {
		return nil, err
	}
	var projectID string
	if a.Project != "" {
		if projectID, err = r.id("project", a.Project); err != nil {
			return nil, err
		}
	}
	domainID, err := r.domainID(a.User)
	if err != nil {
		return nil, err
	}
	roleID := r.b.Roles[a.Role]
	if roleID == "" {
		return nil, fmt.Errorf("pressure set: no cloud id for role %q", a.Role)
	}
	return &churnGrant{a: a, userID: userID, projectID: projectID, domainID: domainID, roleID: roleID}, nil
}

// Prime fills the pool with o.Tokens tokens, token k for pool pair k modulo
// the number of pairs, with at most concurrency issues in flight (at least
// one), each bounded by o.OpTimeout. The first failure stops further issues;
// Prime waits for the issues in flight and returns the failure with the
// number of tokens issued so far. On success it starts the refresher, which
// replaces every entry that comes within a quarter of the token lifetime (at
// most 5 minutes) of its expiry, and runs until Close or until ctx ends. The
// lifetime is that of the first token, measured from just before its issue
// started. A pool can be primed once.
func (s *Set) Prime(ctx context.Context, concurrency int) error {
	s.mu.Lock()
	if s.primed {
		s.mu.Unlock()
		return errors.New("keystoneops: the token pool is already primed")
	}
	s.primed = true
	s.mu.Unlock()
	concurrency = max(concurrency, 1)

	var (
		mu       sync.Mutex // guards the four below
		firstErr error
		issued   int
		entries  = make([]entry, s.o.Tokens)
		lifetime time.Duration
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, concurrency)
	for k := range s.o.Tokens {
		sem <- struct{}{}
		mu.Lock()
		if firstErr == nil && ctx.Err() != nil {
			firstErr = ctx.Err()
		}
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			started := s.now()
			e, err := s.issueBounded(ctx, k%len(s.pairs))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			entries[k] = e
			issued++
			if k == 0 {
				lifetime = e.expiresAt.Sub(started)
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return fmt.Errorf("priming token pool (%d of %d issued): %w", issued, s.o.Tokens, firstErr)
	}
	// The entries enter the pool in issue order, so the pool's layout, and with
	// it which entry a seeded draw picks, does not depend on which issue
	// returned first.
	for _, e := range entries {
		s.pool.put(e)
	}
	s.startRefresher(ctx, lifetime, concurrency)
	return nil
}

// refreshSchedule derives the refresher's timing from the pool token lifetime.
// An entry is replaced once it comes within margin of its expiry, where margin
// is the smaller of maxRefreshMargin and a quarter of the lifetime. The
// refresher looks every interval, half the margin, so an entry is replaced at
// least half a margin before it expires. ok is false when the lifetime is too
// short to derive a positive interval from, as when Keystone reported no
// expiry.
func refreshSchedule(lifetime time.Duration) (margin, interval time.Duration, ok bool) {
	margin = min(maxRefreshMargin, lifetime/4)
	interval = margin / 2
	return margin, interval, interval > 0
}

// startRefresher starts the goroutine that replaces pool entries before they
// expire: every interval of refreshSchedule it calls refreshDue. It runs on a
// context derived from ctx that Close cancels. A lifetime too short to derive
// a tick from (Keystone reported no expiry) starts nothing and logs a warning,
// so the run goes on with tokens that are not renewed. After Close it starts
// nothing.
func (s *Set) startRefresher(ctx context.Context, lifetime time.Duration, concurrency int) {
	margin, interval, ok := refreshSchedule(lifetime)
	if !ok {
		slog.Warn("token pool refresher not started: the first pool token reported no usable expiry, so pool tokens are not renewed before they expire")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.margin, s.concurrency = margin, concurrency
	if s.closed {
		return
	}
	refreshCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	s.stop, s.done = stop, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-refreshCtx.Done():
				return
			case <-ticker.C:
				s.refreshDue(refreshCtx, s.now())
			}
		}
	}()
}

// refreshDue replaces every pool entry that expires less than the refresh
// margin after now. For each one it first issues a token for the same pair,
// with at most Prime's concurrency in flight and each issue bounded by
// o.OpTimeout, and only then swaps the new entry in place of the old one, so
// the pool is never emptied by a refresh and the operations keep drawing from
// it meanwhile. An entry a revoke or an issue took in the meantime is not
// replaced and its new token is discarded, so the pool never grows. A failed
// issue keeps the old entry for the next tick.
func (s *Set) refreshDue(ctx context.Context, now time.Time) {
	s.mu.Lock()
	margin, concurrency := s.margin, s.concurrency
	s.mu.Unlock()
	due := s.pool.expiringBefore(now.Add(margin))
	if len(due) == 0 {
		return
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	sem := make(chan struct{}, max(concurrency, 1))
	for _, old := range due {
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			next, err := s.issueBounded(ctx, old.pair)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("token pool refresh failed; the entry is retried on the next tick", "error", err)
				}
				return
			}
			s.pool.replace(old, next)
		}()
	}
}

// Close stops the refresher and waits for it to return. It is safe to call
// more than once, and before or without Prime.
func (s *Set) Close() {
	s.mu.Lock()
	s.closed = true
	stop, done := s.stop, s.done
	s.mu.Unlock()
	if stop == nil {
		return
	}
	stop()
	<-done
}

// issueBounded issues a token for pool pair i on a context bounded by
// o.OpTimeout, for the issues Prime and the refresher make outside any
// operation.
func (s *Set) issueBounded(ctx context.Context, i int) (entry, error) {
	ctx, cancel := context.WithTimeout(ctx, s.o.OpTimeout)
	defer cancel()
	return s.issue(ctx, i)
}

// issue issues a token for pool pair i and returns it as a pool entry.
func (s *Set) issue(ctx context.Context, i int) (entry, error) {
	p := &s.pairs[i]
	t, err := s.c.IssueScopedToken(ctx, p.issue, p.domainID, p.password, p.projectID)
	if err != nil {
		return entry{}, err
	}
	return entry{token: t.ID, expiresAt: t.ExpiresAt, pair: i}, nil
}

// Operations returns the five operations in the order validate, effective,
// revoke, grant_churn, issue, each with its weight from o.Weights (0 when
// missing), for pressure.Run. Each Do makes one attempt and never retries: a
// load generator records what the cloud answered. Each draws only from the rng
// the engine hands it, the calling worker's own generator.
func (s *Set) Operations() []pressure.Operation {
	return []pressure.Operation{
		{Name: OpValidate, Weight: s.o.Weights[OpValidate], Do: s.validate},
		{Name: OpEffective, Weight: s.o.Weights[OpEffective], Do: s.effective},
		{Name: OpRevoke, Weight: s.o.Weights[OpRevoke], Do: s.revoke},
		{Name: OpGrantChurn, Weight: s.o.Weights[OpGrantChurn], Do: s.grantChurn},
		{Name: OpIssue, Weight: s.o.Weights[OpIssue], Do: s.reissue},
	}
}

// validate validates a random pool token, the request keystonemiddleware makes
// on a cache miss. The token stays in the pool.
func (s *Set) validate(ctx context.Context, rng *rand.Rand) error {
	e, ok := s.pool.peek(rng)
	if !ok {
		return ErrPoolEmpty
	}
	return s.c.ValidateToken(ctx, e.token)
}

// effective lists a random pool user's effective role assignments, the
// projects that user can access, and the projects the pool token itself can
// access, in that order. The first error ends the operation.
func (s *Set) effective(ctx context.Context, rng *rand.Rand) error {
	e, ok := s.pool.peek(rng)
	if !ok {
		return ErrPoolEmpty
	}
	userID := s.pairs[e.pair].userID
	if _, err := s.c.ListEffectiveAssignments(ctx, userID); err != nil {
		return err
	}
	if _, err := s.c.ListUserProjects(ctx, userID); err != nil {
		return err
	}
	_, err := s.c.ListAvailableProjects(ctx, e.token)
	return err
}

// revoke takes a random pool token, revokes it, and then always issues a
// replacement for the same pair, so a failed revoke does not shrink the pool.
// It succeeds only when both calls succeed and returns the revoke's error
// first. A failed reissue shrinks the pool by one, which it logs.
func (s *Set) revoke(ctx context.Context, rng *rand.Rand) error {
	e, ok := s.pool.take(rng)
	if !ok {
		return ErrPoolEmpty
	}
	revokeErr := s.c.RevokeToken(ctx, e.token)
	next, err := s.issue(ctx, e.pair)
	if err != nil {
		slog.Warn("token pool shrank; a reissue failed", "size", s.pool.len(), "error", err)
	} else {
		s.pool.put(next)
	}
	if revokeErr != nil {
		return revokeErr
	}
	return err
}

// grantChurn removes a random churn grant and grants it again, so Keystone's
// assignment caches keep being invalidated. It tries the drawn grant's lock
// and then the others' round-robin without waiting, and blocks on the drawn
// one only when every grant is busy, so a lock wait rarely inflates the
// measured latency. The re-grant runs even after a failed removal, so the
// grant is restored whenever the cloud allows it; the first error is
// returned. A pool user's grant is never touched.
func (s *Set) grantChurn(ctx context.Context, rng *rand.Rand) error {
	if len(s.churn) == 0 {
		return errNoChurnGrants
	}
	g := s.lockChurn(rng.Intn(len(s.churn)))
	defer g.mu.Unlock()
	deleteErr := s.c.Delete(ctx, keystone.AssignmentResource(g.a, g.userID, g.projectID, g.domainID, g.roleID))
	_, assignErr := s.c.AssignRole(ctx, g.a, g.userID, g.projectID, g.domainID, g.roleID)
	if deleteErr != nil {
		return deleteErr
	}
	return assignErr
}

// lockChurn locks and returns a churn grant: the first one, from index i on
// round-robin, whose lock is free, or grant i once its lock frees when every
// grant is busy.
func (s *Set) lockChurn(i int) *churnGrant {
	n := len(s.churn)
	for j := range n {
		if g := s.churn[(i+j)%n]; g.mu.TryLock() {
			return g
		}
	}
	g := s.churn[i]
	g.mu.Lock()
	return g
}

// reissue is the issue operation: it takes a random pool token and issues a
// new one for the same pair in its place, measuring the password login alone.
// The old token is dropped, not revoked, because a revocation flushes the
// token cache. When the issue fails the old entry goes back, so the pool
// keeps its size.
func (s *Set) reissue(ctx context.Context, rng *rand.Rand) error {
	e, ok := s.pool.take(rng)
	if !ok {
		return ErrPoolEmpty
	}
	next, err := s.issue(ctx, e.pair)
	if err != nil {
		s.pool.put(e)
		return err
	}
	s.pool.put(next)
	return nil
}
