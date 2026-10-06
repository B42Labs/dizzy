package nova

import (
	"context"
	"errors"
	"io"
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

// TestColdMigrateServer verifies the cold-migrate action body, that a retried
// 409 naming a resize task state is success, and that every other failure
// surfaces wrapped with the logical name.
func TestColdMigrateServer(t *testing.T) {
	srv := resource.Resource{Kind: KindServer, ID: "srv-1", Logical: "srv-0001"}

	t.Run("accepted", func(t *testing.T) {
		var body string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/servers/srv-1/action" {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			data, _ := io.ReadAll(r.Body)
			body = string(data)
			w.WriteHeader(http.StatusAccepted)
		}))
		defer ts.Close()
		if err := testComputeClient(ts).ColdMigrateServer(context.Background(), srv); err != nil {
			t.Fatalf("ColdMigrateServer = %v, want nil", err)
		}
		if body != `{"migrate":null}` {
			t.Errorf("posted body = %s, want {\"migrate\":null}", body)
		}
	})

	t.Run("already migrating", func(t *testing.T) {
		ts := conflictActionServer(t, `{"conflictingRequest":{"message":"Cannot 'migrate' instance srv-1 while it is in task_state resize_migrating"}}`)
		defer ts.Close()
		if err := testComputeClient(ts).ColdMigrateServer(context.Background(), srv); err != nil {
			t.Errorf("ColdMigrateServer = %v, want nil (a committed-then-retried 409 is success)", err)
		}
	})

	t.Run("error state is not tolerated", func(t *testing.T) {
		ts := conflictActionServer(t, `{"conflictingRequest":{"message":"Cannot 'migrate' instance srv-1 while it is in vm_state error"}}`)
		defer ts.Close()
		err := testComputeClient(ts).ColdMigrateServer(context.Background(), srv)
		if want := `cold-migrating server "srv-0001":`; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("ColdMigrateServer = %v, want an error starting with %q", err, want)
		}
	})

	t.Run("forbidden", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer ts.Close()
		err := testComputeClient(ts).ColdMigrateServer(context.Background(), srv)
		if want := `cold-migrating server "srv-0001":`; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("ColdMigrateServer = %v, want an error starting with %q", err, want)
		}
		var code gophercloud.ErrUnexpectedResponseCode
		if !errors.As(err, &code) || code.Actual != http.StatusForbidden {
			t.Errorf("ColdMigrateServer = %v, want it to wrap the gophercloud 403", err)
		}
	})
}

// TestIsSameFlavor verifies Nova's 400 for a resize to the flavor the server
// already has surfaces from ResizeServer as an error IsSameFlavor recognizes,
// and that no other rejection does.
func TestIsSameFlavor(t *testing.T) {
	srv := resource.Resource{Kind: KindServer, ID: "srv-1", Logical: "srv-0001"}
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"resize to the current flavor", http.StatusBadRequest, `{"badRequest":{"code":400,"message":"When resizing, instances must change flavor!"}}`, true},
		{"another bad request", http.StatusBadRequest, `{"badRequest":{"code":400,"message":"Invalid flavorRef provided."}}`, false},
		{"a conflict", http.StatusConflict, `{"conflictingRequest":{"message":"Cannot 'resize' instance srv-1 while it is in vm_state error"}}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			err := testComputeClient(ts).ResizeServer(context.Background(), srv, "flavor-2")
			if err == nil {
				t.Fatal("ResizeServer = nil, want the rejection")
			}
			if got := IsSameFlavor(err); got != tc.want {
				t.Errorf("IsSameFlavor(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
	if IsSameFlavor(nil) {
		t.Error("IsSameFlavor(nil) = true, want false")
	}
}
