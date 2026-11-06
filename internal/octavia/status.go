package octavia

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"

	"github.com/B42Labs/dizzy/internal/resource"
)

// ListByTag returns the resources of kind carrying this run's dizzy:run=<runID>
// tag, the discovery step tag-based cleanup deletes from. It covers load
// balancers, whose cascade delete reaches every listener, pool, member, and
// health monitor, and the VIP networks and subnets; other kinds are refused
// without a request. The list is filtered by the tag server side and again
// client side, so a server that ignores the tags query never widens the result
// beyond what the tool tagged. The call is timed under <kind>/list.
func (c *Client) ListByTag(ctx context.Context, kind resource.Kind, runID string) ([]resource.Resource, error) {
	return c.listByTag(ctx, kind, "", runTag(runID))
}

// ListByTypeTag returns the resources of kind carrying dizzy:type=<kind> in the
// token's project, whatever run tagged them: the discovery step of the monitor's
// pre-flight sweep, which must reclaim leftovers of a crashed iteration whose
// run id it no longer holds. Octavia and Neutron list every project's objects
// for an administrator, so the listing is filtered to the project of the auth
// result server side and again client side, and it is refused with
// ErrNoProject when the auth result names no project. It covers the kinds
// ListByTag covers and is timed under <kind>/list.
func (c *Client) ListByTypeTag(ctx context.Context, kind resource.Kind) ([]resource.Resource, error) {
	projectID, ok := ProjectIDFromAuth(c.lb)
	if !ok {
		return nil, fmt.Errorf("listing %s by type tag: %w", kind, ErrNoProject)
	}
	return c.listByTag(ctx, kind, projectID, metaType+"="+string(kind))
}

// listByTag is the shared timed body behind ListByTag and ListByTypeTag: it
// lists kind filtered to tag and, when projectID is not empty, to that project,
// both server side and again client side. An empty projectID sends no
// project_id query and checks no project.
func (c *Client) listByTag(ctx context.Context, kind resource.Kind, projectID, tag string) ([]resource.Resource, error) {
	var list func(ctx context.Context) ([]resource.Resource, error)
	switch kind {
	case KindLoadBalancer:
		list = func(ctx context.Context) ([]resource.Resource, error) {
			return listTagged(ctx, kind, loadbalancers.List(c.lb, loadbalancers.ListOpts{ProjectID: projectID, Tags: []string{tag}}),
				loadbalancers.ExtractLoadBalancers, func(it loadbalancers.LoadBalancer) (string, string, string, []string) {
					return it.Name, it.ID, it.ProjectID, it.Tags
				}, tag, projectID)
		}
	case KindNetwork:
		list = func(ctx context.Context) ([]resource.Resource, error) {
			return listTagged(ctx, kind, networks.List(c.network, networks.ListOpts{ProjectID: projectID, Tags: tag}),
				networks.ExtractNetworks, func(it networks.Network) (string, string, string, []string) {
					return it.Name, it.ID, it.ProjectID, it.Tags
				}, tag, projectID)
		}
	case KindSubnet:
		list = func(ctx context.Context) ([]resource.Resource, error) {
			return listTagged(ctx, kind, subnets.List(c.network, subnets.ListOpts{ProjectID: projectID, Tags: tag}),
				subnets.ExtractSubnets, func(it subnets.Subnet) (string, string, string, []string) {
					return it.Name, it.ID, it.ProjectID, it.Tags
				}, tag, projectID)
		}
	default:
		return nil, fmt.Errorf("list by tag not supported for kind %q", kind)
	}

	var found []resource.Resource
	err := c.timed(ctx, string(kind), "list", func(ctx context.Context) error {
		var listErr error
		found, listErr = list(ctx)
		return listErr
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s by tag: %w", kind, err)
	}
	return found, nil
}

// Delete removes a resource, recording the call under <kind>/delete. A load
// balancer is deleted with cascade, which removes its listeners, pools,
// members, and health monitors with it; a VIP subnet or network is deleted by
// id. Octavia's children are never deleted on their own, so any other kind is
// refused without a request. A 404 surfaces to the caller, which treats an
// already-gone resource as success to keep cleanup idempotent.
func (c *Client) Delete(ctx context.Context, r resource.Resource) error {
	var del func(context.Context) error
	switch r.Kind {
	case KindLoadBalancer:
		del = func(ctx context.Context) error {
			return loadbalancers.Delete(ctx, c.lb, r.ID, loadbalancers.DeleteOpts{Cascade: true}).ExtractErr()
		}
	case KindSubnet:
		del = func(ctx context.Context) error { return subnets.Delete(ctx, c.network, r.ID).ExtractErr() }
	case KindNetwork:
		del = func(ctx context.Context) error { return networks.Delete(ctx, c.network, r.ID).ExtractErr() }
	default:
		return fmt.Errorf("delete not supported for kind %q", r.Kind)
	}
	return c.timed(ctx, string(r.Kind), "delete", del)
}

// Observe re-queries the live state of a created resource, recording the call
// under <kind>/get. It returns the provisioning status of a load balancer,
// listener, pool, or health monitor, the status of a VIP network, and an empty
// status for a subnet, which reports none; whether the resource still exists;
// and any error other than a 404. A 404 is reported as ("", false, nil) so a
// resource deleted out of band reads as gone rather than as a failure. A member
// is read below its pool, which its Resource does not carry, so it is refused
// without a request in favor of ObserveMember. The status command drives this
// over a run's resources.
func (c *Client) Observe(ctx context.Context, r resource.Resource) (status string, exists bool, err error) {
	var get func(context.Context) (string, error)
	switch r.Kind {
	case KindLoadBalancer:
		get = func(ctx context.Context) (string, error) {
			lb, err := loadbalancers.Get(ctx, c.lb, r.ID).Extract()
			if err != nil {
				return "", err
			}
			return lb.ProvisioningStatus, nil
		}
	case KindListener:
		get = func(ctx context.Context) (string, error) {
			l, err := listeners.Get(ctx, c.lb, r.ID).Extract()
			if err != nil {
				return "", err
			}
			return l.ProvisioningStatus, nil
		}
	case KindPool:
		get = func(ctx context.Context) (string, error) {
			p, err := pools.Get(ctx, c.lb, r.ID).Extract()
			if err != nil {
				return "", err
			}
			return p.ProvisioningStatus, nil
		}
	case KindHealthMonitor:
		get = func(ctx context.Context) (string, error) {
			m, err := monitors.Get(ctx, c.lb, r.ID).Extract()
			if err != nil {
				return "", err
			}
			return m.ProvisioningStatus, nil
		}
	case KindNetwork:
		get = func(ctx context.Context) (string, error) {
			n, err := networks.Get(ctx, c.network, r.ID).Extract()
			if err != nil {
				return "", err
			}
			return n.Status, nil
		}
	case KindSubnet:
		get = func(ctx context.Context) (string, error) {
			_, err := subnets.Get(ctx, c.network, r.ID).Extract()
			return "", err
		}
	case KindMember:
		return "", false, fmt.Errorf("observing member %q needs its pool; use ObserveMember", r.Logical)
	default:
		return "", false, fmt.Errorf("observe not supported for kind %q", r.Kind)
	}
	return c.observe(ctx, r.Kind, get)
}

// ObserveMember re-queries the live state of the created member r of the pool
// poolID, as Observe does for the other kinds: it returns the member's
// provisioning status, whether it still exists, and any error other than a
// 404, recording the call under member/get.
func (c *Client) ObserveMember(ctx context.Context, r resource.Resource, poolID string) (status string, exists bool, err error) {
	return c.observe(ctx, KindMember, func(ctx context.Context) (string, error) {
		m, err := pools.GetMember(ctx, c.lb, poolID, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return m.ProvisioningStatus, nil
	})
}

// observe runs get timed under <kind>/get and folds a 404 into ("", false,
// nil), the shared body of Observe and ObserveMember.
func (c *Client) observe(ctx context.Context, kind resource.Kind, get func(context.Context) (string, error)) (status string, exists bool, err error) {
	err = c.timed(ctx, string(kind), "get", func(ctx context.Context) error {
		s, getErr := get(ctx)
		if getErr != nil {
			return getErr
		}
		status = s
		return nil
	})
	switch {
	case IsNotFound(err):
		return "", false, nil
	case err != nil:
		return "", false, err
	default:
		return status, true, nil
	}
}
