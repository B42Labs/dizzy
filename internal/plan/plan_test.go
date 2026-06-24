package plan

import (
	"strings"
	"testing"
)

// validPlan returns a small, internally consistent plan that exercises every
// reference kind Validate checks. Tests mutate a copy to provoke each failure.
func validPlan() *Plan {
	return &Plan{
		Scenario:      "test",
		Seed:          1,
		AddressScopes: []AddressScope{{Name: "as-0001", IPVersion: 4}},
		SubnetPools: []SubnetPool{{
			Name:             "pool-0001",
			IPVersion:        4,
			Prefixes:         []string{"172.16.0.0/16"},
			DefaultPrefixLen: 26,
			AddressScope:     "as-0001",
		}},
		Networks: []Network{{Name: "net-0001"}},
		Subnets: []Subnet{
			{Name: "subnet-0001", Network: "net-0001", IPVersion: 4, CIDR: "10.0.0.0/24"},
			{Name: "subnet-0002", Network: "net-0001", IPVersion: 4, SubnetPool: "pool-0001", PrefixLen: 26},
		},
		Routers:          []Router{{Name: "router-0001"}},
		RouterInterfaces: []RouterInterface{{Name: "rif-0001", Router: "router-0001", Subnet: "subnet-0001"}},
		SecurityGroups: []SecurityGroup{{
			Name: "sg-0001",
			Rules: []SecurityGroupRule{
				{Direction: "ingress", EtherType: "IPv4", Protocol: "tcp", PortRangeMin: 22, PortRangeMax: 22, RemoteIPPrefix: "0.0.0.0/0"},
				{Direction: "ingress", EtherType: "IPv4", Protocol: "tcp", RemoteGroup: "sg-0001"},
			},
		}},
		Ports: []Port{{
			Name:           "port-0001",
			Network:        "net-0001",
			FixedIPs:       []FixedIP{{Subnet: "subnet-0001"}},
			SecurityGroups: []string{"sg-0001"},
		}},
	}
}

func TestPlanValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(p *Plan)
		wantErr string
	}{
		{
			name:   "valid plan",
			mutate: func(*Plan) {},
		},
		{
			name: "subnet attached to two routers",
			mutate: func(p *Plan) {
				p.RouterInterfaces = append(p.RouterInterfaces,
					RouterInterface{Name: "rif-0002", Router: "router-0001", Subnet: "subnet-0001"})
			},
			wantErr: `subnet "subnet-0001" is attached to more than one router`,
		},
		{
			name: "port-based router interface is valid",
			mutate: func(p *Plan) {
				p.Ports = append(p.Ports, Port{Name: "port-link", Network: "net-0001", FixedIPs: []FixedIP{{Subnet: "subnet-0001"}}})
				p.RouterInterfaces = append(p.RouterInterfaces,
					RouterInterface{Name: "rif-link", Router: "router-0001", Port: "port-link"})
			},
		},
		{
			name: "router interface sets both subnet and port",
			mutate: func(p *Plan) {
				p.RouterInterfaces[0].Port = "port-0001"
			},
			wantErr: `router interface "rif-0001" sets both a subnet and a port; set exactly one`,
		},
		{
			name: "router interface sets neither subnet nor port",
			mutate: func(p *Plan) {
				p.RouterInterfaces[0].Subnet = ""
			},
			wantErr: `router interface "rif-0001" sets neither a subnet nor a port; set exactly one`,
		},
		{
			name: "router interface references unknown port",
			mutate: func(p *Plan) {
				p.RouterInterfaces[0].Subnet = ""
				p.RouterInterfaces[0].Port = "port-9999"
			},
			wantErr: `router interface "rif-0001" references unknown port "port-9999"`,
		},
		{
			name: "port attached to two routers",
			mutate: func(p *Plan) {
				p.RouterInterfaces[0].Subnet = ""
				p.RouterInterfaces[0].Port = "port-0001"
				p.RouterInterfaces = append(p.RouterInterfaces,
					RouterInterface{Name: "rif-0002", Router: "router-0001", Port: "port-0001"})
			},
			wantErr: `port "port-0001" is attached to more than one router`,
		},
		{
			name: "floating ip associated with a port is valid",
			mutate: func(p *Plan) {
				p.FloatingIPs = []FloatingIP{{Name: "fip-0001", Port: "port-0001"}}
			},
		},
		{
			name: "floating ip references unknown port",
			mutate: func(p *Plan) {
				p.FloatingIPs = []FloatingIP{{Name: "fip-0001", Port: "port-9999"}}
			},
			wantErr: `floating ip "fip-0001" references unknown port "port-9999"`,
		},
		{
			name: "subnet references unknown network",
			mutate: func(p *Plan) {
				p.Subnets[0].Network = "net-9999"
			},
			wantErr: `subnet "subnet-0001" references unknown network "net-9999"`,
		},
		{
			name: "subnet references unknown pool",
			mutate: func(p *Plan) {
				p.Subnets[1].SubnetPool = "pool-9999"
			},
			wantErr: `subnet "subnet-0002" references unknown subnet pool "pool-9999"`,
		},
		{
			name: "subnet pool references unknown address scope",
			mutate: func(p *Plan) {
				p.SubnetPools[0].AddressScope = "as-9999"
			},
			wantErr: `subnet pool "pool-0001" references unknown address scope "as-9999"`,
		},
		{
			name: "router interface references unknown router",
			mutate: func(p *Plan) {
				p.RouterInterfaces[0].Router = "router-9999"
			},
			wantErr: `router interface "rif-0001" references unknown router "router-9999"`,
		},
		{
			name: "router interface references unknown subnet",
			mutate: func(p *Plan) {
				p.RouterInterfaces[0].Subnet = "subnet-9999"
			},
			wantErr: `router interface "rif-0001" references unknown subnet "subnet-9999"`,
		},
		{
			name: "rule references unknown remote group",
			mutate: func(p *Plan) {
				p.SecurityGroups[0].Rules[1].RemoteGroup = "sg-9999"
			},
			wantErr: `security group "sg-0001" has a rule referencing unknown remote group "sg-9999"`,
		},
		{
			name: "port references unknown network",
			mutate: func(p *Plan) {
				p.Ports[0].Network = "net-9999"
			},
			wantErr: `port "port-0001" references unknown network "net-9999"`,
		},
		{
			name: "port references unknown subnet",
			mutate: func(p *Plan) {
				p.Ports[0].FixedIPs[0].Subnet = "subnet-9999"
			},
			wantErr: `port "port-0001" references unknown subnet "subnet-9999"`,
		},
		{
			name: "port references unknown security group",
			mutate: func(p *Plan) {
				p.Ports[0].SecurityGroups[0] = "sg-9999"
			},
			wantErr: `port "port-0001" references unknown security group "sg-9999"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := validPlan()
			tc.mutate(p)

			err := p.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error %q", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("Validate() = %q, want %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestPlanSummary(t *testing.T) {
	p := validPlan()
	got := p.Summary()

	for _, want := range []string{
		`scenario "test" (seed 1)`,
		"address scopes:    1",
		"subnet pools:      1",
		"networks:          1",
		"subnets:           2",
		"routers:           1",
		"router interfaces: 1",
		"security groups:   1",
		"ports:             1",
		"floating IPs:      0",
		"with external gateway",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary() missing %q in:\n%s", want, got)
		}
	}
}
