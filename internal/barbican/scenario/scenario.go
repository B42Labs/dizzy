// Package scenario defines the human-authored YAML scenario format for Barbican
// (the secret, order, and container counts, the payload-size range, the
// read-back ratio, the container fan-in, and the seed) and the deterministic
// generator that expands a scenario plus its seed into a fully-enumerated plan.
// The same scenario and seed always yield a byte-identical plan. Barbican gets
// its own scenario schema rather than sharing another service's: the services
// share no resources, and separate schemas keep yaml.UnmarshalStrict failing
// loudly on a typo in any of them.
package scenario

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v2"
)

// Scenario is the parametrized description of a desired Barbican workload. It
// is parsed from YAML, validated, optionally overridden via Set, and expanded
// into a plan by Generate. Every stored secret carries a synthetic payload dizzy
// derives from the seed, so a run references no pre-existing secret. The schema
// has no chaos block, so a scenario carrying a chaos key fails Parse like any
// other unknown key.
type Scenario struct {
	Name         string       `yaml:"name"`
	Seed         int64        `yaml:"seed"`
	Resources    Resources    `yaml:"resources"`
	Distribution Distribution `yaml:"distribution"`
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
