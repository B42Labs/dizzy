package neutron

import (
	"context"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"

	"github.com/B42Labs/openstack-tester/internal/plan"
)

// CreateSubnet creates a tagged subnet on networkID. The plan sets exactly one
// allocation source: an explicit CIDR, or a subnet pool together with a prefix
// length (subnetPoolID is empty for the explicit-CIDR case). The IPv6 mode
// fields are sent only for IPv6 subnets, where the plan populates them.
func (c *Client) CreateSubnet(ctx context.Context, s plan.Subnet, networkID, subnetPoolID string) (Resource, error) {
	return c.createTagged(ctx, KindSubnet, s.Name, func(ctx context.Context, name string) (string, error) {
		opts := subnets.CreateOpts{
			NetworkID:       networkID,
			Name:            name,
			IPVersion:       gophercloud.IPVersion(s.IPVersion),
			CIDR:            s.CIDR,
			SubnetPoolID:    subnetPoolID,
			Prefixlen:       s.PrefixLen,
			IPv6AddressMode: s.IPv6AddressMode,
			IPv6RAMode:      s.IPv6RAMode,
		}
		created, err := subnets.Create(ctx, c.gc, opts).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}
