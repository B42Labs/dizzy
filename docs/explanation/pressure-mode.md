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
Three more changes, C5C3/cobaltcore#1463, C5C3/cobaltcore#1464 and
C5C3/cobaltcore#1465, change keystonemiddleware itself, which runs inside Nova,
Neutron, Cinder and Glance, so `pressure` can also send its users' tokens to
those services. This page explains why the load is built the way it is.

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
end of the plan's regular users and never enter the pool, so a validation
never fails because another worker took its user's role away a moment
before.

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

The propagation probe, which runs beside the workers, has a generator of its
own, seeded from the run seed. It seeds one generator per probe from it, in
the order the probes start, and each probe draws the token it revokes from its
own. Two probes that overlap therefore draw the same values whichever of them
reaches the pool first.

That is where determinism ends. How far a worker gets into its sequence in the
duration depends on Keystone's latency, which is what the run measures. Which
token a draw picks depends on the order in which other workers put tokens
back. Compare two runs by their rates and percentiles, not by their counts.
See [Determinism and reproducibility](determinism.md).

## What each operation makes visible

The table in #92 maps each CobaltCore change to the load that shows it, and
this build runs every row:

| CobaltCore change | Load | What to compare |
|---|---|---|
| C5C3/cobaltcore#1457, a narrow revocation lookup and no cache flush per logout | `validate` while `revoke` accumulates revocation events | Validate latency per bucket against the cumulative revocations: before the change a slope and a jump after every revocation, after it a flat line |
| C5C3/cobaltcore#1458, one validation and one role resolution per request | `validate` under `grant_churn`, which keeps Keystone's role caches cold | Validate p50 and p95 under churn |
| C5C3/cobaltcore#1461, fewer cache round trips and an in-process cache tier | `validate` warm, with `revoke` and `grant_churn` at weight 0 and 128 workers | Validate p50, and the throughput at a fixed worker count |
| C5C3/cobaltcore#1459, linear implied-role expansion and a bulk role lookup | `effective` for a fan user whose grants carry implied roles, over the stages 500 to 4000 grants, beside `grant_churn`, which keeps Keystone's computed-assignment cache cold | The `effective` latency per stage: before the change it grows about fourfold per doubling of the grants, after it about twofold |
| C5C3/cobaltcore#1463, event-driven token eviction in the middleware | The propagation probe, e.g. `--propagation-every 30s`, next to `probe` with many distinct pool tokens over more than 300 s | The propagation delay, from a revocation to the first 401 at a service: up to 300 s before the change, seconds after it, and fewer slow probes |
| C5C3/cobaltcore#1464, an in-process cache tier in the middleware | `probe` with repeating tokens at a high rate | The probe p50 at each service: it drops by 0.3 to 1.2 ms, which shows over tens of thousands of samples |
| C5C3/cobaltcore#1465, an offline signature and expiry check that rejects a bad token inside the service | `bad_token` next to `validate` | After the change, a bad-token latency of about 0.1 ms at each service and a validate p50 that does not move under the flood |

The first row is why the report's bucket table carries the cumulative
revocations next to each bucket's latency: the revocation lookup's cost grows
with the events Keystone has to scan, so a latency that climbs with the
revocation count is the signature of the unpatched lookup. The third row needs
a warm cache, so a mix without the two operations that flush it, e.g.
`--mix revoke=0 --mix grant_churn=0 --workers 128`. Whether the probes of the
fifth and sixth rows send a service distinct or repeating tokens depends on
the pool's size, `--tokens`, and on the rate of the probes that reach that
service, since a draw picks a pool token at random.

## Probes at the other services

The last three rows measure keystonemiddleware, which runs inside Nova,
Neutron, Cinder and Glance and sees a token only when a request brings it
there. Three parts of a run send tokens to those services: the `probe`
operation, the `bad_token` operation and the propagation probe. See
[`pressure`](../reference/cli.md#pressure) for what each one sends.

Each of them takes a service's endpoint from the catalog of the pool token it
draws, not from the admin's. Cinder's catalog URL carries the project id,
`/v3/<project_id>`, so a Cinder endpoint read from the admin's catalog names
the admin's project, and Cinder refuses a member token of another project
there. The token's catalog also decides which of the four services exist for
that user: a service the catalog does not list is left out, and a `probe` or
`bad_token` draw picks among the listed ones at random. The region and the
interface are those of the `clouds.yaml` entry. A run that sends tokens to the
services needs at least one of the four in the first pool token's catalog,
and fails at priming without one.

Every request is one read, the cheapest authenticated one the service offers:
`GET /limits` at Nova, `GET /v2.0/networks?limit=1` at Neutron,
`GET /v3/<project_id>/limits` at Cinder and `GET /v2/images?limit=1` at
Glance. Each is a read that the `reader` role on the token's project allows,
and its cost in the service does not grow with what the lab holds: the limits
of one project, or the first item of a list. A probe's latency is the
middleware's work on the token plus the read. With the read's cost fixed, the
middleware's work is the part that differs between two builds, and
C5C3/cobaltcore#1464 changes it by a fraction of a millisecond, which a read
that grew with the lab's servers, networks, volumes or images would bury in
its own spread.

The pool users do not hold `reader` from the plan. The plan's roles are the
run's own, created under its name prefix, and a scenario's implied-role rules
link them only to each other, so no service's policy names them. Under their
default policies, Cinder's `GET /v3/<project_id>/limits` and Glance's
`GET /v2/images` ask for `reader` on the token's project, and without it both
would answer every probe with a 403: a policy refusal after the middleware's
work, not the read the probe exists to time. So a run with `probe` or the propagation probe grants the
cloud's `reader` role to every pool user on the project its tokens are scoped
to, before it primes the pool, and tears the grants down with the rest of the
run. Neither a churn user nor a fan user gets the grant, so an `effective`
draw for a fan user lists the grants its stage set, each with the implied
roles its role brings, and nothing more. On a cloud without a `reader` role
the run grants nothing, logs a warning and goes on, and the probes still
reach every service whose read policy does not ask for the role. `bad_token` needs no role at all: keystonemiddleware refuses a
bad token before the service checks any policy.

The propagation probe measures C5C3/cobaltcore#1463 directly. Once per
interval, `propagation.every` or `--propagation-every`, it takes a token out
of the pool, revokes it, puts a fresh token for the same user and project back
as `revoke` does, and sends the revoked token to one service once a second
until the service answers 401. The delay is the time from the revocation to
that 401. Before the change a service keeps accepting the revoked token for as
long as keystonemiddleware caches it, up to 300 s; after it, for seconds. One
poll a second resolves the delay to about a second, fine enough for a number
that drops from minutes to seconds, and a probe against a build without the
change costs a few hundred requests. A poll that ends without a 401 does not
end the probe: a 2xx, a 5xx, a 403 and a poll that runs past `--timeout` each
cost a second until the next poll. Each probe polls one service, and the next
probe the next service in turn. A probe that outlasts the interval does not
hold up the next one: at `every: 30s` and the default ceiling of 10 minutes,
about 20 probes poll at once.

The ceiling, `propagation.timeout` or `--propagation-timeout`, ends a probe
whose service still accepts the token. That probe counts as timed out, and
its delay enters the percentiles as the ceiling, so a percentile at the
ceiling says the service accepted the revoked token at least that long, not
how long. A probe still polling when the run ends is not recorded: the run
ended it, not the service, so it is neither a delay nor a timeout.

`bad_token` sends a service a token it has to refuse and counts the expected
401 as a success, with its latency. The token is either a random string as
long as a pool token, drawn from `A-Za-z0-9-_`, the alphabet of a Fernet
token, or a pool token that `revoke` or the propagation probe revoked earlier
in the run. A revoked token serves only once it is 5 minutes old, which is
keystonemiddleware's default token cache time. A service may still hold a
younger revoked token in its cache as valid, and a draw that sent it would
measure that cache, the subject of the propagation probe, instead of the
rejection path. A service that accepts a bad token shows as `accepted` in the
error breakdown.

The propagation probe's revocations are real revocation events, as those of
`revoke` are. On Keystone 2026.2 each one flushes the token validation cache
for every user of that Keystone, and each adds an event for the revocation
lookup of the first row to scan. The record counts them as `token` calls
under `metrics`, and the `--otel` export as `token`/`revoke`. They are not in
`revocations` or the bucket table's cumulative revocations, which count
`revoke` draws. With the propagation probe on, Keystone therefore holds more
revocation events than the bucket table shows.

## Fan users and stages

The `effective` row needs a user with hundreds to thousands of grants, and a
plan's regular users hold the one to three that `assignments_per_user` draws
for them. A scenario's `distribution.fan_users` adds users with that many
grants, and the stages of its `pressure:` block grow their grants while the
run goes on, so one run measures the listing at several sizes. Without
stages, the 500, 1000, 2000 and 4000 grants of the table would be four runs,
four records and four provisionings of 4000 projects. A scenario's
`resources.implied_roles` lets the roles of the fan users' grants imply other
roles of the plan; without such rules, Keystone expands nothing for the
listing. See
[Fan users](../reference/scenario-schema.md#fan-users) and
[Implied roles](../reference/scenario-schema.md#implied-roles) for the keys
and a scenario for C5C3/cobaltcore#1459.

The fan users are extra users, not pool users given more grants. They are
drawn after everything else in the plan, so a scenario with fan users keeps
the regular users, their grants and with them the token pool it had without
them, and the other operations go to the same users either way. A fan user
holds a token of its own outside the pool, which no `revoke` or `issue` takes
away, and `grant_churn` never touches its grants. An `effective` draw
therefore lists the grants its stage set, each with the implied roles its
role brings, and not grants a worker changed a moment before.

A stage's grants are hundreds to thousands of writes to Keystone. A draw that
ran beside them would measure Keystone under that write load, and a listing in
their middle would see a count between two stages. So at the end of a window
the workers finish the draws they have in flight and wait, the run makes the
grants with no draw in flight, and the workers resume in the next stage: every
draw of a stage lists the grants the stage names. The pause costs time, not
accuracy: the grants' time comes out of the stage's window, and the stage's
throughput divides by the time after them.

A stage only adds grants. A fan user starts the run with a count drawn from
`fan_assignments`, anywhere up to `fan_assignments.max`, and a first stage
below that maximum could leave a fan user holding more than the stage names.
So the first stage that sets `fan_assignments` sets at least
`fan_assignments.max`, every later one at least the one before, and the
stage's `fan_assignments` is the count every fan user holds while the stage
runs. With `min` equal to `max`, every fan user starts at that count.

The grants of a stage are not drawn. Each fan user gets the first projects in
plan order that it holds no grant on, with the plan's roles in turn, so two
runs of the same scenario and seed grant the same projects with the same roles
at every stage, whatever order Keystone answered the grants in. Their
listings then work on the same grants, which is what a comparison of the two
runs needs.

## Comparing two runs

Every proof in #92 comes down to the same scenario and seed, run once against
a build with a CobaltCore change and once against a build without it.
`keystone report --run <without>.json --compare <with>.json` reads the two
records and prints both runs' p50, p95, p99 and throughput per operation, per
stage and per service, and their propagation delays, with the difference; see
[Comparing two runs](../reference/cli.md#comparing-two-runs).

The comparison first checks that the two runs sent the same load: the
scenario, the seed, the workers, the token pool, the mix, the propagation
probe's interval and ceiling, and the stages, their number and each one's
`fan_assignments`. Each of them changes what the run sends, and a delta
between two runs that differ in one would mix the change under test with the
change of the load. The command refuses such records and names every
difference. `--allow-mismatch` compares them anyway, for a reader who changed
one on purpose, and prints the differences above the tables. What the
comparison cannot check is the build each run measured; the records do not
carry it.

Every delta is the other run minus the base. With the build without the
change as the base, a negative latency delta and a positive `ops/s` delta are
the change's effect, and `CHANGE` puts the delta in proportion to the base
value. Each percentile is an estimate within 1% of the exact value, so a
change of a percent or two is inside the error of the two estimates. Two runs
of the same build differ as well; run the base twice to see by how much,
since a delta below that spread says nothing about the change. In a staged
run, read the `Stages` table: each of its stages compares the two runs at the
same number of grants.

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

Some of these changes move a latency by less than a millisecond:
C5C3/cobaltcore#1461 the validate latency, C5C3/cobaltcore#1464 the probe
latency. A client measures that latency plus its own network path to Keystone
or to the service, and a path through a WAN, a VPN or a load balancer in
another site can add more spread than the change is worth. Runs that look for
sub-millisecond deltas belong on a client close to the cloud:
the [container image](../how-to/install-and-verify.md#use-the-container-image)
running in the cluster next to Keystone. The table report rounds the pressure
latencies to the microsecond, and the collector's percentile estimates stay
within 1% of the exact value, 20 µs at a 2 ms median.

## A lab, never a shared cloud

Every `revoke` is a real revocation event. On Keystone 2026.2 each one flushes
the token validation cache for every user of that Keystone, not only for the
run's own users, so a pressure run with a positive `revoke` weight slows down
every service's validations for as long as it runs. The revocations of the
propagation probe do the same, one per interval. `bad_token` floods Nova,
Neutron, Cinder or Glance with tokens they have to reject; it exists to load
the services' rejection path. `probe`, `bad_token` and the propagation probe
are all off by default. The verb also needs the admin tier, since validating
and revoking other users' tokens and listing their role assignments are admin
operations; see [Keystone's privilege model](privilege-model.md).

`pressure` therefore belongs on a lab or on the
[`devstack-c5c3`](../how-to/run-against-devstack-c5c3.md) stack, never on a
shared cloud. Like `chaos`, it tears down what it created at the end of the
run or when interrupted, and runs a leak check; `--no-cleanup` is the only
opt-out.
