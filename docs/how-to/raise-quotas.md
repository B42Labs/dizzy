# Deal with the quota pre-check

`neutron apply`, `cinder apply`, `nova apply`, `barbican apply`, and
`octavia apply` compare the expanded plan against your project's quotas
**before creating anything**, and abort with an itemized message if the quotas
are too low:

> `glance apply` runs **no** quota pre-check. Glance exposes no project-quota API
> to compare a plan against — its image count and per-image size caps are
> deployment configuration, not a per-project quota — so an over-limit request
> surfaces only during the run, as a fast-failed 413.

```
error: plan exceeds project quota; raise these quotas before applying:
networks need 100, quota 10; subnets need 200, quota 10; routers need 20, quota 10
```

Nothing was created. You have three ways forward.

## Option 1: Raise the quotas

The pre-check names exactly which quotas block the plan and what the plan needs.
Raise them with admin credentials:

```console
$ openstack quota set --networks 120 --subnets 240 --routers 30 \
    --secgroups 20 --secgroup-rules 300 --ports 400 <project>
```

`dizzy` never raises quotas itself. Doing so would require admin credentials it
otherwise never needs, and silently expanding a project's limits is not a
side effect a load tester should have.

The Neutron quota names the pre-check reports map to `openstack quota set` as
follows:

| Pre-check name | `openstack quota set` flag |
|---|---|
| `networks` | `--networks` |
| `subnets` | `--subnets` |
| `routers` | `--routers` |
| `ports` | `--ports` |
| `security groups` | `--secgroups` |
| `security group rules` | `--secgroup-rules` |
| `subnet pools` | `--subnetpools` |
| `floating IPs` | `--floating-ips` |

For Cinder, the names are `volumes`, `snapshots`, and `gigabytes`. With
`--volume-type` set, the per-type quotas `volumes_<type>`, `snapshots_<type>`,
and `gigabytes_<type>` are checked as well.

For Barbican, the names are `secrets`, `orders`, and `containers`, the keys of
Barbican's project quotas.

For Octavia, the names are `loadbalancer`, `listener`, `pool`, `member`, and
`healthmonitor`, the keys of Octavia's project quotas. Raise them with the
load-balancer client:

```console
$ openstack loadbalancer quota set --loadbalancer <n> --listener <n> \
    --pool <n> --member <n> --healthmonitor <n> <project>
```

## Option 2: Shrink the plan

Override the counts that blocked, without editing the scenario file:

```console
$ dizzy neutron apply --scenario scenarios/neutron/medium.yaml \
    --set resources.networks=8 --set resources.routers=4
```

Check what you get first with `--dry-run`, which runs the same pre-check:

```console
$ dizzy neutron apply --scenario scenarios/neutron/medium.yaml --dry-run \
    --set resources.networks=8
```

Beware that some counts have knock-on effects. Each `router_links` entry adds a
transit network of its own, so `--set resources.networks=8` on a scenario with
`router_links: 1` produces **9** networks.

## Option 3: Use the `small` profile

`scenarios/neutron/small.yaml` is sized to fit Neutron's default per-project
quotas — typically 10 networks, 10 subnets, 10 routers, 10 security groups, 100
rules, 50 ports. `scenarios/cinder/small.yaml` fits Cinder's common defaults of
10 volumes, 10 snapshots, and 1000 GiB.

Both run against a fresh project with nothing raised.

## What the pre-check counts

It is not a naive count of the plan's top-level resources. It accounts for the
ports consumed by things that do not look like ports:

- each subnet-attached **router interface** consumes a port;
- each **external gateway** consumes a port;
- **floating IPs** count against their own quota, and are only checked when the
  cloud actually has an external network.

For Cinder, `gigabytes` is the sum of the volumes' *final* sizes after any
planned resize, plus the snapshot sizes at their source volume's final size,
since both count against the same shared quota.

For Nova, the pre-check counts **instances**, **cores**, and **RAM** computed
from the resolved boot flavor — a resized server counts, per dimension, the
larger of its boot and resize flavor, since it holds both across the resize. It
covers **only** the compute quotas: the Cinder gigabytes the companion (and
boot-from-volume root) volumes consume and the Neutron port/network quotas the
companion networks and ports consume are *not* pre-checked. The executor's quota
fast-fail is the backstop there.

For a plan with server groups, which only the `mix` Gardener persona has, the
pre-check also counts the groups against **server_groups** and the largest
group against **server_group_members**. Nova applies the member limit to each
group on its own, so the check compares the largest group with the limit
alone, without usage. The bundled `mix` profiles need 1, 2 and 3 server groups
of up to 2, 3 and 6 members, within Nova's defaults of 10 and 10.

For Barbican, the pre-check counts **secrets**, **orders**, and **containers**.
The secrets include one per order on top of the stored ones, since every order
that turns ACTIVE generates a secret that counts against the same quota: the
`small` profile's 10 secrets and 5 orders need 15 secrets. A negative limit,
which is Barbican's default for every quota, means unlimited and never blocks.
Like Cinder's, the check compares the plan with the limit, not with what the
project has left.

For Octavia, the pre-check counts **load balancers**, **listeners**,
**pools**, **members**, and **health monitors**, with one pool per listener. A
negative limit, which is Octavia's default for every quota, means unlimited and
never blocks. Octavia's quota API reports limits and no usage, so the check
compares the plan with the limits, not with what the project has left: load
balancers already in the project are not counted. The Neutron networks,
subnets, and VIP ports a run consumes are *not* pre-checked, as for Nova's
companion resources. The executor's quota fast-fail is the backstop there.

## When the pre-check cannot run

If the project cannot read its own quota, the pre-check **fails open**: it logs a
warning and proceeds. The executor's quota fast-fail is the backstop — the first
over-quota response from the API (HTTP 409 for Neutron, HTTP 413 for Cinder,
HTTP 403 for Nova) stops the run immediately rather than retrying.

Barbican's pre-check reads its quotas with Cinder's policy. A denied read (HTTP
403) logs `quota pre-check skipped: quota read denied` and proceeds; any other
read failure, including an answer that carries no `quotas` object, aborts the
apply with `reading Barbican quotas for pre-check: …` before anything is
created. Barbican answers an over-quota create with HTTP 403 as well. The
executor counts a 403 as a quota rejection only when its body names a quota,
so a 403 from Barbican's policy stops the run as `http_403` rather than as
`quota`.

Octavia's pre-check follows Nova's quota-read policy. A project id the auth
result does not carry, or a denied read (HTTP 403), skips the pre-check with a
warning; any other read failure aborts the apply with
`reading Octavia quotas for pre-check: …` before anything is created. Octavia
answers an over-quota create with HTTP 403 as well, with a body that reads
`Quota has been met`, so the executor counts it as `quota`, while a 403 from
Octavia's policy stops the run as `http_403`.

The pre-check exists to give a good error message early, not to be a second
authorization system. It must never refuse work the cloud would have allowed.

## Keystone has no quota pre-check

Keystone has no default per-resource quotas, so `keystone apply` runs a
**privilege pre-check** in that slot instead. If it fails, the problem is your
credentials, not a limit. See
[Keystone's privilege model](../explanation/privilege-model.md).
