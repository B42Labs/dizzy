# CLI reference

A single binary, `dizzy`, with one command namespace per OpenStack service:
`neutron`, `cinder`, `keystone`, `nova`, and `glance`. Each namespace offers the
same verbs.

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

A sixth namespace, `mix`, runs several workload personas side by side in one
churn run. It has its own subset of the verbs; see
[The `mix` namespace](#the-mix-namespace).

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

`neutron apply`, `cinder apply`, and `nova apply` run a read-only **quota
pre-check** against the expanded plan and abort with an itemized message before
creating anything if the quotas are insufficient. `keystone apply` runs a
read-only **privilege pre-check** instead, since Keystone has no default
per-resource quotas. `glance apply` runs **no** pre-check: Glance exposes no
project-quota API (its image count and size caps are deployment config), so an
over-limit request surfaces as a fast-failed 413 during the run rather than being
caught up front. See [Deal with the quota pre-check](../how-to/raise-quotas.md)
and [Keystone's privilege model](../explanation/privilege-model.md).

`nova apply` takes no service-specific flags: the boot image, flavor, and resize
flavor are named in the scenario (`image`, `flavor`, `resize_flavor`) and
resolved to cloud ids at apply time — dizzy uploads no image and creates no
flavor. A name that does not exist fails with an actionable list of the
available images or flavors. Its compute quota pre-check counts instances,
cores, and RAM (see [raise-quotas](../how-to/raise-quotas.md)). If the plan
schedules any live migration, `nova apply` also runs a **fail-open
live-migration pre-check**: it needs the admin role and at least two usable
compute hosts, and when either is missing it skips live migration for the run
with a warning and continues — a missing admin capability never aborts a run.

`glance apply` takes no service-specific flags: every image is created by dizzy
with a synthetic, generated data payload of a configurable size, so a run
references no pre-existing image (`resources.images` and
`distribution.image_size_mib` shape it). Image import through web-download and
multi-store placement are out of scope: every image enters through the direct
data-upload path into the cloud's default store. A payload larger than the
default `--timeout` allows may need `--timeout` raised, since each upload is one
operation bounded by it.

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
override the block. Every built-in profile ships such a block, so `chaos` runs
them with no extra flags.

| Flag | Default | Description |
|---|---|---|
| `--duration <duration>` | — | Total wall-clock runtime (required, via flag or the `chaos:` block). `0` runs until SIGINT or SIGTERM; only the flag selects this, since `duration: 0` in the block means unset |
| `--bucket-width <duration>` | `1h` | Width of one time bucket in the series of a run with `--duration 0`, at least `1m`. A bounded run ignores it and keeps ten equal buckets |
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
| `--lifecycle-ratio <float>` | `nova` | `0.3` | Probability per churn step of mutating a live server (stop/start, resize, or live-migrate); `0` disables |
| `--lifecycle-ratio <float>` | `glance` | `0.3` | Probability per churn step of mutating a live image (deactivate/reactivate, visibility flip, member add/remove, or metadata churn); `0` disables |
| `--privilege`, `--domain`, `--roles` | `keystone` | | As for `apply` |

By default a churn run tears its resources down at the end **or when
interrupted**, then runs a leak check. `--no-cleanup` is the single opt-out,
leaving them for an explicit `cleanup`.

With `--duration 0` the signal is the normal end of the run. It writes the final
run record, tears down and runs the leak check as a bounded run does when its
duration elapses, and exits 0 when they succeed. With `--no-cleanup` it prints
`churn complete`.

While it runs, every churn run rewrites `run-<id>.json` once a minute, starting
one minute after the start, so a killed process or a lost node leaves a record
at most about a minute old. A run stopped by a signal while operations are still
in flight rewrites it once more before it waits for them. A record written this
way carries `"incomplete": true` and its `finishedAt` is the checkpoint time;
the final record omits the key. A checkpoint that cannot be written logs a
warning and the run goes on.

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
> project.** For `neutron`, `cinder`, `nova`, and `glance` the pre-flight sweep is
> always on and reclaims any tester-created resource in the project, so it would
> tear a concurrent run down mid-flight. For `keystone` the equivalent sweep is opt-in
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
builder backs all six namespaces, so `dizzy cinder report` and
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

A record a churn run wrote while it was still running (`"incomplete": true`)
renders in every format and is marked as follows:

- **table** — a first line `Run incomplete: checkpoint written at <time>`, with
  the checkpoint time in RFC 3339 UTC, then a blank line and the usual output.
- **json** — `"incomplete": true` next to `metrics` and `chaos`. A record without
  a `chaos` object stays the bare metrics object.
- **csv** — unchanged; the format has no place for a marker.
- **html** — a yellow `Run incomplete` banner with the checkpoint time instead of
  `Run completed`. A record that carries an error still shows `Run failed`.

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
- **nova** — servers and data volumes are found by their `dizzy:run=<id>`
  metadata; the companion networks, subnets, and ports by the `dizzy:run=<id>`
  tag. Servers are deleted first (so their attachments release), then ports,
  volumes, and networks (a network delete cascades its subnet). A
  boot-from-volume root volume carries no dizzy identity — it is delete-on-
  termination, so it dies with its server.
- **glance** — images are found by the `dizzy:run=<id>` Glance image tag, filtered
  server-side by the list API's `tag` parameter, with the run record's created
  list as a fallback. Images are a single kind with no ordering, so a bare
  `--run-id` reaches everything the run created.

See [Resource identity and cleanup](../explanation/resource-identity.md).

## The `mix` namespace

`mix` runs several workload personas side by side in one churn run. Each
persona has its own churn engine, its own cloud project and its own identity,
and the whole run shares one seed, one run id and one run record. This build
has three personas: `ci`, whose servers are short-lived; `gardener`, whose
servers form clusters in anti-affinity server groups and are replaced one
worker at a time; and `legacy`, whose servers stay until teardown and are
changed in place. Next to the personas, a run can churn single services as
background lanes: `cinder`, `glance`, `keystone` and `neutron`. A lane runs the
churn graph of that service's own `chaos` command in an engine of its own,
under the identity `<run-id>-<lane>`. Every lane is off unless the scenario's
`lanes:` block or `--set lanes.<name>.enabled=true` switches it on, and a run
still needs a persona with a server. See
[Combined runs](../explanation/combined-runs.md).

| Subcommand | Touches the API | Purpose |
|---|---|---|
| `mix generate` | no | Expand a mix scenario into a plan and dump it |
| `mix chaos` | yes | Run one churn engine per persona and per enabled lane |
| `mix status` | yes | Re-query the current state of a mix run's resources, per persona and lane |
| `mix report` | no | Render metrics from a run record, the shared `report` |
| `mix cleanup` | yes | Delete every persona's and lane's resources of a mix run |

There is no `mix apply` and no `mix monitor`: a persona is a behavior over
time, which a one-shot build does not have.

`--concurrency` and `--max-parallel` apply to each persona and each lane
separately. A run of three personas and two lanes can have up to five times
`--concurrency` API calls in flight.

`mix generate`, `mix chaos`, `mix status` and `mix cleanup` reject an opt-in
service under `services` that this build does not support, before they make
any API call:

```console
$ dizzy mix generate --scenario scenarios/mix/small.yaml --set services=octavia
error: opt-in service "octavia" is not supported by this build of dizzy (supported: none)
```

### `mix generate`

Expands a mix scenario into its plan and writes it as JSON: every persona with
at least one server, with its share, its servers, its seed and its compute
plan, and under `lanes` every enabled lane, with its seed and the plan of its
service. A lane's scenario comes from the service's bundled profile or from
the file the lane names. Never touches the API.

| Flag | Description |
|---|---|
| `--scenario <path>` | Path to the mix scenario YAML file (**required**) |
| `--set <key>=<value>` | Override one scenario value; repeatable |
| `--out <path>` | Write the plan to this file instead of stdout |

### `mix chaos`

Runs the combined churn. Each persona authenticates with the `clouds.yaml`
entry its scenario block names under `cloud`, or with `--os-cloud` when that
is empty, and churns its share of `resources.servers` under the identity
`<run-id>-<persona>`. Before anything is created, every persona resolves the
image and flavor and runs the compute quota pre-check against its own plan in
its own project. For the `gardener` persona, whose plan has server groups, the
pre-check also counts the groups against the `server_groups` limit and the
largest group against the `server_group_members` limit. When two personas or
lanes authenticate against the same project, the run logs
`lanes share a project; each quota pre-check saw only its own plan`, with the
names under `lanes`, and goes on.

A persona whose plan migrates servers, `legacy`, also runs the migration
pre-check in its own project. Live and cold migration need the admin role and
at least two usable compute hosts. Without them the persona logs two warnings,
each with the `reason` attribute, and runs without migrations:

```text
level=WARN msg="live migration disabled for this run" reason="credentials lack the admin role"
level=WARN msg="cold migration disabled for this run" reason="credentials lack the admin role"
```

After the personas, each enabled lane authenticates with the `clouds.yaml`
entry its block names under `cloud`, or with `--os-cloud` when that is empty,
and runs the read-only pre-checks of its service's `chaos` command against its
own plan: the volume type and the quota pre-check for `cinder`, the external
network and the quota pre-check for `neutron`, and the privilege pre-check for
`keystone`; `glance` has none. Nothing is created before every persona and
every lane has passed. A lane whose pre-check fails, or whose cloud is not in
`clouds.yaml`, ends the run before anything is created, with an error that
names the lane; a lane is never dropped from a run with a warning:

```console
$ dizzy mix chaos --scenario scenarios/mix/small.yaml --set lanes.keystone.enabled=true
error: lane "keystone": caller is neither cloud admin nor domain manager: token carries roles [member reader]; keystone needs the 'admin' role (any scope) or the 'manager' role on a domain-scoped token (use --privilege to override)
```

On a cloud without those rights, switch the lane off with
`--set lanes.keystone.enabled=false`, or bind it with
`--set lanes.keystone.privilege=domain-manager` and the lane's `domain` and
`roles`. Once every pre-check has passed, the `keystone` lane creates its
domain and role scaffold, or binds the existing domain and roles in
domain-manager mode, and only then do the engines start. When the scaffold
fails, the run deletes what it created and exits with
`provisioning lane "keystone" (run <run-id>-keystone): binding scaffold: …`.

| Flag | Default | Description |
|---|---|---|
| `--scenario <path>` | — | Path to the mix scenario YAML file (**required**) |
| `--set <key>=<value>` | — | Override one scenario value; repeatable |
| `--duration <duration>` | — | Total wall-clock runtime (required, via flag or the `chaos:` block). `0` runs until SIGINT or SIGTERM |
| `--bucket-width <duration>` | `1h` | Width of one time bucket in the series of a run with `--duration 0`, at least `1m` |
| `--max-parallel <n>` | `--concurrency` | Maximum concurrent in-flight churn operations of each persona and lane. Without it, a lane takes `parallel.max` from its own scenario |
| `--no-cleanup` | off | Leave every persona's and lane's resources in place at the end of the run *and* on interrupt |

There are no `--min-interval`, `--max-interval`, `--churn-ratio` or
`--target-fill` flags: each persona's block sets them, and `--set` overrides
them, e.g. `--set personas.ci.target_fill=0.8`. A lane takes them from its own
scenario's `chaos:` block; see [Lanes](scenario-schema.md#lanes). The run
writes and checkpoints one `run-<id>.json` the way `chaos` does, with a
per-persona and per-lane breakdown.

At the end it prints the overall metrics summary, then a table of the
personas, then a table of the lanes when the run has any, then the record
path:

```text
Personas
NAME      PROJECT                           SHARE  SERVERS  OPS   OK  FAILED    P50    P95    P99
ci        5c3f1e0a9b2d4e6f8a7b9c0d1e2f3a4b    50%        3  412  410       2  310ms   1.9s   3.2s
gardener  7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a    30%        2   64   63       1   1.8s  41.5s  58.3s
legacy    9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b    20%        1   96   95       1   2.4s  38.2s  52.1s

Lanes
NAME      PROJECT                           SCENARIO        OPS   OK  FAILED    P50    P95    P99
glance    5c3f1e0a9b2d4e6f8a7b9c0d1e2f3a4b  small/glance     88   88       0  420ms   2.1s   3.4s
keystone  2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e  small/keystone  240  239       1   95ms  310ms  480ms
run record written to run-1a2b3c4d.json
```

Teardown then deletes each persona's and lane's resources by its identity,
prints one line per persona and lane, and runs one leak check across all of
them:

```text
deleted 14 resource(s) for run 1a2b3c4d-ci
deleted 6 resource(s) for run 1a2b3c4d-gardener
deleted 5 resource(s) for run 1a2b3c4d-legacy
deleted 3 resource(s) for run 1a2b3c4d-glance
deleted 9 resource(s) for run 1a2b3c4d-keystone
leak check: no run-tagged resources remain
```

When resources remain, the last line is
`leak check: <n> run-tagged resource(s) still present after teardown`. A
persona or lane whose teardown fails does not stop the others; the command
then exits non-zero naming every failing one, as
`tearing down persona "ci" (run 1a2b3c4d-ci): …` or
`tearing down lane "keystone" (run 1a2b3c4d-keystone): …`, and prints no
leak-check line. A failing leak check reports
`leak check for lane "keystone": …` or `leak check for persona "ci": …`.

With `--no-cleanup` the resources stay in place and the command prints the
hint to reclaim them, `churn interrupted; …` after an interrupt:

```text
churn complete; resources left in place — reclaim with: mix cleanup --run run-1a2b3c4d.json
```

When the run record could not be written, the hint is
`mix cleanup --run-id <id> --scenario '<file>'` instead, followed by one
`--set '<key>=<value>'` for every `--set` the run was given. Either hint ends
with `--os-cloud '<name>'` when the run took its cloud from `--os-cloud` or
`$OS_CLOUD`, the cloud of every persona and lane whose block names none.
Values are single-quoted so the hint pastes into a POSIX shell unchanged.

### `mix status`

Re-queries the current state of a mix run's resources, each persona and lane
under the cloud and identity the record names for it. A persona or lane whose
cloud now authenticates against another project than the record's `projectID`
fails before anything is queried, because its resources would all show as
`gone`.

| Flag | Description |
|---|---|
| `--run <path>` | Path to the mix run record to re-query (**required**) |

For each persona it prints a heading `persona <name> (run <run-id>-<name>)`,
for each lane a heading `lane <name> (run <run-id>-<name>)`, and then the
status table of the resources that persona or lane created. It visits every
persona and lane and exits non-zero when any table failed, with
`re-querying <n> of <m> personas failed`, or
`re-querying <n> of <m> personas and lanes failed` for a record with lanes.

### `mix report`

The shared [`report`](#report). A mix record adds per-persona and per-lane
output in every format; see
[What `report` renders](metrics.md#what-report-renders).

### `mix cleanup`

Deletes every persona's and lane's resources of a mix run, each by its
identity, `<run-id>-<persona>` or `<run-id>-<lane>`, and in the project of the
cloud it ran under. A persona follows the `nova` discovery rules of
[`cleanup`](#cleanup). Server groups carry neither metadata nor tags, so a
persona's server groups are found by the name prefix
`dizzy-<run-id>-<persona>-` and deleted after its other resources, even when
one of those deletes failed. A lane follows the discovery rules of its
service's `cleanup`. Idempotent.

| Flag | Description |
|---|---|
| `--run <path>` | Path to the mix run record whose resources to delete |
| `--run-id <id>` | Delete resources for this run id directly, without a record; needs `--scenario` |
| `--scenario <path>` | With `--run-id` only: the scenario the run used, which names the personas, the lanes and their clouds |
| `--set <key>=<value>` | With `--run-id` only: an override the run used; repeatable |

Exactly one of `--run` and `--run-id` is required. With `--run`, the personas,
the lanes, their clouds and identities come from the record, and a persona or
lane whose cloud now authenticates against another project than the record's
`projectID` fails before anything is deleted:

```console
$ dizzy mix cleanup --run run-1a2b3c4d.json --os-cloud other
error: persona "ci" authenticated against project <id>, but the run record says it ran in project <id>; authenticate with the cloud the run used
```

A lane reports the same with `lane "<name>"`, and a lane whose cloud is not in
`clouds.yaml` fails with `creating <api> client for lane "<name>": …`, where
`<api>` is `block storage`, `image`, `identity` or `network`.

With `--run-id`, they come from the scenario and overrides, which must be the
ones the run used. A lane needs only its name and cloud, so its scenario is
not read again: a lane scenario file moved or changed since the run does not
stop the cleanup. Without a record, the `neutron` and `keystone` lanes cannot
reclaim everything `neutron cleanup` and `keystone cleanup` reclaim with one,
and the command logs their warnings under the lane identity before it deletes
anything:

```text
level=WARN msg="cleaning up by id without a run record; resources that cannot be discovered by tag (e.g. address scopes) will not be reclaimed — pass --run to reclaim them" run=1a2b3c4d-neutron
level=WARN msg="cleaning up by id without a run record; role assignments are best reclaimed with a record — pass --run to use it" run=1a2b3c4d-keystone
```

It prints `deleted <n> resource(s) for run <run-id>-<name>` per persona and
lane and continues past a failing one, then exits non-zero naming it, as
`cleaning up persona "ci" (run …): …` or
`cleaning up lane "keystone" (run …): …`.

`nova cleanup --run` on a mix record fails with
`run record is for service "mix", not "nova"`, and `mix cleanup --run` on a
`nova` record fails the same way the other way round.

## `list-networks`

Lists the project's networks. A working auth and connectivity smoke test with no
side effects. `neutron` namespace only; takes no flags beyond the global ones.

## `verify`

A stub. Returns `not implemented yet`. Reserved for reconciling a run against the
OVN northbound/southbound databases and OVS flows.
