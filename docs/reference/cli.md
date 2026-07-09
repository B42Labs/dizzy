# CLI reference

A single binary, `dizzy`, with one command namespace per OpenStack service:
`neutron`, `cinder`, and `keystone`. Each namespace offers the same verbs.

| Verb | Touches the API | Purpose |
|---|---|---|
| `generate` | no | Expand a scenario into a plan and dump it |
| `apply` | yes | Create the plan's resources, record a run |
| `chaos` | yes | Continuous randomized create/delete churn within the scenario envelope |
| `monitor` | yes | Repeat `apply` → `cleanup` unattended, exporting metrics |
| `status` | yes | Re-query the current state of a run's resources |
| `report` | no | Render metrics from a run record |
| `cleanup` | yes | Delete a run's resources |

`neutron` additionally has `list-networks` (an auth smoke test) and `verify` (a
stub that returns "not implemented yet").

## Global flags

Accepted by every command.

| Flag | Default | Description |
|---|---|---|
| `--os-cloud <name>` | `$OS_CLOUD` | Cloud name in `clouds.yaml` |
| `--concurrency <n>` | `8` | Maximum number of parallel API calls |
| `--timeout <duration>` | `1m` | Per-operation timeout |
| `--seed <int>` | — | Override the scenario's RNG seed |
| `--log-level <level>` | `info` | `debug`, `info`, `warn`, or `error` |
| `--otel` | off | Export metrics via OpenTelemetry OTLP |
| `--version` / `-v` | — | Print the version and exit |

`--otel` is the *only* thing that enables export. The standard
`OTEL_EXPORTER_OTLP_*` environment variables configure the exporter but never
switch it on, so a globally exported `OTEL_EXPORTER_OTLP_ENDPOINT` does not
change the behavior of a command run without `--otel`. See
[Metrics](metrics.md).

Progress output from `apply`, `chaos`, and `cleanup` goes to stderr at `info`
level, so `--log-level warn` silences it while keeping warnings and errors. The
metrics summary and the run-record path always go to stdout.

## Scenario flags

Accepted by `generate`, `apply`, `chaos`, and `monitor` in every namespace.

| Flag | Description |
|---|---|
| `--scenario <path>` | Path to the scenario YAML file (**required**) |
| `--set <key>=<value>` | Override one scenario value; repeatable |

`--set` takes a dotted path into the scenario schema, e.g.
`--set resources.networks=200`. An unknown key is an error:

```console
$ dizzy neutron apply --scenario scenarios/neutron/small.yaml --set resources.bogus=1
error: unknown override key "resources.bogus"
```

Each service has its own schema and therefore its own `--set` keys, so a typo in
one service's scenario keeps failing loudly rather than being silently accepted
by another. See [Scenario schema](scenario-schema.md).

Note that some counts are not the whole story: `--set resources.networks=10` on a
scenario with `router_links: 1` yields **11** networks, because each router link
adds its own dedicated transit network.

## `generate`

Expands a scenario into a plan and writes it as JSON. Never touches the API.

| Flag | Description |
|---|---|
| `--out <path>` | Write the plan to this file instead of stdout |

```console
$ dizzy neutron generate --scenario scenarios/neutron/small.yaml --out plan.json
```

## `apply`

Creates the plan's resources in dependency order, polls their states, and writes
a `run-<id>.json` run record.

Common flags:

| Flag | Description |
|---|---|
| `--dry-run` | Validate the scenario and print the plan summary without making API calls |
| `--keep-on-abort` | On interrupt, leave created resources in place and print the cleanup hint instead of tearing them down |

Namespace-specific flags:

| Flag | Namespace | Description |
|---|---|---|
| `--external-network <name>` | `neutron` | External network for gateways and floating IPs (default: auto-detect the first one) |
| `--volume-type <name>` | `cinder` | Volume type to create volumes with (default: the cloud's default type) |
| `--privilege auto\|admin\|domain-manager` | `keystone` | Privilege tier; default `auto` (detect) |
| `--domain <name>` | `keystone` | In-scope domain for domain-manager mode (default: the token's domain; ignored in admin mode) |
| `--roles <csv>` | `keystone` | Existing roles to reuse in domain-manager mode (default `member,reader`; ignored in admin mode) |

`neutron apply` and `cinder apply` run a read-only **quota pre-check** against
the expanded plan and abort with an itemized message before creating anything if
the quotas are insufficient. `keystone apply` runs a read-only **privilege
pre-check** instead, since Keystone has no default per-resource quotas. See
[Deal with the quota pre-check](../how-to/raise-quotas.md) and
[Keystone's privilege model](../explanation/privilege-model.md).

**On interrupt.** SIGINT/SIGTERM writes the run record first, then tears the
partial topology down in reverse dependency order, logs the deletion count, and
exits non-zero naming the run id. A second signal aborts hard, leaving the record
for a manual `cleanup`. `--keep-on-abort` skips the teardown. A *successful*
apply always keeps its resources — that is the point of the run record.

## `chaos`

Uses the scenario as a spatial **envelope** rather than a target: for the whole
duration it creates *and* deletes planned resources at random, seeded intervals,
so the live population never exceeds the scenario's counts and only planned
resources are ever created. See [The churn engine](../explanation/churn-engine.md).

Every flag can instead be set in a `chaos:` block in the scenario YAML; flags
override the block. All three built-in profiles ship such a block, so `chaos`
runs them with no extra flags.

| Flag | Default | Description |
|---|---|---|
| `--duration <duration>` | — | Total wall-clock runtime (required, via flag or the `chaos:` block) |
| `--min-interval <duration>` | `200ms` | Minimum random delay between scheduled actions |
| `--max-interval <duration>` | `3s` | Maximum random delay between scheduled actions |
| `--max-parallel <n>` | `--concurrency` | Maximum concurrent in-flight churn operations |
| `--churn-ratio <float>` | `0.5` | Create bias at steady state, 0–1 |
| `--target-fill <float>` | `0.8` | Fraction of the envelope to keep populated on average, 0–1 |
| `--no-cleanup` | off | Leave resources in place at the end of the run *and* on interrupt |

Namespace-specific:

| Flag | Namespace | Default | Description |
|---|---|---|---|
| `--external-network <name>` | `neutron` | auto-detect | As for `apply` |
| `--volume-type <name>` | `cinder` | cloud default | As for `apply` |
| `--resize-ratio <float>` | `cinder` | `0.3` | Probability per churn step of extending a live, not-yet-resized volume to its planned target; `0` disables |
| `--token-ratio <float>` | `keystone` | `0.3` | Probability per churn step of issuing a token as a live, assigned user; `0` disables |
| `--privilege`, `--domain`, `--roles` | `keystone` | | As for `apply` |

By default a churn run tears its resources down at the end **or when
interrupted**, then runs a leak check. `--no-cleanup` is the single opt-out,
leaving them for an explicit `cleanup`.

## `monitor`

Repeats the single-shot pipeline — pre-flight sweep → `apply` → `cleanup` —
unattended, so one installation can be observed over time.

| Flag | Default | Description |
|---|---|---|
| `--interval <duration>` | `0` | Target cadence between iteration *starts*; `0` runs iterations back-to-back |
| `--iterations <n>` | `0` | Stop after this many iterations; `0` runs forever |
| `--error-wait <duration>` | `0` | Extra pause after a failed iteration; `0` is off |
| `--keep-run-records` | off | Write a `run-<id>.json` per iteration |

Namespace-specific:

| Flag | Namespace | Description |
|---|---|---|
| `--external-network <name>` | `neutron` | As for `apply` |
| `--volume-type <name>` | `cinder` | As for `apply` |
| `--reclaim-orphans` | `keystone` | Before each iteration, delete leftover `dizzy-` identity resources across **all** tester runs cloud-wide (off by default) |
| `--privilege`, `--domain`, `--roles` | `keystone` | As for `apply` |

With `--interval` omitted or `0`, the next iteration starts the moment the
previous one finishes. A positive `--interval` is the target time between
iteration starts: a fast iteration waits out the remainder, one that overruns
starts the next immediately. Either way iterations never overlap and no backlog
builds up.

Run records are **off by default** because in a long-running loop they accumulate
unboundedly; `--otel` is the intended way to keep the data.

The plan is expanded once at startup, so every iteration reuses the same seed and
therefore the same topology — that is what makes latency trends comparable
across iterations. To broaden coverage, run several monitors with different
`--seed` values.

> **Do not run `monitor` concurrently with another `dizzy` run in the same
> project.** For `neutron` and `cinder` the pre-flight sweep is always on and
> reclaims any tester-created resource in the project, so it would tear a
> concurrent run down mid-flight. For `keystone` the equivalent sweep is opt-in
> via `--reclaim-orphans`, and because an admin token lists cloud-wide it is only
> safe when no other `dizzy` process targets the cloud at all.

**On interrupt.** SIGINT/SIGTERM stops the loop, tears the current iteration
down on a context that survives the signal, flushes the metrics exporter, and
exits. A second signal aborts hard.

## `status`

Re-queries the current state of a run's resources from the API.

| Flag | Description |
|---|---|
| `--run <path>` | Path to the run record to re-query (**required**) |

## `report`

Renders metrics from a run record. Never touches the API. The same command
builder backs all three namespaces, so `dizzy cinder report` and
`dizzy neutron report` are the same code.

| Flag | Default | Description |
|---|---|---|
| `--run <path>` | — | Path to the run record to report on (**required**) |
| `--format <fmt>` | `table` | `table`, `json`, `csv`, or `html` |

- **table** — human-readable, one row per resource kind plus an overall row.
- **json** — the aggregate metrics, machine-readable.
- **csv** — one row per resource kind plus an overall row.
- **html** — a self-contained, offline report with inline SVG charts for
  latency, throughput, and error rates; for a churn run, also the per-bucket
  degradation over time.

## `cleanup`

Deletes a run's resources in reverse dependency order. Idempotent: a 404 counts
as success, so running it twice is harmless.

| Flag | Description |
|---|---|
| `--run <path>` | Path to the run record whose resources to delete |
| `--run-id <id>` | Delete resources for this run id directly, without a record |

Discovery differs per service, and so does what `--run-id` alone can reach:

- **neutron** — resources are found by the `dizzy:run=<id>` tag. Address scopes
  are the exception: some Neutron releases refuse to tag them, so they are
  reclaimed from the run record by id. Removing them therefore needs `--run`,
  not a bare `--run-id`.
- **cinder** — volumes and snapshots are found by their `dizzy:run=<id>`
  metadata, with the run record's created list as a fallback. Snapshots are
  deleted before their volumes.
- **keystone** — projects are found by tag (falling back to the name prefix);
  domains, users, and roles by the `dizzy-<runid>-` name prefix. Role
  assignments are best reclaimed *with* a record, which is their authoritative
  handle.

See [Resource identity and cleanup](../explanation/resource-identity.md).

## `list-networks`

Lists the project's networks. A working auth and connectivity smoke test with no
side effects. `neutron` namespace only; takes no flags beyond the global ones.

## `verify`

A stub. Returns `not implemented yet`. Reserved for reconciling a run against the
OVN northbound/southbound databases and OVS flows.
