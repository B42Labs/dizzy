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
	"github.com/B42Labs/dizzy/internal/pressure/probe"
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
	deadline  time.Time // the context deadline of an issue, a revoke or a grant; zero when it had none
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
// Its tokens list no endpoint unless the test sets a catalog, for every token
// or from the n-th on. It tracks how many issues are in flight at once, can
// hold the first issues until a number of them overlap, and can hold issues at
// a gate the test opens one issue at a time or all at once. It tracks and
// gates role grants the same way, with a gate of their own, and can hold
// revocations at a third gate. It is safe for concurrent use by the pool, the
// refresher, the propagation probe, the engine's workers and GrowFan.
type fakeKeystone struct {
	clock    *fakeClock
	lifetime time.Duration
	// lifetimes overrides lifetime for the first tokens: the n-th token
	// issued lives lifetimes[n-1]. latency is how far each successful issue
	// moves the clock before the token's expiry is computed, as the time a
	// password login takes.
	lifetimes []time.Duration
	latency   time.Duration
	// endpoints is the catalog the tokens carry, a copy each, as
	// IssuedToken.Endpoints, from the endpointsFrom-th token issued on
	// (1-based; 0 is every token); an earlier token lists none. nil, the
	// default, lists none, as a client without endpoint resolution returns.
	endpoints     map[string]string
	endpointsFrom int

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

	// assigning and maxAssigning count the AssignRole calls in flight now and
	// at most. assignGate holds the grants as gate holds the issues.
	assigning, maxAssigning int
	assignGate              chan struct{}
	assignGateOpen          bool

	// revokeGate holds the revocations as gate holds the issues, until it is
	// closed or the revoke's context ends.
	revokeGate     chan struct{}
	revokeGateOpen bool
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

// setEndpoints makes every later token carry a copy of eps as its catalog's
// endpoints; a nil eps lists none again.
func (f *fakeKeystone) setEndpoints(eps map[string]string) {
	f.setEndpointsFrom(0, eps)
}

// setEndpointsFrom makes the n-th token issued (1-based, counted from the
// fake's creation) and every later one carry a copy of eps, and every earlier
// one list none.
func (f *fakeKeystone) setEndpointsFrom(n int, eps map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpoints, f.endpointsFrom = maps.Clone(eps), n
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

// gateAssigns holds every later grant that the fake does not fail at a gate
// until releaseAssign lets it through, as gateIssues holds issues. The gate
// opens when the test ends, so no grant stays held after a failed test.
func (f *fakeKeystone) gateAssigns(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	f.assignGate = make(chan struct{})
	f.mu.Unlock()
	t.Cleanup(f.openAssignGate)
}

// releaseAssign lets one grant held at the gate through, failing the test when
// no grant is held there.
func (f *fakeKeystone) releaseAssign(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	gate := f.assignGate
	f.mu.Unlock()
	select {
	case gate <- struct{}{}:
	default:
		t.Fatal("no grant is held at the gate")
	}
}

// openAssignGate lets every held and every later grant through. It is safe to
// call more than once and without a gate.
func (f *fakeKeystone) openAssignGate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.assignGate != nil && !f.assignGateOpen {
		close(f.assignGate)
		f.assignGateOpen = true
	}
}

// gateRevokes holds every later revocation that the fake does not fail at a
// gate until openRevokeGate opens it or the revoke's context ends, as a
// Keystone that has not answered yet. The gate opens when the test ends, so
// no revocation stays held after a failed test.
func (f *fakeKeystone) gateRevokes(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	f.revokeGate = make(chan struct{})
	f.mu.Unlock()
	t.Cleanup(f.openRevokeGate)
}

// openRevokeGate lets every held and every later revocation through. It is
// safe to call more than once and without a gate.
func (f *fakeKeystone) openRevokeGate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revokeGate != nil && !f.revokeGateOpen {
		close(f.revokeGate)
		f.revokeGateOpen = true
	}
}

// assignsInFlight returns the number of grants in flight now.
func (f *fakeKeystone) assignsInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.assigning
}

// maxAssignsInFlight returns the most grants that were in flight at once.
func (f *fakeKeystone) maxAssignsInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxAssigning
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
	if f.issued >= f.endpointsFrom {
		tok.Endpoints = maps.Clone(f.endpoints)
	}
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

// RevokeToken records the revocation with its context deadline and returns
// the injected error, if any, or holds the revocation at the gate when one is
// set and returns the context's error when the context ends first.
func (f *fakeKeystone) RevokeToken(ctx context.Context, token string) error {
	deadline, _ := ctx.Deadline()
	_, err := f.begin(call{kind: kindRevoke, token: token, deadline: deadline})
	if err != nil {
		return err
	}
	f.mu.Lock()
	gate := f.revokeGate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
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

// AssignRole records the grant with its context deadline and then fails it,
// or holds it at the grant gate when one is set and returns its assignment
// resource.
func (f *fakeKeystone) AssignRole(ctx context.Context, a keystoneplan.Assignment, userID, projectID, domainID, roleID string) (resource.Resource, error) {
	deadline, _ := ctx.Deadline()
	_, err := f.begin(call{kind: kindAssign, a: a, userID: userID, projectID: projectID, domainID: domainID, roleID: roleID, deadline: deadline})

	f.mu.Lock()
	f.assigning++
	f.maxAssigning = max(f.maxAssigning, f.assigning)
	gate := f.assignGate
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.assigning--
		f.mu.Unlock()
	}()

	if err != nil {
		return resource.Resource{}, err
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return resource.Resource{}, ctx.Err()
		}
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

// The kinds of call the services fake records, one per Services method.
const (
	kindProbe      = "probe"
	kindReject     = "reject"
	kindPoll       = "poll"
	kindRevocation = "revocation_delay"
)

// svcCall is one call the services fake saw, with its arguments and its
// outcome; the fields a method does not take stay empty.
type svcCall struct {
	kind     string
	service  string
	endpoint string
	token    string
	began    time.Time     // when a poll began, read from time.Now
	deadline time.Time     // the context deadline of a poll; zero when it had none
	rejected bool          // what a poll answered
	err      error         // the error a probe, a reject or a poll returned
	d        time.Duration // the delay a RecordRevocationDelay recorded
	ok       bool          // the ok a RecordRevocationDelay recorded
}

// fakeServices is an in-process stand-in for Nova, Neutron, Cinder and
// Glance behind the Services seam. It records every Probe, Reject, Poll and
// RecordRevocationDelay call with its arguments and outcome in call order; a
// poll is recorded when it returns, so a recorded poll is one that finished.
// Probe and Reject return the answer set for their service, nil by default,
// which for Reject stands for the expected 401. A poll is rejected from the
// n-th poll of its token on when the test asks for it; any other poll returns
// false and its service's answer, so a fake whose answer is a 503 fails every
// poll. Polls can be made to block until their context ends, as a service
// that never answers does, and then take a moment more to return. A poll on
// an ended context fails with the context's error, as a real request does.
// It is safe for concurrent use by the workers and the propagation probes.
type fakeServices struct {
	mu        sync.Mutex
	calls     []svcCall
	answers   map[string]error // per service; a missing service answers nil
	rejectAt  int              // a token's polls from the rejectAt-th on are rejected; 0 never
	polls     map[string]int   // polls started per token
	blockPoll bool             // every poll waits for its context to end
	linger    time.Duration    // how long a blocked poll takes to return once its context ended
}

// newFakeServices returns a services fake whose probes and rejects succeed and
// whose polls are never rejected.
func newFakeServices() *fakeServices {
	return &fakeServices{answers: make(map[string]error), polls: make(map[string]int)}
}

// answer makes every later Probe, Reject and unrejected poll at service
// return err; a nil err restores the default.
func (fs *fakeServices) answer(service string, err error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.answers[service] = err
}

// answerAll sets err as the answer of every one of the four services.
func (fs *fakeServices) answerAll(err error) {
	for _, svc := range probe.Services {
		fs.answer(svc, err)
	}
}

// rejectFromPoll makes the n-th poll of every token, and every later one of
// it, answer that the service refused the token, as a service does once the
// revocation reached it.
func (fs *fakeServices) rejectFromPoll(n int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.rejectAt = n
}

// blockPolls makes every later poll wait until its context ends, then linger
// more, as a request takes a moment to unwind, and return the context's
// error. A test that runs in a synctest bubble sees the linger on the
// bubble's clock.
func (fs *fakeServices) blockPolls(linger time.Duration) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.blockPoll, fs.linger = true, linger
}

// record appends c to the calls.
func (fs *fakeServices) record(c svcCall) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.calls = append(fs.calls, c)
}

// answerOf returns the answer set for service.
func (fs *fakeServices) answerOf(service string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.answers[service]
}

// recorded returns a copy of the calls of the given kinds in call order, or
// of every call when no kind is given.
func (fs *fakeServices) recorded(kinds ...string) []svcCall {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []svcCall
	for _, c := range fs.calls {
		if len(kinds) == 0 || containsKind(kinds, c.kind) {
			out = append(out, c)
		}
	}
	return out
}

// polledWith returns the recorded polls that sent token, in call order.
func (fs *fakeServices) polledWith(token string) []svcCall {
	var out []svcCall
	for _, c := range fs.recorded(kindPoll) {
		if c.token == token {
			out = append(out, c)
		}
	}
	return out
}

// Probe records the probe and returns its service's answer.
func (fs *fakeServices) Probe(_ context.Context, service, endpoint, token string) error {
	err := fs.answerOf(service)
	fs.record(svcCall{kind: kindProbe, service: service, endpoint: endpoint, token: token, err: err})
	return err
}

// Reject records the reject and returns its service's answer, nil standing
// for the 401 the service owes a bad token.
func (fs *fakeServices) Reject(_ context.Context, service, endpoint, token string) error {
	err := fs.answerOf(service)
	fs.record(svcCall{kind: kindReject, service: service, endpoint: endpoint, token: token, err: err})
	return err
}

// Poll counts the poll for its token, blocks until ctx ends and lingers when
// asked to, and then answers: the context's error once ctx ended, a rejection
// from the token's rejectAt-th poll on, and otherwise its service's answer.
// It records the poll with the instant it began, its context deadline and
// its outcome when it returns.
func (fs *fakeServices) Poll(ctx context.Context, service, endpoint, token string) (bool, error) {
	began := time.Now()
	deadline, _ := ctx.Deadline()
	fs.mu.Lock()
	fs.polls[token]++
	n, rejectAt, block, linger, answer := fs.polls[token], fs.rejectAt, fs.blockPoll, fs.linger, fs.answers[service]
	fs.mu.Unlock()

	if block {
		<-ctx.Done()
		time.Sleep(linger)
	}
	var rejected bool
	var err error
	switch {
	case ctx.Err() != nil:
		err = ctx.Err()
	case rejectAt > 0 && n >= rejectAt:
		rejected = true
	default:
		err = answer
	}
	fs.record(svcCall{kind: kindPoll, service: service, endpoint: endpoint, token: token, began: began, deadline: deadline, rejected: rejected, err: err})
	return rejected, err
}

// RecordRevocationDelay records the delay and its ok.
func (fs *fakeServices) RecordRevocationDelay(_ context.Context, d time.Duration, ok bool) {
	fs.record(svcCall{kind: kindRevocation, d: d, ok: ok})
}

// The fake must satisfy the seam.
var _ Services = (*fakeServices)(nil)

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

// buildSet builds a set over pressurePlan against f and a services fake of
// its own with o, reading the time from f's clock, and closes it when the
// test ends.
func buildSet(t *testing.T, f *fakeKeystone, o Options) *Set {
	t.Helper()
	return buildServiceSet(t, f, newFakeServices(), o)
}

// buildServiceSet builds a set over pressurePlan against f and fs with o,
// reading the time from f's clock, and closes it when the test ends.
func buildServiceSet(t *testing.T, f *fakeKeystone, fs *fakeServices, o Options) *Set {
	t.Helper()
	s, err := Build(pressurePlan(), f, fs, fixtureBindings(), fixtureIDs(), fixturePasswords(), o)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	s.now = f.clock.Now
	t.Cleanup(s.Close)
	return s
}

// primedSet builds a set over pressurePlan against a fake whose tokens live
// lifetime and a services fake, and primes a pool of tokens with concurrency
// 2.
func primedSet(t *testing.T, lifetime time.Duration, tokens int, weights map[string]int) (*Set, *fakeKeystone) {
	t.Helper()
	f := newFake(newFakeClock(), lifetime)
	s := buildSet(t, f, fixtureOptions(tokens, weights))
	if err := s.Prime(context.Background(), 2); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return s, f
}

// fixtureEndpoints returns a catalog listing services, each at a URL naming
// it with the trailing "/" the client normalizes to, as IssuedToken.Endpoints
// carries them.
func fixtureEndpoints(services ...string) map[string]string {
	eps := make(map[string]string, len(services))
	for _, svc := range services {
		eps[svc] = "https://" + svc + ".example/"
	}
	return eps
}

// primedServiceSet builds a set over pressurePlan with o against a fake
// Keystone whose tokens live an hour and list eps, and a services fake. A
// positive pollEvery replaces the propagation probe's poll interval before
// Prime starts the probe. It primes the pool with concurrency 2.
func primedServiceSet(t *testing.T, o Options, eps map[string]string, pollEvery time.Duration) (*Set, *fakeKeystone, *fakeServices) {
	t.Helper()
	f := newFake(newFakeClock(), time.Hour)
	f.setEndpoints(eps)
	fs := newFakeServices()
	s := buildServiceSet(t, f, fs, o)
	if pollEvery > 0 {
		s.pollEvery = pollEvery
	}
	if err := s.Prime(context.Background(), 2); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return s, f, fs
}

// fanPlan is a self-consistent plan in dom-0001 with the two roles of
// fixtureBindings and the projects proj-0001 to proj-<projects>, at least
// three: the regular users user-0001, user-0002 and user-0003, each with a
// role-0001 grant on the project of its number, and then one fan user per
// entry of held, fan-0001 first, each with a role-0001 grant on every project
// its entry names, in that order, where an empty name is a domain-scoped
// grant. The regular users' grants come first among the assignments.
func fanPlan(projects int, held ...[]string) *keystoneplan.Plan {
	p := &keystoneplan.Plan{
		Scenario: "fan", Seed: 9,
		Domains: []keystoneplan.Domain{{Name: "dom-0001"}},
		Roles:   []keystoneplan.Role{{Name: "role-0001"}, {Name: "role-0002"}},
	}
	for i := range projects {
		p.Projects = append(p.Projects, keystoneplan.Project{Name: fmt.Sprintf("proj-%04d", i+1), Domain: "dom-0001"})
	}
	for i := range 3 {
		name := fmt.Sprintf("user-%04d", i+1)
		p.Users = append(p.Users, keystoneplan.User{Name: name, Domain: "dom-0001"})
		p.Assignments = append(p.Assignments, keystoneplan.Assignment{User: name, Role: "role-0001", Project: fmt.Sprintf("proj-%04d", i+1)})
	}
	for i, projects := range held {
		name := fmt.Sprintf("fan-%04d", i+1)
		p.Users = append(p.Users, keystoneplan.User{Name: name, Domain: "dom-0001", Fan: true})
		for _, project := range projects {
			p.Assignments = append(p.Assignments, keystoneplan.Assignment{User: name, Role: "role-0001", Project: project})
		}
	}
	return p
}

// fanIDs returns the cloud ids of p's projects and users, "id-" and the
// logical name, as Provision would return them.
func fanIDs(p *keystoneplan.Plan) map[string]string {
	ids := make(map[string]string, len(p.Projects)+len(p.Users))
	for _, pr := range p.Projects {
		ids[pr.Name] = "id-" + pr.Name
	}
	for _, u := range p.Users {
		ids[u.Name] = "id-" + u.Name
	}
	return ids
}

// fanPasswords returns the passwords of p's users, "pw-" and the logical
// name.
func fanPasswords(p *keystoneplan.Plan) map[string]string {
	passwords := make(map[string]string, len(p.Users))
	for _, u := range p.Users {
		passwords[u.Name] = "pw-" + u.Name
	}
	return passwords
}

// buildFanSet builds a set over p, a fanPlan, against f and a services fake
// with o, the bindings of fixtureBindings and the ids and passwords of fanIDs
// and fanPasswords, reading the time from f's clock, and closes it when the
// test ends.
func buildFanSet(t *testing.T, f *fakeKeystone, p *keystoneplan.Plan, o Options) *Set {
	t.Helper()
	s, err := Build(p, f, newFakeServices(), fixtureBindings(), fanIDs(p), fanPasswords(p), o)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	s.now = f.clock.Now
	t.Cleanup(s.Close)
	return s
}

// primedFanSet builds a set over p, a fanPlan, with o against a fake whose
// tokens live an hour, and primes it with concurrency 1, so the pool tokens
// and then the fan users' tokens are issued in order.
func primedFanSet(t *testing.T, p *keystoneplan.Plan, o Options) (*Set, *fakeKeystone) {
	t.Helper()
	f := newFake(newFakeClock(), time.Hour)
	s := buildFanSet(t, f, p, o)
	if err := s.Prime(context.Background(), 1); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return s, f
}

// poolTokens returns the pool's tokens with the pair index each was issued
// for.
func poolTokens(s *Set) map[string]int { return tokensOf(&s.pool) }

// fanTokens returns the fan pool's tokens with the pair index each was issued
// for.
func fanTokens(s *Set) map[string]int { return tokensOf(&s.fanPool) }

// tokensOf returns p's tokens with the pair index each was issued for.
func tokensOf(p *pool) map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.entries))
	for _, e := range p.entries {
		out[e.token] = e.pair
	}
	return out
}

// poolEntries returns a copy of the pool's entries in pool order.
func poolEntries(s *Set) []entry { return entriesOf(&s.pool) }

// fanEntries returns a copy of the fan pool's entries in pool order.
func fanEntries(s *Set) []entry { return entriesOf(&s.fanPool) }

// entriesOf returns a copy of p's entries in pool order.
func entriesOf(p *pool) []entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]entry(nil), p.entries...)
}

// revokedOf returns the tokens of s's revoked tokens in add order.
func revokedOf(s *Set) []string {
	s.revoked.mu.Lock()
	defer s.revoked.mu.Unlock()
	var out []string
	for _, t := range s.revoked.entries {
		out = append(out, t.token)
	}
	return out
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
