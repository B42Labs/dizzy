// Package scenario defines the human-authored YAML scenario format for Octavia
// (the VIP network, load balancer, listener, member, and health monitor
// counts, the listener and member port ranges, the provider, the seed, and the
// optional chaos block of the churn mode) and the deterministic generator that
// expands a scenario plus its seed into a fully-enumerated plan. The same
// scenario and seed always yield a byte-identical plan. The chaos block
// configures octavia chaos; generate and apply ignore it. Octavia gets its own
// scenario schema rather than sharing another service's: the services share no
// resources, and separate schemas keep yaml.UnmarshalStrict failing loudly on a
// typo in any of them.
package scenario

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

// Scenario is the parametrized description of a desired Octavia workload. It is
// parsed from YAML, validated, optionally overridden via Set, and expanded into
// a plan by Generate. Every load balancer sits on a VIP network dizzy creates,
// and every member is a synthetic address no backend answers on, so a run
// references no pre-existing network or server.
//
// Provider names the Octavia provider driver every load balancer requests, such
// as ovn or amphora; empty selects the cloud's default. It is not validated
// locally: Octavia answers an unknown or disabled provider with a 400 at the
// first load balancer create.
type Scenario struct {
	Name         string       `yaml:"name"`
	Seed         int64        `yaml:"seed"`
	Provider     string       `yaml:"provider"`
	Resources    Resources    `yaml:"resources"`
	Distribution Distribution `yaml:"distribution"`
	// Chaos, when present, configures the random churn/soak mode (the octavia
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
// the command. UpdateRatio is the load-balancer-specific knob: the per-step
// probability that octavia chaos sets a live listener or member to
// admin_state_up=false and back to true. It is a pointer so an omitted key
// (nil) falls back to the command's default while an explicit update_ratio: 0
// disables the toggle; it is the one knob where 0 reads as an on/off switch
// rather than a degenerate value.
//
// BucketWidth is the width of one time bucket of a run without an end, which
// only --duration 0 selects; zero means unset, and a bounded run ignores it.
type Chaos struct {
	Duration    Duration `yaml:"duration"`
	Interval    Interval `yaml:"interval"`
	Parallel    Parallel `yaml:"parallel"`
	ChurnRatio  float64  `yaml:"churn_ratio"`
	TargetFill  float64  `yaml:"target_fill"`
	UpdateRatio *float64 `yaml:"update_ratio"`
	BucketWidth Duration `yaml:"bucket_width"`
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

// Resources holds the fixed counts of resources to create. Every listener gets
// exactly one default pool, so Listeners is also the pool count and the schema
// has no pools key.
type Resources struct {
	// Networks is the number of VIP networks, each with one /24 subnet.
	Networks int `yaml:"networks"`
	// LoadBalancers is the number of load balancers, assigned round-robin over
	// the VIP networks.
	LoadBalancers int `yaml:"loadbalancers"`
	// Listeners is the number of TCP listeners, assigned round-robin over the
	// load balancers, and the number of pools.
	Listeners int `yaml:"listeners"`
	// Members is the number of synthetic members, assigned round-robin over the
	// pools.
	Members int `yaml:"members"`
	// HealthMonitors is the number of pools that get a TCP health monitor, drawn
	// from all pools.
	HealthMonitors int `yaml:"health_monitors"`
}

// Distribution holds the port ranges drawn from per listener and per member.
type Distribution struct {
	// ListenerPort is the TCP port drawn per listener. A port another listener of
	// the same load balancer already holds steps to the next free one.
	ListenerPort Range `yaml:"listener_port"`
	// MemberPort is the TCP port drawn per member.
	MemberPort Range `yaml:"member_port"`
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

// maxCount caps every resource count. It bounds the slice preallocations and
// the one pass Validate makes over the members, mirroring the other services'
// scenarios' cap.
const maxCount = 1_000_000

// maxNetworks caps the VIP network count: network i takes 10.0.<i+1>.0/24, so
// the third octet runs out after 10.0.255.0/24.
const maxNetworks = 255

// Inclusive bounds of a listener or member port.
const (
	minPort = 1
	maxPort = 65535
)

// memberAddresses is the number of member addresses one VIP network holds: .128
// to .254, the upper half of its /24 without the broadcast address. The lower
// half belongs to the gateway and the addresses Neutron allocates.
const memberAddresses = 127

// Validate checks the scenario for semantic consistency, returning an actionable
// error that names the offending field. Beyond the bounds of each value, it
// enforces the relations Generate relies on: every round-robin of the
// assignment has a non-zero divisor, every load balancer gets a listener and
// every pool a member, the listener port range holds a distinct port for every
// listener of one load balancer, and no VIP network gets more members than its
// upper half has addresses. A chaos block, when present, is checked last.
func (s Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name must not be empty")
	}

	r := s.Resources
	for _, c := range []struct {
		key   string
		count int
	}{
		{"resources.networks", r.Networks},
		{"resources.loadbalancers", r.LoadBalancers},
		{"resources.listeners", r.Listeners},
		{"resources.members", r.Members},
		{"resources.health_monitors", r.HealthMonitors},
	} {
		if c.count < 0 {
			return fmt.Errorf("%s must not be negative, got %d", c.key, c.count)
		}
		if c.count > maxCount {
			return fmt.Errorf("%s exceeds the limit of %d, got %d", c.key, maxCount, c.count)
		}
	}

	if r.Networks > maxNetworks {
		return fmt.Errorf("resources.networks exceeds the limit of %d, got %d", maxNetworks, r.Networks)
	}

	// Generate puts load balancers round-robin on networks, listeners on load
	// balancers, and members on pools (one per listener). Each guard keeps one of
	// those round-robins off a zero divisor.
	if r.LoadBalancers > 0 && r.Networks < 1 {
		return fmt.Errorf("resources.networks must be at least 1 when resources.loadbalancers > 0, got %d", r.Networks)
	}
	if r.Listeners > 0 && r.LoadBalancers < 1 {
		return fmt.Errorf("resources.loadbalancers must be at least 1 when resources.listeners > 0, got %d", r.LoadBalancers)
	}
	if r.Members > 0 && r.Listeners < 1 {
		return fmt.Errorf("resources.listeners must be at least 1 when resources.members > 0, got %d", r.Listeners)
	}

	// Every load balancer gets a listener and every pool a member, and a pool
	// carries at most one health monitor.
	if r.Listeners < r.LoadBalancers {
		return fmt.Errorf("resources.listeners (%d) must be at least resources.loadbalancers (%d)", r.Listeners, r.LoadBalancers)
	}
	if r.Members < r.Listeners {
		return fmt.Errorf("resources.members (%d) must be at least resources.listeners (%d)", r.Members, r.Listeners)
	}
	if r.HealthMonitors > r.Listeners {
		return fmt.Errorf("resources.health_monitors (%d) exceeds resources.listeners (%d)", r.HealthMonitors, r.Listeners)
	}

	if err := validateRange("distribution.listener_port", s.Distribution.ListenerPort); err != nil {
		return err
	}
	if err := validateRange("distribution.member_port", s.Distribution.MemberPort); err != nil {
		return err
	}

	// The listeners of one load balancer need distinct ports, so the range must
	// hold one for each listener the busiest load balancer gets.
	if r.LoadBalancers > 0 {
		span := s.Distribution.ListenerPort.Max - s.Distribution.ListenerPort.Min + 1
		perLoadBalancer := (r.Listeners + r.LoadBalancers - 1) / r.LoadBalancers
		if span < perLoadBalancer {
			return fmt.Errorf("distribution.listener_port spans %d ports, fewer than the %d listeners one load balancer gets", span, perLoadBalancer)
		}
	}

	for i, count := range r.membersPerNetwork() {
		if count > memberAddresses {
			return fmt.Errorf("resources.members puts %d members on VIP network %s, more than the %d member addresses in the upper half of its /24", count, networkName(i), memberAddresses)
		}
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
	if c.UpdateRatio != nil && (*c.UpdateRatio < 0 || *c.UpdateRatio > 1) {
		return fmt.Errorf("chaos.update_ratio must be between 0 and 1, got %v", *c.UpdateRatio)
	}
	return nil
}

// membersPerNetwork returns how many members Generate puts on each VIP network,
// indexed like the networks. It follows the generator's round-robin in one pass
// over the members: member m belongs to pool m % listeners, pool i to listener
// i, listener i to load balancer i % loadbalancers, and load balancer i to
// network i % networks. Validate's zero-divisor guards run first, so every
// divisor is positive whenever a member exists.
func (r Resources) membersPerNetwork() []int {
	counts := make([]int, r.Networks)
	for m := 0; m < r.Members; m++ {
		counts[((m%r.Listeners)%r.LoadBalancers)%r.Networks]++
	}
	return counts
}

// networkName returns the logical name of the i-th (0-based) VIP network.
func networkName(i int) string {
	return fmt.Sprintf("net-%04d", i+1)
}

// Set applies a single dotted-key override of the form key=value, matching the
// documented scenario fields. It returns an error for an unknown key or a value
// that does not parse to the field's type. Any provider string is accepted,
// and an empty one selects the cloud's default. The chaos block has no override
// keys; the chaos command's flags override its values.
func (s *Scenario) Set(key, value string) error {
	switch key {
	case "seed":
		return setInt64(&s.Seed, key, value)
	case "provider":
		s.Provider = value
		return nil
	case "resources.networks":
		return setInt(&s.Resources.Networks, key, value)
	case "resources.loadbalancers":
		return setInt(&s.Resources.LoadBalancers, key, value)
	case "resources.listeners":
		return setInt(&s.Resources.Listeners, key, value)
	case "resources.members":
		return setInt(&s.Resources.Members, key, value)
	case "resources.health_monitors":
		return setInt(&s.Resources.HealthMonitors, key, value)
	case "distribution.listener_port.min":
		return setInt(&s.Distribution.ListenerPort.Min, key, value)
	case "distribution.listener_port.max":
		return setInt(&s.Distribution.ListenerPort.Max, key, value)
	case "distribution.member_port.min":
		return setInt(&s.Distribution.MemberPort.Min, key, value)
	case "distribution.member_port.max":
		return setInt(&s.Distribution.MemberPort.Max, key, value)
	default:
		return fmt.Errorf("unknown override key %q", key)
	}
}

// validateRange enforces minPort <= Min <= Max <= maxPort for a named port
// range, checking Min's bounds, then Max's, then their order.
func validateRange(key string, r Range) error {
	if r.Min < minPort || r.Min > maxPort {
		return fmt.Errorf("%s.min must be between %d and %d, got %d", key, minPort, maxPort, r.Min)
	}
	if r.Max < minPort || r.Max > maxPort {
		return fmt.Errorf("%s.max must be between %d and %d, got %d", key, minPort, maxPort, r.Max)
	}
	if r.Min > r.Max {
		return fmt.Errorf("%s.min (%d) must not exceed %s.max (%d)", key, r.Min, key, r.Max)
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
