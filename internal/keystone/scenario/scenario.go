// Package scenario defines the human-authored YAML scenario format for Keystone
// (counts, ranges, ratios, seed) and the deterministic generator that expands a
// scenario plus its seed into a fully-enumerated plan. The same scenario and
// seed always yield a byte-identical plan. Keystone gets its own scenario schema
// rather than sharing the Neutron or Cinder one: the services share no
// resources, and separate schemas keep yaml.UnmarshalStrict failing loudly on a
// typo in any service's file.
package scenario

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

// Scenario is the parametrized description of a desired Keystone workload. It is
// parsed from YAML, validated, optionally overridden via Set, and expanded into
// a plan by Generate.
type Scenario struct {
	Name         string       `yaml:"name"`
	Seed         int64        `yaml:"seed"`
	Resources    Resources    `yaml:"resources"`
	Distribution Distribution `yaml:"distribution"`
	// Chaos, when present, configures the random churn/soak mode (the keystone
	// chaos subcommand). It is a pointer so an absent block stays nil and apply
	// and generate ignore it entirely. The temporal knobs here are an upper bound
	// the chaos CLI flags override; the surrounding scenario is the spatial
	// envelope. It mirrors the Neutron and Cinder scenario chaos blocks,
	// duplicated by design so a typo in any service's scenario keeps failing
	// loudly.
	Chaos *Chaos `yaml:"chaos,omitempty"`
}

// Chaos holds the churn-mode knobs read from a scenario's chaos block. Every
// field has a corresponding chaos CLI flag that overrides it; an unset field
// falls back to the command's default. Duration is intentionally not required
// here (a flag may supply it); the merged "duration must be set" check lives in
// the command. TokenRatio is the identity-specific knob: the per-step
// probability of issuing a token as a live user with a live project assignment.
// It is a pointer so an omitted key (nil) falls back to the command's default
// while an explicit token_ratio: 0 disables token issues — the one knob where 0
// reads as an on/off switch rather than a degenerate value, mirroring Cinder's
// resize_ratio.
type Chaos struct {
	Duration   Duration `yaml:"duration"`
	Interval   Interval `yaml:"interval"`
	Parallel   Parallel `yaml:"parallel"`
	ChurnRatio float64  `yaml:"churn_ratio"`
	TargetFill float64  `yaml:"target_fill"`
	TokenRatio *float64 `yaml:"token_ratio"`
}

// Interval is the random delay range between scheduled churn actions.
type Interval struct {
	Min Duration `yaml:"min"`
	Max Duration `yaml:"max"`
}

// Parallel bounds the fan-out of a churn tick: the actual number of actions
// launched per tick is drawn randomly in [1, Max].
type Parallel struct {
	Max int `yaml:"max"`
}

// Duration is a time.Duration that decodes from a Go duration string (e.g.
// "30m", "200ms") under strict YAML, since yaml.v2 has no native duration
// decoding.
type Duration time.Duration

// UnmarshalYAML decodes a Go duration string into a Duration.
func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parsing duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Resources holds the fixed counts of top-level resources to create. Domains and
// roles are admin-only knobs: in domain-manager mode domains are forced to the
// single in-scope domain and roles are ignored (existing roles reused).
type Resources struct {
	Domains  int `yaml:"domains"`
	Roles    int `yaml:"roles"`
	Projects int `yaml:"projects"`
	Users    int `yaml:"users"`
}

// Distribution holds the ranges and ratios that shape how projects are dealt
// across domains, how many grants each user draws, and which grants and users
// are domain-scoped or issue tokens.
type Distribution struct {
	// ProjectsPerDomain is the clustering granularity of the round-robin deal of
	// the fixed project count across domains, not a hard per-domain cap.
	ProjectsPerDomain Range `yaml:"projects_per_domain"`
	// AssignmentsPerUser is the number of (user, target, role) grants drawn per
	// user.
	AssignmentsPerUser Range `yaml:"assignments_per_user"`
	// DomainScopedAssignmentRatio is the fraction of grants that target the
	// user's domain rather than a project.
	DomainScopedAssignmentRatio float64 `yaml:"domain_scoped_assignment_ratio"`
	// UsersIssuingTokensRatio is the fraction of users that authenticate for a
	// project-scoped token.
	UsersIssuingTokensRatio float64 `yaml:"users_issuing_tokens_ratio"`
}

// Range is an inclusive integer interval drawn from during generation.
type Range struct {
	Min int `yaml:"min"`
	Max int `yaml:"max"`
}

// Parse decodes a scenario from YAML. Unknown keys are rejected so that a typo
// in a scenario file fails loudly instead of being silently ignored. It does no
// semantic validation; call Validate for that.
func Parse(data []byte) (Scenario, error) {
	var s Scenario
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return Scenario{}, fmt.Errorf("parsing scenario: %w", err)
	}
	return s, nil
}

// maxCount caps every resource count and range maximum. It bounds each
// individual slice preallocation and guards randRange's interval arithmetic
// against integer overflow, mirroring the neutron and cinder scenario caps. It
// does not by itself bound the assignment slice, whose length is bounded by
// users × assignments_per_user.max — Validate caps that product separately.
const maxCount = 1_000_000

// Validate checks the scenario for semantic consistency, returning an
// actionable error that names the offending field.
func (s Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name must not be empty")
	}

	for _, c := range []struct {
		key string
		n   int
	}{
		{"resources.domains", s.Resources.Domains},
		{"resources.roles", s.Resources.Roles},
		{"resources.projects", s.Resources.Projects},
		{"resources.users", s.Resources.Users},
	} {
		if c.n < 0 {
			return fmt.Errorf("%s must not be negative, got %d", c.key, c.n)
		}
		if c.n > maxCount {
			return fmt.Errorf("%s exceeds the limit of %d, got %d", c.key, maxCount, c.n)
		}
	}

	for _, c := range []struct {
		key string
		r   Range
	}{
		{"distribution.projects_per_domain", s.Distribution.ProjectsPerDomain},
		{"distribution.assignments_per_user", s.Distribution.AssignmentsPerUser},
	} {
		if err := validateRange(c.key, c.r); err != nil {
			return err
		}
	}

	for _, c := range []struct {
		key string
		v   float64
	}{
		{"distribution.domain_scoped_assignment_ratio", s.Distribution.DomainScopedAssignmentRatio},
		{"distribution.users_issuing_tokens_ratio", s.Distribution.UsersIssuingTokensRatio},
	} {
		if c.v < 0 || c.v > 1 {
			return fmt.Errorf("%s must be between 0 and 1, got %v", c.key, c.v)
		}
	}

	// Projects and users need at least one domain to hang off.
	if (s.Resources.Projects > 0 || s.Resources.Users > 0) && s.Resources.Domains < 1 {
		return fmt.Errorf("resources.domains must be at least 1 when projects or users are created, got %d", s.Resources.Domains)
	}
	// Grants need at least one role to draw from.
	if s.Resources.Users > 0 && s.Distribution.AssignmentsPerUser.Max > 0 && s.Resources.Roles < 1 {
		return fmt.Errorf("resources.roles must be at least 1 when users draw assignments, got %d", s.Resources.Roles)
	}
	// The round-robin deal must make progress, so each turn places at least one
	// project.
	if s.Resources.Projects > 0 && s.Distribution.ProjectsPerDomain.Min < 1 {
		return fmt.Errorf("distribution.projects_per_domain.min must be at least 1 when projects are created, got %d", s.Distribution.ProjectsPerDomain.Min)
	}

	// The assignment slice is preallocated to users × assignments_per_user.max, a
	// product neither per-dimension cap bounds; cap it too. Compute in int64 so
	// the product cannot overflow int on a 32-bit platform.
	if total := int64(s.Resources.Users) * int64(s.Distribution.AssignmentsPerUser.Max); total > maxCount {
		return fmt.Errorf("resources.users (%d) × distribution.assignments_per_user.max (%d) = %d exceeds the total assignment limit of %d",
			s.Resources.Users, s.Distribution.AssignmentsPerUser.Max, total, maxCount)
	}

	if err := s.Chaos.validate(); err != nil {
		return err
	}

	return nil
}

// validate checks the chaos block for semantic consistency. A nil receiver (no
// chaos block) is valid. Duration is not required here because a CLI flag may
// supply it; only the values that are present must be sane.
func (c *Chaos) validate() error {
	if c == nil {
		return nil
	}
	if c.Duration < 0 {
		return fmt.Errorf("chaos.duration must not be negative, got %s", time.Duration(c.Duration))
	}
	if c.Interval.Min < 0 {
		return fmt.Errorf("chaos.interval.min must not be negative, got %s", time.Duration(c.Interval.Min))
	}
	if c.Interval.Min > c.Interval.Max {
		return fmt.Errorf("chaos.interval.min (%s) must not exceed chaos.interval.max (%s)", time.Duration(c.Interval.Min), time.Duration(c.Interval.Max))
	}
	if c.Parallel.Max < 0 {
		return fmt.Errorf("chaos.parallel.max must not be negative, got %d", c.Parallel.Max)
	}
	if c.ChurnRatio < 0 || c.ChurnRatio > 1 {
		return fmt.Errorf("chaos.churn_ratio must be between 0 and 1, got %v", c.ChurnRatio)
	}
	if c.TargetFill < 0 || c.TargetFill > 1 {
		return fmt.Errorf("chaos.target_fill must be between 0 and 1, got %v", c.TargetFill)
	}
	if c.TokenRatio != nil && (*c.TokenRatio < 0 || *c.TokenRatio > 1) {
		return fmt.Errorf("chaos.token_ratio must be between 0 and 1, got %v", *c.TokenRatio)
	}
	return nil
}

// Set applies a single dotted-key override of the form key=value, matching the
// documented scenario fields. It returns an error for an unknown key or a value
// that does not parse to the field's type.
func (s *Scenario) Set(key, value string) error {
	switch key {
	case "seed":
		return setInt64(&s.Seed, key, value)
	case "resources.domains":
		return setInt(&s.Resources.Domains, key, value)
	case "resources.roles":
		return setInt(&s.Resources.Roles, key, value)
	case "resources.projects":
		return setInt(&s.Resources.Projects, key, value)
	case "resources.users":
		return setInt(&s.Resources.Users, key, value)
	case "distribution.projects_per_domain.min":
		return setInt(&s.Distribution.ProjectsPerDomain.Min, key, value)
	case "distribution.projects_per_domain.max":
		return setInt(&s.Distribution.ProjectsPerDomain.Max, key, value)
	case "distribution.assignments_per_user.min":
		return setInt(&s.Distribution.AssignmentsPerUser.Min, key, value)
	case "distribution.assignments_per_user.max":
		return setInt(&s.Distribution.AssignmentsPerUser.Max, key, value)
	case "distribution.domain_scoped_assignment_ratio":
		return setFloat(&s.Distribution.DomainScopedAssignmentRatio, key, value)
	case "distribution.users_issuing_tokens_ratio":
		return setFloat(&s.Distribution.UsersIssuingTokensRatio, key, value)
	default:
		return fmt.Errorf("unknown override key %q", key)
	}
}

// validateRange enforces 0 <= Min <= Max <= maxCount for a named range.
func validateRange(key string, r Range) error {
	if r.Min < 0 {
		return fmt.Errorf("%s.min must not be negative, got %d", key, r.Min)
	}
	if r.Min > r.Max {
		return fmt.Errorf("%s.min (%d) must not exceed %s.max (%d)", key, r.Min, key, r.Max)
	}
	if r.Max > maxCount {
		return fmt.Errorf("%s.max (%d) exceeds the limit of %d", key, r.Max, maxCount)
	}
	return nil
}

// setInt parses value as an int into dst, wrapping a parse failure with the key.
func setInt(dst *int, key, value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("override %s: %q is not an integer", key, value)
	}
	*dst = n
	return nil
}

// setInt64 parses value as an int64 into dst, wrapping a parse failure with the
// key.
func setInt64(dst *int64, key, value string) error {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return fmt.Errorf("override %s: %q is not an integer", key, value)
	}
	*dst = n
	return nil
}

// setFloat parses value as a float64 into dst, wrapping a parse failure with the
// key.
func setFloat(dst *float64, key, value string) error {
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return fmt.Errorf("override %s: %q is not a number", key, value)
	}
	*dst = f
	return nil
}
