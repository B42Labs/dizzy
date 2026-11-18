package barbican

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// testACL is the plan entry the ACL tests set and read back: two users, listed
// out of sorted order so a comparison that depends on order would show.
func testACL() barbicanplan.ACL {
	return barbicanplan.ACL{Name: "acl-0001", Secret: "sec-0001", Users: []string{"u-b", "u-a"}}
}

// storedSecret is the stored secret the ACL tests target.
var storedSecret = resource.Resource{Kind: KindSecret, Logical: "sec-0001", Name: "dizzy-run0-sec-0001", ID: "s1"}

// aclResource is the resource SetACL returns for testACL on storedSecret.
var aclResource = resource.Resource{Kind: KindACL, Logical: "acl-0001", ID: "s1"}

// TestSetACL confirms SetACL sends one PUT to the secret's ACL whose body sets
// the read ACL to the plan's users and carries no project-access key, records
// one succeeded acl/create, and returns an acl resource with the secret's id
// and no name.
func TestSetACL(t *testing.T) {
	var requests []string
	var body []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		var err error
		if body, err = io.ReadAll(r.Body); err != nil {
			t.Errorf("reading request body: %v", err)
		}
		writeJSON(w, http.StatusOK, `{"acl_ref":"https://kv/v1/secrets/s1/acl"}`)
	}))
	defer ts.Close()
	c, m, reader := telemetryTestClient(t, ts)

	got, err := c.SetACL(context.Background(), testACL(), storedSecret)
	if err != nil {
		t.Fatalf("SetACL: %v", err)
	}
	if got != aclResource {
		t.Errorf("SetACL = %+v, want %+v", got, aclResource)
	}
	if want := []string{"PUT /v1/secrets/s1/acl"}; fmt.Sprint(requests) != fmt.Sprint(want) {
		t.Errorf("requests = %v, want %v", requests, want)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		t.Fatalf("request body %q is not JSON: %v", body, err)
	}
	if got, want := compact.String(), `{"read":{"users":["u-b","u-a"]}}`; got != want {
		t.Errorf("request body = %s, want %s", got, want)
	}
	if st := statsOf(m.Aggregate(time.Second), "acl"); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("acl samples = %+v, want 1 succeeded", st)
	}
	if got := metricPoints(t, reader, "dizzy.operation.duration"); !pointsEqual(got,
		map[string]string{"kind": "acl", "operation": "create", "outcome": "success"}) {
		t.Errorf("operation.duration points = %v, want one acl/create success", got)
	}
}

// TestSetACLRefusesUnstoredSecret confirms a secret without an id is refused
// before any request or sample.
func TestSetACLRefusesUnstoredSecret(t *testing.T) {
	var requests int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer ts.Close()
	c, m := testClient(ts)

	_, err := c.SetACL(context.Background(), testACL(), resource.Resource{Kind: KindSecret, Logical: "sec-0001"})
	if want := `acl "acl-0001" targets secret "sec-0001" that was not stored`; err == nil || err.Error() != want {
		t.Errorf("SetACL error = %v, want %q", err, want)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0", requests)
	}
	if n := m.Aggregate(time.Second).Overall.Attempted; n != 0 {
		t.Errorf("samples = %d, want 0", n)
	}
}

// TestSetACLPolicyDenial confirms a 403 without a quota text returns a create
// error that is not a quota rejection and records one failed http_403 sample.
func TestSetACLPolicyDenial(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, `{"code":403,"title":"Forbidden","description":"policy does not allow this"}`)
	}))
	defer ts.Close()
	c, m := testClient(ts)

	_, err := c.SetACL(context.Background(), testACL(), storedSecret)
	if err == nil || !strings.HasPrefix(err.Error(), `creating acl "acl-0001"`) {
		t.Fatalf("SetACL error = %v, want one starting %q", err, `creating acl "acl-0001"`)
	}
	if errors.Is(err, ErrQuota) {
		t.Errorf("SetACL error %v satisfies ErrQuota; a 403 without a quota text is a policy denial", err)
	}
	agg := m.Aggregate(time.Second)
	if st := statsOf(agg, "acl"); st.Attempted != 1 || st.Failed != 1 {
		t.Errorf("acl samples = %+v, want 1 failed", st)
	}
	if len(agg.Errors) != 1 || agg.Errors[0] != (metrics.ErrorCount{Kind: "http_403", Count: 1}) {
		t.Errorf("errors = %+v, want one http_403", agg.Errors)
	}
}

// aclServer answers every request with status and body, recording each as
// method and path.
func aclServer(t *testing.T, status int, body string, requests *[]string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests = append(*requests, r.Method+" "+r.URL.Path)
		writeJSON(w, status, body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestVerifyACL confirms a read ACL listing the plan's users in another order
// reads back as equal, with one GET of the secret's ACL recorded as one
// succeeded acl/get.
func TestVerifyACL(t *testing.T) {
	var requests []string
	ts := aclServer(t, http.StatusOK,
		`{"read":{"users":["u-a","u-b"],"project-access":true,"created":"2026-11-18T10:00:00","updated":"2026-11-18T10:00:00"}}`, &requests)
	c, m, reader := telemetryTestClient(t, ts)

	if err := c.VerifyACL(context.Background(), testACL(), aclResource); err != nil {
		t.Fatalf("VerifyACL = %v, want nil", err)
	}
	if want := []string{"GET /v1/secrets/s1/acl"}; fmt.Sprint(requests) != fmt.Sprint(want) {
		t.Errorf("requests = %v, want %v", requests, want)
	}
	if st := statsOf(m.Aggregate(time.Second), "acl"); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("acl samples = %+v, want 1 succeeded", st)
	}
	if got := metricPoints(t, reader, "dizzy.operation.duration"); !pointsEqual(got,
		map[string]string{"kind": "acl", "operation": "get", "outcome": "success"}) {
		t.Errorf("operation.duration points = %v, want one acl/get success", got)
	}
}

// TestVerifyACLMismatch confirms a read ACL listing one user more, one user
// fewer, or no users at all (a secret's default ACL) is a mismatch that is
// never retryable and records one failed sample with error kind mismatch.
func TestVerifyACLMismatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{"one user more", `{"read":{"users":["u-a","u-b","u-c"],"project-access":true}}`,
			`barbican: acl mismatch: acl "acl-0001" reads back users [u-a u-b u-c], want [u-a u-b]`},
		{"one user fewer", `{"read":{"users":["u-b"],"project-access":true}}`,
			`barbican: acl mismatch: acl "acl-0001" reads back users [u-b], want [u-a u-b]`},
		{"project access only", `{"read":{"project-access":true}}`,
			`barbican: acl mismatch: acl "acl-0001" reads back users [], want [u-a u-b]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			c, m := testClient(aclServer(t, http.StatusOK, tc.body, &requests))

			err := c.VerifyACL(context.Background(), testACL(), aclResource)
			if !errors.Is(err, ErrACLMismatch) {
				t.Fatalf("VerifyACL = %v, want an error wrapping ErrACLMismatch", err)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("VerifyACL error = %q, want %q", err, tc.wantErr)
			}
			if IsRetryable(err) {
				t.Errorf("IsRetryable(%v) = true, want false", err)
			}
			agg := m.Aggregate(time.Second)
			if st := statsOf(agg, "acl"); st.Attempted != 1 || st.Failed != 1 {
				t.Errorf("acl samples = %+v, want 1 failed", st)
			}
			if len(agg.Errors) != 1 || agg.Errors[0] != (metrics.ErrorCount{Kind: ErrKindMismatch, Count: 1}) {
				t.Errorf("errors = %+v, want one mismatch", agg.Errors)
			}
		})
	}
}

// TestVerifyACLReadErrors confirms a failed ACL read returns an error naming
// the ACL that keeps the classification of the underlying error: a 404 is not
// found, and a 503 is retryable.
func TestVerifyACLReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		check  func(error) bool
	}{
		{"404 is not found", http.StatusNotFound, IsNotFound},
		{"503 is retryable", http.StatusServiceUnavailable, IsRetryable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			c, _ := testClient(aclServer(t, tc.status, `{"code":`+fmt.Sprint(tc.status)+`}`, &requests))

			err := c.VerifyACL(context.Background(), testACL(), aclResource)
			if err == nil || !strings.HasPrefix(err.Error(), `reading acl "acl-0001"`) {
				t.Fatalf("VerifyACL error = %v, want one starting %q", err, `reading acl "acl-0001"`)
			}
			if !tc.check(err) {
				t.Errorf("VerifyACL error %v does not classify as %s", err, tc.name)
			}
			if errors.Is(err, ErrACLMismatch) {
				t.Errorf("VerifyACL error %v is a mismatch; a failed read is not", err)
			}
		})
	}
}
