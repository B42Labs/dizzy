# Monitor a cloud continuously

`monitor` re-runs the single-shot pipeline — pre-flight sweep → `apply` →
`cleanup` — unattended, for days or weeks. Control-plane slowdowns, upgrade
regressions, and error-rate changes become visible as trends instead of being
buried in per-run JSON files.

Available in every service namespace: `neutron monitor`, `cinder monitor`,
`keystone monitor`, `nova monitor`, `glance monitor`, `barbican monitor`,
`octavia monitor`, and `c5c3 monitor`.

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

A monitor can instead, or as well, serve its metrics for a Prometheus scrape
and a liveness probe with `--listen-address`; see
[Serve Prometheus metrics and a health check](serve-prometheus-metrics.md).

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
authenticates its own client, so an unhealthy Keystone fails one iteration
rather than dead-looping. When `clouds.yaml` holds credentials, a token that
expires during an iteration is renewed on the first 401, as in a
[churn soak](run-a-soak.md#set-the-duration).

`keystone monitor` resolves the privilege tier once at startup for the same
reason. Its cross-run orphan sweep is **opt-in** via `--reclaim-orphans` and off
by default — with an admin token it lists cloud-wide, so enabling it is only safe
when no other `dizzy` process targets the cloud at all. Left off, the loop still
cleans up after each of its own iterations.

An iteration's cleanup retries calls that fail with a 5xx, a 429, a timeout, or
a connection error for up to `--cleanup-retry-budget`, 5 minutes by default.
Raise it for a cloud whose faults last longer:

```console
$ dizzy keystone monitor --scenario scenarios/keystone/small.yaml --cleanup-retry-budget 10m
```

An iteration whose cleanup still fails is cleaned up again, by its run id,
before each later iteration, so a transient fault costs a retry instead of
leaking the iteration's users, projects, and domains. The monitor keeps at most
20 such runs, in memory only: a restart forgets them, and the monitor logs each
one still pending at shutdown with its `dizzy keystone cleanup --run-id <id>`
command.

## Monitor Barbican

Same cadence, fixed-seed, and shutdown semantics:

```console
$ dizzy barbican monitor --scenario scenarios/barbican/small.yaml --otel
```

`barbican monitor` pre-checks the Barbican quotas once at startup, so an
oversized plan fails before the loop begins, and again at the start of every
iteration's apply, so a quota lowered during a long run fails that iteration
with the itemized message rather than with a wall of rejected creates.

A read-back that returns data that differs from what was written, whether a
payload, an ACL or a container's consumers, fails its iteration like any other
apply error. The loop goes on and the command still exits 0, so watch the
failed iterations and the `error.kind=mismatch` series. To gate CI on
corruption, run `barbican apply` or `barbican chaos`, which exit non-zero on a
mismatch.

The sweep is always on. It reclaims every `dizzy-` container and order in the
project, the secrets those orders generated, any `dizzy-` secret whose
`dizzy_type=secret` metadata confirms it, and any `dizzy-` secret without
metadata that the monitor's user created. That last rule covers a stored
secret whose metadata stamp failed and a secret Barbican kept although the
store failed or was canceled, which happens when a failed operation cancels
the stores still in flight. Keep the monitor running as the same user, so the
next iteration's sweep can confirm such a secret by its creator. A monitor
started as another user leaves it and logs
`leaving a secret without run metadata; another user created it` with its name.

## Monitor Octavia

Same cadence, fixed-seed, and shutdown semantics:

```console
$ dizzy octavia monitor --scenario scenarios/octavia/small.yaml --otel
```

In Octavia a degraded provider shows in time-to-ready, not in API latency, so
watch `dizzy.resource.time_to_ready` with `kind=loadbalancer`: it is the time
from a load balancer's create until it is first `ACTIVE`, the provider's build
time. The listener, pool, member, and health monitor writes record their wait
for `ACTIVE` under their own kinds. An amphora load balancer boots a VM and
takes one to two minutes to build, longer than the default `--timeout` of `1m`,
so run an amphora provider with a longer one:

```console
$ dizzy octavia monitor --scenario scenarios/octavia/small.yaml --set provider=amphora --timeout 5m --otel
```

Every iteration boots one amphora VM per load balancer, two in an
`ACTIVE_STANDBY` topology, so the `small` profile's two load balancers boot two
to four VMs per iteration. Set an `--interval` if that is more than the cloud
should carry back-to-back.

`octavia monitor` needs a project-scoped credential. Octavia and Neutron list
every project's objects for an administrator, so the sweep lists only within
the project of the token, and the monitor exits before its first iteration when
the auth result names no project. It pre-checks the Octavia quotas once at
startup and again at the start of every iteration's apply, as
`barbican monitor` does.

The sweep is always on. It takes every load balancer tagged
`dizzy:type=loadbalancer` in the project with cascade, then every subnet and
network tagged `dizzy:type=subnet` or `dizzy:type=network`, those a Neutron or
Nova run left included. A network that still holds a port, a Neutron run's
router interface for example, fails every sweep at `--timeout` until you clean
that run up with its own namespace's `cleanup --run-id <id>`; the id is the
second field of the network's `dizzy-<id>-<logical>` name.

## Monitor c5c3 orders

Same cadence and shutdown semantics, against the Kubernetes namespace the
scenario names:

```console
$ dizzy c5c3 monitor --scenario scenarios/c5c3/small.yaml --otel
$ dizzy c5c3 monitor --scenario scenarios/c5c3/small.yaml --kube-context <context> --os-cloud <entry> --otel
```

The second form runs from outside the cluster: `--os-cloud` names a
`clouds.yaml` entry whose `auth_url` and TLS settings replace those of the
documents the operator delivers. See
[Test c5c3-operator orders](test-c5c3-orders.md) for the namespace, the
rights and the override.

`c5c3 monitor` builds its Kubernetes client, reads the `--os-cloud` entry and
lists the five order kinds once at startup, so missing CRDs or rights fail
before the loop begins. Every iteration lists them again before its apply. A
c5c3 plan has no seed, so every iteration places the same five orders under a
new run id, and `--seed` changes nothing. An iteration of the bundled profile
lasts at least its 2 minute grace period, and each wait for the operator lasts
up to the scenario's `timeouts.ready` or `timeouts.gone`, 15 minutes each,
except the wait for the superseded application credential, which lasts up to
the grace period plus `timeouts.gone`. Every poll of a wait for the operator
is recorded, so `/healthz` does not read a long wait as a stall.

Watch two series. `dizzy.resource.time_to_ready` under the five order kinds,
`keystone_user` to `keystone_application_credential`, is the time from an
order's create until the operator reports it `Ready`. The errors of the
`authenticate` operation separate a broken delivery from an outage:
`error_kind="mismatch"` is chiefly a delivered credential that Keystone
rejected while its order was ready, and otherwise an application credential
token scoped to another project or a delivered document gophercloud cannot
read. `http_<status>`, `timeout` or `other` is Keystone being unreachable or
failing. A failed iteration does not stop the loop, as for `barbican monitor`,
so watch the failed iterations and these series.

The sweep is always on. It takes every order in the namespace that carries
the `dizzy/type` label, whatever run created it, and runs the steps of
`c5c3 cleanup` over it, so the next iteration reclaims a killed iteration's
orders. Run one dizzy process per Kubernetes namespace. An order whose
finalizer the operator never releases fails every later sweep, and a failed
sweep skips its iteration's apply, until someone removes the order's K-ORC
children in the ControlPlane's namespace; see
[Release a stranded finalizer](test-c5c3-orders.md#release-a-stranded-finalizer).

## Monitor Horizon

Same cadence and shutdown semantics, against the dashboard `--horizon-url`
names:

```console
$ dizzy horizon monitor --scenario scenarios/horizon/small.yaml --horizon-url https://horizon.example.com --otel
$ dizzy horizon monitor --scenario scenarios/horizon/small.yaml --horizon-url https://horizon.example.com --set resources.users=5 --reclaim-orphans --otel
```

The first form logs every session in as the `clouds.yaml` user and creates
nothing, so a member-only test user is enough. The second creates five users
and two projects per iteration, deletes them again at the iteration's end, and
with `--reclaim-orphans` first deletes the `dizzyhorizon-` users and projects
any earlier iteration left behind. Use a dedicated identity: dizzy caps failed
logins nowhere, and Keystone can lock an account after repeated ones.

`horizon monitor` authenticates the `clouds.yaml` entry once at startup, so a
wrong URL, a wrong password or a missing role fails before the loop begins,
and again at the start of every iteration, which a Keystone outage fails
without ending the loop. Watch `dizzy.operation.duration` per step kind,
`login_page` to `logout`, and `dizzy.resource.time_to_ready` under
`kind=session`, the time from the login form to the dashboard. The errors
separate a broken dashboard from a slow one: `error_kind="rejected"` is a
login Horizon refused, `error_kind="mismatch"` an answer that did not hold
what its status promised, and `http_<status>`, `timeout` or `other` the
dashboard failing or unreachable. A failed check fails its iteration while the
loop goes on, so watch the failed iterations and these series. See
[Check the Horizon dashboard](check-horizon.md).

## Via the Makefile

To run against the OSISM testbed, exporting into the local OTEL stack:

```console
$ make otel-up
$ make devstack-osism-monitor
$ make devstack-osism-monitor MONITOR_INTERVAL=5m MONITOR_ITERATIONS=1
$ make devstack-osism-monitor DEVSTACK_OSISM_SERVICE=cinder
$ make devstack-osism-monitor DEVSTACK_OSISM_SERVICE=barbican
$ make devstack-osism-monitor DEVSTACK_OSISM_SCENARIO=scenarios/neutron/medium.yaml ARGS="--error-wait 2m"
```

To monitor c5c3 orders on a local Cobalt Core dev stack:

```console
$ make devstack-c5c3-monitor DEVSTACK_C5C3_SERVICE=c5c3
```

To monitor the dev stack's Horizon:

```console
$ make devstack-c5c3-monitor DEVSTACK_C5C3_SERVICE=horizon ARGS="--horizon-url <URL>"
```

See [Run against a Cobalt Core dev stack](run-against-devstack-c5c3.md#order-through-the-c5c3-operator).
