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

`projects_per_domain` shapes how projects cluster, while the total stays exactly
`resources.projects`: each turn of the deal cycles to the next domain and assigns
a batch drawn from the range.

**Passwords are never in the plan or the run record.** Each user's password is
derived deterministically from `(seed, logical name)` at apply time and held only
in memory for the token-issue step, so runs stay reproducible without persisting
a credential.

### Profiles

Expanded counts at each profile's shipped seed:

| Profile | Domains | Roles | Projects | Users | Assignments | Token issues | Chaos duration |
|---|---|---|---|---|---|---|---|
| `small` | 1 | 2 | 5 | 10 | 21 | 3 | 5m |
| `medium` | 2 | 3 | 20 | 50 | 90 | 25 | 30m |
| `large` | 3 | 4 | 50 | 150 | 296 | 78 | 1h |

`small` keeps a single domain so it stays runnable in domain-manager mode.
`medium` and `large` declare more than one domain and are therefore admin-only.
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
| `duration` | duration | all | Total wall-clock runtime |
| `interval.min` / `interval.max` | duration | all | Random delay range drawn per tick |
| `parallel.max` | int | all | Per-tick fan-out, drawn in `[1, max]`, capped by `--concurrency` |
| `churn_ratio` | ratio | all | Create bias at equilibrium |
| `target_fill` | ratio | all | Fraction of the envelope kept populated |
| `resize_ratio` | ratio | cinder | Probability per step of extending a live volume to its planned target |
| `token_ratio` | ratio | keystone | Probability per step of issuing a token as a live, assigned user |
| `lifecycle_ratio` | ratio | nova | Probability per step of mutating a live server (stop/start, resize, or live-migrate) |

All twelve built-in profiles carry a `chaos:` block, so `chaos` runs any of them
with no flags at all. See [The churn engine](../explanation/churn-engine.md) for
what `churn_ratio` and `target_fill` actually control.
