# Scenario, plan, and run

`dizzy` keeps three things apart that a simpler tool would fuse into one: what
you *asked for*, what that *concretely means*, and what *actually happened*. Each
is a separate artifact with a separate lifetime.

```
scenario.yaml ─┐
               ├─►  generate  ─►  plan.json  ─►  apply  ─►  run-<id>.json  ─►  report
   seed ───────┘                     │                          │
                                     └────────►  verify  ◄──────┘   (against OVN/OVS)
```

## Scenario

A **scenario** is a high-level, parametrized description of a workload: counts,
ranges, ratios, topology shape, and an RNG seed. It is human-authored, lives in a
YAML file, and says nothing about any particular cloud. `resources.networks: 100`
is a scenario. So is `ipv6_ratio: 0.2`.

A scenario is small enough to read in one screen and to diff meaningfully in a
pull request. That is deliberate: it is the surface an operator tunes.

## Plan

A **plan** is the concrete, fully expanded set of resources and their
relationships, produced deterministically from `scenario + seed`. Every network,
every subnet, every router interface, every security-group rule is enumerated
with its intended attributes and a *logical name* — `net-0001`, `port-0003` —
that other plan entries refer to it by.

The plan is the **expected state**. Two consequences follow from making it a
first-class artifact rather than an intermediate value inside `apply`:

**It can be inspected before it is executed.** `generate` dumps a plan without
touching an API, and `apply --dry-run` summarizes one. You can see exactly what a
scenario means before creating anything.

**It is the input for verification.** Comparing the API's state against reality —
against the OVN northbound and southbound databases, against OVS flows — needs a
statement of what *should* exist that is independent of what the API happens to
report. The plan is that statement.

Crucially, a plan is **cloud-independent**. It records *intent*, not the
identities of the target cloud's resources. `routers_with_external_gateway_ratio`
marks which routers want an external gateway, but the plan does not name the
external network — that is discovered at apply time. Cinder's plan records that a
volume should end up at 8 GiB, but not which volume type backs it. The same plan
therefore applies to any cloud.

This is why the intent is allowed to be a no-op: on a cloud with no external
network, the floating IPs and gateways are silently skipped rather than failing.
The plan asked for external connectivity *if the cloud has any*.

## Run

A **run** is one execution of a plan against one cloud. It produces the created
resources' cloud IDs, per-operation timings, observed states, and errors. It is
persisted as a run record — `run-<id>.json` — so it outlives the process.

The run record is what makes the tool's later verbs possible. `status` re-queries
the cloud using it. `report` renders its metrics. `cleanup` deletes what it lists.
None of them need the scenario, the plan, or the original process.

A run is also what carries the **identity** of the resources on the cloud: the run
id is the tag, the metadata value, and the name prefix. See
[Resource identity and cleanup](resource-identity.md).

## Why the split matters

The alternative — a tool that reads a config file and starts calling APIs — makes
three things awkward that here are nearly free.

*Reproducibility* becomes a property of the plan rather than of the whole
pipeline. The same `scenario + seed` yields a byte-identical plan, so a run that
misbehaved can be replayed exactly. See [Determinism](determinism.md).

*Comparability* over time follows from the same property. `monitor` expands the
plan once at startup and reuses it for every iteration, so latency trends measure
the cloud, not a different topology each time.

*Verification* has something to verify against. Without a separate expected-state
artifact, checking whether OVN agrees with Neutron means asking Neutron what it
thinks exists — which is exactly the thing under suspicion.
