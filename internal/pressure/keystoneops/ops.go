// Package keystoneops builds the Keystone operations a pressure run draws:
// token validation, effective role listings, token revocation, role grant
// churn and token issue. They run over a pool of real project-scoped tokens
// that the package issues for the provisioned users of a plan and keeps fresh
// while the run goes on. A plan's fan users, which hold a grant on many
// projects, get a token each outside the pool: the effective listings target
// them, and GrowFan adds grants to them between the stages of a run.
//
// Two more operations and a probe send the pool tokens to the other services,
// Nova, Neutron, Cinder and Glance, through the Services seam: probe sends a
// live pool token for the service's cheapest read, bad_token a token the
// service has to refuse, and the propagation probe revokes a pool token and
// measures how long a service keeps accepting it. Each token goes only to the
// endpoints of its own catalog.
//
// The engine in internal/pressure stays service-neutral; this package supplies
// its operations as closures over a Keystone client, the services and the
// pool. The pool tokens, the revoked ones and the users' passwords live only
// in process memory: no log line, error text, run record or checkpoint
// carries them.
package keystoneops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand"
	"slices"
	"sync"
	"time"

	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneexec "github.com/B42Labs/dizzy/internal/keystone/executor"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/pressure"
	"github.com/B42Labs/dizzy/internal/pressure/probe"
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

// The names of the seven operations, as the scenario's mix keys, the --mix
// flag and the run record name them.
const (
	OpValidate   = "validate"
	OpEffective  = "effective"
	OpRevoke     = "revoke"
	OpGrantChurn = "grant_churn"
	OpIssue      = "issue"
	OpProbe      = "probe"
	OpBadToken   = "bad_token"
)

// OperationNames lists the seven operations in the order Operations returns
// them, the order the --mix help and checkPressureMix use.
var OperationNames = []string{OpValidate, OpEffective, OpRevoke, OpGrantChurn, OpIssue, OpProbe, OpBadToken}

// maxRefreshMargin caps how long before its expiry the refresher replaces a
// pool token. A token whose lifetime is shorter than four times the cap is
// replaced a quarter of its lifetime before it expires instead.
const maxRefreshMargin = 5 * time.Minute

// propagationPoll is how often a propagation probe polls a service with the
// token it revoked. Build sets it as the set's poll interval; tests shorten
// that.
const propagationPoll = time.Second

// errNoChurnGrants is returned by grant_churn when the set reserved no churn
// user. Build reserves churn users whenever grant_churn has a positive weight,
// so the engine never draws the operation in that state.
var errNoChurnGrants = errors.New("keystoneops: grant_churn has no churn assignments to draw from")

// errNoServiceClient is returned by probe and bad_token when the set was
// built without a service client. Build requires one whenever either has a
// positive weight, so the engine never draws them in that state.
var errNoServiceClient = errors.New("keystoneops: probe and bad_token have no service client to send through")

// ErrNoService is returned by probe and bad_token when the drawn token's
// catalog lists none of the four services. Classify labels it "no-service".
var ErrNoService = errors.New("keystoneops: the token's catalog lists none of nova, neutron, cinder, glance")

// Services is the seam to the other OpenStack services: *probe.Client
// satisfies it in production and a fake in tests.
type Services interface {
	Probe(ctx context.Context, service, endpoint, token string) error
	Reject(ctx context.Context, service, endpoint, token string) error
	Poll(ctx context.Context, service, endpoint, token string) (rejected bool, err error)
	RecordRevocationDelay(ctx context.Context, d time.Duration, ok bool)
}

// The production *probe.Client must satisfy the seam.
var _ Services = (*probe.Client)(nil)

// Propagation configures the propagation probe: every Every the set revokes
// one pool token and polls a service with it until the service answers 401
// or Timeout has passed. Every 0 switches the probe off.
type Propagation struct {
	Every   time.Duration
	Timeout time.Duration
}

// Options holds the knobs of an operation set, merged from the defaults, the
// scenario's pressure block and the flags.
type Options struct {
	// Tokens is the pool size, at least 1.
	Tokens int
	// ChurnUsers is the number of users, taken from the end of the plan's
	// regular users, reserved for grant_churn; a fan user is never one. They
	// hold no pool token. It applies only when the grant_churn weight is
	// positive; otherwise no user is reserved.
	ChurnUsers int
	// Weights maps an operation name to its weight; a missing name is 0.
	Weights map[string]int
	// OpTimeout bounds every issue Prime and the refresher make, and each
	// revoke, reissue and poll of the propagation probe. The operations
	// themselves run on the context the engine bounds.
	OpTimeout time.Duration
	// Seed seeds the propagation probe's draws, so the tokens it revokes
	// depend on the seed and the pool alone.
	Seed int64
	// Propagation configures the propagation probe; its zero value is off.
	Propagation Propagation
}

// needsServices reports whether a set with options o sends requests to the
// other services, through probe, bad_token or the propagation probe, and so
// needs the Services seam and a service in the pool users' catalog.
func needsServices(o Options) bool {
	return o.Weights[OpProbe] > 0 || o.Weights[OpBadToken] > 0 || o.Propagation.Every > 0
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

// fanUser is one fan user: its ids, the projects it holds a grant on (by
// logical name) and the index of its pair in pairs, for its token. The
// password its token is issued with lives in that pair.
type fanUser struct {
	name, userID, domainID string
	mu                     sync.Mutex      // guards held while GrowFan grants
	held                   map[string]bool // project logical names with a grant
	pair                   int
}

// idName is a plan resource's logical name and cloud id.
type idName struct{ Name, ID string }

// fanGrant is one grant GrowFan makes: role on project for the fan user u.
type fanGrant struct {
	u       *fanUser
	project idName
	role    idName
}

// Set is the Keystone operation set of one pressure run: the client and the
// services, the resolved pool pairs, churn grants and fan users, the token
// pool, the fan users' tokens, their refresher and the propagation probe.
// Build creates it, GrantPool grants the pool users a role beyond the plan's,
// Prime fills the pools and starts the refresher and the propagation probe,
// Operations hands the operations to pressure.Run, GrowFan grows the fan
// users' grants between stages, ServiceStats reports what the services
// answered, and Close stops the refresher and the probe.
type Set struct {
	c   Keystone
	svc Services // nil when the run sends nothing to the other services
	o   Options
	// pairs holds the poolPairs pool pairs first and then one pair per fan
	// user, in the order of fan, so a pool round robin over the first
	// poolPairs entries never issues a fan user's token.
	pairs     []pair
	poolPairs int
	churn     []*churnGrant
	pool      pool

	fan      []*fanUser
	fanPool  pool     // one entry per fan user, never taken
	projects []idName // every plan project in plan order
	roles    []idName // every plan role in plan order

	// probes and badTokens hold the samples of the probe and bad_token draws,
	// Type being the service, standalone collectors so the run record can
	// list them per service apart from the operations. delays holds the
	// propagation probes' outcomes as time-to-ready records of type
	// probe.KindRevocation.
	probes, badTokens, delays *metrics.Collector
	// revoked holds the pool tokens revoke and the propagation probe revoked,
	// for bad_token.
	revoked revokedTokens
	// services lists the services of the first pool token's catalog in
	// probe.Services order. Prime sets it once, before any entry enters a pool
	// and before the propagation probe starts, and nothing changes it
	// afterwards; it stays nil for a run that needs no service.
	services []string
	// pollEvery is how often a propagation probe polls. Build sets it to
	// propagationPoll; tests shorten it before Prime.
	pollEvery time.Duration

	// now reads the clock for the token lifetime, the refresher's due check
	// and the revoked tokens' age. It is time.Now; tests replace it to move
	// the clock. The propagation delay is measured with time.Now itself,
	// whose monotonic reading survives a clock change.
	now func() time.Time

	// propagating counts the propagation probes in flight. Only the
	// propagation ticker goroutine adds to it, and it waits for it before it
	// returns.
	propagating sync.WaitGroup

	mu          sync.Mutex // guards the fields below
	primed      bool
	closed      bool
	concurrency int                // the most issues a refresh has in flight
	margin      time.Duration      // an entry closer than this to its expiry is replaced
	stop        context.CancelFunc // cancels the refresher; nil until it starts
	done        chan struct{}      // closed when the refresher goroutine returned
	propStop    context.CancelFunc // cancels the propagation probe; nil until it starts
	propDone    chan struct{}      // closed when the ticker goroutine and every probe returned
}

// Build resolves the operation set of plan p for a pressure run against c and
// the services svc, with the root bindings b, the cloud ids of the plan's
// projects and users by logical name in ids, and the users' passwords in
// passwords, all three as keystoneexec.Provision returned them. svc may be nil
// for a run that sends nothing to the other services: probe and bad_token at
// weight 0 and the propagation probe off. A run that needs it fails here
// without it, and a propagation probe needs a positive timeout.
//
// Build partitions the users three ways. The fan users are the users with Fan
// set, in plan order. The churn users are the last o.ChurnUsers of the other,
// regular users in plan order when the grant_churn weight is positive, and
// none otherwise; grant_churn removes and restores their assignments only.
// The pool pairs are the distinct project-scoped (user, project) pairs of
// p.Assignments whose user is neither a churn nor a fan user, in
// first-appearance order. Each fan user gets one more pair, after the pool
// pairs, for its first project-scoped assignment in plan order, which its own
// token is scoped to; a plan with fan users also resolves every project and
// role, the grants GrowFan may make. Build checks the plan, the options and
// the partition, and resolves every id it needs, so a run fails here, before
// any token is issued, rather than in a worker.
func Build(p *keystoneplan.Plan, c Keystone, svc Services, b keystoneexec.Bindings, ids, passwords map[string]string, o Options) (*Set, error) {
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
	if o.Propagation.Every > 0 && o.Propagation.Timeout <= 0 {
		return nil, fmt.Errorf("pressure propagation timeout must be positive, got %s", o.Propagation.Timeout)
	}
	if needsServices(o) && svc == nil {
		return nil, errors.New("pressure probe, bad_token and the propagation probe need a service client; none was given")
	}

	// The fan users are set apart first, so neither the churn set nor the pool
	// ever takes one.
	var regular, fanUsers []keystoneplan.User
	for _, u := range p.Users {
		if u.Fan {
			fanUsers = append(fanUsers, u)
		} else {
			regular = append(regular, u)
		}
	}
	churnUsers := make(map[string]bool)
	if o.Weights[OpGrantChurn] > 0 {
		if o.ChurnUsers >= len(regular) {
			return nil, fmt.Errorf("pressure.churn_users (%d) must leave at least one user outside the churn set; the plan has %d users", o.ChurnUsers, len(regular))
		}
		for _, u := range regular[len(regular)-o.ChurnUsers:] {
			churnUsers[u.Name] = true
		}
	}
	fanHeld := make(map[string]map[string]bool, len(fanUsers))
	for _, u := range fanUsers {
		fanHeld[u.Name] = make(map[string]bool)
	}

	var churnAssignments, poolAssignments []keystoneplan.Assignment
	fanFirst := make(map[string]keystoneplan.Assignment, len(fanUsers)) // a fan user's first project-scoped assignment
	seen := make(map[[2]string]bool)
	for _, a := range p.Assignments {
		if held, ok := fanHeld[a.User]; ok {
			if a.Project == "" {
				continue
			}
			if _, ok := fanFirst[a.User]; !ok {
				fanFirst[a.User] = a
			}
			held[a.Project] = true
			continue
		}
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
		return nil, fmt.Errorf("pressure grant_churn needs the churn users to hold at least one assignment; the last %d regular user(s) of the plan hold none", o.ChurnUsers)
	}
	if len(poolAssignments) == 0 {
		return nil, errors.New("pressure needs at least one project-scoped assignment outside the churn and fan users to issue pool tokens from; the plan has none")
	}
	for _, u := range fanUsers {
		if _, ok := fanFirst[u.Name]; !ok {
			return nil, fmt.Errorf("pressure fan user %q holds no project-scoped assignment to scope its token to", u.Name)
		}
	}

	r := resolver{b: b, ids: ids, passwords: passwords, userDomain: make(map[string]string, len(p.Users))}
	for _, u := range p.Users {
		r.userDomain[u.Name] = u.Domain
	}
	s := &Set{
		c: c, svc: svc, o: o, now: time.Now, pollEvery: propagationPoll,
		probes: metrics.NewCollector(), badTokens: metrics.NewCollector(), delays: metrics.NewCollector(),
	}
	s.o.Weights = maps.Clone(o.Weights)
	for _, a := range poolAssignments {
		pr, err := r.pair(a)
		if err != nil {
			return nil, err
		}
		s.pairs = append(s.pairs, pr)
	}
	s.poolPairs = len(s.pairs)
	for _, u := range fanUsers {
		pr, err := r.pair(fanFirst[u.Name])
		if err != nil {
			return nil, err
		}
		s.fan = append(s.fan, &fanUser{name: u.Name, userID: pr.userID, domainID: pr.domainID, held: fanHeld[u.Name], pair: len(s.pairs)})
		s.pairs = append(s.pairs, pr)
	}
	for _, a := range churnAssignments {
		g, err := r.grant(a)
		if err != nil {
			return nil, err
		}
		s.churn = append(s.churn, g)
	}
	// GrowFan may grant any plan role on any plan project, so a plan with fan
	// users resolves them all up front; a plan without needs neither.
	if len(s.fan) > 0 {
		for _, pr := range p.Projects {
			id, err := r.id("project", pr.Name)
			if err != nil {
				return nil, err
			}
			s.projects = append(s.projects, idName{Name: pr.Name, ID: id})
		}
		for _, rl := range p.Roles {
			id, err := r.role(rl.Name)
			if err != nil {
				return nil, err
			}
			s.roles = append(s.roles, idName{Name: rl.Name, ID: id})
		}
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

// pair resolves the pair of the project-scoped assignment a, a pool pair or
// the pair of a fan user's token.
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
	roleID, err := r.role(a.Role)
	if err != nil {
		return nil, err
	}
	return &churnGrant{a: a, userID: userID, projectID: projectID, domainID: domainID, roleID: roleID}, nil
}

// role returns the cloud id of the role with logical name from the root
// bindings.
func (r resolver) role(name string) (string, error) {
	id := r.b.Roles[name]
	if id == "" {
		return "", fmt.Errorf("pressure set: no cloud id for role %q", name)
	}
	return id, nil
}

// GrantPool grants the existing role roleID, whose name is role, to the user
// of every pool pair on the pair's project, for a role a pool token needs
// beyond the plan's, such as the reader role a service's read policy asks
// for. Neither a churn user nor a fan user gets it. Each grant runs through
// keystoneexec.WithRetry bounded by o.OpTimeout, with at most concurrency
// grants in flight (at least 1). After the first failure, or once ctx ended,
// no further grant starts: GrantPool waits for the grants in flight and
// returns the created resources, in completion order, those created before
// the failure included, and the failure with the number granted.
func (s *Set) GrantPool(ctx context.Context, role, roleID string, concurrency int) ([]resource.Resource, error) {
	var (
		mu      sync.Mutex // guards the two below
		created = make([]resource.Resource, 0, s.poolPairs)
		cause   error
		wg      sync.WaitGroup
	)
	sem := make(chan struct{}, max(concurrency, 1))
	for i := range s.poolPairs {
		sem <- struct{}{}
		mu.Lock()
		if cause == nil && ctx.Err() != nil {
			cause = ctx.Err()
		}
		stop := cause != nil
		mu.Unlock()
		if stop {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			p := &s.pairs[i]
			a := keystoneplan.Assignment{User: p.issue.User, Role: role, Project: p.issue.Project}
			var res resource.Resource
			err := keystoneexec.WithRetry(ctx, s.o.OpTimeout, func(ctx context.Context) error {
				r, err := s.c.AssignRole(ctx, a, p.userID, p.projectID, p.domainID, roleID)
				if err != nil {
					return err
				}
				res = r
				return nil
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if cause == nil {
					cause = err
				}
				return
			}
			created = append(created, res)
		}()
	}
	wg.Wait()
	if cause != nil {
		return created, fmt.Errorf("granting role %q to the pool users (%d of %d granted): %w", role, len(created), s.poolPairs, cause)
	}
	return created, nil
}

// Prime fills the pool with o.Tokens tokens, token k for pool pair k modulo
// the number of pool pairs, and then the fan pool with one token per fan user,
// for its pair, with at most concurrency issues in flight (at least one), each
// bounded by o.OpTimeout. The first failure stops further issues; Prime waits
// for the issues in flight and returns the failure with the number of tokens
// issued so far out of both pools' total.
//
// A run that sends tokens to the other services then takes its services from
// the first pool token's catalog, in probe.Services order, and fails when that
// catalog lists none of the four, before any entry enters a pool and before
// anything starts: the pool users' catalogs decide which services exist for
// them, and a run without one would probe nothing.
//
// On success it starts the refresher, which replaces every entry of either
// pool that comes within a quarter of the token lifetime (at most 5 minutes)
// of its expiry, and, when o.Propagation.Every is positive, the propagation
// probe; both run until Close or until ctx ends. The lifetime is that of the
// first pool token, measured from just before its issue started. A pool can
// be primed once.
func (s *Set) Prime(ctx context.Context, concurrency int) error {
	s.mu.Lock()
	if s.primed {
		s.mu.Unlock()
		return errors.New("keystoneops: the token pool is already primed")
	}
	s.primed = true
	s.mu.Unlock()
	concurrency = max(concurrency, 1)

	// Token k < o.Tokens is pool token k; token o.Tokens+i is fan user i's.
	total := s.o.Tokens + len(s.fan)
	var (
		mu       sync.Mutex // guards the four below
		firstErr error
		issued   int
		entries  = make([]entry, total)
		lifetime time.Duration
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, concurrency)
	for k := range total {
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
		i := k % s.poolPairs
		if k >= s.o.Tokens {
			i = s.fan[k-s.o.Tokens].pair
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			started := s.now()
			e, err := s.issueBounded(ctx, i)
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
		return fmt.Errorf("priming token pool (%d of %d issued): %w", issued, total, firstErr)
	}
	if needsServices(s.o) {
		s.services = listedServices(entries[0].endpoints)
		if len(s.services) == 0 {
			return errors.New("pressure probe needs at least one of nova, neutron, cinder, glance in the pool users' catalog; the first pool token's catalog lists none")
		}
	}
	// The entries enter their pools in issue order, so a pool's layout, and with
	// it which entry a seeded draw picks, does not depend on which issue
	// returned first.
	for _, e := range entries[:s.o.Tokens] {
		s.pool.put(e)
	}
	for _, e := range entries[s.o.Tokens:] {
		s.fanPool.put(e)
	}
	s.startRefresher(ctx, lifetime, concurrency)
	if s.o.Propagation.Every > 0 {
		s.startPropagation(ctx)
	}
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

// refreshDue replaces every entry of the pool and of the fan pool that
// expires less than the refresh margin after now. For each one it first
// issues a token for the same pair, with at most Prime's concurrency in flight
// across both pools and each issue bounded by o.OpTimeout, and only then swaps
// the new entry in place of the old one in the old one's pool, so no pool is
// emptied by a refresh and the operations keep drawing from it meanwhile. An
// entry a revoke or an issue took in the meantime is not replaced and its new
// token is discarded, so the pool never grows. A failed issue keeps the old
// entry for the next tick.
func (s *Set) refreshDue(ctx context.Context, now time.Time) {
	s.mu.Lock()
	margin, concurrency := s.margin, s.concurrency
	s.mu.Unlock()
	type dueEntry struct {
		p   *pool
		old entry
	}
	var due []dueEntry
	for _, p := range []*pool{&s.pool, &s.fanPool} {
		for _, e := range p.expiringBefore(now.Add(margin)) {
			due = append(due, dueEntry{p, e})
		}
	}
	if len(due) == 0 {
		return
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	sem := make(chan struct{}, max(concurrency, 1))
	for _, d := range due {
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			next, err := s.issueBounded(ctx, d.old.pair)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("token pool refresh failed; the entry is retried on the next tick", "error", err)
				}
				return
			}
			d.p.replace(d.old, next)
		}()
	}
}

// startPropagation starts the propagation probe: a goroutine with a ticker of
// o.Propagation.Every that, on every tick, starts one probe, propagate, on a
// goroutine of its own, so a probe that takes longer than the interval never
// delays the next. A probe ends at its timeout, so about ceil(Timeout / Every)
// probes are in flight at once, 20 for an interval of 30s and a timeout of
// 10m, each sending one poll per pollEvery. The probes are numbered from 0 in
// tick order, which picks the service each one polls.
//
// The goroutine owns a generator seeded with o.Seed and hands each probe, in
// tick order, a generator seeded from it. The token probe k takes then
// depends on the seed, k and the pool alone, not on which of two overlapping
// probes reaches the pool first, as it would with one generator shared among
// them. The goroutine runs on a context derived from ctx that Close cancels,
// and waits for every probe it started before it returns. After Close it
// starts nothing.
func (s *Set) startPropagation(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	propCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	s.propStop, s.propDone = stop, done
	go func() {
		defer close(done)
		defer s.propagating.Wait()
		rng := rand.New(rand.NewSource(s.o.Seed))
		ticker := time.NewTicker(s.o.Propagation.Every)
		defer ticker.Stop()
		for k := 0; ; k++ {
			select {
			case <-propCtx.Done():
				return
			case <-ticker.C:
			}
			probeRng := rand.New(rand.NewSource(rng.Int63()))
			s.propagating.Add(1)
			go func() {
				defer s.propagating.Done()
				s.propagate(propCtx, k, probeRng)
			}()
		}
	}()
}

// propagate is one propagation probe, number k. It takes a random pool token,
// revokes it and replaces it as revoke does, and then polls a service with
// the revoked token every pollEvery until the service answers 401, recording
// the delay from the revoke's return to that answer, or until
// o.Propagation.Timeout after the revoke, recording the timeout. Both go into
// the set's delays and through the seam. A poll that fails, a 5xx, a timeout
// or a 403, is not an answer, so the probe polls again. When the deadline
// comes before the next poll is due, the wait ends there and the probe sends
// one last poll, as after every wait, and records the timeout only when that
// poll was no 401 either. Its context has already reached the deadline, so
// against a real service it fails before a request leaves, and the seam
// records a failed call. The service is probe k's turn among the set's
// services, or the first service the token's catalog lists when it lacks
// that one. The revoked token joins the revoked tokens bad_token draws from;
// it is never logged.
//
// The probe skips, with a warning, when the pool is empty, the revoke failed
// or the token's catalog lists no service. When ctx ends, the run is over and
// the probe returns without recording: an abandoned probe is neither a delay
// nor a timeout, and a probe that starts once ctx ended, as one whose tick
// raced Close can, takes no token. The revoke and the reissue are each
// bounded by o.OpTimeout and run on a context the run's end does not cancel,
// as the engine runs an operation: a probe the end catches during either
// still puts a token back for the pair, and neither reports the end as a
// failure. Each poll is bounded by o.OpTimeout and the deadline and ends with
// ctx.
func (s *Set) propagate(ctx context.Context, k int, rng *rand.Rand) {
	if ctx.Err() != nil {
		// A tick that raced Close's cancel: the run is over, so the probe
		// revokes nothing.
		return
	}
	e, ok := s.pool.take(rng)
	if !ok {
		slog.Warn("propagation probe skipped; the token pool is empty")
		return
	}
	revokedAt, revokeErr, _ := s.revokeAndReplace(context.WithoutCancel(ctx), e, s.o.OpTimeout)
	if revokeErr != nil {
		slog.Warn("propagation probe skipped; the revoke failed", "error", revokeErr)
		return
	}
	// The ring ages its tokens on the set's clock, the one bad_token reads;
	// revokedAt measures the delay.
	s.revoked.add(e.token, s.now())

	svc := s.services[k%len(s.services)]
	if e.endpoints[svc] == "" {
		listed := listedServices(e.endpoints)
		if len(listed) == 0 {
			slog.Warn("propagation probe skipped; the token's catalog lists no service")
			return
		}
		svc = listed[0]
	}
	if ctx.Err() != nil {
		// The run ended during the revoke or the reissue: the probe is
		// abandoned before its first poll, which could only fail.
		return
	}

	timeout := s.o.Propagation.Timeout
	deadline := revokedAt.Add(timeout)
	for {
		if s.pollRejected(ctx, svc, e.endpoints[svc], e.token, deadline) {
			s.recordPropagation(ctx, time.Since(revokedAt), true)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if !time.Now().Before(deadline) {
			s.recordPropagation(ctx, timeout, false)
			slog.Warn("propagation probe timed out; the service still accepted the revoked token", "service", svc, "timeout", timeout)
			return
		}
		// The next poll comes pollEvery after this one returned, or at the
		// deadline when that comes first.
		if !sleepCtx(ctx, min(s.pollEvery, time.Until(deadline))) {
			return
		}
	}
}

// pollRejected sends one poll of the propagation probe with token to svc at
// endpoint, on ctx bounded by the earlier of the probe's deadline and
// o.OpTimeout from now, and reports whether the service refused the token. A
// failed poll is no answer and reports false; its failure is in the samples
// the seam records.
func (s *Set) pollRejected(ctx context.Context, svc, endpoint, token string, deadline time.Time) bool {
	by := time.Now().Add(s.o.OpTimeout)
	if deadline.Before(by) {
		by = deadline
	}
	pollCtx, cancel := context.WithDeadline(ctx, by)
	defer cancel()
	rejected, _ := s.svc.Poll(pollCtx, svc, endpoint, token)
	return rejected
}

// recordPropagation records one completed propagation probe: d is its delay,
// or the timeout when ok is false. It goes into the set's delays, which
// ServiceStats reads, and through the seam, which records it as the run's
// time-to-ready sample and in telemetry.
func (s *Set) recordPropagation(ctx context.Context, d time.Duration, ok bool) {
	s.delays.RecordReadiness(metrics.Readiness{Type: probe.KindRevocation, Duration: d, OK: ok})
	s.svc.RecordRevocationDelay(ctx, d, ok)
}

// sleepCtx waits d or until ctx ends, and reports whether it waited the full
// d.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Close stops the refresher and the propagation probe and waits for the
// refresher, the probe's ticker goroutine and every probe in flight to
// return. A probe cut short this way records no delay and no timeout; one
// that is revoking or reissuing finishes both first, each within o.OpTimeout,
// so the pool keeps its size. After Close neither starts. It is safe to call
// more than once, and before or without Prime.
func (s *Set) Close() {
	s.mu.Lock()
	s.closed = true
	stops := []context.CancelFunc{s.stop, s.propStop}
	dones := []chan struct{}{s.done, s.propDone}
	s.mu.Unlock()
	for _, stop := range stops {
		if stop != nil {
			stop()
		}
	}
	for _, done := range dones {
		if done != nil {
			<-done
		}
	}
}

// issueBounded issues a token for pair i, a pool pair or a fan user's, on a
// context bounded by o.OpTimeout, for the issues Prime and the refresher make
// outside any operation.
func (s *Set) issueBounded(ctx context.Context, i int) (entry, error) {
	ctx, cancel := context.WithTimeout(ctx, s.o.OpTimeout)
	defer cancel()
	return s.issue(ctx, i)
}

// issue issues a token for pair i and returns it as an entry of the pool or
// the fan pool, carrying the endpoints of the token's own catalog.
func (s *Set) issue(ctx context.Context, i int) (entry, error) {
	p := &s.pairs[i]
	t, err := s.c.IssueScopedToken(ctx, p.issue, p.domainID, p.password, p.projectID)
	if err != nil {
		return entry{}, err
	}
	return entry{token: t.ID, expiresAt: t.ExpiresAt, pair: i, endpoints: t.Endpoints}, nil
}

// Services returns the services the first pool token's catalog listed, in
// probe.Services order, once Prime ran, and nil before or when the run needs
// none.
func (s *Set) Services() []string {
	return slices.Clone(s.services)
}

// ServiceStats is what the probe, bad_token and propagation draws recorded,
// for the run record: the probe and bad-token samples per service, Type
// being the service, and the propagation delays.
type ServiceStats struct {
	Probes      metrics.Aggregate
	BadTokens   metrics.Aggregate
	Propagation PropagationStats
}

// PropagationStats is the outcome of the propagation probes so far: the
// probes that completed, those among them that reached the timeout, and the
// delays of all of them, a timed-out probe counting as the timeout.
type PropagationStats struct {
	Every, Timeout   time.Duration
	Probes, TimedOut int
	Delay            metrics.Latency
}

// ServiceStats returns the probe and bad-token samples aggregated over wall,
// which sets their throughput, and the propagation probe's settings and
// outcome; the outcome is all zero while no probe completed. A probe the run's
// end cut short is not in it. It is safe to call while the workers and the
// propagation probes record.
func (s *Set) ServiceStats(wall time.Duration) ServiceStats {
	ss := ServiceStats{
		Probes:      s.probes.Aggregate(wall),
		BadTokens:   s.badTokens.Aggregate(wall),
		Propagation: PropagationStats{Every: s.o.Propagation.Every, Timeout: s.o.Propagation.Timeout},
	}
	for _, r := range s.delays.Aggregate(0).Readiness {
		if r.Type == probe.KindRevocation {
			ss.Propagation.Probes = r.Count
			ss.Propagation.TimedOut = r.Count - r.OK
			ss.Propagation.Delay = r.Latency
		}
	}
	return ss
}

// Operations returns the seven operations in the order validate, effective,
// revoke, grant_churn, issue, probe, bad_token, each with its weight from
// o.Weights (0 when missing), for pressure.Run. probe and bad_token come last,
// so the picker's cumulative weights of the first five, and with them a
// worker's draws for a given seed, are those of a run without the two while
// both weights are 0. Each Do makes one attempt and never retries: a load
// generator records what the cloud answered. Each draws only from the rng the
// engine hands it, the calling worker's own generator.
func (s *Set) Operations() []pressure.Operation {
	return []pressure.Operation{
		{Name: OpValidate, Weight: s.o.Weights[OpValidate], Do: s.validate},
		{Name: OpEffective, Weight: s.o.Weights[OpEffective], Do: s.effective},
		{Name: OpRevoke, Weight: s.o.Weights[OpRevoke], Do: s.revoke},
		{Name: OpGrantChurn, Weight: s.o.Weights[OpGrantChurn], Do: s.grantChurn},
		{Name: OpIssue, Weight: s.o.Weights[OpIssue], Do: s.reissue},
		{Name: OpProbe, Weight: s.o.Weights[OpProbe], Do: s.probe},
		{Name: OpBadToken, Weight: s.o.Weights[OpBadToken], Do: s.badToken},
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

// effective lists a user's effective role assignments, the projects that user
// can access, and the projects the user's token itself can access, in that
// order. The user is a random fan user when the set has any, with the token of
// its fan pool entry, so the listings grow with the fan's grants; otherwise it
// is the user of a random pool token. The first error ends the operation.
func (s *Set) effective(ctx context.Context, rng *rand.Rand) error {
	p := &s.pool
	if len(s.fan) > 0 {
		p = &s.fanPool
	}
	e, ok := p.peek(rng)
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
// first. A failed reissue shrinks the pool by one, which it logs. A revoked
// token joins the revoked tokens bad_token draws from, at the set's clock.
func (s *Set) revoke(ctx context.Context, rng *rand.Rand) error {
	e, ok := s.pool.take(rng)
	if !ok {
		return ErrPoolEmpty
	}
	// The engine bounds the operation as a whole, so the calls get no bound
	// of their own.
	_, revokeErr, reissueErr := s.revokeAndReplace(ctx, e, 0)
	if revokeErr != nil {
		return revokeErr
	}
	s.revoked.add(e.token, s.now())
	return reissueErr
}

// revokeAndReplace revokes e's token, which the caller took from the pool,
// and then always issues a token for the same pair and puts it into the pool,
// so a failed revoke does not shrink the pool; a failed reissue does, which it
// logs. A positive bound bounds the revoke and the reissue each, as
// issueBounded bounds an issue, for a caller outside the engine's per-operation
// deadline; 0 leaves ctx as it is. It returns the time the revoke returned,
// read from time.Now so its monotonic reading measures a delay across a clock
// change, the revoke's error and the reissue's error.
func (s *Set) revokeAndReplace(ctx context.Context, e entry, bound time.Duration) (revokedAt time.Time, revokeErr, reissueErr error) {
	callCtx, cancel := withBound(ctx, bound)
	revokeErr = s.c.RevokeToken(callCtx, e.token)
	cancel()
	revokedAt = time.Now()

	callCtx, cancel = withBound(ctx, bound)
	next, reissueErr := s.issue(callCtx, e.pair)
	cancel()
	if reissueErr != nil {
		slog.Warn("token pool shrank; a reissue failed", "size", s.pool.len(), "error", reissueErr)
	} else {
		s.pool.put(next)
	}
	return revokedAt, revokeErr, reissueErr
}

// withBound returns ctx bounded by d when d is positive, and ctx itself with
// a no-op cancel otherwise.
func withBound(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// grantChurn removes a random churn grant and grants it again, so Keystone's
// assignment caches keep being invalidated. It tries the drawn grant's lock
// and then the others' round-robin without waiting, and blocks on the drawn
// one only when every grant is busy, so a lock wait rarely inflates the
// measured latency. The re-grant runs even after a failed removal, so the
// grant is restored whenever the cloud allows it; the first error is
// returned. Neither a pool user's nor a fan user's grant is ever touched.
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

// probe sends a random pool token to a service its catalog lists, drawn at
// random among them, for that service's cheapest authenticated read, so the
// keystonemiddleware inside the service validates it. The token stays in the
// pool. The draw is timed into the set's probe samples under the service, and
// the read's error is returned as is.
func (s *Set) probe(ctx context.Context, rng *rand.Rand) error {
	if s.svc == nil {
		return errNoServiceClient
	}
	e, ok := s.pool.peek(rng)
	if !ok {
		return ErrPoolEmpty
	}
	svcs := listedServices(e.endpoints)
	if len(svcs) == 0 {
		return ErrNoService
	}
	svc := svcs[rng.Intn(len(svcs))]
	start := time.Now()
	err := s.svc.Probe(ctx, svc, e.endpoints[svc], e.token)
	s.probes.Record(metrics.Sample{Type: svc, Duration: time.Since(start), Success: err == nil, ErrKind: Classify(err)})
	return err
}

// badToken sends a token the service has to refuse to a service a random pool
// token's catalog lists, drawn as probe draws it, so the services' rejection
// path carries load. The pool token only lends its catalog and its length.
// The bad token is, by an even draw, a pool token revoked at least
// revokedTokenAge ago when there is one, and otherwise a random string of the
// pool token's length over the alphabet of a Fernet token. A 401 is the
// expected answer and a success; a service that accepted the token fails the
// draw with probe.ErrAccepted. The draw is timed into the set's bad-token
// samples under the service.
func (s *Set) badToken(ctx context.Context, rng *rand.Rand) error {
	if s.svc == nil {
		return errNoServiceClient
	}
	e, ok := s.pool.peek(rng)
	if !ok {
		return ErrPoolEmpty
	}
	svcs := listedServices(e.endpoints)
	if len(svcs) == 0 {
		return ErrNoService
	}
	svc := svcs[rng.Intn(len(svcs))]
	// The even draw happens only when a revoked token is eligible, so a run
	// whose ring is empty draws exactly as one without a ring would.
	tok, ok := s.revoked.pick(rng, s.now())
	if !ok || rng.Intn(2) != 0 {
		tok = randomToken(rng, len(e.token))
	}
	start := time.Now()
	err := s.svc.Reject(ctx, svc, e.endpoints[svc], tok)
	s.badTokens.Record(metrics.Sample{Type: svc, Duration: time.Since(start), Success: err == nil, ErrKind: Classify(err)})
	return err
}

// listedServices returns the services of probe.Services that endpoints has a
// URL for, in that order, and nil when it has none.
func listedServices(endpoints map[string]string) []string {
	var listed []string
	for _, svc := range probe.Services {
		if endpoints[svc] != "" {
			listed = append(listed, svc)
		}
	}
	return listed
}

// tokenAlphabet is the alphabet of a Fernet token, the URL-safe base64 one,
// so a random bad token looks like a real one to the service's parser.
const tokenAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// randomToken returns a string of n characters drawn from rng over
// tokenAlphabet. It is no credential: Keystone never issued it.
func randomToken(rng *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = tokenAlphabet[rng.Intn(len(tokenAlphabet))]
	}
	return string(b)
}

// GrowFan grants role assignments to every fan user until each holds at
// least n. A user holding h grants gets one on each of the first n-h
// projects, in plan order, that it holds none on; the j-th of them (0-based)
// gets the role roles[(h+j) % len(roles)]. Each grant runs through
// keystoneexec.WithRetry bounded by o.OpTimeout, with at most concurrency
// grants in flight (at least 1). It returns the created resources, in
// completion order, those created before a failure included, and the first
// error. A user already holding n grants is left as it is, so a second call
// with the same n grants nothing.
//
// A user's grants are counted as the projects it holds a grant on, so a
// project the plan grants it with two roles counts once. An n above the
// plan's projects fails before any grant, and a cancelled ctx stops GrowFan
// before the next grant. After the first failure no further grant starts:
// GrowFan waits for the grants in flight and names the failing user with the
// number of grants that user got in this call. A set without fan users grants
// nothing.
func (s *Set) GrowFan(ctx context.Context, n, concurrency int) ([]resource.Resource, error) {
	if len(s.fan) == 0 {
		return nil, nil
	}
	if n > len(s.projects) {
		return nil, fmt.Errorf("fan assignments (%d) exceed the plan's %d projects", n, len(s.projects))
	}
	concurrency = max(concurrency, 1)

	var grants []fanGrant
	for _, u := range s.fan {
		u.mu.Lock()
		h, j := len(u.held), 0
		for _, project := range s.projects {
			if h+j >= n {
				break
			}
			if u.held[project.Name] {
				continue
			}
			grants = append(grants, fanGrant{u: u, project: project, role: s.roles[(h+j)%len(s.roles)]})
			j++
		}
		u.mu.Unlock()
	}

	var (
		mu      sync.Mutex // guards the four below
		created = make([]resource.Resource, 0, len(grants))
		granted = make(map[*fanUser]int) // grants made per user in this call
		failed  *fanUser                 // the user of the first failure
		cause   error
		wg      sync.WaitGroup
	)
	sem := make(chan struct{}, concurrency)
	for _, g := range grants {
		sem <- struct{}{}
		mu.Lock()
		if failed == nil && ctx.Err() != nil {
			failed, cause = g.u, ctx.Err()
		}
		stop := failed != nil
		mu.Unlock()
		if stop {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res, err := s.grantFan(ctx, g)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if failed == nil {
					failed, cause = g.u, err
				}
				return
			}
			created = append(created, res)
			granted[g.u]++
		}()
	}
	wg.Wait()
	if failed != nil {
		return created, fmt.Errorf("growing fan user %q to %d assignments (%d granted): %w", failed.name, n, granted[failed], cause)
	}
	return created, nil
}

// grantFan makes grant g through keystoneexec.WithRetry, each attempt bounded
// by o.OpTimeout, and records the project in the fan user's held set once the
// grant succeeded.
func (s *Set) grantFan(ctx context.Context, g fanGrant) (resource.Resource, error) {
	a := keystoneplan.Assignment{User: g.u.name, Role: g.role.Name, Project: g.project.Name}
	var res resource.Resource
	err := keystoneexec.WithRetry(ctx, s.o.OpTimeout, func(ctx context.Context) error {
		r, err := s.c.AssignRole(ctx, a, g.u.userID, g.project.ID, g.u.domainID, g.role.ID)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return resource.Resource{}, err
	}
	g.u.mu.Lock()
	g.u.held[g.project.Name] = true
	g.u.mu.Unlock()
	return res, nil
}
