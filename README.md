# openstack-tester

A scenario-driven load and consistency tester for OpenStack, starting with an
intensive focus on **Neutron** (networking).

The tool builds large, randomized but **reproducible** networking topologies
through the Neutron API in a single project, records how long every operation
takes and which states the resources reach, and is designed to later compare the
intended (API) state against the actual data plane (OVN / OVS).

> **Status:** Phase 1 in progress. The Go module, the `openstack-tester` CLI
> skeleton (the `neutron` command namespace), `clouds.yaml`-based
> authentication, the YAML scenario schema, the deterministic plan generator,
> the `generate` command, and `apply` (both `--dry-run` and the live executor,
> which builds the full tagged topology in dependency order and collects timing
> metrics) now exist. `apply` also pre-checks quotas before creating anything
> and persists a `run-<id>.json` record; `status` re-queries live state,
> `report` renders the metrics as table/JSON/CSV, and `cleanup` deletes a run's
> tagged resources idempotently. Topologies can now also wire **internal routers
> together** (transit-subnet links) and, when the target cloud has an external
> network, plug a fraction of routers into it as a **gateway** and allocate
> **floating IPs**. The `small`, `medium`, and `large` scenario profiles now ship
> under `scenarios/`; the optional Prometheus textfile export is the remaining
> Phase 1 item. A `neutron monitor` command now re-runs a scenario
> continuously or on a fixed cadence, unattended, and — with `--otel` — exports
> per-operation and per-iteration metrics via **OpenTelemetry (OTLP)** so a
> single installation can be observed over time. A first **Cinder** (block
> storage) slice now ships too, under the `cinder` command namespace: it
> creates, extends (resizes), and snapshots volumes from `small`/`medium`/`large`
> profiles, reusing the same plan → apply → record → report/status/cleanup
> machinery (see §15).

---

## 1. Goals

- Create **complex, randomized network scenarios** via the Neutron API, e.g.
  ~20 routers, ~100 networks, ~200 subnets, a handful of subnet pools, several
  security groups (with rules), and a number of ports.
- Make every scenario **fully parametrizable** (counts, ratios, topology shape)
  and **deterministic** via a random seed, so a run can be reproduced exactly.
- **Track timing and state**: how long each resource takes to create, how long
  it takes to reach its expected status, aggregate latency statistics, error
  rates, and total throughput.
- Run everything in a **single project** to begin with, with reliable,
  tag-based cleanup.
- Be structured so that **multiple named scenarios** (profiles) can be defined
  and selected.
- Lay the groundwork for a **later data-plane verification phase** that checks
  whether OVN (NB/SB) and OVS reflect what the API says should exist.

## 2. Non-goals (for now)

- **No VMs / Nova.** Phase 1 is networking only.
- **No load balancers (Octavia).**
- **External gateways and floating IPs are optional, never required** — they are
  used only when the target cloud has an external network (auto-detected or named
  with `--external-network`); otherwise that part of the plan is a silent no-op.
- Not a correctness test suite like Tempest; this is a **load, timing and
  consistency** tool. The two are complementary.

## 3. Scope by phase

| Phase | Focus | State |
|-------|-------|-------|
| **1** | Generate + apply randomized networking topologies via the API; record timings and states; tag-based cleanup. | Planned (this README) |
| **2** | Data-plane verification: reconcile API state against OVN NB/SB DB and OVS flows. | Future |
| **3** | More scenario profiles, optional external connectivity (gateways, FIPs), trunk ports, RBAC, address scopes. | Future |
| **later** | Extend beyond Neutron (Cinder, Nova, …) — hence the generic name `openstack-tester`. | Started: Cinder first slice (create / resize / snapshot volumes) plus `cinder monitor` (#31) and `cinder chaos` (#32). |

---

## 4. Core concepts

The design separates **what we intend to create** from **what actually
happened**. This split is what makes both reproducibility and the later
data-plane verification possible.

- **Scenario** — a high-level, parametrized description of the desired topology
  (counts, ratios, topology shape, RNG seed). Lives in a YAML file or is
  selected from a built-in profile. Human-authored.
- **Plan** — the concrete, fully-expanded set of resources and their
  relationships, produced deterministically from `scenario + seed`. Every
  network, subnet, router interface, port, security group rule is enumerated
  with its intended attributes. The plan is the **source of truth / expected
  state** and is the input for Phase 2 verification. Machine-generated, can be
  dumped to JSON for inspection.
- **Run** — one execution of a plan against a cloud. Produces created resource
  IDs, per-operation timings, observed states, and errors. Persisted as a run
  record (`run-<id>.json`) so it can be reported on, re-checked, or cleaned up
  later.
- **Metrics** — timing and state statistics derived from a run.

```
scenario.yaml ─┐
               ├─►  generate  ─►  plan.json  ─►  apply  ─►  run-<id>.json ─►  report
   seed ───────┘                    │                          │
                                     └────────► (Phase 2) verify ◄── OVN/OVS
```

---

## 5. Neutron resources covered (Phase 1)

Created in dependency order; torn down in reverse.

1. **Subnet pools** (and optionally **address scopes**) — a small number,
   shared by subnets that opt into pool-based allocation.
2. **Networks** — the bulk; tenant networks (geneve/vxlan by default).
3. **Subnets** — multiple per network; some from explicit CIDRs, some allocated
   from a subnet pool; IPv4 and (optionally) IPv6.
4. **Routers** — internal routers, optionally plugged into an external network
   as a gateway when one is available on the target cloud.
5. **Router interfaces** — attach a subset of subnets to routers, forming
   randomized but valid topologies (a subnet attaches to at most one router). An
   interface attaches either a subnet (taking its gateway address) or a port —
   the port form wires two routers together over a shared transit subnet.
6. **Security groups** + **security group rules** — several groups, each with a
   randomized rule set (ingress/egress, protocols, port ranges, remote CIDR or
   remote-group references).
7. **Ports** — created on networks/subnets, with security groups attached;
   fixed IPs either auto-allocated or explicitly assigned.
8. **Floating IPs** — allocated from the external network (when available), some
   associated with an internal port reachable through an external-gateway router.

### Dependency graph

```
address scope ──► subnet pool ──► subnet ──► router interface ──► router ──► (external gateway)
                                    ▲             ▲                            ▲
network ────────────────────────────┘            │                            │
   └──────────────► port ──────────────────────► (router-link port)    floating IP
        ▲                                                                     │
        └──────── security group ◄── security group rule                      ┘ (optional association)
```

**Optional / later:** trunk ports, RBAC policies, port forwarding, QoS policies.

---

## 6. Scenario parametrization

A scenario is defined by counts, ratios and distributions plus a seed. Example
(`scenarios/neutron/medium.yaml`):

```yaml
name: medium
seed: 1234567                 # deterministic; same seed => same plan

resources:
  subnet_pools:   3
  address_scopes: 0
  networks:       100
  routers:        20
  security_groups: 15
  router_links:    5             # router-to-router transit links
  floating_ips:    10            # allocated from the external network, if one exists

distribution:
  subnets_per_network:   { min: 1, max: 3 }    # ~200 subnets total
  ports_per_network:     { min: 0, max: 5 }
  rules_per_security_group: { min: 2, max: 12 }
  subnet_from_pool_ratio: 0.4                   # 40% of subnets use a pool
  ipv6_ratio:            0.2
  subnets_attached_to_router_ratio: 0.6
  routers_with_external_gateway_ratio: 0.3      # 30% of routers want an external gateway
  floating_ip_associated_ratio:        0.5      # half the floating IPs target a port

topology:
  router_attach_strategy: random   # how subnets are distributed across routers
  port_security_group_count: { min: 1, max: 3 }
```

### External connectivity and router links

Three topology features go beyond a single isolated project, all optional and
all deterministic in the plan:

- **External gateways** — `routers_with_external_gateway_ratio` marks that
  fraction of routers as wanting an external gateway. The plan only records the
  *intent*; the actual external network is discovered on the target cloud at
  `apply` time (`--external-network <name>`, or the first external network found).
  If the cloud has no external network, the intent is a silent no-op.
- **Floating IPs** — `resources.floating_ips` allocates that many floating IPs
  from the external network, of which `floating_ip_associated_ratio` are
  associated with an internal port reachable through an external-gateway router
  (each eligible port at most once); the rest stay unassociated. Floating IPs are
  created only when an external network is available.
- **Router links** — `resources.router_links` wires pairs of routers together.
  Each link adds a dedicated transit network and `/30` subnet (allocated from
  `192.168.0.0/16`) plus a port: one router owns the subnet's gateway address,
  the peer attaches through the port. This needs at least two routers.

The example from the original request (20 routers / 100 networks / 200 subnets /
a few subnet pools / various security groups / some ports) maps directly onto
such a file and ships as the `medium` built-in profile (see below).

Parameters can be overridden on the CLI (e.g. `--set resources.networks=200`)
without editing the file, to make sweeps easy.

Generation is deterministic: the same `scenario + seed` always expands to a
byte-identical plan, stable across runs and Go versions. The global `--seed`
flag overrides the scenario's `seed`. Plan CIDRs are allocated deterministically
from non-overlapping ranges — explicit IPv4 subnets from `10.0.0.0/8`, IPv6
subnets from `fd00::/16`, subnet pools from `172.16.0.0/12`, and router-link
transit subnets as `/30`s from `192.168.0.0/16`.

### Churn / soak mode (`chaos:`)

The `neutron chaos` command (§7) reuses the same scenario as the spatial
envelope but adds a temporal frame: the churn knobs. They can be set on the CLI
or in an optional `chaos:` block in the scenario YAML (flags override the block);
an omitted field falls back to the command's default.

```yaml
chaos:
  duration: 30m                 # total runtime; the only hard stop besides Ctrl-C
  interval: { min: 200ms, max: 3s }   # random delay drawn per tick
  parallel: { max: 6 }          # per-tick fan-out, drawn in [1, max]; bounded by --concurrency
  churn_ratio: 0.5              # create bias at steady state (0..1)
  target_fill: 0.8              # fraction of the envelope to keep populated (0..1)
```

### Built-in profiles

Three ready-to-use profiles ship under `scenarios/neutron/`, selected by passing
the file path to `--scenario`:

| Profile | Networks | Routers | Subnets | Chaos duration | Notes |
|---------|----------|---------|---------|----------------|-------|
| `small`  | 3   | 2  | ≤ 9  | 5m  | Fits Neutron's default per-project quotas. |
| `medium` | 100 | 20 | ~200 | 30m | The headline example above; needs raised quotas. |
| `large`  | 200 | 40 | ~400 | 1h  | Twice the headline; needs raised quotas, guarded by the `apply` quota pre-check. |

Every profile also carries a `chaos:` block (duration, intervals, fan-out, and
controller knobs), so `neutron chaos` runs each one straight away with no extra
flags. Override any of them on the CLI when you want a longer soak or denser
churn.

```
openstack-tester neutron generate  --scenario scenarios/neutron/medium.yaml [--out plan.json]
openstack-tester neutron apply     --scenario scenarios/neutron/large.yaml  [--dry-run]
openstack-tester neutron chaos     --scenario scenarios/neutron/small.yaml  # 5m churn, no flags needed
```

---

## 7. CLI design

A single binary `openstack-tester` with subcommands (Neutron grouped under a
`neutron` namespace to leave room for other services later):

```
openstack-tester neutron generate  --scenario medium.yaml [--out plan.json]
openstack-tester neutron apply     --scenario medium.yaml [--dry-run]
openstack-tester neutron chaos     --scenario medium.yaml [--duration 30m]
openstack-tester neutron monitor   --scenario medium.yaml [--interval 15m] [--iterations n] [--error-wait 2m] [--keep-run-records] [--otel]
openstack-tester neutron status    --run run-<id>.json
openstack-tester neutron report    --run run-<id>.json [--format table|json|csv|html]
openstack-tester neutron cleanup   --run run-<id>.json   # or --run-id <id>
openstack-tester neutron verify    --run run-<id>.json   # Phase 2 (future)
```

- `generate` — expand a scenario into a plan and dump it; never touches the API.
- `apply` — generate (or load) a plan, create resources, poll states, record a
  run record + metrics. `--dry-run` validates and prints what would be created.
  `--external-network <name>` selects the external network for router gateways
  and floating IPs (default: auto-detect the first external network).
- `chaos` — random churn / soak mode. Instead of building the topology once, it
  runs for `--duration` and uses the scenario as the **envelope** (upper bound):
  for the whole runtime it keeps creating *and* deleting resources at random,
  seeded intervals and parallelism, so the live population never exceeds the
  scenario's counts and only planned resources are ever created. Knobs:
  `--duration` (the only hard stop besides Ctrl-C / SIGTERM); `--min-interval` /
  `--max-interval` (the random delay range between actions); `--max-parallel`
  (the per-tick fan-out cap, itself bounded by the global `--concurrency`);
  `--churn-ratio` and `--target-fill` (the create/delete controller — see §10);
  `--no-cleanup` (leave the topology in place); `--external-network` (as for
  `apply`). The same knobs can live in a `chaos:` block in the scenario YAML;
  flags override the block. The three built-in profiles ship such a block, so
  `--duration` (and the rest) is optional when running them. With `--seed` fixed
  (and identical settings) the
  whole action schedule is reproducible. On a clean finish it tears the topology
  down by tag and runs a leak check; Ctrl-C / SIGTERM leaves the resources in
  place for an explicit `cleanup --run <id>`.
- `monitor` — continuous or periodic mode. It re-runs the single-shot pipeline
  (pre-flight orphan sweep → `apply` → `cleanup`) unattended, so an
  installation can be observed over time. By default (no `--interval`, or
  `--interval 0`) iterations run **continuously**, back-to-back: the next
  starts the moment the previous one finishes. A positive `--interval` is the
  target cadence between iteration *starts* instead: an iteration shorter than
  the interval waits the remainder, one that overruns starts the next
  immediately. Either way iterations never overlap and no backlog builds up.
  `--iterations` caps the run (`0` = run forever); `--error-wait` adds a pause
  after a failed iteration to avoid hammering an unhealthy cloud, and is the
  recommended brake in continuous mode. It survives individual iteration
  failures and logs a one-line summary per iteration. Ctrl-C / SIGTERM stops
  the loop, tears the
  current iteration down, and flushes the exporter; a second signal aborts
  hard. Per-iteration `run-<id>.json` records are **off by default**
  (`--keep-run-records` re-enables them, but they accumulate unboundedly); the
  export via `--otel` is the intended way to keep the data. See §9 for the
  cadence semantics, the fixed-seed default, and the pre-flight sweep's
  exclusive-project caveat.
- `status` — re-query the current state of a run's resources from the API.
- `report` — render metrics from a run record (table / JSON / CSV / a
  self-contained visual HTML report).
- `cleanup` — delete all resources belonging to a run, in reverse dependency
  order; idempotent. Tag-discoverable resources are found by the run tag; address
  scopes (which Neutron may not let us tag) are reclaimed from the run record by
  id, so reclaiming them needs `--run`, not a bare `--run-id`.
- `verify` — (Phase 2) compare run/plan against OVN/OVS.

Global flags: `--os-cloud` (defaults to `$OS_CLOUD`), `--concurrency`,
`--timeout`, `--seed` (override scenario seed), `--log-level`, `--otel` (export
metrics via OpenTelemetry OTLP — see §9).

---

## 8. Configuration & authentication

Authentication follows the same `clouds.yaml` convention as the rest of the
testbed (see [`../openstack-cli`](../openstack-cli)). gophercloud v2 reads
`clouds.yaml` natively:

```go
authOptions, endpointOptions, tlsConfig, err := clouds.Parse() // OS_CLOUD
providerClient, err := config.NewProviderClient(ctx, authOptions,
    config.WithTLSConfig(tlsConfig))
netClient, err := openstack.NewNetworkV2(ctx, providerClient, endpointOptions)
```

`clouds.Parse()` honors `OS_CLOUD` and the standard search paths (current
directory, `~/.config/openstack`, `/etc/openstack`). The testbed CA must be
trusted (see the `openstack-cli` README for placing `testbed.crt`).

Run from anywhere with API access (operator workstation or a manager node).
Phase 2 additionally needs access to the OVN databases / OVS on the
control/network nodes.

---

## 9. Metrics & state tracking

Every Neutron API call is wrapped with timing instrumentation that records:

- resource type, operation (`create` / `get` / `delete`), wall-clock duration,
  success/error, HTTP status, and a timestamp.
- **Time-to-ready**: for resources with a status field (ports, networks,
  routers), the time from "create returned" to "status == expected" (e.g. a
  port reaching `ACTIVE`/`DOWN`), measured by polling with backoff.

Reported per resource type and overall:

- counts (attempted / succeeded / failed),
- latency stats: min / mean / median / p90 / p95 / p99 / max,
- throughput (operations per second), effective concurrency,
- total wall-clock for the run,
- error breakdown (timeouts, 409 conflicts, quota, 5xx, …).

`report` renders a run record's metrics in four formats:

- human-readable **table** on stdout (the default),
- **JSON** metrics (machine-readable),
- **CSV** with one row per resource type plus an overall row,
- a self-contained, offline **HTML** report with inline SVG charts for
  latency, throughput, and error rates — and, for a churn run, the
  per-bucket degradation over time — to archive next to the run record
  (`--format html > report.html`).

The canonical run record itself is the `run-<id>.json` written by `apply`. An
optional **Prometheus textfile** export to fit the testbed's monitoring is
planned but not yet implemented.

A **churn run** (`chaos`) records the same per-call metrics plus churn-specific
statistics in the run record, which `report` renders after the standard summary:
counts of create / delete operations and completed create→delete cycles, the
live-population summary over the run (min / mean / max and the controller's
target fill), and latency / error rate **bucketed over the run's duration** (to
expose degradation over time, not just an aggregate). When the run finishes with
teardown it also performs an end-of-run **leak check** — listing any resources
still carrying the run tag after the topology should be gone.

### Monitoring over time (`neutron monitor`)

`neutron monitor` re-runs the single-shot pipeline continuously by default or
on a fixed cadence, unattended, for days or weeks, so control-plane slowdowns,
upgrade regressions, and error-rate changes become visible as trends instead of
being buried in per-run JSON files. One **iteration** is a pre-flight orphan
sweep → `apply` →
`cleanup`, composing the same executor, metrics collector, and cleanup code
paths a one-shot `apply` uses. An iteration is always `apply` → `cleanup`; a
scenario's `chaos:` block is not used by `monitor` (chaos-soak iterations are a
possible follow-up).

- **Cadence.** With `--interval` omitted or `0` (the default) the loop runs
  **continuously**: the next iteration starts as soon as the previous one
  finishes, back-to-back, until stopped. A positive `--interval` is the target
  time between iteration *starts* instead. A fast iteration waits out the
  remainder of the interval; one that overruns it starts the next immediately,
  so iterations never overlap and no backlog builds up. `--iterations` caps the
  run (`0` = forever); `--error-wait` adds a pause after a failed iteration and
  is the brake that keeps a broken cloud from being hammered in a tight loop.
- **Fixed seed by default.** The plan is expanded once at startup, so every
  iteration reuses the same seed and therefore the same topology. This is the
  comparability default: the same resources are created each time, so latency
  and error trends compare like-for-like. Rotating the seed per iteration would
  broaden coverage at the cost of that comparability; if you want it, run
  several monitors with different `--seed` values.
- **Self-healing pre-flight sweep.** Before each iteration the loop deletes
  leftover `ostester`-tagged resources so a previous crashed or interrupted
  iteration cannot accumulate. Because Neutron tag filtering is exact-match, the
  sweep matches the `ostester:type=<kind>` tag and therefore reclaims **any**
  tester-created resource in the project, not only `monitor`'s own. **Do not run
  `monitor` concurrently with another `apply`/`chaos`/`monitor` run in the same
  project** — the sweep would tear the other run down mid-flight. Address scopes
  cannot be discovered by tag and are not swept (the same limitation
  `cleanup --run-id` has).
- **Graceful shutdown.** SIGINT/SIGTERM stops the loop, tears the current
  iteration down (best effort, on a context that survives the signal so nothing
  leaks), flushes the metrics exporter, and exits. A second signal aborts hard.

`cinder monitor` drives the same loop for block storage (pre-flight sweep →
`apply` → `cleanup`) with identical cadence, fixed-seed, and shutdown semantics;
see [§15](#15-cinder-block-storage) for its block-storage specifics.

### OpenTelemetry export (`--otel`)

With `--otel`, `monitor` — and one-shot `apply`/`chaos` (flushed on exit) —
export per-operation and per-iteration metrics via the OpenTelemetry SDK over
OTLP, so any OTLP-compatible backend (Prometheus + otel-collector, Mimir,
InfluxDB, VictoriaMetrics, Timescale, …) can store them.

- **Enablement is explicit.** Only `--otel` turns export on. The standard
  `OTEL_EXPORTER_OTLP_*` environment variables *configure* the exporter but
  never *enable* it, so a globally exported `OTEL_EXPORTER_OTLP_ENDPOINT` does
  not change the behavior of any command run without `--otel`.
- **Configuration.** Endpoint, headers, and TLS come from the standard
  `OTEL_EXPORTER_OTLP_*` env vars — there is no custom config surface. The
  protocol is taken from `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL` (falling back to
  `OTEL_EXPORTER_OTLP_PROTOCOL`), defaulting to `http/protobuf` (port 4318);
  set it to `grpc` for a gRPC-only collector endpoint (port 4317).
  `OTEL_METRIC_EXPORT_INTERVAL` controls the push period. Export failures from
  a down collector degrade to warnings and never fail a run.
- **Resource attributes** identify one installation across time:
  `service.name=openstack-tester`, `service.version`, plus `cloud` (the
  `--os-cloud` name), `scenario`, and `service` (`neutron` | `cinder`). The
  `service` attribute keeps the iteration-level series
  (`openstack_tester.iteration.*`), which carry no `kind`, distinguishable when a
  Neutron and a Cinder monitor feed the same backend; it is a bespoke resource
  attribute (like `cloud`/`scenario`), deliberately distinct from the semantic
  `service.name`, and mirrors the run record's `service` field.

Instruments (recorded live at the Neutron client's timing seam alongside the
in-memory collector, which stays the source for run records and reports):

| Instrument | Type | Unit | Attributes |
|---|---|---|---|
| `openstack_tester.operation.duration` | histogram | s | `kind`, `operation`, `outcome` |
| `openstack_tester.operation.errors` | counter | | `kind`, `operation`, `error.kind` |
| `openstack_tester.resource.time_to_ready` | histogram | s | `kind`, `outcome` |
| `openstack_tester.iteration.duration` | histogram | s | `outcome` |
| `openstack_tester.iteration.operations` | counter | | `result` |
| `openstack_tester.iterations` | counter | | `outcome` |

Attribute value sets are bounded: `kind` is the resource type (`network`,
`port`, `router`, … and, for Cinder, `volume` and `snapshot`); `operation` is
one of `create`, `delete`, `get`, `list`, `tag`, `detach`, and — for Cinder's
resize — `extend`; `outcome` is `success`/`error`/`timeout` for an operation,
`success`/`timeout` for time-to-ready, and `success`/`failure` for an
iteration; `result` is `attempted`/`succeeded`/`failed`. **Cardinality rule:**
run IDs, resource IDs, and names are **never** metric attributes — they stay in
the run records and logs.

Cinder needs no new instruments: the new `kind` values (`volume`, `snapshot`)
and the new `operation` value (`extend`) flow through the same instruments, so a
one-shot `cinder apply --otel` (or a `cinder monitor` loop) exports through the
existing seam and appears in the Grafana API-operations dashboard (which keys
panels on the `kind`/`operation` labels) with no dashboard changes. The one
addition is the `service` resource attribute above, so the iteration-level
series stay per-service; the overview dashboard gains a matching `service`
variable to filter on it.

`operation.errors` breaks failures down where `operation.duration`'s `outcome`
collapses them: `error.kind` is the service client's classification —
`quota`, `timeout`, `canceled`, `other`, or `http_<status>` with the exact
status code (the small set the service returns: 400/401/403/404/409/429/5xx for
Neutron, plus Cinder's 413 over-limit), the same values the report's Errors
table shows. The counter is recorded **only for
failed operations**, so a healthy run emits no series at all. On the Prometheus
side it surfaces as `openstack_tester_operation_errors_total` with label
`error_kind` (the `_total` suffix and the dot→underscore label translation
follow the same naming rules the cookbook preamble below explains).

#### Example collector setup

Point the tester at an [OpenTelemetry
Collector](https://opentelemetry.io/docs/collector/) that fans out to your
time-series backend. A minimal Prometheus-remote-write pipeline:

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

Swap the `prometheusremotewrite` exporter for `influxdb`,
`otlphttp` (Mimir), or a VictoriaMetrics remote-write endpoint to target those
backends instead. Shipping or operating the collector/TSDB/Grafana stack is out
of scope for the tester — this is only an example. For a ready-to-run local
backend that needs no collector at all, see the local OTEL smoke stack below.

#### Query cookbook (PromQL)

The otel-collector's Prometheus exporter translates OTLP names dot→underscore
and appends the unit, and counters gain a `_total` suffix — so
`openstack_tester.operation.duration` (s) becomes
`openstack_tester_operation_duration_seconds_*`. (VictoriaMetrics performs the
same translation natively with `-opentelemetry.usePrometheusNaming`, as the
local smoke stack below configures.) The metrics are designed for:

```promql
# p95 operation latency per resource kind + operation, over time
histogram_quantile(0.95, sum by (kind, operation, le) (
  rate(openstack_tester_operation_duration_seconds_bucket[5m])))

# error + timeout rate per kind + operation (non-success share of all calls)
sum by (kind, operation) (rate(openstack_tester_operation_duration_seconds_count{outcome!="success"}[5m]))
  / sum by (kind, operation) (rate(openstack_tester_operation_duration_seconds_count[5m]))

# error rate by error kind, per kind + operation (only failed calls create series)
sum by (kind, operation, error_kind) (rate(openstack_tester_operation_errors_total[15m]))

# p95 time-to-ready per kind, over the last hour
histogram_quantile(0.95, sum by (kind, le) (
  rate(openstack_tester_resource_time_to_ready_seconds_bucket[1h])))

# iteration success rate
sum(rate(openstack_tester_iterations_total{outcome="success"}[1h]))
  / sum(rate(openstack_tester_iterations_total[1h]))
```

Ready-made Grafana dashboards ship with the local OTEL smoke stack below, under
`contrib/otel/dashboards/` — provisioned automatically, no manual import.

### Local OTEL smoke stack (kind + VictoriaMetrics + VMUI + Grafana)

To exercise `--otel` end to end without hand-assembling a backend, the repo
ships a one-command local stack under `contrib/otel/`: a
[kind](https://kind.sigs.k8s.io/) cluster running single-node
[VictoriaMetrics](https://victoriametrics.com/), which ingests the tester's
OTLP/HTTP push directly and serves **VMUI** — its built-in query explorer — in
the browser, plus a fully provisioned **Grafana** for curated dashboards. No
OTel Collector is needed (VictoriaMetrics ingests OTLP natively). VMUI stays the
ad-hoc query explorer; Grafana is the curated view — its datasource and
dashboards come from files in the repo, so it comes up with no clicking.

```text
browser ─▶ http://localhost:3000 ──▶ kind hostPort 3000 ─▶ NodePort 30300
                                       └─ Grafana ──(in-cluster)──▶ http://victoria-metrics:8428
browser ─▶ http://localhost:8428/vmui (unchanged)
tester  ─▶ OTLP/HTTP push to localhost:8428 (unchanged)
```

Grafana talks to VictoriaMetrics over the in-cluster Service DNS; nothing about
the tester's export path changes. Both host ports bind to `127.0.0.1` only —
Grafana runs with **anonymous read-only (Viewer) access** and must not be
exposed beyond loopback.

**Prerequisites:** `docker`, `kind`, `kubectl`, and `curl` on the host, plus
testbed reachability with `contrib/clouds.yaml` (the same setup `make testbed`
needs).

The flow:

```console
$ make otel-up          # boot kind + VictoriaMetrics (:8428) + Grafana (:3000)
$ make testbed-monitor  # run monitor --otel (SERVICE=neutron by default) against the testbed
$ make otel-grafana     # open the provisioned Grafana overview dashboard
$ make otel-ui          # or open VMUI for ad-hoc queries
```

`make testbed-monitor` runs `neutron monitor --otel` continuously by default,
exporting into the local VictoriaMetrics. Override the cadence, count,
scenario, service namespace, or add flags:

```console
$ make testbed-monitor MONITOR_INTERVAL=5m MONITOR_ITERATIONS=1
$ make testbed-monitor SCENARIO=scenarios/neutron/medium.yaml ARGS="--error-wait 2m"
$ make testbed-monitor SERVICE=cinder  # scenarios/cinder/small.yaml, exports service=cinder
```

With `MONITOR_INTERVAL=0` and `MONITOR_ITERATIONS=0` (both the default) it runs
iterations back-to-back forever, driving the dashboards with a steady stream of
data; `MONITOR_INTERVAL=5m` restores the paced behaviour. A single Ctrl-C stops
it gracefully — the current iteration tears down, the exporter flushes, and any
leftover `run-*.json` is swept, exactly as `make testbed` does. The export
interval is pinned to 15 s for fast local feedback.

Once the first iteration has completed, check the stored schema and the Grafana
path:

```console
$ make otel-verify  # five metric families + cloud/scenario labels, Grafana health
```

Beyond the five metric families and their `cloud`/`scenario` labels,
`otel-verify` confirms Grafana answers on `/api/health` and runs a
data-independent query through Grafana's datasource proxy, so a broken
Grafana→VictoriaMetrics wiring is distinguishable from "no data yet".
`openstack_tester_operation_errors_total` is deliberately **not** among the
required families: it only exists once operations have failed, so a healthy
run's steady state is its absence, not a missing-metric failure.

**How it works.** `make testbed-monitor` points
`OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` at
`http://localhost:8428/opentelemetry/v1/metrics`, which VictoriaMetrics ingests
directly — no collector hop. The manifests run it with
`-opentelemetry.usePrometheusNaming`, so the stored series carry exactly the
Prometheus canonical names from the cookbook above
(`openstack_tester_operation_duration_seconds_bucket`, …), and
`-opentelemetry.promoteAllResourceAttributes`, so the `cloud` and `scenario`
resource attributes become labels on every series. Storage is an `emptyDir`:
data survives monitor restarts but not `make otel-down`.

> **Use the signal-specific endpoint variable.**
> `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` takes the full URL including the
> `/opentelemetry/v1/metrics` path. The generic `OTEL_EXPORTER_OTLP_ENDPOINT`
> would get `/v1/metrics` appended and miss VictoriaMetrics' ingestion path —
> and because export failures degrade to warnings, the metrics would silently
> vanish.

#### Grafana dashboards

`make otel-up` provisions Grafana from `contrib/otel/grafana.yaml` (datasource
+ dashboard provider) and the JSON files under `contrib/otel/dashboards/`, so
three dashboards are there the moment Grafana is up — no manual import.
`make otel-grafana` opens the first one; the others are in the dashboard list.

- **OpenStack Tester — Overview** (`ostester-overview`) — the landing page:
  iteration success ratio, iterations by outcome, iteration-duration
  percentiles, operations attempted/succeeded/failed, and the non-success share
  of all API calls.
- **API operations** (`ostester-api-operations`) — the report's
  per-type table over time, covering every monitored service: per-kind p95 and
  mean latency, a latency heatmap, throughput, error+timeout rate, an
  ops-by-outcome table, and an errors-by-error-kind timeseries (from the
  dedicated errors counter, empty on a healthy run). An extra `operation`
  variable filters to `create`/`delete`/… .
- **Resource readiness** (`ostester-time-to-ready`) — time-to-ready
  percentiles by kind, a per-kind readiness success ratio, and a time-to-ready
  heatmap.

All three carry `cloud` and `scenario` template variables; the overview and
API-operations dashboards additionally carry a `service` variable
(`neutron` | `cinder`) so their panels can be filtered to one service when both
monitors feed the same backend. They respect what
the OTLP data actually carries: **percentiles are estimated from the histogram
buckets** and labelled `p95 (est.)`, not true maxima; there are **no true
per-run min/max series** (OTLP histogram min/max are not mapped), so
distribution shape is shown with **heatmaps** over the `_bucket` series; and the
**outcome panels split by `outcome`** (`success`/`error`/`timeout`) from
`operation.duration`, while the **errors-by-kind panel** uses the dedicated
`operation.errors` counter for the `error_kind` breakdown — empty on a healthy
run, since only failed operations create those series.

> **Sampling cadence.** With the default continuous monitor
> (`MONITOR_INTERVAL=0`) iterations run back-to-back, so the metrics flow
> steadily while it runs. On a paced run (`MONITOR_INTERVAL` set, e.g. `5m`) the
> metrics only move while an iteration runs. The panels use `$__rate_interval`
> with a 1 m minimum interval so short ranges don't render gaps as zeros; the
> views are meaningful from a handful of iterations upward.

> **Recreating a pre-Grafana cluster.** kind cannot add the host port 3000
> mapping to a running cluster, so a cluster booted before Grafana was added
> won't work. `make otel-up` detects this and asks you to
> `make otel-down && make otel-up` to recreate it.

**Editing the dashboards.** They are maintained via the UI-export roundtrip:
edit in Grafana, export the dashboard JSON, and commit it under
`contrib/otel/dashboards/`; `make otel-up` re-provisions it (the ConfigMap's
content hash changes, which rolls the Grafana pod). Keep the stable `uid` on
each dashboard so permalinks and exports stay reproducible. In-UI edits that
aren't exported are throwaway — Grafana's state is an `emptyDir`, gone at
`make otel-down`.

#### Cookbook queries

The cookbook above works against this stack unchanged. A few queries to paste
into VMUI:

```promql
# p95 create latency per resource kind
histogram_quantile(0.95, sum(rate(openstack_tester_operation_duration_seconds_bucket{operation="create"}[15m])) by (kind, le))

# operation error+timeout rate
sum(rate(openstack_tester_operation_duration_seconds_count{outcome!="success"}[15m]))
  / sum(rate(openstack_tester_operation_duration_seconds_count[15m]))

# p95 time-to-ready per kind
histogram_quantile(0.95, sum(rate(openstack_tester_resource_time_to_ready_seconds_bucket[15m])) by (kind, le))

# iteration success ratio
sum(rate(openstack_tester_iterations_total{outcome="success"}[1h]))
  / sum(rate(openstack_tester_iterations_total[1h]))
```

**Teardown.** `make otel-down` deletes the kind cluster and everything in it —
VictoriaMetrics with all stored metrics, and Grafana with its throwaway UI
state — nothing is left behind on the host.

---

## 10. Execution model

- **Dependency-ordered**: resources are created in topological order and removed
  in reverse. Independent resources of the same kind are created concurrently.
- **Concurrency**: a configurable worker pool (`--concurrency`) bounds parallel
  API calls. `context.Context` carries cancellation and per-operation timeouts.
- **Retry/backoff**: transient errors (5xx, 409 conflicts, rate limiting) are
  retried with exponential backoff; quota errors fail fast with a clear message.
- **Tagging**: every created resource is tagged with a run identifier (e.g.
  `ostester:run=<id>` plus type/index tags). Cleanup operates strictly on these
  tags, so it never touches resources the tool did not create. Tagging address
  scopes is best-effort — some Neutron releases return 404 for it — so a tag
  failure there is logged and tolerated (and left out of the metrics); those
  resources are instead reclaimed at cleanup from the run record, by id.
- **Naming**: deterministic names like `ostester-<id>-net-0001` for easy
  identification in Horizon / the CLI.
- **Progress output**: long-running commands are not silent between their start
  and their final summary. `apply`, `chaos`, and `cleanup` log a line per
  operation to stderr — each created resource, each scheduled churn create /
  delete, each teardown delete — plus a periodic one-line heartbeat with the
  cumulative op count, the rate since the last tick, the ok/failed split, and
  elapsed time. All of it is at `info`, so `--log-level warn` silences it while
  keeping warnings and errors; the final metrics summary and run-record path
  still go to stdout regardless.

The **churn / soak mode** (`chaos`) reuses all of the above and adds a
single-threaded, seeded **scheduler** over the plan: each tick it draws a random
delay and a random fan-out, then picks valid create/delete actions and dispatches
them through the same bounded worker pool and retry/backoff. The *decision*
sequence (timings, fan-out, create-vs-delete picks, which resource) is fully
deterministic for a given `scenario + seed + chaos settings`, even though the
concurrent cloud-call completion order is not; a problematic run can be replayed
exactly. The plan is a hard **ceiling**: the engine only creates planned
resources whose parents already exist and only deletes resources whose dependents
are already gone, so it never issues a dependency-violating operation of its own
making and the live population never exceeds the scenario. A **controller** keeps
the population oscillating inside the envelope rather than draining to empty or
pinning to the ceiling: each action's create probability is
`clamp(churn_ratio + (target_fill − current_fill), 0, 1)`, so `churn_ratio` is
the neutral bias at equilibrium and `target_fill` pulls the population toward its
level. By default a clean run tears the topology down by tag at the end;
`--no-cleanup` opts out, and an interrupt (Ctrl-C / SIGTERM) always leaves the
resources in place for an explicit `cleanup --run <id>` so an interrupt-to-inspect
never destroys the topology.

---

## 11. Quotas & prerequisites

Large scenarios will exceed Neutron's **default per-project quotas** (typically
10 networks, 10 subnets, 10 routers, 10 security groups, 100 SG rules, 50 ports).
A 100-network / 200-subnet / 20-router scenario therefore requires quotas to be
raised first.

This is resolved as **document-and-require** (see open questions): `apply`
**pre-checks quotas** against the expanded plan and aborts early with an itemized
message before creating anything if they are insufficient, leaving the operator
to raise the quotas. The pre-check accounts for the ports a subnet router
interface and an external gateway each consume, and — when an external network
is available — the floating IPs against their own quota. The tool does **not** auto-raise quotas through an admin
cloud — that would require admin credentials it otherwise never needs. The
pre-check fails open (it logs a warning and proceeds) when the project cannot
read its own quota, with the executor's quota fast-fail as the backstop.

---

## 12. Safety

- Operates only within the project of the selected `clouds.yaml` entry.
- `cleanup` deletes **only** resources from a known run — tag-matched, plus
  address scopes reclaimed from that run's record by id.
- `--dry-run` for `apply` to preview without creating anything.
- No destructive defaults; the cloud and project must be chosen explicitly.

---

## 13. Tech stack

- **Go 1.26.4**
- **[gophercloud v2](https://github.com/gophercloud/gophercloud)** —
  `github.com/gophercloud/gophercloud/v2` and its
  `openstack/networking/v2/*` packages (`networks`, `subnets`, `subnetpools`,
  `routers`, `ports`, `floatingips`, `external`, `security/groups`,
  `security/rules`, `attributestags`).
- `clouds.yaml` loading via
  `github.com/gophercloud/gophercloud/v2/openstack/config` +
  `.../openstack/config/clouds`.
- CLI: `github.com/spf13/cobra` (subcommands).
- Scenario files in **YAML**; run records / metrics in **JSON**.

## 14. Planned project layout

```
contrib/openstack-tester/
├── README.md                 # this file (only this exists today)
├── go.mod
├── cmd/
│   └── openstack-tester/
│       └── main.go
├── internal/
│   ├── config/               # clouds.yaml + run configuration
│   ├── resource/             # shared created-resource identity (kind/name/id)
│   ├── scenario/             # Neutron scenario types + deterministic generator
│   ├── plan/                 # expanded Neutron plan model (expected state)
│   ├── neutron/              # gophercloud wrappers, one file per resource type
│   ├── executor/             # dependency-ordered apply, worker pool, retry
│   ├── chaos/                # random churn/soak engine over the plan envelope
│   ├── cinder/               # Cinder client + plan/scenario/executor subpackages
│   ├── metrics/              # timing collection + reporting
│   ├── run/                  # run-record persistence
│   └── verify/               # Phase 2: OVN/OVS reconciliation (stub for now)
└── scenarios/                # built-in profiles
    ├── neutron/              #   small / medium / large
    └── cinder/               #   small / medium / large
```

## 15. Cinder (block storage)

The first slice beyond Neutron exercises **Cinder** through a sibling `cinder`
command namespace that reuses the same plan → apply → run-record →
report/status/cleanup machinery. It covers exactly three operations, mirroring
how Neutron started:

1. **Create volumes** — blank volumes, no image source, no attachments.
2. **Resize (extend) volumes** — a configurable fraction of the created volumes
   is grown by a random amount (the tool's first mutating action on an existing
   resource).
3. **Create snapshots** of the volumes.

Attachments (Nova), boot-from-volume, image-backed volumes, backups, clones,
transfers, restore-from-snapshot, and retype/migration are **out of scope** for
this slice. `cinder monitor` (the unattended loop driver, below) and `cinder
chaos` (the churn/soak driver, below) are now implemented.

### Commands

```
openstack-tester cinder generate   --scenario scenarios/cinder/small.yaml [--out plan.json]
openstack-tester cinder apply      --scenario scenarios/cinder/small.yaml [--dry-run] [--volume-type <name>]
openstack-tester cinder chaos      --scenario scenarios/cinder/small.yaml [--duration 30m] [--volume-type <name>] [--resize-ratio 0.3] [--no-cleanup] [--otel]
openstack-tester cinder monitor    --scenario scenarios/cinder/small.yaml [--interval 15m] [--iterations n] [--error-wait 2m] [--volume-type <name>] [--keep-run-records] [--otel]
openstack-tester cinder status     --run run-<id>.json
openstack-tester cinder report     --run run-<id>.json [--format table|json|csv|html]
openstack-tester cinder cleanup    --run run-<id>.json   # or --run-id <id>
```

`apply` runs three strictly ordered stages: create every volume and wait for
`available`; extend the volumes with a resize target (`extending` → `available`)
so only `available` volumes are extended; then snapshot the volumes. Snapshots
of the **same** volume are created strictly one after another, while snapshots
of **different** volumes run concurrently up to `--concurrency` — some backends
reject a snapshot while the source volume is still `snapshotting`, so per-volume
serialization is the robust default without giving up cross-volume throughput.
`--dry-run` prints the plan summary (volumes, resized volumes, snapshots, total
GiB) without touching the cloud.

### Scenario schema

Cinder has its **own** scenario schema (own `--set` keys), so a typo in either
service's scenario keeps failing loudly. Fixed counts live under `resources`,
ranges and ratios under `distribution`, and the run is deterministic via `seed`:

```yaml
name: small
seed: 42

resources:
  volumes: 5

distribution:
  volume_size_gib:      { min: 1, max: 5 }   # initial size drawn per volume
  volume_resized_ratio: 0.5                  # fraction of volumes to extend after creation
  resize_growth_gib:    { min: 1, max: 4 }   # extend delta drawn per resized volume
  snapshots_per_volume: { min: 0, max: 2 }   # drawn per volume
```

The resize intent lives **in the plan** (not decided at apply time), so the same
scenario + seed always yields the same volumes, the same resize targets, and the
same snapshot fan-out.

Three profiles ship under `scenarios/cinder/`:

| Profile | Volumes | Size (GiB) | Resized ratio | Growth (GiB) | Snapshots/volume | Chaos duration |
|---|---|---|---|---|---|---|
| `small`  | 5  | 1–5  | 0.5 | 1–4 | 0–2 | 5m |
| `medium` | 20 | 1–10 | 0.5 | 1–8 | 0–3 | 30m |
| `large`  | 50 | 1–10 | 0.6 | 1–8 | 1–4 | 1h |

The `small` profile is deliberately sized to fit Cinder's common default quotas
of 10 volumes / 10 snapshots / 1000 GiB, so it runs against a fresh project with
nothing raised. Every profile also ships a `chaos:` block, so `cinder chaos`
runs each one straight away with no extra flags (see below).

### Churn / soak mode (`cinder chaos`)

`cinder chaos` is the block-storage companion to
[`neutron chaos`](#churn--soak-mode-chaos): instead of building the volumes and
snapshots once, it reuses the **same churn engine** to keep creating and
deleting them at random, seeded intervals within the scenario envelope for a
configured duration, producing time-resolved latency/error buckets — the steady-
state churn under which block-storage backends (scheduler pressure, backend
garbage collection, snapshot chains) tend to degrade. The temporal knobs
(`duration`, `interval`, `parallel`, `churn_ratio`, `target_fill`) live in a
`chaos:` block in the Cinder scenario or on the CLI (flags override the block),
exactly as for Neutron.

- **What churns.** Volumes have no parents; snapshots are parented on their
  source volume. The engine's parents-must-outlive-children invariant gives the
  right lifecycle for free: a snapshot is only created while its volume lives, a
  volume with live snapshots is never a delete candidate, and snapshots churn
  faster than their volumes. Snapshots of the **same** volume stay serialized,
  as in `apply`.
- **Readiness is part of the operation.** A `creating` volume can be neither
  snapshotted, extended, nor reliably deleted, so create and extend operations
  complete only when the resource reaches `available` (recording time-to-ready);
  `error` / `error_extending` surface as **failed operations** in the stats, not
  as engine wedges. A delete completes only once the resource is gone.
- **Extend as a mutation (`resize_ratio`).** Each churn step draws, with
  probability `resize_ratio` (a new chaos-block key, default `0.3`, overridable
  with `--resize-ratio`; `0` disables it), a **mutation** of a live, not-yet-
  resized volume that has a planned resize target — extending it to its plan's
  `resizeToGiB`. A volume instance is extended **at most once per lifetime**,
  always to its planned target; deleted and re-created, it may be extended again.
  This keeps a week-long soak's gigabytes consumption inside the envelope the
  quota pre-check validated (Σ planned final sizes) instead of growing without
  bound. Mutations do not change the population, so the `churn_ratio` /
  `target_fill` controller economics are untouched, and a mutation counts against
  the per-tick fan-out (`parallel.max`) like any other API call.
- **Outputs.** Same as Neutron chaos: a deterministic decision schedule per seed
  and config, a run record carrying `ChaosStats` (creates / deletes / **mutates**
  / cycles, population summary, time buckets), and OTEL export of every operation
  — extends appear as `operation=extend` samples. On a clean finish it tears the
  volumes and snapshots down by metadata (snapshots before volumes) and runs a
  leak check; Ctrl-C / SIGTERM or `--no-cleanup` leaves them in place for an
  explicit `cinder cleanup --run-id <id>` (metadata discovery reclaims a crashed
  or interrupted run in full).

```
openstack-tester cinder chaos --scenario scenarios/cinder/small.yaml            # 5m churn, no flags needed
openstack-tester cinder chaos --scenario scenarios/cinder/medium.yaml --duration 2m --resize-ratio 0.5
```

### `--volume-type`

Like Neutron's external network, the volume type is a property of the target
cloud, not of the cloud-independent plan: it is resolved at apply time and
applied to every volume create. Unset means the cloud's default type; a named
type that does not exist is an error. The chosen type is recorded in the run
record for provenance.

### Quotas

The same document-and-require policy as Neutron (read-only, fail-open on 403,
never auto-raise): before creating anything, `apply` reads the project's Cinder
quotas and aborts an oversized plan with an itemized message covering `volumes`
(count of planned volumes), `snapshots` (count of planned snapshots), and
`gigabytes` (Σ final volume sizes after resize + Σ snapshot sizes at the source
volume's final size, since both count against the shared gigabytes quota). When
`--volume-type` is set, the per-type quotas (`volumes_<type>`, `snapshots_<type>`,
`gigabytes_<type>`) are checked too. Cinder rejects an over-quota request with
HTTP 413, which the executor's fast-fail handles as a backstop.

### Identification and cleanup

Cinder has no Neutron-style tag API, so run identity lives in **volume/snapshot
metadata**: every resource is created with `ostester:run=<id>` and
`ostester:type=<kind>`, plus the same deterministic `ostester-<runid>-<logical>`
name. `cleanup` discovers a run's resources by that metadata, with the run
record's created list as a belt-and-suspenders fallback, and deletes in reverse
dependency order — **snapshots first, then volumes** (a volume with snapshots
cannot be deleted). 404s count as success, so cleanup is idempotent and never
touches resources without the run's metadata.

### Monitoring (`cinder monitor`)

`cinder monitor` re-runs the block-storage pipeline continuously by default or
on a fixed cadence, unattended, for days or weeks — the same loop as
[`neutron monitor`](#monitoring-over-time-neutron-monitor), so `--interval`
(default `0` = continuous), `--iterations`, `--error-wait`, and the fixed-seed,
graceful-shutdown, and minimum-failure-backoff semantics are identical. Each
iteration is a pre-flight sweep → `apply` → `cleanup` with a fresh run id and a
fresh Keystone auth (so token expiry over a multi-day loop fails one iteration
rather than dead-looping). The volume type is resolved and the Cinder quotas are
pre-checked once at startup, so a misconfiguration fails fast before the loop
begins.

- **Self-healing pre-flight sweep.** Before each iteration the loop reclaims
  leftover `ostester`-metadata resources so a previous crashed or interrupted
  iteration cannot accumulate. Discovery is by the `ostester:type=<kind>`
  **metadata** (the metadata analog of Neutron's type tag), so it reclaims
  **any** tester-created volume/snapshot in the project, snapshots before
  volumes. **Do not run `cinder monitor` concurrently with another
  `apply`/`monitor` run in the same project** — the sweep would tear the other
  run down mid-flight.
- **Bounded teardown.** Every cleanup operation is bounded by `--timeout`, so a
  wedged Cinder call bounds one operation, not the whole loop.
- **`--otel`.** Volume/snapshot operation histograms and the per-iteration
  instruments land in the existing schema with `service=cinder`; see
  [§9](#opentelemetry-export---otel).

## 16. Roadmap

1. **Phase 1 — API load & timing**
   - [ ] Scaffold module, CLI, `clouds.yaml` auth.
   - [ ] Scenario schema + deterministic generator (seeded).
   - [ ] `generate` (plan dump) + `--dry-run`.
   - [x] Neutron resource wrappers (pools, networks, subnets, routers,
         interfaces, security groups + rules, ports) with tagging.
   - [x] Dependency-ordered, concurrent executor with retry/backoff.
   - [x] Metrics collection and state polling.
   - [x] Run records, `status` re-query, and `report` (table/JSON/CSV).
         (Prometheus textfile export still pending.)
   - [x] Tag-based `cleanup`; quota pre-check.
   - [x] Built-in profiles (incl. the 20/100/200 example).
   - [x] Random churn / soak mode (`chaos`): continuous seeded create/delete
         within the scenario envelope for a configured duration.
   - [x] Periodic monitor mode (`monitor`) with OpenTelemetry (OTLP) metrics
         export, for observing a single installation over time.
2. **Phase 2 — data-plane verification**
   - [ ] Compare API/plan against OVN NB/SB and OVS flows.
3. **Phase 3+** — external connectivity, trunk ports, RBAC, QoS, more profiles,
   other services.
4. **Beyond Neutron**
   - [x] Cinder first slice (`cinder` namespace): create / resize / snapshot
         volumes with `small`/`medium`/`large` profiles (see §15).
   - [x] `cinder monitor` (#31): unattended sweep→apply→cleanup loop with OTLP
         export (`service=cinder`).
   - [x] `cinder chaos` (#32): seeded volume/snapshot churn with extend
         mutations within the scenario envelope, reusing the Neutron churn engine.

## 17. Open questions / decisions to confirm

- **Quotas**: **resolved** — document-and-require. `apply` pre-checks the
  expanded plan against the project quota and aborts early with an itemized
  message; raising the quota is the operator's step. Auto-raise via an admin
  cloud is deliberately not implemented (see §11).
- **Network types**: **resolved** — geneve/vxlan tenant networks only; the
  generator emits plain tenant networks with no provider attributes (VLAN/flat
  deferred to Phase 3).
- **IPv6**: **resolved** — dual-stack subnets are emitted in Phase 1,
  controlled by `distribution.ipv6_ratio` (set it to 0 for IPv4-only).
- **External connectivity**: skip gateways/FIPs in Phase 1, or wire them up if
  an external network is configured?
- **CLI framework**: **resolved** — `cobra`.
- **Module path**: **resolved** — `github.com/B42Labs/openstack-tester` (the
  module lives at the repository root, not under `contrib/`).
```
