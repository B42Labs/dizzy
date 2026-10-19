package keystoneops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneexec "github.com/B42Labs/dizzy/internal/keystone/executor"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// TestMain silences the pool's warnings so the package's test output stays
// readable. Tests that assert on a log line install their own handler with
// captureLogs.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// The kinds of call the fake records, one per Keystone method.
const (
	kindIssue        = "issue"
	kindValidate     = "validate"
	kindRevoke       = "revoke"
	kindEffective    = "effective"
	kindUserProjects = "user_projects"
	kindAvailable    = "available"
	kindAssign       = "assign"
	kindDelete       = "delete"
)

// call is one call the fake saw, with the arguments of its method; the fields
// a method does not take stay empty.
type call struct {
	kind      string
	user      string // TokenIssue.User of an issue
	project   string // TokenIssue.Project of an issue
	domainID  string // the user's domain id of an issue or an assign
	password  string
	projectID string
	userID    string
	roleID    string
	token     string
	a         keystoneplan.Assignment
	res       resource.Resource
	deadline  time.Time // the context deadline of an issue; zero when it had none
}

// fakeClock is a clock tests move by hand. It is safe for concurrent use.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// newFakeClock returns a clock standing at a fixed instant.
func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
}

// Now returns the clock's current instant.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Set moves the clock to t.
func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// fakeKeystone is an in-process Keystone that records every call with its
// arguments in call order, issues the tokens "tok-1", "tok-2", ... expiring
// lifetime after its clock's instant (no expiry when lifetime is 0), and fails
// a call of a kind on request: every call of the kind, or only its n-th call.
// It tracks how many issues are in flight at once, can hold the first issues
// until a number of them overlap, and can hold issues at a gate the test
// opens one issue at a time or all at once. It is safe for concurrent use by
// the pool, the refresher and the engine's workers.
type fakeKeystone struct {
	clock    *fakeClock
	lifetime time.Duration
	// lifetimes overrides lifetime for the first tokens: the n-th token
	// issued lives lifetimes[n-1]. latency is how far each successful issue
	// moves the clock before the token's expiry is computed, as the time a
	// password login takes.
	lifetimes []time.Duration
	latency   time.Duration

	mu       sync.Mutex
	calls    []call
	count    map[string]int           // calls per kind so far
	failKind map[string]error         // every call of a kind fails with it
	failAt   map[string]map[int]error // the n-th call (1-based) of a kind fails with it
	issued   int

	inFlight, maxInFlight int // IssueScopedToken calls running at once

	// The first rendezvous issues wait until rendezvous issues are in flight
	// together; met is closed once they were.
	rendezvous int
	met        chan struct{}
	metClosed  bool

	// While gate is set, every issue the fake does not fail waits on it: a
	// send lets one held issue through, closing it lets every issue through.
	gate     chan struct{}
	gateOpen bool
}

// newFake returns a fake whose tokens live lifetime on clock.
func newFake(clock *fakeClock, lifetime time.Duration) *fakeKeystone {
	return &fakeKeystone{
		clock:    clock,
		lifetime: lifetime,
		count:    make(map[string]int),
		failKind: make(map[string]error),
		failAt:   make(map[string]map[int]error),
	}
}

// fail makes every later call of kind fail with err; a nil err stops that.
func (f *fakeKeystone) fail(kind string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failKind[kind] = err
}

// failNth makes the n-th call (1-based, counted from the fake's creation) of
// kind fail with err.
func (f *fakeKeystone) failNth(kind string, n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAt[kind] == nil {
		f.failAt[kind] = make(map[int]error)
	}
	f.failAt[kind][n] = err
}

// holdIssues makes the first n issues wait until n issues are in flight at
// once, so a test can prove they ran concurrently.
func (f *fakeKeystone) holdIssues(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rendezvous = n
	f.met = make(chan struct{})
}

// begin records c and returns its 1-based number among the calls of its kind
// and the error the fake was told to fail it with.
func (f *fakeKeystone) begin(c call) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count[c.kind]++
	n := f.count[c.kind]
	f.calls = append(f.calls, c)
	if err := f.failAt[c.kind][n]; err != nil {
		return n, err
	}
	return n, f.failKind[c.kind]
}

// recorded returns a copy of the calls of the given kinds in call order, or
// of every call when no kind is given.
func (f *fakeKeystone) recorded(kinds ...string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if len(kinds) == 0 || containsKind(kinds, c.kind) {
			out = append(out, c)
		}
	}
	return out
}

// mark returns the number of calls recorded so far, so a test can read the
// calls one operation made with since.
func (f *fakeKeystone) mark() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// since returns a copy of the calls recorded after mark m.
func (f *fakeKeystone) since(m int) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls[m:]...)
}

// maxIssuesInFlight returns the most issues that were in flight at once.
func (f *fakeKeystone) maxIssuesInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

// issuesInFlight returns the number of issues in flight now.
func (f *fakeKeystone) issuesInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight
}

// setIssueTiming makes every successful issue move the clock by latency and
// the first tokens live lifetimes, one per token, instead of the fake's
// lifetime.
func (f *fakeKeystone) setIssueTiming(latency time.Duration, lifetimes ...time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latency = latency
	f.lifetimes = lifetimes
}

// gateIssues holds every later issue that the fake does not fail at a gate
// until releaseIssue lets it through or openGate opens the gate. The tests
// that gate issues run in a synctest bubble and call synctest.Wait to know
// that every issue that can start has reached the gate. The gate opens when
// the test ends, so no issue stays held after a failed test.
func (f *fakeKeystone) gateIssues(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	f.gate = make(chan struct{})
	f.mu.Unlock()
	t.Cleanup(f.openGate)
}

// releaseIssue lets one issue held at the gate through, failing the test when
// no issue is held there.
func (f *fakeKeystone) releaseIssue(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	select {
	case gate <- struct{}{}:
	default:
		t.Fatal("no issue is held at the gate")
	}
}

// openGate lets every held and every later issue through. It is safe to call
// more than once and without a gate.
func (f *fakeKeystone) openGate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate != nil && !f.gateOpen {
		close(f.gate)
		f.gateOpen = true
	}
}

// containsKind reports whether kinds holds k.
func containsKind(kinds []string, k string) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// IssueScopedToken records the issue with its context deadline, holds it for
// the rendezvous when asked to, and then fails it, or holds it at the gate
// when one is set and issues the next token.
func (f *fakeKeystone) IssueScopedToken(ctx context.Context, t keystoneplan.TokenIssue, userDomainID, password, projectID string) (keystone.IssuedToken, error) {
	deadline, _ := ctx.Deadline()
	n, err := f.begin(call{kind: kindIssue, user: t.User, project: t.Project, domainID: userDomainID, password: password, projectID: projectID, deadline: deadline})

	f.mu.Lock()
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	met := f.met
	wait := met != nil && n <= f.rendezvous
	if met != nil && !f.metClosed && f.inFlight >= f.rendezvous {
		close(met)
		f.metClosed = true
	}
	gate := f.gate
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	if wait {
		select {
		case <-met:
		case <-ctx.Done():
			return keystone.IssuedToken{}, ctx.Err()
		case <-time.After(10 * time.Second):
			return keystone.IssuedToken{}, fmt.Errorf("fake: %d issues were never in flight together", f.rendezvous)
		}
	}
	if err != nil {
		return keystone.IssuedToken{}, err
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return keystone.IssuedToken{}, ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued++
	if f.latency > 0 {
		f.clock.Set(f.clock.Now().Add(f.latency))
	}
	lifetime := f.lifetime
	if f.issued <= len(f.lifetimes) {
		lifetime = f.lifetimes[f.issued-1]
	}
	tok := keystone.IssuedToken{ID: fmt.Sprintf("tok-%d", f.issued)}
	if lifetime > 0 {
		tok.ExpiresAt = f.clock.Now().Add(lifetime)
	}
	return tok, nil
}

// ValidateToken records the validation and returns the injected error, if any.
func (f *fakeKeystone) ValidateToken(_ context.Context, token string) error {
	_, err := f.begin(call{kind: kindValidate, token: token})
	return err
}

// RevokeToken records the revocation and returns the injected error, if any.
func (f *fakeKeystone) RevokeToken(_ context.Context, token string) error {
	_, err := f.begin(call{kind: kindRevoke, token: token})
	return err
}

// ListEffectiveAssignments records the listing and returns one assignment or
// the injected error.
func (f *fakeKeystone) ListEffectiveAssignments(_ context.Context, userID string) (int, error) {
	if _, err := f.begin(call{kind: kindEffective, userID: userID}); err != nil {
		return 0, err
	}
	return 1, nil
}

// ListUserProjects records the listing and returns one project or the
// injected error.
func (f *fakeKeystone) ListUserProjects(_ context.Context, userID string) (int, error) {
	if _, err := f.begin(call{kind: kindUserProjects, userID: userID}); err != nil {
		return 0, err
	}
	return 1, nil
}

// ListAvailableProjects records the listing and returns one project or the
// injected error.
func (f *fakeKeystone) ListAvailableProjects(_ context.Context, token string) (int, error) {
	if _, err := f.begin(call{kind: kindAvailable, token: token}); err != nil {
		return 0, err
	}
	return 1, nil
}

// AssignRole records the grant and returns its assignment resource or the
// injected error.
func (f *fakeKeystone) AssignRole(_ context.Context, a keystoneplan.Assignment, userID, projectID, domainID, roleID string) (resource.Resource, error) {
	if _, err := f.begin(call{kind: kindAssign, a: a, userID: userID, projectID: projectID, domainID: domainID, roleID: roleID}); err != nil {
		return resource.Resource{}, err
	}
	return keystone.AssignmentResource(a, userID, projectID, domainID, roleID), nil
}

// Delete records the deletion and returns the injected error, if any.
func (f *fakeKeystone) Delete(_ context.Context, r resource.Resource) error {
	_, err := f.begin(call{kind: kindDelete, res: r})
	return err
}

// The fake must satisfy the seam.
var _ Keystone = (*fakeKeystone)(nil)

// errFake is the error the tests inject when only its identity matters.
var errFake = errors.New("fake: injected failure")

// pressurePlan is a self-consistent plan in two domains: four users, three
// distinct project-scoped pool pairs among the first three users
// ((user-0001, proj-0001) granted twice, (user-0002, proj-0002) and
// (user-0003, proj-0001)), a domain-scoped grant of user-0003, and user-0004,
// the last user, holding a project-scoped grant on proj-0002 and a
// domain-scoped one.
func pressurePlan() *keystoneplan.Plan {
	return &keystoneplan.Plan{
		Scenario: "pressure", Seed: 7,
		Domains:  []keystoneplan.Domain{{Name: "dom-0001"}, {Name: "dom-0002"}},
		Roles:    []keystoneplan.Role{{Name: "role-0001"}, {Name: "role-0002"}},
		Projects: []keystoneplan.Project{{Name: "proj-0001", Domain: "dom-0001"}, {Name: "proj-0002", Domain: "dom-0002"}},
		Users: []keystoneplan.User{
			{Name: "user-0001", Domain: "dom-0001"},
			{Name: "user-0002", Domain: "dom-0002"},
			{Name: "user-0003", Domain: "dom-0001"},
			{Name: "user-0004", Domain: "dom-0001"},
		},
		Assignments: []keystoneplan.Assignment{
			{User: "user-0001", Role: "role-0001", Project: "proj-0001"},
			{User: "user-0001", Role: "role-0002", Project: "proj-0001"},
			{User: "user-0002", Role: "role-0001", Project: "proj-0002"},
			{User: "user-0003", Role: "role-0001"},
			{User: "user-0003", Role: "role-0002", Project: "proj-0001"},
			{User: "user-0004", Role: "role-0001", Project: "proj-0002"},
			{User: "user-0004", Role: "role-0002"},
		},
		Tokens: []keystoneplan.TokenIssue{{User: "user-0001", Project: "proj-0001"}},
	}
}

// fixtureBindings are the root bindings of pressurePlan, as Provision would
// return them.
func fixtureBindings() keystoneexec.Bindings {
	return keystoneexec.Bindings{
		Domains: map[string]string{"dom-0001": "did-1", "dom-0002": "did-2"},
		Roles:   map[string]string{"role-0001": "rid-1", "role-0002": "rid-2"},
	}
}

// fixtureIDs are the cloud ids of pressurePlan's projects and users.
func fixtureIDs() map[string]string {
	return map[string]string{
		"proj-0001": "pid-1", "proj-0002": "pid-2",
		"user-0001": "uid-1", "user-0002": "uid-2", "user-0003": "uid-3", "user-0004": "uid-4",
	}
}

// fixturePasswords are the passwords of pressurePlan's users.
func fixturePasswords() map[string]string {
	return map[string]string{"user-0001": "pw-1", "user-0002": "pw-2", "user-0003": "pw-3", "user-0004": "pw-4"}
}

// fixtureOptions are valid options for pressurePlan with the given pool size
// and weights, reserving one churn user.
func fixtureOptions(tokens int, weights map[string]int) Options {
	return Options{Tokens: tokens, ChurnUsers: 1, Weights: maps.Clone(weights), OpTimeout: 5 * time.Second}
}

// buildSet builds a set over pressurePlan against f with o, reading the time
// from f's clock, and closes it when the test ends.
func buildSet(t *testing.T, f *fakeKeystone, o Options) *Set {
	t.Helper()
	s, err := Build(pressurePlan(), f, fixtureBindings(), fixtureIDs(), fixturePasswords(), o)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	s.now = f.clock.Now
	t.Cleanup(s.Close)
	return s
}

// primedSet builds a set over pressurePlan against a fake whose tokens live
// lifetime, and primes a pool of tokens with concurrency 2.
func primedSet(t *testing.T, lifetime time.Duration, tokens int, weights map[string]int) (*Set, *fakeKeystone) {
	t.Helper()
	f := newFake(newFakeClock(), lifetime)
	s := buildSet(t, f, fixtureOptions(tokens, weights))
	if err := s.Prime(context.Background(), 2); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return s, f
}

// poolTokens returns the pool's tokens with the pair index each was issued
// for.
func poolTokens(s *Set) map[string]int {
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	out := make(map[string]int, len(s.pool.entries))
	for _, e := range s.pool.entries {
		out[e.token] = e.pair
	}
	return out
}

// poolEntries returns a copy of the pool's entries in pool order.
func poolEntries(s *Set) []entry {
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	return append([]entry(nil), s.pool.entries...)
}

// operation returns the set's operation named name.
func operation(t *testing.T, s *Set, name string) func(context.Context, *rand.Rand) error {
	t.Helper()
	for _, op := range s.Operations() {
		if op.Name == name {
			return op.Do
		}
	}
	t.Fatalf("no operation %q", name)
	return nil
}

// syncBuffer is a bytes.Buffer safe for the concurrent writes of a log
// handler and the test's reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

// Write appends p.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// String returns everything written so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// captureLogs routes the default logger into a buffer until the test ends.
// The tests that use it do not run in parallel, since the default logger is
// process-wide.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}
