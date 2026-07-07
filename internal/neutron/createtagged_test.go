package neutron

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
)

// testServiceClient builds a Client whose gophercloud calls hit ts. The create
// step is supplied as a closure by the test, so only tag and Delete travel over
// HTTP.
func testServiceClient(ts *httptest.Server) *Client {
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	return New(gc, "run0", metrics.NewCollector())
}

// TestCreateTaggedRollsBackOnTagFailure verifies that a retryable tag failure is
// retried in place — never re-running create — and that the created resource is
// rolled back with a Delete, so no created-but-untagged orphan that tag-based
// cleanup cannot reclaim is left behind.
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

	c := testServiceClient(ts)

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

// TestCreateTaggedLogsOrphanNameOnCreateError verifies that when the create
// itself errors, the deterministic resource name is logged so an operator can
// locate a resource that may have been committed despite the error.
func TestCreateTaggedLogsOrphanNameOnCreateError(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(old)

	// The create closure errors before any HTTP call, so gc is never touched.
	c := New(nil, "run0", metrics.NewCollector())
	_, err := c.createTagged(context.Background(), KindNetwork, "net-0001",
		func(ctx context.Context, name string) (string, error) {
			return "", gophercloud.ErrUnexpectedResponseCode{Actual: 503}
		})
	if err == nil {
		t.Fatal("expected the create error to propagate")
	}
	if logged := buf.String(); !strings.Contains(logged, "dizzy-run0-net-0001") {
		t.Errorf("create-error warning did not include the deterministic name; log=%q", logged)
	}
}

// TestCreateTaggedAddressScopeTagIsBestEffortAndUnmetered verifies that when a
// Neutron release returns 404 for an address-scope tag (it does not expose the
// tag API there), the create still succeeds, the resource is returned so a
// record-based cleanup can reclaim it, and the tolerated tag failure is left out
// of the metrics — only the create is recorded, never counted as a failure.
func TestCreateTaggedAddressScopeTagIsBestEffortAndUnmetered(t *testing.T) {
	var puts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/tags") {
			puts.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	coll := metrics.NewCollector()
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	c := New(gc, "run0", coll)

	// The create closure returns an id without an HTTP call, so the only request
	// is the tag PUT that 404s.
	r, err := c.createTagged(context.Background(), KindAddressScope, "as-0001",
		func(ctx context.Context, name string) (string, error) {
			return "as-id-1", nil
		})
	if err != nil {
		t.Fatalf("a best-effort tag 404 must not fail the create: %v", err)
	}
	if r.ID != "as-id-1" {
		t.Errorf("resource id = %q, want as-id-1 (it must still be returned for record-based cleanup)", r.ID)
	}
	// A 404 is not retryable, so the best-effort tag is attempted exactly once.
	if got := puts.Load(); got != 1 {
		t.Errorf("tag PUT attempted %d times, want 1 (a 404 is not retried)", got)
	}

	agg := coll.Aggregate(time.Second)
	if agg.Overall.Failed != 0 {
		t.Errorf("best-effort tag 404 counted as a failure: overall failed=%d, want 0", agg.Overall.Failed)
	}
	if agg.Overall.Attempted != 1 {
		t.Errorf("metrics recorded %d ops, want 1 (only the create; the best-effort tag is unmetered)", agg.Overall.Attempted)
	}
}
