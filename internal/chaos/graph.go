// Package chaos runs a random churn/soak load against Neutron. Instead of
// building a topology once and stopping, it keeps creating and deleting
// resources at random, seeded intervals and parallelism for a configured
// duration, using a scenario plan as the spatial envelope: the live population
// never exceeds the plan's resource set, and only planned resources whose
// parents exist are ever created. The schedule of create/delete decisions is
// deterministic for a given seed and config, while the concurrent cloud-call
// completion order is not. Everything is tagged with the run id like apply, so a
// crashed or interrupted run is reclaimable by tag.
package chaos

import (
	"context"
	"fmt"

	"github.com/B42Labs/openstack-tester/internal/neutron"
	"github.com/B42Labs/openstack-tester/internal/plan"
)

// Neutron is the create/delete surface the chaos engine drives: the executor's
// per-kind create methods plus Delete and the per-interface
// RemoveRouterInterface. It is the consumer-defined ports-and-adapters seam to
// the cloud — *neutron.Client satisfies it in production and a fake satisfies it
// in tests. Like the executor's Neutron it is wide by necessity (one create per
// kind), mirroring the resource set rather than a behavior to abstract.
type Neutron interface {
	CreateAddressScope(ctx context.Context, as plan.AddressScope) (neutron.Resource, error)
	CreateSubnetPool(ctx context.Context, sp plan.SubnetPool, addressScopeID string) (neutron.Resource, error)
	CreateNetwork(ctx context.Context, n plan.Network) (neutron.Resource, error)
	CreateSubnet(ctx context.Context, s plan.Subnet, networkID, subnetPoolID string) (neutron.Resource, error)
	CreateRouter(ctx context.Context, r plan.Router, externalNetworkID string) (neutron.Resource, error)
	CreateRouterInterface(ctx context.Context, ri plan.RouterInterface, routerID, subnetID, portID string) (neutron.Resource, error)
	CreateSecurityGroup(ctx context.Context, sg plan.SecurityGroup) (neutron.Resource, error)
	CreateSecurityGroupRule(ctx context.Context, rule plan.SecurityGroupRule, sgID, remoteGroupID string) (neutron.Resource, error)
	CreatePort(ctx context.Context, p plan.Port, networkID string, subnetIDByLogical map[string]string, sgIDs []string) (neutron.Resource, error)
	CreateFloatingIP(ctx context.Context, fip plan.FloatingIP, externalNetworkID, portID string) (neutron.Resource, error)
	Delete(ctx context.Context, r neutron.Resource) error
	RemoveRouterInterface(ctx context.Context, routerID, subnetID, portID string) error
}

// Node is one planned resource in the churn graph: a unit the engine can create
// and later delete. Key uniquely identifies it; Parents lists the keys of the
// nodes whose creation it depends on (and which must outlive it). Create and
// Delete invoke the typed Neutron wrappers, resolving parent cloud ids from ids
// (keyed by parent key, which equals the parent's plan logical name).
type Node struct {
	Key     string
	Kind    neutron.Kind
	Parents []string
	Create  func(ctx context.Context, n Neutron, ids map[string]string) (neutron.Resource, error)
	Delete  func(ctx context.Context, n Neutron, ids map[string]string, res neutron.Resource) error
}

// Build turns a plan into the churn graph: every plan resource becomes exactly
// one node, with Parents computed exactly as executor.Apply resolves references,
// so the engine never schedules a create before its parents exist or a delete
// before its dependents are gone. externalNetworkID mirrors apply's external
// handling: when empty, floating IPs are omitted (they require an external
// network) and routers are created without a gateway. The plan is validated
// first so a dangling reference fails loudly instead of yielding a node that can
// never be created.
func Build(p *plan.Plan, externalNetworkID string) ([]Node, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid plan: %w", err)
	}

	var nodes []Node

	for _, as := range p.AddressScopes {
		nodes = append(nodes, Node{
			Key: as.Name, Kind: neutron.KindAddressScope,
			Create: func(ctx context.Context, n Neutron, _ map[string]string) (neutron.Resource, error) {
				return n.CreateAddressScope(ctx, as)
			},
			Delete: deleteByID,
		})
	}

	for _, sp := range p.SubnetPools {
		nodes = append(nodes, Node{
			Key: sp.Name, Kind: neutron.KindSubnetPool,
			Parents: nonEmpty(sp.AddressScope),
			Create: func(ctx context.Context, n Neutron, ids map[string]string) (neutron.Resource, error) {
				return n.CreateSubnetPool(ctx, sp, ids[sp.AddressScope])
			},
			Delete: deleteByID,
		})
	}

	for _, nw := range p.Networks {
		nodes = append(nodes, Node{
			Key: nw.Name, Kind: neutron.KindNetwork,
			Create: func(ctx context.Context, n Neutron, _ map[string]string) (neutron.Resource, error) {
				return n.CreateNetwork(ctx, nw)
			},
			Delete: deleteByID,
		})
	}

	for _, r := range p.Routers {
		nodes = append(nodes, Node{
			Key: r.Name, Kind: neutron.KindRouter,
			Create: func(ctx context.Context, n Neutron, _ map[string]string) (neutron.Resource, error) {
				return n.CreateRouter(ctx, r, externalNetworkID)
			},
			Delete: deleteByID,
		})
	}

	for _, sg := range p.SecurityGroups {
		nodes = append(nodes, Node{
			Key: sg.Name, Kind: neutron.KindSecurityGroup,
			Create: func(ctx context.Context, n Neutron, _ map[string]string) (neutron.Resource, error) {
				return n.CreateSecurityGroup(ctx, sg)
			},
			Delete: deleteByID,
		})
	}

	for _, s := range p.Subnets {
		parents := []string{s.Network}
		if s.SubnetPool != "" {
			parents = append(parents, s.SubnetPool)
		}
		nodes = append(nodes, Node{
			Key: s.Name, Kind: neutron.KindSubnet, Parents: parents,
			Create: func(ctx context.Context, n Neutron, ids map[string]string) (neutron.Resource, error) {
				return n.CreateSubnet(ctx, s, ids[s.Network], ids[s.SubnetPool])
			},
			Delete: deleteByID,
		})
	}

	for _, sg := range p.SecurityGroups {
		for j, rule := range sg.Rules {
			parents := []string{sg.Name}
			if rule.RemoteGroup != "" {
				parents = append(parents, rule.RemoteGroup)
			}
			key := fmt.Sprintf("rule:%s:%d", sg.Name, j)
			sgName := sg.Name
			nodes = append(nodes, Node{
				Key: key, Kind: neutron.KindSecurityGroupRule, Parents: dedup(parents),
				Create: func(ctx context.Context, n Neutron, ids map[string]string) (neutron.Resource, error) {
					var remoteID string
					if rule.RemoteGroup != "" {
						remoteID = ids[rule.RemoteGroup]
					}
					return n.CreateSecurityGroupRule(ctx, rule, ids[sgName], remoteID)
				},
				Delete: deleteByID,
			})
		}
	}

	for _, pt := range p.Ports {
		parents := []string{pt.Network}
		for _, fip := range pt.FixedIPs {
			parents = append(parents, fip.Subnet)
		}
		parents = append(parents, pt.SecurityGroups...)
		nodes = append(nodes, Node{
			Key: pt.Name, Kind: neutron.KindPort, Parents: dedup(parents),
			Create: func(ctx context.Context, n Neutron, ids map[string]string) (neutron.Resource, error) {
				sgIDs := make([]string, 0, len(pt.SecurityGroups))
				for _, sg := range pt.SecurityGroups {
					sgIDs = append(sgIDs, ids[sg])
				}
				subnetIDs := make(map[string]string, len(pt.FixedIPs))
				for _, fip := range pt.FixedIPs {
					subnetIDs[fip.Subnet] = ids[fip.Subnet]
				}
				return n.CreatePort(ctx, pt, ids[pt.Network], subnetIDs, sgIDs)
			},
			Delete: deleteByID,
		})
	}

	for _, ri := range p.RouterInterfaces {
		parents := []string{ri.Router}
		if ri.Subnet != "" {
			parents = append(parents, ri.Subnet)
		} else {
			parents = append(parents, ri.Port)
		}
		nodes = append(nodes, Node{
			Key: ri.Name, Kind: neutron.KindRouterInterface, Parents: parents,
			Create: func(ctx context.Context, n Neutron, ids map[string]string) (neutron.Resource, error) {
				return n.CreateRouterInterface(ctx, ri, ids[ri.Router], ids[ri.Subnet], ids[ri.Port])
			},
			Delete: func(ctx context.Context, n Neutron, ids map[string]string, _ neutron.Resource) error {
				return n.RemoveRouterInterface(ctx, ids[ri.Router], ids[ri.Subnet], ids[ri.Port])
			},
		})
	}

	// Floating IPs only exist when an external network was discovered, mirroring
	// apply: with no external network they are skipped entirely.
	if externalNetworkID != "" {
		for _, fip := range p.FloatingIPs {
			nodes = append(nodes, Node{
				Key: fip.Name, Kind: neutron.KindFloatingIP,
				Parents: nonEmpty(fip.Port),
				Create: func(ctx context.Context, n Neutron, ids map[string]string) (neutron.Resource, error) {
					return n.CreateFloatingIP(ctx, fip, externalNetworkID, ids[fip.Port])
				},
				Delete: deleteByID,
			})
		}
	}

	return nodes, nil
}

// deleteByID is the delete closure shared by every kind the generic Delete
// covers (all kinds except router interfaces). It ignores ids because the
// resource carries its own cloud id.
func deleteByID(ctx context.Context, n Neutron, _ map[string]string, res neutron.Resource) error {
	return n.Delete(ctx, res)
}

// nonEmpty returns a single-element slice for a non-empty reference, or nil for
// an empty one — the parent list for the optional-single-parent kinds.
func nonEmpty(ref string) []string {
	if ref == "" {
		return nil
	}
	return []string{ref}
}

// dedup returns refs with duplicates removed, preserving first-seen order. A
// port may list the same subnet or security group more than once; collapsing
// them keeps the parent list (and its reverse edges) a clean set.
func dedup(refs []string) []string {
	seen := make(map[string]bool, len(refs))
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}
