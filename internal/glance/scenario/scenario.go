// Package scenario defines the human-authored YAML scenario format for Glance
// (the image count, the payload-size range, the lifecycle ratios, and the seed)
// and the deterministic generator that expands a scenario plus its seed into a
// fully-enumerated plan. The same scenario and seed always yield a byte-identical
// plan. Glance gets its own scenario schema rather than sharing the Neutron,
// Cinder, or Nova one: the services share no resources, and separate schemas keep
// yaml.UnmarshalStrict failing loudly on a typo in any of them.
package scenario

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

// Scenario is the parametrized description of a desired Glance workload. It is
// parsed from YAML, validated, optionally overridden via Set, and expanded into
// a plan by Generate. Every image enters through the direct data-upload path with
// a synthetic payload dizzy generates, so a run references no pre-existing image.
type Scenario struct {
	Name         string       `yaml:"name"`
	Seed         int64        `yaml:"seed"`
	Resources    Resources    `yaml:"resources"`
	Distribution Distribution `yaml:"distribution"`
	// Chaos, when present, configures the random churn/soak mode (the glance chaos
	// subcommand). It is a pointer so an absent block stays nil and apply and
	// generate ignore it entirely. It mirrors the other services' scenario chaos
	// blocks, duplicated by design so a typo in any service's scenario keeps
	// failing loudly.
	Chaos *Chaos `yaml:"chaos,omitempty"`
}

// Chaos holds the churn-mode knobs read from a scenario's chaos block. Every
// field has a corresponding chaos CLI flag that overrides it; an unset field
// falls back to the command's default. Duration is intentionally not required
// here (a flag may supply it); the merged "duration must be set" check lives in
// the command. LifecycleRatio is the image-specific knob: the per-step
// probability of applying a lifecycle mutation (a deactivate/reactivate cycle, a
// visibility flip, a member add/accept/remove, or metadata churn) to a live
// image. It is a pointer so an omitted key (nil) falls back to the command's
// default while an explicit lifecycle_ratio: 0 disables mutations — the one knob
// where 0 reads as an on/off switch rather than a degenerate value.
type Chaos struct {
	Duration       Duration `yaml:"duration"`
	Interval       Interval `yaml:"interval"`
	Parallel       Parallel `yaml:"parallel"`
	ChurnRatio     float64  `yaml:"churn_ratio"`
	TargetFill     float64  `yaml:"target_fill"`
	LifecycleRatio *float64 `yaml:"lifecycle_ratio"`
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

// Resources holds the fixed count of images to create.
type Resources struct {
	Images int `yaml:"images"`
}

// Distribution holds the per-image ranges and ratios that shape each image's
// upload payload size and which lifecycle operations it is driven through.
type Distribution struct {
	// ImageSizeMiB is the size drawn per image for its synthetic upload payload,
	// in mebibytes.
	ImageSizeMiB Range `yaml:"image_size_mib"`
	// MetadataUpdateRatio is the fraction of images whose custom properties are
	// churned during the run.
	MetadataUpdateRatio float64 `yaml:"metadata_update_ratio"`
	// SharedRatio is the fraction of images transitioned to shared visibility.
	SharedRatio float64 `yaml:"shared_ratio"`
	// MemberAcceptRatio is the fraction of shared images whose membership is
	// accepted. It is only consulted for an image the run shares.
	MemberAcceptRatio float64 `yaml:"member_accept_ratio"`
	// MemberRemoveRatio is the fraction of shared images whose membership is later
	// removed. It is only consulted for an image the run shares.
	MemberRemoveRatio float64 `yaml:"member_remove_ratio"`
	// CommunityRatio is the fraction of images transitioned to community
	// visibility.
	CommunityRatio float64 `yaml:"community_ratio"`
	// PublicRatio is the fraction of images transitioned to public visibility.
	// publicize_image is admin-only on most clouds, so the shipped profiles set it
	// to 0.
	PublicRatio float64 `yaml:"public_ratio"`
	// DeactivateRatio is the fraction of images driven through a
	// deactivate/reactivate cycle.
	DeactivateRatio float64 `yaml:"deactivate_ratio"`
	// DeletedRatio is the fraction of images deleted during the run.
	DeletedRatio float64 `yaml:"deleted_ratio"`
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

// maxCount caps the image count and every range maximum. It bounds the image
// slice preallocation and guards randRange's interval arithmetic against integer
// overflow, mirroring the other services' scenarios' cap.
const maxCount = 1_000_000

// Validate checks the scenario for semantic consistency, returning an actionable
// error that names the offending field.
func (s Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name must not be empty")
	}

	if s.Resources.Images < 0 {
		return fmt.Errorf("resources.images must not be negative, got %d", s.Resources.Images)
	}
	if s.Resources.Images > maxCount {
		return fmt.Errorf("resources.images exceeds the limit of %d, got %d", maxCount, s.Resources.Images)
	}

	if err := validateRange("distribution.image_size_mib", s.Distribution.ImageSizeMiB); err != nil {
		return err
	}

	for _, c := range []struct {
		key   string
		ratio float64
	}{
		{"distribution.metadata_update_ratio", s.Distribution.MetadataUpdateRatio},
		{"distribution.shared_ratio", s.Distribution.SharedRatio},
		{"distribution.member_accept_ratio", s.Distribution.MemberAcceptRatio},
		{"distribution.member_remove_ratio", s.Distribution.MemberRemoveRatio},
		{"distribution.community_ratio", s.Distribution.CommunityRatio},
		{"distribution.public_ratio", s.Distribution.PublicRatio},
		{"distribution.deactivate_ratio", s.Distribution.DeactivateRatio},
		{"distribution.deleted_ratio", s.Distribution.DeletedRatio},
	} {
		if c.ratio < 0 || c.ratio > 1 {
			return fmt.Errorf("%s must be between 0 and 1, got %v", c.key, c.ratio)
		}
	}

	// Every image needs a positive upload payload, so the size range must start at
	// least at 1 MiB whenever any image is created.
	if s.Resources.Images > 0 && s.Distribution.ImageSizeMiB.Min < 1 {
		return fmt.Errorf("distribution.image_size_mib.min must be at least 1 when resources.images > 0, got %d", s.Distribution.ImageSizeMiB.Min)
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
	if c.LifecycleRatio != nil && (*c.LifecycleRatio < 0 || *c.LifecycleRatio > 1) {
		return fmt.Errorf("chaos.lifecycle_ratio must be between 0 and 1, got %v", *c.LifecycleRatio)
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
	case "resources.images":
		return setInt(&s.Resources.Images, key, value)
	case "distribution.image_size_mib.min":
		return setInt(&s.Distribution.ImageSizeMiB.Min, key, value)
	case "distribution.image_size_mib.max":
		return setInt(&s.Distribution.ImageSizeMiB.Max, key, value)
	case "distribution.metadata_update_ratio":
		return setFloat(&s.Distribution.MetadataUpdateRatio, key, value)
	case "distribution.shared_ratio":
		return setFloat(&s.Distribution.SharedRatio, key, value)
	case "distribution.member_accept_ratio":
		return setFloat(&s.Distribution.MemberAcceptRatio, key, value)
	case "distribution.member_remove_ratio":
		return setFloat(&s.Distribution.MemberRemoveRatio, key, value)
	case "distribution.community_ratio":
		return setFloat(&s.Distribution.CommunityRatio, key, value)
	case "distribution.public_ratio":
		return setFloat(&s.Distribution.PublicRatio, key, value)
	case "distribution.deactivate_ratio":
		return setFloat(&s.Distribution.DeactivateRatio, key, value)
	case "distribution.deleted_ratio":
		return setFloat(&s.Distribution.DeletedRatio, key, value)
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
