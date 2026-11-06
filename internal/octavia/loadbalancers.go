package octavia

import (
	"context"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"

	"github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// CreateLoadBalancer creates a load balancer with the deterministic name and
// the run tags, its VIP on subnetID, through the provider driver named
// provider; an empty provider leaves the choice to the cloud's default. It
// returns the created Resource and the provider Octavia reports for it, the
// effective provider, also when it adopts a load balancer whose create answer
// was lost (see createAdopting). A quota rejection is wrapped with ErrQuota so
// the executor fails fast.
func (c *Client) CreateLoadBalancer(ctx context.Context, lb plan.LoadBalancer, subnetID, provider string) (resource.Resource, string, error) {
	tag := runTag(c.runID)
	r, created, err := createAdopting(ctx, c, KindLoadBalancer, lb.Name,
		func(ctx context.Context, name string) (loadbalancers.LoadBalancer, bool, error) {
			return lookupLost(ctx, KindLoadBalancer, name, tag,
				loadbalancers.List(c.lb, loadbalancers.ListOpts{Name: name, Tags: []string{tag}}),
				loadbalancers.ExtractLoadBalancers,
				func(it loadbalancers.LoadBalancer) (string, string, []string, string) {
					return it.Name, it.ID, it.Tags, it.ProvisioningStatus
				})
		},
		func(ctx context.Context, name string) (loadbalancers.LoadBalancer, error) {
			created, err := loadbalancers.Create(ctx, c.lb, loadbalancers.CreateOpts{
				Name:        name,
				VipSubnetID: subnetID,
				Provider:    provider,
				Tags:        runTags(c.runID, KindLoadBalancer),
			}).Extract()
			if err != nil {
				return loadbalancers.LoadBalancer{}, err
			}
			return *created, nil
		},
		func(it loadbalancers.LoadBalancer) string { return it.ID })
	if err != nil {
		return resource.Resource{}, "", err
	}
	return r, created.Provider, nil
}

// WaitForActive polls the load balancer lb until its provisioning status is
// ACTIVE again after a write of kind, recording the wait as one Readiness
// sample of kind: the time-to-ready of what was just written. The polls go
// through loadbalancers.Get without recording a sample, so they do not flood
// the per-call latency stats.
//
// A load balancer in ERROR is a terminal failure: it records a failed readiness
// sample, records the poll that found it as one failed <kind>/get sample with
// error kind provisioning_error, and returns an ErrProvisioning error. A 404
// means the load balancer is gone and aborts the wait. Any other poll error is
// treated as transient and polled through. It returns ctx.Err() if ctx is
// cancelled or its deadline elapses first. The wait after a churn delete is
// WaitForActiveAfterDelete, which records no Readiness sample.
func (c *Client) WaitForActive(ctx context.Context, lb resource.Resource, kind resource.Kind) error {
	return c.waitActive(ctx, lb, kind, true)
}

// WaitForActiveAfterDelete polls the load balancer lb until its provisioning
// status is ACTIVE again after the delete of a child of kind, as WaitForActive
// does, but records no Readiness sample on any branch: the time a load
// balancer takes to settle after a churn delete is not the time-to-ready of
// any object. A load balancer in ERROR still records the poll that found it as
// one failed <kind>/get sample with error kind provisioning_error and returns
// an ErrProvisioning error, so the provider failure shows in the error
// breakdown. A 404 and the end of ctx return as they do for WaitForActive.
func (c *Client) WaitForActiveAfterDelete(ctx context.Context, lb resource.Resource, kind resource.Kind) error {
	return c.waitActive(ctx, lb, kind, false)
}

// waitActive is the shared body of WaitForActive and WaitForActiveAfterDelete.
// It polls lb until it is ACTIVE. When ready is true it records the wait as one
// Readiness sample of kind on every branch; when ready is false it records
// none. The ERROR branch records its failed <kind>/get sample either way.
func (c *Client) waitActive(ctx context.Context, lb resource.Resource, kind resource.Kind, ready bool) error {
	start := time.Now()
	recordWait := func(ok bool) {
		if ready {
			c.recordReady(ctx, kind, time.Since(start), ok)
		}
	}
	backoff := pollInitial
	for {
		pollStart := time.Now()
		got, err := loadbalancers.Get(ctx, c.lb, lb.ID).Extract()
		poll := time.Since(pollStart)
		switch {
		case err == nil && got.ProvisioningStatus == statusActive:
			recordWait(true)
			return nil
		case err == nil && got.ProvisioningStatus == statusError:
			err := fmt.Errorf("%w: load balancer %s reached ERROR after a %s write", ErrProvisioning, lb.ID, kind)
			recordWait(false)
			c.record(ctx, string(kind), "get", poll, err)
			return err
		case IsNotFound(err):
			recordWait(false)
			return fmt.Errorf("load balancer %s is gone while waiting for ACTIVE: %w", lb.ID, err)
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			recordWait(false)
			return ctx.Err()
		}
		backoff = nextBackoff(backoff)
	}
}

// WaitForGone polls the load balancer lb until it no longer exists: a 404, or a
// provisioning status of DELETED, which Octavia keeps showing for a while
// after a delete. Cleanup uses it so a load balancer's cascade has removed its
// VIP port before the VIP subnet and network are deleted. The polls record no
// sample. It returns ctx.Err() if the deadline elapses first. Any other kind is
// refused without a request.
func (c *Client) WaitForGone(ctx context.Context, lb resource.Resource) error {
	if lb.Kind != KindLoadBalancer {
		return fmt.Errorf("wait for gone not supported for kind %q", lb.Kind)
	}
	backoff := pollInitial
	for {
		got, err := loadbalancers.Get(ctx, c.lb, lb.ID).Extract()
		if IsNotFound(err) || (err == nil && got.ProvisioningStatus == statusDeleted) {
			return nil
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = nextBackoff(backoff)
	}
}
