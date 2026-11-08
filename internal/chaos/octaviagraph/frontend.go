package octaviagraph

import (
	"context"
	"fmt"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/octavia"
	octaviaexec "github.com/B42Labs/dizzy/internal/octavia/executor"
	"github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// FrontendOctavia is the surface the nodes of a frontend drive: the churn
// surface of Octavia plus the two Neutron lookups a frontend's creates need,
// since the network its load balancers sit on and the servers its members
// stand for belong to another graph. *octavia.Client satisfies it in
// production and a fake satisfies it in tests.
type FrontendOctavia interface {
	Octavia
	NetworkSubnet(ctx context.Context, networkID string) (string, error)
	ServerAddress(ctx context.Context, serverID, networkID string) (address, subnetID string, err error)
}

// The production *octavia.Client must satisfy the seam.
var _ FrontendOctavia = (*octavia.Client)(nil)

// BuildFrontend turns the Octavia frontend f into nodes that join another
// graph, the rolling graph of the Gardener persona, which carries the nodes
// they name as parents: a load balancer's network, and a member's server and
// that network. A nil f, or one with every collection empty, yields no node.
// Otherwise f is validated first, and every member's server must have a
// rolling set in roll, which maps a node key of the other graph to its Roll
// value. It emits, one collection after another and in plan order, a node per
// load balancer parented on its network, a node per listener, pool, and
// health monitor with the parents of its Build counterpart, and a node per
// member parented on its pool, its load balancer, its server, and that load
// balancer's network. Every node of one load balancer's family shares that load
// balancer's gate, as in Build, and no node has a mutation, since the engine of
// a rolling persona never draws one.
//
// The load balancer, listener, pool, and health monitor nodes are pinned: the
// persona keeps its load balancers in front of its clusters for the whole run.
// A member node carries the Roll value of its server, so it rolls with its
// worker: a roll deletes the worker's members before the worker and creates
// them again after the replacement's create, and since every delete of a roll
// waits for the last operation of every node of the set, the next replacement
// in the cluster waits until those members have joined or failed. A load
// balancer whose create failed stays present and pinned for the run, so the
// creates of its members are skipped as doomed children while the rolls go on.
//
// No create returns a network's subnet id or a server's fixed IP, so a load
// balancer create first reads its network's subnet with NetworkSubnet, and a
// member create first reads its server's address and subnet on the load
// balancer's network with ServerAddress, each lookup through the octavia
// executor's retry policy. Every closure captures c, and each call and each
// wait is bounded by opTimeout.
func BuildFrontend(f *plan.Frontend, c FrontendOctavia, roll map[string]string, opTimeout time.Duration) ([]chaos.Node, error) {
	if f == nil || (len(f.LoadBalancers) == 0 && len(f.Listeners) == 0 && len(f.Pools) == 0 &&
		len(f.Members) == 0 && len(f.HealthMonitors) == 0) {
		return nil, nil
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("invalid frontend: %w", err)
	}
	for _, m := range f.Members {
		if roll[m.Server] == "" {
			return nil, fmt.Errorf("member %q references server %q, which has no rolling set", m.Name, m.Server)
		}
	}

	// One gate per load balancer serializes every operation of its family, as
	// in Build. Validate resolved every reference, so these lookups always hit.
	gates := make(map[string]chan struct{}, len(f.LoadBalancers))
	lbNetwork := make(map[string]string, len(f.LoadBalancers))
	for _, lb := range f.LoadBalancers {
		gates[lb.Name] = make(chan struct{}, 1)
		lbNetwork[lb.Name] = lb.Network
	}
	listenerLB := make(map[string]string, len(f.Listeners))
	for _, l := range f.Listeners {
		listenerLB[l.Name] = l.LoadBalancer
	}
	poolLB := make(map[string]string, len(f.Pools))
	for _, pl := range f.Pools {
		poolLB[pl.Name] = listenerLB[pl.Listener]
	}
	bound := &bindings{algorithm: make(map[string]string, len(f.LoadBalancers))}

	var nodes []chaos.Node

	for _, lb := range f.LoadBalancers {
		nodes = append(nodes, chaos.Node{
			Key: lb.Name, Kind: octavia.KindLoadBalancer, Parents: []string{lb.Network}, Gate: gates[lb.Name], Pinned: true,
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				var subnet string
				if err := octaviaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
					s, err := c.NetworkSubnet(ctx, ids[lb.Network])
					if err != nil {
						return err
					}
					subnet = s
					return nil
				}); err != nil {
					return resource.Resource{}, err
				}
				return createLoadBalancer(ctx, opTimeout, c, bound, lb, subnet, f.Provider)
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteLoadBalancer(ctx, opTimeout, c, res)
			},
		})
	}

	for _, l := range f.Listeners {
		lbName := l.LoadBalancer
		nodes = append(nodes, chaos.Node{
			Key: l.Name, Kind: octavia.KindListener, Parents: []string{lbName}, Gate: gates[lbName], Pinned: true,
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createChild(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindListener,
					func(ctx context.Context) (resource.Resource, error) { return c.CreateListener(ctx, l, ids[lbName]) })
			},
			Delete: func(ctx context.Context, ids map[string]string, res resource.Resource) error {
				return deleteChild(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindListener,
					func(ctx context.Context) error { return c.DeleteSingle(ctx, res) })
			},
		})
	}

	for _, pl := range f.Pools {
		lbName := poolLB[pl.Name]
		nodes = append(nodes, chaos.Node{
			Key: pl.Name, Kind: octavia.KindPool, Parents: []string{pl.Listener, lbName}, Gate: gates[lbName], Pinned: true,
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				algorithm, ok := bound.get(lbName)
				if !ok {
					return resource.Resource{}, fmt.Errorf("no pool algorithm bound for load balancer %q", lbName)
				}
				return createChild(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindPool,
					func(ctx context.Context) (resource.Resource, error) {
						return c.CreatePool(ctx, pl, ids[lbName], ids[pl.Listener], algorithm)
					})
			},
			Delete: func(ctx context.Context, ids map[string]string, res resource.Resource) error {
				return deleteChild(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindPool,
					func(ctx context.Context) error { return c.DeleteSingle(ctx, res) })
			},
		})
	}

	for _, h := range f.HealthMonitors {
		lbName := poolLB[h.Pool]
		nodes = append(nodes, chaos.Node{
			Key: h.Name, Kind: octavia.KindHealthMonitor, Parents: []string{h.Pool, lbName}, Gate: gates[lbName], Pinned: true,
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createChild(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindHealthMonitor,
					func(ctx context.Context) (resource.Resource, error) {
						return c.CreateHealthMonitor(ctx, h, ids[h.Pool])
					})
			},
			Delete: func(ctx context.Context, ids map[string]string, res resource.Resource) error {
				return deleteChild(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindHealthMonitor,
					func(ctx context.Context) error { return c.DeleteSingle(ctx, res) })
			},
		})
	}

	for _, m := range f.Members {
		lbName := poolLB[m.Pool]
		network := lbNetwork[lbName]
		nodes = append(nodes, chaos.Node{
			Key: m.Name, Kind: octavia.KindMember, Roll: roll[m.Server],
			Parents: []string{m.Pool, lbName, m.Server, network}, Gate: gates[lbName],
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				var address, subnet string
				if err := octaviaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
					a, s, err := c.ServerAddress(ctx, ids[m.Server], ids[network])
					if err != nil {
						return err
					}
					address, subnet = a, s
					return nil
				}); err != nil {
					return resource.Resource{}, err
				}
				return createChild(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindMember,
					func(ctx context.Context) (resource.Resource, error) {
						return c.CreateMember(ctx, plan.Member{Name: m.Name, Pool: m.Pool, Address: address, Port: m.Port}, ids[m.Pool], subnet)
					})
			},
			Delete: func(ctx context.Context, ids map[string]string, res resource.Resource) error {
				return deleteChild(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindMember,
					func(ctx context.Context) error { return c.DeleteMember(ctx, res, ids[m.Pool]) })
			},
		})
	}

	return nodes, nil
}
