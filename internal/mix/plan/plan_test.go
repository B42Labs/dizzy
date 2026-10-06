package plan

import (
	"encoding/json"
	"strings"
	"testing"

	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
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
