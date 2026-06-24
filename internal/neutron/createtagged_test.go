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

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/openstack-tester/internal/metrics"
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
	if logged := buf.String(); !strings.Contains(logged, "ostester-run0-net-0001") {
		t.Errorf("create-error warning did not include the deterministic name; log=%q", logged)
	}
}
