// Package scenario defines the human-authored YAML scenario format (counts,
// ratios, distributions, topology, seed) and the deterministic generator that
// expands a scenario plus its seed into a fully-enumerated plan. The same
// scenario and seed always yield a byte-identical plan.
package scenario

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v2"
)

// Scenario is the parametrized description of a desired topology. It is parsed
// from YAML, validated, optionally overridden via Set, and expanded into a plan
// by Generate.
type Scenario struct {
	Name         string       `yaml:"name"`
	Seed         int64        `yaml:"seed"`
	Resources    Resources    `yaml:"resources"`
	Distribution Distribution `yaml:"distribution"`
	Topology     Topology     `yaml:"topology"`
}

// Resources holds the fixed counts of top-level resources to create.
type Resources struct {
	SubnetPools    int `yaml:"subnet_pools"`
	AddressScopes  int `yaml:"address_scopes"`
	Networks       int `yaml:"networks"`
	Routers        int `yaml:"routers"`
	SecurityGroups int `yaml:"security_groups"`
	// RouterLinks is the number of router-to-router interconnects: each one adds
	// a dedicated transit network, subnet, and port and wires two routers
	// together. Requires at least two routers.
	RouterLinks int `yaml:"router_links"`
	// FloatingIPs is the number of floating IPs to allocate from the external
	// network. They are created only when an external network is available at
	// apply time.
	FloatingIPs int `yaml:"floating_ips"`
}

// Distribution holds the per-parent count ranges and the ratios that shape how
// subnets, ports, and rules are spread across their parents.
type Distribution struct {
	SubnetsPerNetwork            Range   `yaml:"subnets_per_network"`
	PortsPerNetwork              Range   `yaml:"ports_per_network"`
	RulesPerSecurityGroup        Range   `yaml:"rules_per_security_group"`
	SubnetFromPoolRatio          float64 `yaml:"subnet_from_pool_ratio"`
	IPv6Ratio                    float64 `yaml:"ipv6_ratio"`
	SubnetsAttachedToRouterRatio float64 `yaml:"subnets_attached_to_router_ratio"`
	// RoutersWithExternalGatewayRatio is the fraction of routers that intend to
	// plug into an external network. Whether a gateway is actually attached
	// depends on an external network being available at apply time.
	RoutersWithExternalGatewayRatio float64 `yaml:"routers_with_external_gateway_ratio"`
	// FloatingIPAssociatedRatio is the fraction of floating IPs that target an
	// internal port reachable through an external-gateway router; the rest are
	// allocated but left unassociated.
	FloatingIPAssociatedRatio float64 `yaml:"floating_ip_associated_ratio"`
}

// Topology holds the shape controls for how resources relate to one another.
type Topology struct {
	RouterAttachStrategy   string `yaml:"router_attach_strategy"`
	PortSecurityGroupCount Range  `yaml:"port_security_group_count"`
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

// maxCount caps every resource count and range maximum. It keeps the
// generator's slice preallocations and per-port shuffles bounded and guards
// randRange's interval arithmetic against integer overflow: with no upper
// bound a count of MaxInt would make r.Max-r.Min+1 wrap negative and panic
// rng.Intn, and a make([]T, 0, count) of billions would OOM or panic makeslice.
const maxCount = 1_000_000

// Validate checks the scenario for semantic consistency, returning an
// actionable error that names the offending field.
func (s Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name must not be empty")
	}

	for _, c := range []struct {
		key   string
		value int
	}{
		{"resources.subnet_pools", s.Resources.SubnetPools},
		{"resources.address_scopes", s.Resources.AddressScopes},
		{"resources.networks", s.Resources.Networks},
		{"resources.routers", s.Resources.Routers},
		{"resources.security_groups", s.Resources.SecurityGroups},
		{"resources.router_links", s.Resources.RouterLinks},
		{"resources.floating_ips", s.Resources.FloatingIPs},
	} {
		if c.value < 0 {
			return fmt.Errorf("%s must not be negative, got %d", c.key, c.value)
		}
		if c.value > maxCount {
			return fmt.Errorf("%s exceeds the limit of %d, got %d", c.key, maxCount, c.value)
		}
	}

	for _, c := range []struct {
		key string
		r   Range
	}{
		{"distribution.subnets_per_network", s.Distribution.SubnetsPerNetwork},
		{"distribution.ports_per_network", s.Distribution.PortsPerNetwork},
		{"distribution.rules_per_security_group", s.Distribution.RulesPerSecurityGroup},
		{"topology.port_security_group_count", s.Topology.PortSecurityGroupCount},
	} {
		if err := validateRange(c.key, c.r); err != nil {
			return err
		}
	}

	for _, c := range []struct {
		key   string
		value float64
	}{
		{"distribution.subnet_from_pool_ratio", s.Distribution.SubnetFromPoolRatio},
		{"distribution.ipv6_ratio", s.Distribution.IPv6Ratio},
		{"distribution.subnets_attached_to_router_ratio", s.Distribution.SubnetsAttachedToRouterRatio},
		{"distribution.routers_with_external_gateway_ratio", s.Distribution.RoutersWithExternalGatewayRatio},
		{"distribution.floating_ip_associated_ratio", s.Distribution.FloatingIPAssociatedRatio},
	} {
		if c.value < 0 || c.value > 1 {
			return fmt.Errorf("%s must be between 0 and 1, got %v", c.key, c.value)
		}
	}

	if s.Distribution.SubnetFromPoolRatio > 0 && s.Resources.SubnetPools == 0 {
		return fmt.Errorf("subnet_from_pool_ratio is %v but resources.subnet_pools is 0", s.Distribution.SubnetFromPoolRatio)
	}

	if s.Resources.RouterLinks > 0 && s.Resources.Routers < 2 {
		return fmt.Errorf("resources.router_links is %d but needs at least 2 routers, got %d", s.Resources.RouterLinks, s.Resources.Routers)
	}

	switch s.Topology.RouterAttachStrategy {
	case "", "random":
	default:
		return fmt.Errorf("topology.router_attach_strategy %q is not supported, want \"random\"", s.Topology.RouterAttachStrategy)
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
	case "resources.subnet_pools":
		return setInt(&s.Resources.SubnetPools, key, value)
	case "resources.address_scopes":
		return setInt(&s.Resources.AddressScopes, key, value)
	case "resources.networks":
		return setInt(&s.Resources.Networks, key, value)
	case "resources.routers":
		return setInt(&s.Resources.Routers, key, value)
	case "resources.security_groups":
		return setInt(&s.Resources.SecurityGroups, key, value)
	case "resources.router_links":
		return setInt(&s.Resources.RouterLinks, key, value)
	case "resources.floating_ips":
		return setInt(&s.Resources.FloatingIPs, key, value)
	case "distribution.subnets_per_network.min":
		return setInt(&s.Distribution.SubnetsPerNetwork.Min, key, value)
	case "distribution.subnets_per_network.max":
		return setInt(&s.Distribution.SubnetsPerNetwork.Max, key, value)
	case "distribution.ports_per_network.min":
		return setInt(&s.Distribution.PortsPerNetwork.Min, key, value)
	case "distribution.ports_per_network.max":
		return setInt(&s.Distribution.PortsPerNetwork.Max, key, value)
	case "distribution.rules_per_security_group.min":
		return setInt(&s.Distribution.RulesPerSecurityGroup.Min, key, value)
	case "distribution.rules_per_security_group.max":
		return setInt(&s.Distribution.RulesPerSecurityGroup.Max, key, value)
	case "distribution.subnet_from_pool_ratio":
		return setFloat(&s.Distribution.SubnetFromPoolRatio, key, value)
	case "distribution.ipv6_ratio":
		return setFloat(&s.Distribution.IPv6Ratio, key, value)
	case "distribution.subnets_attached_to_router_ratio":
		return setFloat(&s.Distribution.SubnetsAttachedToRouterRatio, key, value)
	case "distribution.routers_with_external_gateway_ratio":
		return setFloat(&s.Distribution.RoutersWithExternalGatewayRatio, key, value)
	case "distribution.floating_ip_associated_ratio":
		return setFloat(&s.Distribution.FloatingIPAssociatedRatio, key, value)
	case "topology.router_attach_strategy":
		s.Topology.RouterAttachStrategy = value
		return nil
	case "topology.port_security_group_count.min":
		return setInt(&s.Topology.PortSecurityGroupCount.Min, key, value)
	case "topology.port_security_group_count.max":
		return setInt(&s.Topology.PortSecurityGroupCount.Max, key, value)
	default:
		return fmt.Errorf("unknown override key %q", key)
	}
}

// validateRange enforces 0 <= Min <= Max for a named range.
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
