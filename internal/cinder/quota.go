package cinder

// Quota handling follows the same document-and-require policy as Neutron.
// PrecheckQuota reads the target project's Cinder quotas and aborts an oversized
// plan with an itemized message before any resource is created; raising the
// quotas is then the operator's step. It never auto-raises quotas, which would
// require admin credentials the tool otherwise never needs.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/quotasets"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"

	"github.com/B42Labs/openstack-tester/internal/cinder/plan"
)

// needs is the count of each quota-bounded resource an expanded plan requires:
// the volume and snapshot counts, and the total gigabytes (Σ final volume sizes
// + Σ snapshot sizes at the source volume's final size), since both volumes and
// snapshots draw from the shared gigabytes quota.
type needs struct {
	volumes   int
	snapshots int
	gigabytes int64
}

// planNeeds counts the quota-bounded resources a plan will create.
func planNeeds(p *plan.Plan) needs {
	return needs{
		volumes:   len(p.Volumes),
		snapshots: len(p.Snapshots),
		gigabytes: p.TotalGiB(),
	}
}

// PrecheckQuota reads the project's Cinder quotas and returns an itemized error
// if the plan would exceed the global (or, when volumeType is set, the per-type)
// volume, snapshot, or gigabytes quota, before any resource is created. It is
// fail-open only where the read cannot meaningfully proceed: if the project id
// cannot be derived from the auth result, or the quota read is denied (a common
// non-admin 403), it logs a warning and returns nil, leaving the executor's
// quota fast-fail as the backstop. Any other read failure is returned so the
// plan aborts before creating anything rather than hitting the real quota wall
// mid-apply.
func PrecheckQuota(ctx context.Context, gc *gophercloud.ServiceClient, p *plan.Plan, volumeType string) error {
	projectID, ok := projectIDFromAuth(gc)
	if !ok {
		slog.Warn("quota pre-check skipped: project id unavailable from auth result")
		return nil
	}
	quota, err := quotasets.Get(ctx, gc, projectID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, 403) {
			slog.Warn("quota pre-check skipped: quota read denied (non-admin?)", "error", err)
			return nil
		}
		return fmt.Errorf("reading project quotas for pre-check: %w", err)
	}
	return checkQuota(planNeeds(p), quota, volumeType)
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
// A negative limit means unlimited (Cinder's convention) and never blocks. When
// volumeType is set the per-type quotas (volumes_<type>, snapshots_<type>,
// gigabytes_<type>) are checked with the same numbers, since they can be tighter
// than the global ones; a per-type key that Cinder has not materialized (the
// operator never set it) is treated as unrestricted and skipped.
func checkQuota(need needs, q *quotasets.QuotaSet, volumeType string) error {
	var over []string
	check := func(name string, want, limit int64) {
		if limit >= 0 && want > limit {
			over = append(over, fmt.Sprintf("%s need %d, quota %d", name, want, limit))
		}
	}
	check("volumes", int64(need.volumes), int64(q.Volumes))
	check("snapshots", int64(need.snapshots), int64(q.Snapshots))
	check("gigabytes", need.gigabytes, int64(q.Gigabytes))

	if volumeType != "" {
		checkExtra := func(name, key string, want int64) {
			if limit, ok := extraLimit(q.Extra, key); ok {
				check(name, want, limit)
			}
		}
		checkExtra("volumes_"+volumeType, "volumes_"+volumeType, int64(need.volumes))
		checkExtra("snapshots_"+volumeType, "snapshots_"+volumeType, int64(need.snapshots))
		checkExtra("gigabytes_"+volumeType, "gigabytes_"+volumeType, need.gigabytes)
	}

	if len(over) == 0 {
		return nil
	}
	return fmt.Errorf("plan exceeds project quota; raise these quotas before applying: %s", strings.Join(over, "; "))
}

// extraLimit reads a per-type quota limit from the QuotaSet's Extra map, where
// values arrive as untyped JSON numbers (float64). It returns ok=false when the
// key is absent, so the caller can treat an unmaterialized per-type quota as
// unrestricted.
func extraLimit(extra map[string]any, key string) (int64, bool) {
	v, ok := extra[key]
	if !ok {
		return 0, false
	}
	n, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int64(n), true
}
