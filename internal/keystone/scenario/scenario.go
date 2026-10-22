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
	// Pressure, when present, configures the closed-loop load mode (the
	// keystone pressure subcommand). Only keystone pressure reads it; apply,
	// generate, chaos and a mix run's keystone lane ignore it, so it takes no
	// part in the plan. It is a pointer so an absent block stays nil, and like
	// the chaos block its values are defaults that the pressure CLI flags
	// override where a flag exists.
	Pressure *Pressure `yaml:"pressure,omitempty"`
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
//
// BucketWidth is the width of one time bucket of a run without an end, which
// only --duration 0 selects; zero means unset, and a bounded run ignores it.
type Chaos struct {
	Duration    Duration `yaml:"duration"`
	Interval    Interval `yaml:"interval"`
	Parallel    Parallel `yaml:"parallel"`
	ChurnRatio  float64  `yaml:"churn_ratio"`
	TargetFill  float64  `yaml:"target_fill"`
	TokenRatio  *float64 `yaml:"token_ratio"`
	BucketWidth Duration `yaml:"bucket_width"`
}

// Pressure holds the closed-loop load knobs read from a scenario's pressure
// block. It is read only by the keystone pressure subcommand. Every field
// except Stages has a pressure CLI flag that overrides it, and so does each
// key of Propagation. Set has no key for the pressure block, as for the chaos
// block. A zero Duration, Workers, Tokens or BucketWidth means unset and falls
// back to the command's default; Duration is not required here because a flag
// may supply it, and a zero duration in the block never selects a run without
// an end, which only --duration 0 does. ChurnUsers is a pointer so an omitted
// key (nil) falls back to the command's default while an explicit
// churn_users: 0 reserves no user for grant_churn.
//
// BucketWidth is the width of one time bucket of a run without an end, as for
// the chaos block. The probe and bad_token weights of Mix and the Propagation
// block are the parts of a run that leave Keystone: they send requests to
// Nova, Neutron, Cinder and Glance, as far as the pool users' catalog lists
// them.
//
// Stages split a bounded run into consecutive windows, each of which may grow
// the fan users' grants when the run enters it; they come from the scenario
// only. No stages means a run of one window, as before stages existed.
type Pressure struct {
	Duration    Duration     `yaml:"duration"`
	Workers     int          `yaml:"workers"`
	Tokens      int          `yaml:"tokens"`
	ChurnUsers  *int         `yaml:"churn_users"`
	Mix         *Mix         `yaml:"mix"`
	Propagation *Propagation `yaml:"propagation"`
	BucketWidth Duration     `yaml:"bucket_width"`
	Stages      []Stage      `yaml:"stages"`
}

// Stage is one stage of a pressure run. FanAssignments, when set, is the
// number of grants every fan user holds once the run has entered the stage.
// Duration is the stage's share of the run; zero means an equal share of
// what the stages with a duration leave.
type Stage struct {
	FanAssignments *int     `yaml:"fan_assignments"`
	Duration       Duration `yaml:"duration"`
}

// Mix holds the pressure operation weights: a worker draws an operation with
// probability weight divided by the sum of the weights. Each weight is a
// pointer so an omitted key (nil) falls back to the command's default while
// an explicit 0 switches the operation off. Probe and BadToken weigh the two
// operations that go to a service other than Keystone: probe sends a pool
// token there for the cheapest read the service offers, and bad_token floods
// it with tokens it has to reject. Both are off by default.
type Mix struct {
	Validate   *int `yaml:"validate"`
	Probe      *int `yaml:"probe"`
	Effective  *int `yaml:"effective"`
	Revoke     *int `yaml:"revoke"`
	GrantChurn *int `yaml:"grant_churn"`
	Issue      *int `yaml:"issue"`
	BadToken   *int `yaml:"bad_token"`
}

// Propagation configures the propagation probe, which revokes one pool token
// every Every and measures how long a service takes to reject it.
type Propagation struct {
	Every Duration `yaml:"every"`
	// Timeout is the ceiling of one probe: a probe whose service still accepts
	// the revoked token after it counts as a timeout. Zero means the command's
	// default.
	Timeout Duration `yaml:"timeout"`
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

// Resources holds the fixed counts of top-level resources to create and the
// implied-role rules between the created roles. Domains, roles and the rules
// are admin-only knobs: in domain-manager mode domains are forced to the single
// in-scope domain, roles are ignored (existing roles reused) and no rule is
// created.
type Resources struct {
	Domains      int           `yaml:"domains"`
	Roles        int           `yaml:"roles"`
	Projects     int           `yaml:"projects"`
	Users        int           `yaml:"users"`
	ImpliedRoles []ImpliedRole `yaml:"implied_roles"`
}

// ImpliedRole declares that a grant of the role Prior also gives its holder
// every role in Implies on the same target. Both name roles of the plan by
// their logical names, role-0001 to role-<resources.roles>.
type ImpliedRole struct {
	Prior   string   `yaml:"prior"`
	Implies []string `yaml:"implies"`
}

// Distribution holds the ranges and ratios that shape how projects are dealt
// across domains, how many grants each user draws, which grants and users are
// domain-scoped or issue tokens, and how many fan users hold a grant on many
// projects.
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
	// FanUsers is the number of fan users, appended after the resources.users
	// users, each holding one grant on many distinct projects. 0 means none.
	FanUsers int `yaml:"fan_users"`
	// FanAssignments is the number of projects a fan user holds a grant on,
	// drawn per fan user. Its max must not exceed resources.projects.
	FanAssignments Range `yaml:"fan_assignments"`
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
// users × assignments_per_user.max plus fan_users × fan_assignments.max —
// Validate caps that sum separately.
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
		{"distribution.fan_users", s.Distribution.FanUsers},
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
		{"distribution.fan_assignments", s.Distribution.FanAssignments},
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

	fanUsers := s.Distribution.FanUsers > 0
	// Projects and users, fan users among them, need at least one domain to
	// hang off.
	if (s.Resources.Projects > 0 || s.Resources.Users > 0 || fanUsers) && s.Resources.Domains < 1 {
		return fmt.Errorf("resources.domains must be at least 1 when projects or users are created, got %d", s.Resources.Domains)
	}
	// Grants need at least one role to draw from, and a fan user always draws
	// some.
	if ((s.Resources.Users > 0 && s.Distribution.AssignmentsPerUser.Max > 0) || fanUsers) && s.Resources.Roles < 1 {
		return fmt.Errorf("resources.roles must be at least 1 when users draw assignments, got %d", s.Resources.Roles)
	}
	// The round-robin deal must make progress, so each turn places at least one
	// project.
	if s.Resources.Projects > 0 && s.Distribution.ProjectsPerDomain.Min < 1 {
		return fmt.Errorf("distribution.projects_per_domain.min must be at least 1 when projects are created, got %d", s.Distribution.ProjectsPerDomain.Min)
	}
	// A fan user holds at least one grant, and each of its grants is on a
	// distinct project. Without fan users the range is not drawn from.
	if fanUsers && s.Distribution.FanAssignments.Min < 1 {
		return fmt.Errorf("distribution.fan_assignments.min must be at least 1 when fan users are created, got %d", s.Distribution.FanAssignments.Min)
	}
	if fanUsers && s.Distribution.FanAssignments.Max > s.Resources.Projects {
		return fmt.Errorf("distribution.fan_assignments.max (%d) exceeds resources.projects (%d); a fan user holds one grant per distinct project",
			s.Distribution.FanAssignments.Max, s.Resources.Projects)
	}

	// The assignment slice grows to users × assignments_per_user.max plus
	// fan_users × fan_assignments.max, a sum no per-dimension cap bounds; cap
	// it too. Compute in int64 so neither product can overflow int on a 32-bit
	// platform. A scenario without fan users keeps the message that names the
	// regular product alone.
	regular := int64(s.Resources.Users) * int64(s.Distribution.AssignmentsPerUser.Max)
	fan := int64(s.Distribution.FanUsers) * int64(s.Distribution.FanAssignments.Max)
	if total := regular + fan; total > maxCount {
		if fanUsers {
			return fmt.Errorf("resources.users (%d) × distribution.assignments_per_user.max (%d) + distribution.fan_users (%d) × distribution.fan_assignments.max (%d) = %d exceeds the total assignment limit of %d",
				s.Resources.Users, s.Distribution.AssignmentsPerUser.Max, s.Distribution.FanUsers, s.Distribution.FanAssignments.Max, total, maxCount)
		}
		return fmt.Errorf("resources.users (%d) × distribution.assignments_per_user.max (%d) = %d exceeds the total assignment limit of %d",
			s.Resources.Users, s.Distribution.AssignmentsPerUser.Max, total, maxCount)
	}

	if err := s.validateImpliedRoles(); err != nil {
		return err
	}
	if err := s.Chaos.validate(); err != nil {
		return err
	}
	if err := s.Pressure.validate(); err != nil {
		return err
	}
	if err := s.validateStages(); err != nil {
		return err
	}

	return nil
}

// validateImpliedRoles checks the implied-role rules against the plan's roles.
// A rule needs two distinct roles, so a scenario with rules needs at least two.
// Each rule must imply at least one role, name only plan roles, never let a
// role imply itself and never repeat a (prior, implied) pair of an earlier rule
// or entry. A rule is named by its 0-based index, an implied role by its
// 0-based index within the rule. A cycle is valid: Keystone accepts it and its
// expansion stops at a cycle. A scenario without rules is valid.
func (s Scenario) validateImpliedRoles() error {
	rules := s.Resources.ImpliedRoles
	if len(rules) == 0 {
		return nil
	}
	roles := s.Resources.Roles
	if roles < 2 {
		return fmt.Errorf("resources.roles must be at least 2 when implied_roles are declared, got %d", roles)
	}
	seen := make(map[[2]string]bool)
	for i, r := range rules {
		if len(r.Implies) == 0 {
			return fmt.Errorf("resources.implied_roles[%d].implies must name at least one role", i)
		}
		if !planRole(r.Prior, roles) {
			return fmt.Errorf("resources.implied_roles[%d].prior %q is not a role of the plan, whose roles are role-0001 to %s", i, r.Prior, roleName(roles))
		}
		for j, implied := range r.Implies {
			if !planRole(implied, roles) {
				return fmt.Errorf("resources.implied_roles[%d].implies[%d] %q is not a role of the plan, whose roles are role-0001 to %s", i, j, implied, roleName(roles))
			}
			if implied == r.Prior {
				return fmt.Errorf("resources.implied_roles[%d].implies[%d] lets %q imply itself", i, j, implied)
			}
			key := [2]string{r.Prior, implied}
			if seen[key] {
				return fmt.Errorf("resources.implied_roles[%d].implies[%d] repeats the rule %q implies %q", i, j, r.Prior, implied)
			}
			seen[key] = true
		}
	}
	return nil
}

// roleName returns the logical name Generate gives the k-th role (1-based).
func roleName(k int) string {
	return fmt.Sprintf("role-%04d", k)
}

// planRole reports whether name is the logical name of one of the roles roles
// a plan creates, role-0001 to roleName(roles). It parses the number rather
// than building the set of names, since roles may be up to maxCount, and
// compares the name back to roleName so that role-1 or role-00001 fail.
func planRole(name string, roles int) bool {
	digits, ok := strings.CutPrefix(name, "role-")
	if !ok {
		return false
	}
	k, err := strconv.Atoi(digits)
	if err != nil || k < 1 || k > roles {
		return false
	}
	return roleName(k) == name
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
	if c.BucketWidth < 0 {
		return fmt.Errorf("chaos.bucket_width must not be negative, got %s", time.Duration(c.BucketWidth))
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

// validate checks the pressure block for semantic consistency. A nil receiver
// (no pressure block) is valid. Nothing is required here because the
// command's defaults and flags may supply every value; only the values that
// are present must not be negative, and a stage's fan_assignments must be at
// least 1. A stage is named by its 0-based index. The stage checks that need
// the distribution belong to Scenario.validateStages; the checks that need the
// merged values, such as the workers ceiling, an all-zero mix, a propagation
// timeout that must be positive while the probe runs or stage durations that
// overrun the run, belong to the command.
func (p *Pressure) validate() error {
	if p == nil {
		return nil
	}
	if p.Duration < 0 {
		return fmt.Errorf("pressure.duration must not be negative, got %s", time.Duration(p.Duration))
	}
	if p.BucketWidth < 0 {
		return fmt.Errorf("pressure.bucket_width must not be negative, got %s", time.Duration(p.BucketWidth))
	}
	if p.Workers < 0 {
		return fmt.Errorf("pressure.workers must not be negative, got %d", p.Workers)
	}
	if p.Tokens < 0 {
		return fmt.Errorf("pressure.tokens must not be negative, got %d", p.Tokens)
	}
	if p.ChurnUsers != nil && *p.ChurnUsers < 0 {
		return fmt.Errorf("pressure.churn_users must not be negative, got %d", *p.ChurnUsers)
	}
	if err := p.Mix.validate(); err != nil {
		return err
	}
	if p.Propagation != nil && p.Propagation.Every < 0 {
		return fmt.Errorf("pressure.propagation.every must not be negative, got %s", time.Duration(p.Propagation.Every))
	}
	if p.Propagation != nil && p.Propagation.Timeout < 0 {
		return fmt.Errorf("pressure.propagation.timeout must not be negative, got %s", time.Duration(p.Propagation.Timeout))
	}
	for i, st := range p.Stages {
		if st.Duration < 0 {
			return fmt.Errorf("pressure.stages[%d].duration must not be negative, got %s", i, time.Duration(st.Duration))
		}
		if st.FanAssignments != nil && *st.FanAssignments < 1 {
			return fmt.Errorf("pressure.stages[%d].fan_assignments must be at least 1, got %d", i, *st.FanAssignments)
		}
	}
	return nil
}

// validateStages checks the fan_assignments of the pressure stages against
// the distribution, over the stages that set one, in order: a stage may set
// one only when the scenario has fan users, never above resources.projects,
// since each grant is on a distinct project, and never below what a fan user
// may already hold, so a stage only adds grants and its fan_assignments is the
// count every fan user holds in it. For the first such stage that is
// distribution.fan_assignments.max, for a later one the previous such stage's
// value. A stage without fan_assignments is a window with no override. A
// scenario without a pressure block or without stages is valid.
func (s Scenario) validateStages() error {
	if s.Pressure == nil {
		return nil
	}
	prev := -1 // the index of the last stage that set fan_assignments
	for i, st := range s.Pressure.Stages {
		if st.FanAssignments == nil {
			continue
		}
		n := *st.FanAssignments
		if s.Distribution.FanUsers == 0 {
			return fmt.Errorf("pressure.stages[%d] sets fan_assignments but distribution.fan_users is 0", i)
		}
		if n > s.Resources.Projects {
			return fmt.Errorf("pressure.stages[%d].fan_assignments (%d) exceeds resources.projects (%d)", i, n, s.Resources.Projects)
		}
		if prev < 0 {
			if n < s.Distribution.FanAssignments.Max {
				return fmt.Errorf("pressure.stages[%d].fan_assignments (%d) is below distribution.fan_assignments.max (%d), which a fan user may already hold",
					i, n, s.Distribution.FanAssignments.Max)
			}
		} else if before := *s.Pressure.Stages[prev].FanAssignments; n < before {
			return fmt.Errorf("pressure.stages[%d].fan_assignments (%d) is below stage %d's (%d); a stage never removes assignments", i, n, prev, before)
		}
		prev = i
	}
	return nil
}

// validate rejects a negative weight in the pressure mix, naming the weight by
// its YAML key. A nil receiver (no mix) and a nil weight (an omitted key) are
// valid.
func (m *Mix) validate() error {
	if m == nil {
		return nil
	}
	for _, w := range []struct {
		key    string
		weight *int
	}{
		{"validate", m.Validate},
		{"probe", m.Probe},
		{"effective", m.Effective},
		{"revoke", m.Revoke},
		{"grant_churn", m.GrantChurn},
		{"issue", m.Issue},
		{"bad_token", m.BadToken},
	} {
		if w.weight != nil && *w.weight < 0 {
			return fmt.Errorf("pressure.mix.%s must not be negative, got %d", w.key, *w.weight)
		}
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
	case "distribution.fan_users":
		return setInt(&s.Distribution.FanUsers, key, value)
	case "distribution.fan_assignments.min":
		return setInt(&s.Distribution.FanAssignments.Min, key, value)
	case "distribution.fan_assignments.max":
		return setInt(&s.Distribution.FanAssignments.Max, key, value)
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
