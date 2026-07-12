package nova

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// testComputeClient builds a Client whose Nova (compute) service calls hit ts.
// Only the compute-backed lifecycle ops are exercised here, so the network and
// block-storage clients are left nil.
func testComputeClient(ts *httptest.Server) *Client {
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	return New(gc, nil, nil, "run0", metrics.NewCollector())
}

// conflictActionServer answers every server-action POST with a 409 carrying
// body, the shape a retried lifecycle op hits once its first request already
// committed and moved the instance out of its prior state.
func conflictActionServer(t *testing.T, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/action") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(body))
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
}

// TestLifecycleOpsTolerateCommittedConflict verifies the async lifecycle ops
// treat a retried 409 that names their settled target state as success — a
// committed-then-lost first attempt must not fail the run once the retry sees
// the instance already transitioned — while a genuine non-target 409 (an ERROR
// state) still surfaces as an error.
func TestLifecycleOpsTolerateCommittedConflict(t *testing.T) {
	srv := resource.Resource{Kind: KindServer, ID: "srv-1", Logical: "srv-0001"}

	tolerated := []struct {
		name string
		body string
		call func(*Client) error
	}{
		{
			name: "resize already in verify-resize",
			body: `{"conflictingRequest":{"message":"Cannot 'resize' instance srv-1 while it is in vm_state resized"}}`,
			call: func(c *Client) error { return c.ResizeServer(context.Background(), srv, "flavor-2") },
		},
		{
			name: "confirm-resize already active",
			body: `{"conflictingRequest":{"message":"Cannot 'confirmResize' instance srv-1 while it is in vm_state active"}}`,
			call: func(c *Client) error { return c.ConfirmResizeServer(context.Background(), srv) },
		},
		{
			name: "live-migrate already migrating",
			body: `{"conflictingRequest":{"message":"Cannot 'os-migrateLive' instance srv-1 while it is in task_state migrating"}}`,
			call: func(c *Client) error { return c.LiveMigrateServer(context.Background(), srv) },
		},
	}
	for _, tc := range tolerated {
		t.Run(tc.name, func(t *testing.T) {
			ts := conflictActionServer(t, tc.body)
			defer ts.Close()
			if err := tc.call(testComputeClient(ts)); err != nil {
				t.Errorf("op = %v, want nil (a committed-then-retried 409 is success)", err)
			}
		})
	}

	// A resize that 409s because the instance is in ERROR is a real failure and
	// must not be swallowed: the body carries the 'resize' action verb but not
	// the target vm_state word "resized", so the tolerance must not match.
	t.Run("resize error state is not tolerated", func(t *testing.T) {
		ts := conflictActionServer(t, `{"conflictingRequest":{"message":"Cannot 'resize' instance srv-1 while it is in vm_state error"}}`)
		defer ts.Close()
		if err := testComputeClient(ts).ResizeServer(context.Background(), srv, "flavor-2"); err == nil {
			t.Error("ResizeServer on an ERROR-state 409 = nil, want an error")
		}
	})
}
