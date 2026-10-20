# Metrics reference

Every API call is wrapped with timing instrumentation that records the resource
kind, the operation, wall-clock duration, success or failure, the HTTP status,
and a timestamp. For resources with a status field, the time from "create
returned" to "status == expected" is recorded separately as **time-to-ready**,
measured by polling with backoff.

These measurements have two sinks. The in-memory collector is the source of
truth for run records and `report`. With `--otel`, the same seam also feeds an
OpenTelemetry exporter.

The collector keeps no individual sample. It adds each one to counters and a
fixed-size latency histogram per resource kind, so its memory does not grow with
the length of a run. Min, mean and max stay exact; the median and the p90, p95
and p99 percentiles are estimates within 1%.

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
time is visible rather than averaged away. A bounded run has ten equal buckets; a
run with `--duration 0` has one bucket per `--bucket-width`. Bucket percentiles
are exact. A run that ends with teardown also
performs a **leak check**, listing any resource still carrying the run tag after
the topology should be gone.

A `keystone pressure` record renders the metrics as above, by resource kind,
then a `Pressure` section with the workers' draws by operation: one sample per
draw, whatever calls it made. Its percentiles, per operation, per stage and
per bucket, are estimates within 1%, unlike a churn run's exact bucket
percentiles. The token path moves in sub-millisecond steps, so the table
rounds its latencies to the microsecond.

- **table**: a `Pressure` block with the lines `workers:`, `token pool:`,
  `churn users:` (only when the run reserved any), `mix:` and
  `revocations:`; an `Operations` table with one row per operation drawn and
  the columns `OPERATION`, `OPS`, `OK`, `FAILED`, `OPS/S`, `MIN`, `MEAN`,
  `P50`, `P90`, `P95`, `P99` and `MAX`; for a run with stages, a `Stages`
  table; and a `Latency and errors over time` table with the columns `START`,
  `OPERATION`, `OPS`, `OK`, `FAILED`, `P50`, `P99`, `REVOCATIONS` and
  `ERRORS`. Per bucket, an `all` row carries the bucket's cumulative
  revocations and its error kinds as `kind=count` pairs (`-` for none),
  followed by one row per operation drawn in the bucket with those two cells
  empty. `START` is rounded to the millisecond, and a row without operations
  shows `-` for its latencies. The `Stages` table has the columns `STAGE`,
  `FAN`, `START`, `END`, `OPERATION`, `OPS`, `OK`, `FAILED`, `OPS/S`, `P50`,
  `P95` and `P99`. Per stage, an `all` row is followed by one row per
  operation drawn in the stage, and every row carries the stage's number, its
  `fan_assignments` or `-` for none, and its start and end rounded to the
  millisecond. A stage's throughput divides by the time from its start to its
  end.
- **json**: `{"metrics": …, "pressure": …}`, with `"incomplete": true` added
  on a checkpoint. A staged run's stages are `pressure.stages`; see
  [Run record](run-record.md#stages).
- **csv**: the same header, with one row per operation labelled
  `pressure/<operation>`, then, per stage, one row per operation labelled
  `pressure/stage-<index>/<operation>`, e.g. `pressure/stage-2/effective`,
  and then, per bucket, one row per operation labelled
  `pressure@<start>/<operation>`, e.g. `pressure@1m0s/validate`, appended
  after the per-kind rows. The cumulative revocations and a stage's
  `fan_assignments` and window have no column; the other formats carry them.
- **html**: after the overall sections, a `Pressure` section with the
  settings and the revocations and the per-operation table; for a run with
  stages, a `Pressure stages` section with a sortable table with the columns
  `stage`, `fan`, `start`, `end`, `operation`, `ops`, `ok`, `failed`,
  `ops/s`, `p50`, `p95` and `p99`, rows as in the text table; then per
  operation a `Pressure: <operation>` section with its throughput and latency
  charts over time. A record without buckets shows no charts.

A `mix` record renders the overall metrics as above, the exact aggregate of
every persona and lane, then a section per persona, and then a section per
background lane when the run had any. It has no merged time series, because
the percentiles of separate engines cannot be merged.

- **table**: a `Personas` table with one row per persona (name, project or
  `-`, share as a whole percentage, servers, operations, ok, failed, p50, p95,
  p99), then per persona a `Persona <name>` section with its own metrics and
  churn summary. A record with lanes continues with a `Lanes` table with one
  row per lane (name, project or `-`, scenario, operations, ok, failed, p50,
  p95, p99), then per lane a `Lane <name>` section with its own metrics and
  churn summary.
- **json**: `{"metrics": …, "services": […], "personas": […], "lanes": […]}`,
  with `.metrics.overall` as for every other record. `services` and `lanes`
  are omitted when empty and `"incomplete": true` is added on a checkpoint.
- **csv**: the same header, with a `<persona>/overall` row and one
  `<persona>/<kind>` row per resource kind appended for each persona, and then
  a `<lane>/overall` row and one `<lane>/<kind>` row per resource kind for each
  lane.
- **html**: after the overall sections, one section per persona with its
  project, share and servers, its KPI tiles, and its throughput and latency
  charts over time, then one `Lane <name>` section per lane with its project
  and scenario, its KPI tiles and the same two charts. A persona or lane
  without churn statistics yet shows no charts.

### Comparing two runs

`report --compare` renders the comparison of two `keystone pressure` records
in three formats; see [Comparing two runs](cli.md#comparing-two-runs) for
which record is the base and when the command refuses two records. Each
metric of an operation is a row: `p50`, `p95`, `p99` and `ops/s`, for `all`
and then for every operation either record drew, sorted by name.

- **table**: a `Comparison` block with a `base:` and an `other:` line, each
  naming the run id, with ` (incomplete)` after it for a checkpoint, and the
  scenario, seed, workers, tokens and mix, and, when `--allow-mismatch` let
  differing loads through, a `loads differ:` line with the differences joined
  by `; `. Then an `Operations` table with the columns `OPERATION`, `METRIC`,
  `BASE`, `OTHER`, `DELTA` and `CHANGE` and, when either record has stages, a
  `Stages` table with `STAGE` and `FAN` before those columns. A latency is
  rounded to the microsecond and a throughput has one decimal. `DELTA` is the
  other minus the base with an explicit sign, e.g. `-420µs`, `+30µs`, `0s` or
  `+1190.4`, and `CHANGE` is the delta as a percentage of the base value,
  e.g. `-22.8%`. A side that lacks the operation or the stage, or drew
  nothing in it, shows `-` in its cell, in `DELTA` and in `CHANGE`, and a base
  value of `0` shows `-` in `CHANGE`. `FAN` is the base's `fan_assignments`,
  or the other's when only the other has the stage, and `-` for none.
- **json**: `{"base": …, "other": …, "mismatches": […], "operations": […], "stages": […]}`.
  `base` and `other` hold `runID`, `scenario`, `seed`, `incomplete` (only
  when `true`), `workers`, `tokens`, `mix` and `stages`, the `fan_assignments`
  of every stage with `0` for none, omitted for a run without stages. Each
  entry of `operations` holds `operation` and `base`, `other` and `delta`,
  each with `p50`, `p95` and `p99` in nanoseconds and `throughput`; a side
  that lacks the operation or the stage, or drew nothing in it, omits its
  key, and `delta` is omitted then too.
  Each entry of `stages` holds `index`, `fanAssignments` and its own
  `operations`. `mismatches` and `stages` are omitted when empty.
- **csv**: not available; `--compare` with `--format csv` fails.
- **html**: a self-contained page with the heading
  `Comparison: run <base> vs run <other>`, one line of chips per run, starting
  with `base run <id>` and `other run <id>`, a yellow `Loads differ: …` banner
  when the loads differ, and a sortable `Operations` table and, when either
  record has stages, a sortable `Stages` table, with the columns of the text
  tables in lower case. The `+` of a `delta` or `change` cell is written as
  the entity `&#43;` in the page source, which a browser shows as `+`.

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
| `cloud` | The `--os-cloud` name; for a `mix` persona or lane, the `clouds.yaml` entry it authenticated with |
| `scenario` | The scenario name; for a `mix` lane, the name of its plan, e.g. `small/keystone` |
| `service` | `neutron`, `cinder`, `keystone`, `nova`, `glance`, or `mix` |
| `persona` | `mix` only; the persona name, `ci`, `gardener` or `legacy` |
| `lane` | `mix` only; the background lane name, `cinder`, `glance`, `keystone` or `neutron`. A resource carries `persona` or `lane`, never both |

`mix chaos` exports one OTLP resource per persona and one per lane, so each
persona's and lane's series stay apart and carry their own iteration counts.
A lane's series carry `service=mix` and the `kind` values of the service it
churns.

`service` is a bespoke attribute, deliberately distinct from the semantic
`service.name`. It keeps the iteration-level series — which carry no `kind` — 
apart when a Neutron, a Cinder, a Keystone, a Nova, and a Glance monitor feed the
same backend. It mirrors the run record's `service` field.

### Instruments

| Instrument | Type | Unit | Attributes |
|---|---|---|---|
| `dizzy.operation.duration` | histogram | s | `kind`, `operation`, `outcome` |
| `dizzy.operation.errors` | counter | | `kind`, `operation`, `error.kind` |
| `dizzy.resource.time_to_ready` | histogram | s | `kind`, `outcome` |
| `dizzy.iteration.duration` | histogram | s | `outcome` |
| `dizzy.iteration.operations` | counter | | `result` |
| `dizzy.iterations` | counter | | `outcome` |

All five services share this schema; no service adds an instrument.

### Attribute values

Every value set is bounded.

| Attribute | Values |
|---|---|
| `kind` | **neutron:** `address-scope`, `subnet-pool`, `network`, `subnet`, `router`, `router-interface`, `security-group`, `security-group-rule`, `port`, `floating-ip` — **cinder:** `volume`, `snapshot` — **keystone:** `domain`, `project`, `user`, `role`, `role_assignment`, `token` — **nova:** `server`, `network`, `subnet`, `port`, `volume`, `server_group` (only the `mix` Gardener persona creates server groups) — **glance:** `image` |
| `operation` | `create`, `delete`, `get`, `list`, `tag`, `attach`, `detach`, `extend` (cinder resize), `update` (keystone domain disable-before-delete; glance property churn), `validate`, `revoke` (keystone token validation and revocation; `keystone pressure` only), `stop`, `start`, `reboot`, `resize`, `confirm-resize`, `live-migrate`, `cold-migrate` (nova server lifecycle; `cold-migrate` only for the `mix` Legacy persona), `upload`, `visibility`, `deactivate`, `reactivate`, `member-add`, `member-accept`, `member-remove` (glance image lifecycle) |
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
`keystone pressure` adds two operations under `kind=token`, `validate` and
`revoke`, which record no time-to-ready; its pool issues record as
`token`/`create` like any token issue, the three listings of its `effective`
operation as `role_assignment`/`list` and `project`/`list`, and the grants of
its stages as `role_assignment`/`create` like any grant. The per-draw samples
of its `Pressure` section stay in the run record and are not exported, so no
call is exported twice.

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
