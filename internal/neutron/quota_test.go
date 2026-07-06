package neutron

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/quotas"

	"github.com/B42Labs/dizzy/internal/plan"
)

// TestPlanNeedsCounts confirms planNeeds tallies each kind: it sums nested
// security-group rules, counts subnet-based router interfaces (but not
// port-based ones, which attach an already-counted port) against the port
// quota, and — only when an external network is available — adds an
// external-gateway port per gateway router and counts the floating IPs.
func TestPlanNeedsCounts(t *testing.T) {
	p := &plan.Plan{
		Networks:    []plan.Network{{Name: "n1"}, {Name: "n2"}},
		Subnets:     []plan.Subnet{{Name: "s1"}},
		Routers:     []plan.Router{{Name: "r1"}, {Name: "r2", ExternalGateway: true}},
		SubnetPools: []plan.SubnetPool{{Name: "p1"}},
		RouterInterfaces: []plan.RouterInterface{
			{Name: "ri1", Router: "r1", Subnet: "s1"},  // subnet-based: +1 gateway port
			{Name: "ri2", Router: "r2", Port: "port1"}, // port-based: attaches an existing port
		},
		SecurityGroups: []plan.SecurityGroup{
			{Name: "sg1", Rules: []plan.SecurityGroupRule{{Direction: "ingress"}, {Direction: "egress"}}},
			{Name: "sg2", Rules: []plan.SecurityGroupRule{{Direction: "ingress"}}},
		},
		Ports:       []plan.Port{{Name: "port1"}},
		FloatingIPs: []plan.FloatingIP{{Name: "fip1"}},
	}

	// With an external network: the gateway router adds a port and the floating
	// IP counts.
	got := planNeeds(p, true)
	want := needs{
		networks:       2,
		subnets:        1,
		routers:        2,
		securityGroups: 2,
		securityRules:  3,
		ports:          3, // 1 explicit port + 1 subnet-interface port + 1 gateway port
		subnetPools:    1,
		floatingIPs:    1,
	}
	if got != want {
		t.Errorf("planNeeds(external) = %+v, want %+v", got, want)
	}

	// Without an external network: no gateway port, no floating IPs.
	gotNoExt := planNeeds(p, false)
	wantNoExt := want
	wantNoExt.ports = 2 // 1 explicit port + 1 subnet-interface port
	wantNoExt.floatingIPs = 0
	if gotNoExt != wantNoExt {
		t.Errorf("planNeeds(no external) = %+v, want %+v", gotNoExt, wantNoExt)
	}
}

// TestCheckQuotaBlocksOversized covers the "an oversized plan is blocked up
// front" acceptance criterion: checkQuota returns an itemized error naming every
// exceeded resource type.
func TestCheckQuotaBlocksOversized(t *testing.T) {
	need := needs{networks: 100, subnets: 200, routers: 20, securityGroups: 15, securityRules: 200, ports: 300, subnetPools: 3}
	q := &quotas.Quota{Network: 10, Subnet: 10, Router: 10, SecurityGroup: 10, SecurityGroupRule: 100, Port: 50, SubnetPool: 10}

	err := checkQuota(need, q)
	if err == nil {
		t.Fatal("expected an over-quota error, got nil")
	}
	for _, want := range []string{"networks", "subnets", "routers", "security groups", "security group rules", "ports"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not itemize %q", err.Error(), want)
		}
	}
	// Subnet pools fit (3 <= 10) and must not be reported.
	if strings.Contains(err.Error(), "subnet pools") {
		t.Errorf("error %q reported subnet pools, which fit within quota", err.Error())
	}
}

func TestCheckQuotaPassesWhenSufficient(t *testing.T) {
	need := needs{networks: 5, subnets: 8, routers: 2, securityGroups: 3, securityRules: 20, ports: 40, subnetPools: 1}
	q := &quotas.Quota{Network: 10, Subnet: 10, Router: 10, SecurityGroup: 10, SecurityGroupRule: 100, Port: 50, SubnetPool: 10}

	if err := checkQuota(need, q); err != nil {
		t.Errorf("checkQuota with sufficient quota returned %v, want nil", err)
	}
}

// TestCheckQuotaTreatsNegativeAsUnlimited confirms a -1 limit (Neutron's
// unlimited convention) never blocks, even for a large plan.
func TestCheckQuotaTreatsNegativeAsUnlimited(t *testing.T) {
	need := needs{networks: 100000, ports: 100000}
	q := &quotas.Quota{Network: -1, Subnet: -1, Router: -1, SecurityGroup: -1, SecurityGroupRule: -1, Port: -1, SubnetPool: -1}

	if err := checkQuota(need, q); err != nil {
		t.Errorf("unlimited quota returned %v, want nil", err)
	}
}

// quotaPrecheckClient builds a ServiceClient whose quota reads hit ts and whose
// auth result yields a project id, so PrecheckQuota reaches the quota read.
func quotaPrecheckClient(ts *httptest.Server) *gophercloud.ServiceClient {
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	ar := tokens.CreateResult{}
	ar.Body = map[string]any{"token": map[string]any{"project": map[string]any{"id": "proj-1"}}}
	_ = gc.SetTokenAndAuthResult(ar)
	return gc
}

// TestPrecheckQuotaSurfacesTransientReadError covers the early-abort guarantee:
// a transient quota-read failure (503) must surface as an error so the plan
// aborts before creating anything, not fail open and let the apply proceed.
func TestPrecheckQuotaSurfacesTransientReadError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	gc := quotaPrecheckClient(ts)
	err := PrecheckQuota(context.Background(), gc, &plan.Plan{Networks: []plan.Network{{Name: "n1"}}}, false)
	if err == nil {
		t.Fatal("PrecheckQuota fell open on a transient 503 quota read; want a surfaced error")
	}
}

// TestPrecheckQuotaFailsOpenWhenReadDenied confirms the one documented fail-open
// case still holds: a 403 quota read denial (a common non-admin restriction) is
// skipped with a warning rather than aborting the apply.
func TestPrecheckQuotaFailsOpenWhenReadDenied(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	gc := quotaPrecheckClient(ts)
	if err := PrecheckQuota(context.Background(), gc, &plan.Plan{Networks: []plan.Network{{Name: "n1"}}}, false); err != nil {
		t.Errorf("PrecheckQuota should fail open on a 403 quota read denial, got %v", err)
	}
}
