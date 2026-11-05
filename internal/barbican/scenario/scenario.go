// Package scenario defines the human-authored YAML scenario format for Barbican
// (the secret, order, and container counts, the payload-size range, the
// read-back ratio, the container fan-in, the seed, and the optional chaos block
// of the churn mode) and the deterministic generator that expands a scenario
// plus its seed into a fully-enumerated plan.
// The same scenario and seed always yield a byte-identical plan. Barbican gets
// its own scenario schema rather than sharing another service's: the services
// share no resources, and separate schemas keep yaml.UnmarshalStrict failing
// loudly on a typo in any of them.
package scenario

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

// Scenario is the parametrized description of a desired Barbican workload. It
// is parsed from YAML, validated, optionally overridden via Set, and expanded
// into a plan by Generate. Every stored secret carries a synthetic payload dizzy
// derives from the seed, so a run references no pre-existing secret.
type Scenario struct {
	Name         string       `yaml:"name"`
	Seed         int64        `yaml:"seed"`
	Resources    Resources    `yaml:"resources"`
	Distribution Distribution `yaml:"distribution"`
	// Chaos, when present, configures the random churn/soak mode (the barbican
	// chaos subcommand). It is a pointer so an absent block stays nil and apply
	// and generate ignore it entirely. It mirrors the other services' scenario
	// chaos blocks, duplicated by design so a typo in any service's scenario keeps
	// failing loudly.
	Chaos *Chaos `yaml:"chaos,omitempty"`
}

// Chaos holds the churn-mode knobs read from a scenario's chaos block. Every
// field has a corresponding chaos CLI flag that overrides it; an unset field
// falls back to the command's default. Duration is intentionally not required
// here (a flag may supply it); the merged "duration must be set" check lives in
// the command. RetrieveRatio is the key-manager-specific knob: the per-step
// probability of reading back the payload of a live stored secret and
// byte-comparing it with the bytes it was stored with. It only picks how often a
// read-back happens; distribution.retrieve_ratio still decides which secrets the
// plan marks as read-back candidates. It is a pointer so an omitted key (nil)
// falls back to the command's default while an explicit retrieve_ratio: 0
// disables read-backs — the one knob where 0 reads as an on/off switch rather
// than a degenerate value.
//
// BucketWidth is the width of one time bucket of a run without an end, which
// only --duration 0 selects; zero means unset, and a bounded run ignores it.
type Chaos struct {
	Duration      Duration `yaml:"duration"`
	Interval      Interval `yaml:"interval"`
	Parallel      Parallel `yaml:"parallel"`
	ChurnRatio    float64  `yaml:"churn_ratio"`
	TargetFill    float64  `yaml:"target_fill"`
	RetrieveRatio *float64 `yaml:"retrieve_ratio"`
	BucketWidth   Duration `yaml:"bucket_width"`
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

// Resources holds the fixed counts of top-level resources to create: opaque
// secrets stored with a payload, asynchronous key orders, and generic
// containers grouping stored secrets.
type Resources struct {
	Secrets    int `yaml:"secrets"`
	Orders     int `yaml:"orders"`
	Containers int `yaml:"containers"`
}

// Distribution holds the per-secret range and ratio that shape each stored
// secret's payload size and whether it is read back, and the per-container
// range that sets how many stored secrets each container references.
type Distribution struct {
	// PayloadBytes is the size drawn per stored secret for its synthetic
	// payload, in bytes.
	PayloadBytes Range `yaml:"payload_bytes"`
	// RetrieveRatio is the fraction of stored secrets whose payload is read back
	// and byte-compared with the expected bytes.
	RetrieveRatio float64 `yaml:"retrieve_ratio"`
	// SecretsPerContainer is the reference count drawn per container. Each
	// container references that many distinct stored secrets.
	SecretsPerContainer Range `yaml:"secrets_per_container"`
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

// maxCount caps every resource count and every range maximum. It bounds the
// slice preallocations and guards randRange's interval arithmetic against
// integer overflow, mirroring the other services' scenarios' cap.
const maxCount = 1_000_000

// Validate checks the scenario for semantic consistency, returning an actionable
// error that names the offending field.
func (s Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name must not be empty")
	}

	for _, c := range []struct {
		key   string
		count int
	}{
		{"resources.secrets", s.Resources.Secrets},
		{"resources.orders", s.Resources.Orders},
		{"resources.containers", s.Resources.Containers},
	} {
		if c.count < 0 {
			return fmt.Errorf("%s must not be negative, got %d", c.key, c.count)
		}
		if c.count > maxCount {
			return fmt.Errorf("%s exceeds the limit of %d, got %d", c.key, maxCount, c.count)
		}
	}

	if err := validateRange("distribution.payload_bytes", s.Distribution.PayloadBytes); err != nil {
		return err
	}
	if err := validateRange("distribution.secrets_per_container", s.Distribution.SecretsPerContainer); err != nil {
		return err
	}

	if r := s.Distribution.RetrieveRatio; r < 0 || r > 1 {
		return fmt.Errorf("distribution.retrieve_ratio must be between 0 and 1, got %v", r)
	}

	// Barbican stores no empty payload in a one-step create, so the size range
	// must start at least at 1 byte whenever any secret is stored.
	if s.Resources.Secrets > 0 && s.Distribution.PayloadBytes.Min < 1 {
		return fmt.Errorf("distribution.payload_bytes.min must be at least 1 when resources.secrets > 0, got %d", s.Distribution.PayloadBytes.Min)
	}

	// A container references distinct stored secrets, so it cannot draw more
	// references than the run stores.
	if s.Resources.Containers > 0 && s.Distribution.SecretsPerContainer.Max > s.Resources.Secrets {
		return fmt.Errorf("distribution.secrets_per_container.max (%d) exceeds resources.secrets (%d)", s.Distribution.SecretsPerContainer.Max, s.Resources.Secrets)
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
	if c.RetrieveRatio != nil && (*c.RetrieveRatio < 0 || *c.RetrieveRatio > 1) {
		return fmt.Errorf("chaos.retrieve_ratio must be between 0 and 1, got %v", *c.RetrieveRatio)
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
	case "resources.secrets":
		return setInt(&s.Resources.Secrets, key, value)
	case "resources.orders":
		return setInt(&s.Resources.Orders, key, value)
	case "resources.containers":
		return setInt(&s.Resources.Containers, key, value)
	case "distribution.payload_bytes.min":
		return setInt(&s.Distribution.PayloadBytes.Min, key, value)
	case "distribution.payload_bytes.max":
		return setInt(&s.Distribution.PayloadBytes.Max, key, value)
	case "distribution.retrieve_ratio":
		return setFloat(&s.Distribution.RetrieveRatio, key, value)
	case "distribution.secrets_per_container.min":
		return setInt(&s.Distribution.SecretsPerContainer.Min, key, value)
	case "distribution.secrets_per_container.max":
		return setInt(&s.Distribution.SecretsPerContainer.Max, key, value)
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
