# Keystone's privilege model

Neutron and Cinder run entirely inside one ordinary project. Give `dizzy` a
`clouds.yaml` entry with a member role and it works.

Keystone cannot. Its operations are policy-gated, and the policy differs by
operation: creating a domain is admin-only, creating a user is not. A tool that
just started calling APIs would discover this as a wall of 403s halfway through a
run, having already created half a topology.

So `keystone apply`, `chaos`, `monitor`, and `pressure` each run a **read-only
privilege pre-check** before the first write, classify the caller, and fail fast
if the credentials cannot do what the plan needs. Structurally this occupies the
slot that the quota pre-check occupies for Neutron and Cinder — and it
*replaces* it, because Keystone has no default per-resource quotas to check.

`pressure` accepts the admin tier only. Validating and revoking other users'
tokens and listing their role assignments are admin operations in Keystone's
default policy, and a tier that cannot run three of its seven operations would
measure something else. For `probe` and the propagation probe, the admin tier
also grants the cloud's `reader` role to the pool users on their projects
before it primes the pool. The plan's roles are the run's own, and a
scenario's implied-role rules link them only to each other, while Cinder's
`GET /v3/<project_id>/limits` and Glance's `GET /v2/images` ask for `reader`
on the token's project under their default policies. The requests to the
services themselves need no admin right: `probe` and the polls of the
propagation probe go out to Nova, Neutron, Cinder and Glance with the pool
users' own tokens, each a read that `reader` on the token's project allows,
and `bad_token` sends tokens the service has to refuse, for which a pool user
needs no role. The propagation probe's revocations, like those of `revoke`, go
to Keystone with the admin token.
`pressure` classifies the caller only with `--privilege auto`;
`--privilege admin` asserts the tier without the classification call. A
caller classified as a domain manager fails before any write, and
`--privilege domain-manager` fails before any API call:

```console
$ dizzy keystone pressure --scenario scenarios/keystone/small.yaml --privilege domain-manager
error: keystone pressure needs the admin tier: validating and revoking other users' tokens and listing their role assignments are admin operations; the caller resolved to tier "domain-manager"
```

`--domain` and `--roles` have no effect on `pressure`; a non-default value of
either logs a warning.

## Two tiers

| Tier | Detected by | Domains | Roles | Projects, users, assignments, tokens |
|---|---|---|---|---|
| **admin** | the `admin` role, at any scope | create | create | yes |
| **domain-manager** | the `manager` role on a domain-scoped token | no | no | yes, within that domain |
| *neither* | — | — | — | fail fast, with a message |

## One plan, two bindings

The tiers do not have separate scenarios or separate plans. They share the same
cloud-independent plan, and an **apply-time binder** reconciles it with what the
caller may actually do.

This is the same pattern Neutron uses for `--external-network` and Cinder for
`--volume-type`: the plan records intent, and a cloud-specific fact is bound to
it at apply time. Here the cloud-specific fact is *who you are*.

**In admin mode** the binder creates the plan's domains and roles, and everything
else hangs off them. The run is fully self-contained: it brings its own domains,
its own roles, and removes both at teardown. Right after the roles it also
creates the scenario's implied-role rules, which link only the run's own
roles, and the teardown removes them too.

**In domain-manager mode** the binder creates no domains and no roles. It *binds*
every logical domain in the plan onto the single in-scope domain — the caller's,
or `--domain <name>` — and *reuses* roles that already exist, discovered at apply
time (`--roles`, defaulting to `member,reader`). Projects, users, assignments,
and tokens then run inside that domain exactly as in admin mode.

`admin` is never among the reused roles. A domain manager may not grant it.

It follows that some scenarios are admin-only. A plan asking for
`resources.domains > 1` cannot be bound onto one in-scope domain, and validation
rejects it in domain-manager mode. `resources.roles` is simply ignored there —
roles are reused, not created. This is why the shipped `small` Keystone profile
declares exactly one domain, and `medium` and `large` do not.
`resources.implied_roles` is ignored in domain-manager mode as well, since the
rules link roles the run creates and this mode creates none, and the run logs
the warning
`implied-role rules are ignored in domain-manager mode, which reuses existing roles`
with `rules=<n>`.

## Why token issue is an operation

The Keystone operations are: create domain, create user, create project,
create role, create implied-role rule, assign role, **issue token**, create
application credential, authenticate with an application credential, create
trust, and authenticate with a trust.

Token issue differs in kind. It authenticates *as a created user* and obtains a
project-scoped token — the tool's first operation that acts as a principal it
created rather than as the operator. That makes it an end-to-end consistency
check as much as a latency measurement: it can only succeed if the whole
create-user → create-project → assign-role chain actually took effect in the
identity backend. A failed issue is recorded as a **failed operation**, not
skipped.

Keystone's creates and deletes are synchronous — the resource is usable when the
call returns — so there is no status polling and no time-to-ready stage. Token
issue and the two authentications are the exceptions: their latencies are
recorded on `time_to_ready` with `kind=token`, `kind=application_credential`
and `kind=trust`, because there is nothing else meaningful to record there.

## Acting as a created user

The operator's token cannot create an application credential or a trust, in
either tier. Keystone's default policy lets only the owner create an
application credential and only the trustor create a trust. Deleting or
reading either is open only to an admin or the owner.

So dizzy acts as the user it created, in admin mode and domain-manager mode
alike. It obtains a project-scoped token of that user with the random
password dizzy generated for the user and holds in memory for the run, and
sends the create with that token. The token is scoped to the project of the
grant the credential or trust carries; for a trust it is the trustor's token.
A churn delete of either takes the same path, with a token of the owner or,
for a trust, the trustor.

`apply` and `monitor` follow each create with an authentication, the
read-back that proves the delegation works; `chaos` authenticates as a
mutation drawn with `token_ratio`. dizzy authenticates with the application
credential's id and the secret it generated for the credential, and with a
trust as its trustee, using the trustee's password and the trust as the
token's scope. A failed authentication fails `apply` and a `monitor`
iteration.

Acting as a user adds a token request to every create and delete of either
kind. Each application credential costs `apply` three requests: the owner
token, the create and the authentication. Each trust costs three as well, two
of them password authentications: the owner token as trustor and the
authentication as trustee. A churn delete of either costs two, the owner token
and the delete. Every owner token is recorded as a `token`/`create` sample
and a `kind=token` time-to-ready sample.

`keystone status` reads both kinds back with the operator's token. An admin
may read them. A domain manager may not read another user's credential or
trust, so Keystone answers 403, and in a domain manager's run `status` shows
each of them as `unknown` instead of failing.

## Failing open, failing fast

The pre-check is read-only, and its failure modes are chosen so that it never
becomes the thing that blocks a legitimate run.

With `--privilege auto` (the default) a classification failure is fatal: the tool
does not know what it may do, so it declines to guess.

With an explicit `--privilege admin` or `--privilege domain-manager`, a
classification failure is only a warning. The operator has asserted the tier, and
the executor's 403 fast-fail is the backstop. Likewise, discovery listings that a
domain manager may not be permitted to read — listing domains, roles, users —
**fail open**: they warn and return empty, mirroring the quota pre-check's
handling of a project that cannot read its own quota.

The principle in both cases is the same. A pre-check exists to give a good error
message early. It must not become a second, stricter authorization system that
refuses work the cloud would have allowed.

## A caveat about older Keystone

On **Keystone 2024.1 and older**, the domain-manager policy is not guaranteed to
be installed. A caller holding the `manager` role may still receive a 403 on the
manage calls, because the policy that grants a manager those rights simply is not
there.

Domain-manager mode is therefore best-effort on those releases, with the
executor's 403 fast-fail as the backstop. `--privilege admin` with a cloud admin
is the path that always works.

## In a combined run

`mix chaos` can run the Keystone churn as a background lane next to its
personas. The lane runs the same privilege pre-check with the same three tiers,
but takes its settings from the scenario's lane block instead of flags:
`lanes.keystone.privilege`, `lanes.keystone.domain` and `lanes.keystone.roles`
stand for `--privilege`, `--domain` and `--roles`, an empty privilege means
`auto`, and empty roles mean `member,reader`. The pre-check's own messages
still name the flags; in a combined run they mean the lane keys.

A failing pre-check ends the combined run before anything exists, with an
error that starts with `lane "keystone": `, because a lane the operator
switched on either runs or stops the run. The bundled mix profiles leave the
lane off and point it at Keystone's `small` profile, the only one a domain
manager can run, since it plans a single domain.

The scaffold is created later than in `keystone chaos`: only once every
persona and every lane of the run has passed its pre-checks, so a lane whose
pre-check fails after the Keystone lane's leaves no domain behind. In
domain-manager mode that step creates nothing and binds the in-scope domain
and the reused roles.

## Teardown under a shared domain

Domain-manager mode makes teardown genuinely delicate. The run lives inside a
pre-existing domain and uses pre-existing roles, neither of which it may delete.

Teardown therefore runs in reverse dependency order and touches only resources
carrying the run's name prefix: tokens need no teardown (they expire); role
assignments are unassigned; users deleted; projects deleted; implied-role rules
and then roles deleted **only in admin mode**; domains disabled and then
deleted, again **only in admin mode**.

An implied-role rule carries no name prefix, since it has no name. The teardown
deletes the rules the run record lists, and without a record they go with their
roles: Keystone deletes a role's implied-role rules when it deletes the role.

Application credentials and trusts get no delete of their own in either tier.
Keystone deletes a user's application credentials, and every trust the user is
trustor or trustee of, when it deletes the user, so both kinds go with the
run's users. The `deleted <n> resource(s)` count leaves them out, and the leak
check counts neither, since neither can outlive its users.

The disable-before-delete is a Keystone requirement — it refuses to delete an
enabled domain — which is why an `update` operation appears in a Keystone run's
metrics at all.

See [Resource identity and cleanup](resource-identity.md) for how the name prefix
bounds what teardown can touch.
