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
the cleanest of the eight: images are one kind with no dependency ordering, so a
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
the prefix, a discovered order names it as its `secret_ref`, or it carries no
metadata at all and its `creator_id` is the user of dizzy's token. Barbican
sets `creator_id` from the token that created the secret, and gives a
generated secret the creator of its order, so discovery never deletes a secret
that merely carries a run's name. Cleanup works in
dependency order: containers first, since they reference secrets, then each
discovered order's generated secret and only after it the orders, since
deleting an order leaves the secret it generated and the order is the one
handle on an unstamped one that does not depend on who runs the cleanup, then
the remaining secrets, stored and
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
`PENDING_DELETE` or `DELETED`, and adopts a match. A member is looked up by its
address and port as well, because the Gardener persona of `mix` creates a
member under the same name for every instance of its worker, each at that
instance's address, and the orphan of an earlier instance must not stand in
for the next one. Each logical name is created
once per apply, so name and run tag identify the instance without a create
token. `octavia chaos` creates a name again only once the delete of its
previous instance has finished. When that delete succeeded, Octavia has removed
the instance by then, so the churn needs no create token either. A load
balancer whose delete Octavia refused with HTTP 400, because a child whose own
delete failed is still attached, stays next to its successor of the same name
until a later delete or the teardown removes it; a lookup that finds both
adopts the first and logs the other.

**c5c3 labels its orders, and the operator owns everything else.** A c5c3 run
creates no OpenStack resource itself. It creates custom resources of
`c5c3.io/v1alpha1` in one Kubernetes namespace, and the c5c3-operator creates
the Keystone user, project, role assignment, catalog entry and application
credential behind them. Every order carries two Kubernetes labels,
`dizzy/run=<id>` and `dizzy/type=<kind>`, the counterpart of the tag pair of
the other services. Discovery is a server-side label selector on
`dizzy/run=<id>`, one listing per kind in that namespace, and the run record
identifies an order as `<namespace>/<name>`. The Secrets the operator delivers
beside the orders carry no dizzy label. The operator writes each with an
owner reference to its order, the garbage collector removes it with the
order, and dizzy touches no Secret that is not named after one of its orders.
Everything else the operator creates lives in the ControlPlane's
namespace, where dizzy has no rights, and goes with the operator's teardown of
each order. Cleanup deletes the orders kind by kind in the order the operator's
holds require, application credentials, role assignments, catalog entries,
projects and then users, and waits until each kind is gone before the next,
because the operator holds a user, a project or a role assignment that another
order still names.

The seven OpenStack services converge on the same deterministic naming: a
resource with logical name `net-0001` in run `a1b2c3d4` is called
`dizzy-a1b2c3d4-net-0001` on the cloud. That is what makes it findable in
Horizon, in `openstack network list`, and by prefix scan.

A c5c3 order is named `dizzyorder-<runid>-<logical>` instead, e.g.
`dizzyorder-a1b2c3d4-user`. The operator names the Keystone user after its
order, and `keystone monitor --reclaim-orphans` deletes every Keystone user
whose name starts with `dizzy-`, whatever run it belongs to. With the
`dizzy-` prefix, a Keystone monitor against the same Keystone would delete the
Keystone users behind running c5c3 orders. The different prefix keeps that
sweep away from them, and the label, not the name, is what c5c3 discovery
reads.

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

Three resource types that cannot be discovered by their own identity are
reclaimed from the **run record's created list**, by id:

- **Neutron address scopes.** Some Neutron releases return 404 when you try to
  tag one. Tagging them is therefore best-effort: a failure is logged, tolerated,
  and left out of the metrics.
- **Keystone role assignments.** A grant is a relation, not an object with a
  name. It has nothing to stamp.
- **Keystone implied-role rules.** A rule is a relation like a grant, with no
  name to stamp. `cleanup --run` deletes it by id from the record. Without a
  record it goes with its roles, since Keystone deletes a role's implied-role
  rules when it deletes the role.

Keystone application credentials and trusts are reclaimed through their users,
not by name or from the record. Keystone deletes a user's application
credentials, and every trust the user is trustor or trustee of, when it
deletes the user, and cleanup deletes the run's users. A credential carries the
name `dizzy-<runid>-appcred-NNNN`, but Keystone keeps it under its user; a trust
has no name at all. The record lists a credential under the id
`<userID>:<credentialID>` and a trust under the trust's id, and cleanup passes
over both entries, so its `deleted <n> resource(s)` count leaves them out. A
bare `--run-id` reaches both, since it reaches the users.

Barbican secret ACLs and container consumers are reclaimed through their
secret or container, not by name or from the record. Barbican deletes a
secret's ACL when it deletes the secret and a container's consumers when it
deletes the container, and cleanup deletes the run's secrets and containers.
An ACL has no name or id of its own. A consumer carries the name
`dizzy-<runid>-con-NNNN`, but Barbican keeps it under its container. The
record lists an ACL under its secret's id and a consumer under the id
`<containerID>:dizzy-<runid>-con-NNNN`, and cleanup passes over both entries,
so its `deleted <n> resource(s)` count leaves them out. A bare `--run-id`
reaches both, since it reaches the secrets and containers.

Barbican adds secrets that never got their metadata, because the stamp is a
second call after the secret exists. Discovery never takes a secret on its name
alone: it confirms one by its `dizzy_run` metadata, as the `secret_ref` of an
order discovered by the run's name prefix, or, when the secret carries no
metadata at all, by its `creator_id`, which must be the user of dizzy's own
token. A bare `--run-id` therefore reaches every container and order by that
prefix, every secret that carries `dizzy_run`, every secret an order of the run
generated, stamped or not, and every secret of the run without metadata that
the token's user created. The last covers three secrets that no stamp names;
the record lists only the first of them:

- **A stored secret whose stamp failed.** The store succeeded and the metadata
  write did not. The run record lists it too, so `cleanup --run` deletes it by
  id whoever runs the cleanup.
- **The secret of a store dizzy gave up on.** When one operation of an apply
  stage fails, the stage cancels the stores still in flight, and Barbican
  completes some of them. A lost answer, or a 5xx sent after the commit, leaves
  the same secret, and the retry stores a second one under the same name. The
  first carries neither metadata nor a place in the run record. The store
  logs `create failed; a secret with this name may be orphaned in the cloud`
  with that name.
- **The secret of an order deleted while PENDING.** A worker that was already
  generating the key when the order was deleted still keeps the secret, and no
  order names it any more.

The secret of a late order, an order whose wait ended before Barbican had
generated its key, at the `--timeout` deadline or because the run stopped, and
that turned ACTIVE afterwards, is found through its order while the order
exists: dizzy never stamped or recorded that secret, and cleanup deletes it
through its order.

Two cases stay out of reach:

- **Cleanup as another user.** A cleanup whose token names another user than
  the one that stored the secret, or names no user at all, cannot confirm a
  secret without metadata, and leaves it. It logs a warning that names the
  secret, or, without a user, one that counts such secrets. `cleanup --run`
  still deletes a stamp-failed secret by id from the record.
- **A create Barbican commits after the listing.** A canceled store or order
  that Barbican completes after the cleanup listed the secrets is not in that
  listing. The next sweep of `barbican monitor` deletes it; after an
  interrupted `barbican apply`, a second `barbican cleanup --run-id <id>` does.

The practical consequence is a real asymmetry in the `cleanup` command:

- `cleanup --run <path>` uses the record, and reaches everything the record
  lists.
- `cleanup --run-id <id>` has no record. It reclaims everything discoverable by
  tag, metadata, prefix, or a Barbican secret's creator — which is most things —
  but it cannot remove address scopes, role assignments, or, run as another
  user, a stored Barbican secret whose stamp failed.

This is why `--run-id` exists at all: it is the recovery path for a run whose
record was lost, and it is honest about being incomplete.

For c5c3 the two reach the same orders, since every order carries the run
label. `c5c3 cleanup --run-id` needs `--namespace`, which `--run` reads from
the record's first entry.

## Cleanup is idempotent and ordered

Deletion runs in reverse dependency order — snapshots before their volumes, ports
before their networks, role assignments before their users. A 404 counts as
success, so running `cleanup` twice is harmless, and a partially-completed
teardown can simply be rerun.

After a `chaos` run's teardown, a **leak check** lists any resource still carrying
the run's tag. It is the assertion that teardown actually finished.

The Gardener persona of a `mix` run with `octavia` under `services` adds one
stage in front of its Nova teardown. It deletes the persona's load balancers,
found by the run tag and in the record, with cascade, and waits until each is
gone. The workers, server groups and cluster networks go after it, because a
load balancer's VIP port sits on its cluster network and the Nova teardown
leaves a port that Octavia owns alone, so the network delete would fail. The
lane does not reuse `octavia cleanup` for this stage. That cleanup also deletes
every subnet and network carrying the run tag, and in the Gardener lane those
are the Nova cluster networks, which carry the same `dizzy:run` tag and belong
to the Nova teardown that follows. The lane runs only the load-balancer stage,
then the Nova teardown even when that stage failed, and reports the errors of
both. `mix cleanup` takes the same order.

A c5c3 cleanup waits after every kind until its orders are gone, because the
operator holds an order's finalizer until K-ORC and the External Secrets
Operator have cleared Keystone and OpenBao. Once a user or application
credential order is gone, cleanup waits for its Secret to disappear and sends
one login with the credential, which must fail, so a cleanup that succeeds
has also shown that the run's credentials stop working. An order whose
finalizer the operator never releases stays, and every later cleanup reports
it by name; see
[Release a stranded finalizer](../how-to/test-c5c3-orders.md#release-a-stranded-finalizer).

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

Neutron, Cinder, Nova, Glance, Barbican, and Octavia sweep unconditionally — all
six are project-scoped. Keystone does not: because an admin token lists **cloud-wide** rather than
within one project, a Keystone sweep would
reach across the entire cloud, deleting a concurrent run's in-flight users, roles,
and whole domains. It is therefore opt-in via `--reclaim-orphans`, off by
default, and safe only when no other `dizzy` process targets that cloud at all.

Left off, a Keystone monitor still cleans up after each of its own iterations. It
just does not adopt orphans it did not create.

Barbican has no type tag, so its sweep widens the name prefix to `dizzy-`. It
takes containers and orders on that prefix, each secret a swept order names as
its `secret_ref`, any other secret once its `dizzy_type=secret` metadata
confirms it, and a secret without any metadata once its `creator_id` is the
user of the monitor's token. A secret whose stamp failed, or whose store the
iteration gave up on, is therefore swept by the next iteration when the
monitor keeps running as the same user; its own iteration's cleanup usually
deletes it first. A monitor started as another user leaves such a secret and
logs a warning naming it.

Octavia's sweep filters on `dizzy:type=<kind>` and on the token's project.
Octavia lists every project's load balancers for an administrator, and Neutron
every project's networks, so a sweep on the type tag alone would reach other
projects' runs. It therefore sends the project id of the auth result as
`project_id` with each listing and checks each answer's project again, and
`octavia monitor` refuses to start when the auth result names no project. It
deletes the load balancers with cascade, waits until each is gone, then
deletes the subnets and the networks. Neutron and Nova runs tag their networks
and subnets with the same `dizzy:type` values, so the sweep takes their
leftovers in the project too, as the Nova sweep takes Neutron's. The monitor
runs the quota pre-check of `octavia apply` once at startup and again in every
iteration. An amphora provider needs `--timeout 5m`, because an amphora build
outlasts the default of 60s that bounds both the wait for a new load balancer
and the sweep's wait until a deleted one is gone.

c5c3 sweeps unconditionally too, scoped to one Kubernetes namespace instead
of a project. Its sweep lists the orders by the `dizzy/type` label, so it
takes every dizzy order in the namespace whatever its run, and runs the same
steps as `c5c3 cleanup` over them, credential checks included. The process
needs rights in that namespace only, so the sweep cannot reach further, but
within it the rule is the same as for a project: run one dizzy process per
Kubernetes namespace. The `dizzyorder-` prefix keeps the Keystone sweep of
`keystone monitor --reclaim-orphans` away from the users behind the orders,
and the c5c3 sweep in turn never touches a Keystone user directly.

## What cleanup will never do

It will never delete a resource that does not carry the run's stamp. Not on
`cleanup`, not on an interrupted `apply`, not on a `chaos` teardown. The tag,
metadata, and prefix filters are the whole authorization model, and they are
applied server-side where the API allows it. Barbican secrets are the
exception: discovery takes two kinds of secret without a stamp. One is the
`secret_ref` of an order the run's name prefix discovered. The other is a
secret that carries the name prefix, no metadata at all, and the user of
dizzy's token as its `creator_id`. Barbican sets that field from the token
that created the secret, so only a secret the same user created under a
`dizzy-` name matches it. A c5c3 cleanup deletes only orders that carry the
run's label or are in its record, reads only the Secrets named after them, and
sends nothing to the ControlPlane's namespace.

The `monitor` sweep is the single place where the filter widens from "this run"
to "any dizzy run", and it is documented above precisely because it is the one
place the guarantee changes shape.
