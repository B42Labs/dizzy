# Scenario schema

A scenario is a YAML file describing a workload by counts, ranges, and ratios,
plus an RNG seed that makes its expansion deterministic. Each service has its own
schema — and therefore its own `--set` keys — so a key that belongs to another
service is rejected rather than silently ignored.

Every scenario shares this top-level shape:

```yaml
name: <string>       # profile name, recorded in the run record
seed: <int>          # deterministic; same seed => same plan
image / flavor / resize_flavor   # nova only; referenced by name, never created
resources:  { … }    # fixed counts
distribution: { … }  # ranges and ratios drawn per resource
topology:   { … }    # neutron only
chaos:      { … }    # optional; consumed only by the `chaos` command
pressure:   { … }    # keystone only; optional; consumed only by `keystone pressure`
```

A `{ min: <int>, max: <int> }` **range** is drawn once per resource, inclusive at
both ends. A **ratio** is a float from 0 to 1.

The global `--seed` flag overrides the file's `seed`. Individual values are
overridden with `--set <dotted.key>=<value>`.

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
fan user. The fan users are drawn after everything else, so adding them to a
scenario leaves the rest of its plan as it was.

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

### A fan for C5C3/cobaltcore#1459

C5C3/cobaltcore#1459 makes the effective-assignment listing linear in the
number of grants. Showing that takes one user with thousands of grants and a
measurement at several sizes. This scenario gives one fan user 500 grants and
grows them to 1000, 2000 and 4000 in four stages of 5 minutes each, while the
workers draw `validate` and `effective` only:

```yaml
name: fan-1459
seed: 42

resources:
  domains:  1
  roles:    2
  projects: 4000
  users:    20

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
    grant_churn:  0
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
  roles:           2
  projects:        4000
  users:           21
  fan users:       1
  assignments:     542 (1 domain-scoped)
  token issues:    11
```

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

| Profile | Domains | Roles | Projects | Users | Assignments | Token issues | Chaos duration | Pressure |
|---|---|---|---|---|---|---|---|---|
| `small` | 1 | 2 | 5 | 10 | 21 | 3 | 5m | 5m, 8 workers, 20 tokens |
| `medium` | 2 | 3 | 20 | 50 | 90 | 25 | 30m | 30m, 32 workers, 200 tokens |
| `large` | 3 | 4 | 50 | 150 | 296 | 78 | 1h | 1h, 128 workers, 1000 tokens |

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

services: []           # opt-in services bound to every persona; this build supports none

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
| `services` | list of strings | Opt-in services bound to every persona; no entry may be empty or appear twice. This build supports none |
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

The `--set` keys are `seed`, `image`, `flavor`, `resources.servers`,
`services` (a comma-separated list; an empty value clears it), and every
`personas.ci.*`, `personas.gardener.*` and `personas.legacy.*` key of the
table, with `personas.ci.volumes_per_server.min`, `.max` and the like for the
ranges and Go duration strings for the intervals, e.g.
`--set personas.ci.interval.max=2s`,
`--set personas.gardener.policy=anti-affinity`,
`--set personas.legacy.resize_flavor=` or
`--set personas.legacy.cold_migration=false`. They also include
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
`cloud: ""`, `policy: soft-anti-affinity`, `volume_gib: { min: 1, max: 2 }` and
`interval: { min: 10s, max: 1m }`; and the Legacy persona `share: 0.2`,
`cloud: ""`, `resize_flavor: m1.small`, `volumes_per_server: { min: 1, max: 2 }`,
`volume_gib: { min: 1, max: 2 }`, `ports_per_server: { min: 0, max: 1 }` and
`interval: { min: 10s, max: 1m }`. Each needs the flavor `m1.small` for the
Legacy persona's resizes. None sets `cold_migration`, so each cold-migrates
when the migration pre-check allows it. The profiles use the soft policy so
that a cluster boots on a cloud with fewer compute hosts than the cluster has
workers.
`small` fits Nova's common default quota of 10 instances and its default
limits of 10 server groups and 10 members per group; `medium` and `large` need
raised quotas.

All three carry the same four lanes, each with `enabled: false`, `cloud: ""`
and `profile: small`. They use the services' `small` profiles because
Keystone's `small` is the only one a domain manager can run. A lane on the
default cloud shares the quota of every persona that also names none.

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
| `token_ratio` | ratio | keystone | Probability per step of issuing a token as a live, assigned user |
| `lifecycle_ratio` | ratio | nova | Probability per step of mutating a live server (stop/start, resize, or live-migrate) |
| `lifecycle_ratio` | ratio | glance | Probability per step of mutating a live image (deactivate/reactivate, visibility flip, member add/remove, or metadata churn) |

All eighteen built-in profiles carry a `chaos:` block, so `chaos` runs any of
them with no flags at all. A mix scenario's block holds only `duration`,
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
`bucket_width` or `stages`, and none sets `distribution.fan_users`; the
scenario of [A fan for C5C3/cobaltcore#1459](#a-fan-for-c5c3cobaltcore1459)
is the starting point for a run with fan users and stages. The block has no
`--set` keys. See
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
