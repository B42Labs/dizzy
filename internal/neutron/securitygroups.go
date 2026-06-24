package neutron

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"

	"github.com/B42Labs/openstack-tester/internal/plan"
)

// CreateSecurityGroup creates a tagged security group. Its rules are created
// separately once every group exists, so remote-group references resolve.
func (c *Client) CreateSecurityGroup(ctx context.Context, sg plan.SecurityGroup) (Resource, error) {
	return c.createTagged(ctx, KindSecurityGroup, sg.Name, func(ctx context.Context, name string) (string, error) {
		created, err := groups.Create(ctx, c.gc, groups.CreateOpts{Name: name}).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}
