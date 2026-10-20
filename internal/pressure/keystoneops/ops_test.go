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
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/pressure"
	"github.com/B42Labs/dizzy/internal/pressure/probe"
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
			s, err := Build(tt.plan(), f, nil, b, ids, passwords, tt.opts)
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
		s, err := Build(p, newFake(newFakeClock(), time.Hour), nil, b, ids, fanPasswords(p), fixtureOptions(2, churnOn))
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
			s, err := Build(p, f, nil, b, ids, passwords, tt.opts)
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
	s, err := Build(pressurePlan(), f, nil, fixtureBindings(), fixtureIDs(), fixturePasswords(), Options{Tokens: 1, Weights: weights, OpTimeout: time.Second})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	weights[OpValidate] = 1
	if got := s.Operations()[0].Weight; got != 60 {
		t.Errorf("validate weight = %d after the caller changed its map, want 60", got)
	}
}

// defaultWeights is the command's default mix: the five Keystone operations
// at their weights and probe and bad_token at 0.
var defaultWeights = map[string]int{OpValidate: 60, OpEffective: 5, OpRevoke: 2, OpGrantChurn: 2, OpIssue: 1, OpProbe: 0, OpBadToken: 0}

// TestBuildNeedsServices checks the Build errors of a run that sends tokens
// to the other services, with their exact messages: a positive probe or
// bad_token weight and the propagation probe each need a service client, and
// the propagation probe needs a positive timeout, which Build checks first,
// with a client or without. A run that needs no service, the default mix
// with the propagation probe off, builds without a client. Build makes no
// call on any of them.
func TestBuildNeedsServices(t *testing.T) {
	const noClient = "pressure probe, bad_token and the propagation probe need a service client; none was given"
	options := func(weights map[string]int, p Propagation) Options {
		o := fixtureOptions(1, defaultWeights)
		maps.Copy(o.Weights, weights)
		o.Propagation = p
		return o
	}
	on := Propagation{Every: 30 * time.Second, Timeout: 10 * time.Minute}
	tests := []struct {
		name    string
		svc     *fakeServices // nil passes no client at all
		opts    Options
		wantErr string // empty when Build succeeds
	}{
		{"probe without a client", nil, options(map[string]int{OpProbe: 1}, Propagation{}), noClient},
		{"bad_token without a client", nil, options(map[string]int{OpBadToken: 1}, Propagation{}), noClient},
		{"the propagation probe without a client", nil, options(nil, on), noClient},
		{"the defaults without a client", nil, options(nil, Propagation{}), ""},
		{"probe, bad_token and the propagation probe with a client", newFakeServices(), options(map[string]int{OpProbe: 1, OpBadToken: 1}, on), ""},
		{"a zero propagation timeout without a client", nil, options(nil, Propagation{Every: 30 * time.Second}), "pressure propagation timeout must be positive, got 0s"},
		{"a zero propagation timeout with a client", newFakeServices(), options(nil, Propagation{Every: 30 * time.Second}), "pressure propagation timeout must be positive, got 0s"},
		{"a negative propagation timeout", newFakeServices(), options(nil, Propagation{Every: 30 * time.Second, Timeout: -time.Second}), "pressure propagation timeout must be positive, got -1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var svc Services // a nil *fakeServices would be a non-nil Services
			if tt.svc != nil {
				svc = tt.svc
			}
			f := newFake(newFakeClock(), time.Hour)
			s, err := Build(pressurePlan(), f, svc, fixtureBindings(), fixtureIDs(), fixturePasswords(), tt.opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				s.Close()
			} else {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("Build error = %v, want %q", err, tt.wantErr)
				}
				if s != nil {
					t.Errorf("Build returned a set with its error")
				}
			}
			if calls := f.recorded(); len(calls) != 0 {
				t.Errorf("Build made %d cloud calls, want none", len(calls))
			}
			if tt.svc != nil {
				if calls := tt.svc.recorded(); len(calls) != 0 {
					t.Errorf("Build made %d services calls, want none", len(calls))
				}
			}
		})
	}
}

// TestOperationsNamesAndWeights checks that Operations returns all seven
// operations in order, the order OperationNames lists, with probe and
// bad_token after the five Keystone operations, each with its weight from
// Options, and 0 for a missing weight.
func TestOperationsNamesAndWeights(t *testing.T) {
	f := newFake(newFakeClock(), time.Hour)
	s := buildSet(t, f, fixtureOptions(1, map[string]int{OpValidate: 60, OpRevoke: 2, OpIssue: 1, OpProbe: 4}))
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
	wantNames := []string{"validate", "effective", "revoke", "grant_churn", "issue", "probe", "bad_token"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("names = %v, want %v", names, wantNames)
	}
	if !reflect.DeepEqual(OperationNames, wantNames) {
		t.Errorf("OperationNames = %v, want %v", OperationNames, wantNames)
	}
	wantWeights := map[string]int{"validate": 60, "effective": 0, "revoke": 2, "grant_churn": 0, "issue": 1, "probe": 4, "bad_token": 0}
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

// TestPrimeServiceCheck checks that a run that sends tokens to the other
// services takes its services from the first pool token's catalog. A first
// token whose catalog lists none of the four fails Prime with its exact
// message after the issues and before any entry enters a pool, also when the
// later tokens list one, and starts neither the refresher nor the propagation
// probe. A catalog listing glance and nova gives the services in
// probe.Services order and starts both. A run that needs no service primes
// over tokens without endpoints and has no services.
func TestPrimeServiceCheck(t *testing.T) {
	const noService = "pressure probe needs at least one of nova, neutron, cinder, glance in the pool users' catalog; the first pool token's catalog lists none"
	plan := func() *keystoneplan.Plan { return fanPlan(5, []string{"proj-0004"}) }
	needs := func() Options {
		o := fixtureOptions(2, map[string]int{OpValidate: 1, OpProbe: 1})
		o.Propagation = Propagation{Every: time.Hour, Timeout: time.Minute} // never ticks during the test
		return o
	}

	t.Run("a first token without a service", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		f.setEndpointsFrom(2, fixtureEndpoints("nova")) // the second token on lists nova
		s := buildFanSet(t, f, plan(), needs())
		err := s.Prime(context.Background(), 1) // one issue at a time, so the first token is pool token 0
		if err == nil || err.Error() != noService {
			t.Fatalf("Prime error = %v, want %q", err, noService)
		}
		if n := len(f.recorded(kindIssue)); n != 3 {
			t.Errorf("%d issues made, want the 2 pool tokens and the fan user's", n)
		}
		if s.pool.len() != 0 || s.fanPool.len() != 0 {
			t.Errorf("a failed Prime left %d pool and %d fan pool entries, want none", s.pool.len(), s.fanPool.len())
		}
		if s.done != nil || s.propDone != nil {
			t.Error("a failed Prime started the refresher or the propagation probe")
		}
		if got := s.Services(); got != nil {
			t.Errorf("Services() = %v after a failed Prime, want nil", got)
		}
		s.Close() // nothing runs, so it returns at once
	})

	t.Run("a catalog listing glance and nova", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		f.setEndpoints(fixtureEndpoints("glance", "nova"))
		s := buildFanSet(t, f, plan(), needs())
		if got := s.Services(); got != nil {
			t.Errorf("Services() = %v before Prime, want nil", got)
		}
		if err := s.Prime(context.Background(), 2); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		got := s.Services()
		if want := []string{"nova", "glance"}; !reflect.DeepEqual(got, want) {
			t.Errorf("Services() = %v, want %v", got, want)
		}
		got[0] = "changed"
		if s.Services()[0] != "nova" {
			t.Error("a change to the returned slice changed the set's services")
		}
		if s.pool.len() != 2 || s.fanPool.len() != 1 {
			t.Errorf("pools hold %d and %d entries, want 2 and 1", s.pool.len(), s.fanPool.len())
		}
		if s.done == nil || s.propDone == nil {
			t.Error("Prime did not start the refresher and the propagation probe")
		}
	})

	t.Run("a run without a need", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		s := buildFanSet(t, f, plan(), fixtureOptions(2, churnOff))
		if err := s.Prime(context.Background(), 2); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		if got := s.Services(); got != nil {
			t.Errorf("Services() = %v for a run that needs none, want nil", got)
		}
		if s.pool.len() != 2 || s.propDone != nil {
			t.Errorf("pool holds %d entries with the propagation probe started %v, want 2 and not started", s.pool.len(), s.propDone != nil)
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

// probeWeights is a mix in which probe and bad_token draw.
var probeWeights = map[string]int{OpValidate: 1, OpProbe: 1, OpBadToken: 1}

// TestProbe checks the probe operation over tokens whose catalog lists nova
// and cinder: it sends the drawn pool token to one of the two at that
// service's endpoint and leaves the token in the pool, it draws among the
// listed services only, and its sample lands in the set's probe samples under
// the service, a failing probe as a failure with its Classify label.
func TestProbe(t *testing.T) {
	ctx := context.Background()
	eps := fixtureEndpoints("nova", "cinder")

	t.Run("sends the drawn token to a listed service", func(t *testing.T) {
		s, _, fs := primedServiceSet(t, fixtureOptions(3, probeWeights), eps, 0)
		before := poolTokens(s)
		// The operation's first draw picks its token, so a peek with the same
		// seed names it.
		drawn, _ := s.pool.peek(rand.New(rand.NewSource(1)))
		if err := operation(t, s, OpProbe)(ctx, rand.New(rand.NewSource(1))); err != nil {
			t.Fatalf("probe: %v", err)
		}
		calls := fs.recorded()
		if len(calls) != 1 || calls[0].kind != kindProbe {
			t.Fatalf("services calls = %+v, want one probe", calls)
		}
		c := calls[0]
		if c.service != "nova" && c.service != "cinder" {
			t.Errorf("probed %q, want nova or cinder", c.service)
		}
		if c.endpoint != eps[c.service] {
			t.Errorf("probed %s at %q, want its endpoint %q", c.service, c.endpoint, eps[c.service])
		}
		if c.token != drawn.token {
			t.Errorf("probed with %q, want the drawn pool token %q", c.token, drawn.token)
		}
		if !maps.Equal(poolTokens(s), before) {
			t.Error("probe changed the pool")
		}
		byType := s.ServiceStats(time.Second).Probes.ByType
		if len(byType) != 1 || byType[0].Type != c.service || byType[0].Attempted != 1 || byType[0].Succeeded != 1 {
			t.Errorf("probe samples = %+v, want one successful %s sample", byType, c.service)
		}
	})

	t.Run("draws among the listed services only", func(t *testing.T) {
		s, _, fs := primedServiceSet(t, fixtureOptions(3, probeWeights), eps, 0)
		probeOp, rng := operation(t, s, OpProbe), rand.New(rand.NewSource(1))
		for range 50 {
			if err := probeOp(ctx, rng); err != nil {
				t.Fatalf("probe: %v", err)
			}
		}
		seen := make(map[string]int)
		for _, c := range fs.recorded(kindProbe) {
			seen[c.service]++
		}
		if len(seen) != 2 || seen["nova"] == 0 || seen["cinder"] == 0 {
			t.Errorf("probes per service = %v, want both nova and cinder and nothing else", seen)
		}
	})

	t.Run("a failing probe", func(t *testing.T) {
		s, _, fs := primedServiceSet(t, fixtureOptions(3, probeWeights), eps, 0)
		fs.answerAll(gophercloud.ErrUnexpectedResponseCode{Actual: 401})
		err := operation(t, s, OpProbe)(ctx, rand.New(rand.NewSource(1)))
		if !gophercloud.ResponseCodeIs(err, 401) {
			t.Fatalf("probe error = %v, want the service's 401", err)
		}
		c := fs.recorded(kindProbe)[0]
		probes := s.ServiceStats(time.Second).Probes
		if len(probes.ByType) != 1 || probes.ByType[0].Type != c.service || probes.ByType[0].Failed != 1 || probes.ByType[0].Succeeded != 0 {
			t.Errorf("probe samples = %+v, want one failed %s sample", probes.ByType, c.service)
		}
		if want := []metrics.ErrorCount{{Kind: "unauthorized", Count: 1}}; Classify(err) != "unauthorized" || !reflect.DeepEqual(probes.Errors, want) {
			t.Errorf("probe errors = %+v with Classify %q, want %+v", probes.Errors, Classify(err), want)
		}
	})
}

// TestProbeNoService checks that probe and bad_token make no services call
// and record no sample when they have nothing to send: on an empty pool they
// return ErrPoolEmpty, and on a token whose catalog lists none of the four
// services ErrNoService, labelled pool-empty and no-service.
func TestProbeNoService(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{OpProbe, OpBadToken} {
		t.Run(name+" on an empty pool", func(t *testing.T) {
			s, _, fs := primedServiceSet(t, fixtureOptions(1, probeWeights), fixtureEndpoints("nova"), 0)
			rng := rand.New(rand.NewSource(1))
			s.pool.take(rng) // another worker holds the only entry
			err := operation(t, s, name)(ctx, rng)
			if !errors.Is(err, ErrPoolEmpty) || Classify(err) != "pool-empty" {
				t.Errorf("%s error = %v (%q), want ErrPoolEmpty", name, err, Classify(err))
			}
			assertNoServiceCall(t, s, fs)
		})

		t.Run(name+" on a token without a service", func(t *testing.T) {
			// A run that needs no service primes over tokens whose catalog
			// lists none of the four, as the client returns it: empty, not nil.
			s, _, fs := primedServiceSet(t, fixtureOptions(3, churnOn), map[string]string{}, 0)
			err := operation(t, s, name)(ctx, rand.New(rand.NewSource(1)))
			if !errors.Is(err, ErrNoService) || Classify(err) != "no-service" {
				t.Errorf("%s error = %v (%q), want ErrNoService", name, err, Classify(err))
			}
			assertNoServiceCall(t, s, fs)
		})
	}
}

// assertNoServiceCall fails the test when fs saw a call or s recorded a probe
// or bad-token sample.
func assertNoServiceCall(t *testing.T, s *Set, fs *fakeServices) {
	t.Helper()
	if calls := fs.recorded(); len(calls) != 0 {
		t.Errorf("services calls = %+v, want none", calls)
	}
	ss := s.ServiceStats(time.Second)
	if ss.Probes.Overall.Attempted != 0 || ss.BadTokens.Overall.Attempted != 0 {
		t.Errorf("recorded %d probe and %d bad-token samples, want none", ss.Probes.Overall.Attempted, ss.BadTokens.Overall.Attempted)
	}
}

// fernetAlphabet is the alphabet of a Fernet token, A-Za-z0-9-_.
const fernetAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// TestBadTokenRandom checks the random bad token of a set that revoked no
// token: over twenty seeds, each draw rejects at neutron, the one service the
// catalog lists, a token as long as the drawn pool token, made of the Fernet
// alphabet only and none of the pool's tokens. The pool holds tok-1 to tok-12,
// so the drawn tokens are 5 or 6 characters long, and the draws cover both.
// The token is the one a generator replayed through the pool draw and the
// service draw alone yields: on an empty ring the draw makes no even draw,
// so a seeded worker sends the tokens it sent before the ring existed.
func TestBadTokenRandom(t *testing.T) {
	ctx := context.Background()
	eps := fixtureEndpoints("neutron")
	s, _, fs := primedServiceSet(t, fixtureOptions(12, probeWeights), eps, 0)
	pool := poolTokens(s)
	badToken := operation(t, s, OpBadToken)
	lengths := make(map[int]bool)
	for seed := range int64(20) {
		// The operation's first draw picks the pool token it takes the
		// length from, so a peek with the same seed names it; the service
		// draw among its listed services, neutron alone, follows, and then
		// the token's characters.
		replay := rand.New(rand.NewSource(seed))
		drawn, _ := s.pool.peek(replay)
		replay.Intn(len(listedServices(drawn.endpoints)))
		want := randomToken(replay, len(drawn.token))
		m := len(fs.recorded())
		if err := badToken(ctx, rand.New(rand.NewSource(seed))); err != nil {
			t.Fatalf("bad_token (seed %d): %v", seed, err)
		}
		calls := fs.recorded()[m:]
		if len(calls) != 1 || calls[0].kind != kindReject {
			t.Fatalf("services calls = %+v, want one reject", calls)
		}
		c := calls[0]
		if c.service != "neutron" || c.endpoint != eps["neutron"] {
			t.Errorf("rejected at %s %q, want neutron at %q", c.service, c.endpoint, eps["neutron"])
		}
		if len(c.token) != len(drawn.token) {
			t.Errorf("seed %d: sent a token of %d characters, want %d as the drawn pool token", seed, len(c.token), len(drawn.token))
		}
		if strings.Trim(c.token, fernetAlphabet) != "" {
			t.Errorf("seed %d: sent a token with characters outside A-Za-z0-9-_", seed)
		}
		if _, ok := pool[c.token]; ok {
			t.Errorf("seed %d: sent a pool token as a bad token", seed)
		}
		if c.token != want {
			t.Errorf("seed %d: sent a token other than the one the pool and service draws alone lead to", seed)
		}
		lengths[len(drawn.token)] = true
	}
	if !lengths[5] || !lengths[6] {
		t.Errorf("the drawn pool tokens had lengths %v, want both 5 and 6", lengths)
	}
	byType := s.ServiceStats(time.Second).BadTokens.ByType
	if len(byType) != 1 || byType[0].Type != "neutron" || byType[0].Attempted != 20 || byType[0].Succeeded != 20 {
		t.Errorf("bad-token samples = %+v, want 20 successful neutron samples", byType)
	}
}

// TestBadTokenRevoked checks the revoked bad token: a token the revoke
// operation revoked joins the ring at the set's clock, is never sent while it
// is 4 minutes old, and from 5 minutes on is sent on about half the draws,
// the others sending a random token. A failed revoke adds nothing.
func TestBadTokenRevoked(t *testing.T) {
	ctx := context.Background()
	weights := map[string]int{OpValidate: 1, OpRevoke: 1, OpBadToken: 1}

	t.Run("sent once 5 minutes old", func(t *testing.T) {
		s, f, fs := primedServiceSet(t, fixtureOptions(3, weights), fixtureEndpoints("nova", "glance"), 0)
		start := f.clock.Now()
		m := f.mark()
		if err := operation(t, s, OpRevoke)(ctx, rand.New(rand.NewSource(1))); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		revoked := f.since(m)[0].token
		if got := revokedOf(s); !reflect.DeepEqual(got, []string{revoked}) {
			t.Fatalf("revoked tokens = %v, want the revoked %q", got, revoked)
		}
		if at := s.revoked.entries[0].at; !at.Equal(start) {
			t.Errorf("the token was revoked at %s on the ring, want the set's clock %s", at, start)
		}

		// draws runs bad_token n times with a generator seeded with seed and
		// counts the revoked and the random tokens it sent.
		badToken := operation(t, s, OpBadToken)
		draws := func(n int, seed int64) (revokedSent, randomSent int) {
			t.Helper()
			rng := rand.New(rand.NewSource(seed))
			m := len(fs.recorded(kindReject))
			for range n {
				if err := badToken(ctx, rng); err != nil {
					t.Fatalf("bad_token: %v", err)
				}
			}
			for _, c := range fs.recorded(kindReject)[m:] {
				if c.token == revoked {
					revokedSent++
				} else {
					randomSent++
				}
			}
			return revokedSent, randomSent
		}

		f.clock.Set(start.Add(4 * time.Minute))
		if r, _ := draws(20, 7); r != 0 {
			t.Errorf("the revoked token was sent %d times at 4 minutes, want never", r)
		}
		f.clock.Set(start.Add(5 * time.Minute))
		if r, rnd := draws(20, 7); r == 0 || rnd == 0 {
			t.Errorf("at 5 minutes the draws sent the revoked token %d and a random one %d times, want both at least once", r, rnd)
		}
	})

	t.Run("a failed revoke adds nothing", func(t *testing.T) {
		s, f, _ := primedServiceSet(t, fixtureOptions(3, weights), fixtureEndpoints("nova"), 0)
		f.fail(kindRevoke, gophercloud.ErrUnexpectedResponseCode{Actual: 404})
		if err := operation(t, s, OpRevoke)(ctx, rand.New(rand.NewSource(1))); err == nil {
			t.Fatal("revoke succeeded, want the 404")
		}
		if n := s.revoked.len(); n != 0 {
			t.Errorf("revoked tokens hold %d after a failed revoke, want none", n)
		}
	})
}

// TestBadTokenOutcomes checks what a bad_token draw makes of the service's
// answer: the 401, which the fake's nil answer stands for, is a success, a
// service that accepted the token fails the draw with probe.ErrAccepted,
// labelled accepted, and any other failure is returned as is. Each lands in
// the set's bad-token samples under the service.
func TestBadTokenOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		answer   error
		check    func(error) bool
		wantKind string // the Classify label; empty for a success
	}{
		{"a 401 is a success", nil, func(err error) bool { return err == nil }, ""},
		{"an accepted token", probe.ErrAccepted, func(err error) bool { return errors.Is(err, probe.ErrAccepted) }, "accepted"},
		{"a 503", gophercloud.ErrUnexpectedResponseCode{Actual: 503}, func(err error) bool { return gophercloud.ResponseCodeIs(err, 503) }, "transient"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, fs := primedServiceSet(t, fixtureOptions(3, probeWeights), fixtureEndpoints("cinder"), 0)
			fs.answer("cinder", tt.answer)
			err := operation(t, s, OpBadToken)(context.Background(), rand.New(rand.NewSource(1)))
			if !tt.check(err) || Classify(err) != tt.wantKind {
				t.Fatalf("bad_token error = %v (%q), want the %q outcome", err, Classify(err), tt.wantKind)
			}
			bad := s.ServiceStats(time.Second).BadTokens
			if len(bad.ByType) != 1 || bad.ByType[0].Type != "cinder" || bad.ByType[0].Attempted != 1 {
				t.Fatalf("bad-token samples = %+v, want one cinder sample", bad.ByType)
			}
			var wantErrors []metrics.ErrorCount
			wantFailed := 0
			if tt.wantKind != "" {
				wantErrors, wantFailed = []metrics.ErrorCount{{Kind: tt.wantKind, Count: 1}}, 1
			}
			if bad.ByType[0].Failed != wantFailed || !reflect.DeepEqual(bad.Errors, wantErrors) {
				t.Errorf("bad-token sample failed %d with errors %+v, want %d and %+v", bad.ByType[0].Failed, bad.Errors, wantFailed, wantErrors)
			}
		})
	}
}

// propagationOptions are options for pressurePlan with a pool of tokens, the
// validate operation only, seed 7 and the propagation probe at every and
// timeout.
func propagationOptions(tokens int, every, timeout time.Duration) Options {
	o := fixtureOptions(tokens, map[string]int{OpValidate: 1})
	o.Seed = 7
	o.Propagation = Propagation{Every: every, Timeout: timeout}
	return o
}

// TestPropagationProbe runs the propagation probe in a synctest bubble, whose
// clock makes its timing exact. With Every 20ms, Timeout 500ms and a poll
// every 5ms over tokens listing nova and glance, the first probe starts at
// 20ms, revokes and replaces a pool token, and polls nova with it at 20, 25
// and 30ms; the services fake rejects a token's third poll, so its delay is
// two poll intervals. The second probe starts at 40ms and polls glance, the
// next service in turn, at 40 and 45ms, and the test closes the set at 48ms,
// before that probe's third poll, so it is abandoned. Each tick that fired
// made one revoke and one reissue, each bounded by OpTimeout, every poll is
// bounded by its probe's deadline, the pool keeps its size, and the revoked
// tokens left the pool and joined the ring.
func TestPropagationProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const every, timeout, pollEvery = 20 * time.Millisecond, 500 * time.Millisecond, 5 * time.Millisecond
		const ticks = 2 // at 20 and 40ms
		o := propagationOptions(3, every, timeout)
		eps := fixtureEndpoints("nova", "glance")
		start := time.Now()
		s, f, fs := primedServiceSet(t, o, eps, pollEvery)
		fs.rejectFromPoll(3)
		primed := f.mark()
		time.Sleep(48 * time.Millisecond)
		s.Close()

		var revokes, issues []call
		for _, c := range f.since(primed) {
			switch c.kind {
			case kindRevoke:
				revokes = append(revokes, c)
			case kindIssue:
				issues = append(issues, c)
			}
		}
		if len(revokes) != ticks || len(issues) != ticks {
			t.Fatalf("the fake saw %d revokes and %d issues, want one of each per tick (%d)", len(revokes), len(issues), ticks)
		}
		for k := range ticks {
			want := start.Add(time.Duration(k+1) * every).Add(o.OpTimeout)
			if !revokes[k].deadline.Equal(want) || !issues[k].deadline.Equal(want) {
				t.Errorf("probe %d revoked with deadline %v and reissued with %v, want both %v", k, revokes[k].deadline, issues[k].deadline, want)
			}
		}
		if n := s.pool.len(); n != 3 {
			t.Errorf("pool size = %d, want 3", n)
		}
		first, second := revokes[0].token, revokes[1].token
		pool := poolTokens(s)
		for _, tok := range []string{first, second} {
			if _, ok := pool[tok]; ok {
				t.Errorf("the revoked token %s is still in the pool", tok)
			}
		}

		polls := fs.polledWith(first)
		if len(polls) != 3 || !polls[2].rejected || polls[0].rejected || polls[1].rejected {
			t.Fatalf("polls with the first revoked token = %+v, want three, the third rejected", polls)
		}
		for _, c := range polls {
			if c.service != "nova" || c.endpoint != eps["nova"] {
				t.Errorf("the first probe polled %s at %q, want nova at %q", c.service, c.endpoint, eps["nova"])
			}
			if want := start.Add(every + timeout); !c.deadline.Equal(want) {
				t.Errorf("a poll ran with deadline %v, want the probe's deadline %v", c.deadline, want)
			}
		}
		secondPolls := fs.polledWith(second)
		if len(secondPolls) != 2 {
			t.Errorf("the second probe polled %d times before Close, want 2", len(secondPolls))
		}
		for _, c := range secondPolls {
			if c.service != "glance" || c.endpoint != eps["glance"] {
				t.Errorf("the second probe polled %s at %q, want glance at %q", c.service, c.endpoint, eps["glance"])
			}
		}

		delays := fs.recorded(kindRevocation)
		if len(delays) != 1 || !delays[0].ok {
			t.Fatalf("RecordRevocationDelay calls = %+v, want one with ok", delays)
		}
		d := delays[0].d
		if d < 2*pollEvery {
			t.Errorf("delay = %s, want at least two poll intervals (%s)", d, 2*pollEvery)
		}
		ps := s.ServiceStats(time.Second).Propagation
		if ps.Probes != 1 || ps.TimedOut != 0 || ps.Delay.Max != d {
			t.Errorf("propagation stats = %+v, want 1 probe, none timed out and a max delay of %s", ps, d)
		}
		if ps.Every != every || ps.Timeout != timeout {
			t.Errorf("propagation settings = %s every, %s timeout, want %s and %s", ps.Every, ps.Timeout, every, timeout)
		}
		if got := revokedOf(s); !reflect.DeepEqual(got, []string{first, second}) {
			t.Errorf("revoked tokens = %v, want %v", got, []string{first, second})
		}
		// The ring ages a token on the set's clock, which the bubble's time
		// does not move, not at the instant the delay is measured from.
		for i, r := range s.revoked.entries {
			if !r.at.Equal(f.clock.Now()) {
				t.Errorf("revoked token %d joined the ring at %s, want the set's clock %s", i, r.at, f.clock.Now())
			}
		}
	})
}

// TestPropagationFallbackService checks the service a probe polls when the
// revoked token's catalog lacks the one whose turn it is, in a synctest
// bubble. The set's services come from the first pool token, nova and
// glance, and the test then gives every pool token, and every token issued
// later, a catalog of neutron and glance alone. The first probe, at 20ms,
// whose turn is nova, polls neutron, the first service its token's catalog
// lists, at that service's endpoint; the second, at 40ms, polls glance, its
// turn. The services fake rejects every first poll, so each probe polls
// once.
func TestPropagationFallbackService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, fs := primedServiceSet(t, propagationOptions(3, 20*time.Millisecond, 500*time.Millisecond), fixtureEndpoints("nova", "glance"), 5*time.Millisecond)
		fs.rejectFromPoll(1)
		if got, want := s.Services(), []string{"nova", "glance"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("Services() = %v, want %v", got, want)
		}
		eps := fixtureEndpoints("neutron", "glance")
		s.pool.mu.Lock()
		for i := range s.pool.entries {
			s.pool.entries[i].endpoints = maps.Clone(eps)
		}
		s.pool.mu.Unlock()
		f.setEndpoints(eps)
		time.Sleep(45 * time.Millisecond)
		s.Close()

		revokes := f.recorded(kindRevoke)
		if len(revokes) != 2 {
			t.Fatalf("the fake saw %d revokes, want one per tick (2)", len(revokes))
		}
		for k, want := range []string{"neutron", "glance"} {
			polls := fs.polledWith(revokes[k].token)
			if len(polls) != 1 || polls[0].service != want || polls[0].endpoint != eps[want] {
				t.Errorf("probe %d polled %+v, want one poll of %s at %q", k, polls, want, eps[want])
			}
		}
	})
}

// TestPropagationPollBound checks the bound of each poll, in a synctest
// bubble: the earlier of the probe's deadline and o.OpTimeout from the poll's
// start. With Every 20ms, Timeout 12ms, OpTimeout 6ms, a poll every 5ms and a
// service that never refuses the token, the probe revokes at 20ms, so its
// deadline is 32ms. It polls at 20 and 25ms, each bounded by OpTimeout, to 26
// and 31ms, and then at 30ms and at the deadline, both bounded by the
// deadline, before it records the timeout.
func TestPropagationPollBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const every, timeout, opTimeout, pollEvery = 20 * time.Millisecond, 12 * time.Millisecond, 6 * time.Millisecond, 5 * time.Millisecond
		o := propagationOptions(3, every, timeout)
		o.OpTimeout = opTimeout
		start := time.Now()
		s, f, fs := primedServiceSet(t, o, fixtureEndpoints("cinder"), pollEvery)
		time.Sleep(35 * time.Millisecond)
		s.Close()

		revokes := f.recorded(kindRevoke)
		if len(revokes) != 1 {
			t.Fatalf("the fake saw %d revokes, want one, at 20ms", len(revokes))
		}
		ms := func(n int) time.Time { return start.Add(time.Duration(n) * time.Millisecond) }
		want := []struct{ began, deadline time.Time }{{ms(20), ms(26)}, {ms(25), ms(31)}, {ms(30), ms(32)}, {ms(32), ms(32)}}
		polls := fs.polledWith(revokes[0].token)
		if len(polls) != len(want) {
			t.Fatalf("the probe polled %d times, want %d", len(polls), len(want))
		}
		for i, c := range polls {
			if !c.began.Equal(want[i].began) || !c.deadline.Equal(want[i].deadline) {
				t.Errorf("poll %d began at %v with deadline %v, want %v and %v", i, c.began.Sub(start), c.deadline.Sub(start), want[i].began.Sub(start), want[i].deadline.Sub(start))
			}
		}
		if delays := fs.recorded(kindRevocation); len(delays) != 1 || delays[0].d != timeout || delays[0].ok {
			t.Errorf("RecordRevocationDelay calls = %+v, want one of %s without ok", delays, timeout)
		}
	})
}

// TestPropagationTimeout runs a propagation probe against a service that never
// refuses the revoked token, in a synctest bubble: with Every 20ms, Timeout
// 50ms and a poll every 5ms, the first probe polls at 20 to 65ms, waits until
// its deadline at 70ms, sends one last poll there on a context already at the
// deadline, and then times out, recording the timeout as its delay and
// logging the warning; the test closes the set at 78ms, before the later
// probes' deadlines. A service that fails every poll with a 503 is no answer
// either: the probe keeps polling until the timeout. A service that hangs
// gets one poll, which the probe's deadline cuts off at 70ms; the poll takes
// until 80ms to return, and the probe still records the timeout itself, not
// the 60ms that passed. In every case the ticks at 20, 40 and 60ms (and 80ms
// for the hung service) each revoked a token on time, although the first
// probe was still polling: each probe runs on its own goroutine.
func TestPropagationTimeout(t *testing.T) {
	const every, timeout, pollEvery = 20 * time.Millisecond, 50 * time.Millisecond, 5 * time.Millisecond
	tests := []struct {
		name      string
		answer    error            // what every poll that is not rejected returns
		hang      time.Duration    // when positive, every poll blocks until its context ends and returns this much later
		check     func(error) bool // whether the error of a poll before the deadline is the service's answer
		run       time.Duration    // when the test closes the set
		polls     int              // the first probe's polls, the last one ending at its deadline
		lastBegan time.Duration    // when the first probe's last poll began, after the start
		ticks     int              // the ticks before the test closes the set, one revoke each
	}{
		{"a service that keeps accepting the token", nil, 0, func(err error) bool { return err == nil }, 78 * time.Millisecond, int(timeout/pollEvery) + 1, every + timeout, 3},
		{"a service that fails every poll", gophercloud.ErrUnexpectedResponseCode{Actual: 503}, 0, func(err error) bool { return gophercloud.ResponseCodeIs(err, 503) }, 78 * time.Millisecond, int(timeout/pollEvery) + 1, every + timeout, 3},
		// The hung service never answers a poll before the deadline.
		{"a service that hangs", nil, 10 * time.Millisecond, func(err error) bool { return err == nil }, 85 * time.Millisecond, 1, every, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := captureLogs(t)
				start := time.Now()
				o := propagationOptions(3, every, timeout)
				s, f, fs := primedServiceSet(t, o, fixtureEndpoints("neutron"), pollEvery)
				fs.answerAll(tt.answer)
				if tt.hang > 0 {
					fs.blockPolls(tt.hang)
				}
				time.Sleep(tt.run)
				s.Close()

				delays := fs.recorded(kindRevocation)
				if len(delays) != 1 || delays[0].d != timeout || delays[0].ok {
					t.Fatalf("RecordRevocationDelay calls = %+v, want one of %s without ok", delays, timeout)
				}
				ps := s.ServiceStats(time.Second).Propagation
				if ps.Probes != 1 || ps.TimedOut != 1 || ps.Delay.Max != timeout {
					t.Errorf("propagation stats = %+v, want 1 probe, 1 timed out and a max delay of %s", ps, timeout)
				}
				revokes := f.recorded(kindRevoke)
				if len(revokes) != tt.ticks {
					t.Fatalf("the fake saw %d revokes, want one per tick (%d) while the first probe polled", len(revokes), tt.ticks)
				}
				for k, c := range revokes {
					if want := start.Add(time.Duration(k+1) * every).Add(o.OpTimeout); !c.deadline.Equal(want) {
						t.Errorf("probe %d revoked with deadline %v, want its tick plus the op timeout, %v", k, c.deadline, want)
					}
				}
				polls := fs.polledWith(revokes[0].token)
				if len(polls) != tt.polls {
					t.Fatalf("the first probe polled %d times, want %d until its timeout", len(polls), tt.polls)
				}
				for _, c := range polls[:len(polls)-1] {
					if c.rejected || !tt.check(c.err) {
						t.Errorf("a poll answered %v, %v, want the service's answer %v", c.rejected, c.err, tt.answer)
					}
				}
				deadline := start.Add(every + timeout)
				last := polls[len(polls)-1]
				if last.rejected || !errors.Is(last.err, context.DeadlineExceeded) || !last.deadline.Equal(deadline) {
					t.Errorf("the last poll answered %v, %v with deadline %v, want it cut off at the probe's deadline %v", last.rejected, last.err, last.deadline, deadline)
				}
				if want := start.Add(tt.lastBegan); !last.began.Equal(want) {
					t.Errorf("the last poll began at %v, want %v", last.began, want)
				}
				out := logs.String()
				for _, want := range []string{"propagation probe timed out; the service still accepted the revoked token", "service=neutron", "timeout=50ms"} {
					if !strings.Contains(out, want) {
						t.Errorf("log = %q, want %q", out, want)
					}
				}
				if strings.Contains(out, "tok-") {
					t.Errorf("log = %q carries a token", out)
				}
			})
		})
	}
}

// TestPropagationSkips checks the three ways a propagation probe skips, each
// in a synctest bubble with one tick at 20ms before the test closes the set at
// 30ms: a failed revoke still reissues for the token's pair, keeps the pool's
// size and polls nothing; an empty pool revokes nothing; and a token whose
// catalog lists no service is revoked and replaced but polls nothing. Each
// logs its warning without a token and records no probe.
func TestPropagationSkips(t *testing.T) {
	const every, timeout, pollEvery = 20 * time.Millisecond, 500 * time.Millisecond, 5 * time.Millisecond

	// run primes a set of tokens tokens over nova, lets prepare change it,
	// runs it for one tick and closes it, and returns the set, the fakes, the
	// Keystone calls the tick made, the pool before the tick and the log.
	run := func(t *testing.T, tokens int, prepare func(*Set, *fakeKeystone)) (*Set, *fakeServices, []call, map[string]int, string) {
		t.Helper()
		logs := captureLogs(t)
		s, f, fs := primedServiceSet(t, propagationOptions(tokens, every, timeout), fixtureEndpoints("nova"), pollEvery)
		prepare(s, f)
		before := poolTokens(s)
		m := f.mark()
		time.Sleep(30 * time.Millisecond)
		s.Close()
		out := logs.String()
		if strings.Contains(out, "tok-") {
			t.Errorf("log = %q carries a token", out)
		}
		if polls := fs.recorded(kindPoll); len(polls) != 0 {
			t.Errorf("polls = %+v, want none", polls)
		}
		if delays := fs.recorded(kindRevocation); len(delays) != 0 || s.ServiceStats(time.Second).Propagation.Probes != 0 {
			t.Errorf("recorded the delays %+v, want no probe", delays)
		}
		return s, fs, f.since(m), before, out
	}

	t.Run("the revoke failed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _, calls, before, out := run(t, 3, func(_ *Set, f *fakeKeystone) { f.fail(kindRevoke, errFake) })
			if len(calls) != 2 || calls[0].kind != kindRevoke || calls[1].kind != kindIssue {
				t.Fatalf("calls = %+v, want a revoke and then an issue", calls)
			}
			p := s.pairs[before[calls[0].token]].issue
			if calls[1].user != p.User || calls[1].project != p.Project {
				t.Errorf("reissued for (%s, %s), want the revoked token's pair (%s, %s)", calls[1].user, calls[1].project, p.User, p.Project)
			}
			if n := s.pool.len(); n != 3 {
				t.Errorf("pool size = %d, want 3", n)
			}
			if !strings.Contains(out, "propagation probe skipped; the revoke failed") {
				t.Errorf("log = %q, want the revoke warning", out)
			}
			if n := s.revoked.len(); n != 0 {
				t.Errorf("revoked tokens hold %d after a failed revoke, want none", n)
			}
		})
	})

	t.Run("the token pool is empty", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, _, calls, _, out := run(t, 1, func(s *Set, _ *fakeKeystone) { s.pool.take(rand.New(rand.NewSource(1))) })
			if len(calls) != 0 {
				t.Errorf("calls = %+v, want none", calls)
			}
			if !strings.Contains(out, "propagation probe skipped; the token pool is empty") {
				t.Errorf("log = %q, want the empty pool warning", out)
			}
		})
	})

	t.Run("the token's catalog lists no service", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var token string
			s, _, calls, _, out := run(t, 1, func(s *Set, _ *fakeKeystone) {
				e, _ := s.pool.take(rand.New(rand.NewSource(1)))
				e.endpoints = map[string]string{}
				s.pool.put(e)
				token = e.token
			})
			if len(calls) != 2 || calls[0].kind != kindRevoke || calls[0].token != token || calls[1].kind != kindIssue {
				t.Fatalf("calls = %+v, want the token's revoke and then an issue", calls)
			}
			if !strings.Contains(out, "propagation probe skipped; the token's catalog lists no service") {
				t.Errorf("log = %q, want the catalog warning", out)
			}
			if got := revokedOf(s); !reflect.DeepEqual(got, []string{token}) {
				t.Errorf("revoked tokens = %v, want the revoked %s", got, token)
			}
		})
	})
}

// TestPropagationCloseWaits checks that Close waits for a probe in flight, in
// a synctest bubble: the first probe starts at 20ms with a 10ms timeout, and
// its poll blocks until its context ends and then takes 10ms more to return.
// Close at 25ms cuts it short and returns only after the poll returned, at
// 35ms on the bubble's clock, with the context's error, and after the probe
// and its ticker goroutine ended. The poll returned past the probe's 30ms
// deadline, yet the probe records no delay and no timeout: the run ended. A
// Close while the probe's revoke or reissue is held does not cut either
// short: it returns once both returned, the pool keeps its size, nothing
// logs a failure, and the abandoned probe polls nothing. A probe that starts
// once the run ended, as one whose tick raced Close does, takes no token. A
// Prime after Close starts no probe.
func TestPropagationCloseWaits(t *testing.T) {
	const linger = 10 * time.Millisecond
	t.Run("a poll in flight", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _, fs := primedServiceSet(t, propagationOptions(3, 20*time.Millisecond, 10*time.Millisecond), fixtureEndpoints("glance"), 5*time.Millisecond)
			fs.blockPolls(linger)
			time.Sleep(25 * time.Millisecond) // the first probe's poll blocks from 20ms on
			if polls := fs.recorded(kindPoll); len(polls) != 0 {
				t.Fatalf("polls = %+v returned before Close, want the poll blocked", polls)
			}
			closing := time.Now()
			s.Close()
			if waited := time.Since(closing); waited < linger {
				t.Errorf("Close returned after %s, before the blocked poll returned %s after the cancel", waited, linger)
			}
			polls := fs.recorded(kindPoll)
			if len(polls) != 1 || !errors.Is(polls[0].err, context.Canceled) {
				t.Fatalf("polls = %+v, want the one poll returned with context.Canceled", polls)
			}
			select {
			case <-s.propDone:
			default:
				t.Error("Close returned before the propagation goroutine")
			}
			if delays := fs.recorded(kindRevocation); len(delays) != 0 {
				t.Errorf("recorded the delays %+v for an abandoned probe, want none", delays)
			}
			if ps := s.ServiceStats(time.Second).Propagation; ps.Probes != 0 || ps.TimedOut != 0 {
				t.Errorf("propagation stats = %+v, want no probe", ps)
			}
		})
	})

	for _, held := range []struct {
		name string
		gate func(*fakeKeystone, *testing.T)
		open func(*fakeKeystone)
	}{
		{"a revoke in flight", (*fakeKeystone).gateRevokes, (*fakeKeystone).openRevokeGate},
		{"a reissue in flight", (*fakeKeystone).gateIssues, (*fakeKeystone).openGate},
	} {
		t.Run(held.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := captureLogs(t)
				s, f, fs := primedServiceSet(t, propagationOptions(3, 20*time.Millisecond, 500*time.Millisecond), fixtureEndpoints("glance"), 5*time.Millisecond)
				held.gate(f, t)
				time.Sleep(25 * time.Millisecond) // the first probe's call is held from 20ms on

				closed := make(chan struct{})
				go func() {
					s.Close()
					close(closed)
				}()
				synctest.Wait()
				select {
				case <-closed:
					t.Fatal("Close returned while the probe's call was held")
				default:
				}
				held.open(f)
				<-closed

				if n := s.pool.len(); n != 3 {
					t.Errorf("pool size = %d after Close, want 3", n)
				}
				revokes, issues := f.recorded(kindRevoke), f.recorded(kindIssue)
				if len(revokes) != 1 || len(issues) != 4 { // three primed, one reissued
					t.Errorf("the fake saw %d revokes and %d issues, want 1 and 4", len(revokes), len(issues))
				}
				out := logs.String()
				for _, unwanted := range []string{"token pool shrank", "propagation probe skipped", "tok-"} {
					if strings.Contains(out, unwanted) {
						t.Errorf("log = %q, want no %q", out, unwanted)
					}
				}
				if calls := fs.recorded(kindPoll, kindRevocation); len(calls) != 0 {
					t.Errorf("services calls = %+v, want no poll and no delay for the abandoned probe", calls)
				}
				if ps := s.ServiceStats(time.Second).Propagation; ps.Probes != 0 {
					t.Errorf("propagation stats = %+v, want no probe", ps)
				}
			})
		})
	}

	t.Run("a probe that starts after the run ended", func(t *testing.T) {
		// A tick can race Close's cancel; the probe it starts then sees the
		// ended context first.
		o := propagationOptions(3, time.Hour, 500*time.Millisecond) // never ticks during the test
		s, f, fs := primedServiceSet(t, o, fixtureEndpoints("glance"), 0)
		before := poolTokens(s)
		m := f.mark()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.propagate(ctx, 0, rand.New(rand.NewSource(1)))
		if calls := f.since(m); len(calls) != 0 {
			t.Errorf("Keystone calls = %+v, want none", calls)
		}
		if !maps.Equal(poolTokens(s), before) {
			t.Error("the probe changed the pool")
		}
		if calls := fs.recorded(); len(calls) != 0 || s.revoked.len() != 0 {
			t.Errorf("services calls = %+v and %d revoked tokens, want none", calls, s.revoked.len())
		}
	})

	t.Run("a Prime after Close", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		f.setEndpoints(fixtureEndpoints("glance"))
		s := buildServiceSet(t, f, newFakeServices(), propagationOptions(2, 20*time.Millisecond, 500*time.Millisecond))
		s.Close()
		if err := s.Prime(context.Background(), 2); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		if s.propDone != nil {
			t.Error("a Prime after Close started the propagation probe")
		}
	})
}

// TestServiceStats checks what ServiceStats reports: nothing but the
// propagation settings for a set that recorded nothing; the probe and
// bad-token samples per service, sorted by service, with their throughput
// over the wall and their error labels; and the propagation outcome from the
// set's delays, a timed-out probe counting as the timeout. It also reads the
// stats while workers draw, which the race detector checks.
func TestServiceStats(t *testing.T) {
	ctx := context.Background()
	o := fixtureOptions(3, probeWeights)
	o.Propagation = Propagation{Every: time.Hour, Timeout: 10 * time.Minute} // never ticks during the test
	eps := fixtureEndpoints("nova", "glance")

	t.Run("nothing recorded", func(t *testing.T) {
		s, _, _ := primedServiceSet(t, o, eps, 0)
		ss := s.ServiceStats(time.Second)
		if ss.Probes.Overall.Attempted != 0 || ss.Probes.ByType != nil || ss.BadTokens.Overall.Attempted != 0 || ss.BadTokens.ByType != nil {
			t.Errorf("stats = %+v, want no probe and no bad-token sample", ss)
		}
		if want := (PropagationStats{Every: time.Hour, Timeout: 10 * time.Minute}); ss.Propagation != want {
			t.Errorf("propagation stats = %+v, want %+v", ss.Propagation, want)
		}
	})

	t.Run("samples per service and the delays", func(t *testing.T) {
		s, _, fs := primedServiceSet(t, o, eps, 0)
		fs.answer("glance", gophercloud.ErrUnexpectedResponseCode{Actual: 503})
		rng := rand.New(rand.NewSource(1))
		for _, name := range []string{OpProbe, OpBadToken} {
			op := operation(t, s, name)
			for range 20 {
				_ = op(ctx, rng) // glance fails, nova succeeds
			}
		}
		perService := func(kind string) map[string]int {
			n := make(map[string]int)
			for _, c := range fs.recorded(kind) {
				n[c.service]++
			}
			return n
		}
		ss := s.ServiceStats(2 * time.Second)
		for _, c := range []struct {
			name  string
			kind  string
			stats metrics.Aggregate
		}{{"probe", kindProbe, ss.Probes}, {"bad-token", kindReject, ss.BadTokens}} {
			n := perService(c.kind)
			if n["glance"] == 0 || n["nova"] == 0 {
				t.Fatalf("%s calls per service = %v, want both services", c.name, n)
			}
			byType := c.stats.ByType
			if len(byType) != 2 || byType[0].Type != "glance" || byType[1].Type != "nova" {
				t.Fatalf("%s samples = %+v, want glance and then nova", c.name, byType)
			}
			if g := byType[0]; g.Attempted != n["glance"] || g.Failed != n["glance"] {
				t.Errorf("%s glance samples = %+v, want %d, all failed", c.name, g, n["glance"])
			}
			if nv := byType[1]; nv.Attempted != n["nova"] || nv.Succeeded != n["nova"] || nv.Throughput != float64(n["nova"])/2 {
				t.Errorf("%s nova samples = %+v, want %d, all succeeded, at %v/s over the 2s wall", c.name, nv, n["nova"], float64(n["nova"])/2)
			}
			if want := []metrics.ErrorCount{{Kind: "transient", Count: n["glance"]}}; !reflect.DeepEqual(c.stats.Errors, want) {
				t.Errorf("%s errors = %+v, want %+v", c.name, c.stats.Errors, want)
			}
		}

		s.recordPropagation(ctx, 2*time.Second, true)
		s.recordPropagation(ctx, 10*time.Minute, false)
		ps := s.ServiceStats(time.Second).Propagation
		if ps.Probes != 2 || ps.TimedOut != 1 || ps.Delay.Min != 2*time.Second || ps.Delay.Max != 10*time.Minute {
			t.Errorf("propagation stats = %+v, want 2 probes, 1 timed out, delays from 2s to the 10m timeout", ps)
		}
		if delays := fs.recorded(kindRevocation); len(delays) != 2 {
			t.Errorf("RecordRevocationDelay calls = %+v, want both through the seam", delays)
		}
	})

	t.Run("read while workers draw", func(t *testing.T) {
		s, _, _ := primedServiceSet(t, o, eps, 0)
		const workers, draws = 4, 100
		probeOp := operation(t, s, OpProbe)
		var wg sync.WaitGroup
		for w := range workers {
			wg.Go(func() {
				rng := rand.New(rand.NewSource(int64(w)))
				for range draws {
					_ = probeOp(ctx, rng)
				}
			})
		}
		for range 50 {
			s.ServiceStats(time.Second)
		}
		wg.Wait()
		if n := s.ServiceStats(time.Second).Probes.Overall.Attempted; n != workers*draws {
			t.Errorf("probe samples = %d, want %d", n, workers*draws)
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
		{"x", errors.New("x"), "other"},
		{"no service", ErrNoService, "no-service"},
		{"no service wrapped", fmt.Errorf("probe: %w", ErrNoService), "no-service"},
		{"accepted", probe.ErrAccepted, "accepted"},
		{"accepted wrapped", fmt.Errorf("bad_token: %w", probe.ErrAccepted), "accepted"},
		{"401", gophercloud.ErrUnexpectedResponseCode{Actual: 401}, "unauthorized"},
		// The probe adapter redacts the token from every error it returns; the
		// status stays reachable, so the label is the same.
		{"401 redacted", keystone.RedactToken(gophercloud.ErrUnexpectedResponseCode{Actual: 401, Body: []byte("tok-1")}, "tok-1"), "unauthorized"},
		{"403", gophercloud.ErrUnexpectedResponseCode{Actual: 403}, "forbidden"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.want {
				t.Errorf("Classify(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}

	// The 401 label is the pressure classifier's own: the churn graph's
	// classifier, which Classify delegates to, keeps a 401 as other.
	if got := keystone.ClassifyOperationError(gophercloud.ErrUnexpectedResponseCode{Actual: 401}); got != "other" {
		t.Errorf("keystone.ClassifyOperationError(401) = %q, want other", got)
	}
}

// TestRunWithEngine drives the seven operations through pressure.Run with four
// workers, over tokens whose catalog lists nova and glance: with revoke at
// weight 0 the fake sees no revocation, the pool keeps its size, the result
// has no revoke entry, and the only error kind is the one the fake injects.
// The probe and bad_token draws reach the services fake, and the set's
// per-service samples count exactly the draws the engine counts for each.
func TestRunWithEngine(t *testing.T) {
	weights := map[string]int{OpValidate: 60, OpRevoke: 0, OpEffective: 5, OpGrantChurn: 2, OpIssue: 1, OpProbe: 3, OpBadToken: 2}
	s, f, fs := primedServiceSet(t, fixtureOptions(6, weights), fixtureEndpoints("nova", "glance"), 0)
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

	attempted := make(map[string]int)
	for _, st := range res.Operations.ByType {
		attempted[st.Type] = st.Attempted
	}
	ss := s.ServiceStats(res.Operations.Wall)
	for _, c := range []struct {
		op    string
		kind  string
		stats metrics.Aggregate
	}{{OpProbe, kindProbe, ss.Probes}, {OpBadToken, kindReject, ss.BadTokens}} {
		if attempted[c.op] == 0 {
			t.Errorf("the run drew no %s", c.op)
		}
		if got := len(fs.recorded(c.kind)); got != attempted[c.op] {
			t.Errorf("the services fake saw %d %s calls, want the %d %s draws", got, c.kind, attempted[c.op], c.op)
		}
		if got := c.stats.Overall.Attempted; got != attempted[c.op] {
			t.Errorf("the set's %s samples count %d, want the engine's %d", c.op, got, attempted[c.op])
		}
		for _, st := range c.stats.ByType {
			if st.Type != "nova" && st.Type != "glance" {
				t.Errorf("%s went to %q, which the tokens' catalog does not list", c.op, st.Type)
			}
		}
	}
}

// TestGrantPool checks GrantPool over pressurePlan with user-0004 as the
// churn user: it grants the role to the user of each of the three pool pairs
// on the pair's project, never to the churn user, each grant bounded by
// OpTimeout, and returns their assignment resources; over a fan plan it
// skips the fan user. A failed grant stops further grants and comes back
// with the grants made before it, a cancelled context grants nothing, and at
// most concurrency grants are in flight at once, at least one.
func TestGrantPool(t *testing.T) {
	ctx := context.Background()
	const role, roleID = "reader", "rid-reader"
	// poolGrant is one grant GrantPool should make over pressurePlan.
	type poolGrant struct{ user, userID, project, projectID, domainID string }
	poolGrants := []poolGrant{
		{"user-0001", "uid-1", "proj-0001", "pid-1", "did-1"},
		{"user-0002", "uid-2", "proj-0002", "pid-2", "did-2"},
		{"user-0003", "uid-3", "proj-0001", "pid-1", "did-1"},
	}

	t.Run("grants the pool pairs only", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		o := fixtureOptions(3, churnOn)
		s := buildSet(t, f, o)
		began := time.Now()
		res, err := s.GrantPool(ctx, role, roleID, 2)
		if err != nil {
			t.Fatalf("GrantPool: %v", err)
		}
		want := make(map[poolGrant]bool, len(poolGrants))
		wantRes := make(map[resource.Resource]bool, len(poolGrants))
		for _, g := range poolGrants {
			want[g] = true
			wantRes[keystone.AssignmentResource(keystoneplan.Assignment{User: g.user, Role: role, Project: g.project}, g.userID, g.projectID, g.domainID, roleID)] = true
		}
		calls := f.recorded(kindAssign)
		if len(calls) != len(poolGrants) {
			t.Fatalf("the fake saw %d grants, want one per pool pair (%d)", len(calls), len(poolGrants))
		}
		for _, c := range calls {
			g := poolGrant{c.a.User, c.userID, c.a.Project, c.projectID, c.domainID}
			if !want[g] || c.a.Role != role || c.roleID != roleID {
				t.Errorf("granted %+v the role %q (%s), want a pool pair granted %q (%s)", g, c.a.Role, c.roleID, role, roleID)
			}
			delete(want, g)
			if c.deadline.Before(began.Add(o.OpTimeout)) || c.deadline.After(time.Now().Add(o.OpTimeout)) {
				t.Errorf("the grant of %s ran with deadline %v, want it bounded by the op timeout %s", g.user, c.deadline, o.OpTimeout)
			}
		}
		got := make(map[resource.Resource]bool, len(res))
		for _, r := range res {
			got[r] = true
		}
		if len(res) != len(poolGrants) || !maps.Equal(got, wantRes) {
			t.Errorf("GrantPool returned %+v, want the assignment resources %+v", res, wantRes)
		}
	})

	t.Run("skips the fan users", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		s := buildFanSet(t, f, fanPlan(3, []string{"proj-0001", "proj-0002"}), fixtureOptions(1, churnOff))
		res, err := s.GrantPool(ctx, role, roleID, 2)
		if err != nil || len(res) != 3 {
			t.Fatalf("GrantPool = %d resources, %v; want one for each of the three regular users", len(res), err)
		}
		for _, c := range f.recorded(kindAssign) {
			if c.a.User == "fan-0001" {
				t.Errorf("granted the fan user %+v", c.a)
			}
		}
	})

	t.Run("a failure stops further grants", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		f.failNth(kindAssign, 2, errFake)
		s := buildSet(t, f, fixtureOptions(3, churnOn))
		res, err := s.GrantPool(ctx, role, roleID, 1)
		const want = `granting role "reader" to the pool users (1 of 3 granted): fake: injected failure`
		if err == nil || err.Error() != want || !errors.Is(err, errFake) {
			t.Errorf("GrantPool error = %v, want %q wrapping the failure", err, want)
		}
		if len(res) != 1 {
			t.Errorf("GrantPool returned %d resources, want the one granted before the failure", len(res))
		}
		if calls := f.recorded(kindAssign); len(calls) != 2 {
			t.Errorf("the fake saw %d grants, want none after the failed second", len(calls))
		}
	})

	t.Run("a cancelled context grants nothing", func(t *testing.T) {
		f := newFake(newFakeClock(), time.Hour)
		s := buildSet(t, f, fixtureOptions(3, churnOn))
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		res, err := s.GrantPool(cancelled, role, roleID, 2)
		if !errors.Is(err, context.Canceled) || len(res) != 0 {
			t.Errorf("GrantPool = %d resources, %v; want none and context.Canceled", len(res), err)
		}
		if calls := f.recorded(kindAssign); len(calls) != 0 {
			t.Errorf("the fake saw %d grants, want none", len(calls))
		}
	})

	// In a synctest bubble the fake holds every grant at its gate and the test
	// lets them through one at a time, counting the grants in flight once
	// every goroutine is blocked.
	for _, tt := range []struct{ concurrency, want int }{{2, 2}, {0, 1}} {
		t.Run(fmt.Sprintf("concurrency %d", tt.concurrency), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFake(newFakeClock(), time.Hour)
				f.gateAssigns(t)
				s := buildSet(t, f, fixtureOptions(3, churnOn))
				granted := make(chan error, 1)
				go func() {
					_, err := s.GrantPool(ctx, role, roleID, tt.concurrency)
					granted <- err
				}()
				for released := range len(poolGrants) {
					synctest.Wait()
					if n, want := f.assignsInFlight(), min(tt.want, len(poolGrants)-released); n != want {
						t.Fatalf("%d grants in flight with %d of %d let through, want %d", n, released, len(poolGrants), want)
					}
					f.releaseAssign(t)
				}
				synctest.Wait()
				select {
				case err := <-granted:
					if err != nil {
						t.Errorf("GrantPool: %v", err)
					}
				default:
					t.Fatal("GrantPool did not return after every grant was let through")
				}
				if n := f.maxAssignsInFlight(); n != tt.want {
					t.Errorf("at most %d grants were in flight at once, want %d", n, tt.want)
				}
			})
		})
	}
}
