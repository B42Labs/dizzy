package c5c3

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/B42Labs/dizzy/internal/c5c3/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// The namespace and run id every test client acts for.
const (
	testNamespace = "dizzy-orders"
	testRun       = "run0"
)

// newFakeDynamic returns a fake dynamic client that serves the five order
// kinds and Secrets, listings included.
func newFakeDynamic() *fake.FakeDynamicClient {
	listKinds := map[schema.GroupVersionResource]string{secretsGVR: "SecretList"}
	for _, k := range orderKinds {
		listKinds[k.gvr()] = k.k8sKind + "List"
	}
	return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
}

// testPlan is the plan CreateOrder reads its ControlPlane and grace period
// from, with the five orders of the bundled profile.
func testPlan() *plan.Plan {
	return &plan.Plan{
		Scenario:     "small",
		Namespace:    testNamespace,
		ControlPlane: plan.ControlPlane{Name: "controlplane", Namespace: "openstack"},
		Orders: []plan.Order{
			{Kind: plan.KindUser, Name: "user"},
			{Kind: plan.KindProject, Name: "project"},
			{Kind: plan.KindRoleAssignment, Name: "role", User: "user", Project: "project", Role: "member"},
			{Kind: plan.KindCatalogEntry, Name: "catalog", ServiceType: "dizzy-probe", EndpointURL: "https://dizzy-probe.invalid/v1"},
			{Kind: plan.KindApplicationCredential, Name: "appcred", User: "user", Project: "project"},
		},
		GracePeriod:  2 * time.Minute,
		ReadyTimeout: 15 * time.Minute,
		GoneTimeout:  15 * time.Minute,
		SecretRepair: true,
	}
}

// testClient builds a Client for testRun in testNamespace over a new fake,
// with testPlan set, recording into the returned Collector, with telemetry
// disabled. Its polls follow each other within milliseconds, so a wait with a
// short context polls many times.
func testClient() (*Client, *fake.FakeDynamicClient, *metrics.Collector) {
	dyn := newFakeDynamic()
	m := metrics.NewCollector()
	c := New(dyn, testNamespace, testRun, m)
	c.SetPlan(testPlan())
	c.pollInitial, c.pollMax = time.Millisecond, 2*time.Millisecond
	return c, dyn, m
}

// telemetryTestClient builds a testClient that also mirrors each call and wait
// into a ManualReader-backed telemetry.
func telemetryTestClient(t *testing.T) (*Client, *fake.FakeDynamicClient, *metrics.Collector, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	tel, err := telemetry.NewWithProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatalf("NewWithProvider: %v", err)
	}
	c, dyn, m := testClient()
	c.SetTelemetry(tel)
	return c, dyn, m, reader
}

// orderRef returns the resource a created order of kind and logical name is
// recorded as.
func orderRef(kind resource.Kind, logical string) resource.Resource {
	name := resourceName(testRun, logical)
	return resource.Resource{Kind: kind, Logical: logical, Name: name, ID: testNamespace + "/" + name}
}

// orderObject returns the order of kind r addresses at metadata.generation
// generation, labelled for testRun, without a status.
func orderObject(r resource.Resource, generation int64) *unstructured.Unstructured {
	k, _ := lookupKind(r.Kind)
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "c5c3.io/v1alpha1",
		"kind":       k.k8sKind,
		"metadata": map[string]any{
			"name":       r.Name,
			"namespace":  testNamespace,
			"generation": generation,
			"labels":     map[string]any{labelRun: testRun, labelType: string(r.Kind)},
		},
		"spec": map[string]any{"controlPlaneRef": map[string]any{"name": "controlplane"}},
	}}
	return u
}

// withStatus sets the order's status to observedGeneration observed, the
// given conditions, and the extra status fields.
func withStatus(u *unstructured.Unstructured, observed int64, extra map[string]any, conditions ...map[string]any) *unstructured.Unstructured {
	u = u.DeepCopy()
	status := map[string]any{"observedGeneration": observed}
	conds := make([]any, 0, len(conditions))
	for _, cd := range conditions {
		conds = append(conds, cd)
	}
	status["conditions"] = conds
	for k, v := range extra {
		status[k] = v
	}
	u.Object["status"] = status
	return u
}

// cond returns a condition of type typ with status and reason, and a message
// naming both.
func cond(typ, status, reason string) map[string]any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "message": "message of " + reason}
}

// readyObject returns the order r addresses at generation 1, ready at
// generation 1, with the extra status fields.
func readyObject(r resource.Resource, extra map[string]any) *unstructured.Unstructured {
	return withStatus(orderObject(r, 1), 1, extra, cond("UserReady", "True", "UserProvisioned"), cond("Ready", "True", "AllReady"))
}

// pendingObject returns the order r addresses at generation 1, reporting
// reason on its sub-condition and NotAllReady on Ready.
func pendingObject(r resource.Resource, reason string) *unstructured.Unstructured {
	return withStatus(orderObject(r, 1), 1, nil, cond("UserReady", "False", reason), cond("Ready", "False", "NotAllReady"))
}

// gvrOf returns the resource the kind's objects are stored under.
func gvrOf(kind resource.Kind) schema.GroupVersionResource {
	if kind == KindSecret {
		return secretsGVR
	}
	k, _ := lookupKind(kind)
	return k.gvr()
}

// seed stores obj under kind's resource in the fake's tracker without
// recording an action.
func seed(t *testing.T, dyn *fake.FakeDynamicClient, kind resource.Kind, obj *unstructured.Unstructured) {
	t.Helper()
	if err := dyn.Tracker().Create(gvrOf(kind), obj, testNamespace); err != nil {
		t.Fatalf("seeding %s %s: %v", kind, obj.GetName(), err)
	}
}

// replace overwrites the stored obj under kind's resource, as the operator
// writing a status or a Secret would.
func replace(t *testing.T, dyn *fake.FakeDynamicClient, kind resource.Kind, obj *unstructured.Unstructured) {
	t.Helper()
	if err := dyn.Tracker().Update(gvrOf(kind), obj, testNamespace); err != nil {
		t.Fatalf("replacing %s %s: %v", kind, obj.GetName(), err)
	}
}

// stored reads obj name of kind back from the fake's tracker.
func stored(t *testing.T, dyn *fake.FakeDynamicClient, kind resource.Kind, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := dyn.Tracker().Get(gvrOf(kind), testNamespace, name)
	if err != nil {
		t.Fatalf("reading %s %s: %v", kind, name, err)
	}
	return obj.(*unstructured.Unstructured)
}

// script makes the fake answer the gets of resource with answers in turn,
// repeating the last one: an *unstructured.Unstructured is returned as the
// object, an error fails the get. It plays the operator, whose writes the
// fake does not make.
func script(t *testing.T, dyn *fake.FakeDynamicClient, resource string, answers ...any) {
	t.Helper()
	if len(answers) == 0 {
		t.Fatal("script needs at least one answer")
	}
	n := 0
	dyn.PrependReactor("get", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		answer := answers[min(n, len(answers)-1)]
		n++
		switch a := answer.(type) {
		case error:
			return true, nil, a
		case *unstructured.Unstructured:
			return true, a.DeepCopy(), nil
		default:
			panic(fmt.Sprintf("script answer of type %T", answer))
		}
	})
}

// failVerb makes the fake answer every verb request on resource with err.
func failVerb(dyn *fake.FakeDynamicClient, verb, resource string, err error) {
	dyn.PrependReactor(verb, resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, err
	})
}

// countActions counts the verb requests on resource the fake received.
func countActions(dyn *fake.FakeDynamicClient, verb, resource string) int {
	n := 0
	for _, a := range dyn.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

// notFound, forbidden, conflict and internalError build the Kubernetes API
// errors the fake answers with.
func notFound(resource, name string) error {
	return apierrors.NewNotFound(schema.GroupResource{Group: apiGroup, Resource: resource}, name)
}

func forbidden(resource, name string) error {
	return apierrors.NewForbidden(schema.GroupResource{Group: apiGroup, Resource: resource}, name,
		errors.New(`User "dizzy" cannot do this in the namespace "dizzy-orders"`))
}

func conflict(resource, name string) error {
	return apierrors.NewConflict(schema.GroupResource{Group: apiGroup, Resource: resource}, name, errors.New("the object has been modified"))
}

func internalError() error {
	return apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
}

// kubeStatus builds a Kubernetes API error carrying only an HTTP status code.
func kubeStatus(code int32) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: code, Message: fmt.Sprintf("status %d", code)}}
}

// keystoneStatus builds a gophercloud unexpected-response error for status.
func keystoneStatus(code int) error {
	return gophercloud.ErrUnexpectedResponseCode{Actual: code, Body: []byte("keystone answered")}
}

// statsOf returns the aggregate's stats for one sample type, or zero stats
// when no sample of that type was recorded.
func statsOf(agg metrics.Aggregate, typ resource.Kind) metrics.Stats {
	for _, s := range agg.ByType {
		if s.Type == string(typ) {
			return s
		}
	}
	return metrics.Stats{}
}

// readinessOf returns the aggregate's time-to-ready stats for one type, or
// zero stats when none was recorded.
func readinessOf(agg metrics.Aggregate, typ resource.Kind) metrics.ReadinessStats {
	for _, r := range agg.Readiness {
		if r.Type == string(typ) {
			return r
		}
	}
	return metrics.ReadinessStats{}
}

// errorCount returns how many failed samples classified to kind.
func errorCount(agg metrics.Aggregate, kind string) int {
	for _, e := range agg.Errors {
		if e.Kind == kind {
			return e.Count
		}
	}
	return 0
}

// point is one data point of a float64 histogram or an int64 sum: its string
// attributes and how many values it holds.
type point struct {
	attrs map[string]string
	count int64
}

// metricPoints collects reader once and returns every data point of the named
// float64 histogram or int64 sum, and an empty slice when it recorded
// nothing.
func metricPoints(t *testing.T, reader *sdkmetric.ManualReader, name string) []point {
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
	points := []point{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					points = append(points, point{attrs(dp.Attributes), int64(dp.Count)})
				}
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					points = append(points, point{attrs(dp.Attributes), dp.Value})
				}
			default:
				t.Fatalf("%s is neither a float64 histogram nor an int64 sum: %T", name, m.Data)
			}
		}
	}
	return points
}

// countOf returns how many values the point with exactly attrs holds, and 0
// when there is none.
func countOf(points []point, attrs map[string]string) int64 {
	for _, p := range points {
		if fmt.Sprint(p.attrs) == fmt.Sprint(attrs) {
			return p.count
		}
	}
	return 0
}

// captureLog sends the process-wide default logger to the returned buffer
// until the test ends. A test that calls it must not run in parallel.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// TestKinds confirms each order kind is the plan's kind, served as the
// c5c3.io/v1alpha1 resource and Kubernetes kind of the issue, and that the
// Secret kind is not an order kind.
func TestKinds(t *testing.T) {
	for _, tc := range []struct {
		kind     resource.Kind
		plan     string
		resource string
		k8sKind  string
	}{
		{KindUser, "keystone_user", "keystoneusers", "KeystoneUser"},
		{KindProject, "keystone_project", "keystoneprojects", "KeystoneProject"},
		{KindRoleAssignment, "keystone_role_assignment", "keystoneroleassignments", "KeystoneRoleAssignment"},
		{KindCatalogEntry, "keystone_catalog_entry", "keystonecatalogentries", "KeystoneCatalogEntry"},
		{KindApplicationCredential, "keystone_application_credential", "keystoneapplicationcredentials", "KeystoneApplicationCredential"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			if string(tc.kind) != tc.plan {
				t.Errorf("kind = %q, want %q", tc.kind, tc.plan)
			}
			k, ok := lookupKind(tc.kind)
			if !ok {
				t.Fatalf("lookupKind(%q) found nothing", tc.kind)
			}
			want := schema.GroupVersionResource{Group: "c5c3.io", Version: "v1alpha1", Resource: tc.resource}
			if k.gvr() != want || k.k8sKind != tc.k8sKind {
				t.Errorf("lookupKind(%q) = %v %s, want %v %s", tc.kind, k.gvr(), k.k8sKind, want, tc.k8sKind)
			}
		})
	}
	if KindSecret != "secret" {
		t.Errorf("KindSecret = %q, want secret", KindSecret)
	}
	for _, kind := range []resource.Kind{KindSecret, "volume"} {
		if _, ok := lookupKind(kind); ok {
			t.Errorf("lookupKind(%q) found an order kind", kind)
		}
	}
	if want := (schema.GroupVersionResource{Version: "v1", Resource: "secrets"}); secretsGVR != want {
		t.Errorf("secretsGVR = %v, want %v", secretsGVR, want)
	}
}

func TestResourceAndSecretNames(t *testing.T) {
	if got, want := resourceName("abc123", "user"), "dizzyorder-abc123-user"; got != want {
		t.Errorf("resourceName = %q, want %q", got, want)
	}
	if got, want := runPrefix("abc123"), "dizzyorder-abc123-"; got != want {
		t.Errorf("runPrefix = %q, want %q", got, want)
	}
	if got, want := secretName("dizzyorder-abc123-user"), "dizzyorder-abc123-user-credentials"; got != want {
		t.Errorf("secretName = %q, want %q", got, want)
	}
	// The Keystone namespace's orphan sweep deletes users named dizzy-*.
	if strings.HasPrefix(resourceName("abc123", "user"), "dizzy-") {
		t.Error("an order name carries the dizzy- prefix the Keystone sweep reclaims")
	}
}

func TestParseID(t *testing.T) {
	for _, tc := range []struct {
		id              string
		namespace, name string
		wantErr         bool
	}{
		{"dizzy-orders/dizzyorder-run0-user", "dizzy-orders", "dizzyorder-run0-user", false},
		{"dizzyorder-run0-user", "", "", true},
		{"a/b/c", "", "", true},
		{"/dizzyorder-run0-user", "", "", true},
		{"dizzy-orders/", "", "", true},
		{"/", "", "", true},
		{"", "", "", true},
	} {
		t.Run(tc.id, func(t *testing.T) {
			ns, name, err := parseID(tc.id)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseID(%q) error = %v, want error %v", tc.id, err, tc.wantErr)
			}
			if ns != tc.namespace || name != tc.name {
				t.Errorf("parseID(%q) = (%q, %q), want (%q, %q)", tc.id, ns, name, tc.namespace, tc.name)
			}
		})
	}
}

// TestPollIntervals confirms New polls from 500 ms, growing by half up to 5 s.
func TestPollIntervals(t *testing.T) {
	c := New(newFakeDynamic(), testNamespace, testRun, metrics.NewCollector())
	if c.pollInitial != 500*time.Millisecond || c.pollMax != 5*time.Second {
		t.Fatalf("New poll intervals = %s up to %s, want 500ms up to 5s", c.pollInitial, c.pollMax)
	}
	var got []time.Duration
	for d := c.pollInitial; len(got) < 8; d = nextInterval(d, c.pollMax) {
		got = append(got, d)
	}
	want := []time.Duration{
		500 * time.Millisecond, 750 * time.Millisecond, 1125 * time.Millisecond, 1687500 * time.Microsecond,
		2531250 * time.Microsecond, 3796875 * time.Microsecond, 5 * time.Second, 5 * time.Second,
	}
	if !slices.Equal(got, want) {
		t.Errorf("intervals = %v, want %v", got, want)
	}
}

// TestWaitPollsWithBackoff runs a wait with New's own intervals and confirms
// the second poll comes no sooner than 500 ms after the first and a third no
// sooner than 750 ms after the second.
func TestWaitPollsWithBackoff(t *testing.T) {
	dyn := newFakeDynamic()
	c := New(dyn, testNamespace, testRun, metrics.NewCollector())
	r := orderRef(KindUser, "user")
	var at []time.Time
	dyn.PrependReactor("get", "keystoneusers", func(k8stesting.Action) (bool, runtime.Object, error) {
		at = append(at, time.Now())
		return true, orderObject(r, 1), nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1400*time.Millisecond)
	defer cancel()
	if err := c.WaitReady(ctx, r); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady = %v, want context.DeadlineExceeded", err)
	}
	if len(at) < 2 {
		t.Fatalf("polls = %d within 1.4s, want at least 2", len(at))
	}
	if gap := at[1].Sub(at[0]); gap < 500*time.Millisecond {
		t.Errorf("second poll %s after the first, want at least 500ms", gap)
	}
	if len(at) > 2 {
		if gap := at[2].Sub(at[1]); gap < 750*time.Millisecond {
			t.Errorf("third poll %s after the second, want at least 750ms", gap)
		}
	}
}

// connError builds the error a dial or read of the API server returns for
// errno, wrapped the way net/http and client-go wrap it.
func connError(errno syscall.Errno) error {
	return &url.Error{Op: "Get", URL: "https://k8s.invalid/apis", Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}}
}

func TestErrKind(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"refused", fmt.Errorf("%w: order x reports RoleNotAllowed", ErrRefused), "refused"},
		{"mismatch", fmt.Errorf("%w: secret x is missing", ErrMismatch), "mismatch"},
		{"accepted", fmt.Errorf("%w: keystone issued a token", ErrAccepted), "accepted"},
		{"mismatch over a 404", fmt.Errorf("%w: secret x is missing: %w", ErrMismatch, notFound("secrets", "x")), "mismatch"},
		{"mismatch over a deadline", fmt.Errorf("%w: secret x is still present: %w", ErrMismatch, context.DeadlineExceeded), "mismatch"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"wrapped timeout", fmt.Errorf("get: %w", context.DeadlineExceeded), "timeout"},
		{"canceled", context.Canceled, "canceled"},
		{"kind not served", fmt.Errorf("%w: %w", ErrKindNotServed, notFound("keystoneusers", "")), "http_404"},
		{"forbidden", fmt.Errorf("%w: %w", ErrForbidden, forbidden("keystoneusers", "")), "http_403"},
		{"patch conflict", wrapPatchError(conflict("keystoneusers", "x")), "http_409"},
		{"kubernetes 404", notFound("keystoneusers", "x"), "http_404"},
		{"kubernetes 403", forbidden("secrets", "x"), "http_403"},
		{"kubernetes 409", conflict("keystoneusers", "x"), "http_409"},
		{"kubernetes 500", internalError(), "http_500"},
		{"kubernetes 418", kubeStatus(418), "http_other"},
		{"keystone 418", keystoneStatus(418), "http_other"},
		{"kubernetes 302", kubeStatus(302), "http_other"},
		{"connection refused", connError(syscall.ECONNREFUSED), "other"},
		{"other", errors.New("boom"), "other"},
	}
	for _, code := range []int{400, 401, 403, 404, 409, 422, 429, 500, 502, 503, 504} {
		want := fmt.Sprintf("http_%d", code)
		tests = append(tests,
			struct {
				name string
				err  error
				want string
			}{fmt.Sprintf("kubernetes status %d", code), kubeStatus(int32(code)), want},
			struct {
				name string
				err  error
				want string
			}{fmt.Sprintf("keystone status %d", code), fmt.Errorf("issuing a token: %w", keystoneStatus(code)), want},
		)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := errKind(tc.err); got != tc.want {
				t.Errorf("errKind(%v) = %q, want %q", tc.err, got, tc.want)
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
		{"refused", fmt.Errorf("%w: x", ErrRefused), false},
		{"mismatch", fmt.Errorf("%w: x", ErrMismatch), false},
		{"accepted", fmt.Errorf("%w: x", ErrAccepted), false},
		{"kind not served", fmt.Errorf("%w: %w", ErrKindNotServed, notFound("keystoneusers", "")), false},
		{"forbidden", fmt.Errorf("%w: %w", ErrForbidden, forbidden("secrets", "x")), false},
		{"mismatch over a 503", fmt.Errorf("%w: %w", ErrMismatch, kubeStatus(503)), false},
		{"mismatch over a reset", fmt.Errorf("%w: %w", ErrMismatch, connError(syscall.ECONNRESET)), false},
		{"kubernetes 429", kubeStatus(429), true},
		{"kubernetes 500", internalError(), true},
		{"kubernetes 502", kubeStatus(502), true},
		{"kubernetes 503", kubeStatus(503), true},
		{"kubernetes 504", kubeStatus(504), true},
		{"keystone 429", keystoneStatus(429), true},
		{"keystone 500", keystoneStatus(500), true},
		{"keystone 502", keystoneStatus(502), true},
		{"keystone 503", keystoneStatus(503), true},
		{"keystone 504", keystoneStatus(504), true},
		{"patch 409", wrapPatchError(conflict("keystoneusers", "x")), true},
		{"wrapped patch 409", fmt.Errorf("raising: %w", wrapPatchError(conflict("keystoneusers", "x"))), true},
		{"create 409", conflict("keystoneusers", "x"), false},
		{"keystone 409", keystoneStatus(409), false},
		{"404", notFound("keystoneusers", "x"), false},
		{"403", forbidden("keystoneusers", "x"), false},
		{"400", kubeStatus(400), false},
		{"401", keystoneStatus(401), false},
		{"connection refused", connError(syscall.ECONNREFUSED), true},
		{"connection reset", connError(syscall.ECONNRESET), true},
		{"unexpected EOF", &url.Error{Op: "Get", URL: "https://k8s.invalid/apis", Err: io.ErrUnexpectedEOF}, true},
		{"wrapped unexpected EOF", fmt.Errorf("reading: %w", io.ErrUnexpectedEOF), true},
		{"EOF", &url.Error{Op: "Get", URL: "https://k8s.invalid/apis", Err: io.EOF}, true},
		{"http2 GOAWAY", &url.Error{Op: "Get", URL: "https://k8s.invalid/apis",
			Err: fmt.Errorf("reading the answer: %w", errors.New("http2: server sent GOAWAY and closed the connection"))}, true},
		{"kubernetes 501", kubeStatus(501), false},
		{"keystone 505", keystoneStatus(505), false},
		{"timeout", context.DeadlineExceeded, false},
		{"canceled", context.Canceled, false},
		{"other", errors.New("boom"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
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
		{"kubernetes 404", notFound("keystoneusers", "x"), true},
		{"wrapped kubernetes 404", fmt.Errorf("reading: %w", notFound("secrets", "x")), true},
		{"keystone 404", keystoneStatus(404), true},
		{"keystone resource not found", gophercloud.ErrResourceNotFound{Name: "x", ResourceType: "credential"}, true},
		{"kubernetes 403", forbidden("keystoneusers", "x"), false},
		{"keystone 401", keystoneStatus(401), false},
		{"other", errors.New("not found"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNotFound(tc.err); got != tc.want {
				t.Errorf("IsNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestTimedRecordsOneSampleAndOperation confirms timed records exactly one
// sample whose type is the kind and one live operation under the kind and op,
// with the error classified in both, and that a client without telemetry
// still records the sample.
func TestTimedRecordsOneSampleAndOperation(t *testing.T) {
	c, _, m, reader := telemetryTestClient(t)

	err := c.timed(context.Background(), KindUser, "get", func(context.Context) error { return internalError() })
	if !apierrors.IsInternalError(err) {
		t.Fatalf("timed returned %v, want fn's 500 unchanged", err)
	}
	agg := m.Aggregate(time.Second)
	if agg.Overall.Attempted != 1 {
		t.Errorf("samples = %d, want 1", agg.Overall.Attempted)
	}
	if st := statsOf(agg, KindUser); st.Attempted != 1 || st.Failed != 1 {
		t.Errorf("keystone_user samples = %+v, want 1 attempted and 1 failed", st)
	}
	if len(agg.Errors) != 1 || agg.Errors[0] != (metrics.ErrorCount{Kind: "http_500", Count: 1}) {
		t.Errorf("errors = %+v, want one http_500", agg.Errors)
	}
	if got := countOf(metricPoints(t, reader, "dizzy.operation.duration"),
		map[string]string{"kind": "keystone_user", "operation": "get", "outcome": "error"}); got != 1 {
		t.Errorf("keystone_user/get error operations = %d, want 1", got)
	}
	if got := countOf(metricPoints(t, reader, "dizzy.operation.errors"),
		map[string]string{"kind": "keystone_user", "operation": "get", "error.kind": "http_500"}); got != 1 {
		t.Errorf("keystone_user/get http_500 errors = %d, want 1", got)
	}

	plain, _, pm := testClient()
	if err := plain.timed(context.Background(), KindSecret, "get", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("timed without telemetry: %v", err)
	}
	if st := statsOf(pm.Aggregate(time.Second), KindSecret); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("secret samples without telemetry = %+v, want 1 succeeded", st)
	}
}

// TestPrecheck confirms Precheck lists each of the five resources once in the
// client's namespace with a limit of 1, each as one list sample of its kind.
func TestPrecheck(t *testing.T) {
	c, dyn, m := testClient()
	if err := c.Precheck(context.Background()); err != nil {
		t.Fatalf("Precheck: %v", err)
	}
	var listed []string
	for _, a := range dyn.Actions() {
		list, ok := a.(k8stesting.ListActionImpl)
		if !ok {
			t.Fatalf("Precheck sent %s %s, want only lists", a.GetVerb(), a.GetResource().Resource)
		}
		if list.GetNamespace() != testNamespace || list.ListOptions.Limit != 1 {
			t.Errorf("list of %s in %q with limit %d, want %q with limit 1",
				list.GetResource().Resource, list.GetNamespace(), list.ListOptions.Limit, testNamespace)
		}
		if list.GetResource().Group != "c5c3.io" || list.GetResource().Version != "v1alpha1" {
			t.Errorf("list of %v, want group c5c3.io version v1alpha1", list.GetResource())
		}
		listed = append(listed, list.GetResource().Resource)
	}
	want := []string{"keystoneusers", "keystoneprojects", "keystoneroleassignments", "keystonecatalogentries", "keystoneapplicationcredentials"}
	if !slices.Equal(listed, want) {
		t.Errorf("listed %v, want %v", listed, want)
	}
	agg := m.Aggregate(time.Second)
	for _, kind := range []resource.Kind{KindUser, KindProject, KindRoleAssignment, KindCatalogEntry, KindApplicationCredential} {
		if st := statsOf(agg, kind); st.Attempted != 1 || st.Succeeded != 1 {
			t.Errorf("%s samples = %+v, want 1 succeeded", kind, st)
		}
	}
}

// bareStatus builds a Kubernetes API error of code and reason whose message
// names neither a resource nor a namespace, so a test that finds them in an
// error's text finds what the client wrote.
func bareStatus(code int32, reason metav1.StatusReason) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: code, Reason: reason, Message: "denied"}}
}

// TestPrecheckKindNotServed confirms a 404 to the keystoneusers list, the
// answer of a cluster without the operator's CRDs, returns ErrKindNotServed
// naming the resource, wraps the 404, and stops the check.
func TestPrecheckKindNotServed(t *testing.T) {
	c, dyn, m := testClient()
	failVerb(dyn, "list", "keystoneusers", bareStatus(404, metav1.StatusReasonNotFound))

	err := c.Precheck(context.Background())
	if !errors.Is(err, ErrKindNotServed) {
		t.Fatalf("Precheck = %v, want ErrKindNotServed", err)
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("Precheck error %v does not wrap the 404", err)
	}
	if !strings.Contains(err.Error(), "keystoneusers.c5c3.io") {
		t.Errorf("Precheck error %q does not name keystoneusers.c5c3.io", err)
	}
	if got := len(dyn.Actions()); got != 1 {
		t.Errorf("requests = %d, want 1: the check stops at the first error", got)
	}
	if got := errorCount(m.Aggregate(time.Second), "http_404"); got != 1 {
		t.Errorf("http_404 samples = %d, want 1", got)
	}
}

// TestPrecheckForbidden confirms a 403 to a list returns ErrForbidden naming
// the resource and the namespace, and wraps the 403.
func TestPrecheckForbidden(t *testing.T) {
	c, dyn, _ := testClient()
	failVerb(dyn, "list", "keystoneprojects", bareStatus(403, metav1.StatusReasonForbidden))

	err := c.Precheck(context.Background())
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("Precheck = %v, want ErrForbidden", err)
	}
	if !apierrors.IsForbidden(err) {
		t.Errorf("Precheck error %v does not wrap the 403", err)
	}
	for _, want := range []string{"keystoneprojects.c5c3.io", `"dizzy-orders"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Precheck error %q does not name %s", err, want)
		}
	}
	if errors.Is(err, ErrKindNotServed) {
		t.Errorf("Precheck error %v is also ErrKindNotServed", err)
	}
}

// TestPrecheckOtherError confirms any other list failure is returned wrapped,
// as neither sentinel.
func TestPrecheckOtherError(t *testing.T) {
	c, dyn, _ := testClient()
	failVerb(dyn, "list", "keystonecatalogentries", internalError())

	err := c.Precheck(context.Background())
	if !apierrors.IsInternalError(err) || errors.Is(err, ErrKindNotServed) || errors.Is(err, ErrForbidden) {
		t.Fatalf("Precheck = %v, want the wrapped 500 and no sentinel", err)
	}
	if !strings.Contains(err.Error(), "keystonecatalogentries.c5c3.io") {
		t.Errorf("Precheck error %q does not name the resource", err)
	}
}

// kubeServer starts an httptest API server that answers every request with
// handler, closed when the test ends, and returns a Client for testRun in
// testNamespace that reaches it through a real dynamic client, which honors a
// request's context where the fake does not. Client-side rate limiting is off,
// so a request's duration is the server's.
func kubeServer(t *testing.T, handler http.HandlerFunc) (*Client, *metrics.Collector) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	dyn, err := dynamic.NewForConfig(&rest.Config{Host: srv.URL, QPS: -1})
	if err != nil {
		t.Fatalf("dynamic.NewForConfig: %v", err)
	}
	m := metrics.NewCollector()
	return New(dyn, testNamespace, testRun, m), m
}

// writeKube answers a Kubernetes API request with status and the JSON body.
func writeKube(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// emptyList is the answer to a list of any order kind that finds nothing.
const emptyList = `{"apiVersion":"c5c3.io/v1alpha1","kind":"List","metadata":{"resourceVersion":"1"},"items":[]}`

// wedge blocks r until its client gives up on it or release is closed.
func wedge(r *http.Request, release <-chan struct{}) {
	select {
	case <-r.Context().Done():
	case <-release:
	}
}

// TestPrecheckRequestTimeout confirms SetRequestTimeout bounds each of
// Precheck's lists on its own rather than the check as a whole: five lists of
// about 100ms each outlast the 300ms timeout together and still pass, while
// one list that outlives the timeout fails the check with the deadline error
// after about the timeout, though the caller's context allows ten seconds.
func TestPrecheckRequestTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	t.Run("each list within the timeout", func(t *testing.T) {
		c, _ := kubeServer(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			writeKube(w, http.StatusOK, emptyList)
		})
		c.SetRequestTimeout(timeout)

		start := time.Now()
		if err := c.Precheck(context.Background()); err != nil {
			t.Fatalf("Precheck: %v", err)
		}
		// The lists together outlast the timeout, so a single bound over the
		// whole check would have failed it.
		if elapsed := time.Since(start); elapsed <= timeout {
			t.Errorf("Precheck took %v, want longer than the %v request timeout", elapsed, timeout)
		}
	})
	t.Run("one list over the timeout", func(t *testing.T) {
		release := make(chan struct{})
		c, m := kubeServer(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/keystoneprojects") {
				wedge(r, release)
				return
			}
			writeKube(w, http.StatusOK, emptyList)
		})
		t.Cleanup(func() { close(release) })
		c.SetRequestTimeout(timeout)

		start := time.Now()
		err := c.Precheck(shortWait(t, 10*time.Second))
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Precheck = %v, want an error wrapping context.DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), "keystoneprojects.c5c3.io") {
			t.Errorf("Precheck error %q does not name the resource", err)
		}
		if elapsed > timeout+2*time.Second {
			t.Errorf("Precheck took %v, want about the %v request timeout", elapsed, timeout)
		}
		if got := errorCount(m.Aggregate(time.Second), "timeout"); got != 1 {
			t.Errorf("timeout samples = %d, want 1", got)
		}
	})
}

// keys returns the sorted attribute keys of every point.
func keys(points []point) []string {
	set := map[string]bool{}
	for _, p := range points {
		for k := range p.attrs {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// values returns the sorted values attribute key takes across points.
func values(points []point, key string) []string {
	set := map[string]bool{}
	for _, p := range points {
		if v, ok := p.attrs[key]; ok {
			set[v] = true
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// TestClientTelemetryLabels drives every recorded call of the order and
// Secret client through one telemetry and confirms the series carry the five
// order kinds and secret, the operations get, create, delete, update, list,
// finalize, rotate, repair and refuse, and no attribute key beyond kind,
// operation, outcome and error.kind.
func TestClientTelemetryLabels(t *testing.T) {
	c, dyn, _, reader := telemetryTestClient(t)
	ctx := context.Background()

	created := map[string]resource.Resource{}
	for _, o := range testPlan().Orders {
		r, err := c.CreateOrder(ctx, o)
		if err != nil {
			t.Fatalf("CreateOrder(%s): %v", o.Name, err)
		}
		created[o.Name] = r
	}
	user, project, role, catalog, appcred := created["user"], created["project"], created["role"], created["catalog"], created["appcred"]

	// The project turns ready.
	replace(t, dyn, KindProject, readyObject(project, map[string]any{"projectName": "p", "domainName": "Default"}))
	if err := c.WaitReady(ctx, project); err != nil {
		t.Fatalf("WaitReady(project): %v", err)
	}

	// The user rotates from generation 1 to 2.
	replace(t, dyn, KindUser, readyObject(user, map[string]any{"passwordGeneration": int64(1)}))
	seed(t, dyn, KindSecret, userSecret("pw-one", "1"))
	before, err := c.ReadCredential(ctx, user)
	if err != nil {
		t.Fatalf("ReadCredential(user): %v", err)
	}
	gen, err := c.RaiseGeneration(ctx, user)
	if err != nil {
		t.Fatalf("RaiseGeneration(user): %v", err)
	}
	replace(t, dyn, KindUser, readyObject(user, map[string]any{"passwordGeneration": gen}))
	replace(t, dyn, KindSecret, userSecret("pw-two", "2"))
	after, err := c.WaitRotated(ctx, user, gen, before)
	if err != nil {
		t.Fatalf("WaitRotated(user): %v", err)
	}

	// The user's Secret is edited and restored, then deleted.
	if err := c.EditSecret(ctx, user); err != nil {
		t.Fatalf("EditSecret(user): %v", err)
	}
	replace(t, dyn, KindSecret, userSecret("pw-two", "3"))
	if err := c.WaitSecretRestored(ctx, user, after); err != nil {
		t.Fatalf("WaitSecretRestored(user): %v", err)
	}
	if err := c.DeleteSecret(ctx, user); err != nil {
		t.Fatalf("DeleteSecret(user): %v", err)
	}
	if _, err := c.ReadCredential(ctx, appcred); !errors.Is(err, ErrMismatch) {
		t.Fatalf("ReadCredential(appcred) without a secret = %v, want ErrMismatch", err)
	}

	// The credential order is refused, the role assignment listed, the
	// catalog entry deleted.
	replace(t, dyn, KindApplicationCredential, pendingObject(appcred, "NoRoleOnProject"))
	if err := c.WaitReason(ctx, appcred, "NoRoleOnProject"); err != nil {
		t.Fatalf("WaitReason(appcred): %v", err)
	}
	if found, err := c.ListByRun(ctx, KindRoleAssignment, testRun); err != nil || len(found) != 1 || found[0].ID != role.ID {
		t.Fatalf("ListByRun(role) = %v, %v, want the role assignment", found, err)
	}
	if err := c.Delete(ctx, catalog); err != nil {
		t.Fatalf("Delete(catalog): %v", err)
	}
	if err := c.WaitGone(ctx, catalog); err != nil {
		t.Fatalf("WaitGone(catalog): %v", err)
	}

	ops := metricPoints(t, reader, "dizzy.operation.duration")
	errs := metricPoints(t, reader, "dizzy.operation.errors")
	ready := metricPoints(t, reader, "dizzy.resource.time_to_ready")
	all := slices.Concat(ops, errs)

	wantKinds := []string{"keystone_application_credential", "keystone_catalog_entry", "keystone_project",
		"keystone_role_assignment", "keystone_user", "secret"}
	if got := values(all, "kind"); !slices.Equal(got, wantKinds) {
		t.Errorf("kinds = %v, want %v", got, wantKinds)
	}
	wantOps := []string{"create", "delete", "finalize", "get", "list", "refuse", "repair", "rotate", "update"}
	if got := values(all, "operation"); !slices.Equal(got, wantOps) {
		t.Errorf("operations = %v, want %v", got, wantOps)
	}
	if got, want := keys(ops), []string{"kind", "operation", "outcome"}; !slices.Equal(got, want) {
		t.Errorf("operation.duration keys = %v, want %v", got, want)
	}
	if got, want := keys(errs), []string{"error.kind", "kind", "operation"}; !slices.Equal(got, want) {
		t.Errorf("operation.errors keys = %v, want %v", got, want)
	}
	if got, want := keys(ready), []string{"kind", "outcome"}; !slices.Equal(got, want) {
		t.Errorf("time_to_ready keys = %v, want %v", got, want)
	}
	if got := countOf(errs, map[string]string{"kind": "secret", "operation": "get", "error.kind": "mismatch"}); got != 1 {
		t.Errorf("secret/get mismatch errors = %d, want 1", got)
	}
	if got := countOf(ready, map[string]string{"kind": "keystone_project", "outcome": "success"}); got != 1 {
		t.Errorf("keystone_project time to ready = %d, want 1 success", got)
	}
}

// TestClientTelemetryServiceAttribute confirms service="c5c3" reaches the
// series the client records, set the way every monitor sets it: as
// telemetry.Config.Service, exported as a resource label by the Prometheus
// reader.
func TestClientTelemetryServiceAttribute(t *testing.T) {
	reg := prometheus.NewRegistry()
	tel, err := telemetry.Setup(context.Background(), telemetry.Config{Scenario: "small", Service: "c5c3", Registerer: reg})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	c, _, _ := testClient()
	c.SetTelemetry(tel)

	if _, err := c.CreateOrder(context.Background(), testPlan().Orders[0]); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var series int
	for _, mf := range mfs {
		if mf.GetName() != "dizzy_operation_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			series++
			if labels["service"] != "c5c3" || labels["kind"] != "keystone_user" || labels["operation"] != "create" {
				t.Errorf("operation duration labels = %v, want service=c5c3 kind=keystone_user operation=create", labels)
			}
		}
	}
	if series != 1 {
		t.Errorf("operation duration series = %d, want 1", series)
	}
}

// userSecret returns the Secret delivered beside the user order, carrying
// password in its document and its password key, at resourceVersion rv.
func userSecret(password, rv string) *unstructured.Unstructured {
	return secretObject(secretName(resourceName(testRun, "user")), rv, map[string]string{
		keyCloudsYAML: userDocument(password),
		keyPassword:   password,
	})
}

// userDocument is a delivered user clouds.yaml with password, in the shape the
// upstream order guide shows, its quoted entry name and endpoint_type
// included.
func userDocument(password string) string {
	return `clouds:
  "admin":
    auth:
      auth_url: "http://controlplane-keystone.openstack.svc:5000/v3"
      username: "dizzyorder-run0-user"
      password: "` + password + `"
      user_domain_name: "Default"
    region_name: "RegionOne"
    endpoint_type: internal
    identity_api_version: 3
`
}

// secretObject returns a Secret named name in testNamespace at
// resourceVersion rv whose data holds the base64 of each value.
func secretObject(name, rv string, data map[string]string) *unstructured.Unstructured {
	encoded := make(map[string]any, len(data))
	for k, v := range data {
		encoded[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       testNamespace,
			"resourceVersion": rv,
		},
		"type": "Opaque",
		"data": encoded,
	}}
}
