// Package executor turns an Octavia plan into real load balancers. It runs six
// strictly ordered stages — create the VIP networks (and their subnets), the
// load balancers, the listeners, the pools, the members, then the health
// monitors — with transient failures retried with exponential backoff, quota
// errors failing fast, and per-operation timeouts. Octavia locks a load
// balancer while it applies a write and answers a second write with a 409, so
// after every write the executor waits for the load balancer to return to
// ACTIVE, and in the listener, pool, member, and health monitor stages one work
// item holds every write to one load balancer and runs them one at a time.
// Different load balancers proceed concurrently up to a configurable limit.
// The created resources and the timing the Octavia wrappers record are the
// hand-off surface a later run record and cleanup consume.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/B42Labs/dizzy/internal/octavia"
	"github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// retryBaseDelay and retryMaxDelay bound the backoff between two attempts of
// the per-operation retry of transient errors, and maxAttempts bounds the
// attempts. They duplicate the other executors' policy rather than sharing it:
// each package's WithRetry hardcodes its own service's classifiers, and this
// one uses the octavia classifiers. The delays are variables so tests can
// shrink them.
var (
	retryBaseDelay = 250 * time.Millisecond
	retryMaxDelay  = 5 * time.Second
)

// maxAttempts bounds the attempts of one operation under WithRetry.
const maxAttempts = 5

// Octavia is the create-and-wait surface the executor drives. It is the single
// ports-and-adapters seam to the cloud: *octavia.Client satisfies it in
// production and a fake satisfies it in tests.
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
}

// The production *octavia.Client must satisfy the seam.
var _ Octavia = (*octavia.Client)(nil)

// Result is the outcome of an apply: every resource that was created, in
// dependency order (each network and its subnet, load balancers, listeners,
// pools, members, health monitors).
type Result struct {
	Created []resource.Resource
}

// Apply creates the plan's VIP networks and subnets, load balancers,
// listeners, pools, members, and health monitors in six strictly ordered
// stages. The network and load balancer stages run one work item per network
// or load balancer; the later four run one work item per load balancer holding
// that load balancer's items of the stage, which it creates one at a time. Up
// to concurrency work items run at once. Every create is retried on transient
// errors and bounded by opTimeout, and every write to a load balancer is
// followed by a wait, also bounded by opTimeout, for the load balancer to
// return to ACTIVE, so no two writes reach one load balancer at once. The
// first quota error, or any non-retryable error, stops the run and is returned
// along with the resources created so far; ctx cancellation returns ctx.Err().
func Apply(ctx context.Context, c Octavia, p *plan.Plan, concurrency int, opTimeout time.Duration) (*Result, error) {
	e := &applier{c: c, opTimeout: opTimeout}
	result := &Result{}

	// Stage 1: VIP networks and their subnets.
	subnetByNetwork := make(map[string]string, len(p.Networks))
	netRes, err := runStage(ctx, p.Networks, concurrency, func(ctx context.Context, n plan.Network) ([]resource.Resource, error) {
		return e.provisionNetwork(ctx, n)
	})
	for i, rs := range netRes {
		result.Created = appendCreated(result.Created, rs)
		if len(rs) > 1 && rs[1].ID != "" {
			subnetByNetwork[p.Networks[i].Name] = rs[1].ID // the network resource is first, its subnet second
		}
	}
	if err != nil {
		return result, err
	}

	// Stage 2: load balancers, each with the pool algorithm its effective
	// provider supports.
	lbRes, err := runStage(ctx, p.LoadBalancers, concurrency, func(ctx context.Context, lb plan.LoadBalancer) (boundLB, error) {
		return e.provisionLoadBalancer(ctx, lb, subnetByNetwork[lb.Network], p.Provider)
	})
	bound := make([]boundLB, 0, len(lbRes))
	for _, b := range lbRes {
		if b.res.ID != "" {
			result.Created = append(result.Created, b.res)
			bound = append(bound, b)
		}
	}
	if err != nil {
		return result, err
	}

	// Each child's load balancer, by logical name, through its parent chain.
	lbOfListener := make(map[string]string, len(p.Listeners))
	for _, l := range p.Listeners {
		lbOfListener[l.Name] = l.LoadBalancer
	}
	lbOfPool := make(map[string]string, len(p.Pools))
	for _, pl := range p.Pools {
		lbOfPool[pl.Name] = lbOfListener[pl.Listener]
	}

	// Stage 3: listeners.
	listenerByLogical, created, err := runLoadBalancerStage(ctx, e, octavia.KindListener, concurrency,
		groupByLoadBalancer(bound, p.Listeners, func(l plan.Listener) string { return l.LoadBalancer }),
		func(l plan.Listener) string { return l.Name },
		func(ctx context.Context, lb boundLB, l plan.Listener) (resource.Resource, error) {
			return e.c.CreateListener(ctx, l, lb.res.ID)
		})
	result.Created = append(result.Created, created...)
	if err != nil {
		return result, err
	}

	// Stage 4: pools, each the default pool of its listener, with its load
	// balancer's algorithm.
	poolByLogical, created, err := runLoadBalancerStage(ctx, e, octavia.KindPool, concurrency,
		groupByLoadBalancer(bound, p.Pools, func(pl plan.Pool) string { return lbOfPool[pl.Name] }),
		func(pl plan.Pool) string { return pl.Name },
		func(ctx context.Context, lb boundLB, pl plan.Pool) (resource.Resource, error) {
			return e.c.CreatePool(ctx, pl, lb.res.ID, listenerByLogical[pl.Listener].ID, lb.algorithm)
		})
	result.Created = append(result.Created, created...)
	if err != nil {
		return result, err
	}

	// Stage 5: members, each on its load balancer's VIP subnet.
	_, created, err = runLoadBalancerStage(ctx, e, octavia.KindMember, concurrency,
		groupByLoadBalancer(bound, p.Members, func(m plan.Member) string { return lbOfPool[m.Pool] }),
		func(m plan.Member) string { return m.Name },
		func(ctx context.Context, lb boundLB, m plan.Member) (resource.Resource, error) {
			return e.c.CreateMember(ctx, m, poolByLogical[m.Pool].ID, lb.subnetID)
		})
	result.Created = append(result.Created, created...)
	if err != nil {
		return result, err
	}

	// Stage 6: health monitors.
	_, created, err = runLoadBalancerStage(ctx, e, octavia.KindHealthMonitor, concurrency,
		groupByLoadBalancer(bound, p.HealthMonitors, func(h plan.HealthMonitor) string { return lbOfPool[h.Pool] }),
		func(h plan.HealthMonitor) string { return h.Name },
		func(ctx context.Context, _ boundLB, h plan.HealthMonitor) (resource.Resource, error) {
			return e.c.CreateHealthMonitor(ctx, h, poolByLogical[h.Pool].ID)
		})
	result.Created = append(result.Created, created...)
	if err != nil {
		return result, err
	}

	return result, nil
}

// applier carries the apply-wide configuration shared by the stage helpers.
type applier struct {
	c         Octavia
	opTimeout time.Duration
}

// boundLB is a created load balancer together with what its children are
// created against: the logical name its plan entry carries, the id of the VIP
// subnet it sits on, which every member of it repeats, and the pool algorithm
// its effective provider supports.
type boundLB struct {
	logical   string
	res       resource.Resource
	subnetID  string
	algorithm string
}

// lbGroup is one work item of the listener, pool, member, or health monitor
// stage: the stage's items that write to lb, in plan order.
type lbGroup[T any] struct {
	lb    boundLB
	items []T
}

// provisionNetwork creates a VIP network (retrying transient errors), waits for
// it to become ready with the deadline tolerated, then creates its subnet. It
// returns both created resources — the network first, the subnet second — even
// when the subnet create fails, so a network that exists is still recorded for
// cleanup.
func (e *applier) provisionNetwork(ctx context.Context, n plan.Network) ([]resource.Resource, error) {
	var net resource.Resource
	if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return e.c.CreateNetwork(ctx, n)
	}, &net); err != nil {
		return nil, err
	}
	slog.Info("created resource", "kind", net.Kind, "logical", net.Logical, "id", net.ID)
	created := []resource.Resource{net}
	if err := e.waitReady(ctx, net); err != nil {
		return created, err
	}

	var sub resource.Resource
	if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return e.c.CreateSubnet(ctx, n, net.ID)
	}, &sub); err != nil {
		return created, err
	}
	slog.Info("created resource", "kind", sub.Kind, "logical", sub.Logical, "id", sub.ID)
	return append(created, sub), nil
}

// provisionLoadBalancer creates a load balancer with its VIP on subnetID
// through provider (retrying transient errors), binds the pool algorithm of
// the provider Octavia reports for it, and waits for it to become ACTIVE. It
// returns the created load balancer even when the wait fails, so a load
// balancer that exists is still recorded for cleanup.
func (e *applier) provisionLoadBalancer(ctx context.Context, lb plan.LoadBalancer, subnetID, provider string) (boundLB, error) {
	var (
		res       resource.Resource
		effective string
	)
	if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
		r, prov, err := e.c.CreateLoadBalancer(ctx, lb, subnetID, provider)
		if err != nil {
			return resource.Resource{}, err
		}
		effective = prov
		return r, nil
	}, &res); err != nil {
		return boundLB{}, err
	}
	slog.Info("created resource", "kind", res.Kind, "logical", res.Logical, "id", res.ID)

	b := boundLB{logical: lb.Name, res: res, subnetID: subnetID, algorithm: octavia.PoolAlgorithm(effective)}
	slog.Info("load balancer provider bound", "logical", lb.Name, "provider", effective, "algorithm", b.algorithm)
	if err := e.waitActive(ctx, res, octavia.KindLoadBalancer, lb.Name); err != nil {
		return b, err
	}
	return b, nil
}

// waitReady bounds a VIP network readiness poll with opTimeout. A readiness
// deadline that elapses while the run is still live is warned and tolerated
// (nil); a parent cancellation returns ctx.Err(); any other error is returned
// so the stage fails.
func (e *applier) waitReady(ctx context.Context, res resource.Resource) error {
	waitCtx, cancel := context.WithTimeout(ctx, e.opTimeout)
	defer cancel()
	err := e.c.WaitForReady(waitCtx, res)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("resource did not reach the expected state before deadline",
			"kind", res.Kind, "id", res.ID, "logical", res.Logical, "error", err)
		return nil
	}
	return err
}

// waitActive bounds the wait for the load balancer lb to return to ACTIVE
// after a write of kind with the logical name logical, with opTimeout. Unlike
// waitReady every error is fatal: a load balancer that is not ACTIVE answers
// the next write with a 409, so tolerating the deadline would only defer the
// failure to a later write with a misleading cause. A missed deadline on the
// load balancer's own first wait names the likely cause, an amphora build that
// outlasts --timeout. A parent cancellation returns ctx.Err().
func (e *applier) waitActive(ctx context.Context, lb resource.Resource, kind resource.Kind, logical string) error {
	waitCtx, cancel := context.WithTimeout(ctx, e.opTimeout)
	defer cancel()
	err := e.c.WaitForActive(waitCtx, lb, kind)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	err = fmt.Errorf("load balancer %s did not return to ACTIVE after creating %s %q: %w", lb.ID, kind, logical, err)
	if kind == octavia.KindLoadBalancer && errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w; an amphora load balancer can take minutes to build, raise --timeout", err)
	}
	return err
}

// groupByLoadBalancer buckets items by the load balancer lbOf names for each,
// one group per load balancer of bound that has items, in the order of bound
// (the plan order of the load balancers) and with each group's items in plan
// order. An item whose load balancer is not in bound is left out: Apply only
// reaches a later stage once every load balancer was created.
func groupByLoadBalancer[T any](bound []boundLB, items []T, lbOf func(T) string) []lbGroup[T] {
	index := make(map[string]int, len(bound))
	groups := make([]lbGroup[T], len(bound))
	for i, b := range bound {
		index[b.logical] = i
		groups[i].lb = b
	}
	for _, it := range items {
		if i, ok := index[lbOf(it)]; ok {
			groups[i].items = append(groups[i].items, it)
		}
	}
	out := groups[:0]
	for _, g := range groups {
		if len(g.items) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// runLoadBalancerStage runs one of the listener, pool, member, and health
// monitor stages over groups, one work item per load balancer, up to
// concurrency at once. A work item creates its items one at a time through
// create (retrying transient errors), each followed by waitActive on the
// group's load balancer with kind, so the next write to that load balancer
// starts only once the last one has been applied. A work item stops at its
// first error and keeps what it created before it. The stage returns the
// created resources by their item's logical name (from name), the created
// resources grouped by load balancer in dispatch order, and runStage's error.
func runLoadBalancerStage[T any](ctx context.Context, e *applier, kind resource.Kind, concurrency int, groups []lbGroup[T],
	name func(T) string, create func(context.Context, boundLB, T) (resource.Resource, error),
) (map[string]resource.Resource, []resource.Resource, error) {
	res, err := runStage(ctx, groups, concurrency, func(ctx context.Context, g lbGroup[T]) ([]resource.Resource, error) {
		created := make([]resource.Resource, 0, len(g.items))
		for _, it := range g.items {
			// A sibling's failure cancels the stage; start no further write.
			if err := ctx.Err(); err != nil {
				return created, err
			}
			var r resource.Resource
			if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
				return create(ctx, g.lb, it)
			}, &r); err != nil {
				return created, err
			}
			slog.Info("created resource", "kind", r.Kind, "logical", r.Logical, "id", r.ID)
			created = append(created, r)
			if err := e.waitActive(ctx, g.lb.res, kind, name(it)); err != nil {
				return created, err
			}
		}
		return created, nil
	})

	byLogical := make(map[string]resource.Resource)
	var all []resource.Resource
	for i, rs := range res {
		// A work item's k-th created resource is its group's k-th item.
		for k, r := range rs {
			byLogical[name(groups[i].items[k])] = r
		}
		all = appendCreated(all, rs)
	}
	return byLogical, all, err
}

// withCreate runs a create through the retry policy, storing the created
// resource into out on success.
func withCreate(ctx context.Context, opTimeout time.Duration, create func(context.Context) (resource.Resource, error), out *resource.Resource) error {
	return WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		r, err := create(ctx)
		if err != nil {
			return err
		}
		*out = r
		return nil
	})
}

// appendCreated appends the populated resources from a stage to dst, skipping
// the zero Resource{} slots a partially-failed stage leaves for items that
// failed or were never dispatched (identified by an empty ID). It keeps the run
// record honest about what actually exists when a stage fails partway.
func appendCreated(dst, stageRes []resource.Resource) []resource.Resource {
	for _, r := range stageRes {
		if r.ID != "" {
			dst = append(dst, r)
		}
	}
	return dst
}

// runStage runs work over items using a fixed pool of at most concurrency
// workers reading from a job channel — a bounded pool rather than one goroutine
// per item, so a large plan cannot exhaust resources. Results are returned in
// item order (populated even for a failing item, so a caller can record what
// already exists). The first error cancels the stage, stops dispatching, and is
// returned, with a quota error taking priority and a parent-context cancellation
// reported as ctx.Err().
func runStage[T, R any](ctx context.Context, items []T, concurrency int, work func(context.Context, T) (R, error)) ([]R, error) {
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

	results := make([]R, len(items))
	errs := make([]error, len(items))
	jobs := make(chan int)

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				res, err := work(stageCtx, items[i])
				results[i] = res
				if err != nil {
					errs[i] = err
					cancel()
				}
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
		if errors.Is(err, octavia.ErrQuota) {
			return results, err
		}
	}
	if err := ctx.Err(); err != nil {
		return results, err
	}
	// Prefer a real failure over the context.Canceled that in-flight siblings
	// return once the first error triggers cancel(); otherwise the root cause
	// is masked by an arbitrary cancelled sibling.
	var canceled error
	for _, err := range errs {
		switch {
		case err == nil:
			continue
		case errors.Is(err, context.Canceled):
			canceled = err
		default:
			return results, err
		}
	}
	if canceled != nil {
		return results, canceled
	}
	return results, nil
}

// WithRetry runs fn, bounding each attempt with opTimeout, and retries transient
// errors with exponential backoff up to maxAttempts. It returns immediately on
// success, on a quota error (so the run fails fast), or on any non-retryable
// error, a load balancer provisioning failure included. Backoff sleeps honor
// the parent context. It is exported so a later octavia driver can run its
// operations through the same transient/quota backoff policy the apply path
// uses.
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
		case errors.Is(err, octavia.ErrQuota):
			return err
		case !octavia.IsRetryable(err):
			return err
		case attempt == maxAttempts:
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
