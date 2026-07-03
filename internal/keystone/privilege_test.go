package keystone

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"

	keystoneplan "github.com/B42Labs/openstack-tester/internal/keystone/plan"
	"github.com/B42Labs/openstack-tester/internal/metrics"
)

// createResult builds a tokens.CreateResult whose Body is the parsed token
// payload, the shape ClassifyPrivilege reads roles and scope from.
func createResult(t *testing.T, body string) tokens.CreateResult {
	t.Helper()
	var parsed any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("parsing token body: %v", err)
	}
	var r tokens.CreateResult
	r.Body = parsed
	return r
}

// gcWithAuth builds a ServiceClient whose cached auth result is the given token
// payload, so ClassifyPrivilege reads it without a network round-trip.
func gcWithAuth(t *testing.T, body string) *gophercloud.ServiceClient {
	t.Helper()
	gc := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}}
	if err := gc.SetTokenAndAuthResult(createResult(t, body)); err != nil {
		t.Fatalf("setting auth result: %v", err)
	}
	return gc
}

func TestClassifyPrivilege(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantTier   Tier
		wantDomain string
		wantErr    bool
	}{
		{
			name:     "admin project-scoped",
			body:     `{"token":{"roles":[{"id":"r1","name":"admin"}],"project":{"id":"p1","name":"proj","domain":{"id":"d1","name":"dom"}}}}`,
			wantTier: TierAdmin,
		},
		{
			name:     "admin domain-scoped",
			body:     `{"token":{"roles":[{"id":"r1","name":"admin"}],"domain":{"id":"d1","name":"dom"}}}`,
			wantTier: TierAdmin,
		},
		{
			name:     "admin system-scoped",
			body:     `{"token":{"roles":[{"id":"r1","name":"admin"}],"system":{"all":true}}}`,
			wantTier: TierAdmin,
		},
		{
			name:       "manager domain-scoped",
			body:       `{"token":{"roles":[{"id":"r2","name":"manager"},{"id":"r3","name":"reader"}],"domain":{"id":"d9","name":"managed"}}}`,
			wantTier:   TierDomainManager,
			wantDomain: "d9",
		},
		{
			name:    "manager project-scoped is rejected",
			body:    `{"token":{"roles":[{"id":"r2","name":"manager"}],"project":{"id":"p1","name":"proj","domain":{"id":"d1","name":"dom"}}}}`,
			wantErr: true,
		},
		{
			name:    "reader only is rejected",
			body:    `{"token":{"roles":[{"id":"r4","name":"reader"}],"domain":{"id":"d1","name":"dom"}}}`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls, err := ClassifyPrivilege(context.Background(), gcWithAuth(t, tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatal("ClassifyPrivilege = nil error, want a fail-fast error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ClassifyPrivilege: %v", err)
			}
			if cls.Tier != tc.wantTier {
				t.Errorf("tier = %q, want %q", cls.Tier, tc.wantTier)
			}
			if cls.DomainID != tc.wantDomain {
				t.Errorf("domain = %q, want %q", cls.DomainID, tc.wantDomain)
			}
		})
	}
}

// TestClassifyPrivilegeNamesRolesSeen confirms the fail-fast error names the
// roles the caller does carry, so an operator can see why they were rejected.
func TestClassifyPrivilegeNamesRolesSeen(t *testing.T) {
	_, err := ClassifyPrivilege(context.Background(),
		gcWithAuth(t, `{"token":{"roles":[{"id":"r","name":"reader"}],"domain":{"id":"d","name":"dom"}}}`))
	if err == nil {
		t.Fatal("expected a rejection error")
	}
	if !strings.Contains(err.Error(), "reader") {
		t.Errorf("error %q does not name the role the caller carries", err)
	}
}

// TestClassifyFallsBackToTokenGet confirms that when no usable auth result is
// cached, classification self-validates via GET /v3/auth/tokens.
func TestClassifyFallsBackToTokenGet(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/auth/tokens") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":{"roles":[{"id":"r1","name":"admin"}],"domain":{"id":"d1","name":"dom"}}}`)
	}))
	defer ts.Close()

	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{TokenID: "subject-token"},
		Endpoint:       ts.URL + "/",
	}
	cls, err := ClassifyPrivilege(context.Background(), gc)
	if err != nil {
		t.Fatalf("ClassifyPrivilege (fallback): %v", err)
	}
	if cls.Tier != TierAdmin {
		t.Errorf("tier = %q, want admin from the self-validation fallback", cls.Tier)
	}
}

func TestResolveDomainManagerRejectsAdminRole(t *testing.T) {
	c := New(&gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}}, "run0", metrics.NewCollector())
	cls := Classification{Tier: TierDomainManager, DomainID: "d1", DomainName: "dom"}
	_, err := ResolveDomainManager(context.Background(), c, "", []string{"member", "admin"}, cls)
	if err == nil {
		t.Fatal("ResolveDomainManager with admin in the role list: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "admin") {
		t.Errorf("error %q does not explain the admin rejection", err)
	}
}

// TestResolveDomainManagerDomainFlag confirms --domain resolves the in-scope
// domain by name and the roles resolve to their cloud ids.
func TestResolveDomainManagerDomainFlag(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/domains"):
			_, _ = io.WriteString(w, `{"domains":[{"id":"dm-1","name":"managed"}]}`)
		case strings.HasSuffix(r.URL.Path, "/roles"):
			_, _ = io.WriteString(w, `{"roles":[{"id":"role-member","name":"member","domain_id":""}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer ts.Close()

	c := New(&gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}, "run0", metrics.NewCollector())

	res, err := ResolveDomainManager(context.Background(), c, "managed", []string{"member"}, Classification{Tier: TierDomainManager})
	if err != nil {
		t.Fatalf("ResolveDomainManager: %v", err)
	}
	if res.DomainID != "dm-1" {
		t.Errorf("resolved domain = %q, want dm-1 from --domain", res.DomainID)
	}
	if len(res.RoleIDs) != 1 || res.RoleIDs[0] != "role-member" {
		t.Errorf("resolved roles = %+v, want [role-member]", res.RoleIDs)
	}
}

func TestCheckDomainManagerPlanRejectsMultiDomain(t *testing.T) {
	multi := &keystoneplan.Plan{Domains: []keystoneplan.Domain{{Name: "dom-0001"}, {Name: "dom-0002"}}}
	if err := CheckDomainManagerPlan(multi); err == nil {
		t.Fatal("CheckDomainManagerPlan with 2 domains: expected an admin-only error, got nil")
	}

	single := &keystoneplan.Plan{Domains: []keystoneplan.Domain{{Name: "dom-0001"}}}
	if err := CheckDomainManagerPlan(single); err != nil {
		t.Errorf("CheckDomainManagerPlan with 1 domain = %v, want nil", err)
	}
}
