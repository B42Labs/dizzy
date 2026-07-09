# Run against a Cobalt Core dev stack

The `devstack-c5c3` Make targets drive a local [Cobalt Core "forge" control
plane](https://c5c3.github.io/forge/quick-start-controlplane.html) instead of a
real cloud. That quick-start brings up **Keystone + Horizon**, so the targets
default to the `keystone` service and to admin credentials — exactly the tier
Keystone's `apply` and `monitor` need.

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
$ make devstack-c5c3 KEEP=1                                                 # keep the resources and the run record
```

Note that `scenarios/keystone/medium.yaml` declares two domains and is therefore
admin-only — fine here, since the `devstack-c5c3` targets use admin credentials.
See [Keystone's privilege model](../explanation/privilege-model.md).

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
