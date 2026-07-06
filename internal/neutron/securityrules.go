package neutron

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/rules"

	"github.com/B42Labs/dizzy/internal/plan"
)

// CreateSecurityGroupRule creates a rule in the security group identified by
// sgID. remoteGroupID is the resolved id of the rule's remote group, or empty
// when the rule uses a remote CIDR instead. The plan's direction, ether type,
// and protocol strings already use Neutron's vocabulary, so they are cast to
// the gophercloud typed aliases directly. Rules are sub-resources of an
// already-tagged group and so are not separately tagged.
func (c *Client) CreateSecurityGroupRule(ctx context.Context, rule plan.SecurityGroupRule, sgID, remoteGroupID string) (Resource, error) {
	var id string
	err := c.timed(ctx, string(KindSecurityGroupRule), "create", func(ctx context.Context) error {
		created, createErr := rules.Create(ctx, c.gc, rules.CreateOpts{
			SecGroupID:     sgID,
			Direction:      rules.RuleDirection(rule.Direction),
			EtherType:      rules.RuleEtherType(rule.EtherType),
			Protocol:       rules.RuleProtocol(rule.Protocol),
			PortRangeMin:   rule.PortRangeMin,
			PortRangeMax:   rule.PortRangeMax,
			RemoteGroupID:  remoteGroupID,
			RemoteIPPrefix: rule.RemoteIPPrefix,
		}).Extract()
		if createErr != nil {
			return createErr
		}
		id = created.ID
		return nil
	})
	if err != nil {
		return Resource{}, wrapCreate(KindSecurityGroupRule, sgID, err)
	}
	return Resource{Kind: KindSecurityGroupRule, ID: id}, nil
}
