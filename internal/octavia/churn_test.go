package octavia

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// The children of the load balancer lb-0001 the churn tests delete and toggle.
var (
	churnListener = resource.Resource{Kind: KindListener, Logical: "lsn-0001", Name: "dizzy-run0-lsn-0001", ID: "lsn-id-1"}
	churnPool     = resource.Resource{Kind: KindPool, Logical: "pool-0001", Name: "dizzy-run0-pool-0001", ID: "pool-id-1"}
	churnMember   = resource.Resource{Kind: KindMember, Logical: "pool-0001-member-01", Name: "dizzy-run0-pool-0001-member-01", ID: "mem-id-1"}
	churnMonitor  = resource.Resource{Kind: KindHealthMonitor, Logical: "hm-0001", Name: "dizzy-run0-hm-0001", ID: "hm-id-1"}
)

// Paths of the listener lsn-id-1 and of the member mem-id-1 of the pool
// pool-id.
const (
	churnListenerPath = "/v2.0/lbaas/listeners/lsn-id-1"
	churnMemberPath   = "/v2.0/lbaas/pools/pool-id/members/mem-id-1"
)

// wantRawBody fails the test unless the raw JSON request body decodes to
// exactly want, compared as decoded maps so key order does not matter.
func wantRawBody(t *testing.T, body string, want map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding request body %q: %v", body, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("request body = %v, want %v", got, want)
	}
}

// wantOneSample fails the test unless the collector holds exactly one
// operation sample, of type kind and with the outcome ok.
func wantOneSample(t *testing.T, agg metrics.Aggregate, kind resource.Kind, ok bool) {
	t.Helper()
	s := statsOf(agg, string(kind))
	wantSucceeded, wantFailed := 1, 0
	if !ok {
		wantSucceeded, wantFailed = 0, 1
	}
	if s.Attempted != 1 || s.Succeeded != wantSucceeded || s.Failed != wantFailed {
		t.Errorf("%s stats = %+v, want one sample with success %v", kind, s, ok)
	}
	if agg.Overall.Attempted != 1 {
		t.Errorf("recorded %d operation samples, want one", agg.Overall.Attempted)
	}
}

// TestDeleteSingleLoadBalancerDoesNotCascade confirms a load balancer is
// deleted without any cascade query, so Octavia refuses it while a child is
// attached, and the call is recorded as one loadbalancer sample.
func TestDeleteSingleLoadBalancerDoesNotCascade(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{
		"DELETE " + loadBalancerPath: {{http.StatusNoContent, ``}},
	}))
	c, m := f.client()

	if err := c.DeleteSingle(context.Background(), createdLB); err != nil {
		t.Fatalf("DeleteSingle: %v", err)
	}
	f.wantLines(t, "DELETE "+loadBalancerPath)
	if q := f.requests()[0].query; q != "" {
		t.Errorf("query = %q, want none", q)
	}
	wantOneSample(t, m.Aggregate(time.Second), KindLoadBalancer, true)
}

// TestDeleteSingleChildKinds confirms a listener, pool, and health monitor are
// each deleted by id at their own path, recorded as one sample of their kind.
func TestDeleteSingleChildKinds(t *testing.T) {
	for _, tc := range []struct {
		r    resource.Resource
		path string
	}{
		{churnListener, "/v2.0/lbaas/listeners/lsn-id-1"},
		{churnPool, "/v2.0/lbaas/pools/pool-id-1"},
		{churnMonitor, "/v2.0/lbaas/healthmonitors/hm-id-1"},
	} {
		t.Run(string(tc.r.Kind), func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{"DELETE " + tc.path: {{http.StatusNoContent, ``}}}))
			c, m := f.client()

			if err := c.DeleteSingle(context.Background(), tc.r); err != nil {
				t.Fatalf("DeleteSingle(%s): %v", tc.r.Kind, err)
			}
			f.wantLines(t, "DELETE "+tc.path)
			wantOneSample(t, m.Aggregate(time.Second), tc.r.Kind, true)
		})
	}
}

// TestDeleteSingleRefusals confirms a member, which needs its pool, and a
// kind DeleteSingle does not delete are refused without a request or a sample.
func TestDeleteSingleRefusals(t *testing.T) {
	for _, tc := range []struct {
		r    resource.Resource
		want string
	}{
		{churnMember, `deleting member "pool-0001-member-01" needs its pool; use DeleteMember`},
		{resource.Resource{Kind: KindSubnet, Logical: "sub-0001", ID: "sub-id-1"}, `delete single not supported for kind "subnet"`},
		{resource.Resource{Kind: KindNetwork, Logical: "net-0001", ID: "net-id-1"}, `delete single not supported for kind "network"`},
	} {
		t.Run(string(tc.r.Kind), func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{}))
			c, m := f.client()

			if err := c.DeleteSingle(context.Background(), tc.r); err == nil || err.Error() != tc.want {
				t.Errorf("DeleteSingle(%s) = %v, want %q", tc.r.Kind, err, tc.want)
			}
			f.wantLines(t)
			if n := m.Aggregate(time.Second).Overall.Attempted; n != 0 {
				t.Errorf("DeleteSingle(%s) recorded %d samples, want none", tc.r.Kind, n)
			}
		})
	}
}

// TestDeleteSingleNotFound confirms a 404 surfaces unchanged, for the caller
// to treat as already gone, and is recorded as one failed sample.
func TestDeleteSingleNotFound(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{
		"DELETE " + churnListenerPath: {{http.StatusNotFound, `{"faultstring":"Listener lsn-id-1 not found."}`}},
	}))
	c, m := f.client()

	if err := c.DeleteSingle(context.Background(), churnListener); !IsNotFound(err) {
		t.Errorf("DeleteSingle(gone listener) = %v, want a 404", err)
	}
	f.wantLines(t, "DELETE "+churnListenerPath)
	wantOneSample(t, m.Aggregate(time.Second), KindListener, false)
}

// TestDeleteMember confirms a member is deleted below the pool it is given,
// recorded as one member sample, and that an empty pool id is refused without
// a request or a sample.
func TestDeleteMember(t *testing.T) {
	t.Run("pool id", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{"DELETE " + churnMemberPath: {{http.StatusNoContent, ``}}}))
		c, m := f.client()

		if err := c.DeleteMember(context.Background(), churnMember, "pool-id"); err != nil {
			t.Fatalf("DeleteMember: %v", err)
		}
		f.wantLines(t, "DELETE "+churnMemberPath)
		wantOneSample(t, m.Aggregate(time.Second), KindMember, true)
	})
	t.Run("no pool id", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{}))
		c, m := f.client()

		err := c.DeleteMember(context.Background(), churnMember, "")
		if want := `deleting member "pool-0001-member-01": no pool id`; err == nil || err.Error() != want {
			t.Errorf("DeleteMember(no pool) = %v, want %q", err, want)
		}
		f.wantLines(t)
		if n := m.Aggregate(time.Second).Overall.Attempted; n != 0 {
			t.Errorf("DeleteMember(no pool) recorded %d samples, want none", n)
		}
	})
}

// TestSetListenerAdminState confirms the toggle sends a PUT to the listener
// whose body holds admin_state_up and nothing else, recorded as one listener
// sample.
func TestSetListenerAdminState(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{
		"PUT " + churnListenerPath: {{http.StatusOK, `{"listener":` + item("lsn-id-1", "dizzy-run0-lsn-0001", "PENDING_UPDATE") + `}`}},
	}))
	c, m := f.client()

	if err := c.SetListenerAdminState(context.Background(), churnListener, false); err != nil {
		t.Fatalf("SetListenerAdminState: %v", err)
	}
	f.wantLines(t, "PUT "+churnListenerPath)
	wantRawBody(t, f.requests()[0].body, map[string]any{"listener": map[string]any{"admin_state_up": false}})
	wantOneSample(t, m.Aggregate(time.Second), KindListener, true)
}

// TestSetListenerAdminStateNotImplemented confirms a provider that refuses the
// option, which Octavia answers with 501, fails the toggle with an error
// classified http_other that is not retryable, recorded as one failed listener
// sample.
func TestSetListenerAdminStateNotImplemented(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{
		"PUT " + churnListenerPath: {{http.StatusNotImplemented, `{"faultcode":"Server","faultstring":"Provider does not support a requested option."}`}},
	}))
	c, m := f.client()

	err := c.SetListenerAdminState(context.Background(), churnListener, false)
	if err == nil {
		t.Fatal("SetListenerAdminState = nil, want the 501")
	}
	f.wantLines(t, "PUT "+churnListenerPath)
	if got := errKind(err); got != "http_other" {
		t.Errorf("errKind(%v) = %q, want http_other", err, got)
	}
	if IsRetryable(err) {
		t.Errorf("IsRetryable(%v) = true, want false", err)
	}
	agg := m.Aggregate(time.Second)
	wantOneSample(t, agg, KindListener, false)
	if len(agg.Errors) != 1 || agg.Errors[0].Kind != "http_other" || agg.Errors[0].Count != 1 {
		t.Errorf("errors = %+v, want one http_other", agg.Errors)
	}
}

// TestSetMemberAdminState confirms the toggle sends a PUT to the member below
// the pool it is given, whose body holds admin_state_up and nothing else,
// recorded as one member sample, and that an empty pool id is refused without
// a request or a sample.
func TestSetMemberAdminState(t *testing.T) {
	t.Run("pool id", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{
			"PUT " + churnMemberPath: {{http.StatusOK, `{"member":` + item("mem-id-1", "dizzy-run0-pool-0001-member-01", "PENDING_UPDATE") + `}`}},
		}))
		c, m := f.client()

		if err := c.SetMemberAdminState(context.Background(), churnMember, "pool-id", true); err != nil {
			t.Fatalf("SetMemberAdminState: %v", err)
		}
		f.wantLines(t, "PUT "+churnMemberPath)
		wantRawBody(t, f.requests()[0].body, map[string]any{"member": map[string]any{"admin_state_up": true}})
		wantOneSample(t, m.Aggregate(time.Second), KindMember, true)
	})
	t.Run("no pool id", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{}))
		c, m := f.client()

		err := c.SetMemberAdminState(context.Background(), churnMember, "", true)
		if want := `updating member "pool-0001-member-01": no pool id`; err == nil || err.Error() != want {
			t.Errorf("SetMemberAdminState(no pool) = %v, want %q", err, want)
		}
		f.wantLines(t)
		if n := m.Aggregate(time.Second).Overall.Attempted; n != 0 {
			t.Errorf("SetMemberAdminState(no pool) recorded %d samples, want none", n)
		}
	})
}
