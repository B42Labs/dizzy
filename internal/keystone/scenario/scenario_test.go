package scenario

import (
	"testing"
	"time"
)

// floatPtr returns a pointer to f, for building the *float64 chaos fields
// (TokenRatio) in test fixtures.
func floatPtr(f float64) *float64 { return &f }

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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			tc.mutate(&s)
			err := s.Validate()
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
