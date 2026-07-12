package nova

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// testNetworkClient builds a Client whose Neutron (network) service calls hit
// ts. Only the network-backed kinds (networks, subnets, ports) are exercised
// here, so the compute and block-storage clients are left nil.
func testNetworkClient(ts *httptest.Server) *Client {
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	return New(nil, gc, nil, "run0", metrics.NewCollector())
}

// TestDeleteNetworkPorts confirms the sweep deletes only the plain (empty
// device_owner) ports on a network — the untagged orphans a cancelled run can
// leave — while sparing router-interface and DHCP service ports, and tolerates
// a port already gone (404).
func TestDeleteNetworkPorts(t *testing.T) {
	var deletedPorts []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/ports":
			if got := r.URL.Query().Get("network_id"); got != "net-1" {
				t.Errorf("network_id query = %q, want net-1", got)
			}
			_, _ = io.WriteString(w, `{"ports":[
				{"id":"orphan-1","device_owner":""},
				{"id":"rif-1","device_owner":"network:router_interface"},
				{"id":"dhcp-1","device_owner":"network:dhcp"},
				{"id":"orphan-2","device_owner":""}
			]}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/ports/"):
			id := strings.TrimPrefix(r.URL.Path, "/ports/")
			deletedPorts = append(deletedPorts, id)
			if id == "orphan-2" {
				w.WriteHeader(http.StatusNotFound) // already gone: must be tolerated
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	c := testNetworkClient(ts)
	deleted, err := c.DeleteNetworkPorts(context.Background(), "net-1")
	if err != nil {
		t.Fatalf("DeleteNetworkPorts: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (orphan-1 removed, orphan-2 already gone, others skipped)", deleted)
	}
	if strings.Join(deletedPorts, ",") != "orphan-1,orphan-2" {
		t.Errorf("deleted ports = %v, want only the empty-device-owner ports attempted", deletedPorts)
	}
}

// TestCreateTaggedRollsBackOnTagFailure verifies that when tagging a freshly
// created resource keeps failing, createTagged retries the tag in place — never
// re-running create — and then rolls the resource back with a Delete, so no
// created-but-untagged orphan that tag-based cleanup can never reclaim survives.
func TestCreateTaggedRollsBackOnTagFailure(t *testing.T) {
	var puts, deletes atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/tags"):
			puts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	c := testNetworkClient(ts)

	creates := 0
	_, err := c.createTagged(context.Background(), KindNetwork, "net-0001",
		func(ctx context.Context, name string) (string, error) {
			creates++
			return "net-id-1", nil
		})
	if err == nil {
		t.Fatal("expected an error when tagging keeps failing")
	}
	if creates != 1 {
		t.Errorf("create closure called %d times, want 1 (a tag failure must not re-create)", creates)
	}
	if got := int(puts.Load()); got != tagAttempts {
		t.Errorf("tag attempted %d times, want %d", got, tagAttempts)
	}
	if got := deletes.Load(); got != 1 {
		t.Errorf("rollback Delete called %d times, want 1", got)
	}
}

// TestWaitForReadyAbortsOnGone verifies a readiness poll aborts promptly with a
// gone (404) error when the resource is deleted out of band, rather than
// re-polling until the context deadline and returning a DeadlineExceeded the
// apply path would tolerate as if the resource had come ready.
func TestWaitForReadyAbortsOnGone(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/networks/") {
			w.WriteHeader(http.StatusNotFound) // deleted out of band: every Get is a 404
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := testNetworkClient(ts)
	// A generous deadline the fix must return well before: an unfixed pollStatus
	// re-polls the 404 until this elapses and returns context.DeadlineExceeded.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.WaitForReady(ctx, resource.Resource{Kind: KindNetwork, ID: "net-gone", Logical: "net-0001"})
	if err == nil {
		t.Fatal("WaitForReady on a gone resource = nil, want a gone error")
	}
	if !IsNotFound(err) {
		t.Errorf("WaitForReady error = %v, want a 404 gone error", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("WaitForReady spun to the deadline instead of aborting on the 404")
	}
}
