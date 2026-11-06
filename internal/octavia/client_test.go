package octavia

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
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
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

// call is one request a fake received: its method, path, raw query, and body.
type call struct {
	method string
	path   string
	query  string
	body   string
}

// String renders the call as "<method> <path>", with "?<query>" appended when
// the request carried one.
func (c call) String() string {
	if c.query == "" {
		return c.method + " " + c.path
	}
	return c.method + " " + c.path + "?" + c.query
}

// fake is an httptest server that records every request and answers it
// through answer. Octavia calls reach it below /v2.0/ (so /v2.0/lbaas/... and
// /v2.0/quotas/...), the prefix openstack.NewLoadBalancerV2 appends, and
// Neutron calls at the root (/networks, /subnets).
type fake struct {
	srv    *httptest.Server
	answer func(w http.ResponseWriter, r *http.Request, c call)

	mu    sync.Mutex
	calls []call
}

// newFake starts a fake answering through answer, closed when the test ends.
func newFake(t *testing.T, answer func(w http.ResponseWriter, r *http.Request, c call)) *fake {
	t.Helper()
	f := &fake{answer: answer}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := call{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(body)}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()
		f.answer(w, r, c)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// lbClient returns a load-balancer service client whose calls hit the fake.
// Its ResourceBase carries the v2.0/ suffix openstack.NewLoadBalancerV2
// appends.
func (f *fake) lbClient() *gophercloud.ServiceClient {
	return &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       f.srv.URL + "/",
		ResourceBase:   f.srv.URL + "/v2.0/",
	}
}

// client builds a Client for run0 whose load-balancer and network calls hit
// the fake and record into the returned Collector, with telemetry disabled.
func (f *fake) client() (*Client, *metrics.Collector) {
	m := metrics.NewCollector()
	network := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       f.srv.URL + "/",
	}
	return New(f.lbClient(), network, "run0", m), m
}

// telemetryClient builds the fake's client with a ManualReader-backed
// telemetry attached, so a test can assert both the in-memory samples and the
// live series of one call.
func (f *fake) telemetryClient(t *testing.T) (*Client, *metrics.Collector, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	tel, err := telemetry.NewWithProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatalf("NewWithProvider: %v", err)
	}
	c, m := f.client()
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

// requests returns the calls the fake received so far, in order.
func (f *fake) requests() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// lines returns the String of every call the fake received so far.
func (f *fake) lines() []string {
	var out []string
	for _, c := range f.requests() {
		out = append(out, c.String())
	}
	return out
}

// wantLines fails the test unless the fake received exactly want, in order.
func (f *fake) wantLines(t *testing.T, want ...string) {
	t.Helper()
	if got := f.lines(); !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q", got, want)
	}
}

// reply is the status and JSON body a scripted fake answers one request with.
// A status of 0 drops the connection without an answer, which gophercloud
// reports as an error without a status.
type reply struct {
	status int
	body   string
}

// scripted returns a fake answer that answers the n-th request to
// "<method> <path>" with the n-th reply script lists for it, and every later one
// with its last reply. A request script does not list fails the test.
func scripted(t *testing.T, script map[string][]reply) func(http.ResponseWriter, *http.Request, call) {
	var mu sync.Mutex
	seen := map[string]int{}
	return func(w http.ResponseWriter, _ *http.Request, c call) {
		key := c.method + " " + c.path
		mu.Lock()
		n := seen[key]
		seen[key]++
		mu.Unlock()
		replies, ok := script[key]
		if !ok || len(replies) == 0 {
			t.Errorf("unexpected request %s", c)
			writeJSON(w, http.StatusInternalServerError, `{}`)
			return
		}
		rep := replies[min(n, len(replies)-1)]
		if rep.status == 0 {
			loseAnswer(t, w)
			return
		}
		writeJSON(w, rep.status, rep.body)
	}
}

// loseAnswer hijacks the connection of w and closes it without an answer, which
// gophercloud reports as a *url.Error wrapping io.EOF.
func loseAnswer(t *testing.T, w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Error("the response writer cannot hijack its connection")
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Errorf("hijacking the connection: %v", err)
		return
	}
	_ = conn.Close()
}

// item renders one Octavia object of a list or get answer with its id, name,
// provisioning status, and tags.
func item(id, name, status string, tags ...string) string {
	quoted := make([]string, 0, len(tags))
	for _, tag := range tags {
		quoted = append(quoted, `"`+tag+`"`)
	}
	return fmt.Sprintf(`{"id":%q,"name":%q,"provisioning_status":%q,"tags":[%s]}`, id, name, status, strings.Join(quoted, ","))
}

// writeJSON answers a request with status and a JSON body.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// decodeBody decodes a JSON request body and returns the object under key, the
// envelope gophercloud wraps every create body in.
func decodeBody(t *testing.T, body, key string) map[string]any {
	t.Helper()
	var envelope map[string]map[string]any
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decoding request body %q: %v", body, err)
	}
	obj, ok := envelope[key]
	if !ok {
		t.Fatalf("request body %q has no %q object", body, key)
	}
	return obj
}

// wantTags fails the test unless the decoded body field tags holds exactly the
// two run tags of kind for run0.
func wantTags(t *testing.T, obj map[string]any, kind resource.Kind) {
	t.Helper()
	want := []any{"dizzy:run=run0", "dizzy:type=" + string(kind)}
	if got := obj["tags"]; !reflect.DeepEqual(got, want) {
		t.Errorf("tags = %v, want %v", got, want)
	}
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

// waitCtx returns a context bounding one readiness or deletion wait of a test
// by a deadline far above what the wait needs, so a wait that never ends fails
// its test instead of hanging the test binary.
func waitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// fastPolls shrinks the poll and tag-retry intervals for the duration of the
// test, so waits and retries finish in milliseconds. A test that calls it must
// not run in parallel.
func fastPolls(t *testing.T) {
	t.Helper()
	prevInitial, prevMax, prevTag := pollInitial, pollMax, tagRetryDelay
	pollInitial, pollMax, tagRetryDelay = time.Millisecond, 5*time.Millisecond, time.Millisecond
	t.Cleanup(func() { pollInitial, pollMax, tagRetryDelay = prevInitial, prevMax, prevTag })
}

func TestErrKind(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"provisioning", ErrProvisioning, "provisioning_error"},
		{"wrapped provisioning", fmt.Errorf("%w: load balancer lb-1 reached ERROR after a listener write", ErrProvisioning), "provisioning_error"},
		{"octavia quota 403", respErr(403, `{"faultstring":"Quota has been met for resources: Listener"}`), "quota"},
		{"neutron quota 409", respErr(409, `{"NeutronError":{"type":"OverQuota","message":"Quota exceeded for resources: ['network']."}}`), "quota"},
		{"wrapped quota 403", wrapCreate(KindListener, "lsn-0001", respErr(403, "Quota has been met")), "quota"},
		{"policy 403 without quota", respErr(403, "Policy does not allow this request to be performed."), "http_403"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"canceled", context.Canceled, "canceled"},
		{"http 400", respErr(400, "bad request"), "http_400"},
		{"http 401", respErr(401, "unauthorized"), "http_401"},
		{"http 404", respErr(404, "not found"), "http_404"},
		{"http 409", respErr(409, "Load Balancer lb-1 is immutable and cannot be updated."), "http_409"},
		{"http 413 is not an octavia status", respErr(413, "too large"), "http_other"},
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
		{"octavia 403 quota has been met", respErr(403, "Quota has been met for resources: Load Balancer"), true},
		{"neutron 409 over quota", respErr(409, "Quota exceeded for resources: ['subnet']."), true},
		{"400 over limit", respErr(400, "OverLimit"), true},
		{"wrapped 403 quota", fmt.Errorf("creating listener: %w", respErr(403, "quota")), true},
		{"403 policy denial", respErr(403, "Policy does not allow this request to be performed."), false},
		{"409 immutable load balancer", respErr(409, "Load Balancer lb-1 is immutable and cannot be updated."), false},
		{"413 with quota body", respErr(413, "quota"), false},
		{"500 with quota body", respErr(500, "quota"), false},
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
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"409 conflict", respErr(409, "Load Balancer lb-1 is immutable and cannot be updated."), true},
		{"429 rate limit", respErr(429, "slow down"), true},
		{"500", respErr(500, "boom"), true},
		{"502", respErr(502, "bad gateway"), true},
		{"503", respErr(503, "unavailable"), true},
		{"504", respErr(504, "gateway timeout"), true},
		{"wrapped 409", wrapCreate(KindListener, "lsn-0001", respErr(409, "Duplicate listener")), true},
		{"quota 403", respErr(403, "Quota has been met for resources: Listener"), false},
		{"quota 409", respErr(409, "Quota exceeded for resources: ['network']."), false},
		{"provisioning", ErrProvisioning, false},
		{"wrapped provisioning", fmt.Errorf("%w: load balancer lb-1 reached ERROR after a pool write", ErrProvisioning), false},
		{"provisioning wrapping a 409", fmt.Errorf("%w: %w", ErrProvisioning, respErr(409, "conflict")), false},
		{"400", respErr(400, "bad request"), false},
		{"404", respErr(404, "not found"), false},
		{"non-http", errors.New("boom"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestMayHaveLanded confirms only a failure after which the service may have
// committed the create counts: a 5xx and an error without a status, never a
// refusal below 500 or an input error gophercloud raised before sending.
func TestMayHaveLanded(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"400", respErr(400, "bad request"), false},
		{"409", respErr(409, "conflict"), false},
		{"quota 403", respErr(403, "Quota has been met"), false},
		{"500", respErr(500, "boom"), true},
		{"503", respErr(503, "unavailable"), true},
		{"lost connection", &url.Error{Op: "Post", URL: "http://lb/v2.0/lbaas/listeners", Err: io.EOF}, true},
		{"deadline", context.DeadlineExceeded, true},
		{"missing input", gophercloud.ErrMissingInput{Argument: "lb_algorithm"}, false},
		{"invalid input", gophercloud.ErrInvalidInput{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mayHaveLanded(tc.err); got != tc.want {
				t.Errorf("mayHaveLanded(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(respErr(404, "not found")) {
		t.Error("IsNotFound(404) = false, want true")
	}
	if !IsNotFound(fmt.Errorf("deleting: %w", gophercloud.ErrResourceNotFound{})) {
		t.Error("IsNotFound(wrapped ErrResourceNotFound) = false, want true")
	}
	for _, err := range []error{nil, respErr(409, "conflict"), errors.New("boom")} {
		if IsNotFound(err) {
			t.Errorf("IsNotFound(%v) = true, want false", err)
		}
	}
}

// TestPoolAlgorithm confirms the OVN provider gets the one algorithm it
// supports and every other provider, the cloud default included, ROUND_ROBIN.
func TestPoolAlgorithm(t *testing.T) {
	for provider, want := range map[string]string{
		"ovn":     "SOURCE_IP_PORT",
		"amphora": "ROUND_ROBIN",
		"":        "ROUND_ROBIN",
		"octavia": "ROUND_ROBIN",
	} {
		if got := PoolAlgorithm(provider); got != want {
			t.Errorf("PoolAlgorithm(%q) = %q, want %q", provider, got, want)
		}
	}
}

// TestResourceNameAndTags confirms the deterministic cloud name and the tag
// pair every created resource carries.
func TestResourceNameAndTags(t *testing.T) {
	if got, want := resourceName("run0", "lb-0001"), "dizzy-run0-lb-0001"; got != want {
		t.Errorf("resourceName = %q, want %q", got, want)
	}
	if got, want := runTag("run0"), "dizzy:run=run0"; got != want {
		t.Errorf("runTag = %q, want %q", got, want)
	}
	if got, want := runTags("run0", KindHealthMonitor), []string{"dizzy:run=run0", "dizzy:type=healthmonitor"}; !slices.Equal(got, want) {
		t.Errorf("runTags = %q, want %q", got, want)
	}
}

// TestKindValues pins the kind strings, which are the metrics type label and
// the dizzy:type tag value the docs and dashboards name.
func TestKindValues(t *testing.T) {
	got := []resource.Kind{KindNetwork, KindSubnet, KindLoadBalancer, KindListener, KindPool, KindMember, KindHealthMonitor}
	want := []resource.Kind{"network", "subnet", "loadbalancer", "listener", "pool", "member", "healthmonitor"}
	if !slices.Equal(got, want) {
		t.Errorf("kinds = %q, want %q", got, want)
	}
}

// TestWrapCreateThreadsQuota confirms a quota rejection is wrapped with
// ErrQuota and keeps the gophercloud error for classification, while any other
// failure is wrapped without it.
func TestWrapCreateThreadsQuota(t *testing.T) {
	quota := wrapCreate(KindLoadBalancer, "lb-0001", respErr(403, "Quota has been met"))
	if !errors.Is(quota, ErrQuota) {
		t.Errorf("wrapCreate(quota 403) = %v, want it to wrap ErrQuota", quota)
	}
	if got := httpStatus(quota); got != 403 {
		t.Errorf("httpStatus(wrapCreate(quota 403)) = %d, want 403", got)
	}
	if !strings.HasPrefix(quota.Error(), `creating loadbalancer "lb-0001": `) {
		t.Errorf("wrapCreate error = %q, want the creating prefix", quota)
	}
	other := wrapCreate(KindListener, "lsn-0001", respErr(409, "conflict"))
	if errors.Is(other, ErrQuota) {
		t.Errorf("wrapCreate(409 conflict) = %v, want no ErrQuota", other)
	}
}

// TestTimedRecordsOneSample confirms timed records one sample of its type with
// the error kind of fn's error and returns that error unchanged.
func TestTimedRecordsOneSample(t *testing.T) {
	m := metrics.NewCollector()
	c := New(nil, nil, "run0", m)
	err := c.timed(context.Background(), string(KindPool), "create", func(context.Context) error { return respErr(503, "unavailable") })
	if httpStatus(err) != 503 {
		t.Errorf("timed = %v, want fn's 503", err)
	}
	agg := m.Aggregate(time.Second)
	if s := statsOf(agg, "pool"); s.Attempted != 1 || s.Failed != 1 {
		t.Errorf("pool stats = %+v, want one failed sample", s)
	}
	if len(agg.Errors) != 1 || agg.Errors[0].Kind != "http_503" {
		t.Errorf("errors = %+v, want one http_503", agg.Errors)
	}
}
