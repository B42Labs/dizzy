package plan

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	cinderplan "github.com/B42Labs/dizzy/internal/cinder/plan"
	glanceplan "github.com/B42Labs/dizzy/internal/glance/plan"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	octaviaplan "github.com/B42Labs/dizzy/internal/octavia/plan"
	neutronplan "github.com/B42Labs/dizzy/internal/plan"
)

// novaPlan returns a valid compute plan of n servers on one network.
func novaPlan(n int) *novaplan.Plan {
	p := &novaplan.Plan{Networks: []novaplan.Network{{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"}}}
	for i := 0; i < n; i++ {
		p.Servers = append(p.Servers, novaplan.Server{Name: "srv", Networks: []string{"net-0001"}})
	}
	return p
}

func TestValidate(t *testing.T) {
	danglingNetwork := novaPlan(1)
	danglingNetwork.Servers[0].Networks = []string{"net-nope"}

	tests := []struct {
		name     string
		personas []Persona
		want     string
	}{
		{"no personas", nil, "plan has no persona with at least one server"},
		{"only empty personas", []Persona{{Name: "ci", Nova: novaPlan(0)}}, "plan has no persona with at least one server"},
		{"duplicate persona", []Persona{{Name: "ci", Servers: 1, Nova: novaPlan(1)}, {Name: "ci", Servers: 1, Nova: novaPlan(1)}}, `duplicate persona "ci"`},
		{"no compute plan", []Persona{{Name: "ci", Servers: 1}}, `persona "ci" has no compute plan`},
		{"server count mismatch", []Persona{{Name: "ci", Servers: 2, Nova: novaPlan(1)}}, `persona "ci" plans 1 servers but its share is 2`},
		{"invalid compute plan", []Persona{{Name: "ci", Servers: 1, Nova: danglingNetwork}}, `persona "ci": server "srv" references unknown network "net-nope"`},
		{"valid", []Persona{{Name: "ci", Servers: 2, Nova: novaPlan(2)}}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := (&Plan{Personas: tc.personas}).Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestPersonaLongLivedOmittedWhenFalse confirms a persona that is not
// long-lived encodes without a longLived key, so the CI persona's entry keeps
// its bytes.
func TestPersonaLongLivedOmittedWhenFalse(t *testing.T) {
	for _, tc := range []struct {
		persona Persona
		want    bool
	}{
		{Persona{Name: "ci", Servers: 1, Nova: novaPlan(1)}, false},
		{Persona{Name: "legacy", Servers: 1, LongLived: true, Nova: novaPlan(1)}, true},
	} {
		data, err := json.Marshal(tc.persona)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if got := strings.Contains(string(data), `"longLived"`); got != tc.want {
			t.Errorf("JSON %s has a longLived key = %v, want %v", data, got, tc.want)
		}
	}
}

// TestPersonaRollingOmittedWhenFalse confirms a persona that is not rolling
// encodes without a rolling key, so the CI and Legacy entries keep their
// bytes.
func TestPersonaRollingOmittedWhenFalse(t *testing.T) {
	for _, tc := range []struct {
		persona Persona
		want    bool
	}{
		{Persona{Name: "ci", Servers: 1, Nova: novaPlan(1)}, false},
		{Persona{Name: "gardener", Servers: 1, Rolling: true, Nova: novaPlan(1)}, true},
	} {
		data, err := json.Marshal(tc.persona)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if got := strings.Contains(string(data), `"rolling"`); got != tc.want {
			t.Errorf("JSON %s has a rolling key = %v, want %v", data, got, tc.want)
		}
	}
}

// gardenerPersona returns a valid gardener persona of two clusters, each one
// network and one worker, with a frontend of one load balancer per cluster on
// the cluster's network, one listener and pool each, the cluster's worker as
// the pool's member, and a health monitor on every pool.
func gardenerPersona() Persona {
	nova := &novaplan.Plan{
		Networks: []novaplan.Network{
			{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"},
			{Name: "net-0002", Subnet: "sub-0002", CIDR: "10.0.2.0/24"},
		},
		ServerGroups: []novaplan.ServerGroup{
			{Name: "grp-0001", Policy: novaplan.PolicySoftAntiAffinity},
			{Name: "grp-0002", Policy: novaplan.PolicySoftAntiAffinity},
		},
		Servers: []novaplan.Server{
			{Name: "srv-0001", Networks: []string{"net-0001"}, Group: "grp-0001"},
			{Name: "srv-0002", Networks: []string{"net-0002"}, Group: "grp-0002"},
		},
	}
	frontend := &octaviaplan.Frontend{
		LoadBalancers: []octaviaplan.LoadBalancer{{Name: "lb-0001", Network: "net-0001"}, {Name: "lb-0002", Network: "net-0002"}},
		Listeners: []octaviaplan.Listener{
			{Name: "lsn-0001", LoadBalancer: "lb-0001", Port: 80},
			{Name: "lsn-0002", LoadBalancer: "lb-0002", Port: 80},
		},
		Pools: []octaviaplan.Pool{{Name: "pool-0001", Listener: "lsn-0001"}, {Name: "pool-0002", Listener: "lsn-0002"}},
		Members: []octaviaplan.ServerMember{
			{Name: "pool-0001-member-01", Pool: "pool-0001", Server: "srv-0001", Port: 30000},
			{Name: "pool-0002-member-01", Pool: "pool-0002", Server: "srv-0002", Port: 30001},
		},
		HealthMonitors: []octaviaplan.HealthMonitor{{Name: "hm-0001", Pool: "pool-0001"}, {Name: "hm-0002", Pool: "pool-0002"}},
	}
	return Persona{Name: "gardener", Servers: 2, Rolling: true, Nova: nova, Octavia: frontend}
}

// TestValidateOctavia covers the checks Validate runs on a persona's octavia
// frontend, in their order: the opt-in, the frontend's own checks, and the
// networks and servers it takes from the compute plan, a member's load
// balancer found through its pool and listener.
func TestValidateOctavia(t *testing.T) {
	tests := []struct {
		name     string
		services []string
		mutate   func(*octaviaplan.Frontend)
		want     string
	}{
		{"valid", []string{ServiceOctavia}, func(*octaviaplan.Frontend) {}, ""},
		{"empty frontend", []string{ServiceOctavia}, func(f *octaviaplan.Frontend) { *f = octaviaplan.Frontend{} }, ""},
		{"services without octavia", nil, func(*octaviaplan.Frontend) {},
			`persona "gardener" has an octavia plan, but services does not list "octavia"`},
		{"services listing another service", []string{"barbican"}, func(*octaviaplan.Frontend) {},
			`persona "gardener" has an octavia plan, but services does not list "octavia"`},
		{"invalid frontend", []string{ServiceOctavia}, func(f *octaviaplan.Frontend) { f.Members[0].Port = 0 },
			`persona "gardener": octavia: member "pool-0001-member-01" has port 0, want 1 to 65535`},
		{"load balancer off the compute plan", []string{ServiceOctavia}, func(f *octaviaplan.Frontend) { f.LoadBalancers[0].Network = "net-0009" },
			`persona "gardener": load balancer "lb-0001" sits on network "net-0009", which the compute plan does not create`},
		{"member off the compute plan", []string{ServiceOctavia}, func(f *octaviaplan.Frontend) { f.Members[0].Server = "srv-0009" },
			`persona "gardener": member "pool-0001-member-01" references server "srv-0009", which the compute plan does not create`},
		{"member off its load balancer's network", []string{ServiceOctavia}, func(f *octaviaplan.Frontend) { f.Members[0].Server = "srv-0002" },
			`persona "gardener": member "pool-0001-member-01" puts server "srv-0002" behind load balancer "lb-0001", but the server is not on network "net-0001"`},
		{"member of the second load balancer off its network", []string{ServiceOctavia}, func(f *octaviaplan.Frontend) { f.Members[1].Server = "srv-0001" },
			`persona "gardener": member "pool-0002-member-01" puts server "srv-0001" behind load balancer "lb-0002", but the server is not on network "net-0002"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ps := gardenerPersona()
			tc.mutate(ps.Octavia)
			err := (&Plan{Services: tc.services, Personas: []Persona{ps}}).Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestValidateOctaviaWrapsFrontendError confirms the frontend's own error
// stays in the chain under the persona's prefix.
func TestValidateOctaviaWrapsFrontendError(t *testing.T) {
	ps := gardenerPersona()
	ps.Octavia.Members[0].Server = ""
	want := ps.Octavia.Validate()
	if want == nil {
		t.Fatal("Frontend.Validate() = nil for a member without a server")
	}
	err := (&Plan{Services: []string{ServiceOctavia}, Personas: []Persona{ps}}).Validate()
	if err == nil || errors.Unwrap(err) == nil || errors.Unwrap(err).Error() != want.Error() {
		t.Errorf("Validate() = %v, want an error wrapping %q", err, want)
	}
}

// TestPersonaOctaviaOmittedWhenNil confirms a persona without a frontend
// encodes without an octavia key, so a plan without the opt-in keeps its
// bytes, and one with a frontend encodes it under octavia.
func TestPersonaOctaviaOmittedWhenNil(t *testing.T) {
	for _, tc := range []struct {
		persona Persona
		want    bool
	}{
		{Persona{Name: "gardener", Servers: 1, Rolling: true, Nova: novaPlan(1)}, false},
		{gardenerPersona(), true},
	} {
		data, err := json.Marshal(tc.persona)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if got := strings.Contains(string(data), `"octavia"`); got != tc.want {
			t.Errorf("JSON %s has an octavia key = %v, want %v", data, got, tc.want)
		}
	}
}

// cinderLane returns a cinder lane whose plan has one volume of sizeGiB.
func cinderLane(sizeGiB int) Lane {
	return Lane{Name: "cinder", Seed: 1, Cinder: &cinderplan.Plan{Scenario: "small/cinder", Volumes: []cinderplan.Volume{{Name: "vol-0001", SizeGiB: sizeGiB}}}}
}

// TestValidateLanes covers the lane checks of Validate on a plan whose one
// persona is valid.
func TestValidateLanes(t *testing.T) {
	tests := []struct {
		name  string
		lanes []Lane
		want  string
	}{
		{"no lanes", nil, ""},
		{"valid lanes", []Lane{cinderLane(1), {Name: "glance", Glance: &glanceplan.Plan{}}, {Name: "keystone", Keystone: &keystoneplan.Plan{}}, {Name: "neutron", Neutron: &neutronplan.Plan{}}}, ""},
		{"duplicate lane", []Lane{cinderLane(1), cinderLane(1)}, `duplicate lane "cinder"`},
		{"unknown lane", []Lane{{Name: "swift"}}, `lane "swift" is not defined by this build of dizzy`},
		{"no plan", []Lane{{Name: "cinder"}}, `lane "cinder" has no cinder plan`},
		{"another service's plan", []Lane{{Name: "glance", Cinder: cinderLane(1).Cinder}}, `lane "glance" has no glance plan`},
		{"invalid plan", []Lane{cinderLane(0)}, `lane "cinder": volume "vol-0001" has size 0 GiB, want at least 1`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := (&Plan{Personas: []Persona{{Name: "ci", Servers: 1, Nova: novaPlan(1)}}, Lanes: tc.lanes}).Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestLaneScenario confirms Scenario reads the scenario name of the plan that
// is set, and is empty without one.
func TestLaneScenario(t *testing.T) {
	for _, tc := range []struct {
		lane Lane
		want string
	}{
		{Lane{Name: "cinder", Cinder: &cinderplan.Plan{Scenario: "small/cinder"}}, "small/cinder"},
		{Lane{Name: "glance", Glance: &glanceplan.Plan{Scenario: "small/glance"}}, "small/glance"},
		{Lane{Name: "keystone", Keystone: &keystoneplan.Plan{Scenario: "small/keystone"}}, "small/keystone"},
		{Lane{Name: "neutron", Neutron: &neutronplan.Plan{Scenario: "small/neutron"}}, "small/neutron"},
		{Lane{Name: "cinder"}, ""},
	} {
		if got := tc.lane.Scenario(); got != tc.want {
			t.Errorf("Scenario() of %s lane = %q, want %q", tc.lane.Name, got, tc.want)
		}
	}
}

// TestPlanOmitsLanesWhenEmpty confirms a plan without lanes encodes without a
// lanes key, so a plan generated before lanes existed keeps its bytes, and a
// lane encodes only the plan that is set.
func TestPlanOmitsLanesWhenEmpty(t *testing.T) {
	data, err := json.Marshal(&Plan{Personas: []Persona{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"lanes"`) {
		t.Errorf("JSON %s has a lanes key, want none", data)
	}

	data, err = json.Marshal(&Plan{Personas: []Persona{}, Lanes: []Lane{cinderLane(1)}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"lanes":[{"name":"cinder","seed":1,"cinder":{`) || strings.Contains(string(data), `"glance"`) {
		t.Errorf("JSON %s, want one cinder lane with only its cinder plan", data)
	}
}
