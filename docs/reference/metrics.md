# Metrics reference

Every API call is wrapped with timing instrumentation that records the resource
kind, the operation, wall-clock duration, success or failure, the HTTP status,
and a timestamp. For resources with a status field, the time from "create
returned" to "status == expected" is recorded separately as **time-to-ready**,
measured by polling with backoff.

These measurements have two sinks. The in-memory collector is the source of
truth for run records and `report`. With `--otel`, the same seam also feeds an
OpenTelemetry exporter.

## What `report` renders

Per resource kind and overall:

- counts: attempted, succeeded, failed
- latency: min, mean, median, p90, p95, p99, max
- throughput (operations per second) and effective concurrency
- total wall-clock for the run
- error breakdown by kind

A churn run adds create/delete/mutate counts, completed create→delete cycles, the
live-population summary (min, mean, max, and the controller's target fill), and
latency and error rate **bucketed over the run's duration** — so degradation over
time is visible rather than averaged away. A run that ends with teardown also
performs a **leak check**, listing any resource still carrying the run tag after
the topology should be gone.

## OpenTelemetry export

`--otel` exports via the OpenTelemetry SDK over OTLP, so any OTLP-compatible
backend can store the data: Prometheus behind an otel-collector, Mimir, InfluxDB,
VictoriaMetrics, Timescale, and so on.

Enablement is explicit. Only `--otel` turns export on; the `OTEL_EXPORTER_OTLP_*`
environment variables *configure* the exporter but never *enable* it.

### Configuration

There is no custom config surface — everything comes from the standard
environment variables.

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Full metrics endpoint URL, path included |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Base endpoint; `/v1/metrics` is appended |
| `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL` | `http/protobuf` (default, port 4318) or `grpc` (port 4317) |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | Fallback for the above |
| `OTEL_METRIC_EXPORT_INTERVAL` | Push period |

Export failures from a down collector degrade to warnings and never fail a run.

### Resource attributes

These identify one installation across time.

| Attribute | Value |
|---|---|
| `service.name` | `dizzy` (semantic convention; constant) |
| `service.version` | The build version |
| `cloud` | The `--os-cloud` name |
| `scenario` | The scenario name |
| `service` | `neutron`, `cinder`, `keystone`, or `nova` |

`service` is a bespoke attribute, deliberately distinct from the semantic
`service.name`. It keeps the iteration-level series — which carry no `kind` — 
apart when a Neutron, a Cinder, a Keystone, and a Nova monitor feed the same
backend. It mirrors the run record's `service` field.

### Instruments

| Instrument | Type | Unit | Attributes |
|---|---|---|---|
| `dizzy.operation.duration` | histogram | s | `kind`, `operation`, `outcome` |
| `dizzy.operation.errors` | counter | | `kind`, `operation`, `error.kind` |
| `dizzy.resource.time_to_ready` | histogram | s | `kind`, `outcome` |
| `dizzy.iteration.duration` | histogram | s | `outcome` |
| `dizzy.iteration.operations` | counter | | `result` |
| `dizzy.iterations` | counter | | `outcome` |

All four services share this schema; no service adds an instrument.

### Attribute values

Every value set is bounded.

| Attribute | Values |
|---|---|
| `kind` | **neutron:** `address-scope`, `subnet-pool`, `network`, `subnet`, `router`, `router-interface`, `security-group`, `security-group-rule`, `port`, `floating-ip` — **cinder:** `volume`, `snapshot` — **keystone:** `domain`, `project`, `user`, `role`, `role_assignment`, `token` — **nova:** `server`, `network`, `subnet`, `port`, `volume` |
| `operation` | `create`, `delete`, `get`, `list`, `tag`, `attach`, `detach`, `extend` (cinder resize), `update` (keystone domain disable-before-delete), `stop`, `start`, `reboot`, `resize`, `confirm-resize`, `live-migrate` (nova server lifecycle) |
| `outcome` | `success`, `error`, `timeout` for an operation; `success`, `timeout` for time-to-ready; `success`, `failure` for an iteration |
| `result` | `attempted`, `succeeded`, `failed` |
| `error.kind` | `quota`, `timeout`, `canceled`, `other`, `http_<status>` for the statuses the service returns, `http_other` for the rest |

> **Cardinality rule.** Run IDs, resource IDs, and resource names are **never**
> metric attributes. They live in the run records and the logs.

`operation.errors` breaks down what `operation.duration`'s `outcome` collapses.
It is recorded **only for failed operations**, so a healthy run emits no series
at all — its absence is the steady state, not a missing metric.

Keystone creates and deletes are synchronous, so `resource.time_to_ready` is
largely unused there. The one exception is **token-issue latency**, recorded with
`kind=token` in addition to the `token`/`create` operation histogram.

### Prometheus naming

An otel-collector's Prometheus exporter translates OTLP names dot→underscore and
appends the unit; counters gain a `_total` suffix. So `dizzy.operation.duration`
(seconds) becomes `dizzy_operation_duration_seconds_bucket` / `_count` / `_sum`,
and `dizzy.operation.errors` becomes `dizzy_operation_errors_total` with the
label `error_kind`.

VictoriaMetrics performs the same translation natively with
`-opentelemetry.usePrometheusNaming`.

### Query cookbook (PromQL)

```promql
# p95 operation latency per resource kind + operation, over time
histogram_quantile(0.95, sum by (kind, operation, le) (
  rate(dizzy_operation_duration_seconds_bucket[5m])))

# error + timeout rate per kind + operation (non-success share of all calls)
sum by (kind, operation) (rate(dizzy_operation_duration_seconds_count{outcome!="success"}[5m]))
  / sum by (kind, operation) (rate(dizzy_operation_duration_seconds_count[5m]))

# error rate by error kind (only failed calls create these series)
sum by (kind, operation, error_kind) (rate(dizzy_operation_errors_total[15m]))

# p95 time-to-ready per kind, over the last hour
histogram_quantile(0.95, sum by (kind, le) (
  rate(dizzy_resource_time_to_ready_seconds_bucket[1h])))

# iteration success rate
sum(rate(dizzy_iterations_total{outcome="success"}[1h]))
  / sum(rate(dizzy_iterations_total[1h]))
```

For a ready-to-run local backend and provisioned Grafana dashboards, see
[Export metrics to OpenTelemetry](../how-to/export-to-otel.md).
