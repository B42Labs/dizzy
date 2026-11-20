package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/horizon"
	horizonexec "github.com/B42Labs/dizzy/internal/horizon/executor"
	horizonplan "github.com/B42Labs/dizzy/internal/horizon/plan"
	"github.com/B42Labs/dizzy/internal/keystone"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// newHorizonCmd builds the "horizon" command namespace and attaches its
// subcommands: generate, apply, chaos, monitor, status, report, and cleanup.
// report is the same service-agnostic builder the other namespaces use.
func newHorizonCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "horizon",
		Short: "Horizon dashboard logins, pages, project switches and logouts, checked the way a browser sees them",
	}

	cmd.AddCommand(
		newHorizonGenerateCmd(opts),
		newHorizonApplyCmd(opts),
		newHorizonChaosCmd(opts),
		newHorizonMonitorCmd(opts),
		newHorizonStatusCmd(opts),
		newReportCmd(opts),
		newHorizonCleanupCmd(opts),
	)

	return cmd
}

// horizonURLFlagHelp is the help text of --horizon-url.
const horizonURLFlagHelp = "base URL of the Horizon dashboard, including its WEBROOT path if any, e.g. https://horizon.example.com or https://cloud.example.com/dashboard (required)"

// parseHorizonURL parses the --horizon-url value into the dashboard's base
// URL, with trailing slashes trimmed from its path, which then prefixes
// every request. It rejects, before any request, an empty value, a scheme
// other than http or https, an empty host, and a query or fragment. An http
// URL logs a warning, since the login form posts the password in the clear.
func parseHorizonURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("--horizon-url is required: the base URL of the Horizon dashboard, e.g. https://horizon.example.com")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("--horizon-url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("--horizon-url %q: the scheme must be http or https", u.Redacted())
	}
	if u.Host == "" {
		return nil, fmt.Errorf("--horizon-url %q: the URL names no host", u.Redacted())
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("--horizon-url %q: the URL must not carry a query or a fragment", u.Redacted())
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = strings.TrimRight(u.RawPath, "/")
	if u.Scheme == "http" {
		slog.Warn("--horizon-url uses http: every login sends its password unencrypted", "url", u.Redacted())
	}
	return u, nil
}

// horizonTarget is what apply, chaos and monitor resolve before the first
// Horizon request: the dashboard's base URL, the authenticated clouds.yaml
// identity, the role created users get (empty without plan users), the
// clouds.yaml user's enabled projects (only when it logs in and a session
// switches), and whether the plan creates users.
type horizonTarget struct {
	base      *url.URL
	identity  *config.PasswordIdentity
	roleID    string
	projects  []string
	withUsers bool
}

// connectHorizon resolves what a run logs in against, in this order: it
// authenticates the clouds.yaml entry at Keystone (config.NewPasswordIdentity)
// and returns that error as it is, so a wrong password fails once, before
// any dashboard login. Without plan users it needs the entry's password, and
// when a session switches, an enabled project of the user to switch to.
// With plan users it looks the scenario's role up by name. It sends no
// Horizon request, and logs one line naming the URL, the identity, the
// user and the domain.
func connectHorizon(ctx context.Context, opts *globalOptions, base *url.URL, p *horizonplan.Plan) (horizonTarget, error) {
	id, err := config.NewPasswordIdentity(ctx, opts.osCloud)
	if err != nil {
		return horizonTarget{}, err
	}
	t := horizonTarget{base: base, identity: id, withUsers: len(p.Users) > 0}

	identity := "cloud"
	if !t.withUsers {
		if id.Password == "" {
			return horizonTarget{}, errors.New("horizon logs in as the clouds.yaml user and the entry has no password; use a username and password entry, or set resources.users to log in as created users")
		}
		if p.Switches() > 0 {
			discovery := horizon.New(base, id.TLS, "", metrics.NewCollector())
			discovery.SetIdentity(id.Identity)
			projects, err := discovery.AvailableProjects(ctx)
			if err != nil {
				return horizonTarget{}, err
			}
			if len(projects) == 0 {
				return horizonTarget{}, errors.New("the clouds.yaml user has no enabled project to switch to")
			}
			t.projects = projects
		}
	} else {
		identity = "created"
		// A discovery client (no run id, its own collector) resolves the role;
		// the lookup is untimed.
		roleID, err := keystone.New(id.Identity, "", metrics.NewCollector()).FindRoleByName(ctx, p.Role)
		if errors.Is(err, keystone.ErrRoleNotFound) {
			return horizonTarget{}, fmt.Errorf("role %q not found in Keystone; set role in the scenario", p.Role)
		}
		if err != nil {
			return horizonTarget{}, err
		}
		t.roleID = roleID
	}

	slog.Info("logging in to Horizon", "url", base.Redacted(), "identity", identity,
		"user", id.Username, "domain", id.DomainName)
	return t, nil
}

// client returns the Horizon client of one run: its sessions carry the
// entry's TLS settings and the dizzy/<version> User-Agent, its Keystone
// side the entry's identity client, and it records into collector and tel.
func (h horizonTarget) client(runID string, collector *metrics.Collector, tel *telemetry.Telemetry) *horizon.Client {
	c := horizon.New(h.base, h.identity.TLS, runID, collector)
	c.SetTelemetry(tel)
	c.SetIdentity(h.identity.Identity)
	c.SetUserAgent("dizzy/" + version)
	return c
}

// execTarget returns the executor's view of the target, opening sessions
// through client. Its Identity is client when the plan creates users and
// nil otherwise.
func (h horizonTarget) execTarget(client *horizon.Client) horizonexec.Target {
	t := horizonexec.Target{
		Open:       func() horizonexec.Session { return client.NewSession() },
		DomainID:   h.identity.DomainID,
		DomainName: h.identity.DomainName,
		RoleID:     h.roleID,
		Cloud: horizon.Credentials{
			Username: h.identity.Username,
			Password: h.identity.Password,
			Domain:   h.identity.DomainName,
		},
		Projects: h.projects,
	}
	if h.withUsers {
		t.Identity = client
	}
	return t
}

// horizonTelemetryConfig is the telemetry configuration of horizon apply,
// chaos and monitor running plan p: the cloud --os-cloud or $OS_CLOUD names,
// p's scenario, and service horizon.
func horizonTelemetryConfig(opts *globalOptions, p *horizonplan.Plan) telemetry.Config {
	return telemetry.Config{Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "horizon"}
}
