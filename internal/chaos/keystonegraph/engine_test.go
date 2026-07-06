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
)

// assignInfo records which user and project a live grant belongs to, so the
// fake can detect a parent deleted while a child grant is still live.
type assignInfo struct {
	userID, projectID string
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

func (f *fakeKeystone) AssignRole(_ context.Context, a keystoneplan.Assignment, userID, projectID, _, _ string) (resource.Resource, error) {
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
	f.assignByID[id] = assignInfo{userID: userID, projectID: projectID}
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
		f.projLiveByID[r.ID] = false
		f.liveProject[r.Logical] = false
		f.curP--
	case keystone.KindUser:
		for _, ai := range f.assignByID {
			if ai.userID == r.ID {
				f.userDeleteWithGrant = true
			}
		}
		f.userLiveByID[r.ID] = false
		f.liveUser[r.Logical] = false
		f.curU--
	case keystone.KindAssignment:
		delete(f.assignByID, r.ID)
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
