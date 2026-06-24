package scenario

import (
	"strings"
	"testing"
)

// mediumScenario returns the README §6 example values, a known-valid scenario
// that tests mutate to provoke individual validation failures.
func mediumScenario() Scenario {
	return Scenario{
		Name: "medium",
		Seed: 1234567,
		Resources: Resources{
			SubnetPools:    3,
			AddressScopes:  0,
			Networks:       100,
			Routers:        20,
			SecurityGroups: 15,
			RouterLinks:    5,
			FloatingIPs:    10,
		},
		Distribution: Distribution{
			SubnetsPerNetwork:               Range{Min: 1, Max: 3},
			PortsPerNetwork:                 Range{Min: 0, Max: 5},
			RulesPerSecurityGroup:           Range{Min: 2, Max: 12},
			SubnetFromPoolRatio:             0.4,
			IPv6Ratio:                       0.2,
			SubnetsAttachedToRouterRatio:    0.6,
			RoutersWithExternalGatewayRatio: 0.3,
			FloatingIPAssociatedRatio:       0.5,
		},
		Topology: Topology{
			RouterAttachStrategy:   "random",
			PortSecurityGroupCount: Range{Min: 1, Max: 3},
		},
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	const yaml = `
name: typo
resources:
  netwroks: 5
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("Parse() = nil, want error for unknown field")
	}
	if !strings.Contains(err.Error(), "netwroks") {
		t.Errorf("Parse() error %q does not name the unknown field", err.Error())
	}
}

func TestParseAcceptsMediumExample(t *testing.T) {
	const yaml = `
name: medium
seed: 1234567
resources:
  subnet_pools:   3
  address_scopes: 0
  networks:       100
  routers:        20
  security_groups: 15
  router_links:    5
  floating_ips:    10
distribution:
  subnets_per_network:   { min: 1, max: 3 }
  ports_per_network:     { min: 0, max: 5 }
  rules_per_security_group: { min: 2, max: 12 }
  subnet_from_pool_ratio: 0.4
  ipv6_ratio:            0.2
  subnets_attached_to_router_ratio: 0.6
  routers_with_external_gateway_ratio: 0.3
  floating_ip_associated_ratio:        0.5
topology:
  router_attach_strategy: random
  port_security_group_count: { min: 1, max: 3 }
`
	got, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() = %v, want nil", err)
	}
	if want := mediumScenario(); got != want {
		t.Errorf("Parse() = %+v, want %+v", got, want)
	}
}

func TestScenarioValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(s *Scenario)
		wantErr string
	}{
		{
			name:   "medium example",
			mutate: func(*Scenario) {},
		},
		{
			name:    "empty name",
			mutate:  func(s *Scenario) { s.Name = "" },
			wantErr: "name must not be empty",
		},
		{
			name:    "negative count",
			mutate:  func(s *Scenario) { s.Resources.Networks = -1 },
			wantErr: "resources.networks must not be negative",
		},
		{
			name:    "count exceeds limit",
			mutate:  func(s *Scenario) { s.Resources.Networks = 2_000_000 },
			wantErr: "resources.networks exceeds the limit of 1000000, got 2000000",
		},
		{
			name:    "range max exceeds limit",
			mutate:  func(s *Scenario) { s.Distribution.SubnetsPerNetwork = Range{Min: 0, Max: 2_000_000} },
			wantErr: "distribution.subnets_per_network.max (2000000) exceeds the limit of 1000000",
		},
		{
			name:    "range min exceeds max",
			mutate:  func(s *Scenario) { s.Distribution.SubnetsPerNetwork = Range{Min: 4, Max: 3} },
			wantErr: "distribution.subnets_per_network.min (4) must not exceed distribution.subnets_per_network.max (3)",
		},
		{
			name:    "negative range min",
			mutate:  func(s *Scenario) { s.Distribution.PortsPerNetwork.Min = -1 },
			wantErr: "distribution.ports_per_network.min must not be negative",
		},
		{
			name:    "ratio above one",
			mutate:  func(s *Scenario) { s.Distribution.IPv6Ratio = 1.5 },
			wantErr: "distribution.ipv6_ratio must be between 0 and 1",
		},
		{
			name:    "ratio below zero",
			mutate:  func(s *Scenario) { s.Distribution.SubnetsAttachedToRouterRatio = -0.1 },
			wantErr: "distribution.subnets_attached_to_router_ratio must be between 0 and 1",
		},
		{
			name: "pool ratio without pools",
			mutate: func(s *Scenario) {
				s.Resources.SubnetPools = 0
				s.Distribution.SubnetFromPoolRatio = 0.4
			},
			wantErr: "subnet_from_pool_ratio is 0.4 but resources.subnet_pools is 0",
		},
		{
			name:    "unknown attach strategy",
			mutate:  func(s *Scenario) { s.Topology.RouterAttachStrategy = "robin" },
			wantErr: `topology.router_attach_strategy "robin" is not supported`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := mediumScenario()
			tc.mutate(&s)

			err := s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestScenarioSet(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		check   func(s Scenario) bool
		wantErr string
	}{
		{
			name:  "seed",
			key:   "seed",
			value: "42",
			check: func(s Scenario) bool { return s.Seed == 42 },
		},
		{
			name:  "resources.networks",
			key:   "resources.networks",
			value: "200",
			check: func(s Scenario) bool { return s.Resources.Networks == 200 },
		},
		{
			name:  "range min",
			key:   "distribution.subnets_per_network.min",
			value: "2",
			check: func(s Scenario) bool { return s.Distribution.SubnetsPerNetwork.Min == 2 },
		},
		{
			name:  "ratio",
			key:   "distribution.ipv6_ratio",
			value: "0.5",
			check: func(s Scenario) bool { return s.Distribution.IPv6Ratio == 0.5 },
		},
		{
			name:  "attach strategy string",
			key:   "topology.router_attach_strategy",
			value: "random",
			check: func(s Scenario) bool { return s.Topology.RouterAttachStrategy == "random" },
		},
		{
			name:    "unknown key",
			key:     "resources.netwroks",
			value:   "5",
			wantErr: `unknown override key "resources.netwroks"`,
		},
		{
			name:    "non-integer value",
			key:     "resources.networks",
			value:   "many",
			wantErr: `override resources.networks: "many" is not an integer`,
		},
		{
			name:    "non-numeric ratio",
			key:     "distribution.ipv6_ratio",
			value:   "half",
			wantErr: `override distribution.ipv6_ratio: "half" is not a number`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := mediumScenario()

			err := s.Set(tc.key, tc.value)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Set(%q, %q) = nil, want error %q", tc.key, tc.value, tc.wantErr)
				}
				if err.Error() != tc.wantErr {
					t.Errorf("Set(%q, %q) = %q, want %q", tc.key, tc.value, err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set(%q, %q) = %v, want nil", tc.key, tc.value, err)
			}
			if !tc.check(s) {
				t.Errorf("Set(%q, %q) did not apply the override: %+v", tc.key, tc.value, s)
			}
		})
	}
}
