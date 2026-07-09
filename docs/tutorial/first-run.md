# Your first run

In this tutorial you will build a small network topology on a real OpenStack
cloud, watch how long each API call took, and remove everything again. It takes
about ten minutes.

You will use the `small` Neutron profile throughout. It is deliberately sized to
fit Neutron's default per-project quotas, so it runs against a fresh project
without raising anything, and everything it creates is removed at the end.

## Before you start

You need three things:

1. The `dizzy` binary on your `PATH`. Run `dizzy --version` to confirm.
2. A `clouds.yaml` with an entry for a cloud you may create resources in.
   `dizzy` reads it exactly as the `openstack` CLI does, from the current
   directory, `~/.config/openstack`, or `/etc/openstack`.
3. A checkout of this repository, so the scenario files under `scenarios/` are
   available.

Point `dizzy` at your cloud by exporting `OS_CLOUD`:

```console
$ export OS_CLOUD=mycloud
```

## Step 1: Confirm you can reach the cloud

Before creating anything, check that authentication works:

```console
$ dizzy neutron list-networks
```

You should see the networks that already exist in your project, one per line. If
this fails, fix your `clouds.yaml` before going further — every later step
depends on it.

## Step 2: See what would be created

A *scenario* is a parametrized description of a topology. Expanding it produces
a *plan*: the concrete, fully enumerated list of resources. Ask for a summary of
the plan without touching the cloud at all:

```console
$ dizzy neutron apply --scenario scenarios/neutron/small.yaml --dry-run
Plan for scenario "small" (seed 42)
  address scopes:    1
  subnet pools:      1
  networks:          4
  subnets:           7
  routers:           2 (2 with external gateway)
  router interfaces: 6
  security groups:   2
  ports:             6
  floating IPs:      2
```

Run it a second time. You get exactly the same numbers, because the scenario
pins an RNG seed. That is the property everything else rests on: the same
scenario and seed always expand to the same plan.

If your cloud has no external network, the two floating IPs and the external
gateways are quietly skipped at apply time. Nothing fails.

## Step 3: Look at the plan itself

The summary above is a count. To see the resources themselves, generate the plan
as JSON:

```console
$ dizzy neutron generate --scenario scenarios/neutron/small.yaml
{
  "scenario": "small",
  "seed": 42,
  "addressScopes": [
    {
      "name": "as-0001",
      "ipVersion": 4
    }
  ],
  "subnetPools": [
...
```

Every resource carries a *logical name* — `as-0001`, `net-0001`, `port-0003`.
These are the names the plan uses to refer to resources internally. The real
names on the cloud get a run-specific prefix, which you will see in a moment.

`generate` never makes an API call, so it works with no cloud at all.

## Step 4: Build the topology

Now create it for real:

```console
$ dizzy neutron apply --scenario scenarios/neutron/small.yaml
```

Several things happen in order. `dizzy` reads your project's quotas and compares
them against the plan, aborting before it creates anything if they are too low.
It then creates the resources in dependency order — address scopes before subnet
pools, networks before subnets, security groups before their rules — running
independent resources of the same kind concurrently.

While it runs, each created resource is logged to stderr, along with a periodic
heartbeat showing the cumulative operation count and rate. When it finishes, a
metrics summary goes to stdout and a run record is written to the current
directory:

```
run record: run-a1b2c3d4.json
```

Note that filename. Every resource on the cloud is now tagged `dizzy:run=a1b2c3d4`
and named `dizzy-a1b2c3d4-net-0001` and so on. Look for them in Horizon or with
`openstack network list` if you like — the run id makes them easy to spot.

## Step 5: Re-query what you built

The run record says what `dizzy` created. To ask the cloud what is actually
there right now:

```console
$ dizzy neutron status --run run-a1b2c3d4.json
```

Each resource is looked up again and its current status printed. This is the
same command you would use days later to check whether a topology is still
intact.

## Step 6: Read the timings

The run record also carries every measurement taken during the run. Render them:

```console
$ dizzy neutron report --run run-a1b2c3d4.json
```

You get a table with one row per resource kind: how many creates were attempted,
how many succeeded, and the latency distribution — min, mean, median, p90, p95,
p99, max. Below it, an overall row and, if anything failed, a breakdown of the
errors by kind.

The same numbers are available as JSON, CSV, or a self-contained HTML report
with charts:

```console
$ dizzy neutron report --run run-a1b2c3d4.json --format html > report.html
```

Open `report.html` in a browser. Nothing is loaded from the network; the charts
are inline SVG, so the file is safe to archive next to the run record.

## Step 7: Remove everything

```console
$ dizzy neutron cleanup --run run-a1b2c3d4.json
```

Cleanup deletes in reverse dependency order and reports how many resources it
removed. It matches strictly on the run's tag, so it cannot touch anything else
in your project. Running it a second time is harmless — it finds nothing and
exits successfully.

Confirm the project is clean:

```console
$ dizzy neutron list-networks
```

The `dizzy-a1b2c3d4-` networks are gone.

## What you did

You expanded a scenario into a plan, built that plan on a real cloud, measured
it, and tore it down — the full `generate → apply → status/report → cleanup`
cycle that every `dizzy` command namespace shares. The `cinder` and `keystone`
namespaces work exactly the same way against their own scenarios.

## Where to go next

- Run the same cycle unattended for hours or days:
  [Monitor a cloud continuously](../how-to/monitor-a-cloud.md).
- Keep creating *and* deleting resources under sustained churn instead of
  building once: [Run a churn soak](../how-to/run-a-soak.md).
- Understand why the plan is a separate artifact from the run:
  [Scenario, plan, and run](../explanation/scenario-plan-run.md).
- Look up any command or flag: [CLI reference](../reference/cli.md).
