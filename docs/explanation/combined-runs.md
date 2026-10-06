# Combined runs

`nova chaos` churns one workload in one project. A cloud in production serves
several tenants with different habits at once: a CI system boots short-lived
servers and throws them away, a legacy tenant keeps servers for months and
resizes them, a Kubernetes cluster scales node groups up and down. When the
error rate of a soak rises, the useful question is which of these patterns
caused it.

`mix chaos` runs several such patterns, called **personas**, side by side in
one run and keeps them apart in the results. This build ships three personas:
`ci`, whose servers live for minutes; `gardener`, whose servers form
Kubernetes clusters in anti-affinity server groups and are replaced one worker
at a time; and `legacy`, whose servers live for the whole run and are changed
in place. Next to the personas, a run can add the churn of single services as
background lanes. This page explains how the combined run is put together and
why.

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
compute hosts, and the pre-check turns them off without these.
`cold_migration: false` in the Legacy block turns cold migration off and
leaves live migration to the pre-check. An empty `resize_flavor` turns resize
off. Stop and start is always on.

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

## Clusters that roll

Gardener runs Kubernetes clusters on OpenStack. The worker nodes of a cluster
boot into one server group with an anti-affinity policy, so the scheduler puts
them on different compute hosts, and a rolling update replaces them one after
another for as long as the cluster exists. The Gardener persona brings both to
the cloud: placement constraints that compete for hosts with the CI churn and
the Legacy migrations, and a steady sequence of one delete and one create per
cluster.

A cluster is one network, one server group and the workers booted into that
group on that network, each with one data volume. The persona's servers are
spread over `clusters` clusters round-robin, so cluster sizes differ by at most
one, and every group has the block's `policy`. `anti-affinity` fails a worker
the scheduler cannot put on a host of its own, so a cluster needs as many
compute hosts as it has workers; `soft-anti-affinity` spreads the workers as
far as the hosts allow. The bundled profiles use the soft policy so that they
run on a small lab.

The replacement is a third node lifecycle of the churn engine, next to
churned and pinned. The networks and server groups are pinned. Every worker and
its volume carry the name of the worker's group as their **Roll** value, and
the nodes that share one form a **rolling set**, whose roots are the workers.
The engine creates absent rolling nodes before anything else, as it does pinned
ones. Once every node of a set is present, a worker may be drawn for a delete,
and the engine then deletes the worker's volume and the worker in the same
step, children first. The steps that follow create the worker and then its
volume again under the same names.

A replacement deletes the worker first and creates it afterwards. There is no
surge server: booting the replacement before the old worker is gone would need
a spare server per cluster beyond the persona's share. While a worker is
replaced, its cluster runs one worker short.

The engine decides a replacement from its logical inventory alone. It does not
wait for a delete to return before it decides the create, or for a create to
return before it rolls the next worker. The order in the cloud comes from
operation dependencies instead: every delete of a roll waits for every earlier
operation of the same set, and a create waits for the delete before it. The
cloud therefore sees the replacements of one cluster strictly one after
another, while two clusters replace concurrently. Reading the outcomes would
make the next decision depend on how fast the cloud answered, and the schedule
would stop being a function of scenario, seed and settings.

A worker whose boot failed is still present in the inventory, since the engine
marks a node present when it decides to create it, so its cluster counts as
whole. A later roll that draws the worker deletes whatever the failed create
left, nothing at all when the create produced no server, and boots it again.
Until then the cluster runs one worker short, and a roll of another worker of
the cluster can take a second one down. A failed create of a network or a
server group is not repaired, as for every pinned node, and the workers that
need it are skipped.

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
project. For the Gardener persona the check also counts its server groups
against the `server_groups` limit and its largest group against the
`server_group_members` limit, which Nova applies to each group on its own.
When two personas, or a persona and a background lane, name entries of the
same project, each check passes on its own even when both plans together do
not fit. The run does not reject this setup, since the suffixed identities
keep their resources apart. It logs `lanes share a project; each quota pre-check saw only its own
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

## Background lanes

The personas drive Nova, Neutron and Cinder, but they touch Glance only to look
up the boot image and Keystone only to get tokens. A **background lane** adds
the churn of one service to the run: `cinder`, `glance`, `keystone` or
`neutron`. It is the node graph that service's own `chaos` command builds, run
in a lane of its own next to the personas. Image or identity churn and the
personas' error rates then land in one record, where running a second `chaos`
command by hand would give a second seed, a second record and a second leak
check, to be lined up by wall-clock time.

A lane is switched on in the scenario's `lanes:` block and is off by default.
Its plan comes from the service's bundled profile or from a scenario file the
operator names. It has no share, because it does not divide the server
envelope, and it never runs alone: a combined run still needs a persona with a
server, and the single-service `chaos` commands already cover one service on
its own.

A lane's identity follows the persona rule, the run id with the lane name
appended, `<runID>-<lane>`, so its teardown, leak check and `mix cleanup`
reach only its own resources. Its seed is derived from the run seed and the
lane name the way a persona's is, so the seed in the lane's scenario has no
effect and the whole run still replays from one seed.

A lane keeps its own rhythm. Its interval, churn ratio, target fill, mutate
ratio and fan-out come from the `chaos:` block of its own scenario, the values
its service's `chaos` command would run with. Only what makes the lanes one
run is shared: the duration, the unbounded mode of `--duration 0`, the bucket
width and the signal that interrupts the run.

An enabled lane either runs or stops the run before it starts. Each lane runs
the read-only pre-checks of its service's `chaos` command: the Keystone
privilege pre-check, the Neutron and Cinder quota pre-checks, the lookup of
the volume type and of the external network, and the `clouds.yaml` entry it
authenticates with. A soak that runs for hours without the churn the operator
asked for measures something else, and nothing in its record would say so. A
failing pre-check therefore ends `mix chaos` with an error that names the
lane, before a single resource exists; an operator on a cloud without the
rights switches the lane off.

That rule decides one ordering. `keystone chaos` creates its domains and roles
right after its own pre-check. In a combined run the Neutron lane's pre-check
comes after the Keystone lane's, and domains created before it fails would
have to be torn down again. The Keystone lane therefore creates its scaffold in
a step of its own, once every persona and every lane has passed its
pre-checks. When that step fails, the run deletes what the step created and
stops. The domains and roles it created join the record as the lane's first
resources, so the teardown and `mix cleanup` reclaim them.

A lane that names no `clouds.yaml` entry authenticates with `--os-cloud`, the
project of every persona that names none either. It then shares that
project's quota with them, and like a persona's its quota pre-check saw only
its own plan.

## Opt-in services

A mix scenario can list opt-in services under `services`. Such a service binds
its own resources to every persona's lane: it adds nodes to the lane's engine
and wraps the lane's teardown and leak check so its resources go first. This
build supports none, so `mix generate`, `mix chaos`, `mix status` and
`mix cleanup` reject a scenario or record that names one before they touch the
cloud.
