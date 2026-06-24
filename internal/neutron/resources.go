package neutron

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/addressscopes"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/rules"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/subnetpools"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
)

// Get fetches a resource and returns its status, recording the call. The status
// is empty for kinds that do not report one. It is used to poll readiness and
// by cleanup to confirm a resource exists.
func (c *Client) Get(ctx context.Context, r Resource) (string, error) {
	var status string
	err := c.timed(ctx, string(r.Kind), func(ctx context.Context) error {
		var getErr error
		status, getErr = c.status(ctx, r)
		return getErr
	})
	return status, err
}

// status fetches a resource and returns its status without recording a sample.
// WaitForReady polls through this so its repeated gets do not flood the
// per-call latency stats; the time-to-ready record stands in for them instead.
func (c *Client) status(ctx context.Context, r Resource) (string, error) {
	switch r.Kind {
	case KindNetwork:
		n, err := networks.Get(ctx, c.gc, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return n.Status, nil
	case KindRouter:
		ro, err := routers.Get(ctx, c.gc, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return ro.Status, nil
	case KindPort:
		p, err := ports.Get(ctx, c.gc, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return p.Status, nil
	default:
		return "", fmt.Errorf("get not supported for kind %q", r.Kind)
	}
}

// Observe re-queries the live state of a created resource, recording the call.
// It returns the resource's status (empty for kinds that do not report one),
// whether the resource still exists, and any error other than a 404. A 404 is
// reported as ("", false, nil) so a resource deleted out of band reads as gone
// rather than as a failure. The status command drives this over a run's
// resources.
func (c *Client) Observe(ctx context.Context, r Resource) (status string, exists bool, err error) {
	err = c.timed(ctx, string(r.Kind), func(ctx context.Context) error {
		s, getErr := c.observe(ctx, r)
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

// observe fetches a resource and returns its status (empty for kinds without
// one) without recording a sample; Observe wraps it through c.timed. Unlike
// status it covers every kind so a run's full resource set can be re-queried.
// Router interfaces are observed through the port id stored at attach time.
func (c *Client) observe(ctx context.Context, r Resource) (string, error) {
	switch r.Kind {
	case KindNetwork:
		n, err := networks.Get(ctx, c.gc, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return n.Status, nil
	case KindRouter:
		ro, err := routers.Get(ctx, c.gc, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return ro.Status, nil
	case KindPort, KindRouterInterface:
		p, err := ports.Get(ctx, c.gc, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return p.Status, nil
	case KindSubnet:
		_, err := subnets.Get(ctx, c.gc, r.ID).Extract()
		return "", err
	case KindSubnetPool:
		_, err := subnetpools.Get(ctx, c.gc, r.ID).Extract()
		return "", err
	case KindSecurityGroup:
		_, err := groups.Get(ctx, c.gc, r.ID).Extract()
		return "", err
	case KindSecurityGroupRule:
		_, err := rules.Get(ctx, c.gc, r.ID).Extract()
		return "", err
	case KindAddressScope:
		_, err := addressscopes.Get(ctx, c.gc, r.ID).Extract()
		return "", err
	case KindFloatingIP:
		fip, err := floatingips.Get(ctx, c.gc, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return fip.Status, nil
	default:
		return "", fmt.Errorf("observe not supported for kind %q", r.Kind)
	}
}

// Delete removes a resource, recording the call. Router interfaces cannot be
// deleted by id (they are detached with RemoveInterface), so Delete rejects
// that kind; the executor never calls it and a later cleanup detaches them
// explicitly.
func (c *Client) Delete(ctx context.Context, r Resource) error {
	return c.timed(ctx, string(r.Kind), func(ctx context.Context) error {
		switch r.Kind {
		case KindNetwork:
			return networks.Delete(ctx, c.gc, r.ID).ExtractErr()
		case KindSubnet:
			return subnets.Delete(ctx, c.gc, r.ID).ExtractErr()
		case KindRouter:
			return routers.Delete(ctx, c.gc, r.ID).ExtractErr()
		case KindPort:
			return ports.Delete(ctx, c.gc, r.ID).ExtractErr()
		case KindSubnetPool:
			return subnetpools.Delete(ctx, c.gc, r.ID).ExtractErr()
		case KindSecurityGroup:
			return groups.Delete(ctx, c.gc, r.ID).ExtractErr()
		case KindSecurityGroupRule:
			return rules.Delete(ctx, c.gc, r.ID).ExtractErr()
		case KindAddressScope:
			return addressscopes.Delete(ctx, c.gc, r.ID).ExtractErr()
		case KindFloatingIP:
			return floatingips.Delete(ctx, c.gc, r.ID).ExtractErr()
		default:
			return fmt.Errorf("delete not supported for kind %q", r.Kind)
		}
	})
}
