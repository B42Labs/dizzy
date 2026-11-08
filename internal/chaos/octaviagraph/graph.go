// Package octaviagraph builds the Octavia churn graph the service-neutral chaos
// engine schedules. It maps an Octavia plan onto engine nodes, one per cloud
// object: a VIP network, its subnet, a load balancer with its VIP on that
// subnet, a TCP listener, the listener's default pool, a member of the pool,
// and the pool's TCP health monitor. Each node is parented on the objects its
// create needs, so the engine's parents-outlive-children invariant gives the
// lifecycle order, and the engine hands every closure the cloud ids of its
// parents. A pool, member, or health monitor also names its load balancer as a
// parent, so its closures know which load balancer to wait for, and a member
// names its load balancer's subnet, which the OVN provider requires.
//
// Octavia locks a load balancer in a PENDING status after every write and
// answers the next write with 409 until the provider has finished, so every
// node of one load balancer's family (the load balancer, its listeners, pools,
// members, and health monitors) shares one gate, the construct the Nova graph
// uses per server: at most one operation of the family is in flight at once.
// Readiness is part of every operation. A create completes when the load
// balancer is ACTIVE again and records that wait as the time-to-ready of the
// created kind; a delete of a child completes when the load balancer is ACTIVE
// again and records no time-to-ready; a load balancer delete completes when
// the load balancer is gone. During the churn every object is deleted through
// its own call; the cascade delete stays reserved for the teardown.
//
// The mutation is an admin-state toggle on a live listener or member: it is
// set to admin_state_up=false and back to true, each change waiting for the
// load balancer and recording its time-to-ready under the toggled kind. It
// leaves the population untouched. Keeping the Octavia coupling here leaves the
// chaos engine free of any service-specific import.
//
// BuildFrontend maps the Octavia frontend of the mix Gardener persona onto
// nodes that join that persona's rolling graph instead of forming one of their
// own: its load balancers sit on a cluster network of that graph and its
// members stand for the cluster's workers, so it builds no network or subnet
// node, and a create reads the subnet or server address it needs from Neutron.
// Its load balancers, listeners, pools, and health monitors are pinned and
// never mutated, and each member carries its worker's rolling set, so a roll
// deletes a worker's members before the worker and creates them again after it.
package octaviagraph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/octavia"
	octaviaexec "github.com/B42Labs/dizzy/internal/octavia/executor"
	"github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Octavia is the create/wait/delete/toggle surface the chaos engine drives
// through the nodes this package builds. It is the consumer-defined
// ports-and-adapters seam to the cloud — *octavia.Client satisfies it in
// production and a fake satisfies it in tests. It takes the apply executor's
// Octavia seam and adds the waits and single deletes a churn run needs, and the
// admin-state updates of the toggle.
type Octavia interface {
	CreateNetwork(ctx context.Context, n plan.Network) (resource.Resource, error)
	CreateSubnet(ctx context.Context, n plan.Network, networkID string) (resource.Resource, error)
	WaitForReady(ctx context.Context, r resource.Resource) error
	CreateLoadBalancer(ctx context.Context, lb plan.LoadBalancer, subnetID, provider string) (resource.Resource, string, error)
	CreateListener(ctx context.Context, l plan.Listener, lbID string) (resource.Resource, error)
	CreatePool(ctx context.Context, p plan.Pool, lbID, listenerID, algorithm string) (resource.Resource, error)
	CreateMember(ctx context.Context, m plan.Member, poolID, subnetID string) (resource.Resource, error)
	CreateHealthMonitor(ctx context.Context, h plan.HealthMonitor, poolID string) (resource.Resource, error)
	WaitForActive(ctx context.Context, lb resource.Resource, kind resource.Kind) error
	WaitForActiveAfterDelete(ctx context.Context, lb resource.Resource, kind resource.Kind) error
	WaitForGone(ctx context.Context, lb resource.Resource) error
	Delete(ctx context.Context, r resource.Resource) error
	DeleteSingle(ctx context.Context, r resource.Resource) error
	DeleteMember(ctx context.Context, r resource.Resource, poolID string) error
	SetListenerAdminState(ctx context.Context, r resource.Resource, up bool) error
	SetMemberAdminState(ctx context.Context, r resource.Resource, poolID string, up bool) error
}

// The production *octavia.Client must satisfy the seam.
var _ Octavia = (*octavia.Client)(nil)

// bindings holds the pool algorithm bound to each load balancer's current
// instance, keyed by the load balancer's logical name. A load balancer create
// writes it once the load balancer is ACTIVE, and the pool creates of that load
// balancer read it; the engine runs a pool create only after its load
// balancer's create finished, on another goroutine, so the map is guarded.
type bindings struct {
	mu        sync.Mutex
	algorithm map[string]string
}

// set binds algorithm to the load balancer named lb.
func (b *bindings) set(lb, algorithm string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.algorithm[lb] = algorithm
}

// get returns the algorithm bound to the load balancer named lb, and whether
// one is bound.
func (b *bindings) get(lb string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	algorithm, ok := b.algorithm[lb]
	return algorithm, ok
}

// Build turns an Octavia plan into the churn graph. It emits, in plan order and
// one collection after another, a parentless node per VIP network, a node per
// subnet parented on its network, a node per load balancer parented on its
// network's subnet, a node per listener parented on its load balancer, a node
// per pool parented on its listener and load balancer, a node per member
// parented on its pool, load balancer, and that load balancer's subnet, and a
// node per health monitor parented on its pool and load balancer. Every node of
// one load balancer's family shares that load balancer's gate; the network and
// subnet nodes have none. Listener and member nodes carry the admin-state
// toggle as their mutation. Every closure captures c and runs its calls through
// the octavia executor's retry policy, each call and each wait bounded by
// opTimeout. The plan is validated first, so a dangling reference fails loudly
// instead of yielding a node that can never be created.
func Build(p *plan.Plan, c Octavia, opTimeout time.Duration) ([]chaos.Node, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid plan: %w", err)
	}

	// One gate per load balancer serializes every operation of its family,
	// since Octavia answers a write to a load balancer in a PENDING status with
	// 409. The gate is a capacity-1 channel the engine acquires before granting a
	// concurrency slot, so an op parked behind a busy family never occupies a
	// slot other families could use.
	gates := make(map[string]chan struct{}, len(p.LoadBalancers))
	for _, lb := range p.LoadBalancers {
		gates[lb.Name] = make(chan struct{}, 1)
	}
	// Validate resolved every reference, so these lookups always hit.
	subnetOf := make(map[string]string, len(p.Networks))
	for _, n := range p.Networks {
		subnetOf[n.Name] = n.Subnet
	}
	lbSubnet := make(map[string]string, len(p.LoadBalancers))
	for _, lb := range p.LoadBalancers {
		lbSubnet[lb.Name] = subnetOf[lb.Network]
	}
	listenerLB := make(map[string]string, len(p.Listeners))
	for _, l := range p.Listeners {
		listenerLB[l.Name] = l.LoadBalancer
	}
	poolLB := make(map[string]string, len(p.Pools))
	for _, pl := range p.Pools {
		poolLB[pl.Name] = listenerLB[pl.Listener]
	}
	bound := &bindings{algorithm: make(map[string]string, len(p.LoadBalancers))}

	var nodes []chaos.Node

	for _, n := range p.Networks {
		nodes = append(nodes, chaos.Node{
			Key: n.Name, Kind: octavia.KindNetwork,
			Create: func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
				return createNetwork(ctx, opTimeout, c, n)
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteNeutron(ctx, opTimeout, c, res)
			},
		})
	}

	for _, n := range p.Networks {
		nodes = append(nodes, chaos.Node{
			Key: n.Subnet, Kind: octavia.KindSubnet, Parents: []string{n.Name},
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
					return c.CreateSubnet(ctx, n, ids[n.Name])
				})
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteNeutron(ctx, opTimeout, c, res)
			},
		})
	}

	for _, lb := range p.LoadBalancers {
		subnet := lbSubnet[lb.Name]
		nodes = append(nodes, chaos.Node{
			Key: lb.Name, Kind: octavia.KindLoadBalancer, Parents: []string{subnet}, Gate: gates[lb.Name],
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createLoadBalancer(ctx, opTimeout, c, bound, lb, ids[subnet], p.Provider)
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteLoadBalancer(ctx, opTimeout, c, res)
			},
		})
	}

	for _, l := range p.Listeners {
		lbName := l.LoadBalancer
		nodes = append(nodes, chaos.Node{
			Key: l.Name, Kind: octavia.KindListener, Parents: []string{lbName}, Gate: gates[lbName],
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createChild(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindListener,
					func(ctx context.Context) (resource.Resource, error) { return c.CreateListener(ctx, l, ids[lbName]) })
			},
			Delete: func(ctx context.Context, ids map[string]string, res resource.Resource) error {
				return deleteChild(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindListener,
					func(ctx context.Context) error { return c.DeleteSingle(ctx, res) })
			},
			Mutate: func(ctx context.Context, ids map[string]string, res resource.Resource) error {
				return toggle(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindListener,
					func(ctx context.Context, up bool) error { return c.SetListenerAdminState(ctx, res, up) })
			},
		})
	}

	for _, pl := range p.Pools {
		lbName := poolLB[pl.Name]
		nodes = append(nodes, chaos.Node{
			Key: pl.Name, Kind: octavia.KindPool, Parents: []string{pl.Listener, lbName}, Gate: gates[lbName],
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

	for _, m := range p.Members {
		lbName := poolLB[m.Pool]
		subnet := lbSubnet[lbName]
		nodes = append(nodes, chaos.Node{
			Key: m.Name, Kind: octavia.KindMember, Parents: []string{m.Pool, lbName, subnet}, Gate: gates[lbName],
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createChild(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindMember,
					func(ctx context.Context) (resource.Resource, error) {
						return c.CreateMember(ctx, m, ids[m.Pool], ids[subnet])
					})
			},
			Delete: func(ctx context.Context, ids map[string]string, res resource.Resource) error {
				return deleteChild(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindMember,
					func(ctx context.Context) error { return c.DeleteMember(ctx, res, ids[m.Pool]) })
			},
			Mutate: func(ctx context.Context, ids map[string]string, res resource.Resource) error {
				return toggle(ctx, opTimeout, c, lbRef(lbName, ids), octavia.KindMember,
					func(ctx context.Context, up bool) error { return c.SetMemberAdminState(ctx, res, ids[m.Pool], up) })
			},
		})
	}

	for _, h := range p.HealthMonitors {
		lbName := poolLB[h.Pool]
		nodes = append(nodes, chaos.Node{
			Key: h.Name, Kind: octavia.KindHealthMonitor, Parents: []string{h.Pool, lbName}, Gate: gates[lbName],
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

	return nodes, nil
}

// lbRef builds the minimal load balancer resource the waits poll, from the
// engine-supplied parent ids.
func lbRef(name string, ids map[string]string) resource.Resource {
	return resource.Resource{Kind: octavia.KindLoadBalancer, Logical: name, ID: ids[name]}
}

// createRetry runs a create through the retry policy, returning the created
// resource, or the zero Resource when the create failed.
func createRetry(ctx context.Context, opTimeout time.Duration, create func(context.Context) (resource.Resource, error)) (resource.Resource, error) {
	var res resource.Resource
	err := octaviaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		r, err := create(ctx)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return resource.Resource{}, err
	}
	return res, nil
}

// createNetwork creates a VIP network and waits for it to become ready. A wait
// that fails returns the network with the error, so the engine keeps it
// deletable and skips its subnet.
func createNetwork(ctx context.Context, opTimeout time.Duration, c Octavia, n plan.Network) (resource.Resource, error) {
	net, err := createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return c.CreateNetwork(ctx, n)
	})
	if err != nil {
		return resource.Resource{}, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	if err := c.WaitForReady(readyCtx, net); err != nil {
		return net, err
	}
	return net, nil
}

// createLoadBalancer creates the load balancer lb with its VIP on subnetID
// through provider and waits for it to become ACTIVE. Once it is, it binds the
// pool algorithm of the provider Octavia reports for it, the effective
// provider, so the pools of this instance get an algorithm the provider
// supports, as apply does. A wait that fails returns the load balancer with the
// error and binds nothing; the engine then skips its children.
func createLoadBalancer(ctx context.Context, opTimeout time.Duration, c Octavia, bound *bindings, lb plan.LoadBalancer, subnetID, provider string) (resource.Resource, error) {
	var (
		res       resource.Resource
		effective string
	)
	if err := octaviaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		r, prov, err := c.CreateLoadBalancer(ctx, lb, subnetID, provider)
		if err != nil {
			return err
		}
		res, effective = r, prov
		return nil
	}); err != nil {
		return resource.Resource{}, err
	}
	if err := waitActive(ctx, opTimeout, c, res, octavia.KindLoadBalancer); err != nil {
		return res, err
	}
	algorithm := octavia.PoolAlgorithm(effective)
	bound.set(lb.Name, algorithm)
	slog.Info("load balancer provider bound", "logical", lb.Name, "provider", effective, "algorithm", algorithm)
	return res, nil
}

// createChild creates a child of the load balancer lb through create and waits
// for lb to be ACTIVE again, recording the wait as the time-to-ready of kind. A
// wait that fails returns the created child with the error, so the engine keeps
// it deletable and skips its own children.
func createChild(ctx context.Context, opTimeout time.Duration, c Octavia, lb resource.Resource, kind resource.Kind, create func(context.Context) (resource.Resource, error)) (resource.Resource, error) {
	res, err := createRetry(ctx, opTimeout, create)
	if err != nil {
		return resource.Resource{}, err
	}
	if err := waitActive(ctx, opTimeout, c, lb, kind); err != nil {
		return res, err
	}
	return res, nil
}

// deleteChild deletes a child of the load balancer lb through del, folding an
// already-gone (404) into success, and waits for lb to be ACTIVE again without
// recording a time-to-ready, since the wait after a delete is the time-to-ready
// of no object. Any other delete error returns without the wait. A wait that
// finds lb gone (404) counts as deleted too: a child cannot outlive its load
// balancer, and the engine retries a failed delete with the parent ids it first
// used, which can name a load balancer instance the churn has deleted since.
func deleteChild(ctx context.Context, opTimeout time.Duration, c Octavia, lb resource.Resource, kind resource.Kind, del func(context.Context) error) error {
	if err := octaviaexec.WithRetry(ctx, opTimeout, del); err != nil && !octavia.IsNotFound(err) {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	if err := c.WaitForActiveAfterDelete(waitCtx, lb, kind); err != nil && !octavia.IsNotFound(err) {
		return err
	}
	return nil
}

// deleteLoadBalancer deletes the load balancer res without cascade, folding an
// already-gone (404) into success, and waits until it is gone, so its VIP port
// is released before its subnet can be deleted. Octavia refuses the delete with
// 400 while a child whose own delete failed is still attached; that error
// returns as is, and the engine deletes the load balancer again before its next
// create, while the teardown's cascade reclaims it at the latest.
func deleteLoadBalancer(ctx context.Context, opTimeout time.Duration, c Octavia, res resource.Resource) error {
	if err := octaviaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		return c.DeleteSingle(ctx, res)
	}); err != nil && !octavia.IsNotFound(err) {
		return err
	}
	goneCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return c.WaitForGone(goneCtx, res)
}

// deleteNeutron deletes a VIP subnet or network, folding an already-gone (404)
// into success. Neutron deletes synchronously, so there is no wait.
func deleteNeutron(ctx context.Context, opTimeout time.Duration, c Octavia, res resource.Resource) error {
	if err := octaviaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		return c.Delete(ctx, res)
	}); err != nil && !octavia.IsNotFound(err) {
		return err
	}
	return nil
}

// toggle runs the admin-state toggle of a listener or member of the load
// balancer lb: set(false), then set(true), each through the retry policy and
// followed by the wait for lb to be ACTIVE again, which records the time-to-ready
// of kind. The first error returns at once, so a failed disable never sends the
// enable, and a failed enable leaves the object disabled until its node's next
// delete; the engine draws the toggle at most once per instance lifetime.
func toggle(ctx context.Context, opTimeout time.Duration, c Octavia, lb resource.Resource, kind resource.Kind, set func(ctx context.Context, up bool) error) error {
	for _, up := range []bool{false, true} {
		if err := octaviaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
			return set(ctx, up)
		}); err != nil {
			return err
		}
		if err := waitActive(ctx, opTimeout, c, lb, kind); err != nil {
			return err
		}
	}
	return nil
}

// waitActive waits, bounded by opTimeout, for the load balancer lb to be ACTIVE
// again after a write of kind, recording the wait as kind's time-to-ready.
func waitActive(ctx context.Context, opTimeout time.Duration, c Octavia, lb resource.Resource, kind resource.Kind) error {
	waitCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return c.WaitForActive(waitCtx, lb, kind)
}

// Classify labels an operation error for the churn engine's per-bucket error
// breakdown, with the labels the other graphs use plus provisioning_error for a
// load balancer that reached ERROR. That one is checked first, so a provider
// failure is never hidden under a broader label. It is wired into the engine
// via chaos.Config.Classify.
func Classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, octavia.ErrProvisioning):
		return "provisioning_error"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, octavia.ErrQuota):
		return "quota"
	case octavia.IsNotFound(err):
		return "not-found"
	case octavia.IsRetryable(err):
		return "transient"
	default:
		return "other"
	}
}
