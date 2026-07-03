package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/B42Labs/openstack-tester/internal/keystone"
	keystoneplan "github.com/B42Labs/openstack-tester/internal/keystone/plan"
	"github.com/B42Labs/openstack-tester/internal/metrics"
)

// defaultKeystoneRoles is the default set of existing roles a domain manager
// reuses for assignments. admin is deliberately absent: a manager may not grant
// it.
const defaultKeystoneRoles = "member,reader"

// newKeystoneCmd builds the "keystone" command namespace and attaches its
// subcommands. It follows the project-name convention (neutron, cinder ->
// keystone), not the API name (identity). report is the same service-agnostic
// builder the neutron and cinder namespaces use.
func newKeystoneCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keystone",
		Short: "Keystone (identity) load and consistency commands",
	}

	cmd.AddCommand(
		newKeystoneGenerateCmd(opts),
		newKeystoneApplyCmd(opts),
		newKeystoneChaosCmd(opts),
		newKeystoneStatusCmd(opts),
		newReportCmd(opts),
		newKeystoneCleanupCmd(opts),
	)

	return cmd
}

// keystonePrivilegeFlags are the apply/chaos/monitor flags that select and
// parametrize the privilege tier.
type keystonePrivilegeFlags struct {
	privilege string
	domain    string
	roles     string
}

// register wires the privilege flags onto a command's flag set.
func (f *keystonePrivilegeFlags) register(flags *pflag.FlagSet) {
	flags.StringVar(&f.privilege, "privilege", "auto", "privilege tier: auto (detect), admin, or domain-manager")
	flags.StringVar(&f.domain, "domain", "", "in-scope domain for domain-manager mode (default: the domain the token is scoped to; ignored in admin mode)")
	flags.StringVar(&f.roles, "roles", defaultKeystoneRoles, "existing roles to reuse for assignments in domain-manager mode, comma-separated (ignored in admin mode)")
}

// resolveKeystonePrivilege runs the read-only privilege pre-check and binds the
// domain-manager tier. It validates the --privilege value, classifies the caller
// from the token, applies the override, and — in domain-manager mode — resolves
// the in-scope domain and reusable roles and rejects an admin-only plan. It is
// the structural replacement for the quota pre-check Neutron and Cinder run.
func resolveKeystonePrivilege(ctx context.Context, gc *gophercloud.ServiceClient, f keystonePrivilegeFlags, p *keystoneplan.Plan) (keystone.Tier, keystone.Resolution, error) {
	switch f.privilege {
	case "auto", "admin", "domain-manager":
	default:
		return "", keystone.Resolution{}, fmt.Errorf("invalid --privilege %q: want auto, admin, or domain-manager", f.privilege)
	}

	cls, clsErr := keystone.ClassifyPrivilege(ctx, gc)

	tier := cls.Tier
	switch f.privilege {
	case "admin":
		tier = keystone.TierAdmin
	case "domain-manager":
		tier = keystone.TierDomainManager
	case "auto":
		if clsErr != nil {
			return "", keystone.Resolution{}, clsErr
		}
	}
	// With an explicit override, a classification failure is not fatal: the
	// operator has asserted the tier and the executor's 403 fast-fail is the
	// backstop.
	if f.privilege != "auto" && clsErr != nil {
		slog.Warn("privilege classification failed; proceeding with the --privilege override",
			"override", f.privilege, "error", clsErr)
	}

	if tier == keystone.TierAdmin {
		if f.domain != "" || f.roles != defaultKeystoneRoles {
			slog.Warn("--domain and --roles are ignored in admin mode (domains are created, roles are created)")
		}
		slog.Info("privilege pre-check", "tier", tier)
		return keystone.TierAdmin, keystone.Resolution{}, nil
	}

	if err := keystone.CheckDomainManagerPlan(p); err != nil {
		return "", keystone.Resolution{}, err
	}
	// A discovery client (no run id, its own collector) resolves the domain and
	// roles; the resolution calls are untimed.
	dc := keystone.New(gc, "", metrics.NewCollector())
	res, err := keystone.ResolveDomainManager(ctx, dc, f.domain, splitRoles(f.roles), cls)
	if err != nil {
		return "", keystone.Resolution{}, err
	}
	slog.Info("privilege pre-check", "tier", tier, "domain", res.DomainName, "roles", res.RoleNames)
	return keystone.TierDomainManager, res, nil
}

// splitRoles parses a comma-separated role list into trimmed, non-empty names.
func splitRoles(csv string) []string {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
