// Package executor turns a plan into a real topology. It creates resources in
// dependency order, running independent same-kind resources concurrently up to
// a configurable limit, retries transient failures with exponential backoff,
// fails fast on quota errors, and honors context cancellation and per-operation
// timeouts. The created resources and the timing the Neutron wrappers record
// are the hand-off surface a later run record and cleanup consume.
package executor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/B42Labs/dizzy/internal/neutron"
	"github.com/B42Labs/dizzy/internal/plan"
)

// retryBaseDelay, retryMaxDelay, and maxAttempts bound the per-operation retry
// of transient errors. conflictMaxAttempts caps 409 conflicts to fewer attempts
// because most Neutron 409s are permanent and should fail fast.
const (
	retryBaseDelay      = 250 * time.Millisecond
	retryMaxDelay       = 5 * time.Second
	maxAttempts         = 5
	conflictMaxAttempts = 2
)

// Neutron is the create-and-wait surface the executor drives. It is the single
// ports-and-adapters seam to the cloud: *neutron.Client satisfies it in
// production and a fake satisfies it in tests. The interface is wide (one
// create per kind) by necessity — it mirrors the resource set, not a behavior
// to abstract.
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
	WaitForReady(ctx context.Context, r neutron.Resource) error
}

// Result is the outcome of an apply: every resource that was created, in
// dependency order.
type Result struct {
	Created []neutron.Resource
}

// Apply creates every resource in p against n, in dependency order. Within each
// stage, independent resources are created concurrently up to concurrency, with
// each create retried on transient errors and bounded by opTimeout. The first
// quota error, or any non-retryable error, stops the run and is returned along
// with the resources created so far; ctx cancellation returns ctx.Err().
//
// externalNetworkID is the external network discovered for this run, or "" when
// the cloud has none. When set, routers marked for an external gateway are
// plugged into it and floating IPs are allocated from it; when empty, both are
// skipped — the plan's external-connectivity intent becomes a no-op rather than
// a failure.
func Apply(ctx context.Context, runID string, n Neutron, p *plan.Plan, concurrency int, opTimeout time.Duration, externalNetworkID string) (*Result, error) {
	e := &applier{n: n, concurrency: concurrency, opTimeout: opTimeout}
	result := &Result{}
	// ids maps a plan logical name to its created cloud id. It is written only
	// between stages (single-threaded) and read concurrently within the next
	// stage, so no lock is needed.
	ids := make(map[string]string)

	// Address scopes.
	res, err := stage(ctx, e, p.AddressScopes, func(ctx context.Context, as plan.AddressScope) (neutron.Resource, error) {
		return e.n.CreateAddressScope(ctx, as)
	})
	result.Created = appendCreated(result.Created, res)
	if err != nil {
		return result, err
	}
	for i, as := range p.AddressScopes {
		ids[as.Name] = res[i].ID
	}

	// Subnet pools (resolve their address scope).
	res, err = stage(ctx, e, p.SubnetPools, func(ctx context.Context, sp plan.SubnetPool) (neutron.Resource, error) {
		return e.n.CreateSubnetPool(ctx, sp, ids[sp.AddressScope])
	})
	result.Created = appendCreated(result.Created, res)
	if err != nil {
		return result, err
	}
	for i, sp := range p.SubnetPools {
		ids[sp.Name] = res[i].ID
	}

	// Networks.
	res, err = stage(ctx, e, p.Networks, func(ctx context.Context, nw plan.Network) (neutron.Resource, error) {
		return e.n.CreateNetwork(ctx, nw)
	})
	result.Created = appendCreated(result.Created, res)
	if err != nil {
		return result, err
	}
	for i, nw := range p.Networks {
		ids[nw.Name] = res[i].ID
	}

	// Routers (plugged into the external network when they want a gateway and
	// one was discovered).
	res, err = stage(ctx, e, p.Routers, func(ctx context.Context, r plan.Router) (neutron.Resource, error) {
		return e.n.CreateRouter(ctx, r, externalNetworkID)
	})
	result.Created = appendCreated(result.Created, res)
	if err != nil {
		return result, err
	}
	for i, r := range p.Routers {
		ids[r.Name] = res[i].ID
	}

	// Security groups (before their rules so remote-group references resolve).
	res, err = stage(ctx, e, p.SecurityGroups, func(ctx context.Context, sg plan.SecurityGroup) (neutron.Resource, error) {
		return e.n.CreateSecurityGroup(ctx, sg)
	})
	result.Created = appendCreated(result.Created, res)
	if err != nil {
		return result, err
	}
	for i, sg := range p.SecurityGroups {
		ids[sg.Name] = res[i].ID
	}

	// Subnets (resolve their network and optional pool).
	res, err = stage(ctx, e, p.Subnets, func(ctx context.Context, s plan.Subnet) (neutron.Resource, error) {
		return e.n.CreateSubnet(ctx, s, ids[s.Network], ids[s.SubnetPool])
	})
	result.Created = appendCreated(result.Created, res)
	if err != nil {
		return result, err
	}
	for i, s := range p.Subnets {
		ids[s.Name] = res[i].ID
	}

	// Security-group rules, flattened from their groups.
	rules := flattenRules(p.SecurityGroups)
	res, err = stage(ctx, e, rules, func(ctx context.Context, it ruleItem) (neutron.Resource, error) {
		var remoteID string
		if it.rule.RemoteGroup != "" {
			remoteID = ids[it.rule.RemoteGroup]
		}
		return e.n.CreateSecurityGroupRule(ctx, it.rule, ids[it.sgName], remoteID)
	})
	result.Created = appendCreated(result.Created, res)
	if err != nil {
		return result, err
	}

	// Ports (resolve their network, fixed-IP subnets, and security groups).
	// Created before router interfaces so a port-based interface — the
	// router-to-router link mechanism — can resolve the port it attaches.
	res, err = stage(ctx, e, p.Ports, func(ctx context.Context, port plan.Port) (neutron.Resource, error) {
		sgIDs := make([]string, 0, len(port.SecurityGroups))
		for _, sg := range port.SecurityGroups {
			sgIDs = append(sgIDs, ids[sg])
		}
		subnetIDs := make(map[string]string, len(port.FixedIPs))
		for _, fip := range port.FixedIPs {
			subnetIDs[fip.Subnet] = ids[fip.Subnet]
		}
		return e.n.CreatePort(ctx, port, ids[port.Network], subnetIDs, sgIDs)
	})
	result.Created = appendCreated(result.Created, res)
	if err != nil {
		return result, err
	}
	for i, port := range p.Ports {
		ids[port.Name] = res[i].ID
	}

	// Router interfaces (resolve their router and either a subnet or a port).
	res, err = stage(ctx, e, p.RouterInterfaces, func(ctx context.Context, ri plan.RouterInterface) (neutron.Resource, error) {
		return e.n.CreateRouterInterface(ctx, ri, ids[ri.Router], ids[ri.Subnet], ids[ri.Port])
	})
	result.Created = appendCreated(result.Created, res)
	if err != nil {
		return result, err
	}

	// Floating IPs, allocated from the external network (resolving an optional
	// internal port to associate). Skipped entirely when no external network was
	// discovered, so the plan's intent degrades to a no-op rather than failing.
	if externalNetworkID == "" {
		if len(p.FloatingIPs) > 0 {
			slog.Warn("skipping floating IPs: no external network available",
				"floatingIPs", len(p.FloatingIPs))
		}
	} else {
		res, err = stage(ctx, e, p.FloatingIPs, func(ctx context.Context, fip plan.FloatingIP) (neutron.Resource, error) {
			return e.n.CreateFloatingIP(ctx, fip, externalNetworkID, ids[fip.Port])
		})
		result.Created = appendCreated(result.Created, res)
		if err != nil {
			return result, err
		}
	}

	return result, nil
}

// applier carries the apply-wide configuration shared by the stage helpers.
type applier struct {
	n           Neutron
	concurrency int
	opTimeout   time.Duration
}

// ruleItem pairs a security-group rule with the logical name of its group so it
// can be created once every group exists.
type ruleItem struct {
	sgName string
	rule   plan.SecurityGroupRule
}

// flattenRules expands the nested rules of every security group into a flat
// work list.
func flattenRules(sgs []plan.SecurityGroup) []ruleItem {
	var items []ruleItem
	for _, sg := range sgs {
		for _, rule := range sg.Rules {
			items = append(items, ruleItem{sgName: sg.Name, rule: rule})
		}
	}
	return items
}

// appendCreated appends the populated resources from a stage to dst, skipping
// the zero Resource{} slots a partially-failed stage leaves for items that
// failed or were never dispatched (identified by an empty ID). It keeps the run
// record honest about what actually exists when a stage fails partway.
func appendCreated(dst, stageRes []neutron.Resource) []neutron.Resource {
	for _, r := range stageRes {
		if r.ID != "" {
			dst = append(dst, r)
		}
	}
	return dst
}

// stage creates every item concurrently through create, returning the resources
// in item order. It provisions each item (retry + wait-for-ready) and stops the
// stage on the first error. It is a free function because Go methods cannot take
// type parameters.
func stage[T any](ctx context.Context, e *applier, items []T, create func(context.Context, T) (neutron.Resource, error)) ([]neutron.Resource, error) {
	return runStage(ctx, items, e.concurrency, func(ctx context.Context, item T) (neutron.Resource, error) {
		return e.provision(ctx, func(ctx context.Context) (neutron.Resource, error) {
			return create(ctx, item)
		})
	})
}

// provision retries the create on transient errors, then waits for the created
// resource to become ready. A readiness deadline that elapses while the run is
// still live is recorded by the wrapper but is not fatal; only a cancellation
// of the parent context stops the run.
func (e *applier) provision(ctx context.Context, create func(context.Context) (neutron.Resource, error)) (neutron.Resource, error) {
	var res neutron.Resource
	err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
		r, err := create(ctx)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return neutron.Resource{}, err
	}
	// Announce each created resource so a long apply shows steady progress
	// instead of going silent between its start and its final metrics. Logged at
	// info (per resource); silence it with --log-level warn.
	slog.Info("created resource", "kind", res.Kind, "logical", res.Logical, "id", res.ID)

	readyCtx, cancel := context.WithTimeout(ctx, e.opTimeout)
	defer cancel()
	if err := e.n.WaitForReady(readyCtx, res); err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		slog.Warn("resource did not reach ready state before deadline",
			"kind", res.Kind, "id", res.ID, "logical", res.Logical, "error", err)
	}
	return res, nil
}

// runStage runs work over items using a fixed pool of at most concurrency
// workers reading from a job channel — a bounded pool rather than one goroutine
// per item, so a large plan cannot exhaust resources. Results are returned in
// item order. The first error cancels the stage, stops dispatching, and is
// returned, with a quota error taking priority and a parent-context
// cancellation reported as ctx.Err(). On error the results created so far are
// still returned (failed or unreached items are the zero Resource{}), so the
// caller can record what already exists in the cloud.
func runStage[T any](ctx context.Context, items []T, concurrency int, work func(context.Context, T) (neutron.Resource, error)) ([]neutron.Resource, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stageCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := concurrency
	if workers < 1 {
		workers = 1
	}
	if workers > len(items) {
		workers = len(items)
	}

	results := make([]neutron.Resource, len(items))
	errs := make([]error, len(items))
	jobs := make(chan int)

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				res, err := work(stageCtx, items[i])
				if err != nil {
					errs[i] = err
					cancel()
					continue
				}
				results[i] = res
			}
		}()
	}

dispatch:
	for i := range items {
		select {
		case jobs <- i:
		case <-stageCtx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()

	for _, err := range errs {
		if errors.Is(err, neutron.ErrQuota) {
			return results, err
		}
	}
	if err := ctx.Err(); err != nil {
		return results, err
	}
	for _, err := range errs {
		if err != nil {
			return results, err
		}
	}
	return results, nil
}

// WithRetry runs fn, bounding each attempt with opTimeout, and retries transient
// errors with exponential backoff up to maxAttempts (or conflictMaxAttempts for
// 409 conflicts, which are usually permanent). It returns immediately on
// success, on a quota error (so the run fails fast), or on any non-retryable
// error. Backoff sleeps honor the parent context. It is exported so the chaos
// churn engine drives its create/delete operations through the same transient/
// conflict/quota backoff policy the apply executor uses.
func WithRetry(ctx context.Context, opTimeout time.Duration, fn func(context.Context) error) error {
	backoff := retryBaseDelay
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		opCtx, cancel := context.WithTimeout(ctx, opTimeout)
		err = fn(opCtx)
		cancel()

		switch {
		case err == nil:
			return nil
		case errors.Is(err, neutron.ErrQuota):
			return err
		case !neutron.IsRetryable(err):
			return err
		case attempt == maxAttempts:
			return err
		case neutron.IsConflict(err) && attempt >= conflictMaxAttempts:
			return err
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff *= 2; backoff > retryMaxDelay {
			backoff = retryMaxDelay
		}
	}
	return err
}
