package neutron

// Quota handling follows a document-and-require policy. PrecheckQuota reads the
// target project's Neutron quotas and aborts an oversized plan with an itemized
// message before any resource is created; raising the quotas is then the
// operator's step (see the README). The considered alternative — having the tool
// auto-raise the project's quotas through an admin cloud — is deliberately not
// implemented, because it would require admin credentials the tool otherwise
// never needs and would let a load test quietly reconfigure the project.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/quotas"

	"github.com/B42Labs/dizzy/internal/plan"
)

// needs is the count of each quota-bounded resource an expanded plan requires.
type needs struct {
	networks       int
	subnets        int
	routers        int
	securityGroups int
	securityRules  int
	ports          int
	subnetPools    int
	floatingIPs    int
}

// planNeeds counts the quota-bounded resources a plan will create. Subnet-based
// router interfaces each create a new gateway port owned by the router, so they
// count against the port quota alongside the explicit ports; port-based
// interfaces attach an already-counted port and add nothing. When an external
// network is available, each external-gateway router also adds a gateway port,
// and the floating IPs count against the floating-IP quota; without one, neither
// is created, so neither is counted.
func planNeeds(p *plan.Plan, externalAvailable bool) needs {
	var rules int
	for _, sg := range p.SecurityGroups {
		rules += len(sg.Rules)
	}
	var subnetInterfaces int
	for _, ri := range p.RouterInterfaces {
		if ri.Subnet != "" {
			subnetInterfaces++
		}
	}
	ports := len(p.Ports) + subnetInterfaces
	floatingIPs := 0
	if externalAvailable {
		ports += p.RoutersWithExternalGateway()
		floatingIPs = len(p.FloatingIPs)
	}
	return needs{
		networks:       len(p.Networks),
		subnets:        len(p.Subnets),
		routers:        len(p.Routers),
		securityGroups: len(p.SecurityGroups),
		securityRules:  rules,
		ports:          ports,
		subnetPools:    len(p.SubnetPools),
		floatingIPs:    floatingIPs,
	}
}

// PrecheckQuota reads the project's Neutron quotas and returns an itemized error
// if the plan would exceed any of them, before any resource is created. It is
// fail-open only where the read cannot meaningfully proceed: if the project id
// cannot be derived from the auth result, or the quota read is denied (a common
// non-admin 403), it logs a warning and returns nil, leaving the executor's
// quota fast-fail as the backstop. Any other read failure (a transient 5xx, a
// timeout) is returned so the plan aborts before creating anything rather than
// hitting the real quota wall mid-apply.
func PrecheckQuota(ctx context.Context, gc *gophercloud.ServiceClient, p *plan.Plan, externalAvailable bool) error {
	projectID, ok := projectIDFromAuth(gc)
	if !ok {
		slog.Warn("quota pre-check skipped: project id unavailable from auth result")
		return nil
	}
	quota, err := quotas.Get(ctx, gc, projectID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, 403) {
			slog.Warn("quota pre-check skipped: quota read denied (non-admin?)", "error", err)
			return nil
		}
		return fmt.Errorf("reading project quotas for pre-check: %w", err)
	}
	return checkQuota(planNeeds(p, externalAvailable), quota)
}

// projectIDFromAuth extracts the authenticated project id from the v3 token auth
// result. It returns false when the result was not recorded (e.g. a manually
// supplied token) or is not a v3 create-token result, so the caller can skip the
// pre-check rather than guess.
func projectIDFromAuth(gc *gophercloud.ServiceClient) (string, bool) {
	ar := gc.GetAuthResult()
	if ar == nil {
		return "", false
	}
	tr, ok := ar.(tokens.CreateResult)
	if !ok {
		return "", false
	}
	project, err := tr.ExtractProject()
	if err != nil || project == nil || project.ID == "" {
		return "", false
	}
	return project.ID, true
}

// checkQuota compares what a plan needs against the project quota and returns
// one itemized error naming every resource type that would be exceeded, or nil.
// A negative limit means unlimited (Neutron's convention) and never blocks.
func checkQuota(need needs, q *quotas.Quota) error {
	var over []string
	check := func(name string, want, limit int) {
		if limit >= 0 && want > limit {
			over = append(over, fmt.Sprintf("%s need %d, quota %d", name, want, limit))
		}
	}
	check("networks", need.networks, q.Network)
	check("subnets", need.subnets, q.Subnet)
	check("routers", need.routers, q.Router)
	check("security groups", need.securityGroups, q.SecurityGroup)
	check("security group rules", need.securityRules, q.SecurityGroupRule)
	check("ports", need.ports, q.Port)
	check("subnet pools", need.subnetPools, q.SubnetPool)
	check("floating IPs", need.floatingIPs, q.FloatingIP)
	if len(over) == 0 {
		return nil
	}
	return fmt.Errorf("plan exceeds project quota; raise these quotas before applying: %s", strings.Join(over, "; "))
}
