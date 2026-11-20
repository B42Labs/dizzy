# Check the Horizon dashboard

`dizzy horizon` uses the Horizon dashboard the way a browser does. Every
session loads the login form, posts it with the form's CSRF token, loads
pages, switches project and logs out, and dizzy checks every response
against what Horizon sends on success. The checks are HTTP only: dizzy runs
none of the dashboard's JavaScript. For every flag see
[The `horizon` namespace](../reference/cli.md#the-horizon-namespace), and for
every scenario key [Horizon](../reference/scenario-schema.md#horizon).

The Horizon behavior these checks rely on was read in openstack/horizon at
`stable/2026.1`. Other releases are not verified.

## What a journey checks

A run of `scenarios/horizon/small.yaml` runs five sessions. Each session is one
journey with its own cookie jar and its own connections, as a new browser has,
and no request follows a redirect:

| Step | Request | Passes when |
|---|---|---|
| `login_page` | `GET <base>/auth/login/` | `200`, and the form carries `csrfmiddlewaretoken` and a `region` |
| `login` | `POST <base>/auth/login/` | `302` to a path outside `<base>/auth/`, and a `sessionid` cookie |
| `page_<name>` | `GET <base><path>` for each page the session loads | `200` |
| `switch` | `GET <base>/auth/switch/<project>/?next=<base><first page>` | `302` to exactly `next`, and the `recent_project` cookie names the target |
| `switch_landing` | `GET <base><first page>` | `200`, without the message `Project switch failed` |
| `logout` | `POST <base>/auth/logout/` with the `csrftoken` cookie | `302` to a path that ends in `/auth/login/` |

The bundled profiles load `/project/api_access/` and `/identity/`, which
answer `200` on a control plane that runs only Keystone and Horizon.
`/project/` does not answer `200` there, because its default panel needs the
compute service.

A failed step does not stop its journey, except a failed login, after which
there is nothing to log out of. Every journey runs to its end, and the
command exits non-zero when one failed. A session that logged in always
logs out, also after Ctrl-C.

## Point dizzy at the dashboard

`--horizon-url` is the dashboard's base URL, the same for every profile:

```console
$ dizzy horizon apply --scenario scenarios/horizon/small.yaml --horizon-url https://horizon.example.com
```

Include Horizon's `WEBROOT` when the dashboard does not live at the root,
e.g. `https://cloud.example.com/dashboard`; every request then starts with
that path. An `http` URL works and logs a warning, since every login then
sends a password in the clear. `apply --dry-run` and `generate` need neither
the flag nor a `clouds.yaml`.

Before the first dashboard request, dizzy authenticates the `clouds.yaml`
entry of `--os-cloud` at Keystone. A wrong password fails there once, and
no dashboard login is sent with it. The token also supplies the user name
and the domain name the login form needs, so an entry that names its user by
id works. Horizon's TLS trust is the entry's: `cacert` and `verify` apply to
the dashboard's connections too. When the dashboard's certificate comes from
another CA than Keystone's, put both certificates into the entry's `cacert`
file.

## Choose the identity

The scenario's `resources.users` picks who the sessions log in as.

**The `clouds.yaml` user**, with `resources.users: 0`, the default of every
bundled profile. Every session logs in with the entry's user name, password
and domain, and the run creates nothing in Keystone, so its record lists no
resource. The entry needs a password: an application credential entry fails
with
`horizon logs in as the clouds.yaml user and the entry has no password`.
A session that switches project picks its target among the user's enabled
projects, so the user needs at least one; with none the run fails with
`the clouds.yaml user has no enabled project to switch to`. A member-only
test user is enough.

**Created users**, with `--set resources.users=N`. The run creates N users
and `resources.projects` projects in the domain of the `clouds.yaml` user,
grants each user the scenario's `role` on each project, and every session
logs in as one of those users and switches between those projects. Each user
gets a fresh random password that never leaves the process. The role must
exist; dizzy looks it up by name and fails with
`role "<name>" not found in Keystone; set role in the scenario` when it does
not. The entry needs the right to create projects and users in its domain
and to assign roles there, typically admin or a domain manager; a 403 stops
the run with a hint saying so. The created users and projects are named
`dizzyhorizon-<run>-<logical>` and stay after `apply` for `status` and
`cleanup`:

```console
$ dizzy horizon apply --scenario scenarios/horizon/small.yaml --horizon-url https://horizon.example.com --set resources.users=5
$ dizzy horizon status --run run-<id>.json
$ dizzy horizon cleanup --run run-<id>.json
```

## Protect the account

Keystone can lock an account after repeated failed logins. dizzy caps
failed logins nowhere on its own: the authentication at Keystone before the
first dashboard request is the guard against a wrong password. Use a
dedicated identity for dizzy, never a person's account, so a lockout costs
nothing but the run.

Behind a login rate limit, a proxy or a WAF in front of Horizon, lower
`--concurrency`. It bounds the journeys that run at once, and so the logins
that reach the dashboard at once. In `chaos`, `--max-parallel` and the
interval pace them as well.

## Read a failed check

Every step is a sample of its kind, and a failed one carries an error kind,
in the run record and under `service=horizon`:

- `rejected` is a login Horizon answered with the login form again, which is
  how it answers wrong credentials. With the `clouds.yaml` user this means
  Keystone accepted the password but the dashboard did not: check the
  domain, and whether Horizon talks to the same Keystone.
- `mismatch` is an answer with the expected status that does not hold what
  that status promises: a login form without its CSRF token or region, a
  login redirected into `/auth/` (an expired password or a TOTP step, which
  dizzy does not follow) or without a session cookie, a switch that lands
  elsewhere or leaves the session in another project, a landing page that
  reports the switch failed, and a logout without a CSRF cookie or one that
  does not lead back to the login page. dizzy checks no page content beyond
  the status, since table rows and panel markup differ by release and theme.
- `http_<status>` is a step answered with another status, e.g. `http_500`
  for a broken page or `http_302` for a page that redirected to the login
  form because the session was lost.
- `timeout`, `canceled` and `other` are a dashboard that did not answer in
  `--timeout`, an interrupted run, and a connection that failed. A refused
  connection is `other` and retried, as a 429 or a 5xx is.

## Soak the dashboard

`horizon chaos` churns sessions within the plan's sessions: it logs them in
and out at random and lets a live session browse, loading its pages and
switching project, with the probability `--browse-ratio`:

```console
$ dizzy horizon chaos --scenario scenarios/horizon/small.yaml --horizon-url https://horizon.example.com
$ dizzy horizon chaos --scenario scenarios/horizon/medium.yaml --horizon-url https://horizon.example.com --set resources.users=10 --browse-ratio 0.5
```

Created users and projects are provisioned once before the churn and deleted
after it, unless `--no-cleanup` leaves them with the cleanup hint. The
teardown logs out every session still logged in, with or without
`--no-cleanup`. A rejected login or a mismatch does not stop the churn, but
once the teardown has run the command exits non-zero, e.g.
`horizon checks failed (run <id>): 2 login(s) rejected, 0 response(s) mismatched`.

## Monitor the dashboard

`horizon monitor` runs the journeys of one apply per iteration, continuously
or on a cadence, and exports the samples with `--otel`:

```console
$ dizzy horizon monitor --scenario scenarios/horizon/small.yaml --horizon-url https://horizon.example.com --interval 5m --otel
```

It authenticates at startup, so a wrong URL, a wrong password or a missing
role fails before the loop, and again in every iteration, so a loop of days
never depends on one token; a failed authentication fails its iteration and
the loop goes on. See [Monitor Horizon](monitor-a-cloud.md#monitor-horizon).

## What dizzy leaves behind

A Horizon session carries no run identifier and cannot be listed, so a run
record never lists one and no cleanup can find one. dizzy logs out every
session it opened, also on interrupt. A session a killed process leaves
behind expires in Horizon on its own. No cookie, CSRF token or password is
written to a plan, a run record, a log line or an error text.
