# The churn engine

`apply` builds a topology once. That measures how a control plane handles a
burst of creates against an empty project — a real thing to measure, but not the
thing that breaks clouds.

`chaos` measures the other thing. It runs for a duration, or with
`--duration 0` until it is stopped, continuously creating *and* deleting
resources at random, seeded intervals, and reports latency and error rates
**bucketed over time**. Neutron's agents, Cinder's scheduler and
backend garbage collection, Keystone's token machinery: these tend to degrade
under sustained churn in ways an aggregate over a single build never shows.

## The plan is a ceiling, not a target

The most important design decision is what the scenario *means* in churn mode.

In `apply`, a scenario is a target: build exactly this. In `chaos`, the same
scenario is an **envelope** — an upper bound on the live population. The engine
only ever creates resources that the plan enumerates, and never more of them than
the plan contains.

Two invariants make this safe without any special-casing per resource type. The
engine models the plan as a dependency graph of nodes, and:

- a node may be **created** only when all of its parents are present;
- a node may be **deleted** only when all of its dependents are gone.

So the engine never issues a dependency-violating call of its own making. A
subnet is not created before its network. A network with live subnets is not a
delete candidate. A security group with live ports keeps existing.

A node can also be **pinned**. A pinned node is never a delete candidate, so
once created it stays until the caller's teardown, and so do its parents, since
they keep a present dependent. The engine creates the absent pinned nodes whose
parents are present before it draws anything else, one per decision, so a
pinned population exists from the first ticks on. A graph without a pinned node
never reaches that draw and keeps the schedule it had before pinned nodes
existed. The Legacy persona of `mix chaos` pins every node of its graph; see
[Combined runs](combined-runs.md#servers-that-stay).

This generalizes across services for free. Cinder volumes have no parents;
snapshots are parented on their source volume. The invariant therefore gives the
right lifecycle without a line of Cinder-specific lifecycle code: a snapshot only
exists while its volume does, a volume with live snapshots is never deleted, and
snapshots naturally churn faster than the volumes under them. Keystone's domains
and roles form a stable scaffold, provisioned once; projects, users, and
assignments churn inside it.

## Keeping the population honest

A naive "flip a coin, create or delete" walk does one of two bad things: it
drains the population to empty, or it pins it to the ceiling. Either way you stop
measuring churn and start measuring an idle project or a full one.

So each action's create probability is computed rather than fixed:

```
p(create) = clamp(churn_ratio + (target_fill − current_fill), 0, 1)
```

Read it as a proportional controller. `current_fill` is the fraction of the plan
currently live. When the population sits exactly at `target_fill`, the correction
term is zero and `p(create)` is exactly `churn_ratio` — the neutral bias at
equilibrium. Below target, the term is positive and creates dominate; above
target, negative and deletes dominate.

The result is a population that oscillates around `target_fill` instead of
wandering off. The run record's `popMin` / `popMean` / `popMax` next to
`targetFill` let you confirm it did.

`churn_ratio` and `target_fill` are therefore doing different jobs, and it is
worth not confusing them. `target_fill` says *how full* the envelope should be.
`churn_ratio` says *how fast* it churns once it is there — at `0.5`, creates and
deletes balance and turnover is maximal.

## Readiness is part of the operation

A `creating` Cinder volume cannot be snapshotted, extended, or reliably deleted.
A Neutron port that has not gone `ACTIVE` may not yet be wired.

So a create operation is not "the API returned 201" — it completes when the
resource reaches its expected state, and the wait is recorded as time-to-ready. A
delete completes only once the resource is actually gone. A resource that lands
in `error` or `error_extending` surfaces as a **failed operation** in the
statistics, not as a wedged engine.

This is why the churn engine can safely pick any live node as a delete candidate:
"live" means ready, not "create returned".

## Mutations

Create and delete change the population. Some interesting operations don't.

Cinder's **extend**, Keystone's **token issue**, Nova's **server lifecycle**, and
Glance's **image lifecycle** are modeled as *mutations*: with probability
`resize_ratio` (or `token_ratio`, or `lifecycle_ratio`), a churn step mutates an
existing resource instead of changing the population. A volume is extended to its
plan's target; a user with a live project assignment authenticates for a scoped
token; a live server is stop/started, resized-and-confirmed, or live-migrated, in
that fixed precedence; a live image is metadata-churned, shared with a member
added/accepted/removed, deactivated-and-reactivated, and flipped to community and
public visibility, in that fixed precedence.

Because mutations leave the population untouched, the controller's economics
above are unaffected. They still count against the per-tick fan-out, since they
are API calls like any other.

Each resource *instance* is mutated at most once per lifetime — a volume is
extended to its planned target and not further, a grant issues one token, a
server runs its planned lifecycle once, an image runs its planned lifecycle once
— and is re-armed when it is deleted and recreated. For Cinder this is what keeps a
week-long soak's gigabyte consumption inside the envelope that the quota
pre-check validated (the sum of planned final sizes), rather than growing without
bound.

A pinned node is exempt from that bound: it is never deleted, so it has a single
lifetime, and the engine may draw its mutation any number of times in it. Each
of those mutations still waits for the node's previous operation, so they run
one after another in decision order. Only the Legacy persona of `mix chaos` pins
nodes, and its changes alternate (a resize goes to the other flavor, a detached
volume or port is attached again), so its population stays inside the envelope
however often it changes.

## Determinism

The scheduler is single-threaded and seeded. Every decision — the delay before
the next tick, the fan-out for it, create-versus-delete, which node — is
determined by `scenario + seed + chaos settings`.

The completion order of the dispatched calls is not, since they run concurrently
through the same bounded worker pool `apply` uses. So a problematic run replays
its *decision schedule* exactly, while the cloud is free to respond differently.
See [Determinism](determinism.md).

## Running without an end

`--duration 0` turns the soak into a run that ends only when it is stopped, for
a lab that stays under load between releases. A run that may last weeks cannot
hold on to everything until its end the way a short run can, so its state is
fixed in size:

- The collector adds every API sample to counters and a log-bucket histogram per
  resource kind instead of storing it. A histogram has at most 2,185 keys however
  many samples it counts; the price is that run-level percentiles are estimates
  within 1%.
- The engine keeps an operation's raw latency only in the time bucket of its
  decision, and only until that bucket is sealed. Once the scheduler has moved
  past a bucket and none of its operations is still in flight, the bucket's
  statistics are computed and the raw data is dropped, so bucket percentiles stay
  exact.
- The decision log is not kept. A bounded run keeps it because the determinism
  tests read it; an unbounded run only counts its creates, deletes and mutates.

The time series is the one part that grows: one bucket per `--bucket-width`, an
hour by default. Ten equal buckets, as a bounded run has, would each span days
after a few weeks and hide the degradation they exist to show. No bucket is
dropped or merged, since the earliest ones are the baseline the later ones are
compared with.

Every chaos run, bounded or not, rewrites its run record once a minute while it
goes on, so a killed process or a lost node costs at most about a minute of
record. The checkpoint
is taken on the scheduler goroutine, between two ticks. The scheduler owns the
population state and knows which operations are in flight, so the snapshot sees
a consistent picture: a create still in flight is left out until it has a cloud
id, and a resource whose delete is still in flight stays listed. Driving the
checkpoint from the engine's clock also lets the tests run it in virtual time.
The cost is that a slow disk delays the next tick by as long as the write takes.

## Teardown is the default

A churn run tears its resources down at the end of the run **or when
interrupted**. The run record is written first, then the teardown runs on a
context that survives the signal, followed by a leak check. For a run with
`--duration 0` the signal is simply how it ends. Before that, the record on disk
is a checkpoint at most about a minute old, so even a hard kill leaves
`cleanup --run` a list to work from.

That ordering is deliberate: the first Ctrl-C should clean up, not abandon. If
you want to interrupt and inspect what is live, `--no-cleanup` is the explicit
opt-out — it leaves everything in place for a later `cleanup`. A second signal
aborts hard, and the run record plus the tag sweep remain as the recovery path.
