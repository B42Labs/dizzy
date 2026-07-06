package keystone

// Privilege handling is Keystone's structural analog of the Neutron/Cinder quota
// pre-check: before any write, apply/chaos/monitor classify the caller into one
// of two tiers from the token's roles and scope and fail fast when neither
// fits. Unlike quota, the classification is a fail-fast gate, not a guarantee —
// Keystone policy is deployment-configurable — so --privilege overrides it and
// the executor's fast-fail on a 403 (ErrForbidden) is the backstop. The
// domain-manager binder resolves the single in-scope domain and the reusable
// roles the run assigns.

import (
	"context"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/domains"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"

	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
)

// Tier is the privilege class a caller is resolved into.
type Tier string

const (
	// TierAdmin is a cloud admin (admin role at any scope): it creates the
	// plan's domains and roles and runs the full plan.
	TierAdmin Tier = "admin"
	// TierDomainManager is a domain manager (manager role on a domain-scoped
	// token): it creates no domains or roles, reuses existing roles, and runs
	// inside the single in-scope domain.
	TierDomainManager Tier = "domain-manager"
)

// Classification is the outcome of the read-only privilege pre-check: the tier
// and, for a domain manager, the token's in-scope domain.
type Classification struct {
	Tier       Tier
	DomainID   string
	DomainName string
}

// Resolution is the domain-manager binding: the single in-scope domain and the
// reusable roles (names and cloud ids) the run's assignments draw from.
type Resolution struct {
	DomainID   string
	DomainName string
	RoleNames  []string
	RoleIDs    []string
}

// tokenExtractor is the read side of a v3 token result — satisfied by both
// tokens.CreateResult (the cached auth result) and tokens.GetResult (the
// self-validation fallback) — so classification reads roles and scope from
// whichever is available.
type tokenExtractor interface {
	ExtractRoles() ([]tokens.Role, error)
	ExtractDomain() (*tokens.Domain, error)
}

// ClassifyPrivilege reads the caller's token roles and scope and classifies the
// tier: the admin role (any scope) is admin; the manager role on a
// domain-scoped token is domain-manager with that domain; anything else
// (including a project-scoped manager) fails fast with a message naming the
// roles seen and what is required. It reads the cached auth result when usable
// and falls back to a GET /v3/auth/tokens self-validation otherwise. It is
// untimed — discovery, not workload.
func ClassifyPrivilege(ctx context.Context, gc *gophercloud.ServiceClient) (Classification, error) {
	ex, err := authExtractor(ctx, gc)
	if err != nil {
		return Classification{}, err
	}
	rolesList, err := ex.ExtractRoles()
	if err != nil {
		return Classification{}, fmt.Errorf("reading token roles for privilege classification: %w", err)
	}
	roleNames := make([]string, 0, len(rolesList))
	for _, r := range rolesList {
		roleNames = append(roleNames, r.Name)
	}
	domain, _ := ex.ExtractDomain() // non-nil only for a domain-scoped token

	if hasRole(roleNames, "admin") {
		return Classification{Tier: TierAdmin}, nil
	}
	if hasRole(roleNames, "manager") && domain != nil && domain.ID != "" {
		return Classification{
			Tier:       TierDomainManager,
			DomainID:   domain.ID,
			DomainName: domain.Name,
		}, nil
	}
	return Classification{}, fmt.Errorf(
		"caller is neither cloud admin nor domain manager: token carries roles %v; keystone needs the 'admin' role (any scope) or the 'manager' role on a domain-scoped token (use --privilege to override)",
		roleNames)
}

// authExtractor returns the usable token result to classify from: the cached v3
// create-token auth result when present, otherwise a self-validation GET on the
// current token.
func authExtractor(ctx context.Context, gc *gophercloud.ServiceClient) (tokenExtractor, error) {
	if ar := gc.GetAuthResult(); ar != nil {
		if cr, ok := ar.(tokens.CreateResult); ok {
			return cr, nil
		}
	}
	token := gc.Token()
	if token == "" {
		return nil, fmt.Errorf("no token available to classify privilege")
	}
	res := tokens.Get(ctx, gc, token)
	if res.Err != nil {
		return nil, fmt.Errorf("validating token for privilege classification: %w", res.Err)
	}
	return res, nil
}

// ResolveDomainManager binds domain-manager mode: it resolves the in-scope
// domain (cls.DomainID, or --domain by name) and each reusable role name to its
// cloud id, rejecting the admin role (a manager may not grant it). A --domain
// that cannot be listed is a hard, actionable error — a manager token should be
// scoped to the domain it manages.
func ResolveDomainManager(ctx context.Context, c *Client, domainFlag string, roleNames []string, cls Classification) (Resolution, error) {
	domainID, domainName := cls.DomainID, cls.DomainName
	if domainFlag != "" {
		d, err := c.findDomainByName(ctx, domainFlag)
		if err != nil {
			return Resolution{}, err
		}
		domainID, domainName = d.ID, d.Name
	}
	if domainID == "" {
		return Resolution{}, fmt.Errorf("domain-manager mode needs an in-scope domain: scope the token to a domain or pass --domain <name>")
	}

	// Reject the admin role anywhere in the list before resolving any role, so a
	// manager can never be asked to grant admin — the check must not depend on a
	// reachable cloud.
	for _, name := range roleNames {
		if strings.EqualFold(strings.TrimSpace(name), "admin") {
			return Resolution{}, fmt.Errorf("role %q cannot be reused in domain-manager mode: a manager may not grant the admin role", name)
		}
	}

	res := Resolution{DomainID: domainID, DomainName: domainName}
	for _, name := range roleNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		id, err := c.FindRoleByName(ctx, name)
		if err != nil {
			return Resolution{}, err
		}
		res.RoleNames = append(res.RoleNames, name)
		res.RoleIDs = append(res.RoleIDs, id)
	}
	if len(res.RoleIDs) == 0 {
		return Resolution{}, fmt.Errorf("no reusable roles resolved for domain-manager mode; pass existing roles with --roles")
	}
	return res, nil
}

// CheckDomainManagerPlan rejects a plan that domain-manager mode cannot run: it
// requires at most one domain (the single in-scope domain), since collapsing
// several logical domains onto one real domain is only meaningful for one. Roles
// are ignored by design — the binder reuses existing roles rather than the
// plan's — so a role count is not a rejection.
func CheckDomainManagerPlan(p *keystoneplan.Plan) error {
	if len(p.Domains) > 1 {
		return fmt.Errorf("scenario needs %d domains but domain-manager mode allows at most 1 (the in-scope domain); use --privilege admin with a cloud admin, or a single-domain scenario", len(p.Domains))
	}
	return nil
}

// findDomainByName resolves a domain by exact name, the --domain override's
// resolver. A 403 is a hard error here (unlike discovery listings): the operator
// asked for a specific domain, so tell them to scope the token to it instead.
func (c *Client) findDomainByName(ctx context.Context, name string) (domains.Domain, error) {
	pages, err := domains.List(c.gc, domains.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		if IsForbidden(err) {
			return domains.Domain{}, fmt.Errorf("listing domains to resolve --domain %q was denied; scope the token to that domain instead: %w", name, err)
		}
		return domains.Domain{}, fmt.Errorf("resolving domain %q: %w", name, err)
	}
	list, err := domains.ExtractDomains(pages)
	if err != nil {
		return domains.Domain{}, fmt.Errorf("extracting domains: %w", err)
	}
	for _, d := range list {
		if d.Name == name {
			return d, nil
		}
	}
	return domains.Domain{}, fmt.Errorf("domain %q not found", name)
}

// hasRole reports whether names contains want, case-insensitively.
func hasRole(names []string, want string) bool {
	for _, n := range names {
		if strings.EqualFold(n, want) {
			return true
		}
	}
	return false
}
