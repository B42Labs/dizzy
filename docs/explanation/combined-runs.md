# Combined runs

`nova chaos` churns one workload in one project. A cloud in production serves
several tenants with different habits at once: a CI system boots short-lived
servers and throws them away, a legacy tenant keeps servers for months and
resizes them, a Kubernetes cluster scales node groups up and down. When the
error rate of a soak rises, the useful question is which of these patterns
caused it.

`mix chaos` runs several such patterns, called **personas**, side by side in
one run and keeps them apart in the results. This build ships two personas:
`ci`, whose servers live for minutes, and `legacy`, whose servers live for the
whole run and are changed in place. This page explains how the combined run is
put together and why.

## One engine per persona

The [churn engine](churn-engine.md) takes its interval, churn ratio and target
fill as settings of the whole engine. The CI persona churns fast and keeps its
envelope half full; the Legacy persona holds every resource and never deletes.
One engine cannot run at two tempos, so every persona gets an engine of its own,
called a **lane**, with its own config, its own node graph and its own seed.

The lanes start together, run concurrently and stop together: they share the
duration of the `chaos:` block and the signal that interrupts the run. Each
lane bounds its own fan-out, so `--concurrency` and `--max-parallel` apply to
every persona separately.

A persona's seed is the run seed XOR the FNV-64a hash of the persona's name,
the derivation Glance uses for its payloads. The whole run therefore replays
from one seed, and two personas never draw the same schedule. See
[Determinism and reproducibility](determinism.md).

## Servers that stay

A server that lives for minutes never reaches the state in which a long-lived
workload fails. Defects in resize, migration and attachment handling often
depend on history: the fifth resize of one server, a migration after several
detach and attach cycles, a port attached again to a server that has moved
twice. The Legacy persona builds that history by keeping its servers for the
whole run and changing them in place.

One node property of the churn engine makes this possible. A **pinned** node is
never a delete candidate, and the engine creates the absent pinned nodes whose
parents exist before it draws anything else, so the Legacy population exists
from the first ticks on and stays until the teardown. A pinned node is also
exempt from the engine's rule that an instance is mutated at most once in its
lifetime, since it has only the one. Every node of the Legacy graph is pinned.
The lane runs with a mutate probability of 1, so once its resources exist, every
step is a mutation.

A mutation of a kept server runs one operation: a stop and start, a resize, a
live migration or a cold migration. The operation is drawn per mutation,
uniformly among the operations enabled for that server, from a generator
seeded with the persona seed XOR the FNV-64a hash of the server name. The
engine runs one node's mutations one after another in decision order, so the
n-th mutation of a server always gets the n-th draw, and the engine's decision
log says only `mutate`. Both migrations need the admin role and two usable
compute hosts, and the pre-check turns them off without these. An empty
`resize_flavor` turns resize off. Stop and start is always on.

The changes alternate instead of repeating, so the run stays inside what the
quota pre-check validated. A resize goes to `resize_flavor` while the server is
on `flavor` and back to `flavor` afterwards, and the pre-check already sizes a
resized server by the larger of its two flavors. A data volume or port is
detached by one mutation and attached again by the next, so it stays one
resource however often it moves. A toggle changes its state only when its step
succeeded, so a failed resize, detach or attach is tried again in the same
direction. A step whose wait gave up may still finish in the cloud, so the
retry can find the change already made: an attach of a volume or port that is
already attached counts as done, and a resize to the flavor the server already
has turns around to the other flavor.

A kept server whose boot failed is not replaced. The engine marks a node present
when it decides to create it, so it never schedules that create again, and it
skips the node's later mutations. Replacing the server would make the engine
read an operation's outcome, and the schedule would stop being a function of
scenario, seed and settings. The failure shows as a failed create, and the
persona runs with one server fewer.

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
