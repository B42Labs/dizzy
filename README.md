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
> Phase 1 item.

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
| **later** | Extend beyond Neutron (Cinder, Nova, …) — hence the generic name `openstack-tester`. | Idea |

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
(`scenarios/medium.yaml`):

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

Three ready-to-use profiles ship under `scenarios/`, selected by passing the
file path to `--scenario`:

| Profile | Networks | Routers | Subnets | Notes |
|---------|----------|---------|---------|-------|
| `small`  | 3   | 2  | ≤ 9  | Fits Neutron's default per-project quotas. |
| `medium` | 100 | 20 | ~200 | The headline example above; needs raised quotas. |
| `large`  | 200 | 40 | ~400 | Twice the headline; needs raised quotas, guarded by the `apply` quota pre-check. |

```
openstack-tester neutron generate  --scenario scenarios/medium.yaml [--out plan.json]
openstack-tester neutron apply     --scenario scenarios/large.yaml  [--dry-run]
```

---

## 7. CLI design

A single binary `openstack-tester` with subcommands (Neutron grouped under a
`neutron` namespace to leave room for other services later):

```
openstack-tester neutron generate  --scenario medium.yaml [--out plan.json]
openstack-tester neutron apply     --scenario medium.yaml [--dry-run]
openstack-tester neutron chaos     --scenario medium.yaml --duration 30m
openstack-tester neutron status    --run run-<id>.json
openstack-tester neutron report    --run run-<id>.json [--format table|json|csv]
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
  flags override the block. With `--seed` fixed (and identical settings) the
  whole action schedule is reproducible. On a clean finish it tears the topology
  down by tag and runs a leak check; Ctrl-C / SIGTERM leaves the resources in
  place for an explicit `cleanup --run <id>`.
- `status` — re-query the current state of a run's resources from the API.
- `report` — render metrics from a run record (table / JSON / CSV).
- `cleanup` — delete all resources belonging to a run, in reverse dependency
  order; idempotent. Tag-discoverable resources are found by the run tag; address
  scopes (which Neutron may not let us tag) are reclaimed from the run record by
  id, so reclaiming them needs `--run`, not a bare `--run-id`.
- `verify` — (Phase 2) compare run/plan against OVN/OVS.

Global flags: `--os-cloud` (defaults to `$OS_CLOUD`), `--concurrency`,
`--timeout`, `--seed` (override scenario seed), `--log-level`.

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

`report` renders a run record's metrics in three formats:

- human-readable **table** on stdout (the default),
- **JSON** metrics (machine-readable),
- **CSV** with one row per resource type plus an overall row.

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
│   ├── scenario/             # scenario types + deterministic generator (seeded)
│   ├── plan/                 # expanded plan model (expected state)
│   ├── neutron/              # gophercloud wrappers, one file per resource type
│   ├── executor/             # dependency-ordered apply, worker pool, retry
│   ├── chaos/                # random churn/soak engine over the plan envelope
│   ├── metrics/              # timing collection + reporting
│   ├── run/                  # run-record persistence
│   └── verify/               # Phase 2: OVN/OVS reconciliation (stub for now)
└── scenarios/                # built-in profiles: small / medium / large
```

## 15. Roadmap

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
2. **Phase 2 — data-plane verification**
   - [ ] Compare API/plan against OVN NB/SB and OVS flows.
3. **Phase 3+** — external connectivity, trunk ports, RBAC, QoS, more profiles,
   other services.

## 16. Open questions / decisions to confirm

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
