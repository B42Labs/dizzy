package keystoneops

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/B42Labs/dizzy/internal/resource"
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
			wantErr: "pressure needs at least one project-scoped assignment outside the churn and fan users to issue pool tokens from; the plan has none",
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
			wantErr: "pressure needs at least one project-scoped assignment outside the churn and fan users to issue pool tokens from; the plan has none",
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
			wantErr: "pressure grant_churn needs the churn users to hold at least one assignment; the last 1 regular user(s) of the plan hold none",
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

// TestBuildFanPartition checks the three-way partition Build makes over a
// plan whose last user is a fan user: the churn user comes from the end of the
// regular users, the pool pairs from the regular users outside the churn set,
// and the fan user gets one pair after them, for its first project-scoped
// assignment in plan order, with every plan project and role resolved for
// GrowFan. A plan without fan users resolves no project or role beyond what
// its pairs and churn grants need.
func TestBuildFanPartition(t *testing.T) {
	type pairArgs struct{ user, project string }
	pairsOf := func(s *Set) []pairArgs {
		var out []pairArgs
		for _, p := range s.pairs {
			out = append(out, pairArgs{p.issue.User, p.issue.Project})
		}
		return out
	}
	// fan-0001 holds a domain-scoped grant first and then grants on proj-0004
	// and proj-0002, so its token pair is proj-0004's.
	plan := func() *keystoneplan.Plan { return fanPlan(5, []string{"", "proj-0004", "proj-0002"}) }

	t.Run("grant_churn reserves the last regular user", func(t *testing.T) {
		s := buildFanSet(t, newFake(newFakeClock(), time.Hour), plan(), fixtureOptions(2, churnOn))
		var churned []keystoneplan.Assignment
		for _, g := range s.churn {
			churned = append(churned, g.a)
		}
		if want := []keystoneplan.Assignment{{User: "user-0003", Role: "role-0001", Project: "proj-0003"}}; !reflect.DeepEqual(churned, want) {
			t.Errorf("churn grants = %v, want user-0003's %v", churned, want)
		}
		want := []pairArgs{{"user-0001", "proj-0001"}, {"user-0002", "proj-0002"}, {"fan-0001", "proj-0004"}}
		if got := pairsOf(s); !reflect.DeepEqual(got, want) {
			t.Errorf("pairs = %v, want the two pool pairs and then the fan pair %v", got, want)
		}
		if s.poolPairs != 2 {
			t.Errorf("poolPairs = %d, want 2", s.poolPairs)
		}
		wantPair := pair{
			issue:     keystoneplan.TokenIssue{User: "fan-0001", Project: "proj-0004"},
			userID:    "id-fan-0001",
			domainID:  "did-1",
			password:  "pw-fan-0001",
			projectID: "id-proj-0004",
		}
		if got := s.pairs[len(s.pairs)-1]; got != wantPair {
			t.Errorf("fan pair = %+v, want %+v", got, wantPair)
		}
		if len(s.fan) != 1 {
			t.Fatalf("%d fan users, want 1", len(s.fan))
		}
		u := s.fan[0]
		if u.name != "fan-0001" || u.userID != "id-fan-0001" || u.domainID != "did-1" || u.pair != 2 {
			t.Errorf("fan user = {name %q, userID %q, domainID %q, pair %d}, want {fan-0001, id-fan-0001, did-1, 2}", u.name, u.userID, u.domainID, u.pair)
		}
		if want := map[string]bool{"proj-0002": true, "proj-0004": true}; !maps.Equal(u.held, want) {
			t.Errorf("fan user holds %v, want its project-scoped grants %v", u.held, want)
		}
		wantProjects := []idName{
			{"proj-0001", "id-proj-0001"}, {"proj-0002", "id-proj-0002"}, {"proj-0003", "id-proj-0003"},
			{"proj-0004", "id-proj-0004"}, {"proj-0005", "id-proj-0005"},
		}
		if !reflect.DeepEqual(s.projects, wantProjects) {
			t.Errorf("projects = %v, want %v", s.projects, wantProjects)
		}
		if want := []idName{{"role-0001", "rid-1"}, {"role-0002", "rid-2"}}; !reflect.DeepEqual(s.roles, want) {
			t.Errorf("roles = %v, want %v", s.roles, want)
		}
	})

	t.Run("without grant_churn every regular user holds a pool pair", func(t *testing.T) {
		s := buildFanSet(t, newFake(newFakeClock(), time.Hour), plan(), fixtureOptions(3, churnOff))
		want := []pairArgs{{"user-0001", "proj-0001"}, {"user-0002", "proj-0002"}, {"user-0003", "proj-0003"}, {"fan-0001", "proj-0004"}}
		if got := pairsOf(s); !reflect.DeepEqual(got, want) {
			t.Errorf("pairs = %v, want %v", got, want)
		}
		if s.poolPairs != 3 || len(s.churn) != 0 {
			t.Errorf("poolPairs = %d with %d churn grants, want 3 and none", s.poolPairs, len(s.churn))
		}
	})

	t.Run("a plan without fan users resolves nothing for GrowFan", func(t *testing.T) {
		p := fanPlan(5)
		ids := fanIDs(p)
		delete(ids, "proj-0005") // no grant names it
		b := fixtureBindings()
		delete(b.Roles, "role-0002") // no grant uses it
		s, err := Build(p, newFake(newFakeClock(), time.Hour), b, ids, fanPasswords(p), fixtureOptions(2, churnOn))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(s.fan) != 0 || len(s.projects) != 0 || len(s.roles) != 0 {
			t.Errorf("set has %d fan users, %d projects and %d roles, want none", len(s.fan), len(s.projects), len(s.roles))
		}
		if s.poolPairs != len(s.pairs) {
			t.Errorf("poolPairs = %d of %d pairs, want every pair a pool pair", s.poolPairs, len(s.pairs))
		}
	})
}

// TestBuildFanErrors covers the Build errors of a plan with fan users, with
// their exact messages: the churn and pool checks count the regular users
// only, a fan user needs a project-scoped grant to scope its token to, and the
// ids of every project and role and a fan user's password must resolve. Build
// makes no cloud call on any of them.
func TestBuildFanErrors(t *testing.T) {
	const noPoolPair = "pressure needs at least one project-scoped assignment outside the churn and fan users to issue pool tokens from; the plan has none"
	tests := []struct {
		name      string
		plan      func() *keystoneplan.Plan
		opts      Options
		ids       func(map[string]string)
		passwords func(map[string]string)
		roles     func(map[string]string)
		wantErr   string
	}{
		{
			// The plan has four users, but only its three regular users can be
			// churn users.
			name:    "fan users are never churn users",
			plan:    func() *keystoneplan.Plan { return fanPlan(5, []string{"proj-0004"}) },
			opts:    Options{Tokens: 1, ChurnUsers: 3, Weights: churnOn, OpTimeout: time.Second},
			wantErr: "pressure.churn_users (3) must leave at least one user outside the churn set; the plan has 3 users",
		},
		{
			// user-0001 and user-0002 hold domain-scoped grants only, so the
			// only regular user with a project-scoped grant is the churn user.
			name: "only the churn and fan users hold project-scoped grants",
			plan: func() *keystoneplan.Plan {
				p := fanPlan(5, []string{"proj-0004"})
				p.Assignments[0].Project, p.Assignments[1].Project = "", ""
				return p
			},
			opts:    fixtureOptions(1, churnOn),
			wantErr: noPoolPair,
		},
		{
			name: "only the fan users hold project-scoped grants",
			plan: func() *keystoneplan.Plan {
				p := fanPlan(5, []string{"proj-0004"})
				for i := range 3 {
					p.Assignments[i].Project = ""
				}
				return p
			},
			opts:    fixtureOptions(1, churnOff),
			wantErr: noPoolPair,
		},
		{
			// The churn user is user-0003, the last regular user, not
			// fan-0001, the last user of the plan.
			name: "the last regular user holds no grant",
			plan: func() *keystoneplan.Plan {
				p := fanPlan(5, []string{"proj-0004"})
				p.Assignments = slices.Delete(p.Assignments, 2, 3)
				return p
			},
			opts:    fixtureOptions(1, churnOn),
			wantErr: "pressure grant_churn needs the churn users to hold at least one assignment; the last 1 regular user(s) of the plan hold none",
		},
		{
			name:    "a fan user with a domain-scoped grant only",
			plan:    func() *keystoneplan.Plan { return fanPlan(5, []string{""}) },
			opts:    fixtureOptions(1, churnOn),
			wantErr: `pressure fan user "fan-0001" holds no project-scoped assignment to scope its token to`,
		},
		{
			name:    "a fan user without a grant",
			plan:    func() *keystoneplan.Plan { return fanPlan(5, []string{"proj-0001"}, nil) },
			opts:    fixtureOptions(1, churnOn),
			wantErr: `pressure fan user "fan-0002" holds no project-scoped assignment to scope its token to`,
		},
		{
			name:    "missing id of a project no grant names",
			plan:    func() *keystoneplan.Plan { return fanPlan(5, []string{"proj-0004"}) },
			opts:    fixtureOptions(1, churnOn),
			ids:     func(m map[string]string) { delete(m, "proj-0005") },
			wantErr: `pressure set: no cloud id for project "proj-0005"`,
		},
		{
			name:    "missing id of a role no grant names",
			plan:    func() *keystoneplan.Plan { return fanPlan(5, []string{"proj-0004"}) },
			opts:    fixtureOptions(1, churnOn),
			roles:   func(m map[string]string) { delete(m, "role-0002") },
			wantErr: `pressure set: no cloud id for role "role-0002"`,
		},
		{
			name:      "missing password of a fan user",
			plan:      func() *keystoneplan.Plan { return fanPlan(5, []string{"proj-0004"}) },
			opts:      fixtureOptions(1, churnOn),
			passwords: func(m map[string]string) { delete(m, "fan-0001") },
			wantErr:   `pressure set: no password for user "fan-0001"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.plan()
			ids, passwords, b := fanIDs(p), fanPasswords(p), fixtureBindings()
			if tt.ids != nil {
				tt.ids(ids)
			}
			if tt.passwords != nil {
				tt.passwords(passwords)
			}
			if tt.roles != nil {
				tt.roles(b.Roles)
			}
			f := newFake(newFakeClock(), time.Hour)
			s, err := Build(p, f, b, ids, passwords, tt.opts)
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

// TestPrimeFanTokens checks that Prime issues the pool tokens and then one
// token per fan user: with Tokens 4 over the two pool pairs and two fan users
// it makes six issues, the round robin of the first four never lands on a fan
// pair, and the last two are the fan users' in fan order, each with its own
// password and project, and fill the fan pool. A failing fifth issue counts
// the tokens of both pools in its message and fills neither pool.
func TestPrimeFanTokens(t *testing.T) {
	plan := func() *keystoneplan.Plan {
		return fanPlan(5, []string{"proj-0004", "proj-0002"}, []string{"proj-0005"})
	}

	t.Run("fills both pools", func(t *testing.T) {
		s, f := primedFanSet(t, plan(), fixtureOptions(4, churnOn))
		type issueArgs struct{ user, project, password, projectID string }
		var got []issueArgs
		for _, c := range f.recorded(kindIssue) {
			got = append(got, issueArgs{c.user, c.project, c.password, c.projectID})
		}
		want := []issueArgs{
			{"user-0001", "proj-0001", "pw-user-0001", "id-proj-0001"},
			{"user-0002", "proj-0002", "pw-user-0002", "id-proj-0002"},
			{"user-0001", "proj-0001", "pw-user-0001", "id-proj-0001"},
			{"user-0002", "proj-0002", "pw-user-0002", "id-proj-0002"},
			{"fan-0001", "proj-0004", "pw-fan-0001", "id-proj-0004"},
			{"fan-0002", "proj-0005", "pw-fan-0002", "id-proj-0005"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("issues = %v, want %v", got, want)
		}
		var poolPairs, fanPairs []int
		for _, e := range poolEntries(s) {
			poolPairs = append(poolPairs, e.pair)
		}
		for _, e := range fanEntries(s) {
			fanPairs = append(fanPairs, e.pair)
		}
		if want := []int{0, 1, 0, 1}; !reflect.DeepEqual(poolPairs, want) {
			t.Errorf("pool entries are for pairs %v, want %v", poolPairs, want)
		}
		if want := []int{2, 3}; !reflect.DeepEqual(fanPairs, want) || s.fan[0].pair != 2 || s.fan[1].pair != 3 {
			t.Errorf("fan pool entries are for pairs %v and the fan users' pairs are %d and %d, want both %v", fanPairs, s.fan[0].pair, s.fan[1].pair, want)
		}
	})

	t.Run("a failing fifth issue", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		f.failNth(kindIssue, 5, gophercloud.ErrUnexpectedResponseCode{Actual: 500})
		s := buildFanSet(t, f, plan(), fixtureOptions(4, churnOn))
		err := s.Prime(context.Background(), 1)
		if want := "priming token pool (4 of 6 issued): "; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("Prime error = %v, want the prefix %q", err, want)
		}
		var code gophercloud.ErrUnexpectedResponseCode
		if !errors.As(err, &code) || code.Actual != 500 {
			t.Errorf("error %v does not wrap the 500", err)
		}
		if n := len(f.recorded(kindIssue)); n != 5 {
			t.Errorf("%d issues made, want 5 (none after the failure)", n)
		}
		if s.pool.len() != 0 || s.fanPool.len() != 0 {
			t.Errorf("a failed Prime left %d pool and %d fan pool entries, want none", s.pool.len(), s.fanPool.len())
		}
		if s.done != nil {
			t.Error("a failed Prime started the refresher")
		}
	})
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

// TestEffectiveFanUser checks that effective lists a fan user when the set has
// one: the assignment and user project listings carry the fan user's id and
// the available projects listing its fan token, and neither pool changes.
// With two fan users the draws reach both, each listing with its own token;
// without fan users effective lists the user of a pool token as before.
func TestEffectiveFanUser(t *testing.T) {
	ctx := context.Background()

	t.Run("lists the fan user", func(t *testing.T) {
		s, f := primedFanSet(t, fanPlan(5, []string{"proj-0002", "proj-0004"}), fixtureOptions(2, churnOn))
		poolBefore, fanBefore := poolTokens(s), fanTokens(s)
		fanToken := fanEntries(s)[0].token
		m := f.mark()
		if err := operation(t, s, OpEffective)(ctx, rand.New(rand.NewSource(1))); err != nil {
			t.Fatalf("effective: %v", err)
		}
		want := []call{
			{kind: kindEffective, userID: "id-fan-0001"},
			{kind: kindUserProjects, userID: "id-fan-0001"},
			{kind: kindAvailable, token: fanToken},
		}
		if calls := f.since(m); !reflect.DeepEqual(calls, want) {
			t.Errorf("calls = %+v, want %+v", calls, want)
		}
		if !maps.Equal(poolTokens(s), poolBefore) || !maps.Equal(fanTokens(s), fanBefore) {
			t.Error("effective changed a pool")
		}
	})

	t.Run("draws every fan user", func(t *testing.T) {
		s, f := primedFanSet(t, fanPlan(5, []string{"proj-0002"}, []string{"proj-0004"}), fixtureOptions(2, churnOn))
		userOf := make(map[string]string) // fan token to its fan user's id
		for _, e := range fanEntries(s) {
			userOf[e.token] = s.pairs[e.pair].userID
		}
		do := operation(t, s, OpEffective)
		rng := rand.New(rand.NewSource(1))
		listed := make(map[string]int)
		for range 50 {
			m := f.mark()
			if err := do(ctx, rng); err != nil {
				t.Fatalf("effective: %v", err)
			}
			calls := f.since(m)
			userID, ok := userOf[calls[2].token]
			if !ok || calls[0].userID != userID || calls[1].userID != userID {
				t.Fatalf("calls = %+v, want the listings of one fan user with its token", calls)
			}
			listed[userID]++
		}
		if listed["id-fan-0001"] == 0 || listed["id-fan-0002"] == 0 {
			t.Errorf("fan users listed = %v, want both", listed)
		}
	})

	t.Run("without fan users lists a pool user", func(t *testing.T) {
		s, f := primedFanSet(t, fanPlan(5), fixtureOptions(2, churnOn))
		before := poolTokens(s)
		m := f.mark()
		if err := operation(t, s, OpEffective)(ctx, rand.New(rand.NewSource(1))); err != nil {
			t.Fatalf("effective: %v", err)
		}
		calls := f.since(m)
		if len(calls) != 3 {
			t.Fatalf("calls = %+v, want three listings", calls)
		}
		pairIdx, ok := before[calls[2].token]
		if !ok {
			t.Fatalf("listed available projects with %q, not a pool token", calls[2].token)
		}
		if userID := s.pairs[pairIdx].userID; calls[0].userID != userID || calls[1].userID != userID {
			t.Errorf("calls = %+v, want the listings of the pool token's user %q", calls, userID)
		}
	})
}

// TestPoolOperationsSkipFanUsers checks that validate, revoke, grant_churn and
// issue never touch a fan user: over many draws of each, no call carries a
// fan user's name, id or token, and the fan pool keeps its entries.
func TestPoolOperationsSkipFanUsers(t *testing.T) {
	ctx := context.Background()
	s, f := primedFanSet(t, fanPlan(5, []string{"proj-0002", "proj-0004"}), fixtureOptions(2, churnOn))
	fanBefore := fanTokens(s)
	m := f.mark()
	rng := rand.New(rand.NewSource(1))
	for _, name := range []string{OpValidate, OpRevoke, OpGrantChurn, OpIssue} {
		do := operation(t, s, name)
		for range 50 {
			if err := do(ctx, rng); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	}
	for _, c := range f.since(m) {
		_, fanToken := fanBefore[c.token]
		if fanToken || c.user == "fan-0001" || c.a.User == "fan-0001" || c.userID == "id-fan-0001" || strings.Contains(c.res.ID, "id-fan-0001") {
			t.Errorf("call %+v touches the fan user", c)
		}
	}
	if !maps.Equal(fanTokens(s), fanBefore) {
		t.Errorf("fan pool = %v after the pool operations, want %v", fanTokens(s), fanBefore)
	}
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

// TestGrowFan covers GrowFan over the five projects and two roles of a
// fanPlan: the grants a user lacks go to the first projects it holds none
// on, in plan order, with the roles cycled from its count of grants; a second
// call with the same target grants nothing; a target above the projects fails
// without a call; a failed grant stops further grants, keeps what was created
// and leaves the failed project for a later call; a transient failure is
// retried; and the grants in flight never exceed the concurrency.
func TestGrowFan(t *testing.T) {
	ctx := context.Background()
	// fanGrantCall is the AssignRole call GrowFan makes to grant role on
	// project to the fan user user, its deadline left out.
	fanGrantCall := func(user, role, project string) call {
		roleID := map[string]string{"role-0001": "rid-1", "role-0002": "rid-2"}[role]
		return call{
			kind:      kindAssign,
			a:         keystoneplan.Assignment{User: user, Role: role, Project: project},
			userID:    "id-" + user,
			projectID: "id-" + project,
			domainID:  "did-1",
			roleID:    roleID,
		}
	}
	// grantCalls returns the fake's grants since mark m in call order, without
	// their deadlines.
	grantCalls := func(f *fakeKeystone, m int) []call {
		var out []call
		for _, c := range f.since(m) {
			if c.kind == kindAssign {
				c.deadline = time.Time{}
				out = append(out, c)
			}
		}
		return out
	}
	resourceOf := func(c call) resource.Resource {
		return keystone.AssignmentResource(c.a, c.userID, c.projectID, c.domainID, c.roleID)
	}
	byID := func(a, b resource.Resource) int { return strings.Compare(a.ID, b.ID) }
	byProject := func(a, b call) int { return strings.Compare(a.a.Project, b.a.Project) }
	// twoHeld is the set whose fan user holds proj-0002 and proj-0004.
	twoHeld := func(t *testing.T, f *fakeKeystone) *Set {
		return buildFanSet(t, f, fanPlan(5, []string{"proj-0002", "proj-0004"}), fixtureOptions(1, churnOn))
	}

	t.Run("grants the first missing projects with cycled roles", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		s := twoHeld(t, f)
		res, err := s.GrowFan(ctx, 4, 2)
		end := time.Now()
		if err != nil {
			t.Fatalf("GrowFan(4, 2): %v", err)
		}
		// h is 2, so proj-0001 gets roles[(2+0) % 2] and proj-0003 roles[(2+1) % 2].
		want := []call{
			fanGrantCall("fan-0001", "role-0001", "proj-0001"),
			fanGrantCall("fan-0001", "role-0002", "proj-0003"),
		}
		for _, c := range f.recorded(kindAssign) {
			if c.deadline.IsZero() || c.deadline.After(end.Add(s.o.OpTimeout)) {
				t.Errorf("grant %+v ran with deadline %v, want one at most OpTimeout %s after the call", c.a, c.deadline, s.o.OpTimeout)
			}
		}
		got := grantCalls(f, 0)
		slices.SortFunc(got, byProject) // two grants in flight finish in either order
		if !reflect.DeepEqual(got, want) {
			t.Errorf("grants = %+v, want %+v", got, want)
		}
		wantRes := []resource.Resource{resourceOf(want[0]), resourceOf(want[1])}
		slices.SortFunc(res, byID)
		slices.SortFunc(wantRes, byID)
		if !reflect.DeepEqual(res, wantRes) {
			t.Errorf("created = %+v, want the role_assignment resources %+v", res, wantRes)
		}
		for _, r := range res {
			if r.Kind != keystone.KindAssignment {
				t.Errorf("created %+v of kind %q, want %q", r, r.Kind, keystone.KindAssignment)
			}
		}
		wantHeld := map[string]bool{"proj-0001": true, "proj-0002": true, "proj-0003": true, "proj-0004": true}
		if !maps.Equal(s.fan[0].held, wantHeld) {
			t.Errorf("fan user holds %v, want %v", s.fan[0].held, wantHeld)
		}

		m := f.mark()
		res, err = s.GrowFan(ctx, 4, 2)
		if err != nil || len(res) != 0 {
			t.Errorf("a second GrowFan(4, 2) = %v, %v; want no resources and no error", res, err)
		}
		if calls := f.since(m); len(calls) != 0 {
			t.Errorf("a second GrowFan(4, 2) made calls %+v, want none", calls)
		}

		// h is 4 now, so proj-0005 gets roles[(4+0) % 2].
		m = f.mark()
		if _, err := s.GrowFan(ctx, 5, 1); err != nil {
			t.Fatalf("GrowFan(5, 1): %v", err)
		}
		if got, want := grantCalls(f, m), []call{fanGrantCall("fan-0001", "role-0001", "proj-0005")}; !reflect.DeepEqual(got, want) {
			t.Errorf("grants = %+v, want %+v", got, want)
		}
	})

	t.Run("grows every fan user", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		s := buildFanSet(t, f, fanPlan(5, []string{"proj-0002", "proj-0004"}, []string{"proj-0001"}), fixtureOptions(1, churnOn))
		res, err := s.GrowFan(ctx, 3, 1)
		if err != nil {
			t.Fatalf("GrowFan(3, 1): %v", err)
		}
		// fan-0001 holds 2 and fan-0002 holds 1; with one grant in flight
		// they run in fan and plan order.
		want := []call{
			fanGrantCall("fan-0001", "role-0001", "proj-0001"),
			fanGrantCall("fan-0002", "role-0002", "proj-0002"),
			fanGrantCall("fan-0002", "role-0001", "proj-0003"),
		}
		got := grantCalls(f, 0)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("grants = %+v, want %+v", got, want)
		}
		if wantRes := []resource.Resource{resourceOf(want[0]), resourceOf(want[1]), resourceOf(want[2])}; !reflect.DeepEqual(res, wantRes) {
			t.Errorf("created = %+v, want %+v in completion order", res, wantRes)
		}
	})

	t.Run("a target above the projects", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		s := twoHeld(t, f)
		res, err := s.GrowFan(ctx, 6, 1)
		if want := "fan assignments (6) exceed the plan's 5 projects"; err == nil || err.Error() != want {
			t.Errorf("GrowFan(6, 1) error = %v, want %q", err, want)
		}
		if len(res) != 0 {
			t.Errorf("GrowFan(6, 1) created %+v, want nothing", res)
		}
		if calls := f.recorded(); len(calls) != 0 {
			t.Errorf("GrowFan(6, 1) made calls %+v, want none", calls)
		}
	})

	t.Run("a failed grant", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		f.failNth(kindAssign, 2, errFake)
		s := twoHeld(t, f)
		res, err := s.GrowFan(ctx, 4, 1)
		if want := `growing fan user "fan-0001" to 4 assignments (1 granted): `; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("GrowFan(4, 1) error = %v, want the prefix %q", err, want)
		}
		if !errors.Is(err, errFake) {
			t.Errorf("error %v does not wrap the grant's", err)
		}
		first := fanGrantCall("fan-0001", "role-0001", "proj-0001")
		if want := []resource.Resource{resourceOf(first)}; !reflect.DeepEqual(res, want) {
			t.Errorf("created = %+v, want the grant made before the failure %+v", res, want)
		}
		if s.fan[0].held["proj-0003"] || !s.fan[0].held["proj-0001"] {
			t.Errorf("fan user holds %v, want proj-0001 and not the failed proj-0003", s.fan[0].held)
		}

		// A later call makes the failed grant, with the role of the user's
		// third grant, roles[(3+0) % 2].
		m := f.mark()
		res, err = s.GrowFan(ctx, 4, 1)
		if err != nil {
			t.Fatalf("GrowFan(4, 1) after the failure: %v", err)
		}
		want := fanGrantCall("fan-0001", "role-0002", "proj-0003")
		if got := grantCalls(f, m); !reflect.DeepEqual(got, []call{want}) {
			t.Errorf("grants = %+v, want %+v", got, []call{want})
		}
		if !reflect.DeepEqual(res, []resource.Resource{resourceOf(want)}) {
			t.Errorf("created = %+v, want %+v", res, resourceOf(want))
		}
	})

	t.Run("a failure stops further grants", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		f.failNth(kindAssign, 1, errFake)
		s := twoHeld(t, f)
		res, err := s.GrowFan(ctx, 5, 1)
		if want := `growing fan user "fan-0001" to 5 assignments (0 granted): `; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("GrowFan(5, 1) error = %v, want the prefix %q", err, want)
		}
		if len(res) != 0 {
			t.Errorf("created = %+v, want nothing", res)
		}
		if n := len(f.recorded(kindAssign)); n != 1 {
			t.Errorf("%d grants made, want 1 (none after the failure)", n)
		}
	})

	t.Run("a cancelled context grants nothing", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		s := twoHeld(t, f)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		res, err := s.GrowFan(cctx, 4, 2)
		if want := `growing fan user "fan-0001" to 4 assignments (0 granted): context canceled`; err == nil || err.Error() != want {
			t.Errorf("GrowFan error = %v, want %q", err, want)
		}
		if !errors.Is(err, context.Canceled) || len(res) != 0 {
			t.Errorf("GrowFan = %+v, %v; want nothing and context.Canceled", res, err)
		}
		if calls := f.recorded(); len(calls) != 0 {
			t.Errorf("GrowFan on a cancelled context made calls %+v, want none", calls)
		}
	})

	t.Run("a transient failure is retried", func(t *testing.T) {
		// In the synctest bubble WithRetry's backoff passes on the bubble's
		// clock, so the retry costs no wall time.
		synctest.Test(t, func(t *testing.T) {
			f := newFake(newFakeClock(), time.Hour)
			f.failNth(kindAssign, 1, gophercloud.ErrUnexpectedResponseCode{Actual: 503})
			s := twoHeld(t, f)
			res, err := s.GrowFan(ctx, 4, 1)
			if err != nil {
				t.Fatalf("GrowFan(4, 1): %v", err)
			}
			first := fanGrantCall("fan-0001", "role-0001", "proj-0001")
			want := []call{first, first, fanGrantCall("fan-0001", "role-0002", "proj-0003")}
			if got := grantCalls(f, 0); !reflect.DeepEqual(got, want) {
				t.Errorf("grants = %+v, want the first one retried %+v", got, want)
			}
			if len(res) != 2 {
				t.Errorf("created %d resources, want 2", len(res))
			}
		})
	})

	t.Run("a failure waits for the grants in flight", func(t *testing.T) {
		// With concurrency 2 one grant is held at the fake's gate while the
		// other fails. In the synctest bubble, synctest.Wait returns once
		// every goroutine is blocked, so a GrowFan that does not wait for the
		// held grant has returned by then.
		synctest.Test(t, func(t *testing.T) {
			f := newFake(newFakeClock(), time.Hour)
			f.failNth(kindAssign, 2, errFake)
			f.gateAssigns(t)
			s := twoHeld(t, f)
			type result struct {
				res []resource.Resource
				err error
			}
			grown := make(chan result, 1)
			go func() {
				res, err := s.GrowFan(ctx, 5, 2)
				grown <- result{res, err}
			}()

			synctest.Wait()
			select {
			case r := <-grown:
				t.Fatalf("GrowFan returned (%v) while a grant was still in flight", r.err)
			default:
			}
			f.releaseAssign(t)
			synctest.Wait()
			var r result
			select {
			case r = <-grown:
			default:
				t.Fatal("GrowFan did not return after the held grant was let through")
			}
			if want := `growing fan user "fan-0001" to 5 assignments (1 granted): `; r.err == nil || !strings.HasPrefix(r.err.Error(), want) {
				t.Errorf("GrowFan error = %v, want the prefix %q", r.err, want)
			}
			if len(r.res) != 1 {
				t.Errorf("created %d resources, want the held grant's 1", len(r.res))
			}
			if n := len(f.recorded(kindAssign)); n != 2 {
				t.Errorf("%d grants made, want 2 (the third never started)", n)
			}
		})
	})

	// Two fan users over ten projects need nine grants each. The fake holds
	// every grant at its gate and the test lets them through one at a time. In
	// the synctest bubble, synctest.Wait returns once every goroutine is
	// blocked, so GrowFan has started every grant it can start before the test
	// counts them: an unbounded GrowFan has all eighteen in flight, and one
	// without the floor of 1 none.
	for _, tt := range []struct{ concurrency, want int }{{2, 2}, {0, 1}} {
		t.Run(fmt.Sprintf("concurrency %d", tt.concurrency), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const grants = 18
				f := newFake(newFakeClock(), time.Hour)
				f.gateAssigns(t)
				s := buildFanSet(t, f, fanPlan(10, []string{"proj-0001"}, []string{"proj-0002"}), fixtureOptions(1, churnOn))
				type result struct {
					res []resource.Resource
					err error
				}
				grown := make(chan result, 1)
				go func() {
					res, err := s.GrowFan(ctx, 10, tt.concurrency)
					grown <- result{res, err}
				}()

				for released := range grants {
					synctest.Wait()
					if n, want := f.assignsInFlight(), min(tt.want, grants-released); n != want {
						t.Fatalf("%d grants in flight with %d of %d let through, want %d", n, released, grants, want)
					}
					f.releaseAssign(t)
				}
				synctest.Wait()
				var r result
				select {
				case r = <-grown:
				default:
					t.Fatal("GrowFan did not return after every grant was let through")
				}
				if r.err != nil || len(r.res) != grants {
					t.Errorf("GrowFan(10, %d) = %d resources, %v; want %d and no error", tt.concurrency, len(r.res), r.err, grants)
				}
				if n := f.maxAssignsInFlight(); n != tt.want {
					t.Errorf("at most %d grants were in flight at once, want %d", n, tt.want)
				}
				for _, u := range s.fan {
					if len(u.held) != 10 {
						t.Errorf("fan user %q holds %d projects, want 10", u.name, len(u.held))
					}
				}
			})
		})
	}
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
