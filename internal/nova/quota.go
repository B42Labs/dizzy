package nova

// Quota handling follows the same document-and-require policy as Neutron and
// Cinder. PrecheckQuota reads the target project's compute quotas and current
// usage and aborts a plan that would not fit the remaining headroom with an
// itemized message before any resource is created; raising the quotas is then
// the operator's step. It covers only the compute quotas (instances, cores,
// RAM) the servers consume — the Cinder gigabytes and Neutron port/network
// quotas the companion resources consume are not pre-checked, so the executor's
// quota fast-fail is the backstop there. It never auto-raises quotas, which
// would require admin credentials the tool otherwise never needs.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/quotasets"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"

	"github.com/B42Labs/dizzy/internal/nova/plan"
)

// needs is the count of each compute quota-bounded resource an expanded plan
// requires: the instance count, the total vCPUs, and the total RAM in MB. A
// resized server counts, per dimension, the larger of its boot and resize
// flavor, since it holds both at the moment of the resize.
type needs struct {
	instances int64
	cores     int64
	ram       int64
}

// planNeeds counts the compute quota-bounded resources a plan will create. Each
// server is one instance sized by boot; a resized server is sized by the
// per-dimension maximum of the boot and resize flavors.
func planNeeds(p *plan.Plan, boot, resize Flavor) needs {
	n := needs{instances: int64(len(p.Servers))}
	for _, s := range p.Servers {
		cores, ram := boot.VCPUs, boot.RAM
		if s.Resize {
			cores = max(cores, resize.VCPUs)
			ram = max(ram, resize.RAM)
		}
		n.cores += int64(cores)
		n.ram += int64(ram)
	}
	return n
}

// PrecheckQuota reads the project's compute quotas and returns an itemized error
// if the plan would exceed the instances, cores, or RAM quota, before any
// resource is created. It is fail-open only where the read cannot meaningfully
// proceed: if the project id cannot be derived from the auth result, or the
// quota read is denied (a common non-admin 403), it logs a warning and returns
// nil, leaving the executor's quota fast-fail as the backstop. Any other read
// failure is returned so the plan aborts before creating anything rather than
// hitting the real quota wall mid-apply.
func PrecheckQuota(ctx context.Context, gc *gophercloud.ServiceClient, p *plan.Plan, boot, resize Flavor) error {
	projectID, ok := projectIDFromAuth(gc)
	if !ok {
		slog.Warn("quota pre-check skipped: project id unavailable from auth result")
		return nil
	}
	quota, err := quotasets.GetDetail(ctx, gc, projectID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, 403) {
			slog.Warn("quota pre-check skipped: quota read denied (non-admin?)", "error", err)
			return nil
		}
		return fmt.Errorf("reading project quotas for pre-check: %w", err)
	}
	return checkQuota(planNeeds(p, boot, resize), quota)
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

// checkQuota compares what a plan needs against the project's remaining compute
// quota — the limit less what is already in use or reserved — and returns one
// itemized error naming every dimension that would be exceeded, or nil. Reading
// the remaining quota rather than the raw limit is what lets a project with
// pre-existing usage be caught before apply rather than mid-apply. A negative
// limit means unlimited (the compute convention) and never blocks.
func checkQuota(need needs, q quotasets.QuotaDetailSet) error {
	var over []string
	check := func(name string, want int64, d quotasets.QuotaDetail) {
		if d.Limit < 0 {
			return // negative limit means unlimited
		}
		used := int64(d.InUse + d.Reserved)
		avail := int64(d.Limit) - used
		if want > avail {
			over = append(over, fmt.Sprintf("%s need %d, available %d (limit %d, used %d)", name, want, avail, d.Limit, used))
		}
	}
	check("instances", need.instances, q.Instances)
	check("cores", need.cores, q.Cores)
	check("ram (MB)", need.ram, q.RAM)

	if len(over) == 0 {
		return nil
	}
	return fmt.Errorf("plan exceeds project compute quota; raise these quotas before applying: %s", strings.Join(over, "; "))
}
