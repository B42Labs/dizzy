package plan

import (
	"encoding/json"
	"strings"
	"testing"

	cinderplan "github.com/B42Labs/dizzy/internal/cinder/plan"
	glanceplan "github.com/B42Labs/dizzy/internal/glance/plan"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
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
