# Serve Prometheus metrics and a health check

`--listen-address` opens an HTTP listener that serves the run's metrics on
`/metrics` for a Prometheus scrape and a health check on `/healthz` for a
liveness probe. Prometheus scrapes dizzy directly, with no collector in
between.

It works on `apply`, `chaos`, `keystone pressure` and `monitor`. The other
verbs accept the flag and open no listener.

For the metric families, their labels and the health rule, see
[Prometheus scrape endpoint](../reference/metrics.md#prometheus-scrape-endpoint)
and [Health endpoint](../reference/metrics.md#health-endpoint) in the metrics
reference.

## Start a monitor with a listener

```console
$ dizzy neutron monitor --scenario scenarios/neutron/small.yaml --listen-address :9464
```

`:9464` binds every interface of the host; `127.0.0.1:9464` binds loopback
only. The listener starts before the command authenticates against the cloud,
and dizzy logs `serving metrics and health` with the address and the stall
timeout. An address that another process holds fails the command:

```text
error: setting up telemetry: listening on :9464: listen tcp :9464: bind: address already in use
```

## Check that it is working

```console
$ curl -s http://localhost:9464/healthz
ok
$ curl -s http://localhost:9464/metrics | grep '^dizzy_iterations_total'
dizzy_iterations_total{cloud="mycloud",outcome="success",scenario="small",service="neutron"} 1
```

A series appears with its first sample, so `dizzy_iterations_total` shows up
once the first iteration has finished. `dizzy_operation_errors_total` appears
only once an operation has failed; on a healthy run its absence is the steady
state.

## Scrape it with Prometheus

```yaml
scrape_configs:
  - job_name: dizzy
    static_configs:
      - targets: ["dizzy-host:9464"]
```

The families carry the same names as on the OTLP path, so the
[query cookbook](../reference/metrics.md#query-cookbook-promql) works unchanged.
Every series carries `cloud`, `scenario` and `service` as labels when they are
set, and a `mix chaos` run serves all of its personas and lanes on one
endpoint, told apart by `persona` and `lane`.

## Probe it from Kubernetes

```yaml
containers:
  - name: dizzy
    image: ghcr.io/b42labs/dizzy:<tag>
    args:
      - "neutron"
      - "monitor"
      - "--scenario"
      - "/usr/share/dizzy/scenarios/neutron/small.yaml"
      - "--listen-address"
      - ":9464"
    env:
      - name: OS_CLOUD
        value: mycloud
    ports:
      - name: metrics
        containerPort: 9464
    livenessProbe:
      httpGet:
        path: /healthz
        port: 9464
      periodSeconds: 30
      failureThreshold: 2
```

Use a release that includes `--listen-address`. The `clouds.yaml` mount is
left out here; see
[Use the container image](install-and-verify.md#use-the-container-image).

`/healthz` answers 503 only after `--health-stall-timeout` has passed without
progress, so the stall timeout does the waiting and the probe can fail fast: a
small `failureThreshold` is fine. With the values above, the kubelet restarts
the container within a minute of the check starting to fail. The clock starts
when the listener starts, so a fresh process has the whole stall timeout to
finish its first operation.

After the first SIGTERM, the listener keeps serving until the process exits,
so Prometheus still scrapes the series while the iteration tears down.

## Choose the stall timeout

`--health-stall-timeout` defaults to `15m`. `/healthz` stays healthy while the
last recorded operation, successful, failed or timed out alike, or the last
finished iteration is younger than that.

Keep it greater than twice `--timeout`. A wait for a resource to become ready
or to go away records nothing until it ends and can last the whole
`--timeout`, and the call after it is recorded only when it returns, which can
take another `--timeout`. dizzy refuses a value that is not greater than twice
`--timeout` before the command runs:

```console
$ dizzy neutron monitor --scenario scenarios/neutron/small.yaml --listen-address :9464 --health-stall-timeout 2m
error: --health-stall-timeout (2m0s) must be greater than twice --timeout (1m0s), or 0 to disable the check
```

For `chaos`, keep it greater than `--max-interval` too, the longest random gap
between two churn actions.

A `monitor` needs no allowance for its pauses. The planned wait between two
iterations, the rest of `--interval`, the `--error-wait` pause and the 5 s
backoff after a failure, never counts as a stall, so a monitor with
`--interval 1h` keeps the default.

`--health-stall-timeout 0` disables the check, and `/healthz` then always
answers 200.

## Keep short runs in `--otel`

A one-shot `apply` closes its listener when it exits. A run shorter than the
scrape interval is never scraped, so for a short run use `--otel`, which
flushes its exporter on exit; see
[Export metrics to OpenTelemetry](export-to-otel.md).

## Run two processes on one host

Each process needs its own address:

```console
$ dizzy neutron monitor --os-cloud project-a --scenario scenarios/neutron/small.yaml --listen-address :9464 &
$ dizzy neutron monitor --os-cloud project-b --scenario scenarios/neutron/small.yaml --listen-address :9465 &
```

Give Prometheus both targets. Two monitors still need
[different projects](monitor-a-cloud.md#change-what-is-measured), because of
the pre-flight sweep.

## Keep the listener off the open network

The listener speaks plain HTTP, with no TLS and no authentication. Whoever
reaches the address reads the metrics and the health check. Bind it to
loopback, `127.0.0.1:9464`, when Prometheus runs on the same host, or to a
cluster-internal address otherwise.

## Use it together with `--otel`

`--listen-address` and `--otel` are independent: a command runs with either,
both, or neither. Both read the same instruments, so one run can push to an
OTLP backend and be scraped at the same time:

```console
$ export OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318
$ dizzy neutron monitor --scenario scenarios/neutron/small.yaml --otel --listen-address :9464
```
