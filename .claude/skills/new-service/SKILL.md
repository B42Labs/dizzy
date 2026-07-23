---
name: new-service
description: Prepares a complete "First <Service> support" GitHub issue for adding a new OpenStack service namespace to dizzy at full parity with the existing ones. Use when the user wants to add a new OpenStack service (Swift, Octavia, Barbican, Manila, Designate, …) to dizzy, or to write up / file the issue for one. It plans the whole service; it does not implement it.
argument-hint: "[service] [openstack api domain]"
allowed-tools: AskUserQuestion Read Write Grep Glob Bash(gh auth status) Bash(gh repo view:*) Bash(gh issue list:*) Bash(gh issue view:*) Bash(gh issue create:*) Bash(git log:*)
---

# Prepare a new-service issue for dizzy

You are an engineer who has added a service to dizzy before and is writing the
issue for the next one. dizzy exercises OpenStack control planes: each service
is a CLI namespace (`dizzy neutron`, `dizzy cinder`, `dizzy keystone`,
`dizzy nova`, `dizzy glance`) speaking the same seven verbs. Adding a service is
a well-worn recipe, done four times the same way. Your job is to turn "add
<service>" into a **complete, self-contained, repo-grounded issue** that plans
the whole namespace at full parity — the kind of issue #30 (Cinder) and #39
(Keystone) are.

Arguments: $ARGUMENTS

Read this in full before Phase 1:

- `${CLAUDE_SKILL_DIR}/references/service-recipe.md` — the full-parity
  architecture checklist, the per-service variation axes, and the issue
  skeleton. This is the knowledge that makes the issue complete instead of
  generic.

**Hard gate: do not produce an issue in your first reply.** Run the phases. A
service that sounds simple is where an incomplete plan does the most damage —
the implementer discovers the missing subsystem three commits in.

## House rules

- **Write the issue in English**, whatever language the conversation uses. Ask
  your questions in the author's language — the issue stays English.
- **Ground every claim in the repo.** This is an elaborate-depth plan: it names
  real files and symbols. Open the directory before you name a file; mark
  anything the new service *adds* as "this issue adds", never as already there.
- **Be concrete and quantified.** Profile counts, not "several". Name the actual
  operation, package, and label value. Never the AI-slop register (delve,
  leverage-as-verb, robust, comprehensive, crucial, seamless).
- **Tag every out-of-scope item "per the author".** An exclusion is a decision,
  not an oversight, and the issue must show it was made on purpose.
- **Nothing reaches GitHub without an explicit yes.** Reading (`gh issue view`,
  `gh issue list`) is free; `gh issue create` is gated on Phase 6.

## Phase 1 — Ground yourself in the current pattern

Resolve the repository: `gh repo view --json nameWithOwner --jq .nameWithOwner`.
State which one you resolved.

Then read how the last service was actually built, so the plan matches today's
code and not this document's memory of it:

- `git log --oneline | grep -iE "glance|nova"` — the real commit sequence of the
  two most recent services; your `## Implementation sketch` mirrors it.
- `internal/glance/` — the live template: `scenario/`, `plan/`, `executor/`,
  `client.go`, and the churn graph under `internal/chaos/glancegraph`.
- `gh issue view 30` and `gh issue view 39` — the two most complete new-service
  issues; reproduce their structure.

Count the namespaces that exist now (`ls internal | grep -vE 'chaos|config|executor|metrics|plan|resource|run|scenario|telemetry'`), so you frame the new one as "the Nth namespace" correctly.

## Phase 2 — Identify the service

From the arguments or by asking: which OpenStack service, and its API domain
(the parenthetical in the title — `image`, `compute`, `identity`, …). Confirm
the **namespace name is the project name, not the API name**.

Read the code before you ask a question the repo answers. Then, using the
variation axes in the recipe, resolve the service-specific facts — some you
know, some you must ask:

- the gophercloud v2 client constructor and any microversion,
- whether creates are synchronous or need readiness polling,
- whether the service has default quotas (quota pre-check), needs elevated
  rights (privilege pre-check), or neither,
- how run identity attaches (tag / metadata / name prefix) — dictated by what
  the service API supports, so check rather than assume it has a tag API,
- any cloud-specific input the plan must reference by name and resolve at apply
  time (a resolution-seam flag).

## Phase 3 — Settle scope and the open decisions

Interview for exactly the choices the recipe's variation axes call out. Ask
inline, three to five numbered questions per round, highest ambiguity first,
each stating your assumption so the author corrects a specific rather than
answering a blank. The decisions that matter most:

1. **In-scope operations** — the resource lifecycle the scenario drives, ordered
   by dependency. Push for the real list, not a gesture at "the lifecycle".
2. **Out-of-scope** — what is deliberately excluded now.
3. **Driver set** — `chaos` and `monitor` in this issue, or a create-only first
   slice with the loop drivers deferred to follow-up issues (as Cinder split
   #30 → #31/#32). This changes the scope line and the acceptance criteria.
4. **Profile sizing** — the small/medium/large counts, `small` fitting the
   service's default quotas.

Use `AskUserQuestion` for a genuine either/or (driver set now vs. deferred;
quota vs. privilege pre-check when it is not obvious), one decision per call,
with a recommendation and the cost of choosing wrong. Ask the operation list and
sizing inline. Push once on a vague answer, then stop and carry whatever is
unsettled into the `## Decisions` section as an open question — never default it
silently.

## Phase 4 — Write the issue

Write the body to a file in your scratchpad, in the house skeleton from the
recipe: the `**Category**: feature | **Scope**: Large` header, then
`## Description`, `## Motivation`, `## Scope (first slice)`,
`## Non-goals (for now)`, `## Proposed design` (numbered subsystems from the
architecture checklist — only the ones this service needs), `## Implementation
sketch` (the commit sequence, mirroring the last service's real history),
`## Acceptance criteria` (a verb-led checkbox per observable check), and
`## Decisions`.

Walk the architecture checklist and confirm every subsystem is either designed
in a subsection or explicitly stated as reused unchanged. A missing subsystem is
the failure mode this skill exists to prevent — check that the pre-check, the
run identity + cleanup, the churn graph, and the docs/Makefile/README updates
are all present, not just the happy-path apply.

Give it the title `First <Service> (<domain>) support: <one line>`.

## Phase 5 — Check for duplicates

```bash
gh issue list --repo <owner/repo> --search "<service> support" --state all --limit 10 --json number,title,state,url
```

If a plausible duplicate exists, show it and ask whether to file anyway, comment
on it, or stop. Do not decide this yourself.

## Phase 6 — Confirm, then file

Show the complete rendered body. Ask, with `AskUserQuestion`, whether to file it
as-is, revise first, or cancel — and which you recommend. The body carries no
attribution footer, matching #30/#39.

File only on an explicit yes. Pass the body through `--body-file` — a body full
of backticks and `$` does not survive a shell string:

```bash
gh issue create --repo <owner/repo> --title "<title>" --body-file <path>
```

Attach only labels the author asked for; this project puts no severity or
priority labels on issues. Print the new issue's URL.

## Phase 7 — Hand off

Name the next step: the issue is ready to implement (or to elaborate further
through the author's planning flow). List any unresolved decision as exactly
that, so nobody mistakes a silent default for a settled choice.

## Before you file, verify

- The namespace is named for the OpenStack **project**, not the API service.
- Every file path in the body exists in the checkout; every added file is marked
  "this issue adds".
- The `## Proposed design` covers the pre-check, run identity + cleanup, the
  churn graph, monitor, and the docs/Makefile/README updates — not only apply.
- Every out-of-scope bullet says "per the author".
- Every acceptance criterion maps to a subsystem the design describes.
- `Scope` is `Large` (a new namespace at full parity always is), unless the
  author deferred the loop drivers to a smaller first slice.
- The body is English; the profile counts are numbers, not adjectives.

If any check fails, fix it before filing, not after.
