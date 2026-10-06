package nova

// Live and cold migration are the admin operations in scope. Because both need
// the admin role and a cloud with at least two usable compute hosts —
// capabilities a project-scoped run does not assume — dizzy checks up front
// whether they are usable at all. The check is fail-open: when it fails, the run
// skips both migration kinds with a warning each and continues, mirroring the
// quota pre-check's shape. A missing admin capability never aborts a run.

import (
	"context"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
)

// PrecheckMigration reports whether migration is usable for this run, with a
// human-readable reason when it is not. Its verdict gates live and cold
// migration alike, since both need the same capabilities. It never returns an
// error: every failure path — missing token roles, no admin role, an inability
// to list hypervisors (including a non-admin 403), or fewer than two usable
// compute hosts — resolves to (false, reason) so the caller can log a warning
// and prune the migrations from the run without aborting. It reads the caller's roles
// from the cached auth result and counts hypervisors that are up and enabled.
func PrecheckMigration(ctx context.Context, gc *gophercloud.ServiceClient) (bool, string) {
	roles, ok := rolesFromAuth(gc)
	if !ok {
		return false, "token roles unavailable from auth result"
	}
	if !hasRole(roles, "admin") {
		return false, "credentials lack the admin role"
	}
	pages, err := hypervisors.List(gc, hypervisors.ListOpts{}).AllPages(ctx)
	if err != nil {
		return false, fmt.Sprintf("listing hypervisors was denied or failed: %v", err)
	}
	hosts, err := hypervisors.ExtractHypervisors(pages)
	if err != nil {
		return false, fmt.Sprintf("reading hypervisors failed: %v", err)
	}
	return decideMigration(roles, true, hosts)
}

// decideMigration is the pure decision behind PrecheckMigration: given
// the caller's roles (and whether they were readable) and the cloud's
// hypervisors, it returns whether migration (live and cold) is usable and,
// when not, why.
// It counts only hypervisors that are both up and enabled, since a down or
// disabled host cannot receive a migration.
func decideMigration(roles []string, rolesOK bool, hosts []hypervisors.Hypervisor) (bool, string) {
	if !rolesOK {
		return false, "token roles unavailable from auth result"
	}
	if !hasRole(roles, "admin") {
		return false, "credentials lack the admin role"
	}
	var usable int
	for _, h := range hosts {
		if h.State == "up" && h.Status == "enabled" {
			usable++
		}
	}
	if usable < 2 {
		return false, fmt.Sprintf("%d usable compute host(s), need at least 2", usable)
	}
	return true, ""
}

// rolesFromAuth reads the caller's role names from the cached v3 create-token
// auth result. It returns false when the result was not recorded or is not a v3
// create-token result, so the caller can treat the capability as unavailable
// rather than guess. It is the ClassifyPrivilege read without its
// identity-API self-validation fallback: the pre-check is fail-open, so a
// missing cached result simply disables migration.
func rolesFromAuth(gc *gophercloud.ServiceClient) ([]string, bool) {
	ar := gc.GetAuthResult()
	if ar == nil {
		return nil, false
	}
	cr, ok := ar.(tokens.CreateResult)
	if !ok {
		return nil, false
	}
	roleList, err := cr.ExtractRoles()
	if err != nil {
		return nil, false
	}
	names := make([]string, 0, len(roleList))
	for _, r := range roleList {
		names = append(names, r.Name)
	}
	return names, true
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
