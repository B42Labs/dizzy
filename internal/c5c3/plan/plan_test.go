package plan

import (
	"strings"
	"testing"
	"time"
)

// samplePlan is the shape the scenario generator emits, with the values of the
// bundled small profile.
func samplePlan() *Plan {
	return &Plan{
		Scenario:     "small",
		Namespace:    "dizzy-orders",
		ControlPlane: ControlPlane{Name: "controlplane", Namespace: "openstack"},
		Orders: []Order{
			{Kind: KindUser, Name: "user"},
			{Kind: KindProject, Name: "project"},
			{Kind: KindRoleAssignment, Name: "role", User: "user", Project: "project", Role: "member"},
			{Kind: KindCatalogEntry, Name: "catalog", ServiceType: "dizzy-probe", EndpointURL: "https://dizzy-probe.invalid/v1"},
			{Kind: KindApplicationCredential, Name: "appcred", User: "user", Project: "project"},
		},
		Refusals: []Refusal{
			{Order: Order{Kind: KindApplicationCredential, Name: "refused-appcred", User: "user", Project: "project"}, Reason: "NoRoleOnProject"},
			{Order: Order{Kind: KindRoleAssignment, Name: "refused-role", User: "user", Project: "project", Role: "dizzy-refused"}, Reason: "RoleNotAllowed"},
		},
		GracePeriod:  2 * time.Minute,
		ReadyTimeout: 15 * time.Minute,
		GoneTimeout:  15 * time.Minute,
		SecretRepair: true,
	}
}

func TestValidate(t *testing.T) {
	if err := samplePlan().Validate(); err != nil {
		t.Fatalf("Validate on the generator's shape: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(p *Plan)
		want   string
	}{
		{"empty name", func(p *Plan) { p.Orders[0].Name = "" }, "empty name"},
		{"unknown kind", func(p *Plan) { p.Orders[1].Kind = "volume" }, `unknown kind "volume"`},
		{"role assignment without role", func(p *Plan) { p.Orders[2].Role = "" }, `order "role" names no role`},
		{"credential without user", func(p *Plan) { p.Orders[4].User = "" }, `order "appcred" names no user`},
		{"catalog without endpoint", func(p *Plan) { p.Orders[3].EndpointURL = "" }, "endpoint URL"},
		{"refusal without reason", func(p *Plan) { p.Refusals[0].Reason = "" }, `refusal "refused-appcred" names no reason`},
		{"user of the wrong kind", func(p *Plan) { p.Orders[2].User = "project" }, "which is a keystone_project order"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := samplePlan()
			tt.mutate(p)
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateDanglingReference(t *testing.T) {
	for _, mutate := range []func(p *Plan){
		func(p *Plan) { p.Orders[2].User = "nobody" },
		func(p *Plan) { p.Orders[4].Project = "nowhere" },
		func(p *Plan) { p.Refusals[1].Order.User = "nobody" },
	} {
		p := samplePlan()
		mutate(p)
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), "references unknown") {
			t.Errorf("Validate = %v, want a dangling-reference error", err)
		}
	}

	// A refusal is not an order another order may reference.
	p := samplePlan()
	p.Orders[2].User = "refused-appcred"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), `unknown user order "refused-appcred"`) {
		t.Errorf("Validate = %v, want a reference to a refusal rejected", err)
	}
}

func TestValidateDuplicateName(t *testing.T) {
	p := samplePlan()
	p.Orders[1].Name = "user"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), `duplicate order name "user"`) {
		t.Errorf("Validate = %v, want a duplicate within Orders rejected", err)
	}

	p = samplePlan()
	p.Refusals[0].Order.Name = "appcred"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), `duplicate order name "appcred"`) {
		t.Errorf("Validate = %v, want a duplicate across Orders and Refusals rejected", err)
	}
}

func TestSummary(t *testing.T) {
	want := `Plan for scenario "small"
  namespace:      dizzy-orders
  control plane:  openstack/controlplane
  order:          user (keystone_user)
  order:          project (keystone_project)
  order:          role (keystone_role_assignment) user=user project=project role=member
  order:          catalog (keystone_catalog_entry) service_type=dizzy-probe endpoint_url=https://dizzy-probe.invalid/v1
  order:          appcred (keystone_application_credential) user=user project=project
  refusal:        refused-appcred (keystone_application_credential) user=user project=project, expects NoRoleOnProject
  refusal:        refused-role (keystone_role_assignment) user=user project=project role=dizzy-refused, expects RoleNotAllowed
  grace period:   2m0s
  ready timeout:  15m0s
  gone timeout:   15m0s
  secret repair:  on
`
	if got := samplePlan().Summary(); got != want {
		t.Errorf("Summary =\n%s\nwant\n%s", got, want)
	}

	// Without a ControlPlane namespace the orders' own namespace is shown.
	p := samplePlan()
	p.ControlPlane.Namespace = ""
	p.SecretRepair = false
	got := p.Summary()
	if !strings.Contains(got, "  control plane:  dizzy-orders/controlplane\n") || !strings.Contains(got, "  secret repair:  off\n") {
		t.Errorf("Summary =\n%s\nwant the orders' namespace and repair off", got)
	}
}
