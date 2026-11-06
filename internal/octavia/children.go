package octavia

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"

	"github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Health monitor timing every planned monitor uses: a TCP connect every
// monitorDelay seconds, failing after monitorTimeout seconds, with the member
// marked down after monitorMaxRetries failures. gophercloud sends a zero
// integer unchecked, so each must be set.
const (
	monitorDelay      = 5
	monitorTimeout    = 3
	monitorMaxRetries = 3
)

// CreateListener creates a TCP listener on l.Port of the load balancer lbID,
// with the deterministic name and the run tags. A create whose answer was lost
// is adopted (see createAdopting); its lookup lists by name, load balancer, and
// run tag.
func (c *Client) CreateListener(ctx context.Context, l plan.Listener, lbID string) (resource.Resource, error) {
	tag := runTag(c.runID)
	r, _, err := createAdopting(ctx, c, KindListener, l.Name,
		func(ctx context.Context, name string) (listeners.Listener, bool, error) {
			return lookupLost(ctx, KindListener, name, tag,
				listeners.List(c.lb, listeners.ListOpts{Name: name, LoadbalancerID: lbID, Tags: []string{tag}}),
				listeners.ExtractListeners,
				func(it listeners.Listener) (string, string, []string, string) {
					return it.Name, it.ID, it.Tags, it.ProvisioningStatus
				})
		},
		func(ctx context.Context, name string) (listeners.Listener, error) {
			created, err := listeners.Create(ctx, c.lb, listeners.CreateOpts{
				Name:           name,
				LoadbalancerID: lbID,
				Protocol:       listeners.ProtocolTCP,
				ProtocolPort:   l.Port,
				Tags:           runTags(c.runID, KindListener),
			}).Extract()
			if err != nil {
				return listeners.Listener{}, err
			}
			return *created, nil
		},
		func(it listeners.Listener) string { return it.ID })
	return r, err
}

// CreatePool creates the TCP default pool of the listener listenerID with the
// load-balancing algorithm algorithm (see PoolAlgorithm), the deterministic
// name, and the run tags. Naming the listener makes the pool its default pool.
// lbID scopes only the lookup of a create whose answer was lost (see
// createAdopting), which lists by name, load balancer, and run tag.
func (c *Client) CreatePool(ctx context.Context, p plan.Pool, lbID, listenerID, algorithm string) (resource.Resource, error) {
	tag := runTag(c.runID)
	r, _, err := createAdopting(ctx, c, KindPool, p.Name,
		func(ctx context.Context, name string) (pools.Pool, bool, error) {
			return lookupLost(ctx, KindPool, name, tag,
				pools.List(c.lb, pools.ListOpts{Name: name, LoadbalancerID: lbID, Tags: []string{tag}}),
				pools.ExtractPools,
				func(it pools.Pool) (string, string, []string, string) {
					return it.Name, it.ID, it.Tags, it.ProvisioningStatus
				})
		},
		func(ctx context.Context, name string) (pools.Pool, error) {
			created, err := pools.Create(ctx, c.lb, pools.CreateOpts{
				Name:       name,
				ListenerID: listenerID,
				Protocol:   pools.ProtocolTCP,
				LBMethod:   pools.LBMethod(algorithm),
				Tags:       runTags(c.runID, KindPool),
			}).Extract()
			if err != nil {
				return pools.Pool{}, err
			}
			return *created, nil
		},
		func(it pools.Pool) string { return it.ID })
	return r, err
}

// CreateMember creates the member m of the pool poolID at m.Address and m.Port
// on the VIP subnet subnetID, which the OVN provider requires, with the
// deterministic name and the run tags. A create whose answer was lost is
// adopted (see createAdopting). Octavia's member list takes no tags filter, so
// its lookup lists the pool's members by name and checks the run tag client
// side only.
func (c *Client) CreateMember(ctx context.Context, m plan.Member, poolID, subnetID string) (resource.Resource, error) {
	tag := runTag(c.runID)
	r, _, err := createAdopting(ctx, c, KindMember, m.Name,
		func(ctx context.Context, name string) (pools.Member, bool, error) {
			return lookupLost(ctx, KindMember, name, tag,
				pools.ListMembers(c.lb, poolID, pools.ListMembersOpts{Name: name}),
				pools.ExtractMembers,
				func(it pools.Member) (string, string, []string, string) {
					return it.Name, it.ID, it.Tags, it.ProvisioningStatus
				})
		},
		func(ctx context.Context, name string) (pools.Member, error) {
			created, err := pools.CreateMember(ctx, c.lb, poolID, pools.CreateMemberOpts{
				Name:         name,
				Address:      m.Address,
				ProtocolPort: m.Port,
				SubnetID:     subnetID,
				Tags:         runTags(c.runID, KindMember),
			}).Extract()
			if err != nil {
				return pools.Member{}, err
			}
			return *created, nil
		},
		func(it pools.Member) string { return it.ID })
	return r, err
}

// CreateHealthMonitor creates the TCP health monitor h of the pool poolID with
// the fixed monitor timing, the deterministic name, and the run tags. A create
// whose answer was lost is adopted (see createAdopting); its lookup lists by
// name, pool, and run tag.
func (c *Client) CreateHealthMonitor(ctx context.Context, h plan.HealthMonitor, poolID string) (resource.Resource, error) {
	tag := runTag(c.runID)
	r, _, err := createAdopting(ctx, c, KindHealthMonitor, h.Name,
		func(ctx context.Context, name string) (monitors.Monitor, bool, error) {
			return lookupLost(ctx, KindHealthMonitor, name, tag,
				monitors.List(c.lb, monitors.ListOpts{Name: name, PoolID: poolID, Tags: []string{tag}}),
				monitors.ExtractMonitors,
				func(it monitors.Monitor) (string, string, []string, string) {
					return it.Name, it.ID, it.Tags, it.ProvisioningStatus
				})
		},
		func(ctx context.Context, name string) (monitors.Monitor, error) {
			created, err := monitors.Create(ctx, c.lb, monitors.CreateOpts{
				Name:       name,
				PoolID:     poolID,
				Type:       monitors.TypeTCP,
				Delay:      monitorDelay,
				Timeout:    monitorTimeout,
				MaxRetries: monitorMaxRetries,
				Tags:       runTags(c.runID, KindHealthMonitor),
			}).Extract()
			if err != nil {
				return monitors.Monitor{}, err
			}
			return *created, nil
		},
		func(it monitors.Monitor) string { return it.ID })
	return r, err
}
