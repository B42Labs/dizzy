# Run a churn soak with `chaos`

`chaos` treats the scenario as an **envelope** rather than a target. For the
whole duration it creates *and* deletes planned resources at random, seeded
intervals, keeping the live population inside the scenario's counts. It reports
latency and error rates bucketed over time, so degradation shows up rather than
being averaged away.

For what the knobs actually control, see
[The churn engine](../explanation/churn-engine.md).

## Run a built-in profile

All eighteen profiles ship a `chaos:` block, so they run with no flags at all:

```console
$ dizzy neutron chaos --scenario scenarios/neutron/small.yaml    # 5m
$ dizzy cinder chaos  --scenario scenarios/cinder/small.yaml     # 5m
$ dizzy keystone chaos --scenario scenarios/keystone/small.yaml  # 5m
$ dizzy nova chaos --scenario scenarios/nova/small.yaml          # 5m
$ dizzy glance chaos --scenario scenarios/glance/small.yaml      # 5m
$ dizzy mix chaos --scenario scenarios/mix/small.yaml            # 5m
```

The topology is torn down at the end of the run, followed by a leak check.

## Set the duration

`--duration` sets when the run ends on its own; Ctrl-C ends it earlier. A flag
overrides the scenario's `chaos:` block:

```console
$ dizzy neutron chaos --scenario scenarios/neutron/medium.yaml --duration 2h
```

## Run until stopped

To keep a lab under load until you stop it, pass `--duration 0`:

```console
$ dizzy neutron chaos --scenario scenarios/neutron/small.yaml --duration 0
```

Stop it with Ctrl-C or SIGTERM. The run schedules nothing more. If operations
are still in flight, it writes a checkpoint of its run record and lets them
finish. `--timeout` bounds each attempt and each wait phase of an operation, so
with retries one operation can take several timeouts. The run then writes its
final record, tears the topology down and runs the leak check, as a bounded run
does when its duration elapses, and exits 0. A second signal ends the process at
once, without the final record or the teardown; clean up from the checkpoint
with `cleanup --run`. Only the flag selects this mode: `duration: 0` in the
`chaos:` block means unset.

Such a run slices its time series into buckets of `--bucket-width`, one hour by
default and at least one minute:

```console
$ dizzy neutron chaos --scenario scenarios/neutron/small.yaml --duration 0 --bucket-width 15m
```

The series keeps every bucket, so it is the one part of the run record that
grows: about 0.6 kB per bucket, 24 buckets per day at the default width.

## Tune the churn

```console
$ dizzy neutron chaos --scenario scenarios/neutron/medium.yaml \
    --duration 1h \
    --target-fill 0.9 \
    --churn-ratio 0.5 \
    --min-interval 50ms --max-interval 500ms \
    --max-parallel 16
```

| Flag | What it does |
|---|---|
| `--target-fill` | How **full** the envelope stays, on average. `0.9` keeps the project near the scenario's counts |
| `--churn-ratio` | How **fast** it turns over once at target. `0.5` balances creates and deletes for maximum turnover |
| `--min-interval` / `--max-interval` | The random delay drawn between ticks. Tighten both to increase pressure |
| `--max-parallel` | Per-tick fan-out cap, itself bounded by the global `--concurrency` |

Raising `--max-parallel` above `--concurrency` has no effect; the global worker
pool is the real ceiling.

## Add mutations

Cinder and Keystone can mutate live resources instead of only creating and
deleting them. Mutations do not change the population.

```console
$ dizzy cinder chaos --scenario scenarios/cinder/medium.yaml \
    --duration 2h --resize-ratio 0.5

$ dizzy keystone chaos --scenario scenarios/keystone/small.yaml \
    --duration 30m --token-ratio 0.5
```

`--resize-ratio` is the probability per churn step of extending a live,
not-yet-resized volume to its planned target. `--token-ratio` is the probability
of issuing a scoped token as a live, assigned user. Set either to `0` to disable.

Each resource instance is mutated at most once per lifetime, and re-armed when it
is deleted and recreated. The exception is the `mix` Legacy persona: its
servers, volumes and ports are never deleted and are changed again and again
(see [Mix workloads in one run](#mix-workloads-in-one-run)).

## Keep the resources for inspection

By default a churn run tears down at the end **and on interrupt** — the first
Ctrl-C cleans up rather than abandoning. To interrupt and inspect instead:

```console
$ dizzy neutron chaos --scenario scenarios/neutron/small.yaml --no-cleanup
```

Then remove them explicitly when done:

```console
$ dizzy neutron cleanup --run run-<id>.json
```

## Make a run reproducible

Fix the seed and keep every other setting identical:

```console
$ dizzy neutron chaos --scenario scenarios/neutron/small.yaml --seed 12345
```

The whole decision schedule — timings, fan-out, create-versus-delete, which
resource — replays exactly. The order in which the concurrent cloud calls
*complete* does not, since that is the cloud's business.

## Mix workloads in one run

`mix chaos` runs several workload personas side by side, each in its own
project, and ends with one run record. Give each persona a project of its own
by adding one `clouds.yaml` entry per project. A persona that names no entry
uses `--os-cloud`:

```yaml
clouds:
  soak:                           # the --os-cloud default
    auth:
      auth_url: https://keystone.example.com/v3
      username: soak
      password: <password>
      project_name: soak
      user_domain_name: Default
      project_domain_name: Default
    region_name: RegionOne
  tenant-ci:                      # the CI persona's project
    auth:
      auth_url: https://keystone.example.com/v3
      username: soak-ci
      password: <password>
      project_name: soak-ci
      user_domain_name: Default
      project_domain_name: Default
    region_name: RegionOne
  tenant-gardener:                # the Gardener persona's project
    auth:
      auth_url: https://keystone.example.com/v3
      username: soak-gardener
      password: <password>
      project_name: soak-gardener
      user_domain_name: Default
      project_domain_name: Default
    region_name: RegionOne
  tenant-legacy:                  # the Legacy persona's project
    auth:
      auth_url: https://keystone.example.com/v3
      username: soak-legacy
      password: <password>
      project_name: soak-legacy
      user_domain_name: Default
      project_domain_name: Default
    region_name: RegionOne
```

Point each persona at its entry and run the soak:

```console
$ dizzy mix chaos --scenario scenarios/mix/small.yaml --os-cloud soak \
    --set personas.ci.cloud=tenant-ci --set personas.gardener.cloud=tenant-gardener \
    --set personas.legacy.cloud=tenant-legacy
```

The Gardener persona's workers boot into server groups with the
`soft-anti-affinity` policy in the bundled profiles, so a cluster boots on a
lab with fewer compute hosts than it has workers. To make the scheduler put
every worker of a cluster on a host of its own, add
`--set personas.gardener.policy=anti-affinity`; a worker it cannot place then
shows as a failed create, and a later replacement of that worker tries again.
The project needs room for one server group per cluster in its
`server_groups` quota; the quota pre-check stops the run before it creates
anything when it lacks it.

The Legacy persona's servers stay for the whole run and are stopped and
started, resized between `flavor` and `resize_flavor`, and live- and
cold-migrated; their volumes and ports are detached and attached again. The
migrations need the admin role in the persona's project and two usable compute
hosts. Without them the run logs `live migration disabled for this run` and
`cold migration disabled for this run` and goes on without migrations. The
resizes need the flavor `resize_flavor` names, `m1.small` in the bundled
profiles; `--set personas.legacy.resize_flavor=` turns them off instead.

Every persona's teardown prints its own
`deleted N resource(s) for run <id>-<persona>` line, and the run ends with one
leak check across all of them.
To re-query a run that is still live, or one kept with `--no-cleanup`:

```console
$ dizzy mix status --run run-<id>.json --os-cloud soak
```

To reclaim a killed or kept run, pass the record; the record names each
persona's cloud and identity:

```console
$ dizzy mix cleanup --run run-<id>.json --os-cloud soak
```

Without a record, pass the run id together with the scenario and every `--set`
the run used:

```console
$ dizzy mix cleanup --run-id <id> --scenario scenarios/mix/small.yaml --os-cloud soak \
    --set personas.ci.cloud=tenant-ci --set personas.gardener.cloud=tenant-gardener \
    --set personas.legacy.cloud=tenant-legacy
```

`--concurrency` and `--max-parallel` bound each persona separately. See
[Combined runs](../explanation/combined-runs.md) for how the personas share the
server envelope.

## Read the results

The run record carries the standard per-operation metrics plus a `chaos` object:
create / delete / mutate counts, completed create→delete cycles, the live
population's min / mean / max against the controller's target, and time-bucketed
latency and error rates.

```console
$ dizzy neutron report --run run-<id>.json
$ dizzy neutron report --run run-<id>.json --format html > soak.html
```

The HTML report renders the time buckets as a chart, which is the quickest way to
see whether the control plane got slower as the run went on. Compare `popMean`
against `targetFill` to confirm the controller held the population where you
asked.

The record on disk is current to the minute: a churn run rewrites it once a
minute while it runs. `report` works on it at any time and marks a record
written mid-run as incomplete. If the process is killed or its node is lost,
reclaim what the last record lists:

```console
$ dizzy neutron cleanup --run run-<id>.json
```

Resources created in the last minute before a kill are not in that record;
`cleanup --run-id <id>` finds the ones a tag or metadata identifies.

## Export live metrics instead

```console
$ dizzy neutron chaos --scenario scenarios/neutron/medium.yaml --duration 4h --otel
```

The exporter is flushed on exit. See
[Export metrics to OpenTelemetry](export-to-otel.md).
