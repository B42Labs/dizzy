package barbican

// Quota handling follows the same document-and-require policy as Cinder.
// PrecheckQuota reads the project's effective Barbican quotas and aborts an
// oversized plan with an itemized message before any resource is created;
// raising the quotas is then the operator's step. It never auto-raises quotas,
// which would require admin credentials the tool otherwise never needs.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gophercloud/gophercloud/v2"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
)

// quotaSet is the part of Barbican's quota answer the pre-check reads. Each
// limit is a pointer so a key the answer leaves out (or sets to null) is told
// apart from a limit of 0 and left unchecked. A negative limit means unlimited,
// which is Barbican's default for every quota.
type quotaSet struct {
	Secrets    *int `json:"secrets"`
	Orders     *int `json:"orders"`
	Containers *int `json:"containers"`
}

// needs is the count of each quota-bounded resource a plan creates. Every
// order that reaches ACTIVE generates a secret counted by the same secrets
// quota as a stored one, so secrets covers both.
type needs struct {
	secrets    int
	orders     int
	containers int
}

// planNeeds counts the quota-bounded resources a plan will create.
func planNeeds(p *barbicanplan.Plan) needs {
	return needs{
		secrets:    len(p.Secrets) + len(p.Orders),
		orders:     len(p.Orders),
		containers: len(p.Containers),
	}
}

// PrecheckQuota reads the project's Barbican quotas and returns an itemized
// error if the plan would exceed the secrets, orders, or containers quota,
// before any resource is created. GET /v1/quotas answers the token project's
// effective quotas, so no project id is derived. gophercloud has no binding for
// it, so it is a raw GET, and it is not recorded in the metrics. A denied read
// (a 403) logs a warning and returns nil, leaving the executor's quota fast-fail
// as the backstop. Any other read failure, including an answer that is not JSON
// or carries no quotas object, is returned so the plan aborts before creating
// anything rather than hitting the real quota wall mid-apply.
func PrecheckQuota(ctx context.Context, kv *gophercloud.ServiceClient, p *barbicanplan.Plan) error {
	var body struct {
		Quotas *quotaSet `json:"quotas"`
	}
	_, err := kv.Get(ctx, kv.ServiceURL("quotas"), &body, &gophercloud.RequestOpts{OkCodes: []int{200}})
	if err != nil {
		if gophercloud.ResponseCodeIs(err, 403) {
			slog.Warn("quota pre-check skipped: quota read denied", "error", err)
			return nil
		}
		return fmt.Errorf("reading Barbican quotas for pre-check: %w", err)
	}
	if body.Quotas == nil {
		return errors.New("reading Barbican quotas for pre-check: response carries no quotas object")
	}
	return checkQuota(planNeeds(p), *body.Quotas)
}

// checkQuota compares what a plan needs against the project quota and returns
// one itemized error naming every quota that would be exceeded, or nil. A key
// the quota answer left out is unchecked, and a negative limit means unlimited
// and never blocks. Like Cinder's, it compares the plan with the limit, not
// with the headroom the project has left.
func checkQuota(need needs, q quotaSet) error {
	var over []string
	check := func(name string, want int, limit *int) {
		if limit != nil && *limit >= 0 && want > *limit {
			over = append(over, fmt.Sprintf("%s need %d, quota %d", name, want, *limit))
		}
	}
	check("secrets", need.secrets, q.Secrets)
	check("orders", need.orders, q.Orders)
	check("containers", need.containers, q.Containers)

	if len(over) == 0 {
		return nil
	}
	return fmt.Errorf("plan exceeds project quota; raise these quotas before applying: %s", strings.Join(over, "; "))
}
