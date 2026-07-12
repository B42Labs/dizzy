# Deal with the quota pre-check

`neutron apply`, `cinder apply`, and `nova apply` compare the expanded plan
against your project's quotas **before creating anything**, and abort with an
itemized message if the quotas are too low:

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

## When the pre-check cannot run

If the project cannot read its own quota, the pre-check **fails open**: it logs a
warning and proceeds. The executor's quota fast-fail is the backstop — the first
over-quota response from the API (HTTP 409 for Neutron, HTTP 413 for Cinder,
HTTP 403 for Nova) stops the run immediately rather than retrying.

The pre-check exists to give a good error message early, not to be a second
authorization system. It must never refuse work the cloud would have allowed.

## Keystone has no quota pre-check

Keystone has no default per-resource quotas, so `keystone apply` runs a
**privilege pre-check** in that slot instead. If it fails, the problem is your
credentials, not a limit. See
[Keystone's privilege model](../explanation/privilege-model.md).
