# CLI reference

A single binary, `dizzy`, with one command namespace per OpenStack service:
`neutron`, `cinder`, `keystone`, `nova`, and `glance`. Each namespace offers the
same verbs.

| Verb | Touches the API | Purpose |
|---|---|---|
| `generate` | no | Expand a scenario into a plan and dump it |
| `apply` | yes | Create the plan's resources, record a run |
| `chaos` | yes | Continuous randomized create/delete churn within the scenario envelope |
| `monitor` | yes | Repeat `apply` → `cleanup` unattended, exporting metrics |
| `status` | yes | Re-query the current state of a run's resources |
| `report` | no | Render metrics from a run record |
| `cleanup` | yes | Delete a run's resources |

`neutron` additionally has `list-networks` (an auth smoke test) and `verify` (a
stub that returns "not implemented yet").

`keystone` additionally has `pressure`, a closed-loop load on Keystone's token
validation, revocation and role listings; see [`pressure`](#pressure).

A sixth namespace, `mix`, runs several workload personas side by side in one
churn run. It has its own subset of the verbs; see
[The `mix` namespace](#the-mix-namespace).

## Global flags

Accepted by every command.

| Flag | Default | Description |
|---|---|---|
| `--os-cloud <name>` | `$OS_CLOUD` | Cloud name in `clouds.yaml` |
| `--concurrency <n>` | `8` | Maximum number of parallel API calls |
| `--timeout <duration>` | `1m` | Per-operation timeout |
| `--seed <int>` | — | Override the scenario's RNG seed |
| `--log-level <level>` | `info` | `debug`, `info`, `warn`, or `error` |
| `--otel` | off | Export metrics via OpenTelemetry OTLP |
| `--version` / `-v` | — | Print the version and exit |

`--otel` is the *only* thing that enables export. The standard
`OTEL_EXPORTER_OTLP_*` environment variables configure the exporter but never
switch it on, so a globally exported `OTEL_EXPORTER_OTLP_ENDPOINT` does not
change the behavior of a command run without `--otel`. See
[Metrics](metrics.md).

Progress output from `apply`, `chaos`, `pressure`, and `cleanup` goes to stderr
at `info` level, so `--log-level warn` silences it while keeping warnings and
errors. The metrics summary and the run-record path always go to stdout.

## Scenario flags

Accepted by `generate`, `apply`, `chaos`, and `monitor` in every namespace, and
by `keystone pressure`.

| Flag | Description |
|---|---|
| `--scenario <path>` | Path to the scenario YAML file (**required**) |
| `--set <key>=<value>` | Override one scenario value; repeatable |

`--set` takes a dotted path into the scenario schema, e.g.
`--set resources.networks=200`. An unknown key is an error:

```console
$ dizzy neutron apply --scenario scenarios/neutron/small.yaml --set resources.bogus=1
error: unknown override key "resources.bogus"
```

Each service has its own schema and therefore its own `--set` keys, so a typo in
one service's scenario keeps failing loudly rather than being silently accepted
by another. See [Scenario schema](scenario-schema.md).

Note that some counts are not the whole story: `--set resources.networks=10` on a
scenario with `router_links: 1` yields **11** networks, because each router link
adds its own dedicated transit network.

## `generate`

Expands a scenario into a plan and writes it as JSON. Never touches the API.

| Flag | Description |
|---|---|
| `--out <path>` | Write the plan to this file instead of stdout |

```console
$ dizzy neutron generate --scenario scenarios/neutron/small.yaml --out plan.json
```

## `apply`

Creates the plan's resources in dependency order, polls their states, and writes
a `run-<id>.json` run record.

Common flags:

| Flag | Description |
|---|---|
| `--dry-run` | Validate the scenario and print the plan summary without making API calls |
| `--keep-on-abort` | On interrupt, leave created resources in place and print the cleanup hint instead of tearing them down |

Namespace-specific flags:

| Flag | Namespace | Description |
|---|---|---|
| `--external-network <name>` | `neutron` | External network for gateways and floating IPs (default: auto-detect the first one) |
| `--volume-type <name>` | `cinder` | Volume type to create volumes with (default: the cloud's default type) |
| `--privilege auto\|admin\|domain-manager` | `keystone` | Privilege tier; default `auto` (detect) |
| `--domain <name>` | `keystone` | In-scope domain for domain-manager mode (default: the token's domain; ignored in admin mode) |
| `--roles <csv>` | `keystone` | Existing roles to reuse in domain-manager mode (default `member,reader`; ignored in admin mode) |

`neutron apply`, `cinder apply`, and `nova apply` run a read-only **quota
pre-check** against the expanded plan and abort with an itemized message before
creating anything if the quotas are insufficient. `keystone apply` runs a
read-only **privilege pre-check** instead, since Keystone has no default
per-resource quotas. `glance apply` runs **no** pre-check: Glance exposes no
project-quota API (its image count and size caps are deployment config), so an
over-limit request surfaces as a fast-failed 413 during the run rather than being
caught up front. See [Deal with the quota pre-check](../how-to/raise-quotas.md)
and [Keystone's privilege model](../explanation/privilege-model.md).

`nova apply` takes no service-specific flags: the boot image, flavor, and resize
flavor are named in the scenario (`image`, `flavor`, `resize_flavor`) and
resolved to cloud ids at apply time — dizzy uploads no image and creates no
flavor. A name that does not exist fails with an actionable list of the
available images or flavors. Its compute quota pre-check counts instances,
cores, and RAM (see [raise-quotas](../how-to/raise-quotas.md)). If the plan
schedules any live migration, `nova apply` also runs a **fail-open
live-migration pre-check**: it needs the admin role and at least two usable
compute hosts, and when either is missing it skips live migration for the run
with a warning and continues — a missing admin capability never aborts a run.

`glance apply` takes no service-specific flags: every image is created by dizzy
with a synthetic, generated data payload of a configurable size, so a run
references no pre-existing image (`resources.images` and
`distribution.image_size_mib` shape it). Image import through web-download and
multi-store placement are out of scope: every image enters through the direct
data-upload path into the cloud's default store. A payload larger than the
default `--timeout` allows may need `--timeout` raised, since each upload is one
operation bounded by it.

**On interrupt.** SIGINT/SIGTERM writes the run record first, then tears the
partial topology down in reverse dependency order, logs the deletion count, and
exits non-zero naming the run id. A second signal aborts hard, leaving the record
for a manual `cleanup`. `--keep-on-abort` skips the teardown. A *successful*
apply always keeps its resources — that is the point of the run record.

## `chaos`

Uses the scenario as a spatial **envelope** rather than a target: for the whole
duration it creates *and* deletes planned resources at random, seeded intervals,
so the live population never exceeds the scenario's counts and only planned
resources are ever created. See [The churn engine](../explanation/churn-engine.md).

Every flag can instead be set in a `chaos:` block in the scenario YAML; flags
override the block. Every built-in profile ships such a block, so `chaos` runs
them with no extra flags.

| Flag | Default | Description |
|---|---|---|
| `--duration <duration>` | — | Total wall-clock runtime (required, via flag or the `chaos:` block). `0` runs until SIGINT or SIGTERM; only the flag selects this, since `duration: 0` in the block means unset |
| `--bucket-width <duration>` | `1h` | Width of one time bucket in the series of a run with `--duration 0`, at least `1m`. A bounded run ignores it and keeps ten equal buckets |
| `--min-interval <duration>` | `200ms` | Minimum random delay between scheduled actions |
| `--max-interval <duration>` | `3s` | Maximum random delay between scheduled actions |
| `--max-parallel <n>` | `--concurrency` | Maximum concurrent in-flight churn operations |
| `--churn-ratio <float>` | `0.5` | Create bias at steady state, 0–1 |
| `--target-fill <float>` | `0.8` | Fraction of the envelope to keep populated on average, 0–1 |
| `--no-cleanup` | off | Leave resources in place at the end of the run *and* on interrupt |

Namespace-specific:

| Flag | Namespace | Default | Description |
|---|---|---|---|
| `--external-network <name>` | `neutron` | auto-detect | As for `apply` |
| `--volume-type <name>` | `cinder` | cloud default | As for `apply` |
| `--resize-ratio <float>` | `cinder` | `0.3` | Probability per churn step of extending a live, not-yet-resized volume to its planned target; `0` disables |
| `--token-ratio <float>` | `keystone` | `0.3` | Probability per churn step of issuing a token as a live, assigned user; `0` disables |
| `--lifecycle-ratio <float>` | `nova` | `0.3` | Probability per churn step of mutating a live server (stop/start, resize, or live-migrate); `0` disables |
| `--lifecycle-ratio <float>` | `glance` | `0.3` | Probability per churn step of mutating a live image (deactivate/reactivate, visibility flip, member add/remove, or metadata churn); `0` disables |
| `--privilege`, `--domain`, `--roles` | `keystone` | | As for `apply` |

By default a churn run tears its resources down at the end **or when
interrupted**, then runs a leak check. `--no-cleanup` is the single opt-out,
leaving them for an explicit `cleanup`.

With `--duration 0` the signal is the normal end of the run. It writes the final
run record, tears down and runs the leak check as a bounded run does when its
duration elapses, and exits 0 when they succeed. With `--no-cleanup` it prints
`churn complete`.

While it runs, every churn run rewrites `run-<id>.json` once a minute, starting
one minute after the start, so a killed process or a lost node leaves a record
at most about a minute old. A run stopped by a signal while operations are still
in flight rewrites it once more before it waits for them. A record written this
way carries `"incomplete": true` and its `finishedAt` is the checkpoint time;
the final record omits the key. A checkpoint that cannot be written logs a
warning and the run goes on.

## `pressure`

`keystone` only. Runs a closed-loop load against Keystone's token validation,
revocation and role-listing path: a fixed number of workers each send one
request after another, with no pause in between, for the whole duration. The
churn of `chaos` never reaches this path, because its only token operation is
a password login. With `probe` or the propagation probe, the run also sends
the pool's tokens to Nova, Neutron, Cinder and Glance, and `bad_token` sends
those services tokens they have to reject. See
[Pressure mode](../explanation/pressure-mode.md).

> **Run `pressure` on a lab, never on a shared cloud.** Every `revoke` and
> every revocation of the propagation probe is a real revocation event, and on
> Keystone 2026.2 each one flushes the token validation cache for every user
> of that Keystone, not only for the run's own users. The verb, with `probe`,
> `bad_token` and the propagation probe, belongs on a lab or on the
> [`devstack-c5c3`](../how-to/run-against-devstack-c5c3.md) stack.

A `pressure:` block in the scenario YAML sets the duration, the bucket width,
the workers, the token pool, the churn users, the mix, the propagation probe
and the stages; the flags override all of these but the stages, which have no
flag. Every built-in Keystone profile ships such a block, so `pressure` runs
them with no extra flags. See
[The `pressure:` block](scenario-schema.md#the-pressure-block).

| Flag | Default | Description |
|---|---|---|
| `--scenario <path>` | — | Path to the Keystone scenario YAML file (**required**) |
| `--set <key>=<value>` | — | Override one scenario value; repeatable. Takes the Keystone scenario's keys; the `pressure:` block has none |
| `--duration <duration>` | — | Total wall-clock runtime of the workers (required, via flag or the `pressure:` block). `0` runs until SIGINT or SIGTERM; only the flag selects this, since `duration: 0` in the block means unset |
| `--bucket-width <duration>` | `1h` | Width of one time bucket in the series of a run with `--duration 0`, at least `1m`. A bounded run keeps ten equal buckets of a tenth of its duration, but a value under `1m` still fails, e.g. `pressure bucket-width must be at least 1m0s, got 30s` |
| `--workers <n>` | `16` | Closed-loop workers, 1 to 1024. `0` takes the block's value or the default |
| `--tokens <n>` | `100` | Size of the token pool. `0` takes the block's value or the default |
| `--churn-users <n>` | `1` | Users reserved for `grant_churn`, taken from the end of the plan's regular users, so a fan user is never one; they never enter the token pool. Reserved only when `grant_churn` has a positive weight; `0` reserves none and then needs a `grant_churn` weight of `0`. A negative value fails with `--churn-users must not be negative, got -1` |
| `--mix <operation>=<weight>` | see below | Weight of one operation; repeatable |
| `--propagation-every <duration>` | `0` | Revoke one pool token at this interval and poll a service with it until it answers 401; `0` switches the propagation probe off, also when given as a flag over a scenario block. A negative value fails with `--propagation-every must not be negative, got -1s`. See **The propagation probe** below |
| `--propagation-timeout <duration>` | `10m` | Ceiling of one propagation probe; a probe still accepted after it counts as a timeout. A negative value fails whether or not the propagation probe is on, and `0` fails while the probe is on, both with `--propagation-timeout must be positive, got <value>`, e.g. `--propagation-timeout must be positive, got 0s` |
| `--no-cleanup` | off | Leave resources in place at the end of the run, on interrupt *and* after a failed setup |
| `--privilege auto\|admin\|domain-manager` | `auto` | Privilege tier. `auto` classifies the caller's token, `admin` asserts the admin tier without classifying it, and `domain-manager` fails before any API call. Only the admin tier runs |
| `--domain <name>` | the token's domain | Without effect, since only the admin tier runs. Setting it logs `--domain and --roles are ignored: keystone pressure runs in the admin tier only` |
| `--roles <csv>` | `member,reader` | Without effect, since only the admin tier runs. A value other than the default logs the same warning as `--domain` |

A worker draws each operation with probability its weight divided by the sum
of the weights, so a weight of `0` switches an operation off. Each `--mix`
sets one weight and leaves the others at the block's value or the default;
when two `--mix` values name the same operation, the later one wins.

| Operation | Default weight | What one draw sends |
|---|---|---|
| `validate` | `60` | `GET /v3/auth/tokens` with the admin token as `X-Auth-Token` and a pool token as `X-Subject-Token`, the request keystonemiddleware makes on a cache miss |
| `effective` | `5` | For a fan user when the scenario has one, otherwise for the user of a pool token: `GET /v3/role_assignments?user.id=<id>&effective=true`, then `GET /v3/users/<id>/projects`, then `GET /v3/auth/projects` sent with that user's token. With fan users, a draw picks one at random, and the token is the fan user's own, outside the pool. The first error ends the draw |
| `revoke` | `2` | Takes a token out of the pool and sends `DELETE /v3/auth/tokens` with it as `X-Subject-Token`, then a password login (`POST /v3/auth/tokens`) for the same user and project, whose token goes back into the pool. The login runs even when the revocation failed. The draw succeeds only when both calls do |
| `grant_churn` | `2` | Unassigns one of the churn users' role assignments (`DELETE /v3/projects/<project>/users/<user>/roles/<role>`, or `/v3/domains/<domain>/...` for a domain-scoped one) and assigns it again with a `PUT` to the same path, so Keystone's assignment caches keep being invalidated. The `PUT` runs even when the `DELETE` failed, and the draw fails with the first error. One worker at a time churns a given assignment |
| `issue` | `1` | A password login (`POST /v3/auth/tokens`) for the user and project of a pool token, whose new token replaces it. The old token is not revoked, so the draw measures the login alone. When the login fails, the old token goes back into the pool |
| `probe` | `0` | Picks a pool token and, at random, one of the services its catalog lists, and sends that service's cheapest authenticated read with the token as `X-Auth-Token`: `GET <compute>/limits` to Nova, `GET <network>/v2.0/networks?limit=1` to Neutron, `GET <block-storage>/limits` to Cinder, whose catalog URL already carries `/v3/<project_id>`, or `GET <image>/v2/images?limit=1` to Glance. One request, whose body is discarded. See **Probes** below |
| `bad_token` | `0` | Picks a pool token and, at random, one of the services its catalog lists, and sends the request of a `probe` with a token the service has to reject in place of the pool token: a revoked pool token or a random one. A 401 is the expected answer and counts as a success; a 2xx fails as `accepted`. See **Bad tokens** below |

Every draw is one sample under its operation's name, whatever calls it made:
a `revoke` sample covers the revocation and the reissue together. No operation
retries, so a failed call is a failed sample. When the reissue of a `revoke`
fails, the pool shrinks by one and the run logs the warning
`token pool shrank; a reissue failed`; a draw that finds the pool empty fails
as `pool-empty`. The error breakdown labels a failure
`canceled`, `timeout`, `forbidden`, `not-found`, `unauthorized` (a 401, e.g.
a service refusing a live pool token), `transient` (a 500, 502, 503, 504 or
429), `other` (any other failure, a 400 or a 501 among them), `pool-empty`,
`no-service` (a `probe` or `bad_token` draw whose token's catalog lists none
of Nova, Neutron, Cinder and Glance) or `accepted` (a service that answered a
`bad_token` draw with a 2xx).

Before it makes any API call, the command checks its input. A mix whose
weights are all `0` fails with
`pressure mix: every weight is 0; at least one operation needs a positive weight`.
A `--mix` value that is not `<operation>=<weight>`, names an unknown operation
or carries a weight that is not a non-negative integer fails with an error
that starts with `--mix:`, e.g. `--mix: unknown operation "nope"`. Without a
duration from the flag or the block, the command fails with
`pressure duration must be set and positive, got 0s`.

A run goes through these steps:

1. **Privilege pre-check.** With `--privilege auto`, the read-only pre-check
   of `apply` classifies the caller's token; `--privilege admin` asserts the
   admin tier and skips the classification. Only the admin tier runs:
   validating and revoking other users' tokens and listing their role
   assignments are admin operations in Keystone's default policy. A detected
   domain-manager tier fails before any write, and `--privilege domain-manager`
   fails before any API call:

   ```console
   $ dizzy keystone pressure --scenario scenarios/keystone/small.yaml --privilege domain-manager
   error: keystone pressure needs the admin tier: validating and revoking other users' tokens and listing their role assignments are admin operations; the caller resolved to tier "domain-manager"
   ```

2. **Provisioning.** Creates the plan's domains, roles, implied-role rules,
   projects, users and role assignments exactly as `apply` does, without
   `apply`'s token stage.
   The fan users and their grants are among them.
3. **Granting the reader role.** After the check of the plan against the
   options below, a run with a positive `probe` weight or with the
   propagation probe looks up the cloud's `reader` role by name and grants it
   to the user of every pool pair, the distinct `(user, project)` pairs the
   pool's tokens are scoped to, on that pair's project. Neither a churn user
   nor a fan user gets it, and a run whose only request to the services is
   `bad_token` looks up and grants nothing. The lookup and each grant retry
   a transient failure as a fan user's grant of a stage does, `--timeout`
   bounding each attempt; the lookup is no call in the metrics summary. At
   most `--concurrency` grants are in flight. The grants count as
   `role_assignment` calls in the metrics summary and as
   `role_assignment`/`create` in the `--otel` export, as the provisioning's
   grants do. They join the run's created resources and go with the teardown;
   the `reader` role is the cloud's and stays. A pool user's `effective`
   listing, which a run without fan users draws, counts the grant too. The
   run then logs
   `granted reader role to the pool users` with `grants`, the number of
   grants, e.g. for the `small` profile with `--mix probe=30`:

   ```text
   level=INFO msg="granted reader role to the pool users" grants=14
   ```

   When the lookup fails, e.g. on a cloud without a role named `reader`, the
   run makes no grant, logs the warning
   `pool users get no reader role; a service whose read policy asks for it refuses the probes with 403`
   with the `error`, `role "reader" not found` for a missing role, and goes
   on. The first failed grant ends the setup with
   `granting reader role (run <id>): granting role "reader" to the pool users (<n> of <m> granted): …`,
   whose `<m>` counts the pool pairs. See **Probes** below for why the run
   needs the role.
4. **Priming the token pool.** Issues `--tokens` project-scoped tokens, round
   robin over the distinct `(user, project)` pairs of the plan's
   project-scoped role assignments in plan order, leaving out the churn users
   and the fan users. Then it issues one more token per fan user, scoped to
   the project of the fan user's first project-scoped grant in plan order;
   these tokens stay outside the pool. Each issue is a password login, which
   Keystone spends about 175 ms on in bcrypt, with at most `--concurrency`
   issues in flight: 1000 tokens at the default `--concurrency 8` take about
   22 s before the first worker starts. The first failed issue stops the
   priming, e.g. `priming token pool (3 of 7 issued): …`, whose counts include
   the fan users' tokens. A run with a positive `probe` or `bad_token` weight
   or with the propagation probe needs at least one of Nova, Neutron, Cinder
   and Glance in the first pool token's catalog. When that catalog lists none
   of them, the priming fails before any token enters the pool with
   `pressure probe needs at least one of nova, neutron, cinder, glance in the pool users' catalog; the first pool token's catalog lists none`.
5. **The workers** run for the duration, or with `--duration 0` until SIGINT
   or SIGTERM. A draw in flight when the run ends finishes or times out on
   its own, and the run waits for it. A run with stages runs them one after
   the other; see **Stages** below.
6. **Results.** Prints the metrics summary and the `Pressure` table to stdout,
   writes `run-<id>.json` with a `pressure` object and prints
   `run record written to run-<id>.json`. See
   [What `report` renders](metrics.md#what-report-renders).
7. **Teardown and leak check**, as for `chaos`: prints
   `deleted <n> resource(s) for run <id>`, then
   `leak check: no run-named resources remain` or
   `leak check: <n> run-named resource(s) still present after teardown`.

**The token pool.** A draw that needs a token picks one from the pool at
random. The pool lives in process memory only: no token and no password
reaches the run record, the logs or a checkpoint, so a killed run cannot
resume its pool. A refresher re-issues every pool token and every fan user's
token that comes within a margin of its expiry, the smaller of 5 minutes and
a quarter of the token lifetime, so a run longer than the lifetime keeps
valid tokens. The lifetime is that of the first pool token. The refresher
looks every half margin, with at most `--concurrency` issues in flight, and
swaps a new token in only after its issue succeeded; a failed issue keeps the
old token until the next look. When the first pool token reports no usable
expiry, the refresher does not start and the run logs a warning. The issues
of the priming and of the refresher count as `token` calls in the metrics
summary and are no pressure operation. `validate`, `revoke`, `issue`,
`probe`, `bad_token` and the propagation probe take their tokens from the
pool only, so a fan user's token is never validated, revoked, replaced or
sent to Nova, Neutron, Cinder or Glance, and `grant_churn` never touches a fan
user's grant.

After provisioning and before the reader grants and the priming, the command
checks the plan against the options, and each failure goes through the
setup-failure path below:

- `pressure needs at least one project-scoped assignment outside the churn and fan users to issue pool tokens from; the plan has none`
- `pressure fan user "<name>" holds no project-scoped assignment to scope its token to`
- `pressure.churn_users (<n>) must leave at least one user outside the churn set; the plan has <m> users`,
  where `<m>` counts the regular users, while `grant_churn` has a positive
  weight
- `pressure grant_churn needs the churn users to hold at least one assignment; the last <n> regular user(s) of the plan hold none`,
  while `grant_churn` has a positive weight, so a run with `--churn-users 0`
  needs a `grant_churn` weight of `0`

**Probes.** `probe`, `bad_token` and the propagation probe send their requests
to the endpoints in the pool token's own catalog, not in the admin's. Every
token the command issues for the pool, in the priming, the refresher, a
`revoke`, an `issue` or the propagation probe, keeps the URLs of Nova,
Neutron, Cinder and Glance from the catalog that comes with it, picked with
the region and the interface of the clouds.yaml entry, and with the public
interface when the entry names none or one other than `internal`,
`internalURL`, `admin` and `adminURL`. Cinder's catalog URL carries the project
id, `/v3/<project_id>`, so every token goes to the Cinder endpoint of its own
project; the admin's catalog would name the admin's project, and Cinder
refuses a member token of another project there. A service the token's
catalog does not list is left out for that token, and a draw picks among the
services that remain. A catalog that cannot be read fails the issue with
`reading the token's catalog: …`. A request to a service never
re-authenticates, so a 401 is the answer to that one request, and a `probe`
draw that gets one fails as `unauthorized`.

A pool user holds only the plan's roles, which are the run's own, and a
scenario's implied-role rules link them only to each other, so no service's
policy names them. Under their default policies, Cinder's
`GET /v3/<project_id>/limits` and Glance's `GET /v2/images` ask for `reader`
on the token's project, and without it they refuse every probe with a 403,
which fails as `forbidden`. A run with `probe` or the propagation probe
therefore grants the pool users the cloud's `reader` role on their projects
before the priming (step 3 above), and the teardown removes the grants with
the rest of the run's resources. `bad_token` gets no grant: a service refuses
its tokens before any policy check.

Every `probe` draw is one sample of `probe` in the `Operations` table, as every
draw is. A draw that sends its request is also one sample under its service in
the record's [`pressure.probes`](run-record.md#probes), which the `Pressure`
table prints as a `Probes` table with one row per service; a draw that fails
as `pool-empty` or `no-service` sends none and is not in it. The `bad_token`
draws go to `pressure.badTokens` and a `Bad tokens` table the same way. Both
tables have the columns of the `Operations` table with `SERVICE` in place of
`OPERATION`, and their `OPS/S` divides by the same time as the `Operations`
table's. After the priming, a run with a positive `probe` or `bad_token`
weight or with the propagation probe logs `probe services` with `services`,
those the first pool token's catalog lists:

```text
level=INFO msg="probe services" services="[nova glance]"
```

**The propagation probe.** `--propagation-every`, or `propagation.every` in
the block, starts a probe that measures how long a service keeps accepting a
revoked token. It runs beside the workers, not as a weighted draw. At every
interval it takes a token out of the pool, revokes it and reissues a token for
the same user and project into the pool, as a `revoke` draw does: the reissue
runs even when the revocation failed, and a failed reissue shrinks the pool
and logs `token pool shrank; a reissue failed`. These revocations are real
revocation events. `revocations` and the buckets' `REVOCATIONS` count the
`revoke` draws only; the probe's revocations count as `token` calls under
`metrics`, and as `token`/`revoke` in the `--otel` export. The probe then
polls one service with the revoked token, sending the request of a `probe`
once a second until the service answers 401. The delay is the time from the revocation to that 401.
The probes take the services in turn, in the order `nova`, `neutron`,
`cinder`, `glance`, among those the first pool token's catalog lists; when the
revoked token's own catalog lacks the service in turn, the probe polls the
first service that catalog lists. A poll that fails another way, e.g. with a
5xx, a 403 or a timeout, does not end the probe; it costs a second until the
next poll. `--timeout` bounds the revoke and the reissue each, and each poll
ends at `--timeout` or at the probe's ceiling, whichever comes first.

A probe whose service has not answered 401 when `--propagation-timeout`, or
`propagation.timeout` in the block, has passed since the revocation counts as
a timeout, with the ceiling as its delay, and logs a warning with `service`
and `timeout`:

```text
level=WARN msg="propagation probe timed out; the service still accepted the revoked token" service=nova timeout=10m0s
```

A new probe starts at every interval while earlier ones still poll, so a slow
probe never delays the next: with an interval of `30s` and the default ceiling
of `10m`, about 20 are in flight at once. A probe skipped before its first
poll records no delay and logs `propagation probe skipped; the token pool is empty`,
`propagation probe skipped; the revoke failed` with the `error`, or
`propagation probe skipped; the token's catalog lists no service`. A probe
still running when the run ends is abandoned without a log line and recorded
neither as a delay nor as a timeout. The end of the run does not cut a revoke
or a reissue short: a probe it catches during either finishes both, each
within `--timeout`, and still puts a new token for the user and project into
the pool. The `starting pressure run` log line carries `propagation`, the
interval or `off`.
The record keeps the interval, the ceiling, the number of probes, the
timeouts and the delays as `pressure.propagation`, and the delays also as the
`revocation` entry of `metrics.readiness`; see
[`propagation`](run-record.md#propagation). The `Pressure` table prints them
as a `Propagation` block after the `Probes` and `Bad tokens` tables, every
duration rounded to the millisecond, with `-` on the `delay:` line when no
probe completed.

**Bad tokens.** A `bad_token` draw picks a pool token for its length and its
catalog's services, then sends a token the service has to reject in its
place. When the run holds a pool token that `revoke` or the propagation probe
revoked at least 5 minutes earlier, about half the draws send one of those.
The others, and every draw before then, send a random string as long as the
pool token, drawn from `A-Za-z0-9-_`, the alphabet of a Fernet token. Five
minutes is keystonemiddleware's default token cache time, so a `bad_token`
draw never measures the cache the propagation probe measures. The run keeps
the last 256 revoked tokens in process memory only, as it keeps the pool. A
401 is the expected answer and counts as a success with its latency; a 2xx
fails as `accepted`, and every other failure keeps its label. The operation
exists to load the services' rejection path and is off by default. It belongs
on a lab, never on a shared cloud.

For a run with `--mix probe=30 --mix bad_token=20 --propagation-every 30s`
whose pool tokens list Nova and Glance, the `Pressure` table continues after
the `Operations` table with:

```text
Probes
SERVICE    OPS     OK  FAILED  OPS/S      MIN      MEAN       P50       P90       P95       P99        MAX
glance   61204  61204       0  170.0  5.912ms  13.873ms  12.604ms  19.311ms  22.147ms   29.85ms  118.402ms
nova     60877  60877       0  169.1  3.481ms   8.226ms   7.519ms  11.642ms  13.408ms  18.733ms   96.115ms

Bad tokens
SERVICE    OPS     OK  FAILED  OPS/S      MIN     MEAN      P50      P90      P95       P99       MAX
glance   40981  40981       0  113.8  2.204ms  5.187ms  4.713ms  7.395ms  8.531ms  11.906ms  64.287ms
nova     40640  40640       0  112.9  2.117ms  4.968ms  4.502ms  7.081ms  8.214ms  11.473ms   58.92ms

Propagation
  every:     30s
  timeout:   10m0s
  probes:    12
  timed out: 0
  delay:     p50 2.1s / p95 4.8s / p99 6.2s / max 6.2s
```

See [What `report` renders](metrics.md#what-report-renders).

**Stages.** A `stages:` list in the `pressure:` block divides a bounded run
into consecutive stages, each of which can grow the fan users' grants; see
[Stages](scenario-schema.md#stages) for its keys and checks. There is no flag
for it. The command gives each stage a window of the run duration, from the
flag or the block. A stage with a `duration` keeps it. The stages without one
share what the others leave equally, the last of them taking the remainder of
the division. When every stage has a duration and they add up to less than
the run duration, the last stage runs to the end. Four stages without a
duration over `--duration 20m` run 5m each, a stage of `2m` and two without a
duration over `5m` run 2m, 1m30s and 1m30s, and stages of `1m` and `1m` over
`3m` run 1m and 2m.

The run enters the first stage before the first worker starts. At the end of
every later window, each worker finishes the draw it has in flight and waits,
so no draw is in flight while the run enters the next stage; the workers
resume once it is entered. Entering a stage with `fan_assignments` grants
every fan user a role on more projects until it holds that many: on the first
projects in plan order it holds no grant on, with the plan's roles in turn.
A grant retries a transient failure as a provisioning grant does. The time a
stage takes to enter comes out of its window, so the run still lasts its
duration. A stage entered after its window ended records no operations, and
the run logs
`stage entered after its window ended; it records no operations`. A stage
whose entry is still running when the run's duration ends is not recorded:
the run logs
`the run ended while a stage was being entered; the stage is not recorded`,
the record and the `Stages` table list only the stages before it, the grants
that entry made are in [`created`](run-record.md#created) and are torn down
with the rest, and the run counts as completed. Size the stage windows so
that their grants fit. Each stage logs `entering stage` with `run` and
`stage`, numbered from 1, and a stage with `fan_assignments` adds
`fanAssignments`, `granted`, the grants it made, and `took`:

```text
level=INFO msg="entering stage" run=1a2b3c4d stage=2 fanAssignments=1000 granted=500 took=6.728945112s
```

The `starting pressure run` log line carries `stages`, the number of stages.
A staged run prints a `Stages` table after the `Operations` table and any
`Probes`, `Bad tokens` or `Propagation` output, with an `all` row and one row
per operation for each stage, e.g. for stages of
`fan_assignments` 500 and 1000 over `--duration 4m`:

```text
Stages
STAGE   FAN  START     END   OPERATION     OPS      OK  FAILED   OPS/S     P50     P95     P99
    1   500  2ms       2m0s  all        214920  214920       0  1791.0  11.8ms  15.6ms  19.4ms
    1   500  2ms       2m0s  effective  143280  143280       0  1194.0  12.4ms  15.9ms  19.7ms
    1   500  2ms       2m0s  validate    71640   71640       0   597.0  2.01ms  2.84ms  3.66ms
    2  1000  2m6.731s  4m0s  all         63560   63560       0   561.1  40.2ms  47.8ms  55.1ms
    2  1000  2m6.731s  4m0s  effective   42373   42373       0   374.1  41.7ms  48.3ms  55.6ms
    2  1000  2m6.731s  4m0s  validate    21187   21187       0   187.0  2.09ms  2.95ms  3.82ms
```

`START` is the offset at which the workers resumed in the stage and `END` the
end of its window; see [What `report` renders](metrics.md#what-report-renders).
The command checks the stages against the run duration before it makes any
API call:

- `pressure.stages need a bounded run; --duration 0 selects a run without an end`
- `pressure.stages: the stage durations add up to 4m0s, more than the run duration 3m0s`
- `pressure.stages: the stages with a duration fill the run duration 3m0s, leaving nothing for the 1 stage(s) without one`

A stage that fails to enter ends the run like a failed setup; see the table
below.

**Global flags.** `--concurrency` bounds the provisioning, the reader grants,
the priming of the pool, the refresher and the grants of a stage, not the
workers; `--workers` sets their number. `--timeout` bounds each draw of a
worker, all of its calls together, each issue of the priming and the
refresher, each provisioning and teardown call, the lookup of the `reader`
role, each attempt of a reader grant or a stage grant, and the revoke, the
reissue and each poll of a propagation probe. `--seed` overrides the scenario seed, which
also seeds the workers' generators and the propagation probe's. `--otel`
exports the client's calls, among them `token`/`validate` and `token`/`revoke`,
the requests to the services under the kinds `probe`, `bad_token` and
`propagation` with the service as the operation, and each propagation delay
as a time-to-ready of kind `revocation`; see [Metrics](metrics.md).

**Connections.** The identity client keeps up to `--workers` plus
`--concurrency` idle connections to the Keystone endpoint, where other verbs
keep 2. Over HTTP/1.1 a smaller pool would close most connections after each
request, and the latency samples would then include a TCP and TLS handshake.
The requests to Nova, Neutron, Cinder and Glance share the identity client's
connection pool. The same limit holds for each service host, but all hosts,
Keystone included, share one total: 100 idle connections, net/http's default,
or the per-host limit when that is larger.

**End of the run.** By default the run tears its resources down at the end or
when interrupted, then runs the leak check, as `chaos` does. With
`--duration 0` the signal is the normal end of the run. `--no-cleanup` leaves
the resources in place and prints the hint to reclaim them, with
`pressure interrupted; …` after an interrupt of a bounded run:

```text
pressure complete; resources left in place — reclaim with: keystone cleanup --run run-1a2b3c4d.json
```

When the run record could not be written, the hint names `--run-id <id>`
instead of `--run`.

A failure before the workers start, while provisioning, checking the plan
against the options, granting the reader role or priming the pool, first
writes a record with `error` set and the resources created so far, the reader
grants made before the failure among them, and prints its path. It then tears
those resources down and prints `deleted <n> resource(s) for run <id>`, with
no leak check, or with `--no-cleanup` prints
`pressure setup failed; resources left in place — reclaim with: keystone cleanup --run run-<id>.json`.
A stage that fails to enter, because one of its grants failed, takes the same
path: the run ends there, and the record carries no `pressure` object and
lists the grants the stages made so far among the resources, so the teardown
removes them too. The command exits non-zero with an error that names the
step and the run id:

| Step | Error |
|---|---|
| Provisioning | `provisioning plan (run <id>): …` |
| Checking the plan against the options | `building pressure set (run <id>): …` |
| Granting the reader role | `granting reader role (run <id>): granting role "reader" to the pool users (<n> of <m> granted): …` |
| Priming | `starting pressure run (run <id>): priming token pool (<n> of <m> issued): …`, or, for a run with `probe`, `bad_token` or the propagation probe, `starting pressure run (run <id>): pressure probe needs at least one of nova, neutron, cinder, glance in the pool users' catalog; the first pool token's catalog lists none` |
| Entering a stage | `running pressure (run <id>): entering stage <n>: …`, e.g. `running pressure (run <id>): entering stage 2: growing fan user "fan-0001" to 1000 assignments (0 granted): …` |

A teardown that fails partway appends
`; teardown incomplete after deleting <n> resource(s): …` and the
`keystone cleanup` hint to the error.

While the run goes on, the command rewrites `run-<id>.json` once a minute,
starting one minute after the run starts, marked `"incomplete": true`, as
`chaos` does. The run starts when the workers start or, in a staged run, when
it begins entering the first stage, so a checkpoint can fall while the first
stage's grants are made and then lists no stage. It writes no checkpoint
during provisioning or priming, and none once the run ends, a signal
included; the final record follows once the draws in flight have returned. A
checkpoint that cannot be written logs a warning and the run goes on.

## `monitor`

Repeats the single-shot pipeline — pre-flight sweep → `apply` → `cleanup` —
unattended, so one installation can be observed over time.

| Flag | Default | Description |
|---|---|---|
| `--interval <duration>` | `0` | Target cadence between iteration *starts*; `0` runs iterations back-to-back |
| `--iterations <n>` | `0` | Stop after this many iterations; `0` runs forever |
| `--error-wait <duration>` | `0` | Extra pause after a failed iteration; `0` is off |
| `--keep-run-records` | off | Write a `run-<id>.json` per iteration |

Namespace-specific:

| Flag | Namespace | Description |
|---|---|---|
| `--external-network <name>` | `neutron` | As for `apply` |
| `--volume-type <name>` | `cinder` | As for `apply` |
| `--reclaim-orphans` | `keystone` | Before each iteration, delete leftover `dizzy-` identity resources across **all** tester runs cloud-wide (off by default) |
| `--privilege`, `--domain`, `--roles` | `keystone` | As for `apply` |

With `--interval` omitted or `0`, the next iteration starts the moment the
previous one finishes. A positive `--interval` is the target time between
iteration starts: a fast iteration waits out the remainder, one that overruns
starts the next immediately. Either way iterations never overlap and no backlog
builds up.

Run records are **off by default** because in a long-running loop they accumulate
unboundedly; `--otel` is the intended way to keep the data.

The plan is expanded once at startup, so every iteration reuses the same seed and
therefore the same topology — that is what makes latency trends comparable
across iterations. To broaden coverage, run several monitors with different
`--seed` values.

> **Do not run `monitor` concurrently with another `dizzy` run in the same
> project.** For `neutron`, `cinder`, `nova`, and `glance` the pre-flight sweep is
> always on and reclaims any tester-created resource in the project, so it would
> tear a concurrent run down mid-flight. For `keystone` the equivalent sweep is opt-in
> via `--reclaim-orphans`, and because an admin token lists cloud-wide it is only
> safe when no other `dizzy` process targets the cloud at all.

**On interrupt.** SIGINT/SIGTERM stops the loop, tears the current iteration
down on a context that survives the signal, flushes the metrics exporter, and
exits. A second signal aborts hard.

## `status`

Re-queries the current state of a run's resources from the API.

| Flag | Description |
|---|---|
| `--run <path>` | Path to the run record to re-query (**required**) |

## `report`

Renders metrics from a run record. Never touches the API. The same command
builder backs all six namespaces, so `dizzy cinder report` and
`dizzy neutron report` are the same code.

| Flag | Default | Description |
|---|---|---|
| `--run <path>` | — | Path to the run record to report on (**required**) |
| `--format <fmt>` | `table` | `table`, `json`, `csv`, or `html` |
| `--compare <path>` | — | Path to a second `keystone pressure` run record; the report then prints both records' percentiles and throughput per operation, per service of the probes and bad tokens and per stage, their propagation delays, and the second minus the first as the difference. See [Comparing two runs](#comparing-two-runs) |
| `--allow-mismatch` | off | Compare two records whose scenario, seed, workers, token pool, mix, propagation probe or stages differ. Needs `--compare` |

- **table** — human-readable, one row per resource kind plus an overall row.
- **json** — the aggregate metrics, machine-readable.
- **csv** — one row per resource kind plus an overall row.
- **html** — a self-contained, offline report with inline SVG charts for
  latency, throughput, and error rates; for a churn run, also the per-bucket
  degradation over time; for a `keystone pressure` run, a `Pressure` section
  with a throughput and a latency chart over time per operation.

A record a churn or pressure run wrote while it was still running
(`"incomplete": true`) renders in every format and is marked as follows:

- **table** — a first line `Run incomplete: checkpoint written at <time>`, with
  the checkpoint time in RFC 3339 UTC, then a blank line and the usual output.
- **json** — `"incomplete": true` next to `metrics` and `chaos` for a
  single-service churn run, next to `metrics` and `pressure` for a pressure
  run, and next to `metrics` and `personas` (with `services` and `lanes` when
  present) for a `mix chaos` run. An `apply` or `monitor` record stays the
  bare metrics object; neither command writes a checkpoint.
- **csv** — unchanged; the format has no place for a marker.
- **html** — a yellow `Run incomplete` banner with the checkpoint time instead of
  `Run completed`. A record that carries an error still shows `Run failed`.

### Comparing two runs

With `--compare`, `report` reads two `keystone pressure` records and prints
their comparison instead of the `--run` record's report. The `--run` record is
the base and the `--compare` record the other, and every delta is the other
minus the base. With the build without a change as `--run` and the build with
it as `--compare`, a negative latency delta and a positive `ops/s` delta mean
the change made the path faster.

```console
$ dizzy keystone report --run run-1a2b3c4d.json --compare run-5e6f7a8b.json
Comparison
  base:  run 1a2b3c4d, scenario small, seed 42, workers 8, tokens 20, mix validate=60
  other: run 5e6f7a8b, scenario small, seed 42, workers 8, tokens 20, mix validate=60

Operations
OPERATION  METRIC    BASE   OTHER    DELTA  CHANGE
all        p50     1.84ms  1.42ms   -420µs  -22.8%
all        p95     2.61ms  2.05ms   -560µs  -21.5%
all        p99     3.48ms  3.51ms    +30µs   +0.9%
all        ops/s   4120.0  5310.4  +1190.4  +28.9%
validate   p50     1.84ms  1.42ms   -420µs  -22.8%
validate   p95     2.61ms  2.05ms   -560µs  -21.5%
validate   p99     3.48ms  3.51ms    +30µs   +0.9%
validate   ops/s   4120.0  5310.4  +1190.4  +28.9%
```

The `Operations` table has four rows per operation, `p50`, `p95`, `p99` and
`ops/s`: first for `all`, the run's draws together, then for every operation
either record drew, sorted by name. When either record has probe draws, a
`Probes` table follows with the columns `SERVICE`, `METRIC`, `BASE`, `OTHER`,
`DELTA` and `CHANGE` and the same four rows for every service either record
probed, sorted by name, and when either has `bad_token` draws, a `Bad tokens`
table follows the same way. When either record ran the propagation probe, a
`Propagation` table follows with the columns `METRIC`, `BASE`, `OTHER`,
`DELTA` and `CHANGE` and the rows `p50`, `p95`, `p99`, `max`, `probes` and
`timed out`. There, a record without the probe, or whose probe completed
none, shows `-` in its cell, in `DELTA` and in `CHANGE`. The `base:` or
`other:` line of a record with the probe ends with its settings, e.g.
`, propagation every=30s,timeout=10m0s`. For a base whose services kept
accepting a revoked token for minutes and an other whose services refused it
within seconds:

```text
Propagation
METRIC      BASE  OTHER     DELTA  CHANGE
p50         3m0s     3s    -2m57s  -98.3%
p95        4m50s   5.4s  -4m44.6s  -98.1%
p99         5m0s     6s    -4m54s  -98.0%
max         5m0s     6s    -4m54s  -98.0%
probes        12     12        +0   +0.0%
timed out      0      0        +0       -
```

When either record has stages, a `Stages` table follows last with the same
rows as the `Operations` table per stage and operation, the stages of the two
records paired by their index. A record written as a checkpoint
(`"incomplete": true`) compares like a final one, and its line under
`Comparison` carries ` (incomplete)` after the run id. See
[Comparing two runs](metrics.md#comparing-two-runs) for every format.

The two records have to come from the same load. When their scenario, seed,
workers, token pool, mix, propagation probe, number of stages or a stage's
`fan_assignments` differ, the command names every difference and fails:

```console
$ dizzy keystone report --run run-1a2b3c4d.json --compare run-9c0d1e2f.json
error: comparing run-1a2b3c4d.json with run-9c0d1e2f.json: the records come from different loads (seed 42 vs 43; workers 8 vs 16); pass --allow-mismatch to compare them anyway
```

The propagation probe differs when its interval or its ceiling does, e.g.
`propagation every=30s,timeout=10m0s vs off` for a base with the probe and an
other without it.

`--allow-mismatch` compares them anyway and prints the differences on a
`loads differ:` line under the two runs. Only `keystone pressure` records
compare. The flags exist on the `report` of every namespace, since all share
one builder, and a record without pressure statistics fails with
`run <id> has no pressure statistics; the comparison needs two keystone pressure records`.
An error of the comparison is wrapped as `comparing <run> with <compare>: …`,
with the two paths as given. Before it reads either record, the command
rejects `--allow-mismatch` without `--compare` with
`--allow-mismatch needs --compare`, and `--compare` with `--format csv` with
`--compare is not available for --format csv; use table, json or html`.

## `cleanup`

Deletes a run's resources in reverse dependency order. Idempotent: a 404 counts
as success, so running it twice is harmless.

| Flag | Description |
|---|---|
| `--run <path>` | Path to the run record whose resources to delete |
| `--run-id <id>` | Delete resources for this run id directly, without a record |

Discovery differs per service, and so does what `--run-id` alone can reach:

- **neutron** — resources are found by the `dizzy:run=<id>` tag. Address scopes
  are the exception: some Neutron releases refuse to tag them, so they are
  reclaimed from the run record by id. Removing them therefore needs `--run`,
  not a bare `--run-id`.
- **cinder** — volumes and snapshots are found by their `dizzy:run=<id>`
  metadata, with the run record's created list as a fallback. Snapshots are
  deleted before their volumes.
- **keystone** — projects are found by tag (falling back to the name prefix);
  domains, users, and roles by the `dizzy-<runid>-` name prefix. Role
  assignments are best reclaimed *with* a record, which is their authoritative
  handle. Implied-role rules have no name; they are deleted by id from the
  run record, after the projects and before the roles. Without a record they
  go with their roles, because Keystone deletes a role's implied-role rules
  when it deletes the role.
- **nova** — servers and data volumes are found by their `dizzy:run=<id>`
  metadata; the companion networks, subnets, and ports by the `dizzy:run=<id>`
  tag. Servers are deleted first (so their attachments release), then ports,
  volumes, and networks (a network delete cascades its subnet). A
  boot-from-volume root volume carries no dizzy identity — it is delete-on-
  termination, so it dies with its server.
- **glance** — images are found by the `dizzy:run=<id>` Glance image tag, filtered
  server-side by the list API's `tag` parameter, with the run record's created
  list as a fallback. Images are a single kind with no ordering, so a bare
  `--run-id` reaches everything the run created.

See [Resource identity and cleanup](../explanation/resource-identity.md).

## The `mix` namespace

`mix` runs several workload personas side by side in one churn run. Each
persona has its own churn engine, its own cloud project and its own identity,
and the whole run shares one seed, one run id and one run record. This build
has three personas: `ci`, whose servers are short-lived; `gardener`, whose
servers form clusters in anti-affinity server groups and are replaced one
worker at a time; and `legacy`, whose servers stay until teardown and are
changed in place. Next to the personas, a run can churn single services as
background lanes: `cinder`, `glance`, `keystone` and `neutron`. A lane runs the
churn graph of that service's own `chaos` command in an engine of its own,
under the identity `<run-id>-<lane>`. Every lane is off unless the scenario's
`lanes:` block or `--set lanes.<name>.enabled=true` switches it on, and a run
still needs a persona with a server. See
[Combined runs](../explanation/combined-runs.md).

| Subcommand | Touches the API | Purpose |
|---|---|---|
| `mix generate` | no | Expand a mix scenario into a plan and dump it |
| `mix chaos` | yes | Run one churn engine per persona and per enabled lane |
| `mix status` | yes | Re-query the current state of a mix run's resources, per persona and lane |
| `mix report` | no | Render metrics from a run record, the shared `report` |
| `mix cleanup` | yes | Delete every persona's and lane's resources of a mix run |

There is no `mix apply` and no `mix monitor`: a persona is a behavior over
time, which a one-shot build does not have.

`--concurrency` and `--max-parallel` apply to each persona and each lane
separately. A run of three personas and two lanes can have up to five times
`--concurrency` API calls in flight.

`mix generate`, `mix chaos`, `mix status` and `mix cleanup` reject an opt-in
service under `services` that this build does not support, before they make
any API call:

```console
$ dizzy mix generate --scenario scenarios/mix/small.yaml --set services=octavia
error: opt-in service "octavia" is not supported by this build of dizzy (supported: none)
```

### `mix generate`

Expands a mix scenario into its plan and writes it as JSON: every persona with
at least one server, with its share, its servers, its seed and its compute
plan, and under `lanes` every enabled lane, with its seed and the plan of its
service. A lane's scenario comes from the service's bundled profile or from
the file the lane names. Never touches the API.

| Flag | Description |
|---|---|
| `--scenario <path>` | Path to the mix scenario YAML file (**required**) |
| `--set <key>=<value>` | Override one scenario value; repeatable |
| `--out <path>` | Write the plan to this file instead of stdout |

### `mix chaos`

Runs the combined churn. Each persona authenticates with the `clouds.yaml`
entry its scenario block names under `cloud`, or with `--os-cloud` when that
is empty, and churns its share of `resources.servers` under the identity
`<run-id>-<persona>`. Before anything is created, every persona resolves the
image and flavor and runs the compute quota pre-check against its own plan in
its own project. For the `gardener` persona, whose plan has server groups, the
pre-check also counts the groups against the `server_groups` limit and the
largest group against the `server_group_members` limit. When two personas or
lanes authenticate against the same project, the run logs
`lanes share a project; each quota pre-check saw only its own plan`, with the
names under `lanes`, and goes on.

A persona whose plan migrates servers, `legacy`, also runs the migration
pre-check in its own project. Live and cold migration need the admin role and
at least two usable compute hosts. Without them the persona logs two warnings,
each with the `reason` attribute, and runs without migrations:

```text
level=WARN msg="live migration disabled for this run" reason="credentials lack the admin role"
level=WARN msg="cold migration disabled for this run" reason="credentials lack the admin role"
```

A scenario that sets `personas.legacy.cold_migration: false` plans no cold
migration. The pre-check then decides live migration alone, and the persona
logs one line after its pre-check lines:

```text
level=INFO msg="cold migration disabled for this run" reason="personas.legacy.cold_migration is false"
```

After the personas, each enabled lane authenticates with the `clouds.yaml`
entry its block names under `cloud`, or with `--os-cloud` when that is empty,
and runs the read-only pre-checks of its service's `chaos` command against its
own plan: the volume type and the quota pre-check for `cinder`, the external
network and the quota pre-check for `neutron`, and the privilege pre-check for
`keystone`; `glance` has none. Nothing is created before every persona and
every lane has passed. A lane whose pre-check fails, or whose cloud is not in
`clouds.yaml`, ends the run before anything is created, with an error that
names the lane; a lane is never dropped from a run with a warning:

```console
$ dizzy mix chaos --scenario scenarios/mix/small.yaml --set lanes.keystone.enabled=true
error: lane "keystone": caller is neither cloud admin nor domain manager: token carries roles [member reader]; keystone needs the 'admin' role (any scope) or the 'manager' role on a domain-scoped token (use --privilege to override)
```

On a cloud without those rights, switch the lane off with
`--set lanes.keystone.enabled=false`, or bind it with
`--set lanes.keystone.privilege=domain-manager` and the lane's `domain` and
`roles`. Once every pre-check has passed, the `keystone` lane creates its
domain and role scaffold, or binds the existing domain and roles in
domain-manager mode, and only then do the engines start. When the scaffold
fails, the run deletes what it created and exits with
`provisioning lane "keystone" (run <run-id>-keystone): binding scaffold: …`.

| Flag | Default | Description |
|---|---|---|
| `--scenario <path>` | — | Path to the mix scenario YAML file (**required**) |
| `--set <key>=<value>` | — | Override one scenario value; repeatable |
| `--duration <duration>` | — | Total wall-clock runtime (required, via flag or the `chaos:` block). `0` runs until SIGINT or SIGTERM |
| `--bucket-width <duration>` | `1h` | Width of one time bucket in the series of a run with `--duration 0`, at least `1m` |
| `--max-parallel <n>` | `--concurrency` | Maximum concurrent in-flight churn operations of each persona and lane. Without it, a lane takes `parallel.max` from its own scenario |
| `--no-cleanup` | off | Leave every persona's and lane's resources in place at the end of the run *and* on interrupt |

There are no `--min-interval`, `--max-interval`, `--churn-ratio` or
`--target-fill` flags: each persona's block sets them, and `--set` overrides
them, e.g. `--set personas.ci.target_fill=0.8`. A lane takes them from its own
scenario's `chaos:` block; see [Lanes](scenario-schema.md#lanes). The run
writes and checkpoints one `run-<id>.json` the way `chaos` does, with a
per-persona and per-lane breakdown.

At the end it prints the overall metrics summary, then a table of the
personas, then a table of the lanes when the run has any, then the record
path:

```text
Personas
NAME      PROJECT                           SHARE  SERVERS  OPS   OK  FAILED    P50    P95    P99
ci        5c3f1e0a9b2d4e6f8a7b9c0d1e2f3a4b    50%        3  412  410       2  310ms   1.9s   3.2s
gardener  7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a    30%        2   64   63       1   1.8s  41.5s  58.3s
legacy    9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b    20%        1   96   95       1   2.4s  38.2s  52.1s

Lanes
NAME      PROJECT                           SCENARIO        OPS   OK  FAILED    P50    P95    P99
glance    5c3f1e0a9b2d4e6f8a7b9c0d1e2f3a4b  small/glance     88   88       0  420ms   2.1s   3.4s
keystone  2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e  small/keystone  240  239       1   95ms  310ms  480ms
run record written to run-1a2b3c4d.json
```

Teardown then deletes each persona's and lane's resources by its identity,
prints one line per persona and lane, and runs one leak check across all of
them:

```text
deleted 14 resource(s) for run 1a2b3c4d-ci
deleted 6 resource(s) for run 1a2b3c4d-gardener
deleted 5 resource(s) for run 1a2b3c4d-legacy
deleted 3 resource(s) for run 1a2b3c4d-glance
deleted 9 resource(s) for run 1a2b3c4d-keystone
leak check: no run-tagged resources remain
```

When resources remain, the last line is
`leak check: <n> run-tagged resource(s) still present after teardown`. A
persona or lane whose teardown fails does not stop the others; the command
then exits non-zero naming every failing one, as
`tearing down persona "ci" (run 1a2b3c4d-ci): …` or
`tearing down lane "keystone" (run 1a2b3c4d-keystone): …`, and prints no
leak-check line. A failing leak check reports
`leak check for lane "keystone": …` or `leak check for persona "ci": …`.

With `--no-cleanup` the resources stay in place and the command prints the
hint to reclaim them, `churn interrupted; …` after an interrupt:

```text
churn complete; resources left in place — reclaim with: mix cleanup --run run-1a2b3c4d.json
```

When the run record could not be written, the hint is
`mix cleanup --run-id <id> --scenario '<file>'` instead, followed by one
`--set '<key>=<value>'` for every `--set` the run was given. Either hint ends
with `--os-cloud '<name>'` when the run took its cloud from `--os-cloud` or
`$OS_CLOUD`, the cloud of every persona and lane whose block names none.
Values are single-quoted so the hint pastes into a POSIX shell unchanged.

### `mix status`

Re-queries the current state of a mix run's resources, each persona and lane
under the cloud and identity the record names for it. A persona or lane whose
cloud now authenticates against another project than the record's `projectID`
fails before anything is queried, because its resources would all show as
`gone`.

| Flag | Description |
|---|---|
| `--run <path>` | Path to the mix run record to re-query (**required**) |

For each persona it prints a heading `persona <name> (run <run-id>-<name>)`,
for each lane a heading `lane <name> (run <run-id>-<name>)`, and then the
status table of the resources that persona or lane created. It visits every
persona and lane and exits non-zero when any table failed, with
`re-querying <n> of <m> personas failed`, or
`re-querying <n> of <m> personas and lanes failed` for a record with lanes.

### `mix report`

The shared [`report`](#report). A mix record adds per-persona and per-lane
output in every format; see
[What `report` renders](metrics.md#what-report-renders).

### `mix cleanup`

Deletes every persona's and lane's resources of a mix run, each by its
identity, `<run-id>-<persona>` or `<run-id>-<lane>`, and in the project of the
cloud it ran under. A persona follows the `nova` discovery rules of
[`cleanup`](#cleanup). Server groups carry neither metadata nor tags, so a
persona's server groups are found by the name prefix
`dizzy-<run-id>-<persona>-` and deleted after its other resources, even when
one of those deletes failed. A lane follows the discovery rules of its
service's `cleanup`. Idempotent.

| Flag | Description |
|---|---|
| `--run <path>` | Path to the mix run record whose resources to delete |
| `--run-id <id>` | Delete resources for this run id directly, without a record; needs `--scenario` |
| `--scenario <path>` | With `--run-id` only: the scenario the run used, which names the personas, the lanes and their clouds |
| `--set <key>=<value>` | With `--run-id` only: an override the run used; repeatable |

Exactly one of `--run` and `--run-id` is required. With `--run`, the personas,
the lanes, their clouds and identities come from the record, and a persona or
lane whose cloud now authenticates against another project than the record's
`projectID` fails before anything is deleted:

```console
$ dizzy mix cleanup --run run-1a2b3c4d.json --os-cloud other
error: persona "ci" authenticated against project <id>, but the run record says it ran in project <id>; authenticate with the cloud the run used
```

A lane reports the same with `lane "<name>"`, and a lane whose cloud is not in
`clouds.yaml` fails with `creating <api> client for lane "<name>": …`, where
`<api>` is `block storage`, `image`, `identity` or `network`.

With `--run-id`, they come from the scenario and overrides, which must be the
ones the run used. A lane needs only its name and cloud, so its scenario is
not read again: a lane scenario file moved or changed since the run does not
stop the cleanup. Without a record, the `neutron` and `keystone` lanes cannot
reclaim everything `neutron cleanup` and `keystone cleanup` reclaim with one,
and the command logs their warnings under the lane identity before it deletes
anything:

```text
level=WARN msg="cleaning up by id without a run record; resources that cannot be discovered by tag (e.g. address scopes) will not be reclaimed — pass --run to reclaim them" run=1a2b3c4d-neutron
level=WARN msg="cleaning up by id without a run record; role assignments are best reclaimed with a record — pass --run to use it" run=1a2b3c4d-keystone
```

It prints `deleted <n> resource(s) for run <run-id>-<name>` per persona and
lane and continues past a failing one, then exits non-zero naming it, as
`cleaning up persona "ci" (run …): …` or
`cleaning up lane "keystone" (run …): …`.

`nova cleanup --run` on a mix record fails with
`run record is for service "mix", not "nova"`, and `mix cleanup --run` on a
`nova` record fails the same way the other way round.

## `list-networks`

Lists the project's networks. A working auth and connectivity smoke test with no
side effects. `neutron` namespace only; takes no flags beyond the global ones.

## `verify`

A stub. Returns `not implemented yet`. Reserved for reconciling a run against the
OVN northbound/southbound databases and OVS flows.
