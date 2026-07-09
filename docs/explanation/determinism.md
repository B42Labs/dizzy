# Determinism and reproducibility

The same `scenario + seed` always expands to a byte-identical plan — stable across
runs, across machines, and across Go versions. This is the property the rest of
the tool leans on, and it costs some care to maintain.

## Why it matters

A load tester that generates a different topology each time can tell you that
something was slow. It cannot tell you *what* was slow, or whether last week's
run and today's are comparable.

Three capabilities follow directly from determinism:

**Replay.** A run that produced a 409 storm, or a router that never went
`ACTIVE`, can be recreated exactly by rerunning with the same seed. The plan is
the same; only the cloud's behavior varies.

**Comparability.** `monitor` expands the plan once at startup and reuses it for
every iteration. Every iteration therefore creates the same resources with the
same shape, and a latency trend over days measures the control plane rather than
a moving target. This is why a fixed seed is the default. Rotating it per
iteration would broaden coverage at the cost of exactly this — if you want that,
run several monitors with different `--seed` values.

**Review.** A scenario diff in a pull request has a knowable consequence, because
`generate` on the new file shows precisely what changes.

## What is seeded

The scenario's `seed` initializes a single RNG that drives every draw the
generator makes, in a fixed order: how many subnets this network gets, whether
this subnet is IPv6, which security groups this port attaches, which two routers
a link joins. The global `--seed` flag overrides the file's value.

Because the order of draws is fixed, an apparently local change can shift
everything downstream. Adding one network changes the RNG's position for every
draw after it. That is expected and harmless — determinism means *the same inputs
give the same output*, not that similar inputs give similar outputs.

## Addresses are allocated, not drawn

CIDRs are not random. They are handed out sequentially from non-overlapping
ranges, so a plan never has to check for collisions and two plans from the same
seed produce identical addresses:

| What | Range | Block size |
|---|---|---|
| Explicit IPv4 subnets | `10.0.0.0/8` | `/24` |
| IPv6 subnets | `fd00::/16` | `/64` |
| Subnet pools | `172.16.0.0/12` | `/16` |
| Router-link transit subnets | `192.168.0.0/16` | `/30` |

Subnets that allocate *from a pool* get no explicit CIDR at all — Neutron picks
one. That is the point of `subnet_from_pool_ratio`: it exercises Neutron's own
allocator on some fraction of subnets while the rest use addresses the plan
already knows.

## What is not deterministic

Determinism ends at the plan. Everything downstream of it is a real distributed
system.

**Cloud IDs.** UUIDs come from the services. Nothing in `dizzy` predicts them;
the run record records them.

**Completion order.** `apply` creates independent resources of the same kind
concurrently, bounded by `--concurrency`. Which of eight parallel network creates
returns first is not knowable and does not need to be — the plan constrains
*what* is created and in what dependency order, not the interleaving.

**Passwords are derived, not stored.** Keystone user passwords are derived
deterministically from `(seed, logical name)` at apply time and held only in
memory for the token-issue step. A run is reproducible without a credential ever
reaching the plan or the run record.

## Determinism under churn

`chaos` extends the property rather than abandoning it. Its scheduler is
single-threaded and seeded: the sequence of decisions — how long to wait, how
many actions to fan out, create or delete, which resource — is fully determined by
`scenario + seed + chaos settings`.

What is *not* deterministic is the order in which those dispatched cloud calls
complete, for the same reason `apply`'s isn't. So a problematic churn run replays
its decision schedule exactly, even though the cloud's responses may interleave
differently. In practice that is the useful half: the schedule is what you want
to reproduce, and the interleaving is what you were testing.
