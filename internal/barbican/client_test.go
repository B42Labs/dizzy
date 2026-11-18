package barbican

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// respErr builds a gophercloud unexpected-response error for the given status
// and body, the shape the classifiers branch on.
func respErr(status int, body string) error {
	return gophercloud.ErrUnexpectedResponseCode{Actual: status, Body: []byte(body)}
}

// testKV builds a key manager service client whose calls hit ts. Its
// ResourceBase carries the v1/ suffix openstack.NewKeyManagerV1 appends, so
// request paths read /v1/secrets, /v1/orders, /v1/containers, and /v1/quotas.
func testKV(ts *httptest.Server) *gophercloud.ServiceClient {
	return &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
		ResourceBase:   ts.URL + "/v1/",
	}
}

// testClient builds a Client for run0 whose calls hit ts and record into the
// returned Collector, with telemetry disabled.
func testClient(ts *httptest.Server) (*Client, *metrics.Collector) {
	m := metrics.NewCollector()
	return New(testKV(ts), "run0", m), m
}

// telemetryTestClient builds a testClient that also mirrors each timed call and
// order wait into a ManualReader-backed telemetry, so a test can assert both the
// in-memory samples and the live series of one call.
func telemetryTestClient(t *testing.T, ts *httptest.Server) (*Client, *metrics.Collector, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	tel, err := telemetry.NewWithProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatalf("NewWithProvider: %v", err)
	}
	c, m := testClient(ts)
	c.SetTelemetry(tel)
	return c, m, reader
}

// metricPoints collects reader once and returns the string attributes of every
// data point of the named float64 histogram or int64 sum, and an empty slice
// when the instrument recorded nothing.
func metricPoints(t *testing.T, reader *sdkmetric.ManualReader, name string) []map[string]string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}
	attrs := func(set attribute.Set) map[string]string {
		out := make(map[string]string, set.Len())
		for _, kv := range set.ToSlice() {
			out[string(kv.Key)] = kv.Value.String()
		}
		return out
	}
	points := []map[string]string{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					points = append(points, attrs(dp.Attributes))
				}
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					points = append(points, attrs(dp.Attributes))
				}
			default:
				t.Fatalf("%s is neither a float64 histogram nor an int64 sum: %T", name, m.Data)
			}
		}
	}
	return points
}

// pointsEqual reports whether got holds exactly the attribute sets of want, in
// any order.
func pointsEqual(got []map[string]string, want ...map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	used := make([]bool, len(got))
	for _, w := range want {
		found := false
		for i, g := range got {
			if !used[i] && fmt.Sprint(g) == fmt.Sprint(w) {
				used[i], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// statsOf returns the aggregate's stats for one sample type, or zero stats when
// no sample of that type was recorded.
func statsOf(agg metrics.Aggregate, typ string) metrics.Stats {
	for _, s := range agg.ByType {
		if s.Type == typ {
			return s
		}
	}
	return metrics.Stats{}
}

// readinessOf returns the aggregate's time-to-ready stats for one type, or zero
// stats when none was recorded.
func readinessOf(agg metrics.Aggregate, typ string) metrics.ReadinessStats {
	for _, r := range agg.Readiness {
		if r.Type == typ {
			return r
		}
	}
	return metrics.ReadinessStats{}
}

// captureLog sends the process-wide default logger to the returned buffer until
// the test ends. A test that calls it must not run in parallel.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// writeJSON answers a request with status and a JSON body.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestErrKind(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"payload mismatch", fmt.Errorf("%w: secret %q differs", ErrPayloadMismatch, "sec-0001"), "mismatch"},
		{"acl mismatch", fmt.Errorf("%w: acl %q differs", ErrACLMismatch, "acl-0001"), "mismatch"},
		{"consumer mismatch", fmt.Errorf("%w: container c1 lacks consumer %q", ErrConsumerMismatch, "con"), "mismatch"},
		{"order failed", fmt.Errorf("%w: order o1: boom (status 500)", ErrOrderFailed), "order_error"},
		{"quota 403", respErr(403, "Creation not allowed because a quota has been reached"), "quota"},
		{"wrapped quota 403", wrapCreate(KindSecret, "sec-0001", respErr(403, "quota reached")), "quota"},
		{"policy 403 without quota", respErr(403, "Forbidden: policy does not allow this"), "http_403"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"canceled", context.Canceled, "canceled"},
		{"http 400", respErr(400, "bad request"), "http_400"},
		{"http 401", respErr(401, "unauthorized"), "http_401"},
		{"http 404", respErr(404, "not found"), "http_404"},
		{"http 409", respErr(409, "conflict"), "http_409"},
		{"http 413 oversized payload", respErr(413, "Provided information too large to process"), "http_413"},
		{"http 415", respErr(415, "unsupported media type"), "http_415"},
		{"http 429", respErr(429, "slow down"), "http_429"},
		{"http 500", respErr(500, "boom"), "http_500"},
		{"http 502", respErr(502, "bad gateway"), "http_502"},
		{"http 503", respErr(503, "unavailable"), "http_503"},
		{"http 504", respErr(504, "gateway timeout"), "http_504"},
		{"http other", respErr(418, "teapot"), "http_other"},
		{"other", errors.New("dial tcp: connection refused"), "other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := errKind(tc.err); got != tc.want {
				t.Errorf("errKind(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsQuota(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		// Barbican's QuotaReached answers 403 with this message.
		{"403 quota reached", respErr(403, "Creation not allowed because a quota has been reached"), true},
		{"403 quota in another case", respErr(403, "QUOTA REACHED"), true},
		{"wrapped 403 quota", fmt.Errorf("creating secret: %w", respErr(403, "quota")), true},
		{"403 policy denial", respErr(403, "Forbidden: policy does not allow this"), false},
		// A 413 is LimitExceeded for an oversized payload, never a quota.
		{"413 oversized payload", respErr(413, "Provided information too large to process"), false},
		{"413 with quota body", respErr(413, "quota"), false},
		{"400 with quota body", respErr(400, "over quota"), false},
		{"409 with quota body", respErr(409, "quota exceeded"), false},
		{"non-http error", errors.New("quota"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isQuota(tc.err); got != tc.want {
				t.Errorf("isQuota(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"quota 403", respErr(403, "Creation not allowed because a quota has been reached"), false},
		{"policy 403", respErr(403, "Forbidden: policy does not allow this"), false},
		{"payload mismatch", fmt.Errorf("%w: secret %q differs", ErrPayloadMismatch, "sec-0001"), false},
		{"acl mismatch", fmt.Errorf("%w: acl %q differs", ErrACLMismatch, "acl-0001"), false},
		{"consumer mismatch", fmt.Errorf("%w: container c1 lacks consumer %q", ErrConsumerMismatch, "con"), false},
		{"order failed", fmt.Errorf("%w: order o1: boom (status 500)", ErrOrderFailed), false},
		{"500", respErr(500, "boom"), true},
		{"502", respErr(502, "bad gateway"), true},
		{"503", respErr(503, "unavailable"), true},
		{"504", respErr(504, "gateway timeout"), true},
		{"429", respErr(429, "slow down"), true},
		{"409 conflict", respErr(409, "conflict"), true},
		{"404", respErr(404, "gone"), false},
		{"413", respErr(413, "too large"), false},
		{"other", errors.New("dial tcp: connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsMismatch confirms IsMismatch reports each of the three read-back
// mismatches, wrapped or not, and nothing else.
func TestIsMismatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"payload", fmt.Errorf("%w: x", ErrPayloadMismatch), true},
		{"acl", fmt.Errorf("%w: x", ErrACLMismatch), true},
		{"consumer", fmt.Errorf("%w: x", ErrConsumerMismatch), true},
		{"wrapped acl", fmt.Errorf("verifying: %w", fmt.Errorf("%w: x", ErrACLMismatch)), true},
		{"order failed", fmt.Errorf("%w: x", ErrOrderFailed), false},
		{"500", respErr(500, "boom"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsMismatch(tc.err); got != tc.want {
				t.Errorf("IsMismatch(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsNotFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"404", respErr(404, "gone"), true},
		{"wrapped 404", fmt.Errorf("deleting: %w", respErr(404, "gone")), true},
		{"resource not found", gophercloud.ErrResourceNotFound{Name: "x", ResourceType: "secret"}, true},
		{"409", respErr(409, "conflict"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNotFound(tc.err); got != tc.want {
				t.Errorf("IsNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestResourceNameAndRunPrefix(t *testing.T) {
	if got, want := resourceName("abc123", "sec-0001"), "dizzy-abc123-sec-0001"; got != want {
		t.Errorf("resourceName = %q, want %q", got, want)
	}
	if got, want := runPrefix("abc123"), "dizzy-abc123-"; got != want {
		t.Errorf("runPrefix = %q, want %q", got, want)
	}
	if !strings.HasPrefix(resourceName("abc123", "sec-0001"), runPrefix("abc123")) {
		t.Error("resourceName does not carry runPrefix")
	}
}

// TestUserIDFromAuth confirms the user id is read from a v3 token auth result,
// and that a client without one, a result naming no user, and a user with an
// empty id all report false, so the creator confirmation stays off.
func TestUserIDFromAuth(t *testing.T) {
	withAuth := func(t *testing.T, token map[string]any) *gophercloud.ServiceClient {
		t.Helper()
		kv := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}}
		ar := tokens.CreateResult{}
		ar.Body = map[string]any{"token": token}
		if err := kv.SetTokenAndAuthResult(ar); err != nil {
			t.Fatalf("SetTokenAndAuthResult: %v", err)
		}
		return kv
	}

	if id, ok := UserIDFromAuth(&gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}}); id != "" || ok {
		t.Errorf("UserIDFromAuth without an auth result = (%q, %v), want (\"\", false)", id, ok)
	}
	if id, ok := UserIDFromAuth(withAuth(t, map[string]any{"user": map[string]any{"id": "u1"}})); id != "u1" || !ok {
		t.Errorf("UserIDFromAuth = (%q, %v), want (\"u1\", true)", id, ok)
	}
	if id, ok := UserIDFromAuth(withAuth(t, map[string]any{})); id != "" || ok {
		t.Errorf("UserIDFromAuth with a result naming no user = (%q, %v), want (\"\", false)", id, ok)
	}
	if id, ok := UserIDFromAuth(withAuth(t, map[string]any{"user": map[string]any{"id": ""}})); id != "" || ok {
		t.Errorf("UserIDFromAuth with an empty user id = (%q, %v), want (\"\", false)", id, ok)
	}
}

// TestMetadataKeysAreURLSafeAndLowercase confirms both run identity keys pass
// Barbican's secret metadata validator, which refuses a key outside
// ^[A-Za-z0-9_-]*$, and survive its lowercasing unchanged, so discovery reads
// back the key the stamp wrote.
func TestMetadataKeysAreURLSafeAndLowercase(t *testing.T) {
	for _, key := range []string{metaRun, metaType} {
		if !urlSafeKey.MatchString(key) {
			t.Errorf("metadata key %q does not match %s; Barbican answers it with 400 Invalid Key", key, urlSafeKey)
		}
		if lower := strings.ToLower(key); lower != key {
			t.Errorf("metadata key %q is not lowercase; Barbican stores it as %q", key, lower)
		}
	}
}

func TestIDFromRef(t *testing.T) {
	for _, tc := range []struct {
		ref     string
		want    string
		wantErr bool
	}{
		{"https://kv/v1/secrets/abc", "abc", false},
		{"https://kv/v1/secrets/abc/", "abc", false},
		{"https://kv:9311/v1/orders/0b5c-41e2", "0b5c-41e2", false},
		{"", "", true},
		{"https://kv/", "", true},
		{"https://kv", "", true},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			got, err := idFromRef(tc.ref)
			if (err != nil) != tc.wantErr {
				t.Fatalf("idFromRef(%q) error = %v, want error %v", tc.ref, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("idFromRef(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

func TestSecretRefIsTheSecretHref(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	c, _ := testClient(ts)
	if got, want := c.secretRef("abc"), ts.URL+"/v1/secrets/abc"; got != want {
		t.Errorf("secretRef = %q, want %q", got, want)
	}
}

// TestTimedRecordsOneSampleAndOperation confirms timed records exactly one
// sample whose type is the kind and one live operation under the kind and op,
// with the error classified in both, and that a client without telemetry still
// records the sample.
func TestTimedRecordsOneSampleAndOperation(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	c, m, reader := telemetryTestClient(t, ts)

	err := c.timed(context.Background(), KindOrder, "get", func(context.Context) error { return respErr(500, "boom") })
	if !gophercloud.ResponseCodeIs(err, 500) {
		t.Fatalf("timed returned %v, want fn's 500 unchanged", err)
	}
	agg := m.Aggregate(time.Second)
	if agg.Overall.Attempted != 1 {
		t.Errorf("samples = %d, want 1", agg.Overall.Attempted)
	}
	if st := statsOf(agg, "order"); st.Attempted != 1 || st.Failed != 1 {
		t.Errorf("order samples = %+v, want 1 attempted and 1 failed", st)
	}
	if len(agg.Errors) != 1 || agg.Errors[0] != (metrics.ErrorCount{Kind: "http_500", Count: 1}) {
		t.Errorf("errors = %+v, want one http_500", agg.Errors)
	}
	if got := metricPoints(t, reader, "dizzy.operation.duration"); !pointsEqual(got,
		map[string]string{"kind": "order", "operation": "get", "outcome": "error"}) {
		t.Errorf("operation.duration points = %v, want one order/get error", got)
	}
	if got := metricPoints(t, reader, "dizzy.operation.errors"); !pointsEqual(got,
		map[string]string{"kind": "order", "operation": "get", "error.kind": "http_500"}) {
		t.Errorf("operation.errors points = %v, want one order/get http_500", got)
	}

	plain, pm := testClient(ts)
	if err := plain.timed(context.Background(), KindSecret, "create", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("timed without telemetry: %v", err)
	}
	if st := statsOf(pm.Aggregate(time.Second), "secret"); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("secret samples without telemetry = %+v, want 1 succeeded", st)
	}
}

// TestDeleteRoutesByKind confirms Delete sends DELETE to the collection of each
// kind, an ACL's to its secret and a consumer's to its container's consumers
// with its name and URL in the body, records the call under that kind, and
// refuses an unknown kind and a malformed consumer id without a request.
func TestDeleteRoutesByKind(t *testing.T) {
	var got []string
	var consumerBody map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/acl"):
			// Barbican answers an ACL delete with 200, the only code gophercloud
			// accepts for it.
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/consumers"):
			if err := json.NewDecoder(r.Body).Decode(&consumerBody); err != nil {
				t.Errorf("decoding consumer delete body: %v", err)
			}
			writeJSON(w, http.StatusOK, `{"container_ref":"https://kv/v1/containers/c1"}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer ts.Close()
	c, m := testClient(ts)

	for _, r := range []resource.Resource{
		{Kind: KindSecret, ID: "s1"},
		{Kind: KindOrder, ID: "o1"},
		{Kind: KindContainer, ID: "c1"},
		{Kind: KindACL, ID: "s1"},
		{Kind: KindConsumer, ID: "c1:dizzy-run0-con-0001"},
	} {
		if err := c.Delete(context.Background(), r); err != nil {
			t.Fatalf("Delete(%s): %v", r.Kind, err)
		}
	}
	want := []string{
		"DELETE /v1/secrets/s1", "DELETE /v1/orders/o1", "DELETE /v1/containers/c1",
		"DELETE /v1/secrets/s1/acl", "DELETE /v1/containers/c1/consumers",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
	wantBody := map[string]string{"name": "dizzy-run0-con-0001", "URL": "https://dizzy.invalid/consumers/dizzy-run0-con-0001"}
	if fmt.Sprint(consumerBody) != fmt.Sprint(wantBody) {
		t.Errorf("consumer delete body = %v, want %v", consumerBody, wantBody)
	}
	agg := m.Aggregate(time.Second)
	for _, kind := range []string{"secret", "order", "container", "acl", "consumer"} {
		if st := statsOf(agg, kind); st.Attempted != 1 || st.Succeeded != 1 {
			t.Errorf("%s samples = %+v, want 1 succeeded", kind, st)
		}
	}

	got = nil
	err := c.Delete(context.Background(), resource.Resource{Kind: "x", ID: "x1"})
	if err == nil || err.Error() != `unknown barbican resource kind "x"` {
		t.Errorf("Delete(kind x) = %v, want unknown barbican resource kind %q", err, "x")
	}
	err = c.Delete(context.Background(), resource.Resource{Kind: KindConsumer, ID: "no-colon"})
	if want := `malformed consumer id "no-colon"`; err == nil || err.Error() != want {
		t.Errorf("Delete(malformed consumer) = %v, want %q", err, want)
	}
	if len(got) != 0 {
		t.Errorf("refused deletes sent %v, want no request", got)
	}
}

// TestObserve confirms Observe reads each kind's status from its own
// collection, reports a 404 as gone rather than as an error, and surfaces any
// other failure. An ACL exists only while its read entry lists a user, and a
// consumer only while its container lists it.
func TestObserve(t *testing.T) {
	for _, tc := range []struct {
		kind resource.Kind
		path string
	}{
		{KindSecret, "/v1/secrets/id1"},
		{KindOrder, "/v1/orders/id1"},
		{KindContainer, "/v1/containers/id1"},
	} {
		t.Run(string(tc.kind)+" active", func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != tc.path {
					t.Errorf("request %s %s, want GET %s", r.Method, r.URL.Path, tc.path)
				}
				writeJSON(w, http.StatusOK, `{"status":"ACTIVE"}`)
			}))
			defer ts.Close()
			c, m := testClient(ts)

			status, exists, err := c.Observe(context.Background(), resource.Resource{Kind: tc.kind, ID: "id1"})
			if status != "ACTIVE" || !exists || err != nil {
				t.Errorf("Observe = (%q, %v, %v), want (\"ACTIVE\", true, nil)", status, exists, err)
			}
			if st := statsOf(m.Aggregate(time.Second), string(tc.kind)); st.Attempted != 1 {
				t.Errorf("%s samples = %+v, want 1", tc.kind, st)
			}
		})
	}

	t.Run("404 is gone", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, `{"code":404,"title":"Not Found"}`)
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		status, exists, err := c.Observe(context.Background(), resource.Resource{Kind: KindSecret, ID: "id1"})
		if status != "" || exists || err != nil {
			t.Errorf("Observe = (%q, %v, %v), want (\"\", false, nil)", status, exists, err)
		}
	})

	t.Run("500 is an error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{"code":500}`)
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		status, exists, err := c.Observe(context.Background(), resource.Resource{Kind: KindOrder, ID: "id1"})
		if status != "" || exists || !gophercloud.ResponseCodeIs(err, 500) {
			t.Errorf("Observe = (%q, %v, %v), want (\"\", false, a 500)", status, exists, err)
		}
	})

	for _, tc := range []struct {
		name       string
		r          resource.Resource
		path       string
		status     int
		body       string
		wantExists bool
		wantErr    bool
	}{
		{"acl lists a user", resource.Resource{Kind: KindACL, ID: "s1"}, "/v1/secrets/s1/acl",
			http.StatusOK, `{"read":{"users":["u1"],"project-access":true}}`, true, false},
		{"acl lists none", resource.Resource{Kind: KindACL, ID: "s1"}, "/v1/secrets/s1/acl",
			http.StatusOK, `{"read":{"project-access":true}}`, false, false},
		{"acl 404 is gone", resource.Resource{Kind: KindACL, ID: "s1"}, "/v1/secrets/s1/acl",
			http.StatusNotFound, `{"code":404}`, false, false},
		{"acl 500 is an error", resource.Resource{Kind: KindACL, ID: "s1"}, "/v1/secrets/s1/acl",
			http.StatusInternalServerError, `{"code":500}`, false, true},
		{"consumer listed", resource.Resource{Kind: KindConsumer, ID: "c1:dizzy-run0-con-0001"}, "/v1/containers/c1/consumers",
			http.StatusOK, consumerPage("", listedConsumer("con-0002"), listedConsumer("con-0001")), true, false},
		{"consumer not listed", resource.Resource{Kind: KindConsumer, ID: "c1:dizzy-run0-con-0001"}, "/v1/containers/c1/consumers",
			http.StatusOK, consumerPage("", listedConsumer("con-0002")), false, false},
		{"consumer 404 is gone", resource.Resource{Kind: KindConsumer, ID: "c1:dizzy-run0-con-0001"}, "/v1/containers/c1/consumers",
			http.StatusNotFound, `{"code":404}`, false, false},
		{"consumer 500 is an error", resource.Resource{Kind: KindConsumer, ID: "c1:dizzy-run0-con-0001"}, "/v1/containers/c1/consumers",
			http.StatusInternalServerError, `{"code":500}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != tc.path {
					t.Errorf("request %s %s, want GET %s", r.Method, r.URL.Path, tc.path)
				}
				writeJSON(w, tc.status, tc.body)
			}))
			defer ts.Close()
			c, m := testClient(ts)

			status, exists, err := c.Observe(context.Background(), tc.r)
			if tc.wantErr {
				if status != "" || exists || !gophercloud.ResponseCodeIs(err, tc.status) {
					t.Errorf("Observe = (%q, %v, %v), want (\"\", false, a %d)", status, exists, err, tc.status)
				}
				return
			}
			if status != "" || exists != tc.wantExists || err != nil {
				t.Errorf("Observe = (%q, %v, %v), want (\"\", %v, nil)", status, exists, err, tc.wantExists)
			}
			if st := statsOf(m.Aggregate(time.Second), string(tc.r.Kind)); st.Attempted != 1 {
				t.Errorf("%s samples = %+v, want 1", tc.r.Kind, st)
			}
		})
	}

	t.Run("malformed consumer id", func(t *testing.T) {
		var requests int
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		_, _, err := c.Observe(context.Background(), resource.Resource{Kind: KindConsumer, ID: "no-colon"})
		if want := `malformed consumer id "no-colon"`; err == nil || err.Error() != want {
			t.Errorf("Observe(malformed consumer) error = %v, want %q", err, want)
		}
		if requests != 0 {
			t.Errorf("requests = %d, want 0", requests)
		}
	})

	t.Run("unknown kind", func(t *testing.T) {
		var requests int
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		_, _, err := c.Observe(context.Background(), resource.Resource{Kind: "x", ID: "id1"})
		if err == nil || err.Error() != `unknown barbican resource kind "x"` {
			t.Errorf("Observe(kind x) error = %v, want unknown barbican resource kind %q", err, "x")
		}
		if requests != 0 {
			t.Errorf("requests = %d, want 0", requests)
		}
	})
}

// TestOrderAndContainerListingsHonorRequestTimeout confirms SetRequestTimeout
// bounds the order and container listings too, not only secret discovery: a
// wedged listing fails with context.DeadlineExceeded although the caller's
// context carries no deadline.
func TestOrderAndContainerListingsHonorRequestTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	for _, tc := range []struct {
		name    string
		path    string
		list    func(*Client) error
		wantErr string
	}{
		{"orders", "/v1/orders", func(c *Client) error {
			_, _, err := c.ListOrdersByRun(context.Background(), "run0")
			return err
		}, "listing orders by run"},
		{"containers", "/v1/containers", func(c *Client) error {
			_, err := c.ListContainersByRun(context.Background(), "run0")
			return err
		}, "listing containers by run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer ts.Close()
			defer close(release)
			c, _ := testClient(ts)
			c.SetRequestTimeout(timeout)

			err := tc.list(c)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("listing = %v, want an error wrapping context.DeadlineExceeded", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("listing error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}
