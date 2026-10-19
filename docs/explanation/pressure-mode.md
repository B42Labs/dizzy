# Pressure mode

`chaos` measures how a control plane holds up while resources come and go.
`keystone pressure` measures one path instead: the validation of an existing
token, the revocation lookup inside it, and the listing of a user's effective
role assignments. Every OpenStack service walks that path through
keystonemiddleware whenever its token cache misses, so its latency is added to
every API call whose token the service has not cached yet.

CobaltCore is changing that path (C5C3/cobaltcore#1457,
C5C3/cobaltcore#1458, C5C3/cobaltcore#1459 and C5C3/cobaltcore#1461). The
evidence behind the changes is a one-process profile on SQLite, and
C5C3/cobaltcore#1456 says its absolute numbers do not carry to MariaDB and
memcached. `pressure` exists so that a run against a build with a change and a
run against one without it can be read side by side on a real control plane.
This page explains why the load is built the way it is.

## Why not the churn engine

The [churn engine](churn-engine.md) ticks. It waits a random 200 ms to 3 s
between ticks, fans one to four operations out per tick (`parallel.max: 4` in
the keystone profiles), and its only token operation is a password login,
which Keystone spends about 175 ms on in bcrypt by design. The token latency
of a churn run is therefore bcrypt latency. A validation that became ten times
faster would move no number in its report, and at about 1.6 operations per
second (2.5 per tick on average, one tick every 1.6 s on average) a change of
a fraction of a millisecond drowns in the spread of the samples.

`pressure` is a closed-loop generator. A fixed number of workers each send one
request after another: the next request leaves when the previous answer
arrives, with no scheduled pause. The load is as high as Keystone's answers
allow, and the throughput at a fixed worker count is a measurement in its own
right. A closed loop also keeps at most one request per worker in flight, so
when Keystone slows down the workers slow down with it. A run measures latency
at a known concurrency, not the length of a queue the client built up.

No operation retries. The churn graph retries a create or a delete after a
transient failure; a load generator records what the cloud answered, so each
draw is one attempt and a 503 is a failed sample.

The engine models no resources and keeps no schedule of its own. A builder per
service hands it the operations as closures, so the engine itself is
service-neutral; this build has the Keystone builder only. Around the engine,
the verb reuses the Keystone plan, the privilege pre-check, the provisioning of
`apply` without its token stage, and the run record, checkpoints, teardown and
leak check of `chaos`.

## The token pool

Validation needs tokens to validate. Issuing a fresh token for every
validation would put a bcrypt login in front of every measurement, the cost
this mode exists to leave out. So before the first worker starts, the run
issues a pool of project-scoped tokens over the plan's `(user, project)`
pairs, and the workers validate, list with and revoke those. Priming costs one
login per token: the 1000 tokens of the `large` profile take about 22 s at
the default `--concurrency 8`.

The pool and the users' passwords stay in process memory. They never reach
the run record, the logs or a checkpoint. A token is a credential for as long
as it lives, and a run record is a file people copy, attach to issues and keep
for months. The price is that a killed run cannot resume its pool; a new run
provisions and primes again.

A run can outlast its tokens: the `large` profile runs for an hour, which is
also Keystone's default token lifetime. A refresher re-issues every pool token
that comes within a margin of its expiry, the smaller of 5 minutes and a
quarter of the lifetime. Without it, the end of a long run would validate
expired tokens and measure Keystone's 404 instead of a validation.

The operations change the pool as they go. `revoke` takes a token out,
revokes it, and puts a fresh token for the same user and project back, so the
pool keeps its size while the revocation events accumulate. `issue` replaces a
token without revoking the old one, so it measures the login alone. The users
whose grants `grant_churn` unassigns and assigns again are reserved from the
end of the plan and never enter the pool, so a validation never fails because
another worker took its user's role away a moment before.

## Two generators per worker

Two runs compared side by side have to send the same load. Each worker
therefore gets two generators, both seeded from the run seed in worker order.
The first draws the worker's next operation from the weighted mix. The second
is handed to the operation, which draws a pool token or a churn assignment
from it.

One generator would not do. An operation may draw a different number of
values depending on what the cloud answered, and every extra draw would shift
the operations the worker draws after it. Two runs against two builds would
then send different mixes, and the comparison would compare the mixes. With
two generators, the sequence of operations a worker draws depends on nothing
but the seed and the worker's index.

That is where determinism ends. How far a worker gets into its sequence in the
duration depends on Keystone's latency, which is what the run measures. Which
token a draw picks depends on the order in which other workers put tokens
back. Compare two runs by their rates and percentiles, not by their counts.
See [Determinism and reproducibility](determinism.md).

## What each operation makes visible

The table in #92 maps each CobaltCore change to the load that shows it. The
rows this build can run:

| CobaltCore change | Load | What to compare |
|---|---|---|
| C5C3/cobaltcore#1457, a narrow revocation lookup and no cache flush per logout | `validate` while `revoke` accumulates revocation events | Validate latency per bucket against the cumulative revocations: before the change a slope and a jump after every revocation, after it a flat line |
| C5C3/cobaltcore#1458, one validation and one role resolution per request | `validate` under `grant_churn`, which keeps Keystone's role caches cold | Validate p50 and p95 under churn |
| C5C3/cobaltcore#1461, fewer cache round trips and an in-process cache tier | `validate` warm, with `revoke` and `grant_churn` at weight 0 and 128 workers | Validate p50, and the throughput at a fixed worker count |
| C5C3/cobaltcore#1459, linear implied-role expansion and a bulk role lookup | `effective` | Listing latency as the number of grants grows |

The first row is why the report's bucket table carries the cumulative
revocations next to each bucket's latency: the revocation lookup's cost grows
with the events Keystone has to scan, so a latency that climbs with the
revocation count is the signature of the unpatched lookup. The third row needs
a warm cache, so a mix without the two operations that flush it, e.g.
`--mix revoke=0 --mix grant_churn=0 --workers 128`.

The `effective` row shows little in this build. Its users are the pool users
with the one to three grants the plan draws for them, and the change shows
when a user holds hundreds to thousands; #94 adds those users, the stages that
grow their grants during a run, and the comparison of two run records. Until
then, compare two reports side by side.

The remaining rows of #92 cover the middleware changes C5C3/cobaltcore#1463,
C5C3/cobaltcore#1464 and C5C3/cobaltcore#1465, which only show when a pool
token goes to Nova, Neutron, Cinder or Glance. #95 adds the `probe` and
`bad_token` operations and the propagation probe for them. Their scenario keys
parse today, and a positive value is rejected, so a scenario written for a
later release fails loudly instead of running a different load.

## What a client cannot see

dizzy measures from the client: the time from sending a request to receiving
the answer, on the client's clock. It cannot see the SQL statements a
validation runs, the memcached round trips it makes or which cache answered.
For C5C3/cobaltcore#1461, whose subject is round trips, dizzy shows the effect
on latency and throughput, not the round trips themselves.

With `--otel`, dizzy exports its operation rates, `token`/`validate` among
them, to the same backend the server metrics go to. On a lab where dizzy is
Keystone's only client, the memcached `get` rate divided by dizzy's validate
rate is the round-trip count per validation that C5C3/cobaltcore#1461
quotes. See [Export metrics to OpenTelemetry](../how-to/export-to-otel.md).

## Distance to the cloud

Some of these changes, C5C3/cobaltcore#1461 among them, move the validate
latency by less than a millisecond. A client measures that latency plus its
own network path to Keystone, and a path through a WAN, a VPN or a load
balancer in another site can add more spread than the change is worth. Runs
that look for sub-millisecond deltas belong on a client close to the cloud:
the [container image](../how-to/install-and-verify.md#use-the-container-image)
running in the cluster next to Keystone. The table report rounds the pressure
latencies to the microsecond, and the collector's percentile estimates stay
within 1% of the exact value, 20 µs at a 2 ms median.

## A lab, never a shared cloud

Every `revoke` is a real revocation event. On Keystone 2026.2 each one flushes
the token validation cache for every user of that Keystone, not only for the
run's own users, so a pressure run with a positive `revoke` weight slows down
every service's validations for as long as it runs. The verb also needs the
admin tier, since validating and revoking other users' tokens and listing
their role assignments are admin operations; see
[Keystone's privilege model](privilege-model.md).

`pressure` therefore belongs on a lab or on the
[`devstack-c5c3`](../how-to/run-against-devstack-c5c3.md) stack, never on a
shared cloud. Like `chaos`, it tears down what it created at the end of the
run or when interrupted, and runs a leak check; `--no-cleanup` is the only
opt-out.
