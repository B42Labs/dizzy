package octavia

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Paths of the load balancer collection and of the load balancer lb-id-1.
const (
	loadBalancersPath = "/v2.0/lbaas/loadbalancers"
	loadBalancerPath  = "/v2.0/lbaas/loadbalancers/lb-id-1"
)

// planLB is the plan entry of the load balancer the tests create.
var planLB = plan.LoadBalancer{Name: "lb-0001", Network: "net-0001"}

// createdLB is the Resource of the load balancer lb-0001 with id lb-id-1.
var createdLB = resource.Resource{Kind: KindLoadBalancer, Logical: "lb-0001", Name: "dizzy-run0-lb-0001", ID: "lb-id-1"}

// lbAnswer renders a load balancer get or create answer with status and
// provider.
func lbAnswer(status, provider string) string {
	return `{"loadbalancer":{"id":"lb-id-1","name":"dizzy-run0-lb-0001","provisioning_status":"` + status +
		`","provider":"` + provider + `","tags":["dizzy:run=run0","dizzy:type=loadbalancer"]}}`
}

// TestCreateLoadBalancerBody confirms the create carries the deterministic
// name, the VIP subnet, the two run tags, and the provider only when one is
// set, and that the returned provider is the one Octavia answers with.
func TestCreateLoadBalancerBody(t *testing.T) {
	for _, tc := range []struct {
		name         string
		provider     string
		answered     string
		wantProvider any
	}{
		{"provider set", "ovn", "ovn", "ovn"},
		{"cloud default", "", "amphora", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{
				"POST " + loadBalancersPath: {{http.StatusCreated, lbAnswer("PENDING_CREATE", tc.answered)}},
			}))
			c, _ := f.client()

			got, provider, err := c.CreateLoadBalancer(context.Background(), planLB, "sub-id-1", tc.provider)
			if err != nil {
				t.Fatalf("CreateLoadBalancer: %v", err)
			}
			if got != createdLB {
				t.Errorf("CreateLoadBalancer = %+v, want %+v", got, createdLB)
			}
			if provider != tc.answered {
				t.Errorf("effective provider = %q, want the answered %q", provider, tc.answered)
			}
			f.wantLines(t, "POST "+loadBalancersPath)

			obj := decodeBody(t, f.requests()[0].body, "loadbalancer")
			if obj["name"] != "dizzy-run0-lb-0001" {
				t.Errorf("name = %v, want dizzy-run0-lb-0001", obj["name"])
			}
			if obj["vip_subnet_id"] != "sub-id-1" {
				t.Errorf("vip_subnet_id = %v, want sub-id-1", obj["vip_subnet_id"])
			}
			wantTags(t, obj, KindLoadBalancer)
			if got, has := obj["provider"]; got != tc.wantProvider || has != (tc.wantProvider != nil) {
				t.Errorf("provider = %v (present %v), want %v", got, has, tc.wantProvider)
			}
		})
	}
}

// TestCreateLoadBalancerAdoptionReturnsProvider confirms an adopted load
// balancer returns the provider of the load balancer the lookup found.
func TestCreateLoadBalancerAdoptionReturnsProvider(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{
		"POST " + loadBalancersPath: {{http.StatusGatewayTimeout, `{}`}},
		"GET " + loadBalancersPath: {{http.StatusOK, `{"loadbalancers":[{"id":"lb-id-1","name":"dizzy-run0-lb-0001",` +
			`"provisioning_status":"PENDING_CREATE","provider":"amphora","tags":["dizzy:run=run0","dizzy:type=loadbalancer"]}]}`}},
	}))
	c, _ := f.client()

	got, provider, err := c.CreateLoadBalancer(context.Background(), planLB, "sub-id-1", "")
	if err != nil {
		t.Fatalf("CreateLoadBalancer: %v", err)
	}
	if got != createdLB || provider != "amphora" {
		t.Errorf("CreateLoadBalancer = %+v, %q, want %+v, amphora", got, provider, createdLB)
	}
	f.wantLines(t, "POST "+loadBalancersPath, "GET "+loadBalancersPath+"?name=dizzy-run0-lb-0001&tags=dizzy%3Arun%3Drun0")
}

// TestCreateLoadBalancerQuota confirms Octavia's 403 "Quota has been met" is
// wrapped with ErrQuota, returns no provider, and looks nothing up.
func TestCreateLoadBalancerQuota(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{
		"POST " + loadBalancersPath: {{http.StatusForbidden, `{"faultcode":"Client","faultstring":"Quota has been met for resources: Load Balancer"}`}},
	}))
	c, _ := f.client()

	_, provider, err := c.CreateLoadBalancer(context.Background(), planLB, "sub-id-1", "amphora")
	if !errors.Is(err, ErrQuota) || provider != "" {
		t.Errorf("CreateLoadBalancer = %q, %v, want no provider and ErrQuota", provider, err)
	}
	f.wantLines(t, "POST "+loadBalancersPath)
}

// TestWaitForActiveRecordsReadinessOfKind confirms a load balancer polled from
// PENDING_UPDATE to ACTIVE records one readiness sample of the kind passed, the
// kind just written, and no operation sample.
func TestWaitForActiveRecordsReadinessOfKind(t *testing.T) {
	fastPolls(t)
	f := newFake(t, scripted(t, map[string][]reply{
		"GET " + loadBalancerPath: {
			{http.StatusOK, lbAnswer("PENDING_UPDATE", "amphora")},
			{http.StatusServiceUnavailable, `{}`},
			{http.StatusOK, lbAnswer("ACTIVE", "amphora")},
		},
	}))
	c, m := f.client()

	if err := c.WaitForActive(waitCtx(t), createdLB, KindListener); err != nil {
		t.Fatalf("WaitForActive: %v", err)
	}
	f.wantLines(t, "GET "+loadBalancerPath, "GET "+loadBalancerPath, "GET "+loadBalancerPath)
	agg := m.Aggregate(time.Second)
	if want := []string{"listener"}; !reflect.DeepEqual(readinessTypes(agg.Readiness), want) {
		t.Errorf("readiness types = %v, want %v", readinessTypes(agg.Readiness), want)
	}
	if r := readinessOf(agg, "listener"); r.Count != 1 || r.OK != 1 {
		t.Errorf("listener readiness = %+v, want one OK sample", r)
	}
	if agg.Overall.Attempted != 0 {
		t.Errorf("recorded %d operation samples, want none", agg.Overall.Attempted)
	}
}

// readinessTypes lists the types of the readiness stats.
func readinessTypes(rs []metrics.ReadinessStats) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Type)
	}
	return out
}

// TestWaitForActiveProvisioningError confirms a load balancer in ERROR fails
// the wait with ErrProvisioning, which is not retryable, records a failed
// readiness sample of the kind written, and records the poll that found it as
// one failed sample of that kind classified provisioning_error.
func TestWaitForActiveProvisioningError(t *testing.T) {
	fastPolls(t)
	f := newFake(t, scripted(t, map[string][]reply{
		"GET " + loadBalancerPath: {
			{http.StatusOK, lbAnswer("PENDING_UPDATE", "ovn")},
			{http.StatusOK, lbAnswer("ERROR", "ovn")},
		},
	}))
	c, m := f.client()

	err := c.WaitForActive(waitCtx(t), createdLB, KindPool)
	if !errors.Is(err, ErrProvisioning) {
		t.Fatalf("WaitForActive = %v, want ErrProvisioning", err)
	}
	if want := "octavia: provisioning failed: load balancer lb-id-1 reached ERROR after a pool write"; err.Error() != want {
		t.Errorf("WaitForActive error = %q, want %q", err, want)
	}
	if IsRetryable(err) {
		t.Error("a provisioning error is retryable")
	}
	agg := m.Aggregate(time.Second)
	if r := readinessOf(agg, "pool"); r.Count != 1 || r.OK != 0 {
		t.Errorf("pool readiness = %+v, want one failed sample", r)
	}
	if s := statsOf(agg, "pool"); s.Attempted != 1 || s.Failed != 1 {
		t.Errorf("pool stats = %+v, want one failed sample", s)
	}
	if agg.Overall.Attempted != 1 {
		t.Errorf("recorded %d operation samples, want only the failed pool get", agg.Overall.Attempted)
	}
	if len(agg.Errors) != 1 || agg.Errors[0].Kind != "provisioning_error" || agg.Errors[0].Count != 1 {
		t.Errorf("errors = %+v, want one provisioning_error", agg.Errors)
	}
}

// TestWaitForActiveMirrorsTelemetry confirms the wait mirrors its readiness
// sample live under the kind just written, records no operation for its polls
// on success, and mirrors the poll that found ERROR as one <kind>/get error
// classified provisioning_error.
func TestWaitForActiveMirrorsTelemetry(t *testing.T) {
	fastPolls(t)
	t.Run("active", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{"GET " + loadBalancerPath: {
			{http.StatusOK, lbAnswer("PENDING_UPDATE", "ovn")},
			{http.StatusOK, lbAnswer("ACTIVE", "ovn")},
		}}))
		c, _, reader := f.telemetryClient(t)

		if err := c.WaitForActive(waitCtx(t), createdLB, KindHealthMonitor); err != nil {
			t.Fatalf("WaitForActive: %v", err)
		}
		want := []map[string]string{{"kind": "healthmonitor", "outcome": "success"}}
		if got := metricPoints(t, reader, "dizzy.resource.time_to_ready"); !reflect.DeepEqual(got, want) {
			t.Errorf("time_to_ready points = %v, want %v", got, want)
		}
		if got := metricPoints(t, reader, "dizzy.operation.duration"); len(got) != 0 {
			t.Errorf("operation points = %v, want none for the polls", got)
		}
	})
	t.Run("error", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{"GET " + loadBalancerPath: {{http.StatusOK, lbAnswer("ERROR", "ovn")}}}))
		c, _, reader := f.telemetryClient(t)

		if err := c.WaitForActive(waitCtx(t), createdLB, KindMember); !errors.Is(err, ErrProvisioning) {
			t.Fatalf("WaitForActive = %v, want ErrProvisioning", err)
		}
		wantReady := []map[string]string{{"kind": "member", "outcome": "timeout"}}
		if got := metricPoints(t, reader, "dizzy.resource.time_to_ready"); !reflect.DeepEqual(got, wantReady) {
			t.Errorf("time_to_ready points = %v, want %v", got, wantReady)
		}
		wantErrors := []map[string]string{{"kind": "member", "operation": "get", "error.kind": "provisioning_error"}}
		if got := metricPoints(t, reader, "dizzy.operation.errors"); !reflect.DeepEqual(got, wantErrors) {
			t.Errorf("operation.errors points = %v, want %v", got, wantErrors)
		}
	})
}

// TestWaitForActiveGone confirms a load balancer deleted out of band aborts the
// wait at once with a gone error and a failed readiness sample.
func TestWaitForActiveGone(t *testing.T) {
	fastPolls(t)
	f := newFake(t, scripted(t, map[string][]reply{
		"GET " + loadBalancerPath: {{http.StatusNotFound, `{"faultstring":"Load Balancer lb-id-1 not found."}`}},
	}))
	c, m := f.client()

	err := c.WaitForActive(waitCtx(t), createdLB, KindMember)
	if err == nil || !strings.HasPrefix(err.Error(), "load balancer lb-id-1 is gone while waiting for ACTIVE: ") || !IsNotFound(err) {
		t.Errorf("WaitForActive = %v, want the gone 404 error", err)
	}
	f.wantLines(t, "GET "+loadBalancerPath)
	if r := readinessOf(m.Aggregate(time.Second), "member"); r.Count != 1 || r.OK != 0 {
		t.Errorf("member readiness = %+v, want one failed sample", r)
	}
}

// TestWaitForActiveDeadline confirms a load balancer that never returns to
// ACTIVE, or an already expired context, ends the wait with
// context.DeadlineExceeded and a failed readiness sample.
func TestWaitForActiveDeadline(t *testing.T) {
	fastPolls(t)
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{"deadline during the wait", 20 * time.Millisecond},
		{"expired context", -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{
				"GET " + loadBalancerPath: {{http.StatusOK, lbAnswer("PENDING_CREATE", "amphora")}},
			}))
			c, m := f.client()
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()

			if err := c.WaitForActive(ctx, createdLB, KindLoadBalancer); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("WaitForActive = %v, want context.DeadlineExceeded", err)
			}
			if r := readinessOf(m.Aggregate(time.Second), "loadbalancer"); r.Count != 1 || r.OK != 0 {
				t.Errorf("loadbalancer readiness = %+v, want one failed sample", r)
			}
		})
	}
}

// TestWaitForGone confirms the deletion wait ends on a 404 or a DELETED status,
// polls through PENDING_DELETE and a transient error, records nothing, returns
// the context's error when it never ends, and refuses a kind other than a load
// balancer without a request.
func TestWaitForGone(t *testing.T) {
	fastPolls(t)
	for _, tc := range []struct {
		name    string
		replies []reply
		polls   int
	}{
		{"404", []reply{{http.StatusNotFound, `{}`}}, 1},
		{"DELETED", []reply{{http.StatusOK, lbAnswer("DELETED", "amphora")}}, 1},
		{"pending delete then gone", []reply{
			{http.StatusOK, lbAnswer("PENDING_DELETE", "amphora")},
			{http.StatusInternalServerError, `{}`},
			{http.StatusNotFound, `{}`},
		}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{"GET " + loadBalancerPath: tc.replies}))
			c, m := f.client()

			if err := c.WaitForGone(waitCtx(t), createdLB); err != nil {
				t.Errorf("WaitForGone = %v, want nil", err)
			}
			if got := len(f.requests()); got != tc.polls {
				t.Errorf("polls = %d, want %d", got, tc.polls)
			}
			if agg := m.Aggregate(time.Second); agg.Overall.Attempted != 0 || len(agg.Readiness) != 0 {
				t.Errorf("WaitForGone recorded %+v, want nothing", agg)
			}
		})
	}
	t.Run("deadline", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{
			"GET " + loadBalancerPath: {{http.StatusOK, lbAnswer("PENDING_DELETE", "amphora")}},
		}))
		c, _ := f.client()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if err := c.WaitForGone(ctx, createdLB); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("WaitForGone = %v, want context.DeadlineExceeded", err)
		}
	})
	t.Run("not a load balancer", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{}))
		c, _ := f.client()
		err := c.WaitForGone(context.Background(), resource.Resource{Kind: KindListener, ID: "lsn-id-1"})
		if err == nil || err.Error() != `wait for gone not supported for kind "listener"` {
			t.Errorf("WaitForGone(listener) = %v, want the unsupported-kind error", err)
		}
		f.wantLines(t)
	})
}
