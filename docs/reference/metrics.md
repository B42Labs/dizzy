# Metrics reference

Every API call is wrapped with timing instrumentation that records the resource
kind, the operation, wall-clock duration, success or failure, the HTTP status,
and a timestamp. For resources with a status field, the time from "create
returned" to "status == expected" is recorded separately as **time-to-ready**,
measured by polling with backoff.

A call that the service refuses because the change it asked for is already in
place records as a success. dizzy confirms this in nine cases: a port or volume
attach whose follow-up read finds the attachment on the server; a stop, start,
resize, confirm-resize, live migration or cold migration whose 409 names the
state the call asked for; and a Cinder extend refused because the volume
already has the size. The `already ...; treating ... as success` log line is
then the only trace of the refusal. A 409 or 400 that dizzy does not confirm
keeps its `http_409` or `http_400`.

These measurements have two sinks. The in-memory collector is the source of
truth for run records and `report`. With `--otel` or `--listen-address`, the
same seam also feeds the OpenTelemetry instruments: `--otel` pushes them to an
OTLP exporter, and `--listen-address` serves them on a
[Prometheus scrape endpoint](#prometheus-scrape-endpoint).

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
draw, whatever calls it made. Its percentiles, per operation, per service,
per stage and per bucket, and those of the propagation delay, are estimates
within 1%, unlike a churn run's exact bucket percentiles. The token path
moves in sub-millisecond steps, so the table rounds its latencies to the
microsecond.

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
  end. After the `Operations` table, a run whose `probe` draws sent requests
  has a `Probes` table, a run whose `bad_token` draws sent requests a
  `Bad tokens` table, and a run with the propagation probe a `Propagation`
  block, in that order and before a staged run's `Stages` table. The two
  tables have one row per service the draws went to, sorted by name, and the
  columns `SERVICE`, `OPS`, `OK`, `FAILED`, `OPS/S`, `MIN`, `MEAN`, `P50`,
  `P90`, `P95`, `P99` and `MAX`; their `OPS/S` divides by the same wall as
  the `Operations` table. The `Propagation` block has the lines `every:`,
  `timeout:`, `probes:`, `timed out:` and `delay:`, the last with the
  delay's percentiles and maximum, e.g.
  `p50 2.1s / p95 4.8s / p99 6.2s / max 6.2s`, or `-` when no probe
  completed, with every duration rounded to the millisecond.
- **json**: `{"metrics": …, "pressure": …}`, with `"incomplete": true` added
  on a checkpoint. A staged run's stages are `pressure.stages`; see
  [Run record](run-record.md#stages). The draws per service are
  `pressure.probes` and `pressure.badTokens`, and the propagation probe is
  `pressure.propagation`; see [Run record](run-record.md#probes).
- **csv**: the same header, with the pressure rows appended after the
  per-kind rows: one row per operation labelled `pressure/<operation>`, then
  one row per service of the `Probes` table labelled
  `pressure/probe/<service>`, e.g. `pressure/probe/nova`, one per service of
  the `Bad tokens` table labelled `pressure/bad_token/<service>` and, for a
  run with the propagation probe, one row labelled `pressure/propagation`,
  then, per stage, one row per operation labelled
  `pressure/stage-<index>/<operation>`, e.g. `pressure/stage-2/effective`,
  and then, per bucket, one row per operation labelled
  `pressure@<start>/<operation>`, e.g. `pressure@1m0s/validate`. The
  attempted, succeeded and failed of the `pressure/propagation` row are the
  completed probes, those that saw the 401 and those that timed out, and its
  latencies are the delays. The cumulative revocations, a stage's
  `fan_assignments` and window, and the propagation probe's interval and
  ceiling have no column; the other formats carry them.
- **html**: after the overall sections, a `Pressure` section with the
  settings and the revocations and the per-operation table; a
  `Probes by service` section for a run whose `probe` draws sent requests and
  a `Bad tokens by service` section for one whose `bad_token` draws did, each
  with a sortable table with one row per service and the columns of the
  per-operation table, `service` in place of `operation`; for a run with the
  propagation probe, a `Propagation` section with the chips `every`,
  `timeout`, `probes`, `timed out`, `p50`, `p95`, `p99` and `max`, the four
  latencies `-` when no probe completed; for a run with stages, a
  `Pressure stages` section with a sortable table with the columns
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
and then for every operation either record drew, sorted by name. The `probe`
and `bad_token` draws compare the same way per service, for every service
either record has, sorted by name and with no `all` row, and the propagation
probes compare by their delays and counts. Two records come from different
loads when their propagation probes differ in interval or ceiling, or when
only one of them ran the probe; the difference reads e.g.
`propagation every=30s,timeout=10m0s vs off`, `off` standing for a run
without the probe, and sits after a `mix` difference and before a `stages`
one.

- **table**: a `Comparison` block with a `base:` and an `other:` line, each
  naming the run id, with ` (incomplete)` after it for a checkpoint, and the
  scenario, seed, workers, tokens and mix, followed for a run with the
  propagation probe by its interval and ceiling, e.g.
  `, propagation every=30s,timeout=10m0s`, and, when `--allow-mismatch` let
  differing loads through, a `loads differ:` line with the differences joined
  by `; `. Then an `Operations` table with the columns `OPERATION`, `METRIC`,
  `BASE`, `OTHER`, `DELTA` and `CHANGE`; when either record has `probes`, a
  `Probes` table, and when either has `badTokens`, a `Bad tokens` table, both
  with `SERVICE` in place of `OPERATION`; when either record ran the
  propagation probe, a `Propagation` table with the columns `METRIC`, `BASE`,
  `OTHER`, `DELTA` and `CHANGE` and the rows `p50`, `p95`, `p99`, `max`,
  `probes` and `timed out`; and, when either record has stages, a `Stages`
  table with `STAGE` and `FAN` before the columns of the `Operations` table.
  A latency is rounded to the microsecond and a throughput has one decimal.
  `DELTA` is the other minus the base with an explicit sign, e.g. `-420µs`,
  `+30µs`, `0s` or `+1190.4`, and `CHANGE` is the delta as a percentage of
  the base value, e.g. `-22.8%`. The two counts of the `Propagation` table
  are integers with a signed delta, e.g. `+0`. A side that lacks the
  operation, the service or the stage, or drew nothing in it, or that ran no
  propagation probe or completed none, shows `-` in its cell, in `DELTA` and
  in `CHANGE`, and a base value of `0` shows `-` in `CHANGE`. `FAN` is the
  base's `fan_assignments`, or the other's when only the other has the
  stage, and `-` for none.
- **json**: `{"base": …, "other": …, "mismatches": […], "operations": […], "probes": […], "badTokens": […], "propagation": {…}, "stages": […]}`.
  `base` and `other` hold `runID`, `scenario`, `seed`, `incomplete` (only
  when `true`), `workers`, `tokens`, `mix`, `propagationEvery` and
  `propagationTimeout`, the propagation probe's interval and ceiling in
  nanoseconds, omitted for a run without the probe, and `stages`, the
  `fan_assignments` of every stage with `0` for none, omitted for a run
  without stages. Each entry of `operations` holds `operation` and `base`,
  `other` and `delta`, each with `p50`, `p95` and `p99` in nanoseconds and
  `throughput`; a side that lacks the operation or the stage, or drew
  nothing in it, omits its key, and `delta` is omitted then too. The entries
  of `probes` and `badTokens` have the same shape, with the service as
  `operation`. `propagation` holds `base`, `other` and `delta`, each with
  `p50`, `p95`, `p99` and `max` in nanoseconds, `probes` and `timedOut`; a
  side that ran no propagation probe or completed none omits its key, and
  `delta` is omitted then too.
  Each entry of `stages` holds `index`, `fanAssignments` and its own
  `operations`. `mismatches`, `probes`, `badTokens` and `stages` are omitted
  when empty, and `propagation` when neither record ran the probe.
- **csv**: not available; `--compare` with `--format csv` fails.
- **html**: a self-contained page with the heading
  `Comparison: run <base> vs run <other>`, one line of chips per run, starting
  with `base run <id>` and `other run <id>` and, for a run with the
  propagation probe, carrying a chip with its interval and ceiling, e.g.
  `propagation every=30s,timeout=10m0s`, a yellow `Loads differ: …` banner
  when the loads differ, and a sortable `Operations` table, then sortable
  `Probes`, `Bad tokens` and `Propagation` tables when the text output has
  them and, when either record has stages, a sortable `Stages` table, with
  the columns of the text tables in lower case. The `+` of a `delta` or
  `change` cell is written as the entity `&#43;` in the page source, which a
  browser shows as `+`.

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
| `cloud` | The `--os-cloud` name; for a `mix` persona or lane, the `clouds.yaml` entry it authenticated with; for `c5c3`, the `--os-cloud` or `$OS_CLOUD` name, whose entry replaced the delivered Keystone endpoint, and absent when neither is set |
| `scenario` | The scenario name; for a `mix` lane, the name of its plan, e.g. `small/keystone` |
| `service` | `neutron`, `cinder`, `keystone`, `nova`, `glance`, `barbican`, `octavia`, `c5c3`, or `mix` |
| `persona` | `mix` only; the persona name, `ci`, `gardener` or `legacy` |
| `lane` | `mix` only; the background lane name, `cinder`, `glance`, `keystone` or `neutron`. A resource carries `persona` or `lane`, never both |

`mix chaos` exports one OTLP resource per persona and one per lane, so each
persona's and lane's series stay apart and carry their own iteration counts.
A lane's series carry `service=mix` and the `kind` values of the service it
churns.

`service` is a bespoke attribute, deliberately distinct from the semantic
`service.name`. It keeps the iteration-level series — which carry no `kind` — 
apart when a Neutron, a Cinder, a Keystone, a Nova, a Glance, a Barbican, an
Octavia, and a c5c3 monitor feed the same backend. It mirrors the run record's
`service` field.

### Instruments

| Instrument | Type | Unit | Attributes |
|---|---|---|---|
| `dizzy.operation.duration` | histogram | s | `kind`, `operation`, `outcome` |
| `dizzy.operation.errors` | counter | | `kind`, `operation`, `error.kind` |
| `dizzy.resource.time_to_ready` | histogram | s | `kind`, `outcome` |
| `dizzy.iteration.duration` | histogram | s | `outcome` |
| `dizzy.iteration.operations` | counter | | `result` |
| `dizzy.iterations` | counter | | `outcome` |

All eight services share this schema; no service adds an instrument.

### Attribute values

Every value set is bounded.

| Attribute | Values |
|---|---|
| `kind` | **neutron:** `address-scope`, `subnet-pool`, `network`, `subnet`, `router`, `router-interface`, `security-group`, `security-group-rule`, `port`, `floating-ip` — **cinder:** `volume`, `snapshot` — **keystone:** `domain`, `project`, `user`, `role`, `implied_role` (an implied-role rule), `role_assignment`, `token`, `application_credential`, `trust`, and for `keystone pressure` also `probe`, `bad_token` and `propagation` (a request to Nova, Neutron, Cinder or Glance) and, on `dizzy.resource.time_to_ready` only, `revocation` (the propagation delay) — **nova:** `server`, `network`, `subnet`, `port`, `volume`, `server_group` (only the `mix` Gardener persona creates server groups) — **glance:** `image`; **barbican:** `secret` (a stored secret and the secret an order generates alike), `order`, `container`, `acl` (a secret's read ACL), `consumer` (a container consumer) — **octavia:** `loadbalancer`, `listener`, `pool`, `member`, `healthmonitor`, `network`, `subnet`; the `mix` Gardener persona records `loadbalancer`, `listener`, `pool`, `member` and `healthmonitor` when Octavia is opted in with `services: [octavia]`, plus `port` and `subnet` samples of operation `list` from the address lookups of its member and load balancer creates — **c5c3:** `keystone_user`, `keystone_project`, `keystone_role_assignment`, `keystone_catalog_entry`, `keystone_application_credential` (the five order kinds), `secret` (a Secret the operator delivered beside an order) |
| `operation` | `create`, `delete`, `get`, `list`, `tag`, `attach`, `detach`, `extend` (cinder resize), `update` (keystone domain disable-before-delete; glance property churn; barbican secret metadata stamp; octavia listener and member admin-state toggle; c5c3 raise of an order's password or credential generation, and edit of a delivered Secret), `validate`, `revoke` (keystone token validation and revocation in `keystone pressure`; c5c3 records its check of a scoped token's role and catalog as `validate`), `authenticate` (keystone authentication with an application credential or a trust; c5c3 login with a delivered credential), `finalize` (c5c3: from the return of a delete until the order is gone, and under `kind=secret` until the garbage collector removed a deleted order's delivered Secret, with `error.kind=mismatch` when the Secret is still present at the deadline), `rotate` (c5c3: from the raised generation until the new credential is delivered), `repair` (c5c3: from the edit or delete of a Secret until the operator restored it), `reject` (c5c3: a login that must fail), `refuse` (c5c3: from the create of an order until it reports the expected refusal reason), `stop`, `start`, `reboot`, `resize`, `confirm-resize`, `live-migrate`, `cold-migrate` (nova server lifecycle; `cold-migrate` only for the `mix` Legacy persona), `upload`, `visibility`, `deactivate`, `reactivate`, `member-add`, `member-accept`, `member-remove` (glance image lifecycle), `download` (barbican payload read-back), `nova`, `neutron`, `cinder`, `glance` (the service a `probe`, `bad_token` or `propagation` request went to; `keystone pressure` only) |
| `outcome` | `success`, `error`, `timeout` for an operation; `success`, `timeout` for time-to-ready, where `timeout` under `kind=revocation` is a propagation probe that reached its ceiling; `success`, `failure` for an iteration |
| `result` | `attempted`, `succeeded`, `failed` |
| `error.kind` | `quota`, `timeout`, `canceled`, `other`, `http_<status>` for the statuses the service returns, `http_other` for the rest, `accepted` for a `bad_token` request that a service answered with a 2xx, `mismatch` for a barbican read-back of a payload, an ACL or a container's consumers that returned data that differs from what was written, `order_error` for a barbican order that ended in ERROR, `provisioning_error` for an octavia load balancer that reached ERROR. c5c3 adds `refused` for an order whose condition carries a reason the operator does not retry, such as `NamespaceNotAssigned` or `RoleNotAllowed`, and gives `mismatch` and `accepted` a meaning of their own: `mismatch` for a delivered credential that Keystone rejects while its order is ready, an application credential token scoped to another project than the ordered one, a delivered document gophercloud cannot read, a scoped token without the role or a catalog without the entry, a Secret that still carries the old credential after a rotation or is still missing when the rotation's wait ends, a ready order's Secret that is missing or unreadable, a refusal order that turned ready or reported another reason, and a Secret still present after its order is gone; `accepted` for a login that had to fail and got a token |

> **Cardinality rule.** Run IDs, resource IDs, and resource names are **never**
> metric attributes. They live in the run records and the logs.

`operation.errors` breaks down what `operation.duration`'s `outcome` collapses.
It is recorded **only for failed operations**, so a healthy run emits no series
at all — its absence is the steady state, not a missing metric.

Keystone creates and deletes are synchronous, so `resource.time_to_ready` is
largely unused there. One exception is **token-issue latency**, recorded with
`kind=token` in addition to the `token`/`create` operation histogram. The
others are the authentication latency of application credentials and trusts
and the propagation delay of `keystone pressure`, both below.
`keystone pressure` adds two operations under `kind=token`, `validate` and
`revoke`, which record no time-to-ready; its pool issues record as
`token`/`create` like any token issue, the three listings of its `effective`
operation as `role_assignment`/`list` and `project`/`list`, and the `reader`
grants of its pool users and the grants of its stages as
`role_assignment`/`create` like any grant.

Application credentials and trusts record their creates as
`application_credential`/`create` and `trust`/`create`, and the deletes of
the churn as `application_credential`/`delete` and `trust`/`delete`. Each
authentication with a credential or a trust records as
`application_credential`/`authenticate` or `trust`/`authenticate`, and with
the same duration on `dizzy.resource.time_to_ready` under
`kind=application_credential` or `kind=trust`, where a failed one records
`outcome=timeout`, as a failed token issue does. dizzy creates and deletes
both kinds as their owners, with a token it first issues to the owning user.
Each such owner token records as `token`/`create` and under `kind=token` on
`dizzy.resource.time_to_ready`, like any token issue, so the `token` counts
of a run with either kind include these owner tokens beside the plan's token
issues.

Each request of a `probe` or `bad_token` draw and each poll of the
propagation probe records on `dizzy.operation.duration` and, when it fails,
on `dizzy.operation.errors`, under `kind=probe`, `kind=bad_token` or
`kind=propagation` with the service as `operation`. A `bad_token` request
that a service answers with 401 records `outcome=success`, and one it
answers with a 2xx records `outcome=error` and `error.kind=accepted`; a poll
records `outcome=success` for a 401 and a 2xx alike. The propagation probe's
revocations and reissues record as `token`/`revoke` and `token`/`create`,
and its delays on `dizzy.resource.time_to_ready` under `kind=revocation`,
with `outcome=timeout` for a probe that reached its ceiling. The per-draw
samples of the `Pressure` section and the per-service samples of the
`pressure` object stay in the run record and are not exported, so no call
is exported twice.

Barbican records `dizzy.resource.time_to_ready` under `kind=order`, for the
wait from the return of an order's create until Barbican reports the order
ACTIVE: the PENDING to ACTIVE time of its key generation. An order that ends in
ERROR, disappears, or is still pending when its wait gives up records
`outcome=timeout`. The polls of the wait record no operation sample, except the
poll that finds the order in ERROR, or ACTIVE without a secret reference, which
records one failed `order`/`get` with `error.kind=order_error`. A payload read-back records as
`secret`/`download`, timed around the request and the byte comparison
together, so a read-back that returns different bytes records `outcome=error`
with `error.kind=mismatch`. The metadata stamp after each store and each order
records as `secret`/`update`, and the metadata reads of cleanup's secret
discovery as `secret`/`get`.

An ACL records its write as `acl`/`create` and its read as `acl`/`get`, and a
consumer its registration as `consumer`/`create` and the listing of its
container's consumers as `consumer`/`list`. The reads of `barbican status`
record under the same two. A read-back compares inside the timed call, as the
payload read-back does, so one that differs records `outcome=error` with
`error.kind=mismatch`. The churn's deletes record as `acl`/`delete` and
`consumer`/`delete`; they are the only deletes of either kind, since the
teardown leaves both to Barbican.

Octavia records `dizzy.resource.time_to_ready` for each create and each
admin-state change, under the kind it wrote (`loadbalancer`, `listener`,
`pool`, `member`, or `healthmonitor`): the wait from the return of the write
until the load balancer is ACTIVE again. The API answers in milliseconds and
the provider does the work afterwards, so this is the number that shows an
amphora build or a slow OVN provider. An admin-state toggle of `octavia chaos`
is two changes, `admin_state_up=false` and then `true`, each recorded as
`listener`/`update` or `member`/`update` and each with its own time-to-ready
under `listener` or `member`. A churn delete records none: the wait after it,
for ACTIVE or for a deleted load balancer to be gone, is not the time-to-ready
of any object. A load balancer that reaches ERROR, disappears, or is not
ACTIVE again when the wait of a create or an admin-state change gives up
records `outcome=timeout`. The polls of a wait record no operation sample,
except the poll that finds the load balancer in ERROR, which records one
failed `<kind>`/`get` under the written kind with
`error.kind=provisioning_error`, after the churn delete of a listener, pool,
member, or health monitor too. The VIP networks record their time-to-ready
under `kind=network`, as Nova's companion networks do.

c5c3 records `dizzy.resource.time_to_ready` under the five order kinds, for
the wait from the return of an order's create until the order is `Ready` at
its current generation: the time the c5c3-operator, K-ORC, the External
Secrets Operator, OpenBao and Keystone take together before the credential is
delivered. A wait that ends without `Ready`, at its deadline, on a refusal,
because the order vanished or because the run was interrupted, records
`outcome=timeout`. Unlike the Barbican and Octavia waits, every poll of a c5c3
wait records a `get` sample for each read, of the order's kind for the order
and of kind `secret` for a Secret, and the 404 that ends the wait for a
deleted order or its Secret records a successful one. The health check of
`--listen-address` counts those samples as progress, so a wait of 15 minutes
does not read as a stall. Most waits also record one sample over their whole
length under an operation of their own: `finalize` for a deleted order, and
under `kind=secret` for a deleted order's Secret; `rotate` for a raised
generation; `repair` for an edited or deleted Secret; and `refuse` for an
order that must be refused. The wait for the superseded application
credential to go and cleanup's wait for a user's password generation to
settle record their polls only. The reads, edits and deletes of the delivered Secrets
record under `kind=secret`. A login records as `authenticate` or, for one that
must fail, `reject` of the order's kind, and the checks of a scoped token as
`validate` under `keystone_role_assignment` and `keystone_catalog_entry`. The
catalog check logs in again until the catalog lists the entry and records one
`validate` over all its logins when it ends, at the latest once `--timeout`
has passed. Before each login except one that must fail, dizzy reads the
credential's order and its Secret, one `get` of the order's kind and one of
kind `secret`, and it reads both again after a 401, or a 404 to an
application credential login.

A delivered credential that Keystone rejects while its order is ready shows as
`dizzy_operation_errors_total{service="c5c3", operation="authenticate", error_kind="mismatch"}`,
and so do an application credential token scoped to another project than the
ordered one and a delivered document gophercloud cannot read. A Keystone
outage shows as `http_<status>`, `timeout` or `other` on the same operation: a
5xx or a connection error is retried, a timeout ends the call with error kind
`timeout`, and none of them becomes a mismatch. A 401, or for an application
credential a 404, that raced a change of the credential records as `http_401`
or `http_404`.

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

## Prometheus scrape endpoint

`--listen-address <host:port>` serves the instruments above in the Prometheus
text exposition format on `/metrics`, for a Prometheus server to scrape. A
second reader on the OpenTelemetry MeterProvider that `--otel` pushes from
feeds it, so the endpoint and the OTLP export read the same instruments. The
two are independent: a command runs with either, both, or neither.

The listener starts only in the verbs that record metrics: `apply`, `chaos`
(`mix chaos` included), `keystone pressure` and `monitor`. `generate`,
`status`, `report`, `cleanup`, `verify` and `list-networks` accept the flag
and open no listener, as they accept `--otel`. It starts once per process,
before the command authenticates against the cloud, and dizzy then logs
`serving metrics and health` with `address` and `stallTimeout`. An address
that cannot be bound fails the command, e.g.
`setting up telemetry: listening on :9464: listen tcp :9464: bind: address already in use`.
The listener serves until the process exits, so the series stay scrapeable
through the teardown after the first SIGTERM. Every path other than `/metrics`
and `/healthz` answers 404.

The families carry the names that [Prometheus naming](#prometheus-naming) gives
the OTLP path, so the [query cookbook](#query-cookbook-promql) works unchanged
against the endpoint.

| Family | Instrument |
|---|---|
| `dizzy_operation_duration_seconds` (`_bucket`, `_sum`, `_count`) | `dizzy.operation.duration` |
| `dizzy_operation_errors_total` | `dizzy.operation.errors` |
| `dizzy_resource_time_to_ready_seconds` (`_bucket`, `_sum`, `_count`) | `dizzy.resource.time_to_ready` |
| `dizzy_iteration_duration_seconds` (`_bucket`, `_sum`, `_count`) | `dizzy.iteration.duration` |
| `dizzy_iteration_operations_total` | `dizzy.iteration.operations` |
| `dizzy_iterations_total` | `dizzy.iterations` |
| `target_info` | None; one series per resource |

Every series carries its instrument's attributes as the labels `kind`,
`operation`, `outcome`, `result` and `error_kind`, the dot of `error.kind`
turned into an underscore. It also carries the
[resource attributes](#resource-attributes) `cloud`, `scenario`, `service`,
`persona` and `lane` as labels, each only when its value is set, so `persona`
appears only on a series of a `mix` persona and `lane` only on one of a `mix`
lane. `target_info` carries the whole resource: `service_name="dizzy"`,
`service_version`, `cloud`, `scenario`, `service`, `persona` or `lane`, and the
`telemetry_sdk_*` attributes. No series carries an `otel_scope_*` label, and
the endpoint serves no Go runtime or process metrics, no `go_*` and no
`process_*` family.

One endpoint carries every persona and every lane of a `mix chaos` run, and
the `persona` and `lane` labels tell their series apart.

A series appears with its first sample. As on the OTLP path,
`dizzy_operation_errors_total` is therefore absent on a healthy run.

## Health endpoint

`/healthz` on the same listener reports whether the run still makes progress.
It is healthy while the last recorded operation, successful, failed or timed
out alike, or the last finished iteration is younger than
`--health-stall-timeout` (default `15m`). The clock starts when the listener
starts. The planned wait between two `monitor` iterations never counts as a
stall: the rest of `--interval`, the `--error-wait` pause and the 5 s backoff
after a failure.

| State | Status | Body |
|---|---|---|
| Healthy | `200` | `ok` |
| Stalled | `503` | `stalled: no operation or iteration finished for <idle> (limit <limit>)` |

Both bodies are `text/plain`, and both durations are rounded to the second,
e.g. `stalled: no operation or iteration finished for 16m2s (limit 15m0s)`. A
stalled answer also logs the warning `health check failing` with `idle` and
`limit`.

`--health-stall-timeout 0` disables the check, and `/healthz` then always
answers 200. Before any command runs, dizzy checks the flag. A negative value
fails with `--health-stall-timeout must not be negative, got <value>`. With
`--listen-address` set and a non-zero stall timeout, the stall timeout must be
greater than twice `--timeout`; otherwise the command fails with
`--health-stall-timeout (<v>) must be greater than twice --timeout (<v>), or 0 to disable the check`.
A readiness or deletion wait records nothing until it ends and can last the
whole `--timeout`, and the call after it is recorded only when it returns,
which can take another `--timeout`.

The listener speaks plain HTTP, with no TLS and no authentication; the bind
address limits who reaches it. For choosing the stall timeout and wiring the
endpoint into a liveness probe, see
[Serve Prometheus metrics and a health check](../how-to/serve-prometheus-metrics.md).
