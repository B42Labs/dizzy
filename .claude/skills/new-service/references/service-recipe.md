# The dizzy new-service recipe

Every OpenStack service dizzy supports was added the same way. Four services
have landed on this pattern — Neutron, Cinder, Keystone, Nova — with Glance as
the fifth and newest complete example. Read `internal/glance/` before you write:
it is the current, working template for everything below.

The reference issues, in the tracker, are the format you must reproduce:

- **#30** — First Cinder support (fully specified, quota pre-check, metadata identity)
- **#39** — First Keystone support (fully specified, privilege pre-check, name-prefix identity)
- **#48** — First Nova support
- **#50** — First Glance support

Read at least #30 and #39 in full (`gh issue view 30 --repo <owner/repo>`,
`gh issue view 39 --repo <owner/repo>`). They are the two most complete
new-service plans and the model for the issue you produce.

---

## Full parity: the seven verbs

A new namespace is not done until `dizzy <service>` speaks the same verbs as the
existing ones. This is the promise the README makes and every issue must state:

```
dizzy <service> generate   # scenario -> deterministic plan JSON
dizzy <service> apply       # --dry-run and live; writes run-<id>.json
dizzy <service> chaos       # churn/soak within a scenario envelope
dizzy <service> monitor     # unattended sweep -> apply -> cleanup loop
dizzy <service> status      # re-query live state of a run
dizzy <service> report      # table/JSON/CSV/HTML from a run record
dizzy <service> cleanup     # delete a run's resources idempotently
```

The namespace is named after the **OpenStack project** (`nova`, `glance`,
`keystone`), never the API service name (`compute`, `image`, `identity`).

---

## The architecture checklist

Each item is a subsystem the issue's `## Proposed design` must cover. The path
anchors are real — verify each still exists before you cite it, and mark
anything the new service *adds* as "this issue adds", not as already present.

1. **CLI namespace** — a sibling of the existing `cmd/dizzy/<service>*.go`
   command files, wired into the root command and the global flags
   (`--os-cloud`, `--concurrency`, `--timeout`, `--seed`, `--otel`,
   `--log-level`). One file per verb, following the `glance_*.go` layout.

2. **Client constructor** — `internal/config/config.go` gains
   `New<Service>Client` (gophercloud v2 `openstack.New<Service>V<n>`), next to
   `NewNetworkClient`, `NewBlockStorageClient`, `NewIdentityClient`,
   `NewImageClient`. Name the microversion if the operations need one.

3. **Scenario schema** — `internal/<service>/scenario`: its **own** struct,
   `Validate`, and `--set` keys, parsed with `yaml.UnmarshalStrict` so a typo in
   any service's file still fails loudly. Fixed counts under `resources`, ranges
   and ratios under `distribution`, deterministic via `seed`, plus a `chaos:`
   block so `chaos` runs a profile with no extra flags.

4. **Built-in profiles** — `scenarios/<service>/{small,medium,large}.yaml`,
   embedded through `scenarios/scenarios.go`. Size `small` to fit the service's
   common default quotas so it runs anywhere. Give a sizing table in the issue.

5. **Plan model** — `internal/<service>/plan`: pure data, slices only,
   byte-identical JSON for the same scenario + seed, `Validate` resolving
   cross-references by logical name, `Summary` for `apply --dry-run`. A
   deterministic generator and a golden-plan test (`small.plan.json`).

6. **Client wrappers** — `internal/<service>/*.go`: create / get / delete and any
   mutating operations, each through the shared `timed` seam so every call feeds
   the metrics collector and the OTEL instruments. Add a `WaitForReady` polling
   step **only if the service's creates are asynchronous** (Neutron ports reach
   `ACTIVE`, Cinder volumes reach `available`); Keystone creates are synchronous
   and poll nothing.

7. **Staged apply executor** — `internal/<service>/executor`: stages in
   dependency order, concurrent within a stage up to `--concurrency`, retry with
   backoff on transient errors, **fail fast on the terminal pre-check error**
   (a quota `413` or a privilege `403`), per-operation timeout. Test it against a
   fake client, as `internal/glance/executor` does, including the partial-failure
   and fast-fail paths.

8. **Pre-check** — read-only, fail-open, never auto-raise. Pick the one that
   matches the service's first failure mode:
   - a **quota pre-check** (Neutron, Cinder, Nova) that sizes the plan against
     the project quota and aborts with an itemized message; or
   - a **privilege pre-check** (Keystone) that classifies the caller's rights and
     fails fast when they are insufficient.
   A service with neither default quota nor a privilege gate states that it has
   no pre-check and why.

9. **Run identity and cleanup** — attach a run identifier through whatever the
   service API supports, and say which in the issue:
   - Neutron — a **tag**; Cinder — **metadata**; Keystone — a **name prefix**
     (`dizzy-<runid>-<logical>`, projects also tagged); Nova — server and volume
     **metadata**; Glance — an image **tag**.
   `cleanup` deletes in reverse dependency order, is idempotent (404 = success),
   and **never touches a resource without the run's identifier**. The run
   record's created-list is the belt-and-suspenders fallback handle.

10. **Churn graph** — `internal/chaos/<service>graph`, feeding the shared churn
    engine `internal/chaos/engine.go`. The engine's parents-outlive-children
    invariant gives lifecycle ordering for free. Model each service's mutating
    operation as a churn step: Cinder `extend`, Keystone token issue, Glance
    visibility/member churn, Nova resize/attach.

11. **Monitor** — the shared loop driver (pre-flight sweep -> `apply` ->
    `cleanup`), `--interval` (default `0` = continuous), `--iterations`,
    `--error-wait`, fresh auth per iteration, graceful two-signal shutdown. The
    pre-flight sweep reclaims leftover `dizzy-<...>` resources — so never run two
    testers in the same project concurrently.

12. **Run record, report, status, OTEL — reused, not duplicated** — the run
    record's `service` field (`internal/run/run.go`) takes the new value; the
    created-list stays `internal/resource.Resource`. New `kind` and `operation`
    label values flow through the existing histograms and the errors-by-kind
    counter with **no schema change**; the `service=<name>` resource attribute
    keeps the OTEL series separable and Grafana's `service` variable picks it up.
    Note any panel that hardcodes a `kind`/`operation` regex to verify.

13. **Docs, Makefile, README** — `docs/reference/{cli,scenario-schema,metrics,run-record}.md`
    and the relevant `docs/explanation/*` gain the new service; the README status
    paragraph and the safety identity line name it; the Makefile
    `devstack-osism` / `devstack-c5c3` service lists include it.

---

## The variation axes — what the interview must settle

The checklist is fixed; these are the knobs that differ per service. Resolve
each one with the author before writing, and record the answer as a decision.

- **Namespace name** — the OpenStack project name (confirm it, e.g. Swift ->
  `swift`, Octavia -> `octavia`, Barbican -> `barbican`, Manila -> `manila`,
  Designate -> `designate`).
- **gophercloud client** — the v2 constructor and any required microversion.
- **In-scope operations** — the resource lifecycle the scenario drives. Be
  specific and ordered by dependency, as #39's numbered scope list is.
- **Out-of-scope** — every excluded operation, one per bullet, each tagged
  "per the author" so a reviewer knows it was a decision, not an oversight.
- **Sync vs async creates** — whether stage N needs a readiness-polling step.
- **Pre-check kind** — quota, privilege, or none (with the reason).
- **Run-identity mechanism** — tag, metadata, or name prefix, dictated by what
  the service's API actually offers. Check before asserting it has a tag API.
- **Resolution-seam flags** — cloud-specific inputs the plan references by
  logical name and an apply-time binder resolves: Neutron `--external-network`,
  Cinder `--volume-type`, Keystone `--privilege`/`--domain`/`--roles`. Name any
  the new service needs.
- **Referenced vs created supporting resources** — what dizzy creates and tears
  down itself versus what it references by name because creating it would need
  rights the tool does not assume (Nova references images and flavors; it
  creates the networks, ports, and volumes its servers need).
- **Driver set now** — land `chaos` and `monitor` in this issue (Keystone, Nova,
  Glance did), or ship a create-only first slice and defer the loop drivers to
  follow-up issues (Cinder did: #30 -> #31 monitor, #32 chaos). Ask.
- **Profile sizing** — the small/medium/large counts, with `small` fitting the
  service's default quotas.
- **New label values** — the `kind` and `operation` values the service adds to
  the metrics and OTEL vocabulary.

---

## The issue skeleton

Reproduce the structure of #30 and #39. A complete new-service issue is
**elaborate depth**: it plans the work, so it cites real files and symbols
(verify each) and marks new ones as added.

```markdown
**Category**: feature | **Scope**: Large

## Description
   dizzy exercises N OpenStack services today — <list>. <Service> is not among
   them. This issue adds `dizzy <service>` as the (N+1)th namespace at full
   parity: the seven verbs, bundled profiles, deterministic plans, a run record,
   chaos, monitor, status, report, cleanup, reference docs, and tests. State
   what the scenario drives and, in one paragraph, what stays out of scope.

## Motivation
   The concrete gap this closes: what an operator cannot measure today, and why
   the service sits on a path that matters. Two or three paragraphs, problem
   first.

## Scope (first slice)
   The in-scope operations, numbered in dependency order.

## Non-goals (for now)
   One bullet per excluded operation, each "per the author".

## Proposed design
   Numbered subsections walking the architecture checklist above, in this order:
   CLI namespace; scenario schema and profiles (with the sizing table); plan
   model; apply pipeline stages; the pre-check; churn/chaos; monitor; run
   identity and cleanup; run records/report/status/OTEL. Only the subsystems the
   service actually needs — drop readiness polling for a synchronous service,
   name the identity mechanism the API supports.

## Implementation sketch
   An ordered list of the commits, each a self-contained step, matching the real
   commit history of the last service (the profiles-and-embed commit, the client
   commit, the executor commit, the churn-graph commit, the namespace-wiring
   commit, the chaos+monitor commit, the docs commit).

## Acceptance criteria
   A `- [ ]` checkbox per observable check, each starting with a verb: byte-
   identical generate, apply writes a run record with per-operation timings,
   dry-run prints the summary, the pre-check aborts an oversized/under-privileged
   plan, chaos records ChaosStats and tears down by identifier, monitor loops on
   cadence, cleanup is idempotent and identifier-scoped, OTEL carries the new
   labels with `service=<name>`, and existing services stay green.

## Decisions
   The settled choices, each "Decided", and any genuinely open question called
   out as "Open" rather than defaulted silently.
```

### Title

`First <Service> (<domain>) support: <one line naming the scenario, the full
verb set, and any headline concern>`, imperative, no priority prefix. Examples
from the tracker:

- `First Glance (image) support: image lifecycle scenarios with the full verb set and synthetic upload payloads`
- `First Nova (compute) support: server lifecycle scenarios with the full verb set and a live-migration admin pre-check`

### Footer

No attribution footer. The body ends with the last line of `## Decisions`,
matching the fully-specified new-service issues #30 and #39.
