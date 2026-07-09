# Export metrics to OpenTelemetry

`--otel` exports per-operation and per-iteration metrics over OTLP, so any
OTLP-compatible backend can store them: Prometheus behind an otel-collector,
Mimir, InfluxDB, VictoriaMetrics, Timescale.

It works on `monitor` and on one-shot `apply` and `chaos`, which flush the
exporter on exit.

For instrument names, attributes, and PromQL, see the
[metrics reference](../reference/metrics.md).

## Point it at a collector

Enablement and configuration are separate. `--otel` turns export on; the standard
environment variables configure it.

```console
$ export OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318
$ dizzy neutron monitor --scenario scenarios/neutron/small.yaml --otel
```

Without `--otel`, a globally exported `OTEL_EXPORTER_OTLP_ENDPOINT` does nothing.
That is deliberate.

To use gRPC instead of the default `http/protobuf`:

```console
$ export OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=grpc
$ export OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4317
```

Export failures from a down collector degrade to warnings. They never fail a run
— which is why a silently wrong endpoint loses metrics without an obvious error.

## A minimal collector pipeline

Remote-writing to Prometheus:

```yaml
receivers:
  otlp:
    protocols:
      grpc:
      http:
processors:
  batch:
exporters:
  prometheusremotewrite:
    endpoint: http://prometheus:9090/api/v1/write
service:
  pipelines:
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [prometheusremotewrite]
```

Swap `prometheusremotewrite` for `influxdb`, `otlphttp` (Mimir), or a
VictoriaMetrics remote-write endpoint to target those instead. Operating a
collector, TSDB, or Grafana is out of scope for `dizzy`; this is an example, not
a supported deployment.

## Use the local smoke stack instead

To exercise `--otel` end to end without assembling a backend, the repo ships a
one-command local stack: a [kind](https://kind.sigs.k8s.io/) cluster running
single-node [VictoriaMetrics](https://victoriametrics.com/), which ingests
OTLP/HTTP directly — no collector hop — plus a fully provisioned Grafana.

**Prerequisites:** `docker`, `kind`, `kubectl`, and `curl` on the host, plus
testbed reachability with `contrib/clouds.yaml`.

```console
$ make otel-up          # kind + VictoriaMetrics (:8428) + Grafana (:3000)
$ make testbed-monitor  # neutron monitor --otel against the testbed
$ make otel-grafana     # open the provisioned overview dashboard
$ make otel-ui          # or VMUI, for ad-hoc queries
```

Both host ports bind to `127.0.0.1` only. Grafana runs with **anonymous
read-only access** and must not be exposed beyond loopback.

Override the cadence, scenario, or service:

```console
$ make testbed-monitor MONITOR_INTERVAL=5m MONITOR_ITERATIONS=1
$ make testbed-monitor SERVICE=cinder
$ make testbed-monitor SCENARIO=scenarios/neutron/medium.yaml ARGS="--error-wait 2m"
```

With the defaults (`MONITOR_INTERVAL=0`, `MONITOR_ITERATIONS=0`) iterations run
back-to-back forever, driving the dashboards with a steady stream of data. The
export interval is pinned to 15 s for fast local feedback.

Tear it all down — VictoriaMetrics with its stored metrics, Grafana with its
throwaway state — with `make otel-down`. Storage is an `emptyDir`: data survives
a monitor restart, but not this.

## Check that it is actually working

Once the first iteration has completed:

```console
$ make otel-verify
```

It confirms five metric families are stored, prints the `cloud` / `scenario` /
`service` label values, checks Grafana's `/api/health`, and runs a
data-independent query through Grafana's datasource proxy — so a broken
Grafana→VictoriaMetrics wiring is distinguishable from "no data yet".

`dizzy_operation_errors_total` is deliberately **not** required. It only exists
once operations have failed, so a healthy run's steady state is its absence.

## The endpoint variable that bites

`make testbed-monitor` sets `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` to
`http://localhost:8428/opentelemetry/v1/metrics` — the full URL, path included.

The generic `OTEL_EXPORTER_OTLP_ENDPOINT` would get `/v1/metrics` appended and
miss VictoriaMetrics' ingestion path entirely. Because export failures degrade to
warnings, the metrics would vanish silently. Use the signal-specific variable
when the backend's path is not the OTLP default.

VictoriaMetrics runs with `-opentelemetry.usePrometheusNaming`, so the stored
series carry the canonical Prometheus names
(`dizzy_operation_duration_seconds_bucket`, …), and
`-opentelemetry.promoteAllResourceAttributes`, so `cloud` and `scenario` become
labels on every series.

## The dashboards

`make otel-up` provisions Grafana from `contrib/otel/grafana.yaml` and the JSON
under `contrib/otel/dashboards/`. Three dashboards are there the moment Grafana
comes up; no manual import.

- **Overview** (`dizzy-overview`) — iteration success ratio, iterations by
  outcome, iteration-duration percentiles, operations attempted/succeeded/failed,
  and the non-success share of all API calls.
- **API operations** (`dizzy-api-operations`) — per-kind p95 and mean latency, a
  latency heatmap, throughput, error+timeout rate, an ops-by-outcome table, and
  errors broken down by error kind. An `operation` variable filters to
  `create` / `delete` / … .
- **Resource readiness** (`dizzy-time-to-ready`) — time-to-ready percentiles by
  kind, per-kind readiness success ratio, and a heatmap.

All three carry `cloud` and `scenario` template variables; the first two also
carry `service`, so panels can be filtered when several monitors feed one
backend.

They respect what the OTLP data actually carries. **Percentiles are estimated
from histogram buckets** and labelled `p95 (est.)`, not true maxima. There are
**no true per-run min/max series**, so distribution shape is shown with heatmaps
over the `_bucket` series. The outcome panels split by `outcome` from
`operation.duration`, while the errors-by-kind panel uses the dedicated
`operation.errors` counter — empty on a healthy run.

### Editing them

Edit in Grafana, export the dashboard JSON, commit it under
`contrib/otel/dashboards/`. `make otel-up` re-provisions it: the ConfigMap's
content hash changes, which rolls the Grafana pod. Keep each dashboard's `uid`
stable so permalinks survive.

In-UI edits that are not exported are throwaway — Grafana's state is an
`emptyDir`, gone at `make otel-down`.

### If `make otel-up` refuses

kind cannot add a host-port mapping to a running cluster, so a cluster booted
before Grafana was added to the stack has no port 3000. `make otel-up` detects
this and tells you to `make otel-down && make otel-up` to recreate it.
