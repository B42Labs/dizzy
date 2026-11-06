package octavia

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/quotas"

	"github.com/B42Labs/dizzy/internal/octavia/plan"
)

// quotaPath is the quota read of the project proj-1.
const quotaPath = "/v2.0/quotas/proj-1"

// overQuota is the exact error the large plan fails with against limits of 10
// load balancers and 100 members.
const overQuota = "plan exceeds project quota; raise these quotas before applying: loadbalancer need 15, quota 10; member need 120, quota 100"

// largePlan builds a valid plan with the counts of the bundled large profile:
// 3 networks, 15 load balancers, 30 listeners, 30 pools, 120 members, and 15
// health monitors, assigned round-robin as the generator assigns them. It is
// built from the plan types directly, so the pre-check is tested apart from the
// scenario generator.
func largePlan(t *testing.T) *plan.Plan {
	t.Helper()
	const networks, loadBalancers, listeners, members, monitors = 3, 15, 30, 120, 15
	p := &plan.Plan{Scenario: "large", Seed: 42}
	for i := range networks {
		p.Networks = append(p.Networks, plan.Network{
			Name: fmt.Sprintf("net-%04d", i+1), Subnet: fmt.Sprintf("sub-%04d", i+1), CIDR: fmt.Sprintf("10.0.%d.0/24", i+1),
		})
	}
	for i := range loadBalancers {
		p.LoadBalancers = append(p.LoadBalancers, plan.LoadBalancer{Name: fmt.Sprintf("lb-%04d", i+1), Network: p.Networks[i%networks].Name})
	}
	for i := range listeners {
		p.Listeners = append(p.Listeners, plan.Listener{Name: fmt.Sprintf("lsn-%04d", i+1), LoadBalancer: p.LoadBalancers[i%loadBalancers].Name, Port: 1024 + i})
		p.Pools = append(p.Pools, plan.Pool{Name: fmt.Sprintf("pool-%04d", i+1), Listener: p.Listeners[i].Name})
	}
	next := make([]int, networks) // next member address octet offset per network
	for m := range members {
		pool := m % listeners
		network := (pool % loadBalancers) % networks
		p.Members = append(p.Members, plan.Member{
			Name:    plan.MemberName(p.Pools[pool].Name, m/listeners+1),
			Pool:    p.Pools[pool].Name,
			Address: fmt.Sprintf("10.0.%d.%d", network+1, 128+next[network]),
			Port:    8080,
		})
		next[network]++
	}
	for i := range monitors {
		p.HealthMonitors = append(p.HealthMonitors, plan.HealthMonitor{Name: fmt.Sprintf("hm-%04d", i+1), Pool: p.Pools[2*i].Name})
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("large plan is invalid: %v", err)
	}
	return p
}

// quotaClient returns a load-balancer service client whose calls hit f and
// whose auth result carries the project proj-1, so PrecheckQuota reaches the
// quota read.
func quotaClient(t *testing.T, f *fake) *gophercloud.ServiceClient {
	t.Helper()
	gc := f.lbClient()
	ar := tokens.CreateResult{}
	ar.Body = map[string]any{"token": map[string]any{"project": map[string]any{"id": "proj-1"}}}
	if err := gc.SetTokenAndAuthResult(ar); err != nil {
		t.Fatalf("SetTokenAndAuthResult: %v", err)
	}
	return gc
}

// TestPlanNeedsCountsEveryOctaviaObject confirms the needs of the large plan.
func TestPlanNeedsCountsEveryOctaviaObject(t *testing.T) {
	got := planNeeds(largePlan(t))
	want := needs{loadBalancers: 15, listeners: 30, pools: 30, members: 120, healthMonitors: 15}
	if got != want {
		t.Errorf("planNeeds = %+v, want %+v", got, want)
	}
}

// TestCheckQuota confirms a negative limit is unlimited, a need equal to its
// limit fits, and every exceeded quota is itemized in the order loadbalancer,
// listener, pool, member, healthmonitor.
func TestCheckQuota(t *testing.T) {
	need := needs{loadBalancers: 15, listeners: 30, pools: 30, members: 120, healthMonitors: 15}
	for _, tc := range []struct {
		name string
		q    quotas.Quota
		want string
	}{
		{"unlimited", quotas.Quota{Loadbalancer: -1, Listener: -1, Pool: -1, Member: -1, Healthmonitor: -1}, ""},
		{"exactly enough", quotas.Quota{Loadbalancer: 15, Listener: 30, Pool: 30, Member: 120, Healthmonitor: 15}, ""},
		{"load balancers and members", quotas.Quota{Loadbalancer: 10, Listener: -1, Pool: -1, Member: 100, Healthmonitor: -1}, overQuota},
		{"every quota", quotas.Quota{Loadbalancer: 1, Listener: 2, Pool: 3, Member: 4, Healthmonitor: 0},
			"plan exceeds project quota; raise these quotas before applying: loadbalancer need 15, quota 1; listener need 30, quota 2; " +
				"pool need 30, quota 3; member need 120, quota 4; healthmonitor need 15, quota 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkQuota(need, &tc.q)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("checkQuota = %v, want nil", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("checkQuota = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestPrecheckQuota confirms the pre-check reads <endpoint>/v2.0/quotas/
// <project> and nothing else, passes the large plan on unlimited quotas, fails
// it with the exact itemized message on limits of 10 load balancers and 100
// members (also under the deprecated load_balancer spelling), skips with a
// warning on a 403, and aborts on a 500 or an answer without a quota object.
func TestPrecheckQuota(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantErr  string // the whole error, or its prefix when it ends in ": "
		wantWarn bool
	}{
		{"unlimited passes the large plan", http.StatusOK,
			`{"quota":{"loadbalancer":-1,"listener":-1,"pool":-1,"member":-1,"healthmonitor":-1,"l7policy":-1,"l7rule":-1}}`, "", false},
		{"limits fail the large plan", http.StatusOK,
			`{"quota":{"loadbalancer":10,"listener":-1,"pool":-1,"member":100,"healthmonitor":-1,"l7policy":-1,"l7rule":-1}}`, overQuota, false},
		{"deprecated spellings", http.StatusOK,
			`{"quota":{"load_balancer":10,"listener":-1,"pool":-1,"member":100,"health_monitor":-1}}`, overQuota, false},
		{"403 skips", http.StatusForbidden,
			`{"faultcode":"Client","faultstring":"Policy does not allow this request to be performed."}`, "", true},
		{"500 aborts", http.StatusInternalServerError,
			`{"faultstring":"Internal Server Error"}`, "reading Octavia quotas for pre-check: ", false},
		{"no quota object aborts", http.StatusOK,
			`{}`, "reading Octavia quotas for pre-check: response carries no quota object", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			f := newFake(t, scripted(t, map[string][]reply{"GET " + quotaPath: {{tc.status, tc.body}}}))

			err := PrecheckQuota(context.Background(), quotaClient(t, f), largePlan(t))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("PrecheckQuota = %v, want nil", err)
			case strings.HasSuffix(tc.wantErr, ": ") && (err == nil || !strings.HasPrefix(err.Error(), tc.wantErr)):
				t.Errorf("PrecheckQuota = %v, want an error starting with %q", err, tc.wantErr)
			case tc.wantErr != "" && !strings.HasSuffix(tc.wantErr, ": ") && (err == nil || err.Error() != tc.wantErr):
				t.Errorf("PrecheckQuota = %v, want %q", err, tc.wantErr)
			}
			f.wantLines(t, "GET "+quotaPath)
			warned := strings.Contains(logs.String(), "quota pre-check skipped: quota read denied")
			if warned != tc.wantWarn {
				t.Errorf("skip warning logged = %v, want %v; log %q", warned, tc.wantWarn, logs.String())
			}
		})
	}
}

// domainScopedClient returns a load-balancer client whose v3 auth result is
// present but carries a domain instead of a project, as a domain-scoped token
// does.
func domainScopedClient(t *testing.T, f *fake) *gophercloud.ServiceClient {
	t.Helper()
	gc := f.lbClient()
	ar := tokens.CreateResult{}
	ar.Body = map[string]any{"token": map[string]any{"domain": map[string]any{"id": "default"}}}
	if err := gc.SetTokenAndAuthResult(ar); err != nil {
		t.Fatalf("SetTokenAndAuthResult: %v", err)
	}
	return gc
}

// TestPrecheckQuotaWithoutProjectID confirms a client whose auth result
// carries no project skips the pre-check with a warning and reads nothing,
// both when no auth result was recorded and when a domain-scoped token's
// result names no project.
func TestPrecheckQuotaWithoutProjectID(t *testing.T) {
	cases := []struct {
		name   string
		client func(*testing.T, *fake) *gophercloud.ServiceClient
	}{
		{"no auth result", func(_ *testing.T, f *fake) *gophercloud.ServiceClient { return f.lbClient() }},
		{"auth result without a project", domainScopedClient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			f := newFake(t, scripted(t, map[string][]reply{}))

			if err := PrecheckQuota(context.Background(), tc.client(t, f), largePlan(t)); err != nil {
				t.Errorf("PrecheckQuota = %v, want nil", err)
			}
			f.wantLines(t)
			if !strings.Contains(logs.String(), "quota pre-check skipped: project id unavailable from auth result") {
				t.Errorf("log %q does not warn that the pre-check was skipped", logs.String())
			}
		})
	}
}

// TestProjectIDFromAuth confirms the project id is read from a v3 token auth
// result, and that a client without one reports none rather than guessing.
func TestProjectIDFromAuth(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{}))
	if id, ok := projectIDFromAuth(f.lbClient()); id != "" || ok {
		t.Errorf("projectIDFromAuth without an auth result = (%q, %v), want (\"\", false)", id, ok)
	}
	if id, ok := projectIDFromAuth(domainScopedClient(t, f)); id != "" || ok {
		t.Errorf("projectIDFromAuth with a domain-scoped result = (%q, %v), want (\"\", false)", id, ok)
	}
	if id, ok := projectIDFromAuth(quotaClient(t, f)); id != "proj-1" || !ok {
		t.Errorf("projectIDFromAuth = (%q, %v), want (\"proj-1\", true)", id, ok)
	}
}
