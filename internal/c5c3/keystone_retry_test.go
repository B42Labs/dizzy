package c5c3_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"

	"github.com/B42Labs/dizzy/internal/c5c3"
	"github.com/B42Labs/dizzy/internal/c5c3/executor"
	"github.com/B42Labs/dizzy/internal/metrics"
)

// TestAuthenticate503RetriedByWithRetry confirms that a 503 to a login is the
// transient failure executor.WithRetry retries, not a mismatch that ends the
// stage: against a Keystone that answers the first login with 503 and the
// second with a token, the retried Authenticate passes after two logins, one
// recorded as http_503 and one as a success. It lives in an external test
// package because package c5c3's own tests cannot import the executor, which
// imports c5c3.
func TestAuthenticate503RetriedByWithRetry(t *testing.T) {
	var logins atomic.Int32
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if logins.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error": {"code": 503, "message": "unavailable", "title": "Service Unavailable"}}`))
			return
		}
		w.Header().Set("X-Subject-Token", "token")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token": {"methods": ["password"], "catalog": []}}`))
	}))
	t.Cleanup(ks.Close)

	m := metrics.NewCollector()
	c := c5c3.New(fake.NewSimpleDynamicClient(runtime.NewScheme()), "dizzy-orders", "run0", m)
	cred := c5c3.Credential{
		CloudsYAML: []byte(`clouds:
  admin:
    auth:
      auth_url: "` + ks.URL + `/v3"
      username: "dizzyorder-run0-user"
      password: "s3cr3t-password"
      user_domain_name: "Default"
`),
		Cloud:    "admin",
		Password: "s3cr3t-password",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := executor.WithRetry(ctx, 5*time.Second, func(ctx context.Context) error {
		return c.Authenticate(ctx, c5c3.KindUser, cred, "")
	})
	if err != nil {
		t.Fatalf("WithRetry(Authenticate) = %v, want the second login to pass", err)
	}
	if got := logins.Load(); got != 2 {
		t.Errorf("logins = %d, want 2: one 503 and its retry", got)
	}
	agg := m.Aggregate(time.Second)
	if len(agg.Errors) != 1 || agg.Errors[0] != (metrics.ErrorCount{Kind: "http_503", Count: 1}) {
		t.Errorf("errors = %+v, want one http_503", agg.Errors)
	}
	if agg.Overall.Attempted != 2 || agg.Overall.Succeeded != 1 {
		t.Errorf("samples = %+v, want 2 authenticates with 1 succeeded", agg.Overall)
	}
}
