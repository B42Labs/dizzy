package octavia

// These are the calls octavia chaos makes while it churns: it deletes one
// load balancer, listener, pool, member, or health monitor at a time, and it
// toggles a listener's or member's admin_state_up. None of them cascades. A
// churn delete of a load balancer that still has a child is refused by
// Octavia, and the churn engine deletes it again later. Cleanup still deletes
// only through Delete, whose load balancer delete cascades. Each call is timed
// under <kind>/delete or <kind>/update and returns the gophercloud error
// unwrapped, so IsNotFound and IsRetryable classify it.

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"

	"github.com/B42Labs/dizzy/internal/resource"
)

// DeleteSingle deletes exactly r, never cascading: a load balancer, listener,
// pool, or health monitor. The call is recorded under <kind>/delete. A load
// balancer is deleted without the cascade query, so Octavia refuses it while a
// child is still attached. A member is deleted below its pool, which its
// Resource does not carry, so it is refused without a request in favor of
// DeleteMember; any other kind is refused without a request too. A 404
// surfaces unchanged, for the caller to treat as already gone.
func (c *Client) DeleteSingle(ctx context.Context, r resource.Resource) error {
	var del func(context.Context) error
	switch r.Kind {
	case KindLoadBalancer:
		del = func(ctx context.Context) error {
			return loadbalancers.Delete(ctx, c.lb, r.ID, loadbalancers.DeleteOpts{}).ExtractErr()
		}
	case KindListener:
		del = func(ctx context.Context) error { return listeners.Delete(ctx, c.lb, r.ID).ExtractErr() }
	case KindPool:
		del = func(ctx context.Context) error { return pools.Delete(ctx, c.lb, r.ID).ExtractErr() }
	case KindHealthMonitor:
		del = func(ctx context.Context) error { return monitors.Delete(ctx, c.lb, r.ID).ExtractErr() }
	case KindMember:
		return fmt.Errorf("deleting member %q needs its pool; use DeleteMember", r.Logical)
	default:
		return fmt.Errorf("delete single not supported for kind %q", r.Kind)
	}
	return c.timed(ctx, string(r.Kind), "delete", del)
}

// DeleteMember deletes the member r of the pool poolID, recording the call
// under member/delete. An empty poolID is refused without a request. A 404
// surfaces unchanged, as it does for DeleteSingle.
func (c *Client) DeleteMember(ctx context.Context, r resource.Resource, poolID string) error {
	if poolID == "" {
		return fmt.Errorf("deleting member %q: no pool id", r.Logical)
	}
	return c.timed(ctx, string(KindMember), "delete", func(ctx context.Context) error {
		return pools.DeleteMember(ctx, c.lb, poolID, r.ID).ExtractErr()
	})
}

// SetListenerAdminState sets the listener r's admin_state_up to up, recording
// the call under listener/update. The request body carries admin_state_up and
// nothing else. A provider that does not support the option makes Octavia
// answer 501, which surfaces as an error IsRetryable rejects.
func (c *Client) SetListenerAdminState(ctx context.Context, r resource.Resource, up bool) error {
	return c.timed(ctx, string(KindListener), "update", func(ctx context.Context) error {
		_, err := listeners.Update(ctx, c.lb, r.ID, listeners.UpdateOpts{AdminStateUp: &up}).Extract()
		return err
	})
}

// SetMemberAdminState sets the member r of the pool poolID to
// admin_state_up=up, recording the call under member/update. The request body
// carries admin_state_up and nothing else. An empty poolID is refused without
// a request.
func (c *Client) SetMemberAdminState(ctx context.Context, r resource.Resource, poolID string, up bool) error {
	if poolID == "" {
		return fmt.Errorf("updating member %q: no pool id", r.Logical)
	}
	return c.timed(ctx, string(KindMember), "update", func(ctx context.Context) error {
		_, err := pools.UpdateMember(ctx, c.lb, poolID, r.ID, pools.UpdateMemberOpts{AdminStateUp: &up}).Extract()
		return err
	})
}
