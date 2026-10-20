package scenario

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// floatPtr returns a pointer to f, for building the *float64 chaos fields
// (TokenRatio) in test fixtures.
func floatPtr(f float64) *float64 { return &f }

// intPtr returns a pointer to n, for building the *int pressure fields
// (ChurnUsers and the Mix weights) in test fixtures.
func intPtr(n int) *int { return &n }

// smallScenario is the compact scenario backing the golden test; it mirrors the
// shipped scenarios/keystone/small.yaml so the profile test can tie to it.
func smallScenario() Scenario {
	return Scenario{
		Name: "small",
		Seed: 42,
		Resources: Resources{
			Domains:  1,
			Roles:    2,
			Projects: 5,
			Users:    10,
		},
		Distribution: Distribution{
			ProjectsPerDomain:           Range{Min: 1, Max: 3},
			AssignmentsPerUser:          Range{Min: 1, Max: 3},
			DomainScopedAssignmentRatio: 0.1,
			UsersIssuingTokensRatio:     0.5,
		},
		Chaos: &Chaos{
			Duration:   Duration(5 * time.Minute),
			Interval:   Interval{Min: Duration(200 * time.Millisecond), Max: Duration(3 * time.Second)},
			Parallel:   Parallel{Max: 4},
			ChurnRatio: 0.5,
			TargetFill: 0.8,
			TokenRatio: floatPtr(0.3),
		},
		Pressure: &Pressure{
			Duration: Duration(5 * time.Minute),
			Workers:  8,
			Tokens:   20,
		},
	}
}

func TestParseRejectsUnknownKey(t *testing.T) {
	// A stray key (here a cinder-only "volume_size_gib" distribution) must fail
	// strict unmarshal loudly rather than being silently ignored.
	_, err := Parse([]byte("name: x\nresources:\n  domains: 1\ndistribution:\n  volume_size_gib: { min: 1, max: 1 }\n"))
	if err == nil {
		t.Fatal("Parse of a scenario with an unknown key: expected an error, got nil")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Scenario)
		wantErr bool
		wantMsg string // the exact error, when set; implies wantErr
	}{
		{name: "valid", mutate: func(*Scenario) {}},
		{name: "empty name", mutate: func(s *Scenario) { s.Name = "" }, wantErr: true},
		{name: "negative domains", mutate: func(s *Scenario) { s.Resources.Domains = -1 }, wantErr: true},
		{name: "negative projects", mutate: func(s *Scenario) { s.Resources.Projects = -1 }, wantErr: true},
		{name: "domains zero with users rejected", mutate: func(s *Scenario) { s.Resources.Domains = 0 }, wantErr: true},
		{
			name:   "domains zero allowed with no projects or users",
			mutate: func(s *Scenario) { s.Resources.Domains = 0; s.Resources.Projects = 0; s.Resources.Users = 0 },
		},
		{name: "roles zero with assignments rejected", mutate: func(s *Scenario) { s.Resources.Roles = 0 }, wantErr: true},
		{
			name:   "roles zero allowed without assignments",
			mutate: func(s *Scenario) { s.Resources.Roles = 0; s.Distribution.AssignmentsPerUser = Range{Min: 0, Max: 0} },
		},
		{
			name:    "projects_per_domain min zero with projects rejected",
			mutate:  func(s *Scenario) { s.Distribution.ProjectsPerDomain.Min = 0 },
			wantErr: true,
		},
		{name: "assignments range min above max", mutate: func(s *Scenario) { s.Distribution.AssignmentsPerUser = Range{Min: 5, Max: 1} }, wantErr: true},
		{name: "domain-scoped ratio above one", mutate: func(s *Scenario) { s.Distribution.DomainScopedAssignmentRatio = 1.5 }, wantErr: true},
		{name: "tokens ratio below zero", mutate: func(s *Scenario) { s.Distribution.UsersIssuingTokensRatio = -0.1 }, wantErr: true},
		{
			// Each dimension is legal, but users × assignments_per_user.max would
			// demand an oversized assignment preallocation.
			name: "users times assignment max exceeds total limit",
			mutate: func(s *Scenario) {
				s.Resources.Users = maxCount
				s.Distribution.AssignmentsPerUser = Range{Min: 0, Max: maxCount}
			},
			wantErr: true,
		},
		{
			// Without fan users fan_assignments is never drawn from, so the
			// total and its message name the regular product alone.
			name: "total without fan users ignores fan_assignments",
			mutate: func(s *Scenario) {
				s.Resources.Users = maxCount
				s.Distribution.AssignmentsPerUser = Range{Min: 0, Max: 2}
				s.Distribution.FanAssignments = Range{Min: 1, Max: maxCount}
			},
			wantMsg: "resources.users (1000000) × distribution.assignments_per_user.max (2) = 2000000 exceeds the total assignment limit of 1000000",
		},
		{
			name: "fan users",
			mutate: func(s *Scenario) {
				s.Resources.Projects = 4
				s.Distribution.FanUsers = 2
				s.Distribution.FanAssignments = Range{Min: 3, Max: 4}
			},
		},
		{
			name:   "fan_assignments unchecked against projects without fan users",
			mutate: func(s *Scenario) { s.Distribution.FanAssignments = Range{Min: 0, Max: 99} },
		},
		{
			name:    "negative fan users",
			mutate:  func(s *Scenario) { s.Distribution.FanUsers = -1 },
			wantMsg: "distribution.fan_users must not be negative, got -1",
		},
		{
			name:    "fan users above the limit",
			mutate:  func(s *Scenario) { s.Distribution.FanUsers = maxCount + 1 },
			wantMsg: "distribution.fan_users exceeds the limit of 1000000, got 1000001",
		},
		{
			name:    "fan_assignments min above max",
			mutate:  func(s *Scenario) { s.Distribution.FanAssignments = Range{Min: 3, Max: 2} },
			wantMsg: "distribution.fan_assignments.min (3) must not exceed distribution.fan_assignments.max (2)",
		},
		{
			name: "fan users without a domain",
			mutate: func(s *Scenario) {
				s.Resources.Domains, s.Resources.Projects, s.Resources.Users = 0, 0, 0
				s.Distribution.FanUsers = 1
				s.Distribution.FanAssignments = Range{Min: 1, Max: 1}
			},
			wantMsg: "resources.domains must be at least 1 when projects or users are created, got 0",
		},
		{
			// The regular users draw no grant, so only the fan user needs a
			// role.
			name: "fan users without a role",
			mutate: func(s *Scenario) {
				s.Resources.Roles = 0
				s.Distribution.AssignmentsPerUser = Range{Min: 0, Max: 0}
				s.Distribution.FanUsers = 1
				s.Distribution.FanAssignments = Range{Min: 1, Max: 2}
			},
			wantMsg: "resources.roles must be at least 1 when users draw assignments, got 0",
		},
		{
			name: "fan_assignments min zero with fan users",
			mutate: func(s *Scenario) {
				s.Distribution.FanUsers = 1
				s.Distribution.FanAssignments = Range{Min: 0, Max: 2}
			},
			wantMsg: "distribution.fan_assignments.min must be at least 1 when fan users are created, got 0",
		},
		{
			name: "fan_assignments max above the projects",
			mutate: func(s *Scenario) {
				s.Distribution.FanUsers = 1
				s.Distribution.FanAssignments = Range{Min: 1, Max: s.Resources.Projects + 1}
			},
			wantMsg: "distribution.fan_assignments.max (6) exceeds resources.projects (5); a fan user holds one grant per distinct project",
		},
		{
			// 10 users × 3 plus 1 fan user × 999970 is exactly the limit.
			name: "fan users at the total limit",
			mutate: func(s *Scenario) {
				s.Resources.Projects = maxCount - 30
				s.Distribution.FanUsers = 1
				s.Distribution.FanAssignments = Range{Min: 1, Max: maxCount - 30}
			},
		},
		{
			name: "fan users exceed the total limit",
			mutate: func(s *Scenario) {
				s.Resources.Projects = maxCount
				s.Distribution.FanUsers = 1
				s.Distribution.FanAssignments = Range{Min: 1, Max: maxCount}
			},
			wantMsg: "resources.users (10) × distribution.assignments_per_user.max (3) + distribution.fan_users (1) × distribution.fan_assignments.max (1000000) = 1000030 exceeds the total assignment limit of 1000000",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			tc.mutate(&s)
			err := s.Validate()
			if tc.wantMsg != "" {
				if err == nil || err.Error() != tc.wantMsg {
					t.Fatalf("Validate() = %v, want %q", err, tc.wantMsg)
				}
				return
			}
			if tc.wantErr && err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestSet(t *testing.T) {
	// Every documented key parses into the matching field.
	keys := []struct {
		key   string
		value string
		check func(Scenario) bool
	}{
		{"seed", "7", func(s Scenario) bool { return s.Seed == 7 }},
		{"resources.domains", "3", func(s Scenario) bool { return s.Resources.Domains == 3 }},
		{"resources.roles", "4", func(s Scenario) bool { return s.Resources.Roles == 4 }},
		{"resources.projects", "9", func(s Scenario) bool { return s.Resources.Projects == 9 }},
		{"resources.users", "11", func(s Scenario) bool { return s.Resources.Users == 11 }},
		{"distribution.projects_per_domain.min", "2", func(s Scenario) bool { return s.Distribution.ProjectsPerDomain.Min == 2 }},
		{"distribution.projects_per_domain.max", "6", func(s Scenario) bool { return s.Distribution.ProjectsPerDomain.Max == 6 }},
		{"distribution.assignments_per_user.min", "1", func(s Scenario) bool { return s.Distribution.AssignmentsPerUser.Min == 1 }},
		{"distribution.assignments_per_user.max", "5", func(s Scenario) bool { return s.Distribution.AssignmentsPerUser.Max == 5 }},
		{"distribution.domain_scoped_assignment_ratio", "0.25", func(s Scenario) bool { return s.Distribution.DomainScopedAssignmentRatio == 0.25 }},
		{"distribution.users_issuing_tokens_ratio", "0.75", func(s Scenario) bool { return s.Distribution.UsersIssuingTokensRatio == 0.75 }},
		{"distribution.fan_users", "1", func(s Scenario) bool { return s.Distribution.FanUsers == 1 }},
		{"distribution.fan_assignments.min", "2", func(s Scenario) bool { return s.Distribution.FanAssignments.Min == 2 }},
		{"distribution.fan_assignments.max", "3", func(s Scenario) bool { return s.Distribution.FanAssignments.Max == 3 }},
	}
	for _, k := range keys {
		t.Run(k.key, func(t *testing.T) {
			s := smallScenario()
			if err := s.Set(k.key, k.value); err != nil {
				t.Fatalf("Set(%q,%q) = %v", k.key, k.value, err)
			}
			if !k.check(s) {
				t.Errorf("Set(%q,%q) did not apply", k.key, k.value)
			}
		})
	}
}

// TestParseFanDistribution confirms the fan keys of the distribution block
// parse into their fields and validate when the fan users' max fits the
// projects, and that an unknown key inside fan_assignments fails strict
// unmarshal loudly.
func TestParseFanDistribution(t *testing.T) {
	yaml := `name: f
seed: 1
resources:
  domains: 1
  roles: 2
  projects: 4
  users: 2
distribution:
  projects_per_domain: { min: 1, max: 4 }
  assignments_per_user: { min: 1, max: 1 }
  fan_users: 2
  fan_assignments: { min: 3, max: 4 }
`
	s, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Distribution.FanUsers != 2 {
		t.Errorf("fan_users = %d, want 2", s.Distribution.FanUsers)
	}
	if want := (Range{Min: 3, Max: 4}); s.Distribution.FanAssignments != want {
		t.Errorf("fan_assignments = %+v, want %+v", s.Distribution.FanAssignments, want)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for fan_assignments.max equal to resources.projects", err)
	}

	if _, err := Parse([]byte("name: f\ndistribution:\n  fan_assignments: { min: 1, max: 1, nope: 1 }\n")); err == nil {
		t.Error("Parse of a fan_assignments block with an unknown key: expected an error, got nil")
	}
}

// TestParseChaosBlock confirms a full chaos block — including the identity-
// specific token_ratio — parses and validates, that a scenario with no chaos
// block is valid, and that an unknown chaos key fails strict unmarshal loudly.
func TestParseChaosBlock(t *testing.T) {
	yaml := `name: c
seed: 1
resources:
  domains: 1
  roles: 1
  projects: 1
  users: 1
distribution:
  projects_per_domain: { min: 1, max: 1 }
  assignments_per_user: { min: 1, max: 1 }
chaos:
  duration: 10m
  interval: { min: 200ms, max: 3s }
  parallel: { max: 4 }
  churn_ratio: 0.5
  target_fill: 0.8
  token_ratio: 0.3
`
	s, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Chaos == nil {
		t.Fatal("chaos block did not parse into a non-nil Chaos")
	}
	if s.Chaos.TokenRatio == nil || *s.Chaos.TokenRatio != 0.3 {
		t.Errorf("token_ratio = %v, want 0.3", s.Chaos.TokenRatio)
	}
	if s.Chaos.Duration != Duration(10*time.Minute) {
		t.Errorf("duration = %s, want 10m", time.Duration(s.Chaos.Duration))
	}
	if err := s.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for a well-formed chaos block", err)
	}

	// A scenario with no chaos block validates the same as before.
	noChaos := smallScenario()
	noChaos.Chaos = nil
	if err := noChaos.Validate(); err != nil {
		t.Errorf("Validate() with no chaos block = %v, want nil", err)
	}

	// An unknown chaos key must fail strict unmarshal, not be silently ignored.
	if _, err := Parse([]byte("name: c\nresources:\n  domains: 1\nchaos:\n  nope: 1\n")); err == nil {
		t.Error("Parse of a chaos block with an unknown key: expected an error, got nil")
	}
}

// TestValidateChaosBlockRejects covers each way a chaos block can be
// semantically invalid, one violated rule at a time.
func TestValidateChaosBlockRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Chaos)
	}{
		{"negative duration", func(c *Chaos) { c.Duration = Duration(-time.Second) }},
		{"negative interval min", func(c *Chaos) { c.Interval.Min = Duration(-time.Millisecond) }},
		{"interval min above max", func(c *Chaos) { c.Interval.Min = c.Interval.Max + Duration(time.Second) }},
		{"negative parallel max", func(c *Chaos) { c.Parallel.Max = -1 }},
		{"churn_ratio above one", func(c *Chaos) { c.ChurnRatio = 1.5 }},
		{"target_fill below zero", func(c *Chaos) { c.TargetFill = -0.1 }},
		{"token_ratio above one", func(c *Chaos) { c.TokenRatio = floatPtr(1.5) }},
		{"token_ratio below zero", func(c *Chaos) { c.TokenRatio = floatPtr(-0.1) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			tc.mutate(s.Chaos)
			if err := s.Validate(); err == nil {
				t.Fatal("Validate() = nil, want error")
			}
		})
	}
}

func TestSetErrors(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"unknown key", "resources.nope", "1"},
		{"non-integer projects", "resources.projects", "many"},
		{"non-number ratio", "distribution.domain_scoped_assignment_ratio", "half"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			if err := s.Set(tc.key, tc.value); err == nil {
				t.Errorf("Set(%q,%q) = nil, want error", tc.key, tc.value)
			}
		})
	}
}

// TestChaosBucketWidth covers the chaos block's bucket_width key: a duration
// string parses, an absent or zero key reads as unset, a negative width fails
// validation, and a malformed one fails parsing.
func TestChaosBucketWidth(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    time.Duration
		wantErr string
	}{
		{name: "set", line: "  bucket_width: 30m\n", want: 30 * time.Minute},
		{name: "absent", line: ""},
		{name: "zero is unset", line: "  bucket_width: 0\n"},
		{name: "negative", line: "  bucket_width: -1m\n", wantErr: "chaos.bucket_width must not be negative"},
		{name: "malformed", line: "  bucket_width: soon\n", wantErr: `parsing duration "soon"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := Parse([]byte("name: x\nchaos:\n  duration: 1m\n" + tc.line))
			if err == nil {
				s := smallScenario()
				s.Chaos = parsed.Chaos
				err = s.Validate()
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Parse and Validate = %v, want an error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse and Validate = %v, want nil", err)
			}
			if got := time.Duration(parsed.Chaos.BucketWidth); got != tc.want {
				t.Errorf("chaos.bucket_width = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestParsePressureBlock confirms a full pressure block, including every mix
// weight, the propagation block that later work delivers and two stages, one
// without a duration, parses into the matching fields and validates; that a
// scenario with no pressure block is valid; and that an unknown key at any
// level of the block fails strict unmarshal loudly. Each weight carries a
// distinct value so a swapped YAML tag shows up as a mismatch.
func TestParsePressureBlock(t *testing.T) {
	yaml := `name: p
seed: 1
resources:
  domains: 1
  roles: 1
  projects: 1000
  users: 2
distribution:
  projects_per_domain: { min: 1, max: 1 }
  assignments_per_user: { min: 1, max: 1 }
  fan_users: 1
  fan_assignments: { min: 500, max: 500 }
pressure:
  duration: 10m
  workers: 4
  tokens: 50
  churn_users: 1
  bucket_width: 2m
  mix:
    validate: 60
    probe: 3
    effective: 5
    revoke: 2
    grant_churn: 4
    issue: 1
    bad_token: 7
  propagation: { every: 30s }
  stages:
    - fan_assignments: 500
    - { fan_assignments: 1000, duration: 2m }
`
	s, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := &Pressure{
		Duration:   Duration(10 * time.Minute),
		Workers:    4,
		Tokens:     50,
		ChurnUsers: intPtr(1),
		Mix: &Mix{
			Validate:   intPtr(60),
			Probe:      intPtr(3),
			Effective:  intPtr(5),
			Revoke:     intPtr(2),
			GrantChurn: intPtr(4),
			Issue:      intPtr(1),
			BadToken:   intPtr(7),
		},
		Propagation: &Propagation{Every: Duration(30 * time.Second)},
		BucketWidth: Duration(2 * time.Minute),
		Stages: []Stage{
			{FanAssignments: intPtr(500)},
			{FanAssignments: intPtr(1000), Duration: Duration(2 * time.Minute)},
		},
	}
	if !reflect.DeepEqual(s.Pressure, want) {
		t.Errorf("pressure block = %s, want %s", describePressure(s.Pressure), describePressure(want))
	}
	if err := s.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for a well-formed pressure block", err)
	}

	// A scenario with no pressure block validates the same as before.
	noPressure := smallScenario()
	noPressure.Pressure = nil
	if err := noPressure.Validate(); err != nil {
		t.Errorf("Validate() with no pressure block = %v, want nil", err)
	}

	// An unknown key in the block or in one of its nested blocks must fail
	// strict unmarshal, not be silently ignored.
	for _, block := range []string{
		"{ nope: 1 }",
		"{ mix: { nope: 1 } }",
		"{ propagation: { nope: 1s } }",
		"{ stages: [{ nope: 1 }] }",
	} {
		if _, err := Parse([]byte("name: p\npressure: " + block + "\n")); err == nil {
			t.Errorf("Parse of pressure: %s: expected an error, got nil", block)
		}
	}
}

// TestValidatePressureBlock covers each way a pressure block can be
// semantically invalid, one violated rule at a time, each failing with the
// message that names its key; and the values that read as unset or as an
// operation switched off, which must validate.
func TestValidatePressureBlock(t *testing.T) {
	tests := []struct {
		name    string
		block   string
		wantErr string
	}{
		{name: "empty block", block: "{}"},
		{name: "zero values are unset", block: "{ duration: 0, workers: 0, tokens: 0, bucket_width: 0 }"},
		{name: "zero churn users", block: "{ churn_users: 0 }"},
		{
			name:  "every weight zero",
			block: "{ mix: { validate: 0, probe: 0, effective: 0, revoke: 0, grant_churn: 0, issue: 0, bad_token: 0 } }",
		},
		{name: "empty mix", block: "{ mix: {} }"},
		{name: "zero propagation interval", block: "{ propagation: { every: 0s } }"},
		{name: "negative workers", block: "{ workers: -1 }", wantErr: "pressure.workers must not be negative, got -1"},
		{name: "negative tokens", block: "{ tokens: -1 }", wantErr: "pressure.tokens must not be negative, got -1"},
		{name: "negative churn_users", block: "{ churn_users: -1 }", wantErr: "pressure.churn_users must not be negative, got -1"},
		{name: "negative duration", block: "{ duration: -1m }", wantErr: "pressure.duration must not be negative, got -1m0s"},
		{name: "negative bucket_width", block: "{ bucket_width: -1m }", wantErr: "pressure.bucket_width must not be negative, got -1m0s"},
		{name: "negative validate weight", block: "{ mix: { validate: -1 } }", wantErr: "pressure.mix.validate must not be negative, got -1"},
		{name: "negative probe weight", block: "{ mix: { probe: -2 } }", wantErr: "pressure.mix.probe must not be negative, got -2"},
		{name: "negative effective weight", block: "{ mix: { effective: -3 } }", wantErr: "pressure.mix.effective must not be negative, got -3"},
		{name: "negative revoke weight", block: "{ mix: { revoke: -4 } }", wantErr: "pressure.mix.revoke must not be negative, got -4"},
		{name: "negative grant_churn weight", block: "{ mix: { grant_churn: -5 } }", wantErr: "pressure.mix.grant_churn must not be negative, got -5"},
		{name: "negative issue weight", block: "{ mix: { issue: -6 } }", wantErr: "pressure.mix.issue must not be negative, got -6"},
		{name: "negative bad_token weight", block: "{ mix: { bad_token: -7 } }", wantErr: "pressure.mix.bad_token must not be negative, got -7"},
		{
			name:    "negative propagation interval",
			block:   "{ propagation: { every: -1s } }",
			wantErr: "pressure.propagation.every must not be negative, got -1s",
		},
		{name: "empty stages", block: "{ stages: [] }"},
		{name: "stages without an override", block: "{ stages: [{ duration: 1m }, {}] }"},
		{
			name:    "negative stage duration",
			block:   "{ stages: [{ duration: -1m }] }",
			wantErr: "pressure.stages[0].duration must not be negative, got -1m0s",
		},
		{
			name:    "negative duration of a later stage",
			block:   "{ stages: [{ duration: 1m }, { duration: -1s }] }",
			wantErr: "pressure.stages[1].duration must not be negative, got -1s",
		},
		{
			name:    "zero stage fan_assignments",
			block:   "{ stages: [{ fan_assignments: 0 }] }",
			wantErr: "pressure.stages[0].fan_assignments must be at least 1, got 0",
		},
		{
			name:    "negative stage fan_assignments",
			block:   "{ stages: [{ fan_assignments: -2 }] }",
			wantErr: "pressure.stages[0].fan_assignments must be at least 1, got -2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := Parse([]byte("name: x\npressure: " + tc.block + "\n"))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if parsed.Pressure == nil {
				t.Fatal("pressure block did not parse into a non-nil Pressure")
			}
			s := smallScenario()
			s.Pressure = parsed.Pressure
			err = s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Validate() = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateStages covers the checks of the stages' fan_assignments against
// the distribution, one violated rule at a time with its exact message and
// 0-based stage index, and the stage lists that validate. A stage without
// fan_assignments is skipped by every check, so a later stage compares with
// the last stage that set one.
func TestValidateStages(t *testing.T) {
	// fan gives smallScenario's five projects one fan user holding two or
	// three grants.
	fan := func(s *Scenario) {
		s.Distribution.FanUsers = 1
		s.Distribution.FanAssignments = Range{Min: 2, Max: 3}
	}
	tests := []struct {
		name    string
		mutate  func(*Scenario)
		stages  string
		wantErr string
	}{
		{
			name:    "fan_assignments without fan users",
			stages:  "[{ fan_assignments: 5 }]",
			wantErr: "pressure.stages[0] sets fan_assignments but distribution.fan_users is 0",
		},
		{
			name:    "fan_users checked before the projects",
			stages:  "[{ duration: 1m }, { fan_assignments: 6 }]",
			wantErr: "pressure.stages[1] sets fan_assignments but distribution.fan_users is 0",
		},
		{name: "durations without fan users", stages: "[{ duration: 1m }, {}]"},
		{
			name:    "above the projects",
			mutate:  fan,
			stages:  "[{ fan_assignments: 6 }]",
			wantErr: "pressure.stages[0].fan_assignments (6) exceeds resources.projects (5)",
		},
		{
			name:    "a later stage above the projects",
			mutate:  fan,
			stages:  "[{ fan_assignments: 3 }, { fan_assignments: 6 }]",
			wantErr: "pressure.stages[1].fan_assignments (6) exceeds resources.projects (5)",
		},
		{
			name:    "first below the fan max",
			mutate:  fan,
			stages:  "[{ fan_assignments: 2 }]",
			wantErr: "pressure.stages[0].fan_assignments (2) is below distribution.fan_assignments.max (3), which a fan user may already hold",
		},
		{
			name:    "first override after a window below the fan max",
			mutate:  fan,
			stages:  "[{ duration: 1m }, { fan_assignments: 2 }]",
			wantErr: "pressure.stages[1].fan_assignments (2) is below distribution.fan_assignments.max (3), which a fan user may already hold",
		},
		{
			name:    "a later stage below the one before",
			mutate:  fan,
			stages:  "[{ fan_assignments: 4 }, { fan_assignments: 3 }]",
			wantErr: "pressure.stages[1].fan_assignments (3) is below stage 0's (4); a stage never removes assignments",
		},
		{
			name:    "a later stage below the last override across a window",
			mutate:  fan,
			stages:  "[{ fan_assignments: 4 }, { duration: 1m }, { fan_assignments: 3 }]",
			wantErr: "pressure.stages[2].fan_assignments (3) is below stage 0's (4); a stage never removes assignments",
		},
		{name: "a window and then the fan max", mutate: fan, stages: "[{ duration: 1m }, { fan_assignments: 3 }]"},
		{name: "non-decreasing overrides", mutate: fan, stages: "[{ fan_assignments: 3 }, { fan_assignments: 3 }, {}, { fan_assignments: 5 }]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := Parse([]byte("name: x\npressure: { stages: " + tc.stages + " }\n"))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			s := smallScenario()
			if tc.mutate != nil {
				tc.mutate(&s)
			}
			s.Pressure = parsed.Pressure
			err = s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Validate() = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// describePressure renders a pressure block with its durations as duration
// strings and its pointers dereferenced, so a failed comparison prints values
// rather than addresses.
func describePressure(p *Pressure) string {
	if p == nil {
		return "<nil>"
	}
	ptr := func(n *int) string {
		if n == nil {
			return "<nil>"
		}
		return strconv.Itoa(*n)
	}
	s := fmt.Sprintf("{duration: %s, workers: %d, tokens: %d, churn_users: %s, bucket_width: %s",
		time.Duration(p.Duration), p.Workers, p.Tokens, ptr(p.ChurnUsers), time.Duration(p.BucketWidth))
	if m := p.Mix; m != nil {
		s += fmt.Sprintf(", mix: {validate: %s, probe: %s, effective: %s, revoke: %s, grant_churn: %s, issue: %s, bad_token: %s}",
			ptr(m.Validate), ptr(m.Probe), ptr(m.Effective), ptr(m.Revoke), ptr(m.GrantChurn), ptr(m.Issue), ptr(m.BadToken))
	}
	if p.Propagation != nil {
		s += fmt.Sprintf(", propagation: {every: %s}", time.Duration(p.Propagation.Every))
	}
	if len(p.Stages) > 0 {
		stages := make([]string, len(p.Stages))
		for i, st := range p.Stages {
			stages[i] = fmt.Sprintf("{fan_assignments: %s, duration: %s}", ptr(st.FanAssignments), time.Duration(st.Duration))
		}
		s += ", stages: [" + strings.Join(stages, ", ") + "]"
	}
	return s + "}"
}
