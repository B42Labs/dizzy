# Run against a Cobalt Core dev stack

The `devstack-c5c3` Make targets drive a local [Cobalt Core "forge" control
plane](https://c5c3.github.io/forge/quick-start-controlplane.html) instead of a
real cloud. That quick-start brings up **Keystone + Horizon**, so the targets
default to the `keystone` service and to admin credentials — exactly the tier
Keystone's `apply` and `monitor` need, and the only one `pressure` accepts.
`DEVSTACK_C5C3_SERVICE=c5c3` orders Keystone resources through the stack's
c5c3-operator instead; see
[Order through the c5c3-operator](#order-through-the-c5c3-operator).

## The two target families

The same binary, two clouds:

| | devstack-osism | devstack-c5c3 |
|---|---|---|
| cloud | OSISM testbed | Cobalt Core control plane |
| run once + clean up | `make devstack-osism` | `make devstack-c5c3` |
| monitor + OTLP export | `make devstack-osism-monitor` | `make devstack-c5c3-monitor` |
| clouds file | `contrib/devstack-osism-clouds.yaml` (committed) | `contrib/devstack-c5c3-clouds.yaml` (generated, gitignored) |
| cloud name | `test` (`DEVSTACK_OSISM_OS_CLOUD`) | `devstack-c5c3` (`DEVSTACK_C5C3_OS_CLOUD`) |
| default service | `neutron` (`DEVSTACK_OSISM_SERVICE`) | `keystone` (`DEVSTACK_C5C3_SERVICE`) |
| TLS | testbed CA (`contrib/devstack-osism.pem`) | `verify: false` (self-signed, dev only) |

## Bring up the control plane

Follow the forge quick-start. Roughly:

```console
$ WITH_CONTROLPLANE=true KIND_HOST_PORT=8443 make deploy-infra
$ # apply the ControlPlane CR
$ kubectl wait controlplane/controlplane -n openstack --for=condition=Ready
```

## Run a scenario

```console
$ make devstack-c5c3                                                        # keystone apply of the small profile, then clean up
$ make devstack-c5c3 DEVSTACK_C5C3_SCENARIO=scenarios/keystone/medium.yaml
$ make devstack-c5c3 DEVSTACK_C5C3_CMD=chaos ARGS="--duration 10m"
$ make devstack-c5c3 DEVSTACK_C5C3_CMD=pressure ARGS="--duration 5m"        # closed-loop token load, then clean up
$ make devstack-c5c3 DEVSTACK_C5C3_CMD=pressure ARGS="--mix probe=30 --propagation-every 30s"  # also needs Nova, Neutron, Cinder or Glance
$ make devstack-c5c3 KEEP=1                                                 # keep the resources and the run record
```

The quick-start's Horizon is checked with `DEVSTACK_C5C3_SERVICE=horizon`. This
repository records no Horizon hostname for the stack, so pass the dashboard's
URL; `<URL>` below is that placeholder:

```console
$ make devstack-c5c3 DEVSTACK_C5C3_SERVICE=horizon ARGS="--horizon-url <URL>"   # scenarios/horizon/small.yaml as the admin user
$ make devstack-c5c3 DEVSTACK_C5C3_SERVICE=horizon ARGS="--horizon-url <URL> --set resources.users=5"
```

Every session logs in as the admin user of the generated `clouds.yaml`, or,
with `--set resources.users`, as users the run creates; the cleanup step after
the run deletes those and finds nothing to delete otherwise. The entry's
`verify: false` applies to the dashboard's connections too. See
[Check the Horizon dashboard](check-horizon.md).

Note that `scenarios/keystone/medium.yaml` declares two domains and is therefore
admin-only — fine here, since the `devstack-c5c3` targets use admin credentials.
See [Keystone's privilege model](../explanation/privilege-model.md).

## Put pressure on the token path

`DEVSTACK_C5C3_CMD=pressure` runs `keystone pressure`, which validates,
revokes and lists role assignments of the run's own users as fast as Keystone
answers. The dev stack is the place for it: every `revoke` is a real
revocation event, and on Keystone 2026.2 each one flushes the token validation
cache for every user of that Keystone, so the verb never belongs on a shared
cloud. See [`pressure`](../reference/cli.md#pressure) and
[Pressure mode](../explanation/pressure-mode.md).

`--mix probe=30` also sends pool tokens to Nova, Neutron, Cinder and Glance,
and `--propagation-every 30s` revokes a pool token every 30 s and polls one of
those services with it until it answers 401, for at most the 10 minutes
`--propagation-timeout` defaults to. The propagation probe's revocations are
real revocation events too. Both flags need at least one of the four services
in the pool users' catalog. The quick-start's control plane runs Keystone and
Horizon, none of the four, so a run with either flag needs a control plane that
also runs Nova, Neutron, Cinder or Glance. On a stack whose catalog lists none
of them, the run stops at priming with
`pressure probe needs at least one of nova, neutron, cinder, glance in the pool users' catalog; the first pool token's catalog lists none`
and tears down what it created. Against a build without C5C3/cobaltcore#1463
a probe may poll for up to 300 s, and a probe still polling when the run ends
is not recorded, so give such a run a `--duration` well above the 5 minutes of
the default `small` profile.

The run tears its own resources down and runs its leak check before it exits.
The target's cleanup step then runs `keystone cleanup --run` on the new run
record as after any other command. The resources are already gone and a 404
counts as success, so the step succeeds and deletes the record. To keep the
record for `keystone report`, pass `KEEP=1`: the pressure run has already torn
its resources down unless `ARGS` contains `--no-cleanup`.

```console
$ make devstack-c5c3 DEVSTACK_C5C3_CMD=pressure ARGS="--duration 5m" KEEP=1
$ ./dizzy keystone report --run run-<id>.json
```

Each Keystone profile's `pressure:` block sets a duration too, 5 minutes for
the default `small` profile, so `ARGS` may stay empty.

## Order through the c5c3-operator

`DEVSTACK_C5C3_SERVICE=c5c3` runs `c5c3 apply` with
`scenarios/c5c3/small.yaml` against the same stack, then `c5c3 cleanup --run`
on the new run record. The profile orders from the ControlPlane `controlplane`
in `openstack`, the one the quick-start creates. Before the first run, create
the profile's namespace `dizzy-orders` and assign it on that ControlPlane,
with `member` on its `allowedRoles` and `allowCatalogEntries` set, as
[Prepare the namespace](test-c5c3-orders.md#prepare-the-namespace) shows.

```console
$ make devstack-c5c3 DEVSTACK_C5C3_SERVICE=c5c3                                          # c5c3 apply of the small profile, then clean up
$ make devstack-c5c3 DEVSTACK_C5C3_SERVICE=c5c3 ARGS="--set checks.secret_repair=false"
$ make devstack-c5c3 DEVSTACK_C5C3_SERVICE=c5c3 KEEP=1                                   # keep the orders and the run record
```

The target regenerates `contrib/devstack-c5c3-clouds.yaml` and passes
`--os-cloud devstack-c5c3`, as for every service, but c5c3 logs in with no
admin credential. dizzy takes only the entry's `auth_url`, which is
`DEVSTACK_C5C3_AUTH_URL`, and its `verify: false`, and puts them in place of
the `auth_url` and TLS settings of each document the operator delivers. Those
documents name the in-cluster Service
`http://controlplane-keystone.openstack.svc:5000/v3`, which the host does not
resolve. The admin password in the entry stays unused.

dizzy reaches the cluster through `$KUBECONFIG` or `~/.kube/config` and its
current context, the one `kubectl` uses. `DEVSTACK_C5C3_KUBE_CONTEXT` reaches
only the `kubectl` calls that read the admin Secret, and `ARGS` reaches the
`apply` command but not the cleanup step. To use another context, switch to it
with `kubectl config use-context` before the run rather than passing
`--kube-context` in `ARGS`, so both steps use it.

The cleanup step takes the namespace from the run record. A run lasts at least
the profile's 2 minute grace period. There is no `c5c3 chaos` and no
`c5c3 pressure`, so leave `DEVSTACK_C5C3_CMD` at `apply`. With `KEEP=1` the
orders stay in place; look at them and remove them later with:

```console
$ ./dizzy c5c3 status --run run-<id>.json
$ OS_CLIENT_CONFIG_FILE=contrib/devstack-c5c3-clouds.yaml ./dizzy c5c3 cleanup --os-cloud devstack-c5c3 --run run-<id>.json
```

`status` sends no Keystone request and needs no `clouds.yaml`. `cleanup` sends
a login with each deleted credential that must fail, so it needs the endpoint
override as well.

`make devstack-c5c3-monitor DEVSTACK_C5C3_SERVICE=c5c3` runs
`c5c3 monitor --otel` the same way, and its series carry
`cloud="devstack-c5c3"` and `service="c5c3"`. See
[Test c5c3-operator orders](test-c5c3-orders.md) for what a run checks and
how to read its failures.

## Why `clouds.yaml` is regenerated every time

The admin password is a live Kubernetes Secret
(`controlplane-keystone-admin-credentials` in namespace `openstack`), not a
committed value. So `make devstack-c5c3` first runs `make devstack-c5c3-clouds`,
which reads that Secret and rewrites `contrib/devstack-c5c3-clouds.yaml` — a
gitignored `clouds.yaml` carrying the current password and `verify: false`.

Because it is regenerated on every run, a recreated control plane with a new
password is picked up automatically. The stack serves a self-signed certificate,
which is why `verify: false` is there; it matches the quick-start's
`openstack --insecure` and `curl -k`.

Run it standalone to point `openstack` or `dizzy` at the stack yourself:

```console
$ make devstack-c5c3-clouds
$ OS_CLIENT_CONFIG_FILE=contrib/devstack-c5c3-clouds.yaml dizzy keystone status --run run-<id>.json
```

## Override the endpoint and cluster details

On Linux with rootful Docker the quick-start uses port 443, so drop the `:8443`:

```console
$ make devstack-c5c3 DEVSTACK_C5C3_AUTH_URL=https://keystone.127-0-0-1.nip.io/v3
$ make devstack-c5c3 DEVSTACK_C5C3_KUBE_CONTEXT=kind-forge DEVSTACK_C5C3_NAMESPACE=openstack
```

Every `DEVSTACK_C5C3_*` variable is documented in the `Makefile` header:
`DEVSTACK_C5C3_OS_CLOUD`, `DEVSTACK_C5C3_CLOUDS_FILE`, `DEVSTACK_C5C3_AUTH_URL`,
`DEVSTACK_C5C3_NAMESPACE`, `DEVSTACK_C5C3_SECRET`, `DEVSTACK_C5C3_KUBE_CONTEXT`,
`DEVSTACK_C5C3_SERVICE`, `DEVSTACK_C5C3_SCENARIO`, `DEVSTACK_C5C3_CMD`.

## Monitor the dev stack

`make devstack-c5c3-monitor` is the analogue of `make devstack-osism-monitor`: it
runs `keystone monitor --otel` against the dev stack, exporting into the same
local VictoriaMetrics under `cloud="devstack-c5c3"`, and honours the same cadence
knobs.

```console
$ make otel-up
$ make devstack-c5c3-monitor
$ make devstack-c5c3-monitor MONITOR_INTERVAL=5m MONITOR_ITERATIONS=1
$ make otel-grafana
```

See [Export metrics to OpenTelemetry](export-to-otel.md) for the local stack.
