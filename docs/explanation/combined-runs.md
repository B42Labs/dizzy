# Combined runs

`nova chaos` churns one workload in one project. A cloud in production serves
several tenants with different habits at once: a CI system boots short-lived
servers and throws them away, a legacy tenant keeps servers for months and
resizes them, a Kubernetes cluster scales node groups up and down. When the
error rate of a soak rises, the useful question is which of these patterns
caused it.

`mix chaos` runs several such patterns, called **personas**, side by side in
one run and keeps them apart in the results. This build ships one persona,
`ci`. This page explains how the combined run is put together and why.

## One engine per persona

The [churn engine](churn-engine.md) takes its interval, churn ratio and target
fill as settings of the whole engine. A CI persona churns fast and keeps its
envelope half full; a legacy persona would hold steady and rarely delete. One
engine cannot run at two tempos, so every persona gets an engine of its own,
called a **lane**, with its own config, its own node graph and its own seed.

The lanes start together, run concurrently and stop together: they share the
duration of the `chaos:` block and the signal that interrupts the run. Each
lane bounds its own fan-out, so `--concurrency` and `--max-parallel` apply to
every persona separately.

A persona's seed is the run seed XOR the FNV-64a hash of the persona's name,
the derivation Glance uses for its payloads. The whole run therefore replays
from one seed, and two personas never draw the same schedule. See
[Determinism and reproducibility](determinism.md).

## Shares divide servers

`resources.servers` is the server envelope of the whole run, and each
persona's `share` decides how many of those servers it may keep alive. A share
is not an operation budget: each lane draws its own tempo from its own
settings, and the share only bounds how large its population can grow.

The division uses the largest-remainder method because it always hands out
exactly `resources.servers` servers and because it is deterministic: every
persona gets the whole part of its quota, and the leftover servers go to the
largest fractional remainders, ties to the persona whose name sorts first. A
persona whose part rounds down to zero drops out of the plan.

## One project per persona

Each persona authenticates with the `clouds.yaml` entry its scenario block
names, so each persona can run in a project of its own, the way tenants are
separated. A persona that names no entry uses `--os-cloud`. dizzy creates no
project: that needs administrative rights the tool does not assume, so the
operator supplies one entry per persona.

## Why the identity carries the persona

dizzy stamps every resource it creates with a run identity and cleans up
strictly by it (see [Resource identity and cleanup](resource-identity.md)). In
a combined run the identity is the run id with the persona name appended,
`<runID>-<persona>`, so the CI persona's first server in run `a1b2c3d4` is
`dizzy-a1b2c3d4-ci-srv-0001` and carries `dizzy:run=a1b2c3d4-ci`.

A shared run id would not be safe. Neutron lists by tag across every project
when the caller is an administrator, so the teardown of one persona would find
the networks and ports of another persona in another project, delete them
while that persona still runs, or count them as leaked. With the suffix, each
persona's teardown, leak check and `mix cleanup` reach only that persona's
resources, whoever the caller is and whichever project the resources are in.

## Quota pre-checks see one persona

Before anything is created, each lane resolves the image and flavor and runs
the compute quota pre-check of `nova chaos` against its own plan in its own
project. When two personas name entries of the same project, each check passes
on its own even when both plans together do not fit. The run does not reject
this setup, since the suffixed identities keep the two personas' resources
apart. It logs `personas share a project; each quota pre-check saw only its own
plan` and goes on, with a weaker pre-check.

## One record, no merged time series

A combined run writes one run record with `service: "mix"`. Its top-level
metrics are exact: every persona's client records into a collector of its own,
and each of those also records into one overall collector, so the overall
numbers are the aggregate of every call of every persona.

The churn statistics are a different matter. An engine's time buckets hold
percentiles computed from that engine's own samples, and the p99 of two
engines together is not a function of their two p99s. A merged series would
show numbers that no sample produced. The record therefore has no top-level
`chaos` object; each persona carries its own, and `report` draws each
persona's series separately.

## Opt-in services

A mix scenario can list opt-in services under `services`. Such a service binds
its own resources to every persona's lane: it adds nodes to the lane's engine
and wraps the lane's teardown and leak check so its resources go first. This
build supports none, so `mix generate`, `mix chaos`, `mix status` and
`mix cleanup` reject a scenario or record that names one before they touch the
cloud.
