package nova

import (
	"context"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// telemetryComputeClient builds a Client whose compute and network calls hit ts
// and record into a fresh Collector, with a ManualReader-backed telemetry
// attached, so a test can assert both the in-memory sample and the live
// operation series of one call.
func telemetryComputeClient(t *testing.T, ts *httptest.Server) (*Client, *metrics.Collector, *sdkmetric.ManualReader) {
	t.Helper()
	m := metrics.NewCollector()
	reader := sdkmetric.NewManualReader()
	tel, err := telemetry.NewWithProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatalf("NewWithProvider: %v", err)
	}
	c := testComputeNetworkClient(ts, m)
	c.SetTelemetry(tel)
	return c, m, reader
}

// metricPoints collects reader once and returns the string attributes of every
// data point of the named float64 histogram or int64 sum, and an empty slice
// when the instrument recorded nothing. An instrument of the name with another
// data type fails the test.
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
