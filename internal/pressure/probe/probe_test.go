package probe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/B42Labs/dizzy/internal/keystone"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// adminToken is the token the identity client of testProbe carries, and
// poolToken the token a test hands the adapter, so a test can tell the two
// apart in the request headers.
const (
	adminToken = "admin-tok"
	poolToken  = "pool-tok-4f9c"
)

// testProbe builds a Client over an identity client that carries adminToken
// and a user agent of its own, recording into a fresh collector and a
// ManualReader-backed telemetry, as internal/telemetry's tests build one.
func testProbe(t *testing.T) (*Client, *gophercloud.ServiceClient, *metrics.Collector, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	tel, err := telemetry.NewWithProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatalf("NewWithProvider: %v", err)
	}
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       "http://identity.invalid/v3/",
	}
	gc.UserAgent.Prepend("dizzy-test/1.0")
	gc.SetToken(adminToken)
	m := metrics.NewCollector()
	c := New(gc, m)
	c.SetTelemetry(tel)
	return c, gc, m, reader
}

// request is what a stand-in service saw of one request.
type request struct {
	method, path, query, token, userAgent string
}

// fakeService stands in for one service's endpoint: it answers with its status
// and body and records each request it received.
type fakeService struct {
	srv *httptest.Server

	mu   sync.Mutex
	seen []request
}

// newFakeService starts a fakeService answering status and body, where every
// "$URL" in body becomes the server's own URL, so a next link points back at
// it. Only the first request gets body; every later one gets an empty object,
// so a listing that wrongly follows a next link ends on the second page and
// shows as a second request instead of looping.
func newFakeService(t *testing.T, status int, body string) *fakeService {
	t.Helper()
	fs := &fakeService{}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.seen = append(fs.seen, request{
			method:    r.Method,
			path:      r.URL.Path,
			query:     r.URL.RawQuery,
			token:     r.Header.Get("X-Auth-Token"),
			userAgent: r.Header.Get("User-Agent"),
		})
		first := len(fs.seen) == 1
		fs.mu.Unlock()
		answer := `{}`
		if first {
			answer = strings.ReplaceAll(body, "$URL", "http://"+r.Host)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

// requests returns the requests the service received, in order.
func (fs *fakeService) requests() []request {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]request(nil), fs.seen...)
}

// unauthorized is the body a service answers a refused token with. It echoes
// the token, as a gateway quoting the request header may.
const unauthorized = `{"error":{"code":401,"title":"Unauthorized","message":"The request you have made requires authentication: ` + poolToken + `"}}`

// hasPoint reports whether the metric name has a data point whose attributes
// are exactly want. It reads the float64 histograms and the int64 counters
// the telemetry package records.
func hasPoint(t *testing.T, reader *sdkmetric.ManualReader, name string, want map[string]string) bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			var sets []attribute.Set
			switch data := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					sets = append(sets, dp.Attributes)
				}
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					sets = append(sets, dp.Attributes)
				}
			default:
				t.Fatalf("%s has unexpected data %T", name, m.Data)
			}
			for _, set := range sets {
				if attrsEqual(set, want) {
					return true
				}
			}
		}
	}
	return false
}

// attrsEqual reports whether set has exactly the wanted string attributes.
func attrsEqual(set attribute.Set, want map[string]string) bool {
	if set.Len() != len(want) {
		return false
	}
	for k, v := range want {
		got, ok := set.Value(attribute.Key(k))
		if !ok || got.AsString() != v {
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

// errorKinds lists the error kinds of the aggregate's failed samples, sorted.
func errorKinds(agg metrics.Aggregate) []string {
	var kinds []string
	for _, e := range agg.Errors {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// TestProbeRequests confirms each service gets exactly its one cheap read at
// the endpoint, with the given token and the identity client's user agent: no
// listing fetches a second page although the first carries a next link. A 200
// with an empty object or a one-item list returns nil and records a
// successful probe sample under the service as the operation.
func TestProbeRequests(t *testing.T) {
	cases := []struct {
		name      string
		service   string
		prefix    string // the endpoint's path on the stand-in server
		body      string
		wantPath  string
		wantQuery string
	}{
		{
			name:     "nova",
			service:  Nova,
			prefix:   "/",
			body:     `{}`,
			wantPath: "/limits",
		},
		{
			name:      "neutron",
			service:   Neutron,
			prefix:    "/",
			body:      `{"networks":[{"id":"net-1"}],"networks_links":[{"rel":"next","href":"$URL/v2.0/networks?limit=1&marker=net-1"}]}`,
			wantPath:  "/v2.0/networks",
			wantQuery: "limit=1",
		},
		{
			name:     "cinder",
			service:  Cinder,
			prefix:   "/v3/pid1/",
			body:     `{}`,
			wantPath: "/v3/pid1/limits",
		},
		{
			name:     "cinder endpoint without trailing slash",
			service:  Cinder,
			prefix:   "/v3/pid1",
			body:     `{}`,
			wantPath: "/v3/pid1/limits",
		},
		{
			name:      "glance",
			service:   Glance,
			prefix:    "/",
			body:      `{"images":[{"id":"img-1"}],"next":"/v2/images?limit=1&marker=img-1"}`,
			wantPath:  "/v2/images",
			wantQuery: "limit=1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeService(t, http.StatusOK, tc.body)
			c, gc, m, reader := testProbe(t)

			if err := c.Probe(context.Background(), tc.service, fs.srv.URL+tc.prefix, poolToken); err != nil {
				t.Fatalf("Probe(%s): %v", tc.service, err)
			}

			got := fs.requests()
			want := request{
				method:    http.MethodGet,
				path:      tc.wantPath,
				query:     tc.wantQuery,
				token:     poolToken,
				userAgent: gc.UserAgent.Join(),
			}
			if len(got) != 1 || got[0] != want {
				t.Errorf("requests = %+v, want exactly %+v", got, want)
			}
			if st := statsOf(m.Aggregate(time.Second), KindProbe); st.Attempted != 1 || st.Succeeded != 1 {
				t.Errorf("probe samples = %+v, want 1 attempted and 1 succeeded", st)
			}
			point := map[string]string{"kind": KindProbe, "operation": tc.service, "outcome": "success"}
			if !hasPoint(t, reader, "dizzy.operation.duration", point) {
				t.Errorf("no dizzy.operation.duration point with %v", point)
			}
		})
	}
}

// TestProbeNeverReauthenticates confirms a 401 at a service is the probe's
// own outcome: one request, no re-authentication of the identity client whose
// HTTP client the probe shares, and an http_401 sample.
func TestProbeNeverReauthenticates(t *testing.T) {
	fs := newFakeService(t, http.StatusUnauthorized, unauthorized)
	c, gc, m, _ := testProbe(t)
	var reauths int
	gc.ReauthFunc = func(context.Context) error {
		reauths++
		return nil
	}

	err := c.Probe(context.Background(), Nova, fs.srv.URL+"/", poolToken)
	if !gophercloud.ResponseCodeIs(err, http.StatusUnauthorized) {
		t.Errorf("Probe error = %v, want a 401 response error", err)
	}
	if n := len(fs.requests()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
	if reauths != 0 {
		t.Errorf("identity re-authentications = %d, want 0", reauths)
	}
	if tok := gc.Token(); tok != adminToken {
		t.Errorf("identity token after the probe = %q, want %q unchanged", tok, adminToken)
	}
	agg := m.Aggregate(time.Second)
	if st := statsOf(agg, KindProbe); st.Attempted != 1 || st.Failed != 1 {
		t.Errorf("probe samples = %+v, want 1 attempted and 1 failed", st)
	}
	if got := errorKinds(agg); len(got) != 1 || got[0] != "http_401" {
		t.Errorf("error kinds = %v, want [http_401]", got)
	}
}

// TestProbeRecordsSample confirms a failing probe records a failed sample
// labelled with the status, an error point in telemetry, and an error the
// shared classifier reads, a 503 being transient.
func TestProbeRecordsSample(t *testing.T) {
	fs := newFakeService(t, http.StatusServiceUnavailable, `{"message":"try later"}`)
	c, _, m, reader := testProbe(t)

	err := c.Probe(context.Background(), Glance, fs.srv.URL+"/", poolToken)
	if !gophercloud.ResponseCodeIs(err, http.StatusServiceUnavailable) {
		t.Fatalf("Probe error = %v, want a 503 response error", err)
	}
	if got := keystone.ClassifyOperationError(err); got != "transient" {
		t.Errorf("ClassifyOperationError(%v) = %q, want transient", err, got)
	}
	agg := m.Aggregate(time.Second)
	if st := statsOf(agg, KindProbe); st.Attempted != 1 || st.Failed != 1 {
		t.Errorf("probe samples = %+v, want 1 attempted and 1 failed", st)
	}
	if got := errorKinds(agg); len(got) != 1 || got[0] != "http_503" {
		t.Errorf("error kinds = %v, want [http_503]", got)
	}
	duration := map[string]string{"kind": KindProbe, "operation": Glance, "outcome": "error"}
	if !hasPoint(t, reader, "dizzy.operation.duration", duration) {
		t.Errorf("no dizzy.operation.duration point with %v", duration)
	}
	errs := map[string]string{"kind": KindProbe, "operation": Glance, "error.kind": "http_503"}
	if !hasPoint(t, reader, "dizzy.operation.errors", errs) {
		t.Errorf("no dizzy.operation.errors point with %v", errs)
	}
}

// TestProbeRedactsToken confirms the token never reaches an error text,
// although the service's body echoes it, for each of the three calls, and
// that the status stays reachable for the classifiers.
func TestProbeRedactsToken(t *testing.T) {
	calls := []struct {
		name string
		call func(c *Client, endpoint string) error
	}{
		{"Probe", func(c *Client, endpoint string) error {
			return c.Probe(context.Background(), Nova, endpoint, poolToken)
		}},
		{"Reject", func(c *Client, endpoint string) error {
			return c.Reject(context.Background(), Nova, endpoint, poolToken)
		}},
		{"Poll", func(c *Client, endpoint string) error {
			_, err := c.Poll(context.Background(), Nova, endpoint, poolToken)
			return err
		}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			// Probe fails on the 401 itself; Reject and Poll take a 401 as
			// their answer, so a 403 quoting the token stands in for them.
			status, body := http.StatusForbidden, strings.Replace(unauthorized, "401", "403", 1)
			if tc.name == "Probe" {
				status, body = http.StatusUnauthorized, unauthorized
			}
			fs := newFakeService(t, status, body)
			c, _, _, _ := testProbe(t)

			err := tc.call(c, fs.srv.URL+"/")
			if err == nil {
				t.Fatalf("%s returned nil, want the %d", tc.name, status)
			}
			if strings.Contains(err.Error(), poolToken) {
				t.Errorf("%s error %q carries the token", tc.name, err)
			}
			if !gophercloud.ResponseCodeIs(err, status) {
				t.Errorf("%s error = %v, want the %d reachable", tc.name, err, status)
			}
		})
	}
}

// TestRejectOutcomes confirms a 401 is the expected answer to a bad token, a
// 2xx is the service accepting it, labelled accepted, and any other failure is
// returned as is with its status label.
func TestRejectOutcomes(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		wantErr     func(error) bool
		wantSuccess bool
		wantKinds   []string
	}{
		{
			name:        "401",
			status:      http.StatusUnauthorized,
			wantErr:     func(err error) bool { return err == nil },
			wantSuccess: true,
		},
		{
			name:      "200",
			status:    http.StatusOK,
			wantErr:   func(err error) bool { return errors.Is(err, ErrAccepted) },
			wantKinds: []string{"accepted"},
		},
		{
			name:      "203 the read did not expect",
			status:    http.StatusNonAuthoritativeInfo,
			wantErr:   func(err error) bool { return errors.Is(err, ErrAccepted) },
			wantKinds: []string{"accepted"},
		},
		{
			name:   "503",
			status: http.StatusServiceUnavailable,
			wantErr: func(err error) bool {
				return gophercloud.ResponseCodeIs(err, http.StatusServiceUnavailable) && !errors.Is(err, ErrAccepted)
			},
			wantKinds: []string{"http_503"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{}`
			if tc.status == http.StatusUnauthorized {
				body = unauthorized
			}
			fs := newFakeService(t, tc.status, body)
			c, _, m, reader := testProbe(t)

			err := c.Reject(context.Background(), Cinder, fs.srv.URL+"/v3/pid1/", "bad-token")
			if !tc.wantErr(err) {
				t.Errorf("Reject error = %v, unexpected for a %d", err, tc.status)
			}
			if n := len(fs.requests()); n != 1 {
				t.Errorf("requests = %d, want 1", n)
			}
			agg := m.Aggregate(time.Second)
			st := statsOf(agg, KindBadToken)
			if st.Attempted != 1 || (st.Succeeded == 1) != tc.wantSuccess {
				t.Errorf("bad_token samples = %+v, want 1 attempted, success %v", st, tc.wantSuccess)
			}
			if got := errorKinds(agg); strings.Join(got, ",") != strings.Join(tc.wantKinds, ",") {
				t.Errorf("error kinds = %v, want %v", got, tc.wantKinds)
			}
			outcome := "success"
			if !tc.wantSuccess {
				outcome = "error"
			}
			duration := map[string]string{"kind": KindBadToken, "operation": Cinder, "outcome": outcome}
			if !hasPoint(t, reader, "dizzy.operation.duration", duration) {
				t.Errorf("no dizzy.operation.duration point with %v", duration)
			}
			if tc.status == http.StatusOK {
				errs := map[string]string{"kind": KindBadToken, "operation": Cinder, "error.kind": "accepted"}
				if !hasPoint(t, reader, "dizzy.operation.errors", errs) {
					t.Errorf("no dizzy.operation.errors point with %v", errs)
				}
			}
		})
	}
}

// TestPollOutcomes confirms a poll answers whether the service refused the
// token: a 401 is rejected, a 2xx is still accepted, both successful samples
// since the poll got its answer, and any other failure is a failed sample
// with the error returned.
func TestPollOutcomes(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		wantRejected bool
		wantErr      bool
		wantSuccess  bool
		wantKinds    []string
	}{
		{name: "401", status: http.StatusUnauthorized, wantRejected: true, wantSuccess: true},
		{name: "200", status: http.StatusOK, wantSuccess: true},
		{name: "203 the read did not expect", status: http.StatusNonAuthoritativeInfo, wantSuccess: true},
		{name: "503", status: http.StatusServiceUnavailable, wantErr: true, wantKinds: []string{"http_503"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"networks":[]}`
			if tc.status == http.StatusUnauthorized {
				body = unauthorized
			}
			fs := newFakeService(t, tc.status, body)
			c, _, m, reader := testProbe(t)

			rejected, err := c.Poll(context.Background(), Neutron, fs.srv.URL+"/", poolToken)
			if rejected != tc.wantRejected || (err != nil) != tc.wantErr {
				t.Errorf("Poll = (%v, %v), want rejected %v and an error %v", rejected, err, tc.wantRejected, tc.wantErr)
			}
			if tc.wantErr && !gophercloud.ResponseCodeIs(err, tc.status) {
				t.Errorf("Poll error = %v, want the %d", err, tc.status)
			}
			agg := m.Aggregate(time.Second)
			st := statsOf(agg, KindPropagation)
			if st.Attempted != 1 || (st.Succeeded == 1) != tc.wantSuccess {
				t.Errorf("propagation samples = %+v, want 1 attempted, success %v", st, tc.wantSuccess)
			}
			if got := errorKinds(agg); strings.Join(got, ",") != strings.Join(tc.wantKinds, ",") {
				t.Errorf("error kinds = %v, want %v", got, tc.wantKinds)
			}
			outcome := "success"
			if !tc.wantSuccess {
				outcome = "error"
			}
			duration := map[string]string{"kind": KindPropagation, "operation": Neutron, "outcome": outcome}
			if !hasPoint(t, reader, "dizzy.operation.duration", duration) {
				t.Errorf("no dizzy.operation.duration point with %v", duration)
			}
		})
	}
}

// TestUnknownService confirms a service outside Services fails every call
// before any request and without a sample.
func TestUnknownService(t *testing.T) {
	fs := newFakeService(t, http.StatusOK, `{}`)
	c, _, m, _ := testProbe(t)
	const want = `probe: unknown service "swift"`
	endpoint := fs.srv.URL + "/"

	if err := c.Probe(context.Background(), "swift", endpoint, poolToken); err == nil || err.Error() != want {
		t.Errorf("Probe error = %v, want %q", err, want)
	}
	if err := c.Reject(context.Background(), "swift", endpoint, poolToken); err == nil || err.Error() != want {
		t.Errorf("Reject error = %v, want %q", err, want)
	}
	if rejected, err := c.Poll(context.Background(), "swift", endpoint, poolToken); rejected || err == nil || err.Error() != want {
		t.Errorf("Poll = (%v, %v), want (false, %q)", rejected, err, want)
	}
	if n := len(fs.requests()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
	if attempted, _, _ := m.Snapshot(); attempted != 0 {
		t.Errorf("samples = %d, want 0", attempted)
	}
}

// TestRecordRevocationDelay confirms a propagation delay lands on the
// time-to-ready instrument under the revocation kind, a timed-out probe as a
// not-OK record and a timeout outcome.
func TestRecordRevocationDelay(t *testing.T) {
	c, _, m, reader := testProbe(t)

	c.RecordRevocationDelay(context.Background(), 2*time.Second, false)

	var got *metrics.ReadinessStats
	for _, r := range m.Aggregate(time.Second).Readiness {
		if r.Type == KindRevocation {
			got = &r
		}
	}
	if got == nil || got.Count != 1 || got.OK != 0 {
		t.Errorf("revocation readiness = %+v, want Count 1 and OK 0", got)
	}
	if got != nil && got.Latency.Max != 2*time.Second {
		t.Errorf("revocation delay max = %v, want 2s", got.Latency.Max)
	}
	point := map[string]string{"kind": KindRevocation, "outcome": "timeout"}
	if !hasPoint(t, reader, "dizzy.resource.time_to_ready", point) {
		t.Errorf("no dizzy.resource.time_to_ready point with %v", point)
	}
}
