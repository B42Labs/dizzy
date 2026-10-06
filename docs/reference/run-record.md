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

## Top level

| Field | Type | Notes |
|---|---|---|
| `runID` | string | The run id; also the tag/metadata/name-prefix value |
| `service` | string | `neutron`, `cinder`, `keystone`, `nova`, or `glance`. Omitted on older records |
| `scenario` | string | Scenario name |
| `seed` | int | The seed the plan was expanded from |
| `startedAt` | timestamp | RFC 3339 |
| `finishedAt` | timestamp | RFC 3339 |
| `created` | array | Every created resource; see below |
| `error` | string | Present only when the run failed |
| `metrics` | object | Aggregate metrics; see below |
| `volumeType` | string | Cinder only; the resolved volume type, for provenance |
| `chaos` | object | Churn runs only; see below |
| `incomplete` | bool | `true` on a record `chaos` wrote while still running; `finishedAt` is then the checkpoint time and `metrics.wall` the time elapsed so far. Omitted on a final record |

## `created[]`

One entry per created resource, in creation order. Service-agnostic: the shape
is the same whichever service produced it.

| Field | Type | Notes |
|---|---|---|
| `kind` | string | Resource type, e.g. `network`, `volume`, `domain`, `server` |
| `logical` | string | The plan's reference name, e.g. `net-0001` |
| `name` | string | The applied cloud name, e.g. `dizzy-a1b2c3d4-net-0001` |
| `id` | string | The service's UUID |

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
`chaos.buckets[]`.

### Error kinds

The `kind` in an `errors[]` entry is the service client's classification:
`quota`, `timeout`, `canceled`, `other`, `http_<status>` for the statuses the
service actually returns, or `http_other`. These are the same values the OTLP
`error.kind` attribute carries — see [Metrics](metrics.md).

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

## Stability

The record is written by `dizzy` and read by `dizzy`. Treat it as an artifact of
a specific version, not as a stable public interface. If you need a stable
machine-readable surface across versions, use `report --format json` or the OTLP
export.
