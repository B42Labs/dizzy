package octavia

// Quota handling follows the same document-and-require policy as Neutron and
// Barbican. PrecheckQuota reads the project's Octavia quotas and aborts an
// oversized plan with an itemized message before any resource is created;
// raising the quotas is then the operator's step. Octavia's quota API reports
// limits and no usage, so objects already in the project are not counted. The
// Neutron networks, subnets, and VIP ports a run consumes are not pre-checked,
// so the executor's quota fast-fail is the backstop there. It never
// auto-raises quotas, which would require admin credentials the tool otherwise
// never needs.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/quotas"

	"github.com/B42Labs/dizzy/internal/octavia/plan"
)

// needs is the count of each Octavia quota-bounded object a plan creates.
type needs struct {
	loadBalancers  int
	listeners      int
	pools          int
	members        int
	healthMonitors int
}

// planNeeds counts the quota-bounded objects a plan will create.
func planNeeds(p *plan.Plan) needs {
	return needs{
		loadBalancers:  len(p.LoadBalancers),
		listeners:      len(p.Listeners),
		pools:          len(p.Pools),
		members:        len(p.Members),
		healthMonitors: len(p.HealthMonitors),
	}
}

// PrecheckQuota reads the project's Octavia quotas from
// <endpoint>/v2.0/quotas/<project> and returns an itemized error if the plan
// would exceed the loadbalancer, listener, pool, member, or healthmonitor
// quota, before any resource is created. The read is not recorded in the
// metrics. It is fail-open only where the read cannot meaningfully proceed: if
// the project id cannot be derived from the auth result, or the quota read is
// denied (a common non-admin 403), it logs a warning and returns nil, leaving
// the executor's quota fast-fail as the backstop. Any other read failure (a
// transient 5xx, a timeout) is returned so the plan aborts before creating
// anything rather than hitting the real quota wall mid-apply.
func PrecheckQuota(ctx context.Context, lb *gophercloud.ServiceClient, p *plan.Plan) error {
	return precheck(ctx, lb, planNeeds(p))
}

// PrecheckFrontendQuota is PrecheckQuota for the Octavia frontend of a mix
// persona: it reads the project's Octavia quotas from
// <endpoint>/v2.0/quotas/<project> and returns an itemized error if the
// frontend's load balancers, listeners, pools, members, or health monitors
// would exceed the loadbalancer, listener, pool, member, or healthmonitor
// quota, before any resource is created. A nil frontend needs nothing, so it
// returns nil without a request. The read, its fail-open paths, and its errors
// are PrecheckQuota's.
func PrecheckFrontendQuota(ctx context.Context, lb *gophercloud.ServiceClient, f *plan.Frontend) error {
	if f == nil {
		return nil
	}
	return precheck(ctx, lb, needs{
		loadBalancers:  len(f.LoadBalancers),
		listeners:      len(f.Listeners),
		pools:          len(f.Pools),
		members:        len(f.Members),
		healthMonitors: len(f.HealthMonitors),
	})
}

// precheck reads the project's Octavia quotas and compares need against them,
// the shared body of PrecheckQuota and PrecheckFrontendQuota.
func precheck(ctx context.Context, lb *gophercloud.ServiceClient, need needs) error {
	projectID, ok := ProjectIDFromAuth(lb)
	if !ok {
		slog.Warn("quota pre-check skipped: project id unavailable from auth result")
		return nil
	}
	quota, err := quotas.Get(ctx, lb, projectID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, 403) {
			slog.Warn("quota pre-check skipped: quota read denied", "error", err)
			return nil
		}
		return fmt.Errorf("reading Octavia quotas for pre-check: %w", err)
	}
	if quota == nil {
		return errors.New("reading Octavia quotas for pre-check: response carries no quota object")
	}
	return checkQuota(need, quota)
}

// ProjectIDFromAuth extracts the authenticated project id from the v3 token auth
// result. It returns false when the result was not recorded (e.g. a manually
// supplied token), is not a v3 create-token result, or names no project (a
// domain-scoped token), so the caller can skip the pre-check rather than guess.
// It also scopes the monitor's pre-flight sweep: ListByTypeTag refuses to list
// without a project, and octavia monitor refuses to start without one.
func ProjectIDFromAuth(gc *gophercloud.ServiceClient) (string, bool) {
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
// one itemized error naming every quota that would be exceeded, or nil. A
// negative limit means unlimited (Octavia's default for every quota) and never
// blocks. Like Neutron's, it compares the plan with the limit, not with the
// headroom the project has left.
func checkQuota(need needs, q *quotas.Quota) error {
	var over []string
	check := func(name string, want, limit int) {
		if limit >= 0 && want > limit {
			over = append(over, fmt.Sprintf("%s need %d, quota %d", name, want, limit))
		}
	}
	check("loadbalancer", need.loadBalancers, q.Loadbalancer)
	check("listener", need.listeners, q.Listener)
	check("pool", need.pools, q.Pool)
	check("member", need.members, q.Member)
	check("healthmonitor", need.healthMonitors, q.Healthmonitor)

	if len(over) == 0 {
		return nil
	}
	return fmt.Errorf("plan exceeds project quota; raise these quotas before applying: %s", strings.Join(over, "; "))
}
