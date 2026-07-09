# Keystone's privilege model

Neutron and Cinder run entirely inside one ordinary project. Give `dizzy` a
`clouds.yaml` entry with a member role and it works.

Keystone cannot. Its operations are policy-gated, and the policy differs by
operation: creating a domain is admin-only, creating a user is not. A tool that
just started calling APIs would discover this as a wall of 403s halfway through a
run, having already created half a topology.

So `keystone apply`, `chaos`, and `monitor` each run a **read-only privilege
pre-check** before the first write, classify the caller, and fail fast if the
credentials cannot do what the plan needs. Structurally this occupies the slot
that the quota pre-check occupies for Neutron and Cinder — and it *replaces* it,
because Keystone has no default per-resource quotas to check.

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
its own roles, and removes both at teardown.

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

## Why token issue is an operation

The six Keystone operations are: create domain, create user, create project,
create role, assign role, and **issue token**.

The last is different in kind. It authenticates *as a created user* and obtains a
project-scoped token — the tool's first operation that acts as a principal it
created rather than as the operator. That makes it an end-to-end consistency
check as much as a latency measurement: it can only succeed if the whole
create-user → create-project → assign-role chain actually took effect in the
identity backend. A failed issue is recorded as a **failed operation**, not
skipped.

Keystone's creates and deletes are synchronous — the resource is usable when the
call returns — so there is no status polling and no time-to-ready stage. Token
issue is the one exception: its latency is recorded on `time_to_ready` with
`kind=token`, because there is nothing else meaningful to record there.

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

## Teardown under a shared domain

Domain-manager mode makes teardown genuinely delicate. The run lives inside a
pre-existing domain and uses pre-existing roles, neither of which it may delete.

Teardown therefore runs in reverse dependency order and touches only resources
carrying the run's name prefix: tokens need no teardown (they expire); role
assignments are unassigned; users deleted; projects deleted; roles deleted **only
in admin mode**; domains disabled and then deleted, again **only in admin mode**.

The disable-before-delete is a Keystone requirement — it refuses to delete an
enabled domain — which is why an `update` operation appears in a Keystone run's
metrics at all.

See [Resource identity and cleanup](resource-identity.md) for how the name prefix
bounds what teardown can touch.
