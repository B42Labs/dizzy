package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/keystone"
	"github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeKeystone is an in-process Keystone implementation that records call order,
// stage-2 concurrency, injects transient/forbidden/conflict failures, records
// the passwords it is handed, and can block until cancelled. It lets the
// executor's stage ordering, retry, fail-fast, token-password, and cancellation
// logic be exercised without a cloud.
type fakeKeystone struct {
	mu     sync.Mutex
	nextID int

	opOrder  []string       // "domain"/"role"/"project"/"user"/"assign"/"token" in call-start order
	attempts map[string]int // create attempts per logical name

	forbid   map[string]bool // logical -> return a 403 (wrapped ErrForbidden)
	conflict map[string]bool // logical -> return a 409 (terminal)
	failures map[string]int  // logical -> remaining transient (503) failures

	userPassword  map[string]string // user logical -> password passed to CreateUser
	tokenPassword map[string]string // user logical -> password passed to IssueToken
	tokenFail     map[string]bool   // user logical -> IssueToken returns an error

	createDelay     time.Duration // sleep inside project/user create to expose concurrency
	holdUntilCancel bool          // block each create until ctx is cancelled

	// stage-2 concurrency tracking
	curProjects, curUsers int
	inFlight, maxInFlight int
	mixedInFlight         bool // a project and a user were in flight together

	started     chan struct{}
	startedOnce sync.Once
}

func newFake() *fakeKeystone {
	return &fakeKeystone{
		attempts:      make(map[string]int),
		forbid:        make(map[string]bool),
		conflict:      make(map[string]bool),
		failures:      make(map[string]int),
		userPassword:  make(map[string]string),
		tokenPassword: make(map[string]string),
		tokenFail:     make(map[string]bool),
	}
}

// injectedErr returns a failure for logical if one is configured, decrementing a
// transient budget. The caller holds no lock.
func (f *fakeKeystone) injectedErr(logical string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[logical]++
	switch {
	case f.forbid[logical]:
		return wrapForbidden()
	case f.conflict[logical]:
		return gophercloud.ErrUnexpectedResponseCode{Actual: 409}
	case f.failures[logical] > 0:
		f.failures[logical]--
		return gophercloud.ErrUnexpectedResponseCode{Actual: 503}
	}
	return nil
}

// wrapForbidden builds a 403 wrapped so errors.Is finds keystone.ErrForbidden,
// exactly as the real client's wrapCreate does.
func wrapForbidden() error {
	return fmt.Errorf("fake forbidden: %w: %w", keystone.ErrForbidden, gophercloud.ErrUnexpectedResponseCode{Actual: 403})
}

func (f *fakeKeystone) record(kind resource.Kind, logical string) resource.Resource {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	return resource.Resource{Kind: kind, Logical: logical, ID: fmt.Sprintf("id-%d", f.nextID)}
}

func (f *fakeKeystone) note(op string) {
	f.mu.Lock()
	f.opOrder = append(f.opOrder, op)
	f.mu.Unlock()
}

func (f *fakeKeystone) CreateDomain(ctx context.Context, d plan.Domain) (resource.Resource, error) {
	f.note("domain")
	if err := f.injectedErr(d.Name); err != nil {
		return resource.Resource{}, err
	}
	return f.record(keystone.KindDomain, d.Name), nil
}

func (f *fakeKeystone) CreateRole(ctx context.Context, r plan.Role) (resource.Resource, error) {
	f.note("role")
	if err := f.injectedErr(r.Name); err != nil {
		return resource.Resource{}, err
	}
	return f.record(keystone.KindRole, r.Name), nil
}

func (f *fakeKeystone) CreateProject(ctx context.Context, p plan.Project, domainID string) (resource.Resource, error) {
	f.note("project")
	f.enterChild(true)
	defer f.leaveChild(true)
	if f.started != nil {
		f.startedOnce.Do(func() { close(f.started) })
	}
	if err := f.injectedErr(p.Name); err != nil {
		return resource.Resource{}, err
	}
	if f.holdUntilCancel {
		<-ctx.Done()
		return resource.Resource{}, ctx.Err()
	}
	if f.createDelay > 0 {
		time.Sleep(f.createDelay)
	}
	return f.record(keystone.KindProject, p.Name), nil
}

func (f *fakeKeystone) CreateUser(ctx context.Context, u plan.User, domainID, password string) (resource.Resource, error) {
	f.note("user")
	f.enterChild(false)
	defer f.leaveChild(false)
	f.mu.Lock()
	f.userPassword[u.Name] = password
	f.mu.Unlock()
	if f.started != nil {
		f.startedOnce.Do(func() { close(f.started) })
	}
	if err := f.injectedErr(u.Name); err != nil {
		return resource.Resource{}, err
	}
	if f.holdUntilCancel {
		<-ctx.Done()
		return resource.Resource{}, ctx.Err()
	}
	if f.createDelay > 0 {
		time.Sleep(f.createDelay)
	}
	return f.record(keystone.KindUser, u.Name), nil
}

func (f *fakeKeystone) enterChild(isProject bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	if isProject {
		f.curProjects++
	} else {
		f.curUsers++
	}
	if f.curProjects > 0 && f.curUsers > 0 {
		f.mixedInFlight = true
	}
}

func (f *fakeKeystone) leaveChild(isProject bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
	if isProject {
		f.curProjects--
	} else {
		f.curUsers--
	}
}

func (f *fakeKeystone) AssignRole(ctx context.Context, a plan.Assignment, userID, projectID, domainID, roleID string) (resource.Resource, error) {
	f.note("assign")
	if err := f.injectedErr("assign:" + a.User); err != nil {
		return resource.Resource{}, err
	}
	target := "project:" + projectID
	if projectID == "" {
		target = "domain:" + domainID
	}
	return resource.Resource{Kind: keystone.KindAssignment, Logical: a.User, ID: userID + ":" + target + ":" + roleID}, nil
}

func (f *fakeKeystone) IssueToken(ctx context.Context, t plan.TokenIssue, userDomainID, password, projectID string) error {
	f.note("token")
	f.mu.Lock()
	f.tokenPassword[t.User] = password
	fail := f.tokenFail[t.User]
	f.mu.Unlock()
	if fail {
		return fmt.Errorf("fake token issue failed for %s", t.User)
	}
	return nil
}

// fullPlan is a self-consistent single-domain plan with two roles, two projects,
// two users (one with a project grant, one with a domain grant), and one token.
func fullPlan() *plan.Plan {
	return &plan.Plan{
		Scenario: "test", Seed: 7,
		Domains:  []plan.Domain{{Name: "dom-0001"}},
		Roles:    []plan.Role{{Name: "role-0001"}, {Name: "role-0002"}},
		Projects: []plan.Project{{Name: "proj-0001", Domain: "dom-0001"}, {Name: "proj-0002", Domain: "dom-0001"}},
		Users:    []plan.User{{Name: "user-0001", Domain: "dom-0001"}, {Name: "user-0002", Domain: "dom-0001"}},
		Assignments: []plan.Assignment{
			{User: "user-0001", Role: "role-0001", Project: "proj-0001"},
			{User: "user-0002", Role: "role-0002"}, // domain-scoped
		},
		Tokens: []plan.TokenIssue{{User: "user-0001", Project: "proj-0001"}},
	}
}

func adminRes() keystone.Resolution { return keystone.Resolution{} }

// TestApplyStageOrdering confirms the stages are strictly ordered: domains,
// then roles, then projects/users, then assignments, then tokens.
func TestApplyStageOrdering(t *testing.T) {
	f := newFake()
	res, err := Apply(context.Background(), f, fullPlan(), keystone.TierAdmin, adminRes(), 4, time.Minute)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 1 domain + 2 roles + 2 projects + 2 users + 2 assignments = 9 (tokens make no resource).
	if len(res.Created) != 9 {
		t.Errorf("created %d resources, want 9", len(res.Created))
	}

	rank := map[string]int{"domain": 0, "role": 1, "project": 2, "user": 2, "assign": 3, "token": 4}
	phase := 0
	for i, op := range f.opOrder {
		if rank[op] < phase {
			t.Fatalf("op %q at index %d violates stage order %v", op, i, f.opOrder)
		}
		phase = rank[op]
	}
}

// TestApplyProjectsAndUsersShareAStage confirms projects and users run in one
// concurrent pool: a project and a user are in flight at the same time.
func TestApplyProjectsAndUsersShareAStage(t *testing.T) {
	p := &plan.Plan{
		Scenario: "t", Seed: 1,
		Domains: []plan.Domain{{Name: "dom-0001"}},
		Roles:   []plan.Role{{Name: "role-0001"}},
	}
	for i := 1; i <= 4; i++ {
		p.Projects = append(p.Projects, plan.Project{Name: fmt.Sprintf("proj-%04d", i), Domain: "dom-0001"})
		p.Users = append(p.Users, plan.User{Name: fmt.Sprintf("user-%04d", i), Domain: "dom-0001"})
	}

	f := newFake()
	f.createDelay = 10 * time.Millisecond // widen the window so the pool overlaps

	if _, err := Apply(context.Background(), f, p, keystone.TierAdmin, adminRes(), 4, time.Minute); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !f.mixedInFlight {
		t.Error("no project and user were in flight together; stage 2 must share one pool")
	}
}

// TestApplyDomainManagerCreatesNoRoots locks the domain-manager acceptance
// criterion: no domain or role is created, the bindings map onto the resolution,
// and logical roles bind onto the reused roles by modulo.
func TestApplyDomainManagerCreatesNoRoots(t *testing.T) {
	f := newFake()
	res := keystone.Resolution{DomainID: "dm-real", RoleIDs: []string{"role-a", "role-b"}}

	b, roots, err := BindRoots(context.Background(), f, fullPlan(), keystone.TierDomainManager, res, 4, time.Minute)
	if err != nil {
		t.Fatalf("BindRoots: %v", err)
	}
	if len(roots) != 0 {
		t.Errorf("domain-manager BindRoots created %d roots, want 0", len(roots))
	}
	if b.Domains["dom-0001"] != "dm-real" {
		t.Errorf("logical domain bound to %q, want the in-scope dm-real", b.Domains["dom-0001"])
	}
	if b.Roles["role-0001"] != "role-a" || b.Roles["role-0002"] != "role-b" {
		t.Errorf("roles bound to %+v, want role-0001->role-a and role-0002->role-b", b.Roles)
	}

	if _, err := Apply(context.Background(), f, fullPlan(), keystone.TierDomainManager, res, 4, time.Minute); err != nil {
		t.Fatalf("Apply (domain-manager): %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, op := range f.opOrder {
		if op == "domain" || op == "role" {
			t.Errorf("domain-manager mode created a %s; roots must be reused, not created", op)
		}
	}
}

// TestApplyFailFastForbidden confirms a 403 on a domain create fails fast (no
// retry), cancels the stage, and returns the partial Created.
func TestApplyFailFastForbidden(t *testing.T) {
	f := newFake()
	f.forbid["dom-0001"] = true

	res, err := Apply(context.Background(), f, fullPlan(), keystone.TierAdmin, adminRes(), 4, time.Minute)
	if err == nil {
		t.Fatal("expected a forbidden error, got nil")
	}
	if !errors.Is(err, keystone.ErrForbidden) {
		t.Errorf("error %v does not match ErrForbidden", err)
	}
	if len(res.Created) != 0 {
		t.Errorf("created %d resources after a 403 on the only domain, want 0", len(res.Created))
	}
	if n := f.attempts["dom-0001"]; n != 1 {
		t.Errorf("forbidden domain create attempted %d times, want 1 (no retry)", n)
	}
	// No project or user should have been created after the stage-1 failure.
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, op := range f.opOrder {
		if op == "project" || op == "user" {
			t.Errorf("a %s was created after a root-binding 403", op)
		}
	}
}

// TestApplyConflictNotRetried confirms a 409 conflict is terminal (not retried),
// matching Keystone's unique-name semantics.
func TestApplyConflictNotRetried(t *testing.T) {
	f := newFake()
	f.conflict["role-0001"] = true

	if _, err := Apply(context.Background(), f, fullPlan(), keystone.TierAdmin, adminRes(), 4, time.Minute); err == nil {
		t.Fatal("expected the 409 to fail the run")
	}
	if n := f.attempts["role-0001"]; n != 1 {
		t.Errorf("conflicting role create attempted %d times, want 1 (409 is terminal)", n)
	}
}

// TestApplyRetriesTransient confirms a transient 503 is retried with backoff and
// the create ultimately succeeds.
func TestApplyRetriesTransient(t *testing.T) {
	f := newFake()
	f.failures["proj-0001"] = 2 // fail twice, succeed on the third attempt

	if _, err := Apply(context.Background(), f, fullPlan(), keystone.TierAdmin, adminRes(), 1, time.Minute); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := f.attempts["proj-0001"]; n != 3 {
		t.Errorf("proj-0001 attempted %d times, want 3", n)
	}
}

// TestApplyTokenUsesFreshPassword confirms the token issue authenticates with
// exactly the password the create used, and that the password is a fresh random
// value — not derived from the seed or the user's cloud-visible name, so two runs
// of the same plan never share a credential an observer could recompute.
func TestApplyTokenUsesFreshPassword(t *testing.T) {
	p := fullPlan()
	f := newFake()
	if _, err := Apply(context.Background(), f, p, keystone.TierAdmin, adminRes(), 4, time.Minute); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	created := f.userPassword["user-0001"]
	if created == "" {
		t.Fatal("CreateUser got an empty password")
	}
	if got := f.tokenPassword["user-0001"]; got != created {
		t.Errorf("IssueToken password = %q, want the create-time password %q", got, created)
	}

	// A second run of the same plan must not reuse the credential: the password is
	// random, not derived from the seed or the cloud-visible user name.
	f2 := newFake()
	if _, err := Apply(context.Background(), f2, p, keystone.TierAdmin, adminRes(), 4, time.Minute); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if again := f2.userPassword["user-0001"]; again == created {
		t.Error("two runs of the same plan produced an identical user password; it must be random, not derived")
	}
}

// TestApplyTokenFailureFailsRun locks the acceptance criterion that a failed
// token issue surfaces as a failed operation that fails the run.
func TestApplyTokenFailureFailsRun(t *testing.T) {
	f := newFake()
	f.tokenFail["user-0001"] = true

	if _, err := Apply(context.Background(), f, fullPlan(), keystone.TierAdmin, adminRes(), 4, time.Minute); err == nil {
		t.Fatal("expected a failed token issue to fail the run")
	}
}

// TestApplyCancellation confirms cancelling mid-run stops promptly and returns a
// context error.
func TestApplyCancellation(t *testing.T) {
	f := newFake()
	f.holdUntilCancel = true
	f.started = make(chan struct{})

	// A project-only stage-2 plan so the held create is reached deterministically.
	p := &plan.Plan{
		Scenario: "t", Seed: 1,
		Domains:  []plan.Domain{{Name: "dom-0001"}},
		Roles:    []plan.Role{{Name: "role-0001"}},
		Projects: []plan.Project{{Name: "proj-0001", Domain: "dom-0001"}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := Apply(ctx, f, p, keystone.TierAdmin, adminRes(), 1, time.Minute)
		done <- result{err}
	}()

	<-f.started
	cancel()

	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("Apply returned %v, want context.Canceled", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Apply did not return after cancellation")
	}
}

// TestApplyConcurrencyBound confirms stage 2 never runs more than concurrency
// creates at once while still saturating the limit.
func TestApplyConcurrencyBound(t *testing.T) {
	const concurrency = 5
	p := &plan.Plan{
		Scenario: "t", Seed: 1,
		Domains: []plan.Domain{{Name: "dom-0001"}},
		Roles:   []plan.Role{{Name: "role-0001"}},
	}
	for i := 1; i <= 30; i++ {
		p.Projects = append(p.Projects, plan.Project{Name: fmt.Sprintf("proj-%04d", i), Domain: "dom-0001"})
	}

	f := newFake()
	f.createDelay = 10 * time.Millisecond

	if _, err := Apply(context.Background(), f, p, keystone.TierAdmin, adminRes(), concurrency, time.Minute); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.maxInFlight > concurrency {
		t.Errorf("max in-flight %d exceeded concurrency %d", f.maxInFlight, concurrency)
	}
	if f.maxInFlight != concurrency {
		t.Errorf("max in-flight %d did not reach concurrency %d", f.maxInFlight, concurrency)
	}
}
