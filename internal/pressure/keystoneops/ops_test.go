package keystoneops

import (
	"context"
	"errors"
	"maps"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/pressure"
)

// churnOn is a mix in which grant_churn draws, so Build reserves churn users.
var churnOn = map[string]int{OpValidate: 60, OpGrantChurn: 2}

// churnOff is a mix without grant_churn, so Build reserves no churn user.
var churnOff = map[string]int{OpValidate: 60, OpRevoke: 2, OpIssue: 1}

// oneUserPlan is a valid plan with a single user holding a single
// project-scoped grant.
func oneUserPlan() *keystoneplan.Plan {
	return &keystoneplan.Plan{
		Scenario: "one", Seed: 1,
		Domains:     []keystoneplan.Domain{{Name: "dom-0001"}},
		Roles:       []keystoneplan.Role{{Name: "role-0001"}},
		Projects:    []keystoneplan.Project{{Name: "proj-0001", Domain: "dom-0001"}},
		Users:       []keystoneplan.User{{Name: "user-0001", Domain: "dom-0001"}},
		Assignments: []keystoneplan.Assignment{{User: "user-0001", Role: "role-0001", Project: "proj-0001"}},
	}
}

// TestBuildErrors covers every error Build returns, with its exact message,
// for the plans and options that trigger it.
func TestBuildErrors(t *testing.T) {
	tests := []struct {
		name      string
		plan      func() *keystoneplan.Plan
		opts      Options
		ids       func(map[string]string)
		passwords func(map[string]string)
		bindings  func(map[string]string, map[string]string)
		wantErr   string
	}{
		{
			name: "churn users leave no pool user",
			plan: oneUserPlan,
			opts: Options{Tokens: 1, ChurnUsers: 1, Weights: map[string]int{OpGrantChurn: 1}, OpTimeout: time.Second},
			// churn_users 1 with a one-user plan and grant_churn 1.
			wantErr: "pressure.churn_users (1) must leave at least one user outside the churn set; the plan has 1 users",
		},
		{
			name: "only domain-scoped assignments",
			plan: func() *keystoneplan.Plan {
				p := oneUserPlan()
				p.Assignments = []keystoneplan.Assignment{{User: "user-0001", Role: "role-0001"}}
				return p
			},
			opts:    Options{Tokens: 1, Weights: map[string]int{OpValidate: 1}, OpTimeout: time.Second},
			wantErr: "pressure needs at least one project-scoped assignment outside the churn users to issue pool tokens from; the plan has none",
		},
		{
			name: "only the churn users hold project-scoped assignments",
			plan: func() *keystoneplan.Plan {
				p := pressurePlan()
				p.Assignments = []keystoneplan.Assignment{
					{User: "user-0003", Role: "role-0001"},
					{User: "user-0004", Role: "role-0001", Project: "proj-0002"},
				}
				p.Tokens = nil
				return p
			},
			opts:    fixtureOptions(1, churnOn),
			wantErr: "pressure needs at least one project-scoped assignment outside the churn users to issue pool tokens from; the plan has none",
		},
		{
			name:    "zero tokens",
			plan:    pressurePlan,
			opts:    fixtureOptions(0, churnOn),
			wantErr: "pressure tokens must be at least 1, got 0",
		},
		{
			name:    "negative churn users",
			plan:    pressurePlan,
			opts:    Options{Tokens: 1, ChurnUsers: -1, Weights: churnOn, OpTimeout: time.Second},
			wantErr: "pressure churn users must not be negative, got -1",
		},
		{
			name:    "zero op timeout",
			plan:    pressurePlan,
			opts:    Options{Tokens: 1, ChurnUsers: 1, Weights: churnOn},
			wantErr: "pressure op-timeout must be positive, got 0s",
		},
		{
			name: "churn users hold no assignment",
			plan: func() *keystoneplan.Plan {
				p := pressurePlan()
				p.Users = append(p.Users, keystoneplan.User{Name: "user-0005", Domain: "dom-0002"})
				return p
			},
			opts:    fixtureOptions(1, churnOn),
			wantErr: "pressure grant_churn needs the churn users to hold at least one assignment; the last 1 user(s) of the plan hold none",
		},
		{
			name: "invalid plan",
			plan: func() *keystoneplan.Plan {
				p := pressurePlan()
				p.Assignments = append(p.Assignments, keystoneplan.Assignment{User: "user-0009", Role: "role-0001"})
				return p
			},
			opts:    fixtureOptions(1, churnOn),
			wantErr: `invalid plan: assignment references unknown user "user-0009"`,
		},
		{
			name:    "missing user id",
			plan:    pressurePlan,
			opts:    fixtureOptions(1, churnOn),
			ids:     func(m map[string]string) { delete(m, "user-0002") },
			wantErr: `pressure set: no cloud id for user "user-0002"`,
		},
		{
			name:    "missing project id",
			plan:    pressurePlan,
			opts:    fixtureOptions(1, churnOn),
			ids:     func(m map[string]string) { delete(m, "proj-0002") },
			wantErr: `pressure set: no cloud id for project "proj-0002"`,
		},
		{
			name:      "missing password",
			plan:      pressurePlan,
			opts:      fixtureOptions(1, churnOn),
			passwords: func(m map[string]string) { delete(m, "user-0003") },
			wantErr:   `pressure set: no password for user "user-0003"`,
		},
		{
			name:     "missing domain id",
			plan:     pressurePlan,
			opts:     fixtureOptions(1, churnOn),
			bindings: func(domains, _ map[string]string) { delete(domains, "dom-0002") },
			wantErr:  `pressure set: no cloud id for domain "dom-0002" of user "user-0002"`,
		},
		{
			name:     "missing role id of a churn grant",
			plan:     pressurePlan,
			opts:     fixtureOptions(1, churnOn),
			bindings: func(_, roles map[string]string) { delete(roles, "role-0002") },
			wantErr:  `pressure set: no cloud id for role "role-0002"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, passwords, b := fixtureIDs(), fixturePasswords(), fixtureBindings()
			if tt.ids != nil {
				tt.ids(ids)
			}
			if tt.passwords != nil {
				tt.passwords(passwords)
			}
			if tt.bindings != nil {
				tt.bindings(b.Domains, b.Roles)
			}
			f := newFake(newFakeClock(), time.Hour)
			s, err := Build(tt.plan(), f, b, ids, passwords, tt.opts)
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("Build error = %v, want %q", err, tt.wantErr)
			}
			if s != nil {
				t.Errorf("Build returned a set with its error")
			}
			if calls := f.recorded(); len(calls) != 0 {
				t.Errorf("Build made %d cloud calls, want none", len(calls))
			}
		})
	}
}

// TestBuildPartition checks which users Build reserves for grant_churn and
// which (user, project) pairs enter the pool, by priming a pool of one token
// per pair and reading the pairs the issues used.
func TestBuildPartition(t *testing.T) {
	type pairArgs struct{ user, project string }
	poolPairs := func(t *testing.T, o Options) ([]pairArgs, *Set) {
		t.Helper()
		f := newFake(newFakeClock(), time.Hour)
		s := buildSet(t, f, o)
		if err := s.Prime(context.Background(), 1); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		var got []pairArgs
		for _, c := range f.recorded(kindIssue) {
			got = append(got, pairArgs{c.user, c.project})
		}
		return got, s
	}

	t.Run("grant_churn weight 0 reserves no user", func(t *testing.T) {
		o := fixtureOptions(4, churnOff) // ChurnUsers is 1, the weight 0
		got, s := poolPairs(t, o)
		want := []pairArgs{
			{"user-0001", "proj-0001"},
			{"user-0002", "proj-0002"},
			{"user-0003", "proj-0001"},
			{"user-0004", "proj-0002"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("pool pairs = %v, want every project-scoped pair %v", got, want)
		}
		if len(s.churn) != 0 {
			t.Errorf("%d churn grants reserved, want none", len(s.churn))
		}
	})

	t.Run("grant_churn reserves the last users", func(t *testing.T) {
		got, s := poolPairs(t, fixtureOptions(3, churnOn))
		want := []pairArgs{
			{"user-0001", "proj-0001"},
			{"user-0002", "proj-0002"},
			{"user-0003", "proj-0001"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("pool pairs = %v, want %v", got, want)
		}
		var churned []keystoneplan.Assignment
		for _, g := range s.churn {
			churned = append(churned, g.a)
		}
		wantChurn := []keystoneplan.Assignment{
			{User: "user-0004", Role: "role-0001", Project: "proj-0002"},
			{User: "user-0004", Role: "role-0002"},
		}
		if !reflect.DeepEqual(churned, wantChurn) {
			t.Errorf("churn grants = %v, want user-0004's %v", churned, wantChurn)
		}
	})

	t.Run("two churn users", func(t *testing.T) {
		o := fixtureOptions(2, churnOn)
		o.ChurnUsers = 2
		got, _ := poolPairs(t, o)
		want := []pairArgs{{"user-0001", "proj-0001"}, {"user-0002", "proj-0002"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("pool pairs = %v, want %v", got, want)
		}
	})
}

// TestBuildCopiesWeights checks that a change to the caller's weight map after
// Build does not change the operations' weights.
func TestBuildCopiesWeights(t *testing.T) {
	f := newFake(newFakeClock(), time.Hour)
	weights := map[string]int{OpValidate: 60}
	s, err := Build(pressurePlan(), f, fixtureBindings(), fixtureIDs(), fixturePasswords(), Options{Tokens: 1, Weights: weights, OpTimeout: time.Second})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	weights[OpValidate] = 1
	if got := s.Operations()[0].Weight; got != 60 {
		t.Errorf("validate weight = %d after the caller changed its map, want 60", got)
	}
}

// TestOperationsNamesAndWeights checks that Operations returns all five
// operations in order, each with its weight from Options, and 0 for a missing
// weight.
func TestOperationsNamesAndWeights(t *testing.T) {
	f := newFake(newFakeClock(), time.Hour)
	s := buildSet(t, f, fixtureOptions(1, map[string]int{OpValidate: 60, OpRevoke: 2, OpIssue: 1}))
	ops := s.Operations()
	var names []string
	weights := make(map[string]int)
	for _, op := range ops {
		names = append(names, op.Name)
		weights[op.Name] = op.Weight
		if op.Do == nil {
			t.Errorf("operation %q has no Do", op.Name)
		}
	}
	wantNames := []string{"validate", "effective", "revoke", "grant_churn", "issue"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("names = %v, want %v", names, wantNames)
	}
	wantWeights := map[string]int{"validate": 60, "effective": 0, "revoke": 2, "grant_churn": 0, "issue": 1}
	if !maps.Equal(weights, wantWeights) {
		t.Errorf("weights = %v, want %v", weights, wantWeights)
	}
	if err := pressure.ValidateOperations(ops); err != nil {
		t.Errorf("the engine rejects the operations: %v", err)
	}
}

// TestPrimeRoundRobin primes 7 tokens over the 3 pool pairs: the pairs get 3,
// 2 and 2 tokens in plan order, every issue carries the pair's user, domain
// id, password and project id, and with concurrency 2 two issues run at once
// and never more.
func TestPrimeRoundRobin(t *testing.T) {
	f := newFake(newFakeClock(), time.Hour)
	f.holdIssues(2) // the first two issues return only once both are in flight
	s := buildSet(t, f, fixtureOptions(7, churnOn))
	if err := s.Prime(context.Background(), 2); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	type issueArgs struct{ user, project, domainID, password, projectID string }
	want := map[issueArgs]int{
		{"user-0001", "proj-0001", "did-1", "pw-1", "pid-1"}: 3,
		{"user-0002", "proj-0002", "did-2", "pw-2", "pid-2"}: 2,
		{"user-0003", "proj-0001", "did-1", "pw-3", "pid-1"}: 2,
	}
	got := make(map[issueArgs]int)
	for _, c := range f.recorded(kindIssue) {
		got[issueArgs{c.user, c.project, c.domainID, c.password, c.projectID}]++
	}
	if !maps.Equal(got, want) {
		t.Errorf("issues per pair = %v, want %v", got, want)
	}
	if n := f.maxIssuesInFlight(); n != 2 {
		t.Errorf("at most %d issues were in flight at once, want 2 (the concurrency)", n)
	}
	if n := s.pool.len(); n != 7 {
		t.Errorf("pool size = %d, want 7", n)
	}
	// The pool holds the tokens in issue order: entry k for pair k mod 3.
	for k, e := range poolEntries(s) {
		if e.pair != k%3 {
			t.Errorf("pool entry %d is for pair %d, want %d", k, e.pair, k%3)
		}
	}
}

// TestPrimeConcurrencyFloor checks that a concurrency below 1 primes the pool
// one token at a time instead of none: Prime with concurrency 0 over four
// tokens has exactly one issue in flight at every step, and issues every token
// once each is let through. The fake holds every issue at its gate and the
// test lets them through one at a time. In the synctest bubble, synctest.Wait
// returns once every goroutine is blocked, so Prime has started every issue it
// can start before the test counts them: a floor of 2 or 3 has more than one
// in flight, and a missing floor, which blocks Prime on its semaphore before
// the first issue, has none.
func TestPrimeConcurrencyFloor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const tokens = 4 // more than the 3 pool pairs, and more than a floor of 3
		f := newFake(newFakeClock(), time.Hour)
		f.gateIssues(t)
		s := buildSet(t, f, fixtureOptions(tokens, churnOn))
		primed := make(chan error, 1)
		go func() { primed <- s.Prime(context.Background(), 0) }()

		for released := range tokens {
			synctest.Wait()
			if n := f.issuesInFlight(); n != 1 {
				t.Fatalf("%d issues in flight with %d of %d let through, want 1 (a concurrency of 0 primes one token at a time)", n, released, tokens)
			}
			f.releaseIssue(t)
		}
		synctest.Wait()
		select {
		case err := <-primed:
			if err != nil {
				t.Fatalf("Prime: %v", err)
			}
		default:
			t.Fatal("Prime did not return after every issue was let through")
		}

		if n := len(f.recorded(kindIssue)); n != tokens {
			t.Errorf("%d issues made, want %d", n, tokens)
		}
		if n := s.pool.len(); n != tokens {
			t.Errorf("pool size = %d, want %d", n, tokens)
		}
		if n := f.maxIssuesInFlight(); n != 1 {
			t.Errorf("at most %d issues were in flight at once, want 1", n)
		}
	})
}

// TestPrimeFailure checks that a failed issue stops priming: with concurrency 1
// and the fourth issue failing with a 500, Prime returns the failure wrapped
// with the count of tokens issued, makes no further issue and starts no
// refresher.
func TestPrimeFailure(t *testing.T) {
	f := newFake(newFakeClock(), time.Hour)
	f.failNth(kindIssue, 4, gophercloud.ErrUnexpectedResponseCode{Actual: 500})
	s := buildSet(t, f, fixtureOptions(7, churnOn))

	err := s.Prime(context.Background(), 1)
	if err == nil {
		t.Fatal("Prime succeeded, want the fourth issue's error")
	}
	if !strings.HasPrefix(err.Error(), "priming token pool (3 of 7 issued): ") {
		t.Errorf("error = %q, want the prefix %q", err, "priming token pool (3 of 7 issued): ")
	}
	var code gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &code) || code.Actual != 500 {
		t.Errorf("error %v does not wrap the 500", err)
	}
	if n := len(f.recorded(kindIssue)); n != 4 {
		t.Errorf("%d issues made, want 4 (none after the failure)", n)
	}
	if s.done != nil {
		t.Error("a failed Prime started the refresher")
	}
}

// TestPrimeFailureWaitsForInFlightIssues checks that a failed issue does not
// end Prime while another issue is in flight: with concurrency 2 one issue is
// held at the fake's gate while the other fails with a 500, and Prime returns
// only after the held issue was let through, counting it among the issued
// tokens. In the synctest bubble, synctest.Wait returns once every goroutine
// is blocked, so a Prime that does not wait has returned by then.
func TestPrimeFailureWaitsForInFlightIssues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		f.failNth(kindIssue, 2, gophercloud.ErrUnexpectedResponseCode{Actual: 500})
		f.gateIssues(t)
		s := buildSet(t, f, fixtureOptions(7, churnOn))
		type result struct {
			err      error
			inFlight int // the fake's issues in flight when Prime returned
		}
		primed := make(chan result, 1)
		go func() {
			err := s.Prime(context.Background(), 2)
			primed <- result{err, f.issuesInFlight()}
		}()

		// One issue is held at the gate and the other failed, in either
		// order.
		synctest.Wait()
		select {
		case r := <-primed:
			t.Fatalf("Prime returned (%v) while an issue was still in flight", r.err)
		default:
		}
		if n := len(f.recorded(kindIssue)); n != 2 {
			t.Fatalf("%d issues made before the held one returned, want 2", n)
		}
		f.releaseIssue(t)
		synctest.Wait()
		var r result
		select {
		case r = <-primed:
		default:
			t.Fatal("Prime did not return after the held issue was let through")
		}

		if r.inFlight != 0 {
			t.Errorf("Prime returned with %d issues in flight, want 0", r.inFlight)
		}
		if want := "priming token pool (1 of 7 issued): "; r.err == nil || !strings.HasPrefix(r.err.Error(), want) {
			t.Errorf("error = %v, want the prefix %q", r.err, want)
		}
		var code gophercloud.ErrUnexpectedResponseCode
		if !errors.As(r.err, &code) || code.Actual != 500 {
			t.Errorf("error %v does not wrap the 500", r.err)
		}
		if n := len(f.recorded(kindIssue)); n != 2 {
			t.Errorf("%d issues made, want 2 (none after the failure)", n)
		}
		if s.done != nil {
			t.Error("a failed Prime started the refresher")
		}
	})
}

// TestPrimeLifetimeFromFirstToken checks that the refresh margin derives from
// the lifetime of the first token, measured from just before its issue
// started: every issue takes 2s on the fake's clock, the first token lives 40s
// after its issue returned and the later ones 4h, so the lifetime is 42s and
// the margin a quarter of it.
func TestPrimeLifetimeFromFirstToken(t *testing.T) {
	f := newFake(newFakeClock(), 4*time.Hour)
	f.setIssueTiming(2*time.Second, 40*time.Second)
	s := buildSet(t, f, fixtureOptions(3, churnOn))
	if err := s.Prime(context.Background(), 1); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	s.mu.Lock()
	margin := s.margin
	s.mu.Unlock()
	if want := 42 * time.Second / 4; margin != want {
		t.Errorf("margin = %s, want %s (a quarter of the first token's 42s lifetime)", margin, want)
	}
}

// TestPrimeAndRefreshIssuesBoundedByOpTimeout checks that every issue Prime
// and the refresher make runs on a context bounded by Options.OpTimeout, also
// when the caller's context allows an hour.
func TestPrimeAndRefreshIssuesBoundedByOpTimeout(t *testing.T) {
	f := newFake(newFakeClock(), 40*time.Second)
	o := fixtureOptions(3, churnOn)
	s := buildSet(t, f, o)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	if err := s.Prime(ctx, 2); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	// Stop the refresher goroutine, so only the test's own call refreshes.
	s.Close()
	s.refreshDue(ctx, f.clock.Now().Add(31*time.Second))
	end := time.Now()

	issues := f.recorded(kindIssue)
	if len(issues) != 6 {
		t.Fatalf("%d issues, want 3 from Prime and 3 from the refresh", len(issues))
	}
	for i, c := range issues {
		if c.deadline.IsZero() || c.deadline.After(end.Add(o.OpTimeout)) {
			t.Errorf("issue %d ran with deadline %v, want one at most OpTimeout %s after the call", i+1, c.deadline, o.OpTimeout)
		}
	}
}

// TestPrimeCanceled checks that Prime issues nothing on a cancelled context
// and reports the cancellation.
func TestPrimeCanceled(t *testing.T) {
	f := newFake(newFakeClock(), time.Hour)
	s := buildSet(t, f, fixtureOptions(3, churnOn))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Prime(ctx, 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prime error = %v, want context.Canceled", err)
	}
	if !strings.HasPrefix(err.Error(), "priming token pool (0 of 3 issued): ") {
		t.Errorf("error = %q, want the 0 of 3 prefix", err)
	}
	if n := len(f.recorded(kindIssue)); n != 0 {
		t.Errorf("%d issues made on a cancelled context, want 0", n)
	}
}

// TestPrimeTwice checks that a primed pool refuses a second Prime instead of
// starting a second refresher.
func TestPrimeTwice(t *testing.T) {
	s, f := primedSet(t, time.Hour, 3, churnOn)
	if err := s.Prime(context.Background(), 2); err == nil {
		t.Fatal("a second Prime succeeded")
	}
	if n := len(f.recorded(kindIssue)); n != 3 {
		t.Errorf("%d issues made, want the first Prime's 3", n)
	}
}

// TestValidate checks that validate validates a pool token and leaves it in
// the pool.
func TestValidate(t *testing.T) {
	s, f := primedSet(t, time.Hour, 3, churnOn)
	before := poolTokens(s)
	m := f.mark()
	if err := operation(t, s, OpValidate)(context.Background(), rand.New(rand.NewSource(1))); err != nil {
		t.Fatalf("validate: %v", err)
	}
	calls := f.since(m)
	if len(calls) != 1 || calls[0].kind != kindValidate {
		t.Fatalf("calls = %+v, want one validation", calls)
	}
	if _, ok := before[calls[0].token]; !ok {
		t.Errorf("validated %q, not a pool token", calls[0].token)
	}
	if !maps.Equal(poolTokens(s), before) {
		t.Error("validate changed the pool")
	}
}

// TestRevoke covers the revoke operation: a pool token is revoked and a token
// for the same pair is issued in its place; a failed revoke still reissues; a
// failed reissue shrinks the pool and is logged; an empty pool is
// ErrPoolEmpty.
func TestRevoke(t *testing.T) {
	ctx := context.Background()

	t.Run("revokes a pool token and reissues for its pair", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 3, churnOn)
		before := poolTokens(s)
		m := f.mark()
		if err := operation(t, s, OpRevoke)(ctx, rand.New(rand.NewSource(1))); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		calls := f.since(m)
		if len(calls) != 2 || calls[0].kind != kindRevoke || calls[1].kind != kindIssue {
			t.Fatalf("calls = %+v, want a revoke and then an issue", calls)
		}
		revoked := calls[0].token
		pairIdx, ok := before[revoked]
		if !ok {
			t.Fatalf("revoked %q, not a pool token", revoked)
		}
		p := s.pairs[pairIdx].issue
		if calls[1].user != p.User || calls[1].project != p.Project {
			t.Errorf("reissued for (%s, %s), want the revoked token's pair (%s, %s)", calls[1].user, calls[1].project, p.User, p.Project)
		}
		after := poolTokens(s)
		if len(after) != len(before) {
			t.Errorf("pool size = %d, want %d", len(after), len(before))
		}
		if _, ok := after[revoked]; ok {
			t.Error("the revoked token is still in the pool")
		}
		if after["tok-4"] != pairIdx {
			t.Errorf("the new token is for pair %d, want %d", after["tok-4"], pairIdx)
		}
	})

	t.Run("a failed revoke still reissues", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 3, churnOn)
		f.fail(kindRevoke, gophercloud.ErrUnexpectedResponseCode{Actual: 404})
		m := f.mark()
		err := operation(t, s, OpRevoke)(ctx, rand.New(rand.NewSource(1)))
		if got := Classify(err); got != "not-found" {
			t.Errorf("Classify(%v) = %q, want not-found", err, got)
		}
		if calls := f.since(m); len(calls) != 2 || calls[1].kind != kindIssue {
			t.Errorf("calls = %+v, want the revoke and then the reissue", calls)
		}
		if n := s.pool.len(); n != 3 {
			t.Errorf("pool size = %d, want 3", n)
		}
	})

	t.Run("a failed reissue shrinks the pool", func(t *testing.T) {
		logs := captureLogs(t)
		s, f := primedSet(t, time.Hour, 3, churnOn)
		f.fail(kindIssue, gophercloud.ErrUnexpectedResponseCode{Actual: 500})
		err := operation(t, s, OpRevoke)(ctx, rand.New(rand.NewSource(1)))
		var code gophercloud.ErrUnexpectedResponseCode
		if !errors.As(err, &code) || code.Actual != 500 {
			t.Errorf("revoke error = %v, want the reissue's 500", err)
		}
		if n := s.pool.len(); n != 2 {
			t.Errorf("pool size = %d, want 2", n)
		}
		out := logs.String()
		if !strings.Contains(out, "token pool shrank; a reissue failed") || !strings.Contains(out, "size=2") {
			t.Errorf("log = %q, want the shrink warning with size=2", out)
		}
		if strings.Contains(out, "tok-") || strings.Contains(out, "pw-") {
			t.Errorf("log = %q carries a token or a password", out)
		}
	})

	t.Run("a failed revoke and a failed reissue return the revoke's error", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 3, churnOn)
		f.fail(kindRevoke, gophercloud.ErrUnexpectedResponseCode{Actual: 404})
		f.fail(kindIssue, gophercloud.ErrUnexpectedResponseCode{Actual: 500})
		err := operation(t, s, OpRevoke)(ctx, rand.New(rand.NewSource(1)))
		var code gophercloud.ErrUnexpectedResponseCode
		if !errors.As(err, &code) || code.Actual != 404 {
			t.Errorf("revoke error = %v, want the revoke's 404", err)
		}
		if n := s.pool.len(); n != 2 {
			t.Errorf("pool size = %d, want 2", n)
		}
	})

	t.Run("an empty pool is ErrPoolEmpty", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 1, churnOn)
		rng := rand.New(rand.NewSource(1))
		if _, ok := s.pool.take(rng); !ok { // another worker holds the only entry
			t.Fatal("the primed pool is empty")
		}
		m := f.mark()
		err := operation(t, s, OpRevoke)(ctx, rng)
		if !errors.Is(err, ErrPoolEmpty) {
			t.Fatalf("revoke error = %v, want ErrPoolEmpty", err)
		}
		if got := Classify(err); got != "pool-empty" {
			t.Errorf("Classify = %q, want pool-empty", got)
		}
		if calls := f.since(m); len(calls) != 0 {
			t.Errorf("calls = %+v, want none", calls)
		}
	})
}

// TestEmptyPool checks that every operation drawing a pool token returns
// ErrPoolEmpty without a cloud call when the pool is empty.
func TestEmptyPool(t *testing.T) {
	for _, name := range []string{OpValidate, OpEffective, OpRevoke, OpIssue} {
		t.Run(name, func(t *testing.T) {
			s, f := primedSet(t, time.Hour, 1, churnOn)
			rng := rand.New(rand.NewSource(1))
			s.pool.take(rng)
			m := f.mark()
			if err := operation(t, s, name)(context.Background(), rng); !errors.Is(err, ErrPoolEmpty) {
				t.Errorf("%s error = %v, want ErrPoolEmpty", name, err)
			}
			if calls := f.since(m); len(calls) != 0 {
				t.Errorf("calls = %+v, want none", calls)
			}
		})
	}
}

// TestEffective checks that effective lists the effective assignments and the
// projects of a pool token's user and then the projects of the token itself,
// in that order, and that the first failure ends it.
func TestEffective(t *testing.T) {
	ctx := context.Background()

	t.Run("lists in order", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 3, churnOn)
		before := poolTokens(s)
		m := f.mark()
		if err := operation(t, s, OpEffective)(ctx, rand.New(rand.NewSource(1))); err != nil {
			t.Fatalf("effective: %v", err)
		}
		calls := f.since(m)
		if len(calls) != 3 {
			t.Fatalf("calls = %+v, want three listings", calls)
		}
		token := calls[2].token
		pairIdx, ok := before[token]
		if !ok {
			t.Fatalf("listed available projects with %q, not a pool token", token)
		}
		userID := s.pairs[pairIdx].userID
		want := []call{
			{kind: kindEffective, userID: userID},
			{kind: kindUserProjects, userID: userID},
			{kind: kindAvailable, token: token},
		}
		if !reflect.DeepEqual(calls, want) {
			t.Errorf("calls = %+v, want %+v", calls, want)
		}
		if !maps.Equal(poolTokens(s), before) {
			t.Error("effective changed the pool")
		}
	})

	t.Run("stops at the first error", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 3, churnOn)
		f.fail(kindUserProjects, errFake)
		m := f.mark()
		err := operation(t, s, OpEffective)(ctx, rand.New(rand.NewSource(1)))
		if !errors.Is(err, errFake) {
			t.Errorf("effective error = %v, want the user projects listing's", err)
		}
		var kinds []string
		for _, c := range f.since(m) {
			kinds = append(kinds, c.kind)
		}
		if want := []string{kindEffective, kindUserProjects}; !reflect.DeepEqual(kinds, want) {
			t.Errorf("calls = %v, want %v and no available projects listing", kinds, want)
		}
	})
}

// TestGrantChurn checks that grant_churn removes a churn user's grant and
// grants it again with the same arguments, never touches a pool user's grant,
// restores the grant after a failed removal, and skips a busy grant.
func TestGrantChurn(t *testing.T) {
	ctx := context.Background()
	projectGrant := "uid-4:project:pid-2:rid-1"
	domainGrant := "uid-4:domain:did-1:rid-2"
	wantAssign := map[string]call{
		projectGrant: {kind: kindAssign, a: keystoneplan.Assignment{User: "user-0004", Role: "role-0001", Project: "proj-0002"}, userID: "uid-4", projectID: "pid-2", domainID: "did-1", roleID: "rid-1"},
		domainGrant:  {kind: kindAssign, a: keystoneplan.Assignment{User: "user-0004", Role: "role-0002"}, userID: "uid-4", domainID: "did-1", roleID: "rid-2"},
	}

	t.Run("removes and restores churn grants only", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 3, churnOn)
		do := operation(t, s, OpGrantChurn)
		rng := rand.New(rand.NewSource(3))
		m := f.mark()
		const draws = 200
		for range draws {
			if err := do(ctx, rng); err != nil {
				t.Fatalf("grant_churn: %v", err)
			}
		}
		calls := f.since(m)
		if len(calls) != 2*draws {
			t.Fatalf("%d calls, want %d", len(calls), 2*draws)
		}
		hit := make(map[string]int)
		for i := 0; i < len(calls); i += 2 {
			del, assign := calls[i], calls[i+1]
			if del.kind != kindDelete || del.res.Kind != keystone.KindAssignment {
				t.Fatalf("call %d = %+v, want an assignment delete", i, del)
			}
			want, ok := wantAssign[del.res.ID]
			if !ok {
				t.Fatalf("deleted assignment %q, not a churn user's grant", del.res.ID)
			}
			if !reflect.DeepEqual(assign, want) {
				t.Errorf("after deleting %q: call = %+v, want %+v", del.res.ID, assign, want)
			}
			hit[del.res.ID]++
		}
		if hit[projectGrant] == 0 || hit[domainGrant] == 0 {
			t.Errorf("grants churned = %v, want both churn grants", hit)
		}
	})

	t.Run("restores the grant after a failed removal", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 3, churnOn)
		f.fail(kindDelete, errFake)
		m := f.mark()
		err := operation(t, s, OpGrantChurn)(ctx, rand.New(rand.NewSource(1)))
		if !errors.Is(err, errFake) {
			t.Errorf("grant_churn error = %v, want the delete's", err)
		}
		calls := f.since(m)
		if len(calls) != 2 || calls[0].kind != kindDelete || calls[1].kind != kindAssign {
			t.Fatalf("calls = %+v, want the delete and then the grant", calls)
		}
		if !reflect.DeepEqual(calls[1], wantAssign[calls[0].res.ID]) {
			t.Errorf("grant = %+v, want %+v", calls[1], wantAssign[calls[0].res.ID])
		}
	})

	t.Run("a failed removal and a failed grant return the removal's error", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 3, churnOn)
		errDelete, errAssign := errors.New("fake: delete failed"), errors.New("fake: grant failed")
		f.fail(kindDelete, errDelete)
		f.fail(kindAssign, errAssign)
		err := operation(t, s, OpGrantChurn)(ctx, rand.New(rand.NewSource(1)))
		if !errors.Is(err, errDelete) || errors.Is(err, errAssign) {
			t.Errorf("grant_churn error = %v, want the removal's %q", err, errDelete)
		}
	})

	t.Run("returns a failed grant", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 3, churnOn)
		f.fail(kindAssign, errFake)
		if err := operation(t, s, OpGrantChurn)(ctx, rand.New(rand.NewSource(1))); !errors.Is(err, errFake) {
			t.Errorf("grant_churn error = %v, want the grant's", err)
		}
	})

	t.Run("skips a busy grant", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 3, churnOn)
		busy := s.churn[0] // another worker is churning the project grant
		busy.mu.Lock()
		defer busy.mu.Unlock()
		do := operation(t, s, OpGrantChurn)
		rng := rand.New(rand.NewSource(5))
		m := f.mark()
		done := make(chan error, 1)
		go func() {
			for range 20 {
				if err := do(ctx, rng); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("grant_churn: %v", err)
			}
		case <-time.After(10 * time.Second):
			// The deferred unlock releases the blocked operation.
			t.Fatal("grant_churn waited for the busy grant instead of taking the free one")
		}
		for _, c := range f.since(m) {
			if c.kind == kindDelete && c.res.ID != domainGrant {
				t.Fatalf("deleted %q while it was busy, want only %q", c.res.ID, domainGrant)
			}
		}
	})

	t.Run("no churn grants", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 3, churnOff)
		m := f.mark()
		if err := operation(t, s, OpGrantChurn)(ctx, rand.New(rand.NewSource(1))); err == nil {
			t.Error("grant_churn without churn grants succeeded")
		}
		if calls := f.since(m); len(calls) != 0 {
			t.Errorf("calls = %+v, want none", calls)
		}
	})
}

// TestIssue checks that issue replaces a pool token with a new one for the
// same pair without revoking it, and puts the old one back when the issue
// fails.
func TestIssue(t *testing.T) {
	ctx := context.Background()

	t.Run("replaces a token without revoking it", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 3, churnOn)
		before := poolTokens(s)
		m := f.mark()
		if err := operation(t, s, OpIssue)(ctx, rand.New(rand.NewSource(1))); err != nil {
			t.Fatalf("issue: %v", err)
		}
		calls := f.since(m)
		if len(calls) != 1 || calls[0].kind != kindIssue {
			t.Fatalf("calls = %+v, want one issue and no revoke", calls)
		}
		after := poolTokens(s)
		if len(after) != len(before) {
			t.Errorf("pool size = %d, want %d", len(after), len(before))
		}
		newPair, ok := after["tok-4"]
		if !ok {
			t.Fatal("the new token is not in the pool")
		}
		p := s.pairs[newPair].issue
		if calls[0].user != p.User || calls[0].project != p.Project {
			t.Errorf("issued for (%s, %s), want the new entry's pair (%s, %s)", calls[0].user, calls[0].project, p.User, p.Project)
		}
		var dropped []string
		for tok, pairIdx := range before {
			if _, ok := after[tok]; !ok {
				dropped = append(dropped, tok)
				if pairIdx != newPair {
					t.Errorf("dropped %s of pair %d, replaced by a token of pair %d", tok, pairIdx, newPair)
				}
			}
		}
		if len(dropped) != 1 {
			t.Errorf("dropped %v, want exactly one token", dropped)
		}
	})

	t.Run("a failed issue keeps the old token", func(t *testing.T) {
		s, f := primedSet(t, time.Hour, 3, churnOn)
		f.fail(kindIssue, errFake)
		before := poolTokens(s)
		if err := operation(t, s, OpIssue)(ctx, rand.New(rand.NewSource(1))); !errors.Is(err, errFake) {
			t.Errorf("issue error = %v, want the fake's", err)
		}
		if after := poolTokens(s); !maps.Equal(after, before) {
			t.Errorf("pool = %v after a failed issue, want %v", after, before)
		}
	})
}

// TestClassify checks the labels Classify gives.
func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"pool empty", ErrPoolEmpty, "pool-empty"},
		{"pool empty wrapped", errors.Join(errFake, ErrPoolEmpty), "pool-empty"},
		{"404", gophercloud.ErrUnexpectedResponseCode{Actual: 404}, "not-found"},
		{"500", gophercloud.ErrUnexpectedResponseCode{Actual: 500}, "transient"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"canceled", context.Canceled, "canceled"},
		{"other", errFake, "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.want {
				t.Errorf("Classify(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// TestRunWithEngine drives the operations through pressure.Run with four
// workers: with revoke at weight 0 the fake sees no revocation, the pool keeps
// its size, the result has no revoke entry, and the only error kind is the one
// the fake injects.
func TestRunWithEngine(t *testing.T) {
	weights := map[string]int{OpValidate: 60, OpRevoke: 0, OpEffective: 5, OpGrantChurn: 2, OpIssue: 1}
	s, f := primedSet(t, time.Hour, 6, weights)
	f.failNth(kindValidate, 3, gophercloud.ErrUnexpectedResponseCode{Actual: 404})
	f.failNth(kindValidate, 7, gophercloud.ErrUnexpectedResponseCode{Actual: 404})

	res, err := pressure.Run(context.Background(), s.Operations(), pressure.Config{
		Duration:  100 * time.Millisecond,
		Workers:   4,
		Seed:      42,
		OpTimeout: time.Second,
		Classify:  Classify,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := len(f.recorded(kindRevoke)); n != 0 {
		t.Errorf("the fake saw %d revocations at weight 0", n)
	}
	if n := s.pool.len(); n != 6 {
		t.Errorf("pool size = %d after the run, want 6", n)
	}
	var names []string
	for _, st := range res.Operations.ByType {
		names = append(names, st.Type)
	}
	if slices.Contains(names, OpRevoke) {
		t.Errorf("result operations %v include revoke", names)
	}
	if !slices.Contains(names, OpValidate) {
		t.Errorf("result operations %v lack validate", names)
	}
	for _, e := range res.Operations.Errors {
		if e.Kind != "not-found" {
			t.Errorf("error kind %q (%d), want only the injected not-found", e.Kind, e.Count)
		}
	}
	if failed := res.Operations.Overall.Failed; failed > 2 {
		t.Errorf("%d operations failed, want at most the 2 injected", failed)
	}
}
