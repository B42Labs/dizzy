# The churn engine

`apply` builds a topology once. That measures how a control plane handles a
burst of creates against an empty project — a real thing to measure, but not the
thing that breaks clouds.

`chaos` measures the other thing. It runs for a duration, continuously creating
*and* deleting resources at random, seeded intervals, and reports latency and
error rates **bucketed over time**. Neutron's agents, Cinder's scheduler and
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

## Determinism

The scheduler is single-threaded and seeded. Every decision — the delay before
the next tick, the fan-out for it, create-versus-delete, which node — is
determined by `scenario + seed + chaos settings`.

The completion order of the dispatched calls is not, since they run concurrently
through the same bounded worker pool `apply` uses. So a problematic run replays
its *decision schedule* exactly, while the cloud is free to respond differently.
See [Determinism](determinism.md).

## Teardown is the default

A churn run tears its resources down at the end of the run **or when
interrupted**. The run record is written first, then the teardown runs on a
context that survives the signal, followed by a leak check.

That ordering is deliberate: the first Ctrl-C should clean up, not abandon. If
you want to interrupt and inspect what is live, `--no-cleanup` is the explicit
opt-out — it leaves everything in place for a later `cleanup`. A second signal
aborts hard, and the run record plus the tag sweep remain as the recovery path.
