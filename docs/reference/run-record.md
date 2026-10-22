# Run record

`apply` writes a run record to `run-<id>.json` in the current directory. It is
the canonical artifact of a run: what was created, how long each call took, and
what went wrong. `status`, `report`, and `cleanup` all consume it.

`chaos` writes one too, with an extra `chaos` object. It rewrites the record
once a minute while it runs, marked `incomplete`, and writes the final record
when the run ends. A consumer must check `incomplete` before it treats a
`chaos` record as final: dizzy releases that predate the key ignore it and read
a checkpoint as a finished run. `monitor` writes one per iteration only with
`--keep-run-records`, since in a long loop they accumulate unboundedly.

`keystone pressure` writes one with an extra `pressure` object instead of a
`chaos` object, and checkpoints it the same way, once a minute marked
`incomplete`.

`mix chaos` writes one record for all of its personas and background lanes,
with `service: "mix"`, a `personas` array and, when the run had lanes, a
`lanes` array instead of a top-level `chaos` object, and checkpoints it the
same way.

## Top level

| Field | Type | Notes |
|---|---|---|
| `runID` | string | The run id; also the tag/metadata/name-prefix value |
| `service` | string | `neutron`, `cinder`, `keystone`, `nova`, `glance`, or `mix`. Omitted on older records |
| `scenario` | string | Scenario name |
| `seed` | int | The seed the plan was expanded from |
| `startedAt` | timestamp | RFC 3339 |
| `finishedAt` | timestamp | RFC 3339 |
| `created` | array | Every created resource; see below |
| `error` | string | Present only when the run failed |
| `metrics` | object | Aggregate metrics; see below |
| `volumeType` | string | Cinder only; the resolved volume type, for provenance |
| `chaos` | object | Churn runs only; see below. Absent on a `mix` record, whose personas and lanes carry their own |
| `services` | array of strings | `mix` only; the opt-in services bound to the personas. Omitted when empty |
| `personas` | array | `mix` only; one entry per persona; see below |
| `lanes` | array | `mix` only; one entry per background lane; see below. Omitted when the run had none |
| `pressure` | object | `keystone pressure` runs only; see below. Absent on every other record |
| `incomplete` | bool | `true` on a record `chaos` or `keystone pressure` wrote while still running; `finishedAt` is then the checkpoint time and `metrics.wall` the time elapsed so far. Omitted on a final record |

## `created[]`

One entry per created resource, in creation order. Service-agnostic: the shape
is the same whichever service produced it.

| Field | Type | Notes |
|---|---|---|
| `kind` | string | Resource type, e.g. `network`, `volume`, `domain`, `server` |
| `logical` | string | The plan's reference name, e.g. `net-0001` |
| `name` | string | The applied cloud name, e.g. `dizzy-a1b2c3d4-net-0001` |
| `id` | string | The service's UUID |
| `persona` | string | `mix` only; the persona that created the resource |
| `lane` | string | `mix` only; the background lane that created the resource. An entry carries `persona` or `lane`, never both |

On a `mix` record the list holds every persona's resources, persona by persona,
and then every lane's, lane by lane, each lane with the resources it created
before its engine started (the Keystone lane's domains, roles and implied-role
rules) first. An empty run writes `"created": []`.

On a `keystone pressure` record the list also holds, after the provisioned
resources, the grants of the cloud's `reader` role to the pool users that a
run with a positive `probe` weight or with the propagation probe makes before
it primes the pool, as `role_assignment` entries such as
`user-0001->proj-0001/reader`, and then the role assignments the run granted
while it entered its [stages](#stages). A run that failed while it made the
reader grants or entered a stage lists the grants made up to the failure, so
`cleanup` reclaims them like any other grant. The `reader` role itself is not
in the list, and no teardown deletes it.

This list is `cleanup`'s belt-and-suspenders handle. It is the *only* handle for
resources that cannot be discovered by tag — Neutron address scopes, and Keystone
role assignments.

## `metrics`

| Field | Type | Notes |
|---|---|---|
| `wall` | duration | Total wall-clock for the run, in nanoseconds |
| `overall` | Stats | Aggregated across every resource type |
| `byType` | array of Stats | One entry per resource type |
| `errors` | array | `{kind, count}` per error kind |
| `readiness` | array | `{type, count, ok, latency}` per resource type |

### Stats

| Field | Type | Notes |
|---|---|---|
| `type` | string | Resource type; empty on the `overall` entry |
| `attempted` | int | |
| `succeeded` | int | |
| `failed` | int | |
| `throughput` | float | Operations per second |
| `latency` | Latency | |

### Latency

`min`, `mean`, `median`, `p90`, `p95`, `p99`, `max` — all durations in
nanoseconds, as Go encodes `time.Duration`.

Under `metrics`, `median`, `p90`, `p95` and `p99` are estimates within 1% of the
exact value, since the collector keeps a fixed-size histogram instead of every
sample. `min`, `mean` and `max` are exact, and so is every value in
`chaos.buckets[]`. Under `pressure`, the percentiles of `operations`, of
every bucket, of every stage, of `probes`, of `badTokens` and of the
propagation delay are estimates within 1% as well.

### Error kinds

The `kind` in an `errors[]` entry is the service client's classification:
`quota`, `timeout`, `canceled`, `other`, `http_<status>` for the statuses the
service actually returns, or `http_other`. These are the same values the OTLP
`error.kind` attribute carries — see [Metrics](metrics.md).
A `keystone pressure` record adds `accepted`, for a `bad_token` request that
a service answered with a 2xx.

## `chaos`

Present only on a run record written by `chaos`.

| Field | Type | Notes |
|---|---|---|
| `creates` | int | Create operations issued |
| `deletes` | int | Delete operations issued |
| `mutates` | int | Cinder extends, Keystone token issues, or Nova server lifecycle mutations; omitted when zero |
| `cycles` | int | Completed create→delete cycles |
| `popMin` | int | Minimum live population over the run |
| `popMax` | int | Maximum live population |
| `popMean` | float | Mean live population |
| `targetFill` | float | The controller's target, for comparison against `popMean` |
| `buckets` | array | Time-sliced stats; see below |
| `bucketWidth` | duration | Width of every bucket of a run with `--duration 0`, in nanoseconds. Omitted for a bounded run |

### `buckets[]`

Latency and errors bucketed over the run's duration, so degradation over time is
visible rather than averaged away. `report` renders these as a time series.

A bounded run has ten buckets of equal width. A run with `--duration 0` has one
bucket per `bucketWidth`, starting at offset 0, including buckets without
operations; its last bucket may be partial, and its throughput still divides by
the full width. A record written before the first tick has no buckets.

| Field | Type | Notes |
|---|---|---|
| `start` | duration | Offset from the start of the run |
| `stats` | Stats | Stats for this bucket |
| `errors` | array | `{kind, count}` for this bucket |

## `pressure`

Present only on a run record written by `keystone pressure`. A record written
by any other command, or by a release that predates the verb, has no
`pressure` key.

The top-level `metrics` count, by resource kind, every call the run made
before the record was written: the provisioning, the reader grants of the
pool users, the token pool's issues and refreshes, the grants of the stages,
every call of a draw and every call of the propagation probe. A validation
and a revocation both count under `token`, and so do the propagation probe's
revocations and reissues. The reader grants count under `role_assignment`,
as the provisioning's grants do; the lookup of the `reader` role is not in
`metrics`.

A request to Nova, Neutron, Cinder or Glance counts under a kind of its own,
whatever the service: `probe` for a `probe` draw, `bad_token` for a
`bad_token` draw, which succeeds on a 401, and `propagation` for a poll of
the propagation probe, which succeeds on a 401 and on a 2xx alike, since
either answers the poll's question. The
[`probes` and `badTokens`](#probes) entries below split the draws by service.
`metrics.readiness` carries a `revocation` entry for the propagation delays;
see [`propagation`](#propagation).

`pressure` counts the workers' draws by operation: one sample per draw,
whatever calls it made. A `revoke` sample therefore covers the revocation and
the reissue together; the revocation call alone is in the `token`/`revoke`
series of the `--otel` export, next to the propagation probe's revocations.

| Field | Type | Notes |
|---|---|---|
| `workers` | int | Closed-loop workers |
| `tokens` | int | Size of the token pool |
| `churnUsers` | int | Users reserved for `grant_churn`. `0`, and so omitted, when the run reserved none, which includes every run whose `grant_churn` weight is `0` |
| `mix` | array | `{operation, weight}` for every operation with a positive weight, sorted by operation |
| `operations` | object | The run's draws, shaped like [`metrics`](#metrics), with one `byType` entry per operation drawn; `type` is the operation name and `wall` the time from the start of the run, from which the bucket offsets count, to its end, so in a staged run it includes the time the stages took to enter |
| `revocations` | int | `revoke` draws that succeeded. The propagation probe's revocations are not in it; they count as `token` calls in the top-level `metrics` |
| `buckets` | array | Time-sliced stats; see below |
| `bucketWidth` | duration | Width of every bucket of a run with `--duration 0`, in nanoseconds. Omitted for a bounded run |
| `stages` | array | One entry per stage the run entered, in stage order; see below. Omitted for a run without stages |
| `probes` | array of Stats | The `probe` draws that sent a request, per service; see below. Omitted when there are none, which includes every run whose `probe` weight is `0` |
| `badTokens` | array of Stats | The `bad_token` draws that sent a request, per service, shaped like `probes`. Omitted when there are none, which includes every run whose `bad_token` weight is `0` |
| `propagation` | object | The propagation probe's settings and outcome; see below. Omitted for a run without the probe |

The median, p90, p95 and p99 in `operations`, in every bucket and in every
stage are the collector's estimates within 1% of the exact value; `min`,
`mean` and `max` are exact. Their `errors[]` label a failure `canceled`,
`timeout`, `forbidden`, `not-found`, `transient` (a 500, 502, 503, 504 or
429), `unauthorized` (a 401, such as a service refusing a live pool token),
`other` (any other failure, a 400 or a 501 among them), `pool-empty` (a draw
found the token pool empty), `no-service` (the drawn token's catalog lists
none of Nova, Neutron, Cinder and Glance) or `accepted` (a service answered a
`bad_token` request with a 2xx), where the top-level `metrics` use the
[error kinds](#error-kinds) above.

The record carries no token and no password: the token pool and the users'
passwords stay in the memory of the process that ran it.

### `buckets[]`

Every draw lands in the bucket of the offset at which it started, so a
latency change over the run is visible next to the revocations that had
accumulated by then. A bounded run has ten buckets of equal width, and a draw
that starts at or after the end lands in the last one. A run with
`--duration 0` has one bucket per `bucketWidth`, starting at offset 0,
including buckets without draws; its last bucket may be partial, and its
throughput still divides by the full width. In a staged run the offsets count
from the moment the run began entering its first stage, so a bucket that
falls within that entry holds no draws.

| Field | Type | Notes |
|---|---|---|
| `start` | duration | Offset from the start of the run: the moment the workers start or, in a staged run, the moment the run began entering its first stage |
| `operations` | object | The draws that started in this bucket, shaped like [`metrics`](#metrics), with one `byType` entry per operation drawn in it; `wall` is the bucket width, and `errors` the bucket's error breakdown |
| `revocations` | int | Cumulative: the `revoke` draws that succeeded in this bucket and in every earlier one |

### `stages[]`

Present only on the record of a run whose `pressure:` block has
[stages](scenario-schema.md#stages). Every draw lands in the stage it started
in, as well as in its bucket and in `operations`. A checkpoint lists the
stages entered so far, and so does the record of an interrupted run or of a
run whose duration ended while it was entering a stage; see
[`pressure`](cli.md#pressure).

| Field | Type | Notes |
|---|---|---|
| `index` | int | The stage's number, from 1 |
| `fanAssignments` | int | The stage's `fan_assignments`: the grants every fan user holds in the stage. `0`, and so omitted, when the stage set none |
| `start` | duration | Offset in nanoseconds, counted like a bucket's `start`, at which the workers resumed after the run entered the stage. For the first stage it is the time that stage took to enter, since the offsets count from the moment the run began entering it and the workers start after it. Equals `end` for a stage entered after its window ended |
| `end` | duration | Offset at which the stage's window ended. For the stage the run was in when a checkpoint was written or when it was interrupted, the offset at that time |
| `operations` | object | The draws that started between `start` and `end`, shaped like [`metrics`](#metrics), with one `byType` entry per operation drawn in the stage; `wall` is `end` minus `start` |

### `probes[]`

Present only on the record of a run whose `probe` draws sent at least one
request. One [Stats](#stats) entry per service a `probe` draw sent a pool
token to, sorted by service name. Each draw that sent a request is one sample
under its service, timed around that request. A draw that found the pool
empty or drew a token whose catalog lists none of the four services sent
nothing and is not in `probes`; like every draw, it counts in `operations`,
in its bucket and in its stage. `badTokens` has the same shape for the
`bad_token` draws.

| Field | Type | Notes |
|---|---|---|
| `type` | string | The service: `cinder`, `glance`, `neutron` or `nova` |
| `attempted` | int | Draws that sent a request to the service |
| `succeeded` | int | Under `probes`, draws whose request succeeded; under `badTokens`, draws the service answered with the expected 401 |
| `failed` | int | The other draws. An entry carries no `errors`; the labels of the failures are in the `errors` of `operations`, of the buckets and of the stages |
| `throughput` | float | Draws per second over `operations.wall`, the wall the `operations` entries divide by |
| `latency` | Latency | The request's time. The median, p90, p95 and p99 are estimates within 1%; `min`, `mean` and `max` are exact |

### `propagation`

Present only on the record of a run that ran the propagation probe, which a
positive `--propagation-every` or `propagation.every` switches on; see
[The `pressure:` block](scenario-schema.md#the-pressure-block). A probe
completes when its service answers a poll with 401 or when it reaches the
timeout. A probe that the end of the run abandoned is not counted, and
neither is one skipped because the pool was empty, the revocation failed or
the token's catalog listed no service.

| Field | Type | Notes |
|---|---|---|
| `every` | duration | The probe's interval, in nanoseconds |
| `timeout` | duration | The ceiling of one probe, in nanoseconds |
| `probes` | int | Probes that completed |
| `timedOut` | int | Probes among them that reached `timeout` without a 401 |
| `delay` | Latency | The time from the return of the revocation to the return of the first poll the service answered with 401, over every completed probe, a timed-out probe counting as `timeout`. The median, p90, p95 and p99 are estimates within 1%; `min`, `mean` and `max` are exact. Every field is `0` when no probe completed |

The `revocation` entry of the top-level `metrics.readiness` holds the same
delays: its `count` counts the completed probes and its `ok` those that did
not time out.

## `personas[]`

Present only on a `mix` record: one entry per persona, in the order the run ran
them. The top-level `metrics` are the exact aggregate of every persona and
lane.

| Field | Type | Notes |
|---|---|---|
| `name` | string | Persona name, e.g. `ci` |
| `runID` | string | The identity the persona's resources carry, `<runID>-<name>`; `mix cleanup --run` deletes by it |
| `cloud` | string | The `clouds.yaml` entry the scenario named for the persona. Omitted when the persona used `--os-cloud` |
| `projectID` | string | The project the persona authenticated against. Omitted when the token did not name one |
| `share` | float | The persona's share divided by the sum of all shares |
| `servers` | int | The persona's part of `resources.servers` |
| `seed` | int | The seed the persona's plan was expanded from |
| `metrics` | object | The persona's own aggregate metrics, shaped like the top-level `metrics` |
| `chaos` | object | The persona's churn statistics, shaped like [`chaos`](#chaos). Omitted in a checkpoint written before the persona's engine produced its first snapshot |

There is no time series merged across personas: the percentiles of separate
engines cannot be merged from their bucket statistics, so each persona carries
its own `chaos.buckets`.

## `lanes[]`

Present only on a `mix` record of a run with background lanes: one entry per
lane, in the order the run ran them. A lane has no share and no servers.

| Field | Type | Notes |
|---|---|---|
| `name` | string | Lane name, the service it churned: `cinder`, `glance`, `keystone` or `neutron` |
| `runID` | string | The identity the lane's resources carry, `<runID>-<name>`; `mix cleanup --run` deletes by it |
| `cloud` | string | The `clouds.yaml` entry the scenario named for the lane. Omitted when the lane used `--os-cloud` |
| `projectID` | string | The project the lane authenticated against. Omitted when the token did not name one |
| `scenario` | string | The name of the lane's plan, `<scenario>/<name>`, e.g. `small/keystone` |
| `seed` | int | The seed the lane's plan was expanded from |
| `metrics` | object | The lane's own aggregate metrics, shaped like the top-level `metrics` |
| `chaos` | object | The lane's churn statistics, shaped like [`chaos`](#chaos). Omitted in a checkpoint written before the lane's engine produced its first snapshot |

The entry records no resolved volume type, privilege tier or external network:
the scenario and the `--set` values of the run state them, and `mix cleanup`
needs none of them.

## Stability

The record is written by `dizzy` and read by `dizzy`. Treat it as an artifact of
a specific version, not as a stable public interface. If you need a stable
machine-readable surface across versions, use `report --format json` or the OTLP
export.
