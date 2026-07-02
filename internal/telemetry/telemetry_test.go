package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestTelemetry builds a Telemetry backed by an in-memory ManualReader so a
// test can record and then read back the emitted metrics without a collector.
func newTestTelemetry(t *testing.T) (*Telemetry, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tel, err := NewWithProvider(mp)
	if err != nil {
		t.Fatalf("NewWithProvider: %v", err)
	}
	return tel, reader
}

// collectByName gathers the tester's metrics into a name-keyed map.
func collectByName(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// attrsMatch reports whether set has exactly the wanted string attributes.
func attrsMatch(set attribute.Set, want map[string]string) bool {
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

// histoCount returns the recorded count of the histogram data point whose
// attributes match want, and whether such a point exists.
func histoCount(t *testing.T, m metricdata.Metrics, want map[string]string) (uint64, bool) {
	t.Helper()
	h, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %s is not a float64 histogram: %T", m.Name, m.Data)
	}
	for _, dp := range h.DataPoints {
		if attrsMatch(dp.Attributes, want) {
			return dp.Count, true
		}
	}
	return 0, false
}

// sumValue returns the value of the counter data point whose attributes match
// want, and whether such a point exists.
func sumValue(t *testing.T, m metricdata.Metrics, want map[string]string) (int64, bool) {
	t.Helper()
	s, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %s is not an int64 sum: %T", m.Name, m.Data)
	}
	for _, dp := range s.DataPoints {
		if attrsMatch(dp.Attributes, want) {
			return dp.Value, true
		}
	}
	return 0, false
}

func TestInstrumentsMatchDocumentedSchema(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()

	tel.RecordOperation(ctx, "network", "create", 100*time.Millisecond, "")
	// A failing operation drives the operation.errors counter (a success alone
	// creates no series on it).
	tel.RecordOperation(ctx, "port", "delete", 50*time.Millisecond, "quota")
	tel.RecordTimeToReady(ctx, "network", 200*time.Millisecond, true)
	tel.RecordIteration(ctx, 5*time.Second, true)
	tel.RecordIterationOperations(ctx, 10, 8, 2)

	metrics := collectByName(t, reader)

	// The six documented instruments must all be present, with the seconds unit
	// on the three histograms.
	histograms := []string{
		"openstack_tester.operation.duration",
		"openstack_tester.resource.time_to_ready",
		"openstack_tester.iteration.duration",
	}
	for _, name := range histograms {
		m, ok := metrics[name]
		if !ok {
			t.Fatalf("histogram %s not emitted", name)
		}
		if m.Unit != "s" {
			t.Errorf("%s unit = %q, want %q", name, m.Unit, "s")
		}
	}
	for _, name := range []string{
		"openstack_tester.operation.errors",
		"openstack_tester.iteration.operations",
		"openstack_tester.iterations",
	} {
		if _, ok := metrics[name]; !ok {
			t.Fatalf("counter %s not emitted", name)
		}
	}

	// operation.duration carries exactly kind/operation/outcome.
	if _, ok := histoCount(t, metrics["openstack_tester.operation.duration"],
		map[string]string{"kind": "network", "operation": "create", "outcome": "success"}); !ok {
		t.Error("operation.duration missing the documented {kind,operation,outcome} attribute set")
	}
	// operation.errors carries exactly kind/operation/error.kind, incremented once.
	if got, ok := sumValue(t, metrics["openstack_tester.operation.errors"],
		map[string]string{"kind": "port", "operation": "delete", "error.kind": "quota"}); !ok || got != 1 {
		t.Errorf("operation.errors{kind=port,operation=delete,error.kind=quota} = %d (present=%v), want 1", got, ok)
	}
	// time_to_ready carries exactly kind/outcome.
	if _, ok := histoCount(t, metrics["openstack_tester.resource.time_to_ready"],
		map[string]string{"kind": "network", "outcome": "success"}); !ok {
		t.Error("resource.time_to_ready missing the documented {kind,outcome} attribute set")
	}
}

func TestRecordOperationOutcomeMapping(t *testing.T) {
	tests := []struct {
		errKind string
		outcome string
	}{
		{"", "success"},
		{"timeout", "timeout"},
		{"http_500", "error"},
		{"canceled", "error"},
	}
	for _, tc := range tests {
		t.Run(tc.errKind+"_"+tc.outcome, func(t *testing.T) {
			tel, reader := newTestTelemetry(t)
			tel.RecordOperation(context.Background(), "port", "delete", 10*time.Millisecond, tc.errKind)

			m := collectByName(t, reader)["openstack_tester.operation.duration"]
			if _, ok := histoCount(t, m, map[string]string{
				"kind": "port", "operation": "delete", "outcome": tc.outcome,
			}); !ok {
				t.Errorf("errKind %q did not map to outcome %q", tc.errKind, tc.outcome)
			}
		})
	}
}

func TestRecordOperationErrorsCounter(t *testing.T) {
	// A successful operation must not create any operation.errors series.
	t.Run("success creates no series", func(t *testing.T) {
		tel, reader := newTestTelemetry(t)
		tel.RecordOperation(context.Background(), "network", "create", 10*time.Millisecond, "")

		if _, ok := collectByName(t, reader)["openstack_tester.operation.errors"]; ok {
			t.Error("a successful operation created an openstack_tester.operation.errors series")
		}
	})

	// A failed operation increments the counter once with the verbatim errKind as
	// error.kind, for both the classifier's static kinds and its per-status codes.
	tests := []struct {
		errKind string
	}{
		{"quota"},
		{"http_409"},
	}
	for _, tc := range tests {
		t.Run(tc.errKind, func(t *testing.T) {
			tel, reader := newTestTelemetry(t)
			tel.RecordOperation(context.Background(), "port", "delete", 10*time.Millisecond, tc.errKind)

			m := collectByName(t, reader)["openstack_tester.operation.errors"]
			if got, ok := sumValue(t, m, map[string]string{
				"kind": "port", "operation": "delete", "error.kind": tc.errKind,
			}); !ok || got != 1 {
				t.Errorf("operation.errors{error.kind=%s} = %d (present=%v), want 1", tc.errKind, got, ok)
			}
		})
	}
}

func TestRecordTimeToReadyOutcome(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	// A readiness deadline that elapsed records the timeout outcome.
	tel.RecordTimeToReady(context.Background(), "router", time.Second, false)

	m := collectByName(t, reader)["openstack_tester.resource.time_to_ready"]
	if _, ok := histoCount(t, m, map[string]string{"kind": "router", "outcome": "timeout"}); !ok {
		t.Error("ok=false did not map to the timeout outcome")
	}
}

func TestRecordIterationCountsAndDuration(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()

	tel.RecordIteration(ctx, 30*time.Second, false)
	tel.RecordIterationOperations(ctx, 20, 17, 3)

	metrics := collectByName(t, reader)

	// The duration histogram and the iterations counter share the outcome value.
	if _, ok := histoCount(t, metrics["openstack_tester.iteration.duration"],
		map[string]string{"outcome": "failure"}); !ok {
		t.Error("iteration.duration missing the failure-outcome data point")
	}
	if got, ok := sumValue(t, metrics["openstack_tester.iterations"],
		map[string]string{"outcome": "failure"}); !ok || got != 1 {
		t.Errorf("iterations{outcome=failure} = %d (present=%v), want 1", got, ok)
	}

	// iteration.operations splits by result with the supplied counts.
	ops := metrics["openstack_tester.iteration.operations"]
	for result, want := range map[string]int64{"attempted": 20, "succeeded": 17, "failed": 3} {
		if got, ok := sumValue(t, ops, map[string]string{"result": result}); !ok || got != want {
			t.Errorf("iteration.operations{result=%s} = %d (present=%v), want %d", result, got, ok, want)
		}
	}
}

func TestNilTelemetryIsNoOp(t *testing.T) {
	var tel *Telemetry // the disabled no-op
	ctx := context.Background()

	// None of these may panic or dereference the nil receiver.
	tel.RecordOperation(ctx, "network", "create", time.Second, "http_500")
	tel.RecordTimeToReady(ctx, "network", time.Second, false)
	tel.RecordIteration(ctx, time.Second, true)
	tel.RecordIterationOperations(ctx, 1, 1, 0)
	if err := tel.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown on nil telemetry = %v, want nil", err)
	}
}

func TestSetupDisabledReturnsNil(t *testing.T) {
	tel, err := Setup(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("Setup(disabled): %v", err)
	}
	if tel != nil {
		t.Errorf("Setup(disabled) = %v, want nil so the whole path is a no-op", tel)
	}
}

func TestNewExporterSelectsProtocolFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{name: "default is http/protobuf", env: nil},
		{name: "explicit grpc", env: map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}},
		{name: "explicit http/protobuf", env: map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf"}},
		{name: "metrics-specific overrides generic", env: map[string]string{
			"OTEL_EXPORTER_OTLP_PROTOCOL":         "grpc",
			"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/protobuf",
		}},
		{name: "unsupported protocol errors", env: map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "carrier-pigeon"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Clear both variables so the ambient environment cannot leak in.
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			exp, err := newExporter(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error for an unsupported protocol, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("newExporter: %v", err)
			}
			// The exporter is constructed lazily (no dial), so shutting it down
			// releases it without a collector present.
			if err := exp.Shutdown(context.Background()); err != nil {
				t.Errorf("exporter Shutdown: %v", err)
			}
		})
	}
}
