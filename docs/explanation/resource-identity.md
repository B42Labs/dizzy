# Resource identity and cleanup

A tool that creates hundreds of resources in someone's project has one
obligation above all others: it must be able to say, with certainty, which
resources are its own — and it must never delete anything else.

`dizzy` gives every resource it creates a **run identity**: the run id, stamped
onto the resource at creation. Cleanup operates strictly on that stamp.

The stamp takes a different form in each service, because OpenStack does not
offer one mechanism that works everywhere. That inconsistency is not an
implementation detail you can ignore; it determines what cleanup can reach when
the run record is lost.

## Three mechanisms

**Neutron has a real tag API.** Every resource is created and then tagged
`dizzy:run=<id>` and `dizzy:type=<kind>`. Cleanup lists by tag, server-side, and
deletes what comes back.

**Cinder has no tag API, but volumes and snapshots carry metadata.** So the same
two keys live there: `dizzy:run=<id>` and `dizzy:type=<kind>`. Discovery is a
metadata filter — the metadata analog of Neutron's tags.

**Keystone has neither, except for projects.** Only projects support tags. So
identity lives primarily in the deterministic **name prefix**
`dizzy-<runid>-<logical>`, which is a reliable handle because Keystone names are
unique within their scope. Projects are additionally tagged, so they at least get
a server-side filter.

All three converge on the same deterministic naming: a resource with logical name
`net-0001` in run `a1b2c3d4` is called `dizzy-a1b2c3d4-net-0001` on the cloud.
That is what makes it findable in Horizon, in `openstack network list`, and by
prefix scan.

## Where the stamp doesn't reach

Two resource types cannot be discovered by their own identity, and both are
reclaimed from the **run record's created list**, by id:

- **Neutron address scopes.** Some Neutron releases return 404 when you try to
  tag one. Tagging them is therefore best-effort: a failure is logged, tolerated,
  and left out of the metrics.
- **Keystone role assignments.** A grant is a relation, not an object with a
  name. It has nothing to stamp.

The practical consequence is a real asymmetry in the `cleanup` command:

- `cleanup --run <path>` uses the record, and reaches everything.
- `cleanup --run-id <id>` has no record. It reclaims everything discoverable by
  tag, metadata, or prefix — which is most things — but it cannot remove address
  scopes or role assignments.

This is why `--run-id` exists at all: it is the recovery path for a run whose
record was lost, and it is honest about being incomplete.

## Cleanup is idempotent and ordered

Deletion runs in reverse dependency order — snapshots before their volumes, ports
before their networks, role assignments before their users. A 404 counts as
success, so running `cleanup` twice is harmless, and a partially-completed
teardown can simply be rerun.

After a `chaos` run's teardown, a **leak check** lists any resource still carrying
the run's tag. It is the assertion that teardown actually finished.

## The one dangerous exception: the monitor sweep

Everything above is scoped to *one run*. The `monitor` loop needs something
broader, and this is where you have to be careful.

A monitor iteration that crashes leaves resources behind. Left alone, a loop
running for days would accumulate the debris of every failed iteration. So before
each iteration, the loop sweeps.

The sweep cannot filter on `dizzy:run=<id>`, because the whole point is to catch
resources from *previous* run ids. It filters on `dizzy:type=<kind>` instead —
which matches **any** `dizzy`-created resource in the project, not just this
monitor's own.

That makes the sweep self-healing and makes concurrent runs unsafe:

> **Do not run `monitor` alongside another `dizzy` process in the same project.**
> The sweep would tear the other run's resources down mid-flight.

Neutron and Cinder sweep unconditionally. Keystone does not: because an admin
token lists **cloud-wide** rather than within one project, a Keystone sweep would
reach across the entire cloud, deleting a concurrent run's in-flight users, roles,
and whole domains. It is therefore opt-in via `--reclaim-orphans`, off by
default, and safe only when no other `dizzy` process targets that cloud at all.

Left off, a Keystone monitor still cleans up after each of its own iterations. It
just does not adopt orphans it did not create.

## What cleanup will never do

It will never delete a resource that does not carry the run's stamp. Not on
`cleanup`, not on an interrupted `apply`, not on a `chaos` teardown. The tag,
metadata, and prefix filters are the whole authorization model, and they are
applied server-side where the API allows it.

The `monitor` sweep is the single place where the filter widens from "this run"
to "any dizzy run", and it is documented above precisely because it is the one
place the guarantee changes shape.
