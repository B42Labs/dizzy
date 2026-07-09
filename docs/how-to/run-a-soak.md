# Run a churn soak with `chaos`

`chaos` treats the scenario as an **envelope** rather than a target. For the
whole duration it creates *and* deletes planned resources at random, seeded
intervals, keeping the live population inside the scenario's counts. It reports
latency and error rates bucketed over time, so degradation shows up rather than
being averaged away.

For what the knobs actually control, see
[The churn engine](../explanation/churn-engine.md).

## Run a built-in profile

All nine profiles ship a `chaos:` block, so they run with no flags at all:

```console
$ dizzy neutron chaos --scenario scenarios/neutron/small.yaml    # 5m
$ dizzy cinder chaos  --scenario scenarios/cinder/small.yaml     # 5m
$ dizzy keystone chaos --scenario scenarios/keystone/small.yaml  # 5m
```

The topology is torn down at the end of the run, followed by a leak check.

## Set the duration

`--duration` is the only hard stop besides Ctrl-C. A flag overrides the
scenario's `chaos:` block:

```console
$ dizzy neutron chaos --scenario scenarios/neutron/medium.yaml --duration 2h
```

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
is deleted and recreated.

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

## Export live metrics instead

```console
$ dizzy neutron chaos --scenario scenarios/neutron/medium.yaml --duration 4h --otel
```

The exporter is flushed on exit. See
[Export metrics to OpenTelemetry](export-to-otel.md).
