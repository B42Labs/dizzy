// Package telemetry is the OpenTelemetry (OTLP push) metrics seam for the
// tester. It wraps the OTEL Go SDK behind a small set of nil-safe recording
// methods so a run with export disabled constructs nothing and behaves exactly
// as before: a nil *Telemetry is the no-op, and every method returns early on
// it. When enabled via Setup it builds an OTLP exporter configured entirely
// from the standard OTEL_EXPORTER_OTLP_* environment variables and records the
// documented per-operation, time-to-ready, and per-iteration instruments —
// including the per-error-kind operation.errors counter — alongside the
// in-memory metrics.Collector, which stays the source of truth for run records
// and reports.
//
// Cardinality rule: metric attributes carry only bounded, low-cardinality
// labels (kind, operation, outcome, result, error.kind). error.kind is the
// neutron client's classification of a failure (quota, timeout, canceled,
// other, or http_<status> for a fixed set of status codes, with any other code
// collapsed to http_other), so it stays bounded. A run id, resource id, or
// resource name must never become a metric attribute; those live in the run
// records and logs.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// meterName scopes the tester's instruments in the OTEL meter registry.
const meterName = "github.com/B42Labs/openstack-tester"

// Config carries the enablement flag and the two identifying attributes that
// distinguish one installation across time in the backend. Cloud is the
// --os-cloud name and Scenario is the plan's scenario name; both are omitted
// from the resource when empty.
type Config struct {
	Enabled  bool
	Cloud    string
	Scenario string
}

// Telemetry holds the constructed instruments and the provider shutdown hook.
// A nil *Telemetry is the disabled no-op: every method is safe to call on it.
type Telemetry struct {
	shutdown func(context.Context) error

	operationDuration metric.Float64Histogram
	operationErrors   metric.Int64Counter
	timeToReady       metric.Float64Histogram
	iterationDuration metric.Float64Histogram
	iterationOps      metric.Int64Counter
	iterations        metric.Int64Counter
}

// Setup returns the OTEL export seam. It returns (nil, nil) when export is
// disabled, so callers thread a nil *Telemetry through the disabled path and
// every recording method becomes a no-op. When enabled it builds an OTLP
// exporter from the standard OTEL_EXPORTER_OTLP_* environment variables, wraps
// it in a periodic reader (which honors OTEL_METRIC_EXPORT_INTERVAL/_TIMEOUT),
// and constructs the instruments. Export errors from a down collector are
// routed to a slog.Warn error handler so they degrade to warnings and never
// break a run.
func Setup(ctx context.Context, cfg Config) (*Telemetry, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	exp, err := newExporter(ctx)
	if err != nil {
		return nil, err
	}

	attrs := []attribute.KeyValue{
		semconv.ServiceName("openstack-tester"),
		semconv.ServiceVersion(buildVersion()),
	}
	if cfg.Cloud != "" {
		attrs = append(attrs, attribute.String("cloud", cfg.Cloud))
	}
	if cfg.Scenario != "" {
		attrs = append(attrs, attribute.String("scenario", cfg.Scenario))
	}
	res, err := resource.Merge(resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, attrs...))
	if err != nil {
		return nil, fmt.Errorf("building telemetry resource: %w", err)
	}

	// A down or misconfigured collector must degrade to warnings, not crash the
	// run: batched exports fail asynchronously and surface through this handler.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("opentelemetry metric export error", "error", err)
	}))

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
	)
	t, err := NewWithProvider(mp)
	if err != nil {
		return nil, err
	}
	t.shutdown = mp.Shutdown
	return t, nil
}

// NewWithProvider constructs the tester's instruments against mp. Setup uses it
// with the OTLP-backed provider; tests use it with an in-memory ManualReader
// provider to assert the emitted metric schema without a collector. All six
// instruments are built once here so recording on the hot path is a cheap
// method call.
func NewWithProvider(mp metric.MeterProvider) (*Telemetry, error) {
	// Boundaries for per-call and time-to-ready latencies, in seconds: dense
	// below a second where most Neutron calls land, sparse into the minutes for
	// a struggling control plane.
	callBoundaries := metric.WithExplicitBucketBoundaries(
		0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300)
	// Boundaries for a whole iteration's wall-clock, in seconds: an apply+cleanup
	// cycle spans seconds to hours over a multi-day loop.
	iterBoundaries := metric.WithExplicitBucketBoundaries(
		1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200)

	m := mp.Meter(meterName)
	var err error
	t := &Telemetry{}

	if t.operationDuration, err = m.Float64Histogram("openstack_tester.operation.duration",
		metric.WithDescription("Duration of a single Neutron API operation."),
		metric.WithUnit("s"), callBoundaries); err != nil {
		return nil, fmt.Errorf("creating operation.duration instrument: %w", err)
	}
	if t.operationErrors, err = m.Int64Counter("openstack_tester.operation.errors",
		metric.WithDescription("Failed Neutron API operations by error kind.")); err != nil {
		return nil, fmt.Errorf("creating operation.errors instrument: %w", err)
	}
	if t.timeToReady, err = m.Float64Histogram("openstack_tester.resource.time_to_ready",
		metric.WithDescription("Time from create returning to a resource reaching its expected status."),
		metric.WithUnit("s"), callBoundaries); err != nil {
		return nil, fmt.Errorf("creating resource.time_to_ready instrument: %w", err)
	}
	if t.iterationDuration, err = m.Float64Histogram("openstack_tester.iteration.duration",
		metric.WithDescription("Wall-clock duration of one monitor iteration or one-shot run."),
		metric.WithUnit("s"), iterBoundaries); err != nil {
		return nil, fmt.Errorf("creating iteration.duration instrument: %w", err)
	}
	if t.iterationOps, err = m.Int64Counter("openstack_tester.iteration.operations",
		metric.WithDescription("Neutron operations attempted, succeeded, and failed within an iteration.")); err != nil {
		return nil, fmt.Errorf("creating iteration.operations instrument: %w", err)
	}
	if t.iterations, err = m.Int64Counter("openstack_tester.iterations",
		metric.WithDescription("Completed iterations by outcome.")); err != nil {
		return nil, fmt.Errorf("creating iterations instrument: %w", err)
	}
	return t, nil
}

// newExporter builds the OTLP metric exporter for the configured protocol. The
// protocol comes from OTEL_EXPORTER_OTLP_METRICS_PROTOCOL, falling back to
// OTEL_EXPORTER_OTLP_PROTOCOL and finally the OTLP spec default http/protobuf.
// Endpoint, headers, TLS, and timeout are read by the exporters themselves from
// the standard env vars, so there is no custom config surface.
func newExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	proto := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if proto == "" {
		proto = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if proto == "" {
		proto = "http/protobuf"
	}
	switch proto {
	case "grpc":
		return otlpmetricgrpc.New(ctx)
	case "http/protobuf":
		return otlpmetrichttp.New(ctx)
	default:
		return nil, fmt.Errorf("unsupported OTLP protocol %q: want grpc or http/protobuf", proto)
	}
}

// RecordOperation records one Neutron API call's duration under kind and op with
// the outcome derived from errKind (as the neutron client classifies it): an
// empty errKind is success, "timeout" is timeout, anything else is error. A
// failed call additionally increments operation.errors with the verbatim
// errKind as error.kind, preserving the per-error-kind breakdown that outcome
// collapses; a successful call creates no operation.errors series.
func (t *Telemetry) RecordOperation(ctx context.Context, kind, op string, d time.Duration, errKind string) {
	if t == nil {
		return
	}
	t.operationDuration.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String("kind", kind),
		attribute.String("operation", op),
		attribute.String("outcome", operationOutcome(errKind)),
	))
	if errKind != "" {
		t.operationErrors.Add(ctx, 1, metric.WithAttributes(
			attribute.String("kind", kind),
			attribute.String("operation", op),
			attribute.String("error.kind", errKind),
		))
	}
}

// RecordTimeToReady records how long a status-bearing resource of kind took to
// reach its expected status. ok is false when the readiness deadline elapsed
// first, recorded as the timeout outcome.
func (t *Telemetry) RecordTimeToReady(ctx context.Context, kind string, d time.Duration, ok bool) {
	if t == nil {
		return
	}
	t.timeToReady.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String("kind", kind),
		attribute.String("outcome", readyOutcome(ok)),
	))
}

// RecordIteration records one iteration's wall-clock duration and increments the
// iterations counter, both tagged with the success/failure outcome.
func (t *Telemetry) RecordIteration(ctx context.Context, d time.Duration, ok bool) {
	if t == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("outcome", iterationOutcome(ok)))
	t.iterationDuration.Record(ctx, d.Seconds(), attrs)
	t.iterations.Add(ctx, 1, attrs)
}

// RecordIterationOperations records the attempted/succeeded/failed operation
// counts of one iteration, taken from the already-computed metrics.Aggregate.
func (t *Telemetry) RecordIterationOperations(ctx context.Context, attempted, succeeded, failed int) {
	if t == nil {
		return
	}
	t.iterationOps.Add(ctx, int64(attempted), metric.WithAttributes(attribute.String("result", "attempted")))
	t.iterationOps.Add(ctx, int64(succeeded), metric.WithAttributes(attribute.String("result", "succeeded")))
	t.iterationOps.Add(ctx, int64(failed), metric.WithAttributes(attribute.String("result", "failed")))
}

// Shutdown flushes any buffered metrics and stops the provider. It is nil-safe,
// so the disabled path and tests using NewWithProvider can defer it freely.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil || t.shutdown == nil {
		return nil
	}
	return t.shutdown(ctx)
}

// operationOutcome maps a neutron errKind to the operation.duration outcome
// attribute: success for no error, timeout for a deadline, error otherwise
// (quota, canceled, http_5xx, and the rest collapse to error).
func operationOutcome(errKind string) string {
	switch errKind {
	case "":
		return "success"
	case "timeout":
		return "timeout"
	default:
		return "error"
	}
}

// readyOutcome maps a readiness result to the time_to_ready outcome attribute.
func readyOutcome(ok bool) string {
	if ok {
		return "success"
	}
	return "timeout"
}

// iterationOutcome maps an iteration result to the iteration outcome attribute.
func iterationOutcome(ok bool) string {
	if ok {
		return "success"
	}
	return "failure"
}

// buildVersion resolves service.version from the build info, falling back to
// "unknown" when the binary carries none (e.g. a go run without VCS stamping).
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}
