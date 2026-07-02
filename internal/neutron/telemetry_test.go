package neutron

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/B42Labs/openstack-tester/internal/telemetry"
)

// telemetryClient builds a Client whose calls hit ts with an in-memory
// ManualReader-backed telemetry attached, so a test can assert the metrics a
// timed call or readiness poll emits live.
func telemetryClient(t *testing.T, ts *httptest.Server) (*Client, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tel, err := telemetry.NewWithProvider(mp)
	if err != nil {
		t.Fatalf("NewWithProvider: %v", err)
	}
	c := testServiceClient(ts)
	c.SetTelemetry(tel)
	return c, reader
}

// hasHistoPoint reports whether the named histogram has a data point whose
// attributes exactly match want.
func hasHistoPoint(t *testing.T, reader *sdkmetric.ManualReader, name string, want map[string]string) bool {
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
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is not a float64 histogram: %T", name, m.Data)
			}
			for _, dp := range h.DataPoints {
				if setMatches(dp.Attributes, want) {
					return true
				}
			}
		}
	}
	return false
}

// hasSumPoint reports whether the named int64 counter has a data point whose
// attributes exactly match want. It is the metricdata.Sum twin of hasHistoPoint.
func hasSumPoint(t *testing.T, reader *sdkmetric.ManualReader, name string, want map[string]string) bool {
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
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is not an int64 sum: %T", name, m.Data)
			}
			for _, dp := range s.DataPoints {
				if setMatches(dp.Attributes, want) {
					return true
				}
			}
		}
	}
	return false
}

// hasMetric reports whether a metric of the given name was emitted at all.
func hasMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}

// setMatches reports whether set has exactly the wanted string attributes.
func setMatches(set attribute.Set, want map[string]string) bool {
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

// TestTimedRecordsOperationTelemetry confirms the timing seam mirrors each call
// into operation.duration with the low-cardinality kind/operation/outcome
// labels, covering both the create closure and the follow-up tag PUT.
func TestTimedRecordsOperationTelemetry(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/networks/net-id-1/tags" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"tags":["ostester:run=run0","ostester:type=network"]}`)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	c, reader := telemetryClient(t, ts)
	// The create closure returns an id without an HTTP call, so only the tag PUT
	// travels over the wire; both are timed and mirrored into telemetry.
	if _, err := c.createTagged(context.Background(), KindNetwork, "net-0001",
		func(ctx context.Context, name string) (string, error) {
			return "net-id-1", nil
		}); err != nil {
		t.Fatalf("createTagged: %v", err)
	}

	if !hasHistoPoint(t, reader, "openstack_tester.operation.duration",
		map[string]string{"kind": "network", "operation": "create", "outcome": "success"}) {
		t.Error("create was not recorded as {kind=network, operation=create, outcome=success}")
	}
	if !hasHistoPoint(t, reader, "openstack_tester.operation.duration",
		map[string]string{"kind": "network", "operation": "tag", "outcome": "success"}) {
		t.Error("tag was not recorded as {kind=network, operation=tag, outcome=success}")
	}
	// An all-success flow must not create the operation.errors series at all.
	if hasMetric(t, reader, "openstack_tester.operation.errors") {
		t.Error("an all-success flow created an openstack_tester.operation.errors series")
	}
}

// TestTimedRecordsErrorOutcome confirms a failed call maps to the error outcome
// (an HTTP 500 from the tag PUT, retried and then rolled back).
func TestTimedRecordsErrorOutcome(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/networks/net-id-1/tags":
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	c, reader := telemetryClient(t, ts)
	if _, err := c.createTagged(context.Background(), KindNetwork, "net-0001",
		func(ctx context.Context, name string) (string, error) {
			return "net-id-1", nil
		}); err == nil {
		t.Fatal("expected an error when tagging keeps failing")
	}

	if !hasHistoPoint(t, reader, "openstack_tester.operation.duration",
		map[string]string{"kind": "network", "operation": "tag", "outcome": "error"}) {
		t.Error("a failing tag PUT was not recorded with outcome=error")
	}
	// The same failure reaches operation.errors with the exact error.kind the
	// classifier derives from the gophercloud 500 — parity with the report's
	// Errors table, end to end.
	if !hasSumPoint(t, reader, "openstack_tester.operation.errors",
		map[string]string{"kind": "network", "operation": "tag", "error.kind": "http_500"}) {
		t.Error("a failing tag PUT was not counted as {kind=network, operation=tag, error.kind=http_500}")
	}
}

// TestWaitForReadyRecordsTimeToReady confirms the readiness poll mirrors its
// time-to-ready measurement into telemetry: success when the resource reaches
// its status, timeout when the context ends first.
func TestWaitForReadyRecordsTimeToReady(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/networks/n1" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"network":{"id":"n1","status":"ACTIVE"}}`)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	t.Run("success", func(t *testing.T) {
		c, reader := telemetryClient(t, ts)
		if err := c.WaitForReady(context.Background(), Resource{Kind: KindNetwork, ID: "n1"}); err != nil {
			t.Fatalf("WaitForReady: %v", err)
		}
		if !hasHistoPoint(t, reader, "openstack_tester.resource.time_to_ready",
			map[string]string{"kind": "network", "outcome": "success"}) {
			t.Error("a resource reaching ACTIVE was not recorded as time_to_ready outcome=success")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		c, reader := telemetryClient(t, ts)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already cancelled: the readiness deadline is effectively past
		if err := c.WaitForReady(ctx, Resource{Kind: KindNetwork, ID: "n1"}); err == nil {
			t.Fatal("expected WaitForReady to return the context error")
		}
		if !hasHistoPoint(t, reader, "openstack_tester.resource.time_to_ready",
			map[string]string{"kind": "network", "outcome": "timeout"}) {
			t.Error("a cancelled readiness wait was not recorded as time_to_ready outcome=timeout")
		}
	})
}
