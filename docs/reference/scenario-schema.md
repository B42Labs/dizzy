# Scenario schema

A scenario is a YAML file describing a workload by counts, ranges, and ratios,
plus an RNG seed that makes its expansion deterministic. Each service has its own
schema — and therefore its own `--set` keys — so a key that belongs to another
service is rejected rather than silently ignored.

Every scenario except a c5c3 one shares this top-level shape; see
[C5C3](#c5c3) for that one:

```yaml
name: <string>       # profile name, recorded in the run record
seed: <int>          # deterministic; same seed => same plan
image / flavor / resize_flavor   # nova only; referenced by name, never created
role / pages         # horizon only; see Horizon below
provider: <string>   # octavia only; empty selects the cloud's default
resources:  { … }    # fixed counts
distribution: { … }  # ranges and ratios drawn per resource
topology:   { … }    # neutron only
chaos:      { … }    # optional; consumed only by the `chaos` command
pressure:   { … }    # keystone only; optional; consumed only by `keystone pressure`
```

A `{ min: <int>, max: <int> }` **range** is drawn once per resource, inclusive at
both ends. A **ratio** is a float from 0 to 1.

The global `--seed` flag overrides the file's `seed`; a c5c3 scenario has no
seed, and the flag has no effect there. Individual values are overridden with
`--set <dotted.key>=<value>`.

## Neutron

```yaml
name: medium
seed: 1234567

resources:
  subnet_pools:    3
  address_scopes:  0
  networks:        100
  routers:         20
  security_groups: 15
  router_links:    5      # router-to-router transit links
  floating_ips:    10     # allocated from the external network, if one exists

distribution:
  subnets_per_network:                 { min: 1, max: 3 }
  ports_per_network:                   { min: 0, max: 5 }
  rules_per_security_group:            { min: 2, max: 12 }
  subnet_from_pool_ratio:              0.4
  ipv6_ratio:                          0.2
  subnets_attached_to_router_ratio:    0.6
  routers_with_external_gateway_ratio: 0.3
  floating_ip_associated_ratio:        0.5

topology:
  router_attach_strategy:    random
  port_security_group_count: { min: 1, max: 3 }
```

### `resources`

| Key | Type | Meaning |
|---|---|---|
| `subnet_pools` | int | Subnet pools, shared by subnets that opt into pool-based allocation |
| `address_scopes` | int | Address scopes that subnet pools may belong to |
| `networks` | int | Tenant networks. Each router link adds one more on top of this |
| `routers` | int | Internal routers |
| `security_groups` | int | Security groups |
| `router_links` | int | Router-to-router transit links; needs at least two routers |
| `floating_ips` | int | Floating IPs; created only when an external network exists |

### `distribution`

| Key | Type | Meaning |
|---|---|---|
| `subnets_per_network` | range | Subnets drawn per network |
| `ports_per_network` | range | Ports drawn per network |
| `rules_per_security_group` | range | Rules drawn per security group |
| `subnet_from_pool_ratio` | ratio | Fraction of subnets allocated from a pool rather than an explicit CIDR |
| `ipv6_ratio` | ratio | Fraction of subnets that are IPv6. Set to `0` for IPv4-only |
| `subnets_attached_to_router_ratio` | ratio | Fraction of subnets attached to a router. A subnet attaches to at most one |
| `routers_with_external_gateway_ratio` | ratio | Fraction of routers that *want* an external gateway |
| `floating_ip_associated_ratio` | ratio | Fraction of floating IPs associated with a port; the rest stay unassociated |

### `topology`

| Key | Type | Meaning |
|---|---|---|
| `router_attach_strategy` | string | How subnets are distributed across routers. `random` |
| `port_security_group_count` | range | Distinct security groups attached per port |

### External connectivity

`routers_with_external_gateway_ratio` and `floating_ips` record *intent* only.
The external network itself is a property of the target cloud, not of the plan,
and is resolved at apply time — by `--external-network <name>`, or by
auto-detecting the first external network. **If the cloud has no external
network, the intent is a silent no-op**, not a failure.

Each entry in `router_links` adds a dedicated transit network, a `/30` transit
subnet, and a port. One router attaches to the subnet, taking its gateway
address; the peer attaches through the explicit port.

### Profiles

Three profiles ship under `scenarios/neutron/`. These are the counts each one
actually expands to at its shipped seed — check any of them yourself with
`apply --dry-run`.

| Profile | Networks | Subnets | Routers | Ports | Security groups | Floating IPs | Chaos duration |
|---|---|---|---|---|---|---|---|
| `small` | 4 | 7 | 2 | 6 | 2 | 2 | 5m |
| `medium` | 105 | 200 | 20 | 189 | 15 | 10 | 30m |
| `large` | 210 | 375 | 40 | 361 | 30 | 20 | 1h |

The network counts exceed `resources.networks` because each `router_links` entry
adds its own transit network: `small` declares 3 networks and 1 link, `medium`
100 and 5, `large` 200 and 10.

`small` fits Neutron's default per-project quotas. `medium` and `large` need
raised quotas; the `apply` quota pre-check names exactly which ones.

## Cinder

```yaml
name: small
seed: 42

resources:
  volumes: 5

distribution:
  volume_size_gib:      { min: 1, max: 5 }   # initial size drawn per volume
  volume_resized_ratio: 0.5                  # fraction of volumes extended after creation
  resize_growth_gib:    { min: 1, max: 4 }   # extend delta drawn per resized volume
  snapshots_per_volume: { min: 0, max: 2 }   # drawn per volume
```

| Key | Type | Meaning |
|---|---|---|
| `resources.volumes` | int | Blank volumes to create; no image source, no attachments |
| `distribution.volume_size_gib` | range | Initial size drawn per volume |
| `distribution.volume_resized_ratio` | ratio | Fraction of volumes extended after creation |
| `distribution.resize_growth_gib` | range | Extend delta drawn per resized volume |
| `distribution.snapshots_per_volume` | range | Snapshots drawn per volume |

The resize intent lives **in the plan**, not in a decision made at apply time, so
the same scenario and seed always yield the same volumes, the same resize
targets, and the same snapshot fan-out.

### Profiles

Expanded counts at each profile's shipped seed:

| Profile | Volumes | Resized | Snapshots | Total GiB | Chaos duration |
|---|---|---|---|---|---|
| `small` | 5 | 3 | 7 | 52 | 5m |
| `medium` | 20 | 10 | 34 | 505 | 30m |
| `large` | 50 | 34 | 132 | 1487 | 1h |

`small` is sized to fit Cinder's common default quotas of 10 volumes, 10
snapshots, and 1000 GiB. `medium` and `large` exceed the snapshot quota, and
`large` also exceeds the default 1000 GiB.

## Keystone

```yaml
name: small
seed: 42

resources:
  domains:  1     # admin-only; forced to the single in-scope domain in domain-manager mode
  roles:    2     # admin-only; ignored (existing roles reused) in domain-manager mode
  projects: 5
  users:    10
  application_credentials: 2   # see "Application credentials and trusts"
  trusts:                  2

distribution:
  projects_per_domain:            { min: 1, max: 3 }
  assignments_per_user:           { min: 1, max: 3 }
  domain_scoped_assignment_ratio: 0.1
  users_issuing_tokens_ratio:     0.5

pressure:          # read only by `keystone pressure`; see "The pressure: block"
  duration: 5m
  workers:  8
  tokens:   20
```

| Key | Type | Meaning |
|---|---|---|
| `resources.domains` | int | Domains. Admin-only; domain-manager mode requires `<= 1` |
| `resources.roles` | int | Roles. Admin-only; ignored in domain-manager mode, which reuses existing roles |
| `resources.projects` | int | Projects |
| `resources.users` | int | Users |
| `resources.implied_roles` | list | Implied-role rules, each a `prior` role and the roles it `implies`, by logical name. Admin-only; see [Implied roles](#implied-roles) |
| `resources.application_credentials` | int | Application credentials, each created by a user on one of its project-scoped grants and then authenticated with. Both tiers; see [Application credentials and trusts](#application-credentials-and-trusts) |
| `resources.trusts` | int | Trusts, each from a user on one of its project-scoped grants to another user of the plan, and then authenticated with as that user. Both tiers; needs at least 2 users; see [Application credentials and trusts](#application-credentials-and-trusts) |
| `distribution.projects_per_domain` | range | Clustering granularity of the round-robin deal of projects across domains — **not** a per-domain cap |
| `distribution.assignments_per_user` | range | `(user, target, role)` grants drawn per user |
| `distribution.domain_scoped_assignment_ratio` | ratio | Fraction of grants targeting the domain rather than a project |
| `distribution.users_issuing_tokens_ratio` | ratio | Fraction of users that authenticate for a scoped token |
| `distribution.fan_users` | int | Fan users, appended after the `resources.users` users, each holding a grant on many projects; see [Fan users](#fan-users). Omitted or `0` means none |
| `distribution.fan_assignments` | range | Distinct projects drawn per fan user, each holding one grant of the fan user; `min >= 1` and `max <= resources.projects` when `fan_users > 0` |

`projects_per_domain` shapes how projects cluster, while the total stays exactly
`resources.projects`: each turn of the deal cycles to the next domain and assigns
a batch drawn from the range.

The optional `pressure:` block configures `keystone pressure` and takes no part
in the plan; see [The `pressure:` block](#the-pressure-block).
`users_issuing_tokens_ratio` has no effect on a pressure run, which skips the
plan's token issues and fills its token pool from the project-scoped role
assignments instead.

**Passwords are never in the plan or the run record.** Each user's password is
a random value generated when the run creates the users and held only in
memory, for the token-issue step and, in `keystone pressure`, for the token
pool, so the plan stays reproducible without persisting a credential.

### Fan users

A fan user is a user with a grant on many projects, the user the `effective`
operation of `keystone pressure` lists when a scenario has one.
`distribution.fan_users` adds that many fan users to the plan, named
`fan-0001`, `fan-0002` and so on, after the `resources.users` users and in the
first domain. Each fan user draws a count from `distribution.fan_assignments`
and holds one grant on that many distinct projects, drawn uniformly from the
projects of every domain, with a role drawn uniformly from the plan's roles
for each grant. Keystone allows a grant on a project of another domain, so the
rule that keeps a regular user's grants in its own domain does not apply to a
fan user. The fan users are drawn after everything but the
[application credentials and trusts](#application-credentials-and-trusts),
so adding them to a scenario leaves the rest of its plan as it was. Those two
draw from the fan users' grants as well, so adding fan users changes which
credentials and trusts a plan with them holds.

A fan user never issues a plan token, and in `keystone pressure` it is never a
pool user or a churn user: `effective` lists its grants with a token of its
own, and the [stages](#stages) of the `pressure:` block grow them. `apply`
creates fan users and their grants like any other user and grant, and `chaos`
churns them the same way. `apply --dry-run` prints a `fan users:` line when
the plan has any, and its `users:` line counts them too.
`--set distribution.fan_users=<n>`,
`--set distribution.fan_assignments.min=<n>` and
`--set distribution.fan_assignments.max=<n>` override the keys.

A negative count fails like any other, e.g.
`distribution.fan_users must not be negative, got -1`. With `fan_users` above
`0`, validation also reports, in this order:

- `resources.domains must be at least 1 when projects or users are created, got 0`
- `resources.roles must be at least 1 when users draw assignments, got 0`
- `distribution.fan_assignments.min must be at least 1 when fan users are created, got 0`
- `distribution.fan_assignments.max (4001) exceeds resources.projects (4000); a fan user holds one grant per distinct project`

Validation caps the assignments a plan can draw at 1,000,000:
`resources.users` × `assignments_per_user.max`, plus, with fan users,
`fan_users` × `fan_assignments.max`. With fan users the message names both
terms, e.g.
`resources.users (20) × distribution.assignments_per_user.max (3) + distribution.fan_users (1000) × distribution.fan_assignments.max (1000) = 1000060 exceeds the total assignment limit of 1000000`;
without them it names the first alone.

### Implied roles

`resources.implied_roles` declares implied-role rules between the plan's own
roles. Each rule names a `prior` role and, under `implies`, the roles it
implies, all by logical name, from `role-0001` to the last role of
`resources.roles`:

```yaml
resources:
  roles: 3
  implied_roles:
    - { prior: role-0001, implies: [role-0002] }
    - { prior: role-0002, implies: [role-0003] }
```

A rule gives every holder of a grant of `prior` the implied roles on the same
target, and Keystone lists each implied role as an extra effective
assignment. With the rules above, an effective role-assignment listing shows
a grant of `role-0001` on a project together with `role-0002` and `role-0003`
on that project. Since a rule names only the plan's roles, it can never name
a cloud role such as `admin` or `reader`.

In admin mode the run creates the rules right after the roles, before the
projects and users. The run record lists each (prior, implied) pair in its
created list as kind `implied_role`, with a logical name such as
`role-0001->role-0002` and the id `<prior role id>:<implied role id>`; a rule
has no cloud name. `keystone status` shows a rule as `present` or `gone`, and
the metrics record its create and its delete under the kind `implied_role`.
`apply --dry-run` prints an `implied roles:` line directly
after the `roles:` line when the plan has rules. The line counts the (prior,
implied) pairs, so a rule that implies two roles counts twice.

Domain-manager mode creates no roles, so it ignores the key and logs the
warning
`implied-role rules are ignored in domain-manager mode, which reuses existing roles`
with `rules=<n>`, the number of pairs.

The teardown deletes the recorded rules after the projects and before the
roles, and a 404 counts as success. Without a record, in
`keystone cleanup --run-id` or the orphan sweep of
`keystone monitor --reclaim-orphans`, the cleanup has no rule to delete, but
the rules go with their roles: Keystone deletes a role's implied-role rules
when it deletes the role. The leak check is unchanged, since a rule cannot
outlive its roles.

A cycle, such as `role-0001` implying `role-0002` and `role-0002` implying
`role-0001`, is allowed: Keystone accepts it, and its expansion stops at a
cycle. The key has no `--set` form; declare the rules in the scenario file.
See [A fan for C5C3/cobaltcore#1459](#a-fan-for-c5c3cobaltcore1459) for a
scenario whose fan user's grants carry implied roles.

With rules declared, validation checks the role count first, then each rule
in turn: its `implies` list, its `prior`, and then each entry of its
`implies` in turn. It reports the first failure, in this order:

- `resources.roles must be at least 2 when implied_roles are declared, got 1`
- `resources.implied_roles[0].implies must name at least one role`
- `resources.implied_roles[0].prior "role-0004" is not a role of the plan, whose roles are role-0001 to role-0003`
- `resources.implied_roles[0].implies[0] "role-0004" is not a role of the plan, whose roles are role-0001 to role-0003`
- `resources.implied_roles[0].implies[0] lets "role-0001" imply itself`
- `resources.implied_roles[1].implies[0] repeats the rule "role-0001" implies "role-0002"`,
  for a pair that an earlier rule or entry already declared

### Application credentials and trusts

`resources.application_credentials` and `resources.trusts` ask for that many
application credentials and trusts. Each one carries one role on one project,
the role and project of a project-scoped grant of the plan. A credential
belongs to the grant's user. A trust belongs to the grant's user, its
trustor, and delegates the role to another user of the plan, its trustee.

The generator draws both last, after the fan users, so adding them to a
scenario leaves the rest of its plan as it was. A count of `0` draws nothing.
Both draw from the plan's project-scoped grants in plan order, the fan users'
grants included:

- The application credentials come first, named `appcred-0001`,
  `appcred-0002` and so on. Each draws one grant uniformly and takes its
  user, project and role.
- The trusts come next, named `trust-0001`, `trust-0002` and so on. Each
  draws one grant uniformly, whose user is the trustor, and then the trustee
  uniformly over the plan's other users. Keystone refuses a trust that
  repeats the trustor, trustee, project, impersonation and expiry of another,
  and every trust dizzy creates has the same impersonation and expiry, so a
  draw whose (trustor, trustee, project) an earlier trust already has is
  skipped, not redrawn. A plan can therefore hold fewer trusts than
  `resources.trusts`, and their names stay contiguous.

`apply --dry-run` prints an `app credentials:` and a `trusts:` line after the
`token issues:` line, each only when the plan has any.
`--set resources.application_credentials=<n>` and
`--set resources.trusts=<n>` override the keys.

A negative count or one above the limit fails like any other, e.g.
`resources.application_credentials must not be negative, got -1` or
`resources.trusts exceeds the limit of 1000000, got 1000001`. A trust needs
two users, fan users counted, so with `resources.trusts` above `0` and a
single user validation also reports
`resources.trusts needs at least 2 users, a trustor and a trustee, got 1`.

A positive count also needs a project-scoped grant to draw from. Without
one, for example with `distribution.domain_scoped_assignment_ratio: 1` and
no fan users, the generator reports the first that applies:

- `resources.application_credentials is 2 but the plan has no project-scoped assignment to create one on`
- `resources.trusts is 2 but the plan has no project-scoped assignment to create one on`

Keystone's default policy lets only a credential's own user create it and
only the trustor create a trust. dizzy therefore creates both kinds as their
owners, in admin mode and in domain-manager mode alike: it logs in with the
owner's password for a token scoped to the project of the credential or
trust, and sends the create with that token. In domain-manager mode the role
is one of the reused roles. Each credential's secret is a random value
generated before the create and held only in memory, like the passwords, so
neither the plan nor the run record holds it. A trust is created with
impersonation, as Heat creates its trusts, and without expiry or use limit.

`apply` and `monitor` create the credentials after the token issues and then
the trusts, each stage up to `--concurrency` at a time, and authenticate with
every one they create. A credential or trust that fails to authenticate fails
`apply`, or the `monitor` iteration. Each credential costs three requests:
the owner's token, the create, and an authentication with the credential's id
and secret. Each trust costs three: the trustor's token, the create, and a
login of the trustee scoped to the trust, so two of the three are password
logins.

`chaos` and the `keystone` lane of a mix run churn both kinds. A create of
either costs two requests, the owner's token and the create, and so does a
delete, the owner's token and the delete. An authentication costs one and is
drawn with `token_ratio`; see [The `chaos:` block](#the-chaos-block). The
churn keeps each credential and trust on a live grant: a credential lives
only while its user, its project and its grant do, and a trust only while its
trustor, its trustee, its project and the trustor's grant do. Keystone
deletes a user's application credentials on a project when it revokes the
user's role there, and keeps a trust whose trustor lost the delegated role
but refuses to issue a token with it.

The run record lists a credential as kind `application_credential`, with the
cloud name `dizzy-<run>-appcred-0001` and the id
`<user id>:<credential id>`, and a trust as kind `trust`, with an empty name
and the trust's id. `keystone status` shows either as `present` or `gone`,
and as `unknown` when Keystone answers the read with a 403, as it does for a
domain manager, who may not read another user's credential or trust.

The teardown deletes neither kind itself. Keystone deletes a user's
application credentials, and every trust the user is trustor or trustee of,
when it deletes the user, and the teardown deletes the run's users with a
record or without one. The count of deleted resources leaves both kinds out.
The leak check is unchanged, since neither kind can outlive its users.

`keystone pressure` creates neither kind, whatever the two keys say.

### A fan for C5C3/cobaltcore#1459

C5C3/cobaltcore#1459 makes the effective-assignment listing linear in the
number of grants. Showing that takes one user with thousands of grants and a
measurement at several sizes. This scenario gives one fan user 500 grants of
roles that carry implied roles and grows them to 1000, 2000 and 4000 in four
stages of 5 minutes each, while the workers draw `validate`, `effective` and
`grant_churn` only:

```yaml
name: fan-1459
seed: 42

resources:
  domains:  1
  roles:    3
  projects: 4000
  users:    20
  implied_roles:
    - { prior: role-0001, implies: [role-0002] }
    - { prior: role-0002, implies: [role-0003] }

distribution:
  projects_per_domain:            { min: 1, max: 3 }
  assignments_per_user:           { min: 1, max: 3 }
  domain_scoped_assignment_ratio: 0.1
  users_issuing_tokens_ratio:     0.5
  fan_users:                      1
  fan_assignments:                { min: 500, max: 500 }

pressure:
  duration: 20m
  mix:
    validate:    10
    effective:   20
    revoke:       0
    grant_churn: 20
    issue:        0
  stages:          # four windows of 5m each
    - fan_assignments: 500
    - fan_assignments: 1000
    - fan_assignments: 2000
    - fan_assignments: 4000
```

```console
$ dizzy keystone apply --scenario fan-1459.yaml --dry-run
Plan for scenario "fan-1459" (seed 42)
  domains:         1
  roles:           3
  implied roles:   2
  projects:        4000
  users:           21
  fan users:       1
  assignments:     542 (1 domain-scoped)
  token issues:    11
```

The two rules have the shape of the chain of Keystone's default roles, in
which `admin` implies `member` and `member` implies `reader`, between the
plan's own roles. A fan grant of `role-0001`
brings two implied assignments, a grant of `role-0002` one and a grant of
`role-0003` none. The fan user's grants draw their roles from all three, and
the grants of a stage take them in turn, so on average each grant brings one
implied assignment, and the listing at 4000 grants returns about 8000
entries.

`grant_churn` at the weight of `effective` keeps Keystone's cache of computed
assignments cold. Keystone answers a repeated listing of the same user from
that cache until the next assignment write, and a cached listing expands
nothing. Each `grant_churn` draw makes two such writes: it unassigns a churn
user's grant and assigns it again. The churn user is `user-0020`, the last
regular user, since `churn_users` falls back to 1.

`keystone pressure --scenario fan-1459.yaml` then prints a `Stages` table with
an `effective` row per stage: the listing latency at 500, 1000, 2000 and 4000
grants, one under the other. The first stage's `fan_assignments` equals
`fan_assignments.max`, so entering it grants nothing. The price is the size of
the run. It provisions 4000 projects and the fan user's 500 grants before the
first worker starts, and grows the fan by 3500 grants while it runs: 500, 1000
and 2000 at the start of stages 2, 3 and 4, each with at most `--concurrency`
grants in flight. The grants come out of their stages' windows, so the 2000
grants of stage 4 shorten its draws the most. When they take too much of a
window, raise `--concurrency` to grant faster or give the last stage a longer
`duration`. A stage whose grants are still running when the run's duration
ends is missing from the `Stages` table. The teardown deletes all of it
again. See [`pressure`](cli.md#pressure) for how a run enters its stages and
what it records of a stage whose grants outlast the run.

### Profiles

Expanded counts at each profile's shipped seed:

| Profile | Domains | Roles | Projects | Users | Assignments | Token issues | Application credentials | Trusts | Chaos duration | Pressure |
|---|---|---|---|---|---|---|---|---|---|---|
| `small` | 1 | 2 | 5 | 10 | 21 | 3 | 2 | 2 | 5m | 5m, 8 workers, 20 tokens |
| `medium` | 2 | 3 | 20 | 50 | 90 | 25 | 10 | 10 | 30m | 30m, 32 workers, 200 tokens |
| `large` | 3 | 4 | 50 | 150 | 296 | 78 | 30 | 30 | 1h | 1h, 128 workers, 1000 tokens |

`small` keeps a single domain so it stays runnable in domain-manager mode.
`medium` and `large` declare more than one domain and are therefore admin-only.
`keystone pressure` runs every profile, `small` included, in admin mode only.
See [Keystone's privilege model](../explanation/privilege-model.md).

## Nova

```yaml
name: small
seed: 42

image: cirros          # referenced by name; dizzy uploads no image
flavor: m1.tiny        # referenced by name; dizzy creates no flavor
resize_flavor: m1.small  # the second flavor `resized` servers move to

resources:
  servers: 3
  networks: 2

distribution:
  networks_per_server:    { min: 1, max: 2 }   # a server wired into 1 or 2 networks
  volumes_per_server:     { min: 0, max: 2 }   # data volumes attached per server
  ports_per_server:       { min: 0, max: 1 }   # extra ports attached per server
  attached_volume_gib:    { min: 1, max: 3 }   # size drawn per data volume
  root_volume_gib:        { min: 2, max: 4 }   # root size drawn per boot-from-volume server
  boot_from_volume_ratio: 0.34                 # fraction booted from a root volume
  user_data_ratio:        0.5                  # fraction given user data at boot
  stop_start_ratio:       0.5                  # fraction stop/started during the run
  stop_start_hard_ratio:  0.5                  # of those, fraction using the hard variant
  resized_ratio:          0.34                 # fraction resized to resize_flavor and confirmed
  live_migrated_ratio:    0.34                 # fraction live-migrated (admin pre-check permitting)
  deleted_ratio:          0.34                 # fraction deleted during the run
  volume_detach_ratio:    0.5                  # fraction of data volumes detached
  port_detach_ratio:      0.5                  # fraction of ports detached
```

The top-level `image`, `flavor`, and `resize_flavor` name resources that must
already exist on the target cloud — dizzy uploads no test image and creates no
flavor, since both would need rights the tool does not assume. Override them for
a cloud that names them differently, e.g. `--set image=ubuntu-22.04`.
`resize_flavor` is required whenever `resized_ratio > 0` and must differ from
`flavor`.

| Key | Type | Meaning |
|---|---|---|
| `image` | string | Boot image, referenced by name (or id) |
| `flavor` | string | Boot flavor, referenced by name (or id) |
| `resize_flavor` | string | Second flavor a `resized` server moves to; required when `resized_ratio > 0` |
| `resources.servers` | int | Servers to boot |
| `resources.networks` | int | Companion networks dizzy creates for the servers |
| `distribution.networks_per_server` | range | Networks each server is wired into; `min >= 1`, `max <= resources.networks` |
| `distribution.volumes_per_server` | range | Data volumes attached per server |
| `distribution.ports_per_server` | range | Extra ports attached per server |
| `distribution.attached_volume_gib` | range | Size drawn per data volume |
| `distribution.root_volume_gib` | range | Root size drawn per boot-from-volume server |
| `distribution.boot_from_volume_ratio` | ratio | Fraction booted from a dizzy-created root volume |
| `distribution.user_data_ratio` | ratio | Fraction given a deterministic cloud-config at boot |
| `distribution.stop_start_ratio` | ratio | Fraction stop/started during the run |
| `distribution.stop_start_hard_ratio` | ratio | Of stop/started servers, fraction using the hard (reboot) variant rather than the soft (stop then start) one |
| `distribution.resized_ratio` | ratio | Fraction resized to `resize_flavor` and confirmed |
| `distribution.live_migrated_ratio` | ratio | Fraction live-migrated, subject to the fail-open admin pre-check |
| `distribution.deleted_ratio` | ratio | Fraction deleted during the run |
| `distribution.volume_detach_ratio` | ratio | Fraction of data volumes detached |
| `distribution.port_detach_ratio` | ratio | Fraction of ports detached |

Every lifecycle decision lives **in the plan**, not in a choice made at apply
time, so the same scenario and seed always boot the same fleet, drive the same
operations, and lay out the same companion networks, volumes, and ports. The
networks, ports, and volumes are created and torn down by dizzy; images and
flavors are only referenced.

### Profiles

Expanded counts at each profile's shipped seed:

| Profile | Servers | Networks | Volumes | Ports | Chaos duration |
|---|---|---|---|---|---|
| `small` | 3 | 2 | 3 | 1 | 5m |
| `medium` | 8 | 3 | 5 | 9 | 30m |
| `large` | 20 | 4 | 25 | 23 | 1h |

`small` is sized to fit Nova's common default 10-instance / 20-core quota, so it
runs against a fresh project without raising any compute quota. `medium` and
`large` need raised quotas. The companion Cinder and Neutron consumption is not
pre-checked — see [Deal with the quota pre-check](../how-to/raise-quotas.md).

## Glance

```yaml
name: small
seed: 42

resources:
  images: 5

distribution:
  image_size_mib:        { min: 1, max: 8 }   # synthetic upload payload per image, in MiB
  metadata_update_ratio: 0.5                  # fraction whose custom properties are churned
  shared_ratio:          0.5                  # fraction transitioned to shared visibility
  member_accept_ratio:   0.5                  # of the shared images, fraction whose membership is accepted
  member_remove_ratio:   0.5                  # of the shared images, fraction whose membership is later removed
  community_ratio:       0.25                 # fraction transitioned to community visibility
  public_ratio:          0.0                  # fraction transitioned to public visibility (admin-only; see below)
  deactivate_ratio:      0.34                 # fraction driven through a deactivate/reactivate cycle
  deleted_ratio:         0.34                 # fraction deleted during the run
```

Every image is created by dizzy with a **synthetic, generated data payload** of
`image_size_mib` mebibytes, uploaded through the direct data path — so a run
references no pre-existing image, and the byte volume it pushes is a scenario
parameter rather than an accident of the cloud. Image import through
web-download and multi-store placement are out of scope: every image lands in the
cloud's default store. `image_size_mib.max` is the byte-volume knob; because each
upload is one operation bounded by `--timeout`, a large payload may need
`--timeout` raised.

| Key | Type | Meaning |
|---|---|---|
| `resources.images` | int | Images to create |
| `distribution.image_size_mib` | range | Synthetic upload payload size drawn per image, in MiB; `min >= 1` when `images > 0` |
| `distribution.metadata_update_ratio` | ratio | Fraction whose custom properties are churned (an add pass then a replace/remove pass) |
| `distribution.shared_ratio` | ratio | Fraction transitioned to shared visibility, with the run's own project added as a member (a self-share, so the accept stays authorized) |
| `distribution.member_accept_ratio` | ratio | Of the shared images, fraction whose membership is accepted; only consulted for a shared image |
| `distribution.member_remove_ratio` | ratio | Of the shared images, fraction whose membership is later removed; only consulted for a shared image |
| `distribution.community_ratio` | ratio | Fraction transitioned to community visibility |
| `distribution.public_ratio` | ratio | Fraction transitioned to public visibility; `publicize_image` is admin-only on most clouds, so the shipped profiles keep this at `0` |
| `distribution.deactivate_ratio` | ratio | Fraction driven through a deactivate/reactivate cycle |
| `distribution.deleted_ratio` | ratio | Fraction deleted during the run |

Every lifecycle decision — the visibility transitions, the member operations, and
the deactivate cycle — lives **in the plan**, not in a choice made at apply time,
so the same scenario and seed drive the same operations. The upload payload bytes
are likewise derived from the seed, so a run pushes byte-identical data every
time.

### Profiles

Expanded counts at each profile's shipped seed:

| Profile | Images | Upload total | Shared | Community | Deactivations | Deletes | Chaos duration |
|---|---|---|---|---|---|---|---|
| `small` | 5 | 20 MiB | 3 | 3 | 2 | 2 | 5m |
| `medium` | 20 | 406 MiB | 11 | 6 | 10 | 8 | 30m |
| `large` | 50 | 2007 MiB | 29 | 12 | 24 | 23 | 1h |

All three profiles keep `public_ratio` at `0`, since `publicize_image` is
admin-only on most clouds; raise it only against a cloud that grants the policy.
Glance exposes no project-quota API, so none of the profiles is pre-checked — an
over-limit request surfaces as a fast-failed 413.

## Barbican

```yaml
name: small
seed: 42

resources:
  secrets:    10                # opaque secrets stored with a payload
  orders:     5                 # AES-256 key orders, each generating one more secret
  containers: 3                 # generic containers referencing stored secrets
  acls:       3                 # read ACLs, each on its own stored secret
  consumers:  3                 # consumers, each registered on one container

distribution:
  payload_bytes:         { min: 64, max: 8192 }   # synthetic payload per stored secret, in raw bytes
  retrieve_ratio:        0.5                      # fraction of stored secrets read back and byte-compared
  secrets_per_container: { min: 1, max: 3 }       # distinct stored secrets referenced per container
  users_per_acl:         { min: 1, max: 3 }       # synthetic user ids listed per ACL

chaos:                                 # `barbican chaos --scenario scenarios/barbican/small.yaml` runs as-is
  duration: 5m                         # total runtime; the only hard stop besides Ctrl-C
  interval: { min: 200ms, max: 3s }    # random delay drawn per churn tick
  parallel: { max: 4 }                 # per-tick fan-out, drawn in [1, max]; capped by --concurrency
  churn_ratio: 0.5                     # create bias at steady state (0..1)
  target_fill: 0.8                     # fraction of the envelope kept populated (0..1)
  retrieve_ratio: 0.3                  # probability per step of one read-back: a marked secret's payload, an ACL, or a consumer
```

Every secret is stored by dizzy with a **synthetic payload** of `payload_bytes`
raw bytes, sent base64-encoded in the create request, so a run references no
pre-existing secret. Each order asks Barbican to generate a 256-bit AES key in
CBC mode server side and is polled until ACTIVE; its key is never read back,
since there are no expected bytes to compare it with. A container references
stored secrets only, never the secret of an order.

| Key | Type | Meaning |
|---|---|---|
| `resources.secrets` | int | Opaque secrets to store, each with a synthetic payload |
| `resources.orders` | int | AES-256 key orders to submit; each generates one more secret once ACTIVE |
| `resources.containers` | int | Generic containers to create, each referencing stored secrets |
| `resources.acls` | int | Read ACLs to set, each on its own stored secret; at most `resources.secrets`. See [Secret ACLs and container consumers](#secret-acls-and-container-consumers) |
| `resources.consumers` | int | Consumers to register, each on one container; needs `containers > 0`. See [Secret ACLs and container consumers](#secret-acls-and-container-consumers) |
| `distribution.payload_bytes` | range | Synthetic payload size drawn per stored secret, in raw bytes; `min >= 1` when `secrets > 0` |
| `distribution.retrieve_ratio` | ratio | Fraction of stored secrets whose payload is read back and byte-compared with the bytes stored |
| `distribution.secrets_per_container` | range | Distinct stored secrets drawn per container; `max <= resources.secrets` when `containers > 0` |
| `distribution.users_per_acl` | range | Synthetic user ids drawn per ACL; `min >= 1` when `acls > 0` |

The payload sizes, the read-back decisions, and the secrets each container
references live **in the plan**, so the same scenario and seed store the same
secrets and read back the same ones. The payload bytes are not in the plan: they
are derived from the plan seed and the secret's logical name, so a run stores
byte-identical data every time and a read-back compares against the same bytes.

The two `retrieve_ratio` keys do different jobs. `distribution.retrieve_ratio`
picks the payload read-back candidates: the plan marks each stored secret for
read-back or not, and both `apply` and `chaos` read back only the payloads of
the marked ones. It marks only secrets: every ACL and every consumer is a
read-back candidate. `chaos.retrieve_ratio` is the probability per churn step
that `barbican chaos` reads back one of three things: the payload of a live
marked secret, a live ACL, or a live consumer. Each is read back at most once
per lifetime. It picks no candidates of its own, so with
`distribution.retrieve_ratio: 0` a churn run reads back no payload but still
reads back ACLs and consumers. `chaos.retrieve_ratio: 0` or
`--retrieve-ratio 0` turns all three churn read-backs off. `apply` ignores the
`chaos:` block, so it still reads back every marked secret, every ACL and the
consumers of every container that has any.

The schema sets no payload cap of its own. Barbican's limit is its
`max_allowed_secret_in_bytes` setting, checked against the base64 text, whose
default of 20000 admits up to 15000 raw bytes. The shipped profiles stay at
8192, 10924 characters once encoded. A deployment still on the older default of
10000 refuses a payload above 7500 bytes with HTTP 413.

### Secret ACLs and container consumers

`resources.acls` and `resources.consumers` ask for that many secret ACLs and
container consumers. An ACL belongs to one stored secret and lists users that
may read it. A consumer belongs to one container and records a name and a
URL. Neither is an object of its own in Barbican: an ACL is addressed through
its secret (`/v1/secrets/<id>/acl`) and a consumer through its container
(`/v1/containers/<id>/consumers`).

The generator draws both last, after the containers, so adding them to a
scenario leaves the rest of its plan as it was. A count of `0` draws nothing,
and a plan without either kind carries no `acls` or `consumers` key.

- The ACLs come first, named `acl-0001`, `acl-0002` and so on. The generator
  picks `resources.acls` of the stored secrets at random and sets one ACL on
  each, in the secrets' plan order. An order's secret never gets one. Each ACL
  draws a user count from `distribution.users_per_acl` and that many user
  ids, 32 hex characters each, drawn from the seed and written into the plan.
  Barbican's ACL validator accepts any string of up to 255 characters and
  never asks Keystone, so the ids name no real user and a run needs no user
  beyond its own.
- The consumers come next, named `con-0001`, `con-0002` and so on. Each draws
  its container uniformly, so a container can carry several consumers or
  none.

`apply --dry-run` prints an `acls:` and a `consumers:` line after the
`containers:` line, each only when its count is above zero. For the `small`
profile they read:

```
  acls:            3 (7 users)
  consumers:       3
```

`--set resources.acls=<n>`, `--set resources.consumers=<n>`,
`--set distribution.users_per_acl.min=<n>` and
`--set distribution.users_per_acl.max=<n>` override the keys.

A negative count or one above the limit fails like any other, e.g.
`resources.acls must not be negative, got -1` or
`resources.consumers exceeds the limit of 1000000, got 1000001`. Three more
rules follow, checked in this order:

- `resources.acls (11) exceeds resources.secrets (10)`, since a secret
  carries one ACL
- `distribution.users_per_acl.min must be at least 1 when resources.acls > 0, got 0`,
  since an ACL written without users sets nothing
- `resources.consumers is 2 but resources.containers is 0`

The expansion ends with a check of the plan, which tests the ACLs and then
the consumers after the containers and reports the first failure:

- `acl has an empty name`
- `duplicate acl name "<name>"`
- `acl "<name>" references unknown secret "<s>"`, where an order's name does
  not count as a secret
- `secret "<s>" carries more than one acl`
- `acl "<name>" lists no users`
- `acl "<name>" has an empty user`
- `acl "<name>" lists user "<u>" more than once`
- `consumer has an empty name`
- `duplicate consumer name "<name>"`
- `consumer "<name>" references unknown container "<c>"`

`apply` and `monitor` run eight stages: they store the secrets, submit the
orders, create the containers, set the ACLs, register the consumers, and then
read back the payloads, the ACLs and the consumers. An ACL is written with
`PUT /v1/secrets/<id>/acl` and the body `{"read":{"users":[...]}}`. The body
carries no `project-access` key, so Barbican sets it to `true` and the secret
stays readable and deletable by its project. The read-back reads the ACL with
`GET` and compares the users of its `read` entry with the plan's as a set, in
any order. A consumer is registered with `POST /v1/containers/<id>/consumers`
under the name `dizzy-<run>-con-NNNN` and the URL
`https://dizzy.invalid/consumers/<name>`. `.invalid` is a reserved top-level
domain, and nothing contacts the URL. The consumer read-back runs once per
container that has consumers: it lists the container's consumers, 100 per
page, and compares them by name and URL with the plan's consumers of that
container. It is exact, so a missing and an extra consumer are both a
mismatch.

Each ACL costs `apply` two requests, the `PUT` and the `GET`, and each
consumer one, the `POST`, plus one list request per 100 consumers of each
container that has any. For `small` that adds 12 requests to an `apply` run
or a `monitor` iteration.

A read-back that differs from what was written is a mismatch of the same
weight as a payload mismatch. It is never retried, fails `apply` and the
`monitor` iteration, records `error.kind=mismatch`, and counts toward the
non-zero exit of `barbican chaos`. The errors read:

- `barbican: acl mismatch: acl "<name>" reads back users [...], want [...]`
- `barbican: consumer mismatch: container <id> lacks consumer "<name>"`
- `barbican: consumer mismatch: container <id> lists unexpected consumer "<name>"`

`chaos` churns both kinds under their parent. It sets an ACL only while its
secret is live and registers a consumer only while its container is live, and
it never deletes a secret with a live ACL or a container with a live consumer.
Each read-back is a mutation drawn with `chaos.retrieve_ratio`; see
[The `chaos:` block](#the-chaos-block). A consumer's churn read-back checks
only that the consumer is in its container's list, not the whole list, since
other consumers of the container come and go meanwhile. The churn deletes an
ACL with `DELETE /v1/secrets/<id>/acl` and a consumer with
`DELETE /v1/containers/<id>/consumers`, with the name and URL in the body, and
counts a 404 as deleted.

The quota pre-check also compares the plan's consumers with Barbican's
`consumers` quota, in `chaos` as in `apply`. Barbican has no quota for ACLs.
Setting an ACL and registering a consumer need no role beyond the one that
stores the secret and creates the container.

The run record lists an ACL as kind `acl`, with a `logical` such as
`acl-0001`, an empty `name` and its secret's id as `id`, since an ACL has no
name or id of its own. It lists a consumer as kind `consumer`, with a
`logical` such as `con-0001`, the `name` `dizzy-<run>-con-0001` and the id
`<container id>:dizzy-<run>-con-0001`. `barbican status` shows an ACL as
`present` when its `read` entry lists a user, and as `gone` when it lists
none or its secret is gone. It shows a consumer as `present` when its
container lists it, and as `gone` otherwise or when its container is gone.

The teardown deletes neither kind itself. Barbican deletes a secret's ACL with
the secret and a container's consumers with the container, and the teardown
deletes the run's secrets and containers. The count of deleted resources
leaves both kinds out. A bare `--run-id` reaches both, since it reaches the
secrets and containers. The leak check and the `monitor` sweep are unchanged,
since neither kind outlives its secret or container.

### Profiles

Expanded counts at each profile's shipped seed, as `barbican apply --dry-run`
prints them:

| Profile | Secrets | Payload total | Read-backs | Orders | Containers | References | ACLs | ACL users | Consumers | Chaos duration |
|---|---|---|---|---|---|---|---|---|---|---|
| `small` | 10 | 40565 bytes | 7 | 5 | 3 | 8 | 3 | 7 | 3 | 5m |
| `medium` | 40 | 171992 bytes | 19 | 15 | 10 | 22 | 10 | 19 | 10 | 30m |
| `large` | 100 | 435955 bytes | 48 | 40 | 25 | 48 | 25 | 45 | 25 | 1h |

All three set `users_per_acl` to `{ min: 1, max: 3 }`. Each order generates a
secret on top of the stored ones, so the profiles need 15, 55, and 140 secrets
against the secrets quota, and 3, 10, and 25 consumers against the consumers
quota. Barbican's project quotas default to unlimited (`-1`), so none of the
profiles carries a quota risk there; on a project with lowered quotas the
`apply` pre-check names the ones that block.

## Octavia

```yaml
name: small
seed: 42
provider: ""                    # empty selects the cloud's default; --set provider=ovn or amphora

resources:
  networks:        1            # VIP networks, one /24 subnet each
  loadbalancers:   2            # load balancers with their VIP on those subnets
  listeners:       4            # TCP listeners, each with one default pool
  members:         8            # pool members at synthetic addresses
  health_monitors: 2            # TCP health monitors on a drawn subset of the pools

distribution:
  listener_port: { min: 1024, max: 65535 }   # TCP port drawn per listener, distinct per load balancer
  member_port:   { min: 1024, max: 65535 }   # TCP port drawn per member

chaos:                                 # `octavia chaos --scenario scenarios/octavia/small.yaml` runs as-is
  duration: 5m                         # total runtime; the only hard stop besides Ctrl-C
  interval: { min: 200ms, max: 3s }    # random delay drawn per churn tick
  parallel: { max: 4 }                 # per-tick fan-out, drawn in [1, max]; capped by --concurrency
  churn_ratio: 0.5                     # create bias at steady state (0..1)
  target_fill: 0.8                     # fraction of the envelope kept populated (0..1)
  update_ratio: 0.3                    # probability per step of an admin-state toggle on a live listener or member
```

Every object is created by dizzy, so a run references no pre-existing load
balancer, network, or backend. A member's address is synthetic and nothing
answers there, so its operating status reads `ERROR` under a health monitor
and `NO_MONITOR` without one; neither is a failure. `provider` is passed to
Octavia as given, without a local check, and Octavia answers an unknown or
disabled provider with HTTP 400 at the first load balancer create. Every
listener has exactly one pool, so `resources.listeners` is also the pool count
and there is no `pools` key.

| Key | Type | Meaning |
|---|---|---|
| `seed` | int | Seed of the listener-port, member-port, and monitored-pool draws; the global `--seed` flag overrides it |
| `provider` | string | Octavia provider driver, e.g. `ovn` or `amphora`; empty selects the cloud's default |
| `resources.networks` | int | VIP networks dizzy creates, one `/24` subnet each; at most 255, and at least 1 when `loadbalancers > 0` |
| `resources.loadbalancers` | int | Load balancers, each with its VIP on a VIP subnet; at least 1 when `listeners > 0`, and at most `listeners` |
| `resources.listeners` | int | TCP listeners, each with one default TCP pool, so also the pool count; at least 1 when `members > 0`, and at most `members` |
| `resources.members` | int | Pool members at synthetic addresses; at most 127 on one VIP network |
| `resources.health_monitors` | int | TCP health monitors, one each on a drawn subset of the pools; at most `listeners` |
| `distribution.listener_port.min` / `distribution.listener_port.max` | range | TCP port drawn per listener, within 1 to 65535; it must span at least as many ports as one load balancer gets listeners |
| `distribution.member_port.min` / `distribution.member_port.max` | range | TCP port drawn per member, within 1 to 65535 |

The plan assigns every object deterministically and draws only the ports and
the monitored pools, so the same scenario and seed always build the same tree:

- VIP network `i`, counting from 1, is `net-<nnnn>` with subnet `sub-<nnnn>`
  and CIDR `10.0.<i>.0/24`, as Nova's companion networks are. The subnet's
  gateway is `.1` and its allocation pool `.2` to `.127`, so Neutron never
  hands a VIP or an amphora port an address the plan gives a member.
- Load balancers go round-robin over the networks: `lb-0001` on `net-0001`,
  `lb-0002` on `net-0002` when there is one, and so on.
- Listeners go round-robin over the load balancers. Each listener's port is
  drawn from `listener_port` in listener order, and a port its load balancer
  already has steps to the next free one, wrapping from `max` to `min`.
- Pool `pool-<nnnn>` is the default pool of listener `lsn-<nnnn>`.
- Members go round-robin over the pools and are named `<pool>-member-<nn>`,
  counting each pool's members from 1, e.g. `pool-0001-member-01`. Each takes
  the next unused address of its load balancer's network counting up from
  `.128`, so one network holds at most 127 members. Each member's port is
  drawn from `member_port` in member order.
- Health monitors `hm-<nnnn>` go to `health_monitors` distinct pools drawn
  from all of them, in ascending pool order.

Only `octavia chaos` reads the `chaos:` block, and `generate` and `apply`
ignore it; see [The `chaos:` block](#the-chaos-block) for its keys, among them
`update_ratio`, the probability per churn step of an admin-state toggle on a
live listener or member.

### Profiles

Expanded counts at each profile's shipped seed, as `octavia apply --dry-run`
prints them:

| Profile | Networks | Load balancers | Listeners | Pools | Members | Health monitors | Provider | Chaos duration |
|---|---|---|---|---|---|---|---|---|
| `small` | 1 | 2 | 4 | 4 | 8 | 2 | cloud default | 5m |
| `medium` | 2 | 5 | 10 | 10 | 30 | 5 | cloud default | 30m |
| `large` | 3 | 15 | 30 | 30 | 120 | 15 | cloud default | 1h |

Every profile leaves `provider` empty and so runs against the cloud's default
provider; `--set provider=ovn` or `--set provider=amphora` selects one. An
amphora load balancer needs `--timeout 5m`. An OVN provider without TCP health
monitor support runs a profile with `--set resources.health_monitors=0`.
Octavia's project quotas default to unlimited (`-1`), so the profiles are
bounded by build time rather than by quota; on a project with lowered quotas
the `apply` pre-check names the ones that block.

## Mix

A mix scenario describes a combined run: the workload personas that run side by
side, how they divide one server envelope, the background lanes that run next
to them, and the opt-in services bound to them. It has no `distribution`
block, because each persona block shapes its own workload.

```yaml
name: small
seed: 42

image: cirros          # shared by every persona; referenced by name
flavor: m1.tiny        # shared by every persona; referenced by name

services: []           # opt-in services: octavia gives the gardener persona load balancers

resources:
  servers: 6           # the server envelope the personas divide by their shares

personas:
  ci:
    share: 0.5
    cloud: ""                                 # "" authenticates with --os-cloud
    networks: 2
    volumes_per_server: { min: 0, max: 1 }
    volume_gib:         { min: 1, max: 2 }
    interval:           { min: 100ms, max: 1s }
    churn_ratio: 0.5
    target_fill: 0.6
  gardener:
    share: 0.3
    cloud: ""
    clusters: 1
    policy: soft-anti-affinity                # or anti-affinity
    volume_gib:         { min: 1, max: 2 }
    interval:           { min: 10s, max: 1m }
    octavia:                                  # read only with services: [octavia]
      provider: ""                            # "" uses the cloud's default; ovn or amphora (needs --timeout 5m)
      loadbalancers: 1                        # per cluster, VIP on the cluster's network
      listeners: 2                            # per load balancer: ports 80 and 443, each with a pool of every worker
      health_monitors: true                   # a TCP health monitor on every pool
  legacy:
    share: 0.2
    cloud: ""
    networks: 1
    resize_flavor: m1.small                   # "" disables resize
    cold_migration: true                      # optional; false disables cold migration
    volumes_per_server: { min: 1, max: 2 }
    volume_gib:         { min: 1, max: 2 }
    ports_per_server:   { min: 0, max: 1 }
    interval:           { min: 10s, max: 1m }

lanes:                 # single-service churn next to the personas; off by default
  cinder:   { enabled: false, cloud: "", profile: small }   # also volume_type
  glance:   { enabled: false, cloud: "", profile: small }
  keystone: { enabled: false, cloud: "", profile: small }   # also privilege, domain, roles
  neutron:  { enabled: false, cloud: "", profile: small }   # also external_network

chaos:
  duration: 5m
  parallel: { max: 4 }
```

The CI persona boots short-lived servers: each on one of the persona's
networks, with zero or more data volumes, no extra port and no lifecycle
operation.

The Gardener persona keeps Kubernetes clusters as Gardener runs them on
OpenStack. Its servers are spread over `clusters` clusters round-robin, so
cluster sizes differ by at most one. A cluster is one network, one server group
with the block's `policy`, and the worker servers booted into that group on
that network, each with one data volume and no extra port. The networks and
server groups are created before anything else and stay until the run's
teardown. Once every worker of a cluster is up, the persona replaces the
cluster's workers one at a time: it deletes a worker with its volume and boots
it again under the same name. The clusters are replaced concurrently. The block
has no `churn_ratio` or `target_fill`, because the persona keeps its clusters
whole.

With `octavia` under `services`, every cluster also keeps the load balancers
its `octavia` block describes, each with its VIP on the cluster's network. Each
listener has one pool that holds every worker of the cluster on the pool's
NodePort, counting from 30000 over the persona's pools in plan order. The load
balancers, listeners, pools and health monitors stay until the run's teardown,
which deletes the load balancers before the networks. A worker's members are
deleted before the worker and created again once its replacement is up. See
[Opt-in services](../explanation/combined-runs.md#opt-in-services).

The Legacy persona boots long-lived servers: each on one of the persona's
networks, with its data volumes and extra ports. Its servers, volumes and ports
are created before anything else and stay until the run's teardown. Each
mutation of a server runs one of a stop and start, a resize to the other of
`flavor` and `resize_flavor`, a live migration or a cold migration; each
mutation of a volume or port detaches it or attaches it again. The two
migrations need the admin role and two usable compute hosts, and a run without
them skips both. `cold_migration: false` turns cold migration off for the run
and leaves live migration to that pre-check; a server then draws from the
operations that remain. The block has no `churn_ratio` or `target_fill`,
because the persona keeps every planned resource.

| Key | Type | Meaning |
|---|---|---|
| `image` | string | Boot image of every persona, referenced by name (or id) |
| `flavor` | string | Boot flavor of every persona, referenced by name (or id) |
| `services` | list of strings | Opt-in services, each bound to the personas that use it; no entry may be empty or appear twice. This build supports `octavia`, which gives the Gardener persona load balancers and needs a Gardener persona with a server |
| `resources.servers` | int | Server envelope the personas divide by their shares, from 0 to 1,000,000; a plan needs at least 1 |
| `personas.ci.share` | float | Share of `resources.servers`, a finite number of at least 0; at least one persona needs a share above 0 |
| `personas.ci.cloud` | string | `clouds.yaml` entry the persona authenticates with; empty uses `--os-cloud` |
| `personas.ci.networks` | int | Networks the persona's servers are spread over, one per server; at least 1 when the persona has servers |
| `personas.ci.volumes_per_server` | range | Data volumes attached per server |
| `personas.ci.volume_gib` | range | Size drawn per data volume; `min >= 1` when `volumes_per_server.max > 0` |
| `personas.ci.interval.min` / `personas.ci.interval.max` | duration | Random delay range drawn per tick of the persona's engine; `0` falls back to `200ms` / `3s` |
| `personas.ci.churn_ratio` | ratio | Create bias of the persona's engine at equilibrium; `0` falls back to `0.5` |
| `personas.ci.target_fill` | ratio | Fraction of the persona's servers kept populated; `0` falls back to `0.8` |
| `personas.gardener.share` | float | Share of `resources.servers`, a finite number of at least 0; at least one persona needs a share above 0 |
| `personas.gardener.cloud` | string | `clouds.yaml` entry the persona authenticates with; empty uses `--os-cloud` |
| `personas.gardener.clusters` | int | Clusters the persona's servers are spread over, each with its own network and server group; with a share above 0, at least 1 and at most the persona's servers |
| `personas.gardener.policy` | string | Policy of every cluster's server group, `anti-affinity` or `soft-anti-affinity`; required with a share above 0. `anti-affinity` needs a compute host per worker of a cluster |
| `personas.gardener.volume_gib` | range | Size drawn for the one data volume of each worker; `min >= 1` |
| `personas.gardener.interval.min` / `personas.gardener.interval.max` | duration | Random delay range drawn per tick of the persona's engine; `0` falls back to `200ms` / `3s` |
| `personas.gardener.octavia.provider` | string | Octavia provider driver of every load balancer, e.g. `ovn` or `amphora`; empty selects the cloud's default. Like the three keys below, read only when `services` lists `octavia`, and the bounds below are checked only then |
| `personas.gardener.octavia.loadbalancers` | int | Load balancers per cluster, each with its VIP on the cluster's network; at least 1 |
| `personas.gardener.octavia.listeners` | int | TCP listeners per load balancer, each with a default pool of every worker of the cluster; from 1 to 10. They listen on ports 80, 443, then 8080 onward |
| `personas.gardener.octavia.health_monitors` | bool | `true` puts one TCP health monitor on every pool; `false` creates none |
| `personas.legacy.share` | float | Share of `resources.servers`, a finite number of at least 0; at least one persona needs a share above 0 |
| `personas.legacy.cloud` | string | `clouds.yaml` entry the persona authenticates with; empty uses `--os-cloud` |
| `personas.legacy.networks` | int | Networks the persona's servers are spread over, one per server; at least 1 when the persona has servers |
| `personas.legacy.resize_flavor` | string | Second flavor the servers alternate with, referenced by name (or id); must differ from `flavor`. Empty disables resize |
| `personas.legacy.cold_migration` | bool | `false` turns cold migration off and leaves live migration to the migration pre-check. Omitted means `true` |
| `personas.legacy.volumes_per_server` | range | Data volumes per server, each detached and attached again |
| `personas.legacy.volume_gib` | range | Size drawn per data volume; `min >= 1` when `volumes_per_server.max > 0` |
| `personas.legacy.ports_per_server` | range | Extra ports per server, each detached and attached again |
| `personas.legacy.interval.min` / `personas.legacy.interval.max` | duration | Random delay range drawn per tick of the persona's engine; `0` falls back to `200ms` / `3s` |
| `lanes.<name>.enabled` | bool | Switches the lane on; `<name>` is `cinder`, `glance`, `keystone` or `neutron`. Omitted means off |
| `lanes.<name>.cloud` | string | `clouds.yaml` entry the lane authenticates with; empty uses `--os-cloud` |
| `lanes.<name>.profile` | string | Bundled profile of the lane's service, `small`, `medium` or `large`. An enabled lane sets `profile` or `scenario`, not both |
| `lanes.<name>.scenario` | string | Path to a scenario file of the lane's service, relative to the working directory |
| `lanes.cinder.volume_type` | string | Volume type the lane's volumes are created with, by name or id; empty uses the cloud's default type |
| `lanes.keystone.privilege` | string | Privilege tier of the lane, `auto`, `admin` or `domain-manager`; empty means `auto` |
| `lanes.keystone.domain` | string | In-scope domain in domain-manager mode; empty uses the domain the token is scoped to |
| `lanes.keystone.roles` | string | Existing roles to reuse in domain-manager mode, comma-separated; empty means `member,reader` |
| `lanes.neutron.external_network` | string | External network for gateways and floating IPs, by name; empty uses the first external network |
| `chaos.duration` | duration | Total wall-clock runtime of every persona's and lane's engine |
| `chaos.parallel.max` | int | Per-tick fan-out of each persona's engine, drawn in `[1, max]` |
| `chaos.bucket_width` | duration | Width of one time bucket of a run with `--duration 0`, for every persona and lane |

With `octavia` under `services`, every pool of the Gardener persona takes one
NodePort, so its pools, `clusters × loadbalancers × listeners`, number at most
2768, the ports 30000 to 32767. A larger block fails with
`personas.gardener.octavia: <n> pools (clusters x loadbalancers x listeners) exceed the 2768 node ports 30000-32767`.

The `--set` keys are `seed`, `image`, `flavor`, `resources.servers`,
`services` (a comma-separated list; an empty value clears it), and every
`personas.ci.*`, `personas.gardener.*` and `personas.legacy.*` key of the
table, with `personas.ci.volumes_per_server.min`, `.max` and the like for the
ranges and Go duration strings for the intervals, e.g.
`--set personas.ci.interval.max=2s`,
`--set personas.gardener.policy=anti-affinity`,
`--set personas.legacy.resize_flavor=` or
`--set personas.legacy.cold_migration=false`. The Gardener persona's Octavia
block has the keys `personas.gardener.octavia.provider`,
`personas.gardener.octavia.loadbalancers`,
`personas.gardener.octavia.listeners` and
`personas.gardener.octavia.health_monitors`, with `true` or `false` for
`health_monitors`, e.g.
`--set services=octavia --set personas.gardener.octavia.provider=ovn`. The
`--set` keys also include
`lanes.<name>.enabled`, `lanes.<name>.cloud`, `lanes.<name>.profile` and
`lanes.<name>.scenario` for `cinder`, `glance`, `keystone` and `neutron`, with
`true` or `false` for `enabled`, and `lanes.cinder.volume_type`,
`lanes.keystone.privilege`, `lanes.keystone.domain`, `lanes.keystone.roles`
and `lanes.neutron.external_network`, e.g.
`--set lanes.keystone.enabled=true` or
`--set lanes.cinder.profile= --set lanes.cinder.scenario=lanes/cinder.yaml`.
No `--set` key reaches into a lane's scenario; name a scenario file for that.
The `chaos:` block has no `--set` keys; the `mix chaos` flags override it.

### Shares

`resources.servers` is divided among the personas with the largest-remainder
method. Each persona gets `floor(servers × share / sum of shares)` servers, and
the servers left over go one each to the personas with the largest fractional
remainder, ties to the persona whose name sorts first. For example, 20 servers
at shares 5, 3 and 2 give 10, 6 and 4 servers, and 7 servers at three equal
shares give 3, 2 and 2.

A persona that gets no server is left out of the plan, and a scenario in which
no persona gets a server fails with `generated plan failed validation: plan has
no persona with at least one server`. The plan records each persona's share
divided by the sum of all shares. Each persona's compute plan is generated
under a seed derived from the scenario seed and the persona name, so the same
scenario and seed always yield the same plan.

### Lanes

A lane is the churn of one service, the graph that service's own `chaos`
command builds, run in an engine of its own next to the personas. An enabled
lane names its scenario in exactly one way: `profile` takes the service's
bundled profile, `scenarios/<name>/<profile>.yaml`, and `scenario` reads a
scenario file of the service, the format its own `chaos` command reads.
Validation checks every enabled lane, in the order `cinder`, `glance`,
`keystone`, `neutron`, and reports the first of:

- `lanes.<name>: set profile or scenario, not both`
- `lanes.<name>: an enabled lane needs a profile or a scenario`
- `lanes.<name>.profile must be small, medium or large, got "<value>"`
- `lanes.keystone.privilege must be auto, admin or domain-manager, got "<value>"`

A disabled lane is not checked. A scenario file that cannot be read fails with
`lanes.<name>: reading scenario <path>: …`, and one that does not parse with
`lanes.<name>: parsing scenario: …`.

The lane's scenario is named `<mix scenario name>/<lane>`, for example
`small/keystone`, and seeded with a seed derived from the mix scenario seed and
the lane name, the derivation the personas use. Its own `name` and `seed`
therefore have no effect, and the plan lists every enabled lane under `lanes`,
in the order above, with that seed. A lane whose plan has nothing to churn
(no volumes, no images, neither projects nor users, or no networks, routers
and security groups) fails with
`lanes.<name>: the scenario plans no resources to churn`.

A lane takes the interval, the churn ratio, the target fill, its service's
mutate ratio (`resize_ratio`, `lifecycle_ratio` or `token_ratio`) and
`parallel.max` from its own scenario's `chaos:` block. An omitted key falls
back to the default of its service's `chaos` command, and `--max-parallel`
overrides `parallel.max`. It takes the duration, the unbounded mode of
`--duration 0` and the bucket width from the run, so its own `chaos.duration`
and `chaos.bucket_width` have no effect, and the mix scenario's
`chaos.parallel.max` does not apply to it.

### Profiles

| Profile | `resources.servers` | CI servers | Gardener servers | Legacy servers | `personas.gardener.clusters` | Workers per cluster |
|---|---|---|---|---|---|---|
| `small` | 6 | 3 | 2 | 1 | 1 | 2 |
| `medium` | 20 | 10 | 6 | 4 | 2 | 3 |
| `large` | 60 | 30 | 18 | 12 | 3 | 6 |

| Profile | `personas.ci.networks` | `personas.legacy.networks` | `chaos.duration` |
|---|---|---|---|
| `small` | 2 | 1 | 5m |
| `medium` | 4 | 2 | 30m |
| `large` | 8 | 2 | 1h |

All three set `seed: 42`, `image: cirros`, `flavor: m1.tiny`, `services: []`
and `chaos.parallel.max: 4`. They give the CI persona `share: 0.5`,
`cloud: ""`, `volumes_per_server: { min: 0, max: 1 }`,
`volume_gib: { min: 1, max: 2 }`, `interval: { min: 100ms, max: 1s }`,
`churn_ratio: 0.5` and `target_fill: 0.6`; the Gardener persona `share: 0.3`,
`cloud: ""`, `policy: soft-anti-affinity`, `volume_gib: { min: 1, max: 2 }`,
`interval: { min: 10s, max: 1m }` and an `octavia` block of `provider: ""`,
`loadbalancers: 1`, `listeners: 2` and `health_monitors: true`; and the Legacy
persona `share: 0.2`, `cloud: ""`, `resize_flavor: m1.small`,
`volumes_per_server: { min: 1, max: 2 }`,
`volume_gib: { min: 1, max: 2 }`, `ports_per_server: { min: 0, max: 1 }` and
`interval: { min: 10s, max: 1m }`. Each needs the flavor `m1.small` for the
Legacy persona's resizes. None sets `cold_migration`, so each cold-migrates
when the migration pre-check allows it. The profiles use the soft policy so
that a cluster boots on a cloud with fewer compute hosts than the cluster has
workers.
`small` fits Nova's common default quota of 10 instances and its default
limits of 10 server groups and 10 members per group; `medium` and `large` need
raised quotas.

`services` stays empty, so the profiles run on a cloud without an Octavia
endpoint, and `--set services=octavia` opts a run in. The Gardener persona
then keeps, per profile:

| Profile | Load balancers | Listeners | Pools | Members | Health monitors |
|---|---|---|---|---|---|
| `small` | 1 | 2 | 2 | 4 | 2 |
| `medium` | 2 | 4 | 4 | 12 | 4 |
| `large` | 3 | 6 | 6 | 36 | 6 |

Every profile leaves `provider` empty and so runs against the cloud's default
provider; `--set personas.gardener.octavia.provider=ovn` or
`--set personas.gardener.octavia.provider=amphora` selects one. An amphora
load balancer needs `--timeout 5m`. An OVN release without TCP health monitor
support runs a profile with
`--set personas.gardener.octavia.health_monitors=false`. Octavia's project
quotas default to unlimited (`-1`); on a project with lowered quotas the
Gardener persona's pre-check names the ones that block.

All three carry the same four lanes, each with `enabled: false`, `cloud: ""`
and `profile: small`. They use the services' `small` profiles because
Keystone's `small` is the only one a domain manager can run. A lane on the
default cloud shares the quota of every persona that also names none.

## C5C3

A c5c3 scenario names where the orders go and what they request. It has no
`seed`, `resources` or `distribution`: every c5c3 scenario orders the same five
orders and two refusals, so its plan is a function of the scenario alone. A
`seed:` key fails the strict parse like any unknown key.

```yaml
name: small
namespace: dizzy-orders          # Kubernetes namespace the orders are created in
control_plane:
  name: controlplane
  namespace: openstack           # optional; empty means the orders' own namespace
role: member                     # must be on the namespace entry's allowedRoles
refused_role: dizzy-refused      # must not be on allowedRoles
catalog:
  service_type: dizzy-probe      # a type no OpenStack client resolves
  endpoint_url: https://dizzy-probe.invalid/v1
rotation:
  grace_period: 2m               # spec.rotation.gracePeriod of the credential order
timeouts:
  ready: 15m                     # create, rotation, repair and refusal waits
  gone: 15m                      # wait for a deleted order to disappear
checks:
  secret_repair: true
```

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | Profile name, recorded in the run record |
| `namespace` | string | required | Kubernetes namespace the orders are created in, which a ControlPlane must assign |
| `control_plane.name` | string | required | The ControlPlane every order names in `spec.controlPlaneRef.name` |
| `control_plane.namespace` | string | empty | The ControlPlane's namespace, written to `spec.controlPlaneRef.namespace`. Empty leaves that key out, and the operator reads the orders' own namespace |
| `role` | string | required | The role the `role` order assigns the user on the project. It must be on the namespace entry's `allowedRoles` |
| `refused_role` | string | `dizzy-refused` | The role of the `refused-role` order, which the operator must refuse with `RoleNotAllowed`. It must not be on `allowedRoles`. The default names no role in Keystone, so a ControlPlane that admits it by mistake grants nothing |
| `catalog.service_type` | string | required | `spec.serviceType` of the `catalog` order |
| `catalog.endpoint_url` | string | required | URL of the `catalog` order's one endpoint, of interface `public` |
| `rotation.grace_period` | duration | `2m` | `spec.rotation.gracePeriod` of every application credential order: how long the superseded credential stays in Keystone after a rotation |
| `timeouts.ready` | duration | `15m` | Bound of each wait for an order to turn ready, rotate, restore its Secret or report its refusal, and of cleanup's wait for a user order's password generation to settle |
| `timeouts.gone` | duration | `15m` | Bound of each wait for a deleted order to disappear. The wait for the superseded application credential to go lasts up to `rotation.grace_period` plus this bound |
| `checks.secret_repair` | bool | `true` | Edit and delete each delivered Secret and wait until the operator restores it. Absent means `true`; `false` turns the check off for a deployment that grants only `get` on Secrets |

Validation rejects each of these with an error that names the key:

- an empty `name`, `namespace`, `control_plane.name`, `role`,
  `refused_role`, `catalog.service_type` or `catalog.endpoint_url`
- a `namespace`, or a non-empty `control_plane.namespace`, that is not a
  DNS-1123 label: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, at most 63 characters
- a `role` or `refused_role` that contains a comma or is longer than 255
  characters, and a `role` equal to `refused_role`
- a `catalog.service_type` that does not match the label pattern above, is
  longer than 63 characters, or is `identity`
- a `catalog.endpoint_url` longer than 1024 characters, whose scheme is not
  `http` or `https` written in lowercase, or whose host is empty, since the
  KeystoneCatalogEntry CRD admits no other URL
- a `rotation.grace_period` below `10s`, or of `720h` or more, since the CRD
  requires the grace period to be shorter than the default rotation interval
  of 720 hours
- a `timeouts.ready` or `timeouts.gone` that is not positive

`--set` takes `namespace`, `control_plane.name`, `control_plane.namespace`,
`role`, `refused_role`, `catalog.service_type`, `catalog.endpoint_url`,
`rotation.grace_period`, `timeouts.ready`, `timeouts.gone` and
`checks.secret_repair`, e.g. `--set role=reader` or
`--set checks.secret_repair=false`. Any other key fails with an error that
names it.

The plan holds the same orders and refusals for every scenario, in this
order:

| Plan entry | Logical name | Kind | Fields from the scenario | Expected reason |
|---|---|---|---|---|
| order | `user` | `keystone_user` | | |
| order | `project` | `keystone_project` | | |
| order | `role` | `keystone_role_assignment` | `role` | |
| order | `catalog` | `keystone_catalog_entry` | `catalog.service_type`, `catalog.endpoint_url` | |
| order | `appcred` | `keystone_application_credential` | | |
| refusal | `refused-appcred` | `keystone_application_credential` | | `NoRoleOnProject` |
| refusal | `refused-role` | `keystone_role_assignment` | `refused_role` | `RoleNotAllowed` |

The role assignments and the application credentials reference the `user` and
`project` orders. Each entry is created as a custom resource of its kind,
`KeystoneUser`, `KeystoneProject`, `KeystoneRoleAssignment`,
`KeystoneCatalogEntry` or `KeystoneApplicationCredential`, named
`dizzyorder-<runid>-<logical>`. The operator names the Keystone user after its
order, so a run's user is `dizzyorder-<runid>-user`. See
[`c5c3 apply`](cli.md#c5c3-apply) for the stages that create them.

### Profiles

One profile ships, `scenarios/c5c3/small.yaml`, the file above. It is named
`small` so that `make devstack-c5c3 DEVSTACK_C5C3_SERVICE=c5c3` finds it under
`scenarios/c5c3/small.yaml`. It carries no `chaos:` block, since the namespace
has no `chaos`. Its ControlPlane, `controlplane` in `openstack`, is the one
the Cobalt Core ControlPlane quick start creates. A run of it lasts at least
its 2 minute grace period.

## Horizon

```yaml
name: small
seed: 42
resources:
  sessions: 5      # journeys of one apply, and the envelope of chaos
  users: 0         # 0: log in as the clouds.yaml user. N: create N users and log in as them
  projects: 2      # projects created when users > 0; ignored otherwise
role: member       # existing role of each created user on each created project
pages:             # loaded after the login; each must answer 200
  - name: api_access
    path: /project/api_access/
  - name: identity
    path: /identity/
distribution:
  pages_per_session: { min: 1, max: 2 }   # how many of the pages a session loads
  switch_ratio: 1.0                       # fraction of sessions that switch project
chaos:
  duration: 5m
  interval: { min: 1s, max: 5s }
  parallel: { max: 2 }
  churn_ratio: 0.5
  target_fill: 0.8
  browse_ratio: 0.3                       # probability per step that a live session browses
```

A Horizon scenario has `role` and `pages` beside `resources` and
`distribution`. With `resources.users: 0` every session logs in as the user of
the `clouds.yaml` entry and a run creates nothing; `resources.projects` and
`role` are then ignored and not validated. With `resources.users: N` a run
creates N users and `resources.projects` projects in the domain of the
`clouds.yaml` user and grants each user `role` on each project. See
[Check the Horizon dashboard](../how-to/check-horizon.md).

| Key | Type | Meaning |
|---|---|---|
| `resources.sessions` | int | Sessions of one `apply`, each one journey, and the sessions `chaos` churns; at least 1 |
| `resources.users` | int | Users to create; `0` logs in as the `clouds.yaml` user. At most `resources.sessions`; session *i* logs in as user *(i-1) mod users* |
| `resources.projects` | int | Projects to create when `resources.users > 0`; then at least 1 |
| `role` | string | Existing role each created user gets on each created project, looked up by name; required when `resources.users > 0` |
| `pages[].name` | string | Logical name of a page, matching `^[a-z0-9_]{1,32}$` and unique; it names the metric kind `page_<name>` |
| `pages[].path` | string | Path below the dashboard's base URL; starts with one `/`, lies outside `/auth/`, and carries no `?`, `#` or whitespace |
| `distribution.pages_per_session` | range | How many of the pages a session loads, at most the number of pages; a session loads them in the scenario's order |
| `distribution.switch_ratio` | ratio | Fraction of sessions that switch project after their pages and land on the first page |

`pages` is a list, so it has no `--set` key. The `--set` keys are `seed`,
`resources.sessions`, `resources.users`, `resources.projects`, `role`,
`distribution.pages_per_session.min`, `distribution.pages_per_session.max` and
`distribution.switch_ratio`.

A session that switches picks its target with its switch draw among the
run's created projects, or, without them, among the enabled projects of the
`clouds.yaml` user.

### Profiles

| Profile | `sessions` | `users` | `projects` | `chaos.duration` | `chaos.interval` | `chaos.parallel.max` |
|---|---|---|---|---|---|---|
| `small` | 5 | 0 | 2 | 5m | 1s to 5s | 2 |
| `medium` | 25 | 0 | 2 | 30m | 500ms to 3s | 4 |
| `large` | 100 | 0 | 2 | 1h | 200ms to 2s | 8 |

The three differ in nothing else. All log in as the `clouds.yaml` user, so each
runs without admin rights; `--set resources.users=5` switches a profile to
created users. Both pages answer `200` on a control plane that runs only
Keystone and Horizon. `/project/` does not: its default panel needs the
compute service.

## The `chaos:` block

Optional, and read only by the `chaos` command. It adds a temporal frame to the
scenario's spatial envelope. CLI flags override these values; an omitted field
falls back to the command's default.

```yaml
chaos:                                # the block shipped by scenarios/neutron/medium.yaml
  duration: 30m                       # total runtime; the only hard stop besides Ctrl-C
  interval: { min: 200ms, max: 2s }   # random delay drawn per churn tick
  parallel: { max: 6 }                # per-tick fan-out; capped by --concurrency
  churn_ratio: 0.5                    # create bias at steady state (0..1)
  target_fill: 0.7                    # fraction of the envelope kept populated (0..1)
```

| Key | Type | Services | Meaning |
|---|---|---|---|
| `duration` | duration | all | Total wall-clock runtime. `0` means unset; only `--duration 0` selects a run without an end |
| `interval.min` / `interval.max` | duration | all but mix | Random delay range drawn per tick |
| `parallel.max` | int | all | Per-tick fan-out, drawn in `[1, max]`, capped by `--concurrency` |
| `churn_ratio` | ratio | all but mix | Create bias at equilibrium |
| `target_fill` | ratio | all but mix | Fraction of the envelope kept populated |
| `bucket_width` | duration | all | Width of one time bucket of a run with `--duration 0`, at least `1m`. `0` or omitted falls back to `1h`; a bounded run ignores it |
| `resize_ratio` | ratio | cinder | Probability per step of extending a live volume to its planned target |
| `token_ratio` | ratio | keystone | Probability per step of issuing a token as a live, assigned user or authenticating with a live application credential or trust. `0` switches all three off |
| `lifecycle_ratio` | ratio | nova | Probability per step of mutating a live server (stop/start, resize, or live-migrate) |
| `lifecycle_ratio` | ratio | glance | Probability per step of mutating a live image (deactivate/reactivate, visibility flip, member add/remove, or metadata churn) |
| `retrieve_ratio` | ratio | barbican | Probability per step of one read-back: the payload of a live secret the plan marks for read-back, byte-compared; the users of a live ACL; or a live consumer in its container's list. `0` switches all three off |
| `update_ratio` | ratio | octavia | Probability per step of setting a live listener or member to `admin_state_up=false` and back to `true` |
| `browse_ratio` | ratio | horizon | Probability per step that a live session browses: loads its pages and switches project as its plan says, at most once per login. Omitted falls back to `0.3`; `0` switches browsing off |

Every built-in profile except `scenarios/c5c3/small.yaml` carries a `chaos:`
block, so `chaos` runs any of them with no flags at all; the `c5c3` namespace
has no `chaos`. A mix scenario's block holds only `duration`,
`parallel.max` and `bucket_width`; its interval, churn ratio and target fill
are set per persona. A background lane of a mix run reads its own scenario's
block instead; see [Lanes](#lanes). See [The churn engine](../explanation/churn-engine.md) for
what `churn_ratio` and `target_fill` actually control.

## The `pressure:` block

Keystone only, optional, and read only by `keystone pressure`. Every other
`keystone` command and the `keystone` lane of a mix run parse and validate it
but use none of its values, and it takes no part in the plan. It sets the
closed-loop load the workers put on the scenario's identity resources. The
`keystone pressure` flags override its values; an omitted key falls back to
the command's default.

The example below sets every key. Its second stage grows the fan users'
grants, so the block validates only in a scenario with fan users. It fits the
scenario of [A fan for C5C3/cobaltcore#1459](#a-fan-for-c5c3cobaltcore1459)
in place of that scenario's own block, and takes its duration, workers and
tokens from `scenarios/keystone/small.yaml`.

```yaml
pressure:            # in place of the block of the fan-1459 scenario above
  duration: 5m       # runtime of the workers; the only hard stop besides Ctrl-C
  workers: 8         # closed-loop workers
  tokens: 20         # token pool, issued before the first worker starts
  churn_users: 1     # users reserved for grant_churn, from the end of the plan's regular users
  mix:               # an omitted key keeps its default; 0 switches it off
    validate:    60
    effective:    5
    revoke:       2
    grant_churn:  2
    issue:        1
    probe:        0  # off by default; sends a pool token to Nova, Neutron, Cinder or Glance
    bad_token:    0  # off by default; floods a service with tokens it has to reject
  propagation:       # optional; omitted or every: 0 runs no propagation probe
    every: 30s       # revoke one pool token at this interval and poll a service with it
    timeout: 10m     # ceiling of one probe; a token still accepted after it is a timeout
  stages:            # optional; consecutive windows of a bounded run
    - duration: 1m          # omitted or 0: an equal share of what the others leave
    - fan_assignments: 500  # grants every fan user holds from this stage on; needs fan users
```

| Key | Type | Meaning |
|---|---|---|
| `duration` | duration | Total wall-clock runtime of the workers. `0` or omitted means unset; only `--duration 0` selects a run without an end |
| `workers` | int | Closed-loop workers; the merged value must lie between 1 and 1024. `0` or omitted falls back to `16` |
| `tokens` | int | Size of the token pool. `0` or omitted falls back to `100` |
| `churn_users` | int | Users reserved for `grant_churn`, taken from the end of the plan's regular users, so a fan user is never one; they never enter the token pool. Reserved only when `grant_churn` has a positive weight. Omitted falls back to `1`; `0` reserves none and then needs a `grant_churn` weight of `0` |
| `mix.validate` | int | Weight of `validate`. Omitted falls back to `60` |
| `mix.effective` | int | Weight of `effective`. Omitted falls back to `5` |
| `mix.revoke` | int | Weight of `revoke`. Omitted falls back to `2` |
| `mix.grant_churn` | int | Weight of `grant_churn`. Omitted falls back to `2` |
| `mix.issue` | int | Weight of `issue`. Omitted falls back to `1` |
| `mix.probe` | int | Weight of `probe`: a draw sends a pool token to one of Nova, Neutron, Cinder and Glance, drawn at random among those the token's catalog lists, for the cheapest read the service offers. Omitted falls back to `0`, so a run sends no probe unless this key or `--mix probe=<n>` sets a weight |
| `mix.bad_token` | int | Weight of `bad_token`: a draw picks a service as `probe` does and sends it a token it has to reject, a random one or a pool token the run revoked at least 5 minutes earlier, and expects a 401. It loads the services' rejection path and belongs on a lab, never on a shared cloud. Omitted falls back to `0`; `--mix bad_token=<n>` sets it from the command line |
| `propagation.every` | duration | Interval of the propagation probe: at every interval it revokes one pool token, puts a new token for the same user and project into the pool as `revoke` does, and polls a service with the revoked token once a second until the service answers 401 or `propagation.timeout` has passed. `0` or omitted switches the probe off. `--propagation-every` overrides it, and `--propagation-every 0` switches the probe off over a block's value |
| `propagation.timeout` | duration | Ceiling of one propagation probe: a probe whose service still accepts the revoked token after it counts as a timeout. Must not be negative. `0` or omitted falls back to `10m`. `--propagation-timeout` overrides it |
| `bucket_width` | duration | Width of one time bucket of a run with `--duration 0`, at least `1m`. `0` or omitted falls back to `1h`. A bounded run keeps ten equal buckets of a tenth of its duration, but a value under `1m` still fails |
| `stages[].fan_assignments` | int | The grants every fan user holds once the run has entered the stage, at least `1`. Omitted means the stage grants nothing. See [Stages](#stages) |
| `stages[].duration` | duration | The stage's share of the run. `0` or omitted means an equal share of what the stages with a duration leave. See [Stages](#stages) |

A worker draws an operation with probability its weight divided by the sum of
the weights. A mix whose merged weights are all `0` fails with
`pressure mix: every weight is 0; at least one operation needs a positive weight`.

The `keystone pressure` flags override every key but `stages`, which has no
flag: `--mix` sets one weight of `mix`, and `--propagation-every` and
`--propagation-timeout` set the two keys of `propagation`.

A negative value fails validation with a message that names its key, e.g.
`pressure.workers must not be negative, got -1`,
`pressure.mix.revoke must not be negative, got -1`,
`pressure.propagation.every must not be negative, got -1s` or
`pressure.propagation.timeout must not be negative, got -1s`. An unknown key
in the block fails parsing, as everywhere in a scenario.

All three Keystone profiles carry a `pressure:` block with `duration`,
`workers` and `tokens` (see [Keystone](#keystone)), so `keystone pressure`
runs any of them with no flags. None sets `churn_users`, `mix`, `propagation`,
`bucket_width` or `stages`, none sets `distribution.fan_users`, and none
declares `resources.implied_roles`; the scenario of
[A fan for C5C3/cobaltcore#1459](#a-fan-for-c5c3cobaltcore1459) is the
starting point for a run with fan users, implied roles and stages. The block
has no `--set` keys. See
[`pressure`](cli.md#pressure) for what each operation sends and
[Pressure mode](../explanation/pressure-mode.md) for why the load is built
this way.

### Stages

`stages` divides a bounded run into consecutive stages, in list order. A
stage with a `duration` keeps it as its window; the stages without one share
equally what the others leave of the run duration, and when every stage has a
`duration` and they add up to less, the last stage runs to the end. A stage
with `fan_assignments` grows every fan user's grants to that count when the
run enters it, and a stage without one is a window with no override. A stage
never removes a grant. See [`pressure`](cli.md#pressure) for how the command
resolves the windows against the run duration and enters each stage.

Validation names a stage by its 0-based index in the list, while the run's
log lines, record and reports number the stages from 1. It reports the first
failure of these checks, made on each stage in turn:

- `pressure.stages[0].duration must not be negative, got -1m0s`
- `pressure.stages[0].fan_assignments must be at least 1, got 0`

and then of these, made in turn on the stages that set `fan_assignments`:

- `pressure.stages[0] sets fan_assignments but distribution.fan_users is 0`
- `pressure.stages[0].fan_assignments (6) exceeds resources.projects (5)`
- `pressure.stages[0].fan_assignments (2) is below distribution.fan_assignments.max (3), which a fan user may already hold`,
  for the first stage that sets one
- `pressure.stages[1].fan_assignments (3) is below stage 0's (4); a stage never removes assignments`,
  for a later one, which is held to the previous stage that set one

So the first stage that sets `fan_assignments` sets at least
`distribution.fan_assignments.max`, and every later one at least the value
before it, and a stage's `fan_assignments` is the count every fan user holds
in it. The checks against the run duration, a run without an end among them,
belong to the command; see [`pressure`](cli.md#pressure).
