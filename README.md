# dizzy

A scenario-driven load and consistency tester for OpenStack control planes.

`dizzy` builds large, randomized but **reproducible** workloads through the
OpenStack APIs, records how long every operation takes and which states the
resources reach, and cleans up after itself. It covers five services today:

| Namespace | Service | What it exercises |
|---|---|---|
| `dizzy neutron` | Networking | Address scopes, subnet pools, networks, subnets, routers, router interfaces, security groups + rules, ports, floating IPs |
| `dizzy cinder` | Block storage | Volume create, extend (resize), snapshot |
| `dizzy keystone` | Identity | Domains, roles, projects, users, role assignments, scoped token issue |
| `dizzy nova` | Compute | Server boot (image / volume), stop/start (soft / hard), resize + confirm, live migration, volume & port attach/detach, multi-network, user data |
| `dizzy glance` | Image | Image create + synthetic data upload, metadata/property churn, visibility transitions (private / shared / community / public), member add/accept/remove, deactivate/reactivate, delete |

Every namespace offers the same five verbs — `generate`, `apply`, `chaos`,
`monitor`, `cleanup` — plus `status` and `report`. A scenario expands
deterministically into a plan; applying that plan produces a run record you can
re-query, report on, and tear down by tag.

It is **not** a correctness suite like Tempest. It measures latency, error
rates, and state convergence under load. The two are complementary. It creates
no load balancers (no Octavia).

## Installation

Pre-built binaries for **linux/amd64**, **linux/arm64**, and **darwin/arm64**
are published on the [releases page](https://github.com/B42Labs/dizzy/releases),
alongside a `checksums.txt`, a cosign signature and certificate per file, and
SPDX/CycloneDX SBOMs.

```sh
VERSION=v0.1.0
ASSET=dizzy-linux-amd64

curl -fsSLO https://github.com/B42Labs/dizzy/releases/download/$VERSION/$ASSET
curl -fsSLO https://github.com/B42Labs/dizzy/releases/download/$VERSION/checksums.txt

grep " $ASSET$" checksums.txt | sha256sum -c -
chmod +x $ASSET
sudo install $ASSET /usr/local/bin/dizzy

dizzy --version
```

To verify the cosign signature as well, and to build from source instead, see
[How to install and verify a release](docs/how-to/install-and-verify.md).

## Quick start

`dizzy` authenticates through `clouds.yaml` exactly as the `openstack` CLI does,
honoring `$OS_CLOUD` and the standard search paths.

```console
$ export OS_CLOUD=mycloud
$ dizzy neutron list-networks                                  # auth smoke test
$ dizzy neutron apply --scenario scenarios/neutron/small.yaml  # build a topology
$ dizzy neutron report --run run-<id>.json                     # see the timings
$ dizzy neutron cleanup --run run-<id>.json                    # tear it down
```

`--scenario` takes a filesystem path, and the fifteen built-in profiles live under
`scenarios/` in this repository — so clone it even if you installed a release
binary. The `small` profile fits Neutron's default per-project quotas and runs
against a fresh project with nothing raised.

**New here?** Work through [Your first run](docs/tutorial/first-run.md) — it
takes about ten minutes and leaves nothing behind.

## Documentation

The documentation follows [Diátaxis](https://diataxis.fr/). Pick the column that
matches what you are doing right now.

**[Tutorial](docs/tutorial/first-run.md)** — learning by doing, start here.

- [Your first run](docs/tutorial/first-run.md)

**How-to guides** — recipes for a specific goal, assuming you know the basics.

- [Install and verify a release](docs/how-to/install-and-verify.md)
- [Monitor a cloud continuously](docs/how-to/monitor-a-cloud.md)
- [Run a churn soak with `chaos`](docs/how-to/run-a-soak.md)
- [Export metrics to OpenTelemetry](docs/how-to/export-to-otel.md)
- [Run against a Cobalt Core dev stack](docs/how-to/run-against-devstack-c5c3.md)
- [Deal with the quota pre-check](docs/how-to/raise-quotas.md)

**Reference** — dry, complete, look-it-up material.

- [CLI](docs/reference/cli.md) — every command and flag
- [Scenario schema](docs/reference/scenario-schema.md) — every YAML key, per service
- [Metrics](docs/reference/metrics.md) — instruments, attributes, PromQL names
- [Run record](docs/reference/run-record.md) — the `run-<id>.json` format

**Explanation** — why the tool is built the way it is.

- [Scenario, plan, and run](docs/explanation/scenario-plan-run.md)
- [Determinism and reproducibility](docs/explanation/determinism.md)
- [The churn engine](docs/explanation/churn-engine.md)
- [Resource identity and cleanup](docs/explanation/resource-identity.md)
- [Keystone's privilege model](docs/explanation/privilege-model.md)

## Safety

- `dizzy` operates only within the project of the selected `clouds.yaml` entry.
- Every resource it creates carries a run identifier — a Neutron tag, Cinder
  metadata, a Keystone name prefix, (for Nova) server and volume metadata, or
  (for Glance) an image tag. `cleanup` deletes strictly on that identifier, so it
  never touches resources the tool did not create.
- `apply --dry-run` previews a plan without making a single API call.
- There are no destructive defaults; the cloud and project are always explicit.

One exception is worth knowing before you run `monitor`: its pre-flight sweep
reclaims **any** `dizzy`-tagged resource in the project, not just its own. Do not
run two `dizzy` processes against the same project concurrently. The details are
in [Resource identity and cleanup](docs/explanation/resource-identity.md).

## Development

Requires Go 1.26 (see `go.mod`).

```console
$ make build     # build ./dizzy
$ make test      # go test ./...
$ make lint      # golangci-lint
$ make fmt       # gofmt
$ make help      # every target
```

`make devstack-osism` runs a scenario against the OSISM testbed defined in
`contrib/devstack-osism-clouds.yaml`, cleaning up afterwards; `make devstack-c5c3`
does the same against a local [Cobalt Core dev
stack](docs/how-to/run-against-devstack-c5c3.md). `make otel-up` boots a local
kind + VictoriaMetrics + Grafana stack for exercising `--otel` end to end; see
[Export metrics to OpenTelemetry](docs/how-to/export-to-otel.md).
