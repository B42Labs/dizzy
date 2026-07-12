// Package scenario defines the human-authored YAML scenario format for Nova
// (counts, ranges, ratios, seed, image and flavor references) and the
// deterministic generator that expands a scenario plus its seed into a
// fully-enumerated plan. The same scenario and seed always yield a
// byte-identical plan. Nova gets its own scenario schema rather than sharing the
// Neutron or Cinder one: the services share no resources, and separate schemas
// keep yaml.UnmarshalStrict failing loudly on a typo in any of them.
package scenario

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

// Scenario is the parametrized description of a desired Nova workload. It is
// parsed from YAML, validated, optionally overridden via Set, and expanded into
// a plan by Generate. Image, Flavor, and ResizeFlavor name resources that must
// already exist on the target cloud — dizzy uploads no image and creates no
// flavor — and are resolved by name at apply time.
type Scenario struct {
	Name         string       `yaml:"name"`
	Seed         int64        `yaml:"seed"`
	Image        string       `yaml:"image"`
	Flavor       string       `yaml:"flavor"`
	ResizeFlavor string       `yaml:"resize_flavor"`
	Resources    Resources    `yaml:"resources"`
	Distribution Distribution `yaml:"distribution"`
	// Chaos, when present, configures the random churn/soak mode (the nova chaos
	// subcommand). It is a pointer so an absent block stays nil and apply and
	// generate ignore it entirely. It mirrors the Neutron and Cinder scenario
	// chaos blocks, duplicated by design so a typo in any service's scenario keeps
	// failing loudly.
	Chaos *Chaos `yaml:"chaos,omitempty"`
}

// Chaos holds the churn-mode knobs read from a scenario's chaos block. Every
// field has a corresponding chaos CLI flag that overrides it; an unset field
// falls back to the command's default. Duration is intentionally not required
// here (a flag may supply it); the merged "duration must be set" check lives in
// the command. LifecycleRatio is the compute-specific knob: the per-step
// probability of applying a lifecycle mutation (stop/start, resize, or
// live-migrate) to a live server. It is a pointer so an omitted key (nil) falls
// back to the command's default while an explicit lifecycle_ratio: 0 disables
// mutations — the one knob where 0 reads as an on/off switch rather than a
// degenerate value.
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

// Resources holds the fixed counts of top-level resources to create.
type Resources struct {
	Servers  int `yaml:"servers"`
	Networks int `yaml:"networks"`
}

// Distribution holds the per-server ranges and ratios that shape how many
// networks each server joins, how many volumes and ports it gets, their sizes,
// and which lifecycle operations it is driven through.
type Distribution struct {
	// NetworksPerServer is the number of networks a server is wired into.
	NetworksPerServer Range `yaml:"networks_per_server"`
	// VolumesPerServer is the number of data volumes drawn per server.
	VolumesPerServer Range `yaml:"volumes_per_server"`
	// PortsPerServer is the number of extra ports drawn per server.
	PortsPerServer Range `yaml:"ports_per_server"`
	// AttachedVolumeGiB is the size drawn per data volume.
	AttachedVolumeGiB Range `yaml:"attached_volume_gib"`
	// RootVolumeGiB is the root size drawn per boot-from-volume server.
	RootVolumeGiB Range `yaml:"root_volume_gib"`
	// BootFromVolumeRatio is the fraction of servers booted from a root volume.
	BootFromVolumeRatio float64 `yaml:"boot_from_volume_ratio"`
	// UserDataRatio is the fraction of servers given user data at boot.
	UserDataRatio float64 `yaml:"user_data_ratio"`
	// StopStartRatio is the fraction of servers stop/started during the run.
	StopStartRatio float64 `yaml:"stop_start_ratio"`
	// StopStartHardRatio is the fraction of stop/started servers driven through
	// the hard variant rather than the soft one.
	StopStartHardRatio float64 `yaml:"stop_start_hard_ratio"`
	// ResizedRatio is the fraction of servers resized to the resize flavor.
	ResizedRatio float64 `yaml:"resized_ratio"`
	// LiveMigratedRatio is the fraction of servers live-migrated.
	LiveMigratedRatio float64 `yaml:"live_migrated_ratio"`
	// DeletedRatio is the fraction of servers deleted during the run.
	DeletedRatio float64 `yaml:"deleted_ratio"`
	// VolumeDetachRatio is the fraction of data volumes detached during the run.
	VolumeDetachRatio float64 `yaml:"volume_detach_ratio"`
	// PortDetachRatio is the fraction of ports detached during the run.
	PortDetachRatio float64 `yaml:"port_detach_ratio"`
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

// maxCount caps the server and network counts and every range maximum. It bounds
// each individual slice preallocation and guards randRange's interval arithmetic
// against integer overflow, mirroring the neutron and cinder scenarios' cap. It
// does not by itself bound the volume or port slices, whose lengths are
// servers × per-server maxima — Validate caps those products separately.
const maxCount = 1_000_000

// Validate checks the scenario for semantic consistency, returning an actionable
// error that names the offending field.
func (s Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name must not be empty")
	}

	for _, c := range []struct {
		key string
		n   int
	}{
		{"resources.servers", s.Resources.Servers},
		{"resources.networks", s.Resources.Networks},
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
		{"distribution.networks_per_server", s.Distribution.NetworksPerServer},
		{"distribution.volumes_per_server", s.Distribution.VolumesPerServer},
		{"distribution.ports_per_server", s.Distribution.PortsPerServer},
		{"distribution.attached_volume_gib", s.Distribution.AttachedVolumeGiB},
		{"distribution.root_volume_gib", s.Distribution.RootVolumeGiB},
	} {
		if err := validateRange(c.key, c.r); err != nil {
			return err
		}
	}

	for _, c := range []struct {
		key   string
		ratio float64
	}{
		{"distribution.boot_from_volume_ratio", s.Distribution.BootFromVolumeRatio},
		{"distribution.user_data_ratio", s.Distribution.UserDataRatio},
		{"distribution.stop_start_ratio", s.Distribution.StopStartRatio},
		{"distribution.stop_start_hard_ratio", s.Distribution.StopStartHardRatio},
		{"distribution.resized_ratio", s.Distribution.ResizedRatio},
		{"distribution.live_migrated_ratio", s.Distribution.LiveMigratedRatio},
		{"distribution.deleted_ratio", s.Distribution.DeletedRatio},
		{"distribution.volume_detach_ratio", s.Distribution.VolumeDetachRatio},
		{"distribution.port_detach_ratio", s.Distribution.PortDetachRatio},
	} {
		if c.ratio < 0 || c.ratio > 1 {
			return fmt.Errorf("%s must be between 0 and 1, got %v", c.key, c.ratio)
		}
	}

	// The volume and port slices are preallocated to servers × per-server maxima,
	// products neither per-dimension cap bounds; cap them too so a large-but-legal
	// pair cannot demand a terabyte-scale allocation. Compute in int64 so the
	// product cannot overflow int on a 32-bit platform.
	for _, c := range []struct {
		key string
		max int
	}{
		{"distribution.volumes_per_server.max", s.Distribution.VolumesPerServer.Max},
		{"distribution.ports_per_server.max", s.Distribution.PortsPerServer.Max},
	} {
		if total := int64(s.Resources.Servers) * int64(c.max); total > maxCount {
			return fmt.Errorf("resources.servers (%d) × %s (%d) = %d exceeds the limit of %d",
				s.Resources.Servers, c.key, c.max, total, maxCount)
		}
	}

	// Generate draws each server's network membership with rng.Perm(resources.networks),
	// which allocates and shuffles a full networks-length slice per server before keeping
	// only k; neither per-dimension cap bounds that servers × networks cost. Cap it too so
	// a large-but-legal pair cannot churn a terabyte of transient allocations.
	if total := int64(s.Resources.Servers) * int64(s.Resources.Networks); total > maxCount {
		return fmt.Errorf("resources.servers (%d) × resources.networks (%d) = %d exceeds the limit of %d",
			s.Resources.Servers, s.Resources.Networks, total, maxCount)
	}

	if s.Resources.Servers > 0 {
		if s.Image == "" {
			return fmt.Errorf("image must be set when resources.servers > 0")
		}
		if s.Flavor == "" {
			return fmt.Errorf("flavor must be set when resources.servers > 0")
		}
		// A server must join at least one network, and it cannot join more networks
		// than exist.
		if s.Distribution.NetworksPerServer.Min < 1 {
			return fmt.Errorf("distribution.networks_per_server.min must be at least 1 when resources.servers > 0, got %d", s.Distribution.NetworksPerServer.Min)
		}
		if s.Distribution.NetworksPerServer.Max > s.Resources.Networks {
			return fmt.Errorf("distribution.networks_per_server.max (%d) must not exceed resources.networks (%d)", s.Distribution.NetworksPerServer.Max, s.Resources.Networks)
		}
	}

	// A resized server needs a resize flavor distinct from the boot flavor.
	if s.Distribution.ResizedRatio > 0 {
		if s.ResizeFlavor == "" {
			return fmt.Errorf("resize_flavor must be set when distribution.resized_ratio > 0")
		}
		if s.ResizeFlavor == s.Flavor {
			return fmt.Errorf("resize_flavor (%q) must differ from flavor (%q)", s.ResizeFlavor, s.Flavor)
		}
	}

	// A boot-from-volume server needs a positive root size.
	if s.Distribution.BootFromVolumeRatio > 0 && s.Distribution.RootVolumeGiB.Min < 1 {
		return fmt.Errorf("distribution.root_volume_gib.min must be at least 1 when boot_from_volume_ratio > 0, got %d", s.Distribution.RootVolumeGiB.Min)
	}

	// A data volume needs a positive size whenever any server can get one.
	if s.Distribution.VolumesPerServer.Max > 0 && s.Distribution.AttachedVolumeGiB.Min < 1 {
		return fmt.Errorf("distribution.attached_volume_gib.min must be at least 1 when volumes_per_server.max > 0, got %d", s.Distribution.AttachedVolumeGiB.Min)
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
	case "image":
		s.Image = value
		return nil
	case "flavor":
		s.Flavor = value
		return nil
	case "resize_flavor":
		s.ResizeFlavor = value
		return nil
	case "resources.servers":
		return setInt(&s.Resources.Servers, key, value)
	case "resources.networks":
		return setInt(&s.Resources.Networks, key, value)
	case "distribution.networks_per_server.min":
		return setInt(&s.Distribution.NetworksPerServer.Min, key, value)
	case "distribution.networks_per_server.max":
		return setInt(&s.Distribution.NetworksPerServer.Max, key, value)
	case "distribution.volumes_per_server.min":
		return setInt(&s.Distribution.VolumesPerServer.Min, key, value)
	case "distribution.volumes_per_server.max":
		return setInt(&s.Distribution.VolumesPerServer.Max, key, value)
	case "distribution.ports_per_server.min":
		return setInt(&s.Distribution.PortsPerServer.Min, key, value)
	case "distribution.ports_per_server.max":
		return setInt(&s.Distribution.PortsPerServer.Max, key, value)
	case "distribution.attached_volume_gib.min":
		return setInt(&s.Distribution.AttachedVolumeGiB.Min, key, value)
	case "distribution.attached_volume_gib.max":
		return setInt(&s.Distribution.AttachedVolumeGiB.Max, key, value)
	case "distribution.root_volume_gib.min":
		return setInt(&s.Distribution.RootVolumeGiB.Min, key, value)
	case "distribution.root_volume_gib.max":
		return setInt(&s.Distribution.RootVolumeGiB.Max, key, value)
	case "distribution.boot_from_volume_ratio":
		return setFloat(&s.Distribution.BootFromVolumeRatio, key, value)
	case "distribution.user_data_ratio":
		return setFloat(&s.Distribution.UserDataRatio, key, value)
	case "distribution.stop_start_ratio":
		return setFloat(&s.Distribution.StopStartRatio, key, value)
	case "distribution.stop_start_hard_ratio":
		return setFloat(&s.Distribution.StopStartHardRatio, key, value)
	case "distribution.resized_ratio":
		return setFloat(&s.Distribution.ResizedRatio, key, value)
	case "distribution.live_migrated_ratio":
		return setFloat(&s.Distribution.LiveMigratedRatio, key, value)
	case "distribution.deleted_ratio":
		return setFloat(&s.Distribution.DeletedRatio, key, value)
	case "distribution.volume_detach_ratio":
		return setFloat(&s.Distribution.VolumeDetachRatio, key, value)
	case "distribution.port_detach_ratio":
		return setFloat(&s.Distribution.PortDetachRatio, key, value)
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
