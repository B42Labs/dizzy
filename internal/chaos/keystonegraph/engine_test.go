package keystonegraph

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/gophercloud/gophercloud/v2"
)

// assignInfo records which user, project and role a live grant belongs to, so
// the fake can detect a parent deleted while a child grant is still live and
// match an application credential or trust to the grant it rests on.
type assignInfo struct {
	userID, projectID, roleID string
}

// delegationInfo records what a live application credential or trust rests on:
// its owner (the trustor of a trust), its trustee (empty for a credential), its
// project, and the role of the owner's grant on that project.
type delegationInfo struct {
	userID, trusteeID, projectID, roleID string
}

// fakeKeystone is an in-process Keystone that tracks the live population and the
// lifecycle invariants the churn graph must never violate, so the keystone chaos
// graph can be checked without a cloud. It is safe for concurrent use by the
// engine's operation tasks.
type fakeKeystone struct {
	mu     sync.Mutex
	nextID int

	liveProject  map[string]bool // logical -> a live instance exists
	liveUser     map[string]bool
	projLiveByID map[string]bool // project id -> live
	userLiveByID map[string]bool // user id -> live
	assignByID   map[string]assignInfo
	tokenPairs   map[string]bool // "user\x00project" pairs that back a token

	// live counts and high-water marks
	curP, maxP int
	curU, maxU int

	// invariant violations the graph must never cause
	grantOnAbsentUser    bool
	grantOnAbsentProject bool
	userDeleteWithGrant  bool
	projDeleteWithGrant  bool
	doubleLiveProject    bool
	doubleLiveUser       bool

	// token observation
	assignCreatesForToken int  // grants created that back a token issue
	tokenIssues           int  // total token issues drawn
	tokenOnDeadTarget     bool // a token issued against a not-live user/project

	// application credentials and trusts: the live instances by id with what
	// each rests on, and how often each created instance authenticated
	liveDelegation map[string]delegationInfo
	authsByID      map[string]int

	// delegation invariant violations the graph must never cause
	delegationOnAbsentParent   bool // created while its user, trustor, trustee or project is not live
	delegationWithoutGrant     bool // created while no live grant matches (user, project, role)
	grantDeleteWithDelegation  bool // an assignment deleted while a live credential or trust rests on it
	parentDeleteWithDelegation bool // a user or project deleted while a live credential or trust names it
	authOnDeadDelegation       bool // a credential or trust authenticated while it is not live
	doubleAuth                 bool // a credential or trust authenticated twice in one lifetime

	// per-kind delegation observation
	creates map[resource.Kind]int
	auths   map[resource.Kind]int
	deletes map[resource.Kind]int

	// deleteNotFound makes DeleteApplicationCredential and DeleteTrust answer
	// 404, as Keystone does for an instance that is already gone.
	deleteNotFound bool
}

func newFakeKeystone(p *keystoneplan.Plan) *fakeKeystone {
	pairs := make(map[string]bool, len(p.Tokens))
	for _, t := range p.Tokens {
		pairs[t.User+"\x00"+t.Project] = true
	}
	return &fakeKeystone{
		liveProject:  make(map[string]bool),
		liveUser:     make(map[string]bool),
		projLiveByID: make(map[string]bool),
		userLiveByID: make(map[string]bool),
		assignByID:   make(map[string]assignInfo),
		tokenPairs:   pairs,

		liveDelegation: make(map[string]delegationInfo),
		authsByID:      make(map[string]int),
		creates:        make(map[resource.Kind]int),
		auths:          make(map[resource.Kind]int),
		deletes:        make(map[resource.Kind]int),
	}
}

func (f *fakeKeystone) CreateProject(_ context.Context, p keystoneplan.Project, _ string) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.liveProject[p.Name] {
		f.doubleLiveProject = true
	}
	f.nextID++
	id := fmt.Sprintf("p-%d", f.nextID)
	f.liveProject[p.Name] = true
	f.projLiveByID[id] = true
	f.curP++
	if f.curP > f.maxP {
		f.maxP = f.curP
	}
	return resource.Resource{Kind: keystone.KindProject, Logical: p.Name, ID: id}, nil
}

func (f *fakeKeystone) CreateUser(_ context.Context, u keystoneplan.User, _, _ string) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.liveUser[u.Name] {
		f.doubleLiveUser = true
	}
	f.nextID++
	id := fmt.Sprintf("u-%d", f.nextID)
	f.liveUser[u.Name] = true
	f.userLiveByID[id] = true
	f.curU++
	if f.curU > f.maxU {
		f.maxU = f.curU
	}
	return resource.Resource{Kind: keystone.KindUser, Logical: u.Name, ID: id}, nil
}

func (f *fakeKeystone) AssignRole(_ context.Context, a keystoneplan.Assignment, userID, projectID, _, roleID string) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.userLiveByID[userID] {
		f.grantOnAbsentUser = true
	}
	if projectID != "" && !f.projLiveByID[projectID] {
		f.grantOnAbsentProject = true
	}
	if f.tokenPairs[a.User+"\x00"+a.Project] {
		f.assignCreatesForToken++
	}
	f.nextID++
	id := fmt.Sprintf("assign-%d", f.nextID)
	f.assignByID[id] = assignInfo{userID: userID, projectID: projectID, roleID: roleID}
	return resource.Resource{Kind: keystone.KindAssignment, Logical: a.User, ID: id}, nil
}

func (f *fakeKeystone) IssueToken(_ context.Context, t keystoneplan.TokenIssue, _, _, projectID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenIssues++
	if !f.liveUser[t.User] || !f.projLiveByID[projectID] {
		f.tokenOnDeadTarget = true
	}
	return nil
}

func (f *fakeKeystone) Delete(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Kind {
	case keystone.KindProject:
		for _, ai := range f.assignByID {
			if ai.projectID == r.ID {
				f.projDeleteWithGrant = true
			}
		}
		for _, d := range f.liveDelegation {
			if d.projectID == r.ID {
				f.parentDeleteWithDelegation = true
			}
		}
		f.projLiveByID[r.ID] = false
		f.liveProject[r.Logical] = false
		f.curP--
	case keystone.KindUser:
		for _, ai := range f.assignByID {
			if ai.userID == r.ID {
				f.userDeleteWithGrant = true
			}
		}
		for _, d := range f.liveDelegation {
			if d.userID == r.ID || d.trusteeID == r.ID {
				f.parentDeleteWithDelegation = true
			}
		}
		f.userLiveByID[r.ID] = false
		f.liveUser[r.Logical] = false
		f.curU--
	case keystone.KindAssignment:
		ai := f.assignByID[r.ID]
		for _, d := range f.liveDelegation {
			if d.userID == ai.userID && d.projectID == ai.projectID && d.roleID == ai.roleID {
				f.grantDeleteWithDelegation = true
			}
		}
		delete(f.assignByID, r.ID)
	}
	return nil
}

func (f *fakeKeystone) CreateApplicationCredential(_ context.Context, ac keystoneplan.ApplicationCredential, userID, _, _, projectID, roleID, _ string) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("%s:ac-%d", userID, f.nextID)
	f.createDelegation(keystone.KindApplicationCredential, id, delegationInfo{userID: userID, projectID: projectID, roleID: roleID})
	return resource.Resource{Kind: keystone.KindApplicationCredential, Logical: ac.Name, ID: id}, nil
}

func (f *fakeKeystone) AuthenticateApplicationCredential(_ context.Context, _ keystoneplan.ApplicationCredential, res resource.Resource, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authenticate(keystone.KindApplicationCredential, res.ID)
	return nil
}

func (f *fakeKeystone) DeleteApplicationCredential(_ context.Context, _ keystoneplan.ApplicationCredential, res resource.Resource, _, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleteDelegation(keystone.KindApplicationCredential, res.ID)
}

func (f *fakeKeystone) CreateTrust(_ context.Context, t keystoneplan.Trust, trustorID, _, _, trusteeID, projectID, roleID string) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("trust-%d", f.nextID)
	f.createDelegation(keystone.KindTrust, id, delegationInfo{userID: trustorID, trusteeID: trusteeID, projectID: projectID, roleID: roleID})
	return resource.Resource{Kind: keystone.KindTrust, Logical: t.Name, ID: id}, nil
}

func (f *fakeKeystone) AuthenticateTrust(_ context.Context, _ keystoneplan.Trust, res resource.Resource, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authenticate(keystone.KindTrust, res.ID)
	return nil
}

func (f *fakeKeystone) DeleteTrust(_ context.Context, _ keystoneplan.Trust, res resource.Resource, _, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleteDelegation(keystone.KindTrust, res.ID)
}

// createDelegation records a new live credential or trust and flags it when
// one of its users or its project is not live, or when no live grant of its
// owner matches its project and role. A trust's trustee is checked whatever
// id it carries, so an unresolved (empty) trustee id counts as absent. The
// caller holds mu.
func (f *fakeKeystone) createDelegation(kind resource.Kind, id string, d delegationInfo) {
	f.creates[kind]++
	if !f.userLiveByID[d.userID] || !f.projLiveByID[d.projectID] || (kind == keystone.KindTrust && !f.userLiveByID[d.trusteeID]) {
		f.delegationOnAbsentParent = true
	}
	granted := false
	for _, ai := range f.assignByID {
		if ai.userID == d.userID && ai.projectID == d.projectID && ai.roleID == d.roleID {
			granted = true
		}
	}
	if !granted {
		f.delegationWithoutGrant = true
	}
	f.liveDelegation[id] = d
}

// authenticate records an authentication with the credential or trust id and
// flags one against an instance that is not live or that already
// authenticated. The caller holds mu.
func (f *fakeKeystone) authenticate(kind resource.Kind, id string) {
	f.auths[kind]++
	if _, ok := f.liveDelegation[id]; !ok {
		f.authOnDeadDelegation = true
	}
	f.authsByID[id]++
	if f.authsByID[id] > 1 {
		f.doubleAuth = true
	}
}

// deleteDelegation removes the credential or trust id, which is gone either
// way, and answers a 404 when deleteNotFound is set. The caller holds mu.
func (f *fakeKeystone) deleteDelegation(kind resource.Kind, id string) error {
	f.deletes[kind]++
	delete(f.liveDelegation, id)
	if f.deleteNotFound {
		return gophercloud.ErrUnexpectedResponseCode{Actual: 404}
	}
	return nil
}

// fakeClock is a virtual clock: Sleep advances time instantly, so the scheduler
// emits a deterministic number of ticks while the operation tasks still run
// concurrently on real goroutines.
type fakeClock struct{ cur time.Time }

func newFakeClock() *fakeClock { return &fakeClock{cur: time.Unix(0, 0)} }

func (c *fakeClock) Now() time.Time { return c.cur }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.cur = c.cur.Add(d)
	return nil
}

func cfg(tokenRatio float64) chaos.Config {
	return chaos.Config{
		Duration:    2 * time.Second,
		MinInterval: 10 * time.Millisecond,
		MaxInterval: 40 * time.Millisecond,
		MaxParallel: 4,
		ChurnRatio:  0.5,
		TargetFill:  0.7,
		ResizeRatio: tokenRatio, // the engine's mutate-draw probability, fed from token_ratio
		Concurrency: 8,
		Classify:    Classify,
	}
}

func mustBuild(t *testing.T, p *keystoneplan.Plan, c Keystone) []chaos.Node {
	t.Helper()
	nodes, err := Build(p, c, testBindings(), time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return nodes
}

// TestRunLifecycleAndEnvelope drives the full churn graph and checks the
// identity lifecycle invariants: no grant on an absent user or project, no user
// or project deleted while it holds a live grant, no logical doubly live, and
// per-kind live counts within the plan envelope.
func TestRunLifecycleAndEnvelope(t *testing.T) {
	p := churnPlan()
	f := newFakeKeystone(p)

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, cfg(0.4), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Creates == 0 {
		t.Fatal("no creates were scheduled; the test exercises nothing")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.grantOnAbsentUser || f.grantOnAbsentProject {
		t.Errorf("a grant was created against an absent parent: user=%v project=%v", f.grantOnAbsentUser, f.grantOnAbsentProject)
	}
	if f.userDeleteWithGrant || f.projDeleteWithGrant {
		t.Errorf("a parent was deleted with a live grant: user=%v project=%v", f.userDeleteWithGrant, f.projDeleteWithGrant)
	}
	if f.doubleLiveProject || f.doubleLiveUser {
		t.Errorf("a logical was doubly live: project=%v user=%v", f.doubleLiveProject, f.doubleLiveUser)
	}
	if f.maxP > len(p.Projects) {
		t.Errorf("live projects peaked at %d, exceeding the envelope %d", f.maxP, len(p.Projects))
	}
	if f.maxU > len(p.Users) {
		t.Errorf("live users peaked at %d, exceeding the envelope %d", f.maxU, len(p.Users))
	}
}

// TestRunTokenIssueSemantics confirms token issues happen only against live
// grants, never more often than the token-backed grants were created (at most
// once per grant lifetime), and never change the population.
func TestRunTokenIssueSemantics(t *testing.T) {
	p := churnPlan()
	f := newFakeKeystone(p)

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, cfg(0.5), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Mutates == 0 {
		t.Fatal("no token issues were scheduled; the test exercises nothing")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenIssues == 0 {
		t.Error("token_ratio > 0 but no tokens were issued")
	}
	if f.tokenOnDeadTarget {
		t.Error("a token was issued against a not-live user or project")
	}
	// At most once per grant lifetime: never more tokens than token-backed grants
	// were created.
	if f.tokenIssues > f.assignCreatesForToken {
		t.Errorf("token issues %d exceed token-backed grant creates %d (must be at most once per lifetime)", f.tokenIssues, f.assignCreatesForToken)
	}
	// A token issue changes no population: live counts stay within the envelope.
	if f.maxP > len(p.Projects) || f.maxU > len(p.Users) {
		t.Errorf("token issues changed the population: projects peaked at %d, users at %d", f.maxP, f.maxU)
	}
}

// TestRunTokenRatioZeroNeverIssues confirms token_ratio 0 issues no tokens.
func TestRunTokenRatioZeroNeverIssues(t *testing.T) {
	p := churnPlan()
	f := newFakeKeystone(p)

	if _, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, cfg(0), newFakeClock()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenIssues != 0 {
		t.Errorf("token issues = %d, want 0 at token_ratio 0", f.tokenIssues)
	}
}

// TestRunDeterministicSchedule confirms the decision schedule is reproducible
// for a given seed and config, independent of the concurrent cloud completions.
func TestRunDeterministicSchedule(t *testing.T) {
	p := churnPlan()
	c := cfg(0.4)

	r1, err := chaos.Run(context.Background(), mustBuild(t, p, newFakeKeystone(p)), p.Seed, c, newFakeClock())
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	r2, err := chaos.Run(context.Background(), mustBuild(t, p, newFakeKeystone(p)), p.Seed, c, newFakeClock())
	if err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if len(r1.Decisions) == 0 {
		t.Fatal("no decisions were scheduled")
	}
	if !reflect.DeepEqual(r1.Decisions, r2.Decisions) {
		t.Error("decision schedules differ for the same seed/config")
	}
}

// delegationKinds are the two kinds a delegationPlan adds to the churn graph.
var delegationKinds = []resource.Kind{keystone.KindApplicationCredential, keystone.KindTrust}

// TestRunDelegationLifecycle drives the churn graph of a plan with application
// credentials and trusts and confirms none is created while its user, trustor,
// trustee or project is absent or while no live grant matches its user, project
// and role; none is live when its assignment, user or project is deleted; and
// each is authenticated at most once per lifetime and only while live. It also
// confirms the run created, authenticated and deleted both kinds, in the fake
// and in the result's decisions, so the invariants are not vacuous.
func TestRunDelegationLifecycle(t *testing.T) {
	p := delegationPlan()
	f := newFakeKeystone(p)

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, cfg(0.5), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	decided := map[string]map[resource.Kind]int{"create": {}, "mutate": {}, "delete": {}}
	for _, d := range r.Decisions {
		if m, ok := decided[d.Action]; ok {
			m[d.Kind]++
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, kind := range delegationKinds {
		if f.creates[kind] == 0 || f.auths[kind] == 0 || f.deletes[kind] == 0 {
			t.Fatalf("%s: creates=%d authentications=%d deletes=%d, want each above 0; the test exercises nothing",
				kind, f.creates[kind], f.auths[kind], f.deletes[kind])
		}
		for _, action := range []string{"create", "mutate", "delete"} {
			if decided[action][kind] == 0 {
				t.Errorf("the result's decisions hold no %s of kind %s", action, kind)
			}
		}
		if f.auths[kind] > f.creates[kind] {
			t.Errorf("%s: authentications %d exceed creates %d (must be at most once per lifetime)", kind, f.auths[kind], f.creates[kind])
		}
	}
	if f.delegationOnAbsentParent {
		t.Error("a credential or trust was created while its user, trustor, trustee or project was not live")
	}
	if f.delegationWithoutGrant {
		t.Error("a credential or trust was created while no live grant matched its user, project and role")
	}
	if f.grantDeleteWithDelegation {
		t.Error("an assignment was deleted while a live credential or trust rested on it")
	}
	if f.parentDeleteWithDelegation {
		t.Error("a user or project was deleted while a live credential or trust named it")
	}
	if f.doubleAuth {
		t.Error("a credential or trust was authenticated twice in one lifetime")
	}
	if f.authOnDeadDelegation {
		t.Error("a credential or trust was authenticated while it was not live")
	}
	if f.grantOnAbsentUser || f.grantOnAbsentProject || f.userDeleteWithGrant || f.projDeleteWithGrant {
		t.Errorf("a grant lifecycle invariant broke: grantOnAbsentUser=%v grantOnAbsentProject=%v userDeleteWithGrant=%v projDeleteWithGrant=%v",
			f.grantOnAbsentUser, f.grantOnAbsentProject, f.userDeleteWithGrant, f.projDeleteWithGrant)
	}
}

// TestRunDelegationTokenRatioZeroNeverAuthenticates confirms token_ratio 0
// authenticates with no application credential and no trust, while the run
// still creates both kinds.
func TestRunDelegationTokenRatioZeroNeverAuthenticates(t *testing.T) {
	p := delegationPlan()
	f := newFakeKeystone(p)

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, cfg(0), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Mutates != 0 {
		t.Errorf("mutates = %d, want 0 at token_ratio 0", r.Mutates)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, kind := range delegationKinds {
		if f.creates[kind] == 0 {
			t.Fatalf("%s: no creates; the test exercises nothing", kind)
		}
		if f.auths[kind] != 0 {
			t.Errorf("%s authentications = %d, want 0 at token_ratio 0", kind, f.auths[kind])
		}
	}
}

// TestRunDelegationDeleteNotFoundSucceeds confirms the engine counts a delete
// of an application credential or a trust that answers 404 as succeeded: the
// fake saw deletes of both kinds, and no bucket of the result holds a failed
// operation.
func TestRunDelegationDeleteNotFoundSucceeds(t *testing.T) {
	p := delegationPlan()
	f := newFakeKeystone(p)
	f.deleteNotFound = true

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, cfg(0.5), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	f.mu.Lock()
	for _, kind := range delegationKinds {
		if f.deletes[kind] == 0 {
			f.mu.Unlock()
			t.Fatalf("%s: no deletes answered 404; the test exercises nothing", kind)
		}
	}
	f.mu.Unlock()

	if len(r.Buckets) == 0 {
		t.Fatal("the result holds no buckets")
	}
	for _, b := range r.Buckets {
		if b.Stats.Failed != 0 {
			t.Errorf("bucket at %s: %d failed of %d attempted (errors %v), want 0 failed", b.Start, b.Stats.Failed, b.Stats.Attempted, b.Errors)
		}
	}
}
