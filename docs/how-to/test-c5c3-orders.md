# Test c5c3-operator orders

`dizzy c5c3` orders Keystone resources from Cobalt Core's c5c3-operator as
Kubernetes custom resources, waits until the operator reports each one
`Ready`, and logs in to Keystone with every credential the operator delivers.
It writes through the Kubernetes API only. Every request it sends to Keystone
is a token issue with a delivered credential, so it needs no OpenStack account
of its own.

This guide sets up the Kubernetes namespace a run orders into, grants the
process its rights there, and runs it in a pod or from a kubeconfig. For every
flag see [The `c5c3` namespace](../reference/cli.md#the-c5c3-namespace), and
for every scenario key [C5C3](../reference/scenario-schema.md#c5c3).

## What a run checks

A run of `scenarios/c5c3/small.yaml` places five orders in the namespace
`dizzy-orders`, each referencing the ControlPlane `controlplane` in
`openstack`:

| Order | Kind | What dizzy checks once it is `Ready` |
|---|---|---|
| `user` | `KeystoneUser` | The delivered `clouds.yaml` issues an unscoped token |
| `project` | `KeystoneProject` | Its project scopes the tokens below |
| `role` | `KeystoneRoleAssignment` | A token of the user scoped to the project carries the role `member` |
| `catalog` | `KeystoneCatalogEntry` | That token's catalog lists the service type `dizzy-probe` with the public URL `https://dizzy-probe.invalid/v1` |
| `appcred` | `KeystoneApplicationCredential` | The delivered credential issues a token scoped to the project |

Before the role exists, the run orders an application credential that the
operator must refuse with `NoRoleOnProject`, and a role assignment for the role
`dizzy-refused`, which it must refuse with `RoleNotAllowed`. With the Secret
repair check on, the run edits and then deletes the user's and the
credential's Secret and waits until the operator restores each. It then
rotates the password, logs in with the new one and checks that the old one
fails. dizzy sends no login with the user's Secret while the password rotates:
upstream documents that the old password stops working the moment Keystone
applies the new one, before the operator rewrites the Secret. Last it rotates
the application credential while it logs in with the delivered one every 5
seconds, logs in with the superseded credential while its grace period runs,
and checks that it fails once the operator has deleted it. Cleanup deletes the
orders and checks that each delivered credential stops working.

A run of the bundled profile lasts at least its 2 minute grace period. It
sends about ten logins, plus one every 5 seconds while the application
credential rotates. Keystone can lock an account after repeated failed
logins. Each run creates a new user and sends it two password logins that must
fail, one after the rotation and one after cleanup. After the fifth password
login that Keystone rejects in one run, dizzy returns an error instead of
sending another. A `monitor` iteration's sweep counts its own rejected logins,
those it sends after deleting leftover users of other runs, apart from the
iteration's run.

## Prepare the namespace

The namespace and its assignment are the consent of the ControlPlane's owner,
and dizzy creates neither. The owner runs the two commands of Steps 1 and 2 of
the upstream guide
[Order Resources from a ControlPlane](https://github.com/C5C3/cobaltcore/blob/9cf2308e597ee3f3f1f99dd48ec1107216812834/docs/guides/order-resources-from-a-controlplane.md),
with the profile's namespace `dizzy-orders` in place of the guide's
`workflow`:

```sh
kubectl create namespace dizzy-orders
```

```sh
kubectl patch controlplane controlplane -n openstack --type json \
  -p '[{"op":"add","path":"/spec/namespaceAssignments","value":[{"namespace":"dizzy-orders"}]}]'
```

That patch creates the list on a ControlPlane that assigns no namespace yet.
On one that already assigns namespaces, the guide appends an entry instead.
Both are JSON patches, because the list is atomic and a merge patch would
replace every entry in it:

```sh
kubectl patch controlplane controlplane -n openstack --type json \
  -p '[{"op":"add","path":"/spec/namespaceAssignments/-","value":{"namespace":"dizzy-orders"}}]'
```

The entry admits no role and no catalog entry yet, and a run orders both. Add
the scenario's `role` to the entry's `allowedRoles` and set
`allowCatalogEntries`, with the `test` operation the guide uses to guard the
entry's index:

```sh
i=$(kubectl get controlplane controlplane -n openstack -o json \
  | jq '[.spec.namespaceAssignments[] | .namespace == "dizzy-orders" and .targetClusterRef == null] | index(true)')
kubectl patch controlplane controlplane -n openstack --type json -p "[
  {\"op\":\"test\",\"path\":\"/spec/namespaceAssignments/${i}/namespace\",\"value\":\"dizzy-orders\"},
  {\"op\":\"add\",\"path\":\"/spec/namespaceAssignments/${i}/allowedRoles\",\"value\":[\"member\"]},
  {\"op\":\"add\",\"path\":\"/spec/namespaceAssignments/${i}/allowCatalogEntries\",\"value\":true}]"
```

Keep the scenario's `refused_role`, `dizzy-refused` by default, off the list:
dizzy expects the operator to refuse it. No role of that name exists in
Keystone, so a ControlPlane that admits it by mistake grants nothing.

A missing piece shows as the reason the order reports, and the run fails with
error kind `refused`: `NamespaceNotAssigned` without the entry,
`RoleNotAllowed` on the `role` order without `member` on the list, and
`CatalogNotAllowed` on the `catalog` order without `allowCatalogEntries`. This
guide assigns a namespace on the cluster the ControlPlane runs on. Orders on a
registered target cluster are not covered.

## Grant the rights

The process needs rights in the one namespace and nowhere else. It needs no
`watch`, nothing cluster-wide, and nothing in the ControlPlane's namespace.

| Resources | Verbs | Used for |
|---|---|---|
| `keystoneusers`, `keystoneprojects`, `keystoneroleassignments`, `keystonecatalogentries`, `keystoneapplicationcredentials` of `c5c3.io` | `create`, `get`, `list`, `patch`, `delete` | Creating the orders, polling them, finding a run's orders by label, raising the password and credential generations, deleting them |
| `secrets` | `get` | Reading the delivered credentials |
| `secrets` | `patch`, `delete` | The Secret repair check, unless `checks.secret_repair` is `false` |

With the repair check on, the default, bind this Role to the identity dizzy
runs as. The example binds it to a service account `dizzy` in the same
namespace:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: dizzy
  namespace: dizzy-orders
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: dizzy-c5c3
  namespace: dizzy-orders
rules:
  - apiGroups: ["c5c3.io"]
    resources:
      - keystoneusers
      - keystoneprojects
      - keystoneroleassignments
      - keystonecatalogentries
      - keystoneapplicationcredentials
    verbs: ["create", "get", "list", "patch", "delete"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: dizzy-c5c3
  namespace: dizzy-orders
subjects:
  - kind: ServiceAccount
    name: dizzy
    namespace: dizzy-orders
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: dizzy-c5c3
```

A deployment that grants only `get` on Secrets turns the repair check off with
`checks.secret_repair: false` in the scenario, or with
`--set checks.secret_repair=false` on the command line, and binds this Role
instead:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: dizzy-c5c3
  namespace: dizzy-orders
rules:
  - apiGroups: ["c5c3.io"]
    resources:
      - keystoneusers
      - keystoneprojects
      - keystoneroleassignments
      - keystonecatalogentries
      - keystoneapplicationcredentials
    verbs: ["create", "get", "list", "patch", "delete"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get"]
```

For a person or a CI job that runs from a kubeconfig, bind the same Role to
its user or group instead of the service account.

Before it creates anything, `apply` and every `monitor` iteration list each of
the five resources in the namespace once. When the cluster does not serve one,
because the operator's CRDs are missing, or refuses the listing with 403, the
run stops before it creates anything, with an error that names the resource,
and for a 403 also the namespace. That check does not cover Secrets: with the
repair check on and only `get` on Secrets, the run fails at the repair stage
with an error that names `checks.secret_repair`.

## Run it in a pod

Inside the cluster dizzy needs no kubeconfig: when it finds none, it uses the
pod's service account. On the cluster Keystone runs on, the delivered
documents name the in-cluster Keystone Service, which a pod there resolves, so
leave `--os-cloud` and `$OS_CLOUD` unset and dizzy logs in with each document
as delivered.

```yaml
spec:
  serviceAccountName: dizzy
  containers:
    - name: dizzy
      image: ghcr.io/b42labs/dizzy:<tag>
      args:
        - "c5c3"
        - "monitor"
        - "--scenario"
        - "/usr/share/dizzy/scenarios/c5c3/small.yaml"
        - "--listen-address"
        - ":9464"
      ports:
        - name: metrics
          containerPort: 9464
```

The image carries the bundled profile under `/usr/share/dizzy/scenarios`. Add
`--set namespace=<name>` to order into another namespace, together with a
Role and an assignment for that namespace. For the scrape and the liveness
probe, see
[Serve Prometheus metrics and a health check](serve-prometheus-metrics.md).
dizzy records every poll of a wait for the operator as a `get`, so `/healthz`
counts a wait of several minutes as progress.

## Run it from a kubeconfig

From a workstation or a CI host, `--kubeconfig` names the kubeconfig and
`--kube-context` the context:

```console
$ dizzy c5c3 apply --scenario scenarios/c5c3/small.yaml --kubeconfig <path> --kube-context <context>
```

Without `--kubeconfig` dizzy reads `$KUBECONFIG` or `~/.kube/config`, and
without `--kube-context` it uses the kubeconfig's current context, as
`kubectl` does.

Outside the cluster the delivered `auth_url`, the in-cluster Service
`http://<controlplane>-keystone.<namespace>.svc:5000/v3`, usually does not
resolve. `--os-cloud` names a `clouds.yaml` entry whose `auth_url` and TLS
settings (`verify`, `cacert`, `cert` and `key`) replace those of every
document the operator delivers. dizzy authenticates nothing with the entry
and leaves its credentials unused, so the entry needs none:

```yaml
clouds:
  c5c3-public:
    auth:
      auth_url: https://keystone.example.test/v3
    cacert: /etc/ssl/certs/c5c3-keystone.pem
```

```console
$ dizzy c5c3 apply --scenario scenarios/c5c3/small.yaml --os-cloud c5c3-public
```

dizzy reads the entry once at startup, so a missing `clouds.yaml` fails before
the first order, and so does an entry without `auth_url`, with
`clouds.yaml entry "c5c3-public" has no auth_url`. `$OS_CLOUD` selects an
entry the same way. With neither set, dizzy reads no `clouds.yaml` and logs in
with each document as delivered, except for three environment variables:
gophercloud applies `OS_CACERT`, `OS_CERT` and `OS_KEY` to every login with a
delivered document, ahead of the TLS settings of the document or of the
entry. A delivered document names no client certificate, so `OS_CERT` without
`OS_KEY`, or `OS_KEY` without `OS_CERT`, fails every login. Unset `$OS_CLOUD`,
and these three, in a shell that exports them for other dizzy runs when the
delivered address and TLS settings are the ones to test.

A finished `apply` keeps its orders and writes `run-<id>.json`. `status` shows
each order as `Ready`, the reason the operator reports, `Pending` or `gone`,
and `cleanup` deletes them:

```console
$ dizzy c5c3 status --run run-<id>.json
$ dizzy c5c3 cleanup --run run-<id>.json --os-cloud c5c3-public
```

`status` sends no Keystone request and needs no `--os-cloud`. `cleanup` sends
a login with each deleted credential that must fail, so it needs the same
`--os-cloud` as the run. It takes the namespace from the record; with
`--run-id` it needs `--namespace`.

## Read a mismatch

A credential that Keystone rejects while its order reads `Ready` is the
failure this namespace exists to catch. It shows as

```promql
dizzy_operation_errors_total{service="c5c3", operation="authenticate", error_kind="mismatch"}
```

and as error kind `mismatch` in the run record. dizzy calls a rejected login a
mismatch only when the operator was not changing the credential at the time.
A rejection is a 401, and for an application credential also a 404, which is
how Keystone answers a login with a credential id it does not know. dizzy
reads the order and its Secret before the login and again after the
rejection. For a user it is a mismatch when the order was ready both times
and the Secret's `resourceVersion` did not change. For an application
credential it is a mismatch when `status.credentialID` or
`status.previousCredentialID` named the credential dizzy sent both times. Any
other rejection raced a change, or could not be checked because a read around
it failed, and records as `http_401` or `http_404`.

The same series also counts an application credential whose token is scoped
to another project than the one the `project` order reports, and a delivered
document gophercloud cannot read.

An outage looks different. A 5xx or a connection error is retried, a timeout
ends the call with error kind `timeout`, and none of them becomes a mismatch,
so a Keystone outage shows on the same operation as `http_<status>`,
`timeout` or `other`. This query puts the two side by side:

```promql
sum by (error_kind) (rate(dizzy_operation_errors_total{service="c5c3", operation="authenticate"}[15m]))
```

`mismatch` marks the other comparisons between what the operator delivered and
what Keystone holds as well: a scoped token without the role or a catalog
without the entry (operation `validate`), a Secret that still carries the old
password or credential after a rotation, or is still missing when the wait
for the rotation ends while its order is ready (`rotate`), a ready order whose
Secret is missing, lacks one of its keys or does not hold exactly one
`clouds.yaml` entry (`get` of kind `secret`), a Secret still present after its
order is gone (`finalize` of kind `secret`), and an order that turned ready,
or reported another reason, where a refusal was expected (`refuse`).
`accepted` marks a login that had to fail and got a token (`reject`): the old
password after the rotation, the superseded credential after its grace
period, or a credential after cleanup deleted its order. See
[Metrics](../reference/metrics.md#attribute-values) for every value.

## Release a stranded finalizer

Upstream documents one way an order can stay forever. Deleting an application
credential order while its user's password rotates strands the order's
finalizer: its teardown does not rewrite the password document K-ORC
authenticates its deletes with, so K-ORC cannot delete the credentials, and
the finalizer holds until someone removes the order's K-ORC
ApplicationCredentials by hand. An `apply` interrupted while the password
rotates leaves exactly that state.

Cleanup therefore waits, before its first delete, until the password
generation in each user order's status has caught up with its spec, or the
order is being deleted. It skips a user order whose status still reports
password generation 0: Keystone never provisioned that user, so no rotation
can be under way. The wait lasts at most `timeouts.ready`, 15 minutes for
`c5c3 cleanup`; when it passes that bound, cleanup logs a warning and goes on.
Cleanup reads the delivered Secrets only after this wait, so when the
rotation completes in time, the login it sends after the delete carries the
new password. When a credential order then stays present past
`timeouts.gone`, cleanup still sends the deletes of the remaining orders, so
they go once the hold lifts, skips their waits, and fails with an error that
names each order still present and what it reports, for example
`ReferencedByApplicationCredentials`.

dizzy has no rights in the ControlPlane's namespace, where the K-ORC objects
live, so it cannot release the order. The ControlPlane's owner finds the
order's K-ORC ApplicationCredentials by the labels the operator puts on them:

```sh
kubectl get applicationcredentials.openstack.k-orc.cloud -n openstack \
  -l c5c3.io/keystoneapplicationcredential-namespace=dizzy-orders,c5c3.io/keystoneapplicationcredential-name=<order name>
```

Until they are removed, every later `c5c3 cleanup` and every monitor sweep
reports the order by name. A monitor iteration whose sweep fails skips its
apply, so the monitor makes no progress until then.

## Run one dizzy process per namespace

The `c5c3 monitor` sweep lists every order in the namespace that carries the
`dizzy/type` label, whatever run created it, and deletes it. That is how the
next iteration reclaims a killed iteration's orders, and it would also tear
down the orders of a second dizzy process in the same namespace mid-run. Run
one dizzy process per Kubernetes namespace, `c5c3 apply` included, and give
each further process a namespace and an assignment of its own.

`keystone monitor --reclaim-orphans` against the same Keystone leaves the
ordered users alone: it deletes users whose names start with `dizzy-`, and
dizzy names its orders, and with them the users, `dizzyorder-<runid>-<logical>`.
See [Resource identity and cleanup](../explanation/resource-identity.md).
