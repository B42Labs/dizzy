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

## The mechanisms

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

**Nova uses both, split by who provides the resource.** Servers and the data
volumes dizzy attaches carry the same two keys as **metadata** — the Cinder
mechanism — while the companion networks, subnets, and ports carry them as
Neutron **tags**. Nova's server-list API cannot filter on metadata server-side,
so server discovery lists the project's servers and filters client-side on the
metadata dizzy itself wrote. Servers and data volumes carry a third metadata
key, `dizzy:create`: a create token that tells one create request apart from
an earlier one of the same name, so dizzy can find and adopt a resource whose
create answer was lost (see
[The churn engine](churn-engine.md#a-lost-create-answer-is-adopted)). Discovery
and cleanup still filter on `dizzy:run` only. The churn network delete also
removes an untracked server whose Nova-created port holds the network, and it
checks the server's `dizzy:run` before it deletes the server, so it keeps the
guarantee of [What cleanup will never do](#what-cleanup-will-never-do).
A **boot-from-volume root volume** is Nova-created
with delete-on-termination and carries no dizzy identity of its own; it dies with
its server, so cleanup reaches it through the server delete. **Server groups**
carry neither metadata nor tags, so identity lives in the name prefix
`dizzy-<runid>-`, the Keystone handle. Only the `mix` Gardener persona creates
them, so only the `mix` teardown and `mix cleanup` look for them, after the
persona's other resources.

**Glance has first-class image tags.** Images carry the same two keys —
`dizzy:run=<id>` and `dizzy:type=image` — as Glance image tags, set atomically at
create via the create call rather than a follow-up tag request, and the image
list API filters on a tag server-side. Discovery is therefore a single tag query,
the cleanest of the seven: images are one kind with no dependency ordering, so a
bare `--run-id` reclaims everything the run created.

**Barbican layers a name prefix with secret metadata.** Barbican has no tags
and no server-side prefix filter, so identity starts with the Keystone handle,
the name prefix `dizzy-<runid>-`, and every listing is filtered client side.
Secrets and containers carry the prefixed name as their name. An order carries
it as `meta.name`, which Barbican also gives the secret the order generates and
returns in its order list. Only secrets carry metadata, so each secret gets the
two keys `dizzy_run=<id>` and `dizzy_type=secret` as Barbican secret metadata,
written in a second call: after the store for a stored secret, and once its
order is ACTIVE for a generated one. Barbican spells them with an underscore
where every other service writes `dizzy:run` and `dizzy:type`, because its
metadata keys must match `[A-Za-z0-9_-]`. Discovery takes containers and orders
on the prefix alone, and a secret only once its `dizzy_run` metadata confirms
the prefix or a discovered order names it as its `secret_ref`, so discovery
never deletes a secret that merely carries a run's name. Cleanup works in
dependency order: containers first, since they reference secrets, then each
discovered order's generated secret and only after it the orders, since
deleting an order leaves the secret it generated and the order is the only
handle on one that was never stamped, then the remaining secrets, stored and
generated alike. Barbican deletes synchronously, so cleanup waits for nothing.

**Octavia tags every object, and cleanup deletes through the cascade.** Load
balancers, listeners, pools, members, and health monitors carry the tag pair
`dizzy:run=<id>` and `dizzy:type=<kind>` as Octavia tags, set in the create
call itself, which needs Octavia API version 2.5 (Stein) or later. The VIP
networks and their subnets are Neutron resources and carry the same pair as
Neutron tags, as Nova's companion networks do. Discovery lists load balancers,
subnets, and networks by the run tag. Cleanup deletes each load balancer with
cascade, so Octavia removes its listeners, pools, members, and health monitors
with it; none of them is deleted on its own. `octavia chaos` deletes them one
at a time while it churns, and its teardown is this cleanup. Cleanup waits
until each load balancer is gone, then deletes the subnets and then the
networks. The VIP port
is Octavia's: Octavia creates it for the load balancer and removes it with the
load balancer, so it carries no dizzy identity, and cleanup never deletes a
port. A subnet that stays in use after its load balancers are gone holds a port
dizzy did not create, and cleanup reports it rather than deleting it. A create
whose answer was lost is adopted rather than posted again, because Octavia
refuses a second listener on a port, a second default pool on a listener, a
second member at an address and port, or a second monitor on a pool with HTTP
409, and a second load balancer would boot a second amphora. dizzy looks the
name up among the objects that carry the run tag, skipping any in
`PENDING_DELETE` or `DELETED`, and adopts a match. Each logical name is created
once per apply, so name and run tag identify the instance without a create
token. `octavia chaos` creates a name again only once the delete of its
previous instance has finished. When that delete succeeded, Octavia has removed
the instance by then, so the churn needs no create token either. A load
balancer whose delete Octavia refused with HTTP 400, because a child whose own
delete failed is still attached, stays next to its successor of the same name
until a later delete or the teardown removes it; a lookup that finds both
adopts the first and logs the other.

All seven converge on the same deterministic naming: a resource with logical name
`net-0001` in run `a1b2c3d4` is called `dizzy-a1b2c3d4-net-0001` on the cloud.
That is what makes it findable in Horizon, in `openstack network list`, and by
prefix scan.

A combined run (`mix chaos`) gives each persona an identity of its own: the run
id with the persona name appended, `<runID>-<persona>`. The CI persona's first
server in run `a1b2c3d4` is therefore `dizzy-a1b2c3d4-ci-srv-0001` and carries
`dizzy:run=a1b2c3d4-ci`. Each persona is torn down, leak-checked and cleaned
up by its own identity, because Neutron lists by tag across projects for an
administrator and a shared identity would let one persona's teardown reach
another persona's resources. A background lane of a combined run gets the
identity `<runID>-<lane>` the same way: the Glance lane's images in run
`a1b2c3d4` carry `dizzy:run=a1b2c3d4-glance`, and the Keystone lane's names
start with `dizzy-a1b2c3d4-keystone-`. See [Combined runs](combined-runs.md).

## Where the stamp doesn't reach

Three resource types cannot be discovered by their own identity, and all three
are reclaimed from the **run record's created list**, by id:

- **Neutron address scopes.** Some Neutron releases return 404 when you try to
  tag one. Tagging them is therefore best-effort: a failure is logged, tolerated,
  and left out of the metrics.
- **Keystone role assignments.** A grant is a relation, not an object with a
  name. It has nothing to stamp.
- **Keystone implied-role rules.** A rule is a relation like a grant, with no
  name to stamp. `cleanup --run` deletes it by id from the record. Without a
  record it goes with its roles, since Keystone deletes a role's implied-role
  rules when it deletes the role.

Barbican adds secrets that never got their metadata, because the stamp is a
second call after the secret exists. Discovery never takes a secret on its name
alone: it confirms one by its `dizzy_run` metadata, or as the `secret_ref` of
an order discovered by the run's name prefix. A bare `--run-id` therefore
reaches every container and order by that prefix, every secret that carries
`dizzy_run`, and every secret an order of the run generated, stamped or not.
That covers the secret of a late order, an order whose wait ended before
Barbican had generated its key, at the `--timeout` deadline or because the run
stopped, and that turned ACTIVE afterwards: dizzy never stamped or recorded
that secret, and cleanup deletes it through its order. Two stored secrets stay
out of that reach:

- **A stored Barbican secret whose stamp failed.** The store succeeded and the
  metadata write did not. The run record lists the secret, so `cleanup --run`
  deletes it by id, while a bare `--run-id` leaves it.
- **The unstamped duplicate of a retried store.** A store whose answer was
  lost after Barbican had kept the secret, for example a 5xx sent after the
  commit, leaves that secret behind, and the retry stores a second one under
  the same name. The first carries neither metadata nor a place in the run
  record, so no cleanup reaches it. The store logs
  `create failed; a secret with this name may be orphaned in the cloud` with
  that name, for an operator to find the secret and delete it by hand.

The practical consequence is a real asymmetry in the `cleanup` command:

- `cleanup --run <path>` uses the record, and reaches everything.
- `cleanup --run-id <id>` has no record. It reclaims everything discoverable by
  tag, metadata, or prefix — which is most things — but it cannot remove address
  scopes, role assignments, or a stored Barbican secret whose stamp failed.
- Neither reaches the unstamped duplicate of a retried Barbican store above,
  which the `create failed` warning names. It is the only secret both miss.

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

Neutron, Cinder, Nova, Glance, and Barbican sweep unconditionally — all five are
project-scoped. Keystone does not: because an admin token lists **cloud-wide** rather than
within one project, a Keystone sweep would
reach across the entire cloud, deleting a concurrent run's in-flight users, roles,
and whole domains. It is therefore opt-in via `--reclaim-orphans`, off by
default, and safe only when no other `dizzy` process targets that cloud at all.

Left off, a Keystone monitor still cleans up after each of its own iterations. It
just does not adopt orphans it did not create.

Barbican has no type tag, so its sweep widens the name prefix to `dizzy-`. It
takes containers and orders on that prefix, each secret a swept order names as
its `secret_ref`, and any other secret only once its `dizzy_type=secret`
metadata confirms it. A stored secret whose stamp failed is therefore never
swept. Its own iteration's cleanup deletes it through the created list, so
only an iteration whose cleanup does not finish leaves one behind: a cleanup
that stops at its first listing or delete error, a second signal, a killed
process. Without `--keep-run-records` nothing holds its id afterwards, and an
operator deletes it by hand; with it, `barbican cleanup --run` on that
iteration's record reaches it.

## What cleanup will never do

It will never delete a resource that does not carry the run's stamp. Not on
`cleanup`, not on an interrupted `apply`, not on a `chaos` teardown. The tag,
metadata, and prefix filters are the whole authorization model, and they are
applied server-side where the API allows it.

The `monitor` sweep is the single place where the filter widens from "this run"
to "any dizzy run", and it is documented above precisely because it is the one
place the guarantee changes shape.
