# Monitor a cloud continuously

`monitor` re-runs the single-shot pipeline — pre-flight sweep → `apply` →
`cleanup` — unattended, for days or weeks. Control-plane slowdowns, upgrade
regressions, and error-rate changes become visible as trends instead of being
buried in per-run JSON files.

Available in every namespace: `neutron monitor`, `cinder monitor`, and
`keystone monitor`.

> **One `dizzy` per project.** The pre-flight sweep reclaims any `dizzy`-tagged
> resource it finds, so a concurrent `apply` or `chaos` in the same project would
> be torn down mid-flight. See
> [Resource identity and cleanup](../explanation/resource-identity.md).

## Run it continuously

With no `--interval`, iterations run back-to-back: the next starts the moment the
previous finishes.

```console
$ dizzy neutron monitor --scenario scenarios/neutron/small.yaml
```

This is the right default for driving dashboards — the metrics flow steadily.
Stop it with a single Ctrl-C: the current iteration tears down, the exporter
flushes, and the process exits. A second Ctrl-C aborts hard.

## Run it on a cadence

A positive `--interval` is the target time between iteration **starts**:

```console
$ dizzy neutron monitor --scenario scenarios/neutron/small.yaml --interval 15m
```

An iteration shorter than the interval waits out the remainder. One that overruns
starts the next immediately. Iterations never overlap and no backlog accumulates.

Note that on a paced run the metrics only move while an iteration is actually
running. Dashboards will show gaps between iterations. That is expected.

## Bound the run

```console
$ dizzy neutron monitor --scenario scenarios/neutron/small.yaml --iterations 10
```

`--iterations 0` (the default) runs forever.

## Back off when the cloud is unhealthy

In continuous mode a broken cloud is hammered in a tight loop: each iteration
fails fast and the next begins immediately. `--error-wait` is the brake.

```console
$ dizzy neutron monitor --scenario scenarios/neutron/small.yaml --error-wait 2m
```

It adds a pause **only after a failed iteration**. A monitor survives individual
iteration failures either way, logging a one-line summary per iteration.

## Export the metrics

Run records are **off by default** in monitor mode, because over days they
accumulate without bound. The OTLP export is the intended way to keep the data:

```console
$ dizzy neutron monitor --scenario scenarios/neutron/small.yaml --otel
```

See [Export metrics to OpenTelemetry](export-to-otel.md). If you do want the
per-iteration records anyway, pass `--keep-run-records` and clean up after
yourself.

## Change what is measured

The plan is expanded **once at startup** and reused by every iteration, so every
iteration builds the same topology. That is what makes latency and error trends
comparable across iterations — the resources are the same, so a change in the
numbers is a change in the cloud.

To broaden coverage instead, run several monitors with different seeds:

```console
$ dizzy neutron monitor --scenario scenarios/neutron/small.yaml --seed 1 --otel &
$ dizzy neutron monitor --scenario scenarios/neutron/small.yaml --seed 2 --otel &
```

…but only if they target **different projects**, because of the sweep.

## Monitor Cinder or Keystone

Identical cadence, fixed-seed, and shutdown semantics:

```console
$ dizzy cinder monitor --scenario scenarios/cinder/small.yaml --otel
$ dizzy keystone monitor --scenario scenarios/keystone/small.yaml --otel
```

`cinder monitor` resolves the volume type and pre-checks the Cinder quotas once
at startup, so a misconfiguration fails before the loop begins. Each iteration
re-authenticates, so token expiry over a multi-day loop fails one iteration
rather than dead-looping.

`keystone monitor` resolves the privilege tier once at startup for the same
reason. Its cross-run orphan sweep is **opt-in** via `--reclaim-orphans` and off
by default — with an admin token it lists cloud-wide, so enabling it is only safe
when no other `dizzy` process targets the cloud at all. Left off, the loop still
cleans up after each of its own iterations.

## Via the Makefile

To run against the OSISM testbed, exporting into the local OTEL stack:

```console
$ make otel-up
$ make devstack-osism-monitor
$ make devstack-osism-monitor MONITOR_INTERVAL=5m MONITOR_ITERATIONS=1
$ make devstack-osism-monitor DEVSTACK_OSISM_SERVICE=cinder
$ make devstack-osism-monitor DEVSTACK_OSISM_SCENARIO=scenarios/neutron/medium.yaml ARGS="--error-wait 2m"
```
