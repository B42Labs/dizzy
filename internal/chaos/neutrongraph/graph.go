// Package neutrongraph builds the Neutron churn graph the service-neutral chaos
// engine schedules. It turns a Neutron plan into engine nodes whose create and
// delete closures capture a Neutron client, resolve parent cloud ids exactly as
// executor.Apply does, and run through the same transient/conflict/quota retry
// policy the apply path uses. Keeping the Neutron coupling here leaves the chaos
// engine free of any service-specific import.
package neutrongraph

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/executor"
	"github.com/B42Labs/dizzy/internal/neutron"
	"github.com/B42Labs/dizzy/internal/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Neutron is the create/delete surface the chaos engine drives through the
// nodes this package builds: the executor's per-kind create methods plus Delete
// and the per-interface RemoveRouterInterface. It is the consumer-defined
// ports-and-adapters seam to the cloud — *neutron.Client satisfies it in
// production and a fake satisfies it in tests. Like the executor's Neutron it is
// wide by necessity (one create per kind), mirroring the resource set rather
// than a behavior to abstract.
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

// Build turns a plan into the churn graph: every plan resource becomes exactly
// one node, with Parents computed exactly as executor.Apply resolves references,
// so the engine never schedules a create before its parents exist or a delete
// before its dependents are gone. externalNetworkID mirrors apply's external
// handling: when empty, floating IPs are omitted (they require an external
// network) and routers are created without a gateway. Every node's closures
// capture n and run through executor.WithRetry, bounded by opTimeout. The plan
// is validated first so a dangling reference fails loudly instead of yielding a
// node that can never be created.
func Build(p *plan.Plan, externalNetworkID string, n Neutron, opTimeout time.Duration) ([]chaos.Node, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid plan: %w", err)
	}

	// deleteByID is the delete closure shared by every kind the generic Delete
	// covers (all kinds except router interfaces). It ignores ids because the
	// resource carries its own cloud id.
	deleteByID := retryDelete(opTimeout, func(ctx context.Context, _ map[string]string, res resource.Resource) error {
		return n.Delete(ctx, res)
	})

	var nodes []chaos.Node

	for _, as := range p.AddressScopes {
		nodes = append(nodes, chaos.Node{
			Key: as.Name, Kind: neutron.KindAddressScope,
			Create: retryCreate(opTimeout, func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
				return n.CreateAddressScope(ctx, as)
			}),
			Delete: deleteByID,
		})
	}

	for _, sp := range p.SubnetPools {
		nodes = append(nodes, chaos.Node{
			Key: sp.Name, Kind: neutron.KindSubnetPool,
			Parents: nonEmpty(sp.AddressScope),
			Create: retryCreate(opTimeout, func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return n.CreateSubnetPool(ctx, sp, ids[sp.AddressScope])
			}),
			Delete: deleteByID,
		})
	}

	for _, nw := range p.Networks {
		nodes = append(nodes, chaos.Node{
			Key: nw.Name, Kind: neutron.KindNetwork,
			Create: retryCreate(opTimeout, func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
				return n.CreateNetwork(ctx, nw)
			}),
			Delete: deleteByID,
		})
	}

	for _, r := range p.Routers {
		nodes = append(nodes, chaos.Node{
			Key: r.Name, Kind: neutron.KindRouter,
			Create: retryCreate(opTimeout, func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
				return n.CreateRouter(ctx, r, externalNetworkID)
			}),
			Delete: deleteByID,
		})
	}

	for _, sg := range p.SecurityGroups {
		nodes = append(nodes, chaos.Node{
			Key: sg.Name, Kind: neutron.KindSecurityGroup,
			Create: retryCreate(opTimeout, func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
				return n.CreateSecurityGroup(ctx, sg)
			}),
			Delete: deleteByID,
		})
	}

	for _, s := range p.Subnets {
		parents := []string{s.Network}
		if s.SubnetPool != "" {
			parents = append(parents, s.SubnetPool)
		}
		nodes = append(nodes, chaos.Node{
			Key: s.Name, Kind: neutron.KindSubnet, Parents: parents,
			Create: retryCreate(opTimeout, func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return n.CreateSubnet(ctx, s, ids[s.Network], ids[s.SubnetPool])
			}),
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
			nodes = append(nodes, chaos.Node{
				Key: key, Kind: neutron.KindSecurityGroupRule, Parents: dedup(parents),
				Create: retryCreate(opTimeout, func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
					var remoteID string
					if rule.RemoteGroup != "" {
						remoteID = ids[rule.RemoteGroup]
					}
					return n.CreateSecurityGroupRule(ctx, rule, ids[sgName], remoteID)
				}),
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
		nodes = append(nodes, chaos.Node{
			Key: pt.Name, Kind: neutron.KindPort, Parents: dedup(parents),
			Create: retryCreate(opTimeout, func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				sgIDs := make([]string, 0, len(pt.SecurityGroups))
				for _, sg := range pt.SecurityGroups {
					sgIDs = append(sgIDs, ids[sg])
				}
				subnetIDs := make(map[string]string, len(pt.FixedIPs))
				for _, fip := range pt.FixedIPs {
					subnetIDs[fip.Subnet] = ids[fip.Subnet]
				}
				return n.CreatePort(ctx, pt, ids[pt.Network], subnetIDs, sgIDs)
			}),
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
		nodes = append(nodes, chaos.Node{
			Key: ri.Name, Kind: neutron.KindRouterInterface, Parents: parents,
			Create: retryCreate(opTimeout, func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return n.CreateRouterInterface(ctx, ri, ids[ri.Router], ids[ri.Subnet], ids[ri.Port])
			}),
			Delete: retryDelete(opTimeout, func(ctx context.Context, ids map[string]string, _ resource.Resource) error {
				return n.RemoveRouterInterface(ctx, ids[ri.Router], ids[ri.Subnet], ids[ri.Port])
			}),
		})
	}

	// Floating IPs only exist when an external network was discovered, mirroring
	// apply: with no external network they are skipped entirely.
	if externalNetworkID != "" {
		for _, fip := range p.FloatingIPs {
			nodes = append(nodes, chaos.Node{
				Key: fip.Name, Kind: neutron.KindFloatingIP,
				Parents: nonEmpty(fip.Port),
				Create: retryCreate(opTimeout, func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
					return n.CreateFloatingIP(ctx, fip, externalNetworkID, ids[fip.Port])
				}),
				Delete: deleteByID,
			})
		}
	}

	return nodes, nil
}

// retryCreate wraps a raw create in the apply executor's retry policy, so the
// churn engine drives creates through the same transient/conflict/quota backoff
// the apply path uses. It returns the resource the create yielded (zero on
// failure) with the (possibly retried-out) error.
func retryCreate(opTimeout time.Duration, fn func(ctx context.Context, ids map[string]string) (resource.Resource, error)) func(context.Context, map[string]string) (resource.Resource, error) {
	return func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
		var res resource.Resource
		err := executor.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
			r, createErr := fn(ctx, ids)
			if createErr != nil {
				return createErr
			}
			res = r
			return nil
		})
		return res, err
	}
}

// retryDelete wraps a raw delete in the same retry policy and folds a 404 into
// success, so a resource already gone is an idempotent delete rather than a
// failure — the tolerance the engine used to apply itself, now local to the
// builder.
func retryDelete(opTimeout time.Duration, fn func(ctx context.Context, ids map[string]string, res resource.Resource) error) func(context.Context, map[string]string, resource.Resource) error {
	return func(ctx context.Context, ids map[string]string, res resource.Resource) error {
		err := executor.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
			return fn(ctx, ids, res)
		})
		if err != nil && neutron.IsNotFound(err) {
			return nil // a resource already gone is a successful delete (idempotent)
		}
		return err
	}
}

// Classify labels an operation error for the churn engine's per-bucket error
// breakdown, reusing the neutron classification helpers so the labels match the
// kinds operators already see in the metrics report. It is wired into the engine
// via chaos.Config.Classify.
func Classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, neutron.ErrQuota):
		return "quota"
	case neutron.IsNotFound(err):
		return "not-found"
	case neutron.IsConflict(err):
		return "conflict"
	case neutron.IsRetryable(err):
		return "transient"
	default:
		return "other"
	}
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
