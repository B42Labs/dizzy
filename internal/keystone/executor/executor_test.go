package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
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
// the passwords, secrets and ids it is handed, and can block until cancelled.
// It lets the executor's stage ordering, retry, fail-fast, token-password,
// delegation, and cancellation logic be exercised without a cloud.
type fakeKeystone struct {
	mu     sync.Mutex
	nextID int

	// opOrder holds "domain"/"role"/"implied_role"/"project"/"user"/"assign"/
	// "token"/"appcred"/"appcred_auth"/"trust"/"trust_auth" in call-start order.
	opOrder     []string
	attempts    map[string]int // create attempts per logical name; authentications under "auth:<name>"
	ruleRoleIDs [][2]string    // the (prior, implied) role ids of every created implied-role rule

	forbid   map[string]bool // logical -> return a 403 (wrapped ErrForbidden)
	conflict map[string]bool // logical -> return a 409 (terminal)
	failures map[string]int  // logical -> remaining transient (503) failures

	userPassword  map[string]string // user logical -> password passed to CreateUser
	tokenPassword map[string]string // user logical -> password passed to IssueToken
	tokenFail     map[string]bool   // user logical -> IssueToken returns an error

	appcreds    map[string]appcredCall     // credential name -> arguments of its last CreateApplicationCredential
	appcredAuth map[string]appcredAuthCall // credential name -> arguments of its last AuthenticateApplicationCredential
	trusts      map[string]trustCall       // trust name -> arguments of its last CreateTrust
	trustAuth   map[string]trustAuthCall   // trust name -> arguments of its last AuthenticateTrust
	authFail    map[string]bool            // credential or trust name -> its authentication returns a 401

	createDelay     time.Duration // sleep inside project/user create to expose concurrency
	holdUntilCancel bool          // block each project/user create until ctx is cancelled

	// holdAppCredUntilCancel blocks each application credential create (stage
	// 5) until ctx is cancelled, closing appcredStarted when the first arrives.
	holdAppCredUntilCancel bool
	appcredStarted         chan struct{}
	appcredStartedOnce     sync.Once

	// stage-2 concurrency tracking
	curProjects, curUsers int
	inFlight, maxInFlight int
	mixedInFlight         bool // a project and a user were in flight together

	started     chan struct{}
	startedOnce sync.Once
}

// appcredCall is what CreateApplicationCredential was handed besides the plan
// entry.
type appcredCall struct {
	userID, userDomainID, password, projectID, roleID, secret string
}

// appcredAuthCall is what AuthenticateApplicationCredential was handed besides
// the plan entry.
type appcredAuthCall struct {
	res    resource.Resource
	secret string
}

// trustCall is what CreateTrust was handed besides the plan entry.
type trustCall struct {
	trustorID, trustorDomainID, trustorPassword, trusteeID, projectID, roleID string
}

// trustAuthCall is what AuthenticateTrust was handed besides the plan entry.
type trustAuthCall struct {
	res                              resource.Resource
	trusteeDomainID, trusteePassword string
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
		appcreds:      make(map[string]appcredCall),
		appcredAuth:   make(map[string]appcredAuthCall),
		trusts:        make(map[string]trustCall),
		trustAuth:     make(map[string]trustAuthCall),
		authFail:      make(map[string]bool),
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

// CreateImpliedRole records the rule r between the role ids it is handed and
// returns a resource with the client's synthetic id <prior>:<implied>. A
// failure is injected by the rule's label, prior->implied.
func (f *fakeKeystone) CreateImpliedRole(ctx context.Context, r plan.ImpliedRole, priorRoleID, impliedRoleID string) (resource.Resource, error) {
	f.note("implied_role")
	logical := r.Prior + "->" + r.Implied
	if err := f.injectedErr(logical); err != nil {
		return resource.Resource{}, err
	}
	f.mu.Lock()
	f.ruleRoleIDs = append(f.ruleRoleIDs, [2]string{priorRoleID, impliedRoleID})
	f.mu.Unlock()
	return resource.Resource{Kind: keystone.KindImpliedRole, Logical: logical, ID: priorRoleID + ":" + impliedRoleID}, nil
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

// authErr counts one authentication attempt with the credential or trust
// name and returns a 401 when one is configured for it.
func (f *fakeKeystone) authErr(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts["auth:"+name]++
	if f.authFail[name] {
		return gophercloud.ErrUnexpectedResponseCode{Actual: 401}
	}
	return nil
}

// CreateApplicationCredential records the arguments it is handed and returns
// a resource with the client's synthetic id <userID>:<credentialID>. A failure
// is injected by the credential's name; with holdAppCredUntilCancel set it
// blocks until ctx is cancelled.
func (f *fakeKeystone) CreateApplicationCredential(ctx context.Context, ac plan.ApplicationCredential, userID, userDomainID, password, projectID, roleID, secret string) (resource.Resource, error) {
	f.note("appcred")
	f.mu.Lock()
	f.appcreds[ac.Name] = appcredCall{userID: userID, userDomainID: userDomainID, password: password, projectID: projectID, roleID: roleID, secret: secret}
	f.mu.Unlock()
	if f.appcredStarted != nil {
		f.appcredStartedOnce.Do(func() { close(f.appcredStarted) })
	}
	if err := f.injectedErr(ac.Name); err != nil {
		return resource.Resource{}, err
	}
	if f.holdAppCredUntilCancel {
		<-ctx.Done()
		return resource.Resource{}, ctx.Err()
	}
	r := f.record(keystone.KindApplicationCredential, ac.Name)
	r.Name = "dizzy-fake-" + ac.Name
	r.ID = userID + ":" + r.ID
	return r, nil
}

// AuthenticateApplicationCredential records the resource and secret it is
// handed and fails with a 401 when authFail names the credential.
func (f *fakeKeystone) AuthenticateApplicationCredential(ctx context.Context, ac plan.ApplicationCredential, res resource.Resource, secret string) error {
	f.note("appcred_auth")
	f.mu.Lock()
	f.appcredAuth[ac.Name] = appcredAuthCall{res: res, secret: secret}
	f.mu.Unlock()
	return f.authErr(ac.Name)
}

// CreateTrust records the arguments it is handed and returns a resource of
// kind trust without a name, as the client does. A failure is injected by the
// trust's name.
func (f *fakeKeystone) CreateTrust(ctx context.Context, t plan.Trust, trustorID, trustorDomainID, trustorPassword, trusteeID, projectID, roleID string) (resource.Resource, error) {
	f.note("trust")
	f.mu.Lock()
	f.trusts[t.Name] = trustCall{trustorID: trustorID, trustorDomainID: trustorDomainID, trustorPassword: trustorPassword, trusteeID: trusteeID, projectID: projectID, roleID: roleID}
	f.mu.Unlock()
	if err := f.injectedErr(t.Name); err != nil {
		return resource.Resource{}, err
	}
	return f.record(keystone.KindTrust, t.Name), nil
}

// AuthenticateTrust records the resource, trustee domain and password it is
// handed and fails with a 401 when authFail names the trust.
func (f *fakeKeystone) AuthenticateTrust(ctx context.Context, t plan.Trust, res resource.Resource, trusteeDomainID, trusteePassword string) error {
	f.note("trust_auth")
	f.mu.Lock()
	f.trustAuth[t.Name] = trustAuthCall{res: res, trusteeDomainID: trusteeDomainID, trusteePassword: trusteePassword}
	f.mu.Unlock()
	return f.authErr(t.Name)
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

// countKinds tallies resources by kind.
func countKinds(rs []resource.Resource) map[resource.Kind]int {
	n := make(map[resource.Kind]int)
	for _, r := range rs {
		n[r.Kind]++
	}
	return n
}

// TestProvisionReturnsIDsAndPasswords confirms Provision runs the first three
// stages and no token issue: IDs holds one cloud id per project and user (the
// ids the fake returned), Passwords one entry per user (the password the user
// was created with), Bindings the roots, and Created exactly what Apply records
// for the same plan. A single worker makes the fake's ids deterministic so two
// fresh fakes hand out the same ones.
func TestProvisionReturnsIDsAndPasswords(t *testing.T) {
	p := fullPlan()
	f := newFake()
	prov, err := Provision(context.Background(), f, p, keystone.TierAdmin, adminRes(), 1, time.Minute)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if len(prov.IDs) != len(p.Projects)+len(p.Users) {
		t.Errorf("IDs has %d entries, want %d (one per project and user): %v", len(prov.IDs), len(p.Projects)+len(p.Users), prov.IDs)
	}
	for _, r := range prov.Created {
		if r.Kind != keystone.KindProject && r.Kind != keystone.KindUser {
			continue
		}
		if r.ID == "" || prov.IDs[r.Logical] != r.ID {
			t.Errorf("IDs[%q] = %q, want the fake's id %q", r.Logical, prov.IDs[r.Logical], r.ID)
		}
	}

	if len(prov.Passwords) != len(p.Users) {
		t.Errorf("Passwords has %d entries, want %d (one per user)", len(prov.Passwords), len(p.Users))
	}
	for _, u := range p.Users {
		if pw := prov.Passwords[u.Name]; pw == "" || pw != f.userPassword[u.Name] {
			t.Errorf("Passwords[%q] = %q, want the create-time password %q", u.Name, pw, f.userPassword[u.Name])
		}
	}

	if prov.Bindings.Domains["dom-0001"] == "" || len(prov.Bindings.Roles) != len(p.Roles) {
		t.Errorf("Bindings = %+v, want the created domain and both roles", prov.Bindings)
	}

	for _, op := range f.opOrder {
		if op == "token" {
			t.Errorf("Provision issued a token; tokens belong to the caller (ops %v)", f.opOrder)
		}
	}

	applied, err := Apply(context.Background(), newFake(), p, keystone.TierAdmin, adminRes(), 1, time.Minute)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Equal(prov.Created, applied.Created) {
		t.Errorf("Provision created %v, want what Apply records %v", prov.Created, applied.Created)
	}
}

// TestProvisionPartialOnForbidden confirms a 403 on a stage-3 grant stops
// Provision with an error matching keystone.ErrForbidden and a non-nil result
// holding the roots, projects and users created so far, their ids and the
// users' passwords, so the caller can record and tear them down. A single
// worker grants user-0001's assignment before the denied one.
func TestProvisionPartialOnForbidden(t *testing.T) {
	p := fullPlan()
	f := newFake()
	f.forbid["assign:user-0002"] = true

	prov, err := Provision(context.Background(), f, p, keystone.TierAdmin, adminRes(), 1, time.Minute)
	if !errors.Is(err, keystone.ErrForbidden) {
		t.Fatalf("Provision error = %v, want one matching keystone.ErrForbidden", err)
	}
	if prov == nil {
		t.Fatal("Provision returned a nil result with its error")
	}

	got := countKinds(prov.Created)
	want := map[resource.Kind]int{
		keystone.KindDomain:     1,
		keystone.KindRole:       2,
		keystone.KindProject:    2,
		keystone.KindUser:       2,
		keystone.KindAssignment: 1,
	}
	if !maps.Equal(got, want) {
		t.Errorf("created kinds = %v, want %v", got, want)
	}
	if len(prov.IDs) != len(p.Projects)+len(p.Users) {
		t.Errorf("IDs has %d entries, want %d", len(prov.IDs), len(p.Projects)+len(p.Users))
	}
	if len(prov.Passwords) != len(p.Users) {
		t.Errorf("Passwords has %d entries, want %d", len(prov.Passwords), len(p.Users))
	}
	if n := f.attempts["assign:user-0002"]; n != 1 {
		t.Errorf("forbidden grant attempted %d times, want 1 (no retry)", n)
	}
}

// TestProvisionRootFailureReturnsResult confirms a failure in the first stage
// still returns a non-nil, empty result whose maps are safe to read.
func TestProvisionRootFailureReturnsResult(t *testing.T) {
	f := newFake()
	f.forbid["dom-0001"] = true

	prov, err := Provision(context.Background(), f, fullPlan(), keystone.TierAdmin, adminRes(), 4, time.Minute)
	if !errors.Is(err, keystone.ErrForbidden) {
		t.Fatalf("Provision error = %v, want one matching keystone.ErrForbidden", err)
	}
	if prov == nil {
		t.Fatal("Provision returned a nil result with its error")
	}
	if len(prov.Created) != 0 || len(prov.IDs) != 0 || prov.IDs == nil || prov.Passwords == nil {
		t.Errorf("result = %+v, want nothing created and non-nil empty maps", prov)
	}
}

// impliedRolePlan is fullPlan with a third role and the chain role-0001 ->
// role-0002 -> role-0003 as its implied-role rules.
func impliedRolePlan() *plan.Plan {
	p := fullPlan()
	p.Roles = append(p.Roles, plan.Role{Name: "role-0003"})
	p.ImpliedRoles = []plan.ImpliedRole{
		{Prior: "role-0001", Implied: "role-0002"},
		{Prior: "role-0002", Implied: "role-0003"},
	}
	return p
}

// countOps returns how often op appears in the fake's call order.
func (f *fakeKeystone) countOps(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.opOrder {
		if o == op {
			n++
		}
	}
	return n
}

// TestApplyCreatesImpliedRolesAfterRoles confirms admin mode creates the
// implied-role rules as a stage of their own, after every role and before any
// project or user, with the cloud ids the roles stage returned, and records
// them in Created right after the roles.
func TestApplyCreatesImpliedRolesAfterRoles(t *testing.T) {
	f := newFake()
	res, err := Apply(context.Background(), f, impliedRolePlan(), keystone.TierAdmin, adminRes(), 4, time.Minute)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	rank := map[string]int{"domain": 0, "role": 1, "implied_role": 2, "project": 3, "user": 3, "assign": 4, "token": 5}
	phase := 0
	for i, op := range f.opOrder {
		if rank[op] < phase {
			t.Fatalf("op %q at index %d violates stage order %v", op, i, f.opOrder)
		}
		phase = rank[op]
	}
	if n := f.countOps("implied_role"); n != 2 {
		t.Errorf("CreateImpliedRole called %d times, want 2", n)
	}

	// 1 domain + 3 roles + 2 rules + 2 projects + 2 users + 2 assignments.
	if len(res.Created) != 12 {
		t.Fatalf("created %d resources, want 12: %+v", len(res.Created), res.Created)
	}
	roleIDs := make(map[string]string)
	for _, r := range res.Created[1:4] {
		if r.Kind != keystone.KindRole {
			t.Fatalf("Created[1:4] = %+v, want the three roles", res.Created[1:4])
		}
		roleIDs[r.Logical] = r.ID
	}
	wantRules := []resource.Resource{
		{Kind: keystone.KindImpliedRole, Logical: "role-0001->role-0002", ID: roleIDs["role-0001"] + ":" + roleIDs["role-0002"]},
		{Kind: keystone.KindImpliedRole, Logical: "role-0002->role-0003", ID: roleIDs["role-0002"] + ":" + roleIDs["role-0003"]},
	}
	if got := res.Created[4:6]; !slices.Equal(got, wantRules) {
		t.Errorf("Created[4:6] = %+v, want the rules %+v right after the roles", got, wantRules)
	}
	f.mu.Lock()
	gotIDs := slices.Clone(f.ruleRoleIDs)
	f.mu.Unlock()
	slices.SortFunc(gotIDs, func(a, b [2]string) int { return strings.Compare(a[0]+a[1], b[0]+b[1]) })
	wantIDs := [][2]string{{roleIDs["role-0001"], roleIDs["role-0002"]}, {roleIDs["role-0002"], roleIDs["role-0003"]}}
	slices.SortFunc(wantIDs, func(a, b [2]string) int { return strings.Compare(a[0]+a[1], b[0]+b[1]) })
	if !slices.Equal(gotIDs, wantIDs) {
		t.Errorf("rules created between role ids %v, want %v", gotIDs, wantIDs)
	}
}

// TestApplyWithoutImpliedRolesCreatesNone confirms a plan without rules makes
// no CreateImpliedRole call.
func TestApplyWithoutImpliedRolesCreatesNone(t *testing.T) {
	f := newFake()
	if _, err := Apply(context.Background(), f, fullPlan(), keystone.TierAdmin, adminRes(), 4, time.Minute); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := f.countOps("implied_role"); n != 0 {
		t.Errorf("CreateImpliedRole called %d times for a plan without rules, want 0", n)
	}
}

// TestApplyImpliedRoleConflictStopsBeforeChildren confirms a 409 on the second
// rule is not retried, fails the run before any project or user is created,
// and leaves the roots made before it, the first rule among them, in Created
// for the teardown.
func TestApplyImpliedRoleConflictStopsBeforeChildren(t *testing.T) {
	f := newFake()
	f.conflict["role-0002->role-0003"] = true

	res, err := Apply(context.Background(), f, impliedRolePlan(), keystone.TierAdmin, adminRes(), 1, time.Minute)
	if !gophercloud.ResponseCodeIs(err, 409) {
		t.Fatalf("Apply error = %v, want the 409 of the second rule", err)
	}
	if n := f.attempts["role-0002->role-0003"]; n != 1 {
		t.Errorf("conflicting rule attempted %d times, want 1 (409 is terminal)", n)
	}
	if n := f.countOps("project") + f.countOps("user"); n != 0 {
		t.Errorf("%d project/user create(s) after a failed rule, want 0", n)
	}
	var kinds []resource.Kind
	for _, r := range res.Created {
		kinds = append(kinds, r.Kind)
	}
	want := []resource.Kind{keystone.KindDomain, keystone.KindRole, keystone.KindRole, keystone.KindRole, keystone.KindImpliedRole}
	if !slices.Equal(kinds, want) {
		t.Errorf("Created kinds = %v, want %v", kinds, want)
	}
	if last := res.Created[len(res.Created)-1]; last.Logical != "role-0001->role-0002" {
		t.Errorf("recorded rule = %q, want role-0001->role-0002", last.Logical)
	}
}

// TestApplyImpliedRoleRetriesTransient confirms a transient 503 on a rule is
// retried and the rule is created on the third attempt.
func TestApplyImpliedRoleRetriesTransient(t *testing.T) {
	f := newFake()
	f.failures["role-0001->role-0002"] = 2

	if _, err := Apply(context.Background(), f, impliedRolePlan(), keystone.TierAdmin, adminRes(), 1, time.Minute); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := f.attempts["role-0001->role-0002"]; n != 3 {
		t.Errorf("role-0001->role-0002 attempted %d times, want 3", n)
	}
}

// TestApplyDomainManagerIgnoresImpliedRoles confirms domain-manager mode
// creates no implied-role rule, since it reuses roles a manager may not
// change, and logs one warning that the plan's rules are ignored, and none
// for a plan without rules.
func TestApplyDomainManagerIgnoresImpliedRoles(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newFake()
	res := keystone.Resolution{DomainID: "dm-real", RoleIDs: []string{"role-a", "role-b"}}
	result, err := Apply(context.Background(), f, impliedRolePlan(), keystone.TierDomainManager, res, 4, time.Minute)
	if err != nil {
		t.Fatalf("Apply (domain-manager): %v", err)
	}
	if n := f.countOps("implied_role"); n != 0 {
		t.Errorf("domain-manager mode called CreateImpliedRole %d times, want 0", n)
	}
	for _, r := range result.Created {
		if r.Kind == keystone.KindImpliedRole {
			t.Errorf("domain-manager mode recorded the rule %+v", r)
		}
	}
	const msg = `level=WARN msg="implied-role rules are ignored in domain-manager mode, which reuses existing roles" rules=2`
	if n := strings.Count(logs.String(), msg); n != 1 {
		t.Errorf("log has %d line(s) %q, want 1:\n%s", n, msg, logs.String())
	}

	logs.Reset()
	if _, err := Apply(context.Background(), newFake(), fullPlan(), keystone.TierDomainManager, res, 4, time.Minute); err != nil {
		t.Fatalf("Apply (domain-manager, no rules): %v", err)
	}
	if strings.Contains(logs.String(), "implied-role rules are ignored") {
		t.Errorf("a plan without rules logged the implied-role warning:\n%s", logs.String())
	}
}

// delegationPlan is fullPlan with a second project-scoped grant (user-0002,
// role-0002 on proj-0002), one application credential on each user's
// project-scoped grant, and two trusts between the two users, each on its
// trustor's project-scoped grant.
func delegationPlan() *plan.Plan {
	p := fullPlan()
	p.Assignments = append(p.Assignments, plan.Assignment{User: "user-0002", Role: "role-0002", Project: "proj-0002"})
	p.ApplicationCredentials = []plan.ApplicationCredential{
		{Name: "appcred-0001", User: "user-0001", Project: "proj-0001", Role: "role-0001"},
		{Name: "appcred-0002", User: "user-0002", Project: "proj-0002", Role: "role-0002"},
	}
	p.Trusts = []plan.Trust{
		{Name: "trust-0001", Trustor: "user-0001", Trustee: "user-0002", Project: "proj-0001", Role: "role-0001"},
		{Name: "trust-0002", Trustor: "user-0002", Trustee: "user-0001", Project: "proj-0002", Role: "role-0002"},
	}
	return p
}

// cloudIDs maps the logical name of every created domain, role, project and
// user to its cloud id. Assignments are left out, since their logical name is
// the user's.
func cloudIDs(rs []resource.Resource) map[string]string {
	ids := make(map[string]string)
	for _, r := range rs {
		switch r.Kind {
		case keystone.KindDomain, keystone.KindRole, keystone.KindProject, keystone.KindUser:
			ids[r.Logical] = r.ID
		}
	}
	return ids
}

// TestApplyDelegationsAfterTokens confirms Apply runs the token issues, then
// every application credential create and authentication, then every trust
// create and authentication; records both kinds in Created after the
// assignments, credentials before trusts, in plan order; hands each create the
// owner's cloud ids, domain and create-time password; and authenticates each
// with the resource its create returned, a trust as its trustee.
func TestApplyDelegationsAfterTokens(t *testing.T) {
	p := delegationPlan()
	if err := p.Validate(); err != nil {
		t.Fatalf("delegationPlan is invalid: %v", err)
	}
	f := newFake()
	res, err := Apply(context.Background(), f, p, keystone.TierAdmin, adminRes(), 4, time.Minute)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	rank := map[string]int{
		"domain": 0, "role": 1, "project": 2, "user": 2, "assign": 3, "token": 4,
		"appcred": 5, "appcred_auth": 5, "trust": 6, "trust_auth": 6,
	}
	phase := 0
	for i, op := range f.opOrder {
		r, ok := rank[op]
		if !ok {
			t.Fatalf("unexpected op %q at index %d in %v", op, i, f.opOrder)
		}
		if r < phase {
			t.Fatalf("op %q at index %d violates stage order %v", op, i, f.opOrder)
		}
		phase = r
	}
	for op, want := range map[string]int{"token": 1, "appcred": 2, "appcred_auth": 2, "trust": 2, "trust_auth": 2} {
		if n := f.countOps(op); n != want {
			t.Errorf("%s called %d times, want %d (ops %v)", op, n, want, f.opOrder)
		}
	}

	var kinds []resource.Kind
	for _, r := range res.Created {
		kinds = append(kinds, r.Kind)
	}
	wantKinds := []resource.Kind{
		keystone.KindDomain, keystone.KindRole, keystone.KindRole,
		keystone.KindProject, keystone.KindProject, keystone.KindUser, keystone.KindUser,
		keystone.KindAssignment, keystone.KindAssignment, keystone.KindAssignment,
		keystone.KindApplicationCredential, keystone.KindApplicationCredential,
		keystone.KindTrust, keystone.KindTrust,
	}
	if !slices.Equal(kinds, wantKinds) {
		t.Fatalf("Created kinds = %v, want %v", kinds, wantKinds)
	}
	var logicals []string
	for _, r := range res.Created[10:] {
		logicals = append(logicals, r.Logical)
	}
	if want := []string{"appcred-0001", "appcred-0002", "trust-0001", "trust-0002"}; !slices.Equal(logicals, want) {
		t.Errorf("Created delegations = %v, want %v", logicals, want)
	}

	ids := cloudIDs(res.Created)
	domainID := ids["dom-0001"]
	for i, ac := range p.ApplicationCredentials {
		got := f.appcreds[ac.Name]
		want := appcredCall{
			userID: ids[ac.User], userDomainID: domainID, password: f.userPassword[ac.User],
			projectID: ids[ac.Project], roleID: ids[ac.Role], secret: got.secret,
		}
		if got != want {
			t.Errorf("CreateApplicationCredential(%q) got %+v, want %+v", ac.Name, got, want)
		}
		if created := res.Created[10+i]; f.appcredAuth[ac.Name].res != created {
			t.Errorf("credential %q authenticated with %+v, want the created %+v", ac.Name, f.appcredAuth[ac.Name].res, created)
		}
	}
	for i, tr := range p.Trusts {
		got := f.trusts[tr.Name]
		want := trustCall{
			trustorID: ids[tr.Trustor], trustorDomainID: domainID, trustorPassword: f.userPassword[tr.Trustor],
			trusteeID: ids[tr.Trustee], projectID: ids[tr.Project], roleID: ids[tr.Role],
		}
		if got != want {
			t.Errorf("CreateTrust(%q) got %+v, want %+v", tr.Name, got, want)
		}
		wantAuth := trustAuthCall{res: res.Created[12+i], trusteeDomainID: domainID, trusteePassword: f.userPassword[tr.Trustee]}
		if gotAuth := f.trustAuth[tr.Name]; gotAuth != wantAuth {
			t.Errorf("AuthenticateTrust(%q) got %+v, want %+v", tr.Name, gotAuth, wantAuth)
		}
	}
}

// TestApplyDelegationsUseEachUsersDomain confirms each delegation call takes
// the domain of the user it acts as when the users live in different domains:
// a credential its owner's, a trust create its trustor's and a trust
// authentication its trustee's. A single-domain plan cannot tell them apart.
func TestApplyDelegationsUseEachUsersDomain(t *testing.T) {
	p := &plan.Plan{
		Scenario: "test", Seed: 7,
		Domains:  []plan.Domain{{Name: "dom-0001"}, {Name: "dom-0002"}},
		Roles:    []plan.Role{{Name: "role-0001"}, {Name: "role-0002"}},
		Projects: []plan.Project{{Name: "proj-0001", Domain: "dom-0001"}, {Name: "proj-0002", Domain: "dom-0002"}},
		Users:    []plan.User{{Name: "user-0001", Domain: "dom-0001"}, {Name: "user-0002", Domain: "dom-0002"}},
		Assignments: []plan.Assignment{
			{User: "user-0001", Role: "role-0001", Project: "proj-0001"},
			{User: "user-0002", Role: "role-0002", Project: "proj-0002"},
		},
		ApplicationCredentials: []plan.ApplicationCredential{
			{Name: "appcred-0001", User: "user-0002", Project: "proj-0002", Role: "role-0002"},
		},
		Trusts: []plan.Trust{
			{Name: "trust-0001", Trustor: "user-0001", Trustee: "user-0002", Project: "proj-0001", Role: "role-0001"},
			{Name: "trust-0002", Trustor: "user-0002", Trustee: "user-0001", Project: "proj-0002", Role: "role-0002"},
		},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("plan is invalid: %v", err)
	}
	f := newFake()
	res, err := Apply(context.Background(), f, p, keystone.TierAdmin, adminRes(), 4, time.Minute)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ids := cloudIDs(res.Created)
	userDomain := map[string]string{"user-0001": ids["dom-0001"], "user-0002": ids["dom-0002"]}
	if userDomain["user-0001"] == "" || userDomain["user-0001"] == userDomain["user-0002"] {
		t.Fatalf("domain ids %v, want two distinct created domains", userDomain)
	}

	if got, want := f.appcreds["appcred-0001"].userDomainID, userDomain["user-0002"]; got != want {
		t.Errorf("CreateApplicationCredential domain = %q, want the owner's %q", got, want)
	}
	for _, tr := range p.Trusts {
		if got, want := f.trusts[tr.Name].trustorDomainID, userDomain[tr.Trustor]; got != want {
			t.Errorf("CreateTrust(%q) domain = %q, want the trustor's %q", tr.Name, got, want)
		}
		if got, want := f.trustAuth[tr.Name].trusteeDomainID, userDomain[tr.Trustee]; got != want {
			t.Errorf("AuthenticateTrust(%q) domain = %q, want the trustee's %q", tr.Name, got, want)
		}
	}
}

// TestApplyApplicationCredentialSecrets confirms each application credential
// is created and authenticated with the same secret, that the two credentials
// get different secrets, and that no secret equals a user password.
func TestApplyApplicationCredentialSecrets(t *testing.T) {
	p := delegationPlan()
	f := newFake()
	if _, err := Apply(context.Background(), f, p, keystone.TierAdmin, adminRes(), 4, time.Minute); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.userPassword) != len(p.Users) {
		t.Fatalf("fake saw %d user passwords, want %d", len(f.userPassword), len(p.Users))
	}

	secrets := make(map[string]string, len(p.ApplicationCredentials))
	for _, ac := range p.ApplicationCredentials {
		secret := f.appcreds[ac.Name].secret
		if secret == "" {
			t.Errorf("credential %q created with an empty secret", ac.Name)
			continue
		}
		if auth := f.appcredAuth[ac.Name].secret; auth != secret {
			t.Errorf("credential %q authenticated with secret %q, want its create-time secret %q", ac.Name, auth, secret)
		}
		for user, pw := range f.userPassword {
			if secret == pw {
				t.Errorf("secret of credential %q equals the password of %q", ac.Name, user)
			}
		}
		secrets[ac.Name] = secret
	}
	if a, b := secrets["appcred-0001"], secrets["appcred-0002"]; a == b {
		t.Errorf("appcred-0001 and appcred-0002 got the same secret %q, want one each", a)
	}
}

// TestApplyDelegationSecretsNotEncoded confirms neither the plan nor the
// Created list of a run with both kinds carries, once encoded as JSON, any
// secret or password the fake received.
func TestApplyDelegationSecretsNotEncoded(t *testing.T) {
	p := delegationPlan()
	f := newFake()
	res, err := Apply(context.Background(), f, p, keystone.TierAdmin, adminRes(), 4, time.Minute)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	planJSON, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("encoding the plan: %v", err)
	}
	createdJSON, err := json.Marshal(res.Created)
	if err != nil {
		t.Fatalf("encoding Result.Created: %v", err)
	}
	encoded := map[string][]byte{"plan": planJSON, "Result.Created": createdJSON}
	for what, enc := range encoded {
		for _, name := range []string{"appcred-0001", "appcred-0002", "trust-0001", "trust-0002"} {
			if !bytes.Contains(enc, []byte(name)) {
				t.Errorf("%s JSON does not name %q; it must cover the delegations: %s", what, name, enc)
			}
		}
	}

	var received []string
	for _, pw := range f.userPassword {
		received = append(received, pw)
	}
	for _, pw := range f.tokenPassword {
		received = append(received, pw)
	}
	for _, c := range f.appcreds {
		received = append(received, c.password, c.secret)
	}
	for _, c := range f.appcredAuth {
		received = append(received, c.secret)
	}
	for _, c := range f.trusts {
		received = append(received, c.trustorPassword)
	}
	for _, c := range f.trustAuth {
		received = append(received, c.trusteePassword)
	}
	// 2 users + 1 token + 2x(password, secret) + 2 auth secrets + 2 trustor and 2 trustee passwords.
	if len(received) != 13 {
		t.Fatalf("fake received %d secrets and passwords, want 13", len(received))
	}
	for _, s := range received {
		if s == "" {
			t.Error("fake received an empty secret or password")
			continue
		}
		for what, enc := range encoded {
			if bytes.Contains(enc, []byte(s)) {
				t.Errorf("%s JSON contains the secret or password %q: %s", what, s, enc)
			}
		}
	}
}

// TestApplyApplicationCredentialAuthFailure confirms a 401 on a credential's
// authentication fails the run after one attempt (a 401 is not retried),
// leaves the created credential in Created for the record, and skips the
// trust stage.
func TestApplyApplicationCredentialAuthFailure(t *testing.T) {
	f := newFake()
	f.authFail["appcred-0001"] = true

	res, err := Apply(context.Background(), f, delegationPlan(), keystone.TierAdmin, adminRes(), 1, time.Minute)
	if !gophercloud.ResponseCodeIs(err, 401) {
		t.Fatalf("Apply error = %v, want the 401 of appcred-0001's authentication", err)
	}
	if n := f.attempts["auth:appcred-0001"]; n != 1 {
		t.Errorf("appcred-0001 authenticated %d times, want 1 (a 401 is not retried)", n)
	}
	created := f.appcredAuth["appcred-0001"].res
	if created.Kind != keystone.KindApplicationCredential || created.Logical != "appcred-0001" || created.ID == "" {
		t.Fatalf("appcred-0001 authenticated with %+v, want its created credential", created)
	}
	if !slices.Contains(res.Created, created) {
		t.Errorf("Created = %+v, want it to hold the created credential %+v", res.Created, created)
	}
	if n := f.countOps("trust") + f.countOps("trust_auth"); n != 0 {
		t.Errorf("%d trust call(s) after a failed credential authentication, want 0 (ops %v)", n, f.opOrder)
	}
}

// TestApplyTrustAuthFailure confirms a 401 on a trust's authentication fails
// the run and leaves the created trust in Created for the record.
func TestApplyTrustAuthFailure(t *testing.T) {
	f := newFake()
	f.authFail["trust-0001"] = true

	res, err := Apply(context.Background(), f, delegationPlan(), keystone.TierAdmin, adminRes(), 1, time.Minute)
	if !gophercloud.ResponseCodeIs(err, 401) {
		t.Fatalf("Apply error = %v, want the 401 of trust-0001's authentication", err)
	}
	if n := f.attempts["auth:trust-0001"]; n != 1 {
		t.Errorf("trust-0001 authenticated %d times, want 1 (a 401 is not retried)", n)
	}
	created := f.trustAuth["trust-0001"].res
	if created.Kind != keystone.KindTrust || created.Logical != "trust-0001" || created.ID == "" {
		t.Fatalf("trust-0001 authenticated with %+v, want its created trust", created)
	}
	if !slices.Contains(res.Created, created) {
		t.Errorf("Created = %+v, want it to hold the created trust %+v", res.Created, created)
	}
	if n := countKinds(res.Created)[keystone.KindApplicationCredential]; n != 2 {
		t.Errorf("Created holds %d application credentials, want both from the stage before", n)
	}
}

// TestApplyDomainManagerDelegations confirms domain-manager mode creates each
// application credential and trust with the reused role its logical role binds
// onto, in the in-scope domain, with the create-time password of the owner (a
// credential's user, a trust's trustor), and creates no domain or role.
func TestApplyDomainManagerDelegations(t *testing.T) {
	p := delegationPlan()
	f := newFake()
	res := keystone.Resolution{DomainID: "dm-real", RoleIDs: []string{"role-a", "role-b"}}

	result, err := Apply(context.Background(), f, p, keystone.TierDomainManager, res, 4, time.Minute)
	if err != nil {
		t.Fatalf("Apply (domain-manager): %v", err)
	}

	reused := map[string]string{"role-0001": "role-a", "role-0002": "role-b"}
	for _, ac := range p.ApplicationCredentials {
		got := f.appcreds[ac.Name]
		if got.roleID != reused[ac.Role] {
			t.Errorf("credential %q created with role id %q, want the reused %q", ac.Name, got.roleID, reused[ac.Role])
		}
		if got.password == "" || got.password != f.userPassword[ac.User] {
			t.Errorf("credential %q created with password %q, want %q's create-time password %q", ac.Name, got.password, ac.User, f.userPassword[ac.User])
		}
		if got.userDomainID != "dm-real" {
			t.Errorf("credential %q created with user domain %q, want the in-scope dm-real", ac.Name, got.userDomainID)
		}
	}
	for _, tr := range p.Trusts {
		got := f.trusts[tr.Name]
		if got.roleID != reused[tr.Role] {
			t.Errorf("trust %q created with role id %q, want the reused %q", tr.Name, got.roleID, reused[tr.Role])
		}
		if got.trustorPassword == "" || got.trustorPassword != f.userPassword[tr.Trustor] {
			t.Errorf("trust %q created with password %q, want trustor %q's create-time password %q", tr.Name, got.trustorPassword, tr.Trustor, f.userPassword[tr.Trustor])
		}
		if got.trustorDomainID != "dm-real" {
			t.Errorf("trust %q created with trustor domain %q, want the in-scope dm-real", tr.Name, got.trustorDomainID)
		}
	}

	for _, op := range f.opOrder {
		if op == "domain" || op == "role" {
			t.Errorf("domain-manager mode created a %s; roots must be reused, not created", op)
		}
	}
	got := countKinds(result.Created)
	if got[keystone.KindDomain] != 0 || got[keystone.KindRole] != 0 {
		t.Errorf("domain-manager mode recorded created roots: %v", got)
	}
	if got[keystone.KindApplicationCredential] != 2 || got[keystone.KindTrust] != 2 {
		t.Errorf("Created kinds = %v, want 2 application credentials and 2 trusts", got)
	}
}

// TestApplyWithoutDelegationsCallsNone confirms a plan with neither
// application credentials nor trusts makes no call to any Delegations method.
func TestApplyWithoutDelegationsCallsNone(t *testing.T) {
	f := newFake()
	if _, err := Apply(context.Background(), f, fullPlan(), keystone.TierAdmin, adminRes(), 4, time.Minute); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, op := range []string{"appcred", "appcred_auth", "trust", "trust_auth"} {
		if n := f.countOps(op); n != 0 {
			t.Errorf("%s called %d times for a plan without delegations, want 0", op, n)
		}
	}
}

// TestApplyApplicationCredentialForbidden confirms a 403 on a credential
// create fails the run with ErrForbidden after one attempt (no retry), before
// that credential is authenticated and before any trust call.
func TestApplyApplicationCredentialForbidden(t *testing.T) {
	f := newFake()
	f.forbid["appcred-0001"] = true

	_, err := Apply(context.Background(), f, delegationPlan(), keystone.TierAdmin, adminRes(), 1, time.Minute)
	if !errors.Is(err, keystone.ErrForbidden) {
		t.Fatalf("Apply error = %v, want one matching keystone.ErrForbidden", err)
	}
	if n := f.attempts["appcred-0001"]; n != 1 {
		t.Errorf("forbidden credential create attempted %d times, want 1 (no retry)", n)
	}
	if n := f.attempts["auth:appcred-0001"]; n != 0 {
		t.Errorf("a credential that was not created was authenticated %d times, want 0", n)
	}
	if n := f.countOps("trust") + f.countOps("trust_auth"); n != 0 {
		t.Errorf("%d trust call(s) after a forbidden credential create, want 0 (ops %v)", n, f.opOrder)
	}
}

// TestApplyTrustRetriesTransient confirms a transient 503 on a trust create is
// retried and the trust is created on the third attempt.
func TestApplyTrustRetriesTransient(t *testing.T) {
	f := newFake()
	f.failures["trust-0001"] = 2 // fail twice, succeed on the third attempt

	if _, err := Apply(context.Background(), f, delegationPlan(), keystone.TierAdmin, adminRes(), 1, time.Minute); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := f.attempts["trust-0001"]; n != 3 {
		t.Errorf("trust-0001 attempted %d times, want 3", n)
	}
	if n := f.attempts["auth:trust-0001"]; n != 1 {
		t.Errorf("trust-0001 authenticated %d times, want 1", n)
	}
}

// TestApplyCancellationInDelegations confirms cancelling while an application
// credential create blocks in stage 5 stops promptly with context.Canceled,
// records no credential, and makes no trust call.
func TestApplyCancellationInDelegations(t *testing.T) {
	f := newFake()
	f.holdAppCredUntilCancel = true
	f.appcredStarted = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		res *Result
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := Apply(ctx, f, delegationPlan(), keystone.TierAdmin, adminRes(), 1, time.Minute)
		done <- result{res, err}
	}()

	select {
	case <-f.appcredStarted:
	case r := <-done:
		cancel()
		t.Fatalf("Apply returned %v before reaching stage 5", r.err)
	}
	cancel()

	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("Apply returned %v, want context.Canceled", r.err)
		}
		if n := countKinds(r.res.Created)[keystone.KindApplicationCredential]; n != 0 {
			t.Errorf("Created holds %d application credential(s) after the held create was cancelled, want 0", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Apply did not return after cancellation")
	}
	if n := f.countOps("trust") + f.countOps("trust_auth"); n != 0 {
		t.Errorf("%d trust call(s) after a cancelled stage 5, want 0 (ops %v)", n, f.opOrder)
	}
}
