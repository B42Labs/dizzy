package chaos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/B42Labs/openstack-tester/internal/executor"
	"github.com/B42Labs/openstack-tester/internal/metrics"
	"github.com/B42Labs/openstack-tester/internal/neutron"
	"github.com/B42Labs/openstack-tester/internal/plan"
)

// bucketCount is the number of equal-width time buckets the run's duration is
// divided into for the time-series latency/error report.
const bucketCount = 10

// maxParallelCeiling and maxIntervalCeiling bound the churn knobs from above so
// that valid-but-absurd operator input cannot push the scheduler into runaway
// goroutine growth (a million-wide fan-out) or overflow drawDelay's interval
// span. They sit well above any sane churn setting (the defaults are a fan-out
// of a few and intervals of seconds).
const (
	maxParallelCeiling = 1024
	maxIntervalCeiling = time.Hour
)

// Config holds the resolved churn knobs. MinInterval/MaxInterval bound the
// random delay between ticks; MaxParallel caps the per-tick fan-out and, with
// the global Concurrency, the in-flight operation count. ChurnRatio is the
// neutral create bias at equilibrium and TargetFill the population level the
// controller pulls toward. OpTimeout bounds each operation; ExternalNetworkID is
// the network floating IPs and gateways use ("" when the cloud has none).
type Config struct {
	Duration          time.Duration
	MinInterval       time.Duration
	MaxInterval       time.Duration
	MaxParallel       int
	ChurnRatio        float64
	TargetFill        float64
	Concurrency       int
	OpTimeout         time.Duration
	ExternalNetworkID string
}

// Validate checks the merged config (defaults, YAML block, and flag overrides
// combined) for consistency. Unlike the scenario block, it requires a positive
// duration, since by now a flag has had its chance to supply one.
func (c Config) Validate() error {
	if c.Duration <= 0 {
		return fmt.Errorf("chaos duration must be set and positive, got %s", c.Duration)
	}
	if c.MinInterval <= 0 {
		return fmt.Errorf("chaos min-interval must be positive, got %s", c.MinInterval)
	}
	if c.MinInterval > c.MaxInterval {
		return fmt.Errorf("chaos min-interval (%s) must not exceed max-interval (%s)", c.MinInterval, c.MaxInterval)
	}
	if c.MaxInterval > maxIntervalCeiling {
		return fmt.Errorf("chaos max-interval must not exceed %s, got %s", maxIntervalCeiling, c.MaxInterval)
	}
	if c.MaxParallel < 1 || c.MaxParallel > maxParallelCeiling {
		return fmt.Errorf("chaos max-parallel must be between 1 and %d, got %d", maxParallelCeiling, c.MaxParallel)
	}
	if c.Concurrency < 1 {
		return fmt.Errorf("concurrency must be at least 1, got %d", c.Concurrency)
	}
	if c.ChurnRatio < 0 || c.ChurnRatio > 1 {
		return fmt.Errorf("chaos churn-ratio must be between 0 and 1, got %v", c.ChurnRatio)
	}
	if c.TargetFill < 0 || c.TargetFill > 1 {
		return fmt.Errorf("chaos target-fill must be between 0 and 1, got %v", c.TargetFill)
	}
	return nil
}

// Result is the outcome of a churn run: the deterministic decision log, the
// resources still live at the end (for the run record and cleanup), churn
// counters, the population series summary, and per-time-bucket latency/error
// statistics.
type Result struct {
	Decisions  []Decision
	Created    []neutron.Resource
	Creates    int
	Deletes    int
	Cycles     int
	PopMin     int
	PopMax     int
	PopMean    float64
	TargetFill float64
	Buckets    []Bucket
}

// Decision is one scheduled action in the run's deterministic schedule. Action
// is "create", "delete", or "noop"; Kind and Key are empty for a noop.
type Decision struct {
	Offset time.Duration
	Action string
	Kind   neutron.Kind
	Key    string
}

// Bucket summarizes the operations whose decision offset fell in one time slice
// of the run, so latency/error degradation over time is visible.
type Bucket struct {
	Start  time.Duration
	Stats  metrics.Stats
	Errors []metrics.ErrorCount
}

// Run executes a churn/soak run against n, bounded spatially by p and
// temporally by cfg, drawing every decision from a seed taken from the plan.
// It returns when cfg.Duration elapses on clk or ctx is cancelled, after letting
// in-flight operations drain. A non-nil error means the config or plan was
// rejected before any work started; operation-level failures are tolerated and
// reported in the result.
func Run(ctx context.Context, n Neutron, p *plan.Plan, cfg Config, clk Clock) (*Result, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	nodes, err := Build(p, cfg.ExternalNetworkID)
	if err != nil {
		return nil, err
	}
	return newEngine(nodes, n, p.Seed, cfg, clk).run(ctx), nil
}

// op is one in-flight create or delete of a node instance. done is closed when
// the operation finishes (success, failure, or cancellation); the close
// publishes res to any goroutine that reads it after waiting on done.
// deleteFailed is set on a delete op when the cloud delete did not confirm the
// resource was removed (so it may still exist); it is read by liveResources
// after the drain to keep the run record authoritative.
type op struct {
	done         chan struct{}
	res          neutron.Resource
	deleteFailed bool
}

// nodeState is the scheduler's per-node bookkeeping, mutated only by the single
// scheduler goroutine. present is the logical inventory; create points at the
// current instance's create op (the resource source for the node and its
// children); last is the most recent op (create or delete) and serializes a
// node's own operations so its instance history stays linear.
type nodeState struct {
	present bool
	create  *op
	last    *op
}

// outcome records one completed operation for the time-bucketed report.
type outcome struct {
	offset  time.Duration
	latency time.Duration
	success bool
	errKind string
}

// results accumulates operation outcomes and the completed-cycle count from the
// concurrent operation tasks.
type results struct {
	mu       sync.Mutex
	outcomes []outcome
	cycles   int
}

func (r *results) add(o outcome) {
	r.mu.Lock()
	r.outcomes = append(r.outcomes, o)
	r.mu.Unlock()
}

func (r *results) cycle() {
	r.mu.Lock()
	r.cycles++
	r.mu.Unlock()
}

// engine drives one churn run. The fields above the divider are immutable after
// construction; states/present/decisions/population are owned by the single
// scheduler goroutine; res/sem/wg are the synchronization shared with the
// operation tasks.
type engine struct {
	nodes    []Node
	parents  [][]int
	children [][]int
	cfg      Config
	clk      Clock
	rng      *rand.Rand
	n        Neutron

	sem     chan struct{}
	pending chan struct{}
	wg      sync.WaitGroup
	res     *results

	states     []nodeState
	present    int
	decisions  []Decision
	popMin     int
	popMax     int
	popSum     int
	popSamples int
}

// newEngine builds the engine's static graph indices, the concurrency limiter
// (sem), and the scheduler backpressure pool (pending), both sized to limit.
func newEngine(nodes []Node, n Neutron, seed int64, cfg Config, clk Clock) *engine {
	index := make(map[string]int, len(nodes))
	for i, nd := range nodes {
		index[nd.Key] = i
	}
	parents := make([][]int, len(nodes))
	children := make([][]int, len(nodes))
	for i, nd := range nodes {
		for _, pk := range nd.Parents {
			pi, ok := index[pk]
			if !ok {
				continue // Build validated the plan, so every parent resolves.
			}
			parents[i] = append(parents[i], pi)
			children[pi] = append(children[pi], i)
		}
	}

	limit := cfg.MaxParallel
	if cfg.Concurrency < limit {
		limit = cfg.Concurrency
	}
	if limit < 1 {
		limit = 1
	}

	return &engine{
		nodes:    nodes,
		parents:  parents,
		children: children,
		cfg:      cfg,
		clk:      clk,
		rng:      rand.New(rand.NewSource(seed)),
		n:        n,
		sem:      make(chan struct{}, limit),
		pending:  make(chan struct{}, limit),
		res:      &results{},
		states:   make([]nodeState, len(nodes)),
	}
}

// run is the single-threaded scheduler loop: until the duration elapses (or the
// context is cancelled), it sleeps a random delay then dispatches a random
// fan-out of decisions, each transitioning the logical inventory and launching a
// bounded, retrying cloud operation. After the loop it lets in-flight work drain
// and assembles the result.
func (e *engine) run(ctx context.Context) *Result {
	start := e.clk.Now()
	for e.clk.Now().Sub(start) < e.cfg.Duration {
		if ctx.Err() != nil {
			break
		}
		if err := e.clk.Sleep(ctx, e.drawDelay()); err != nil {
			break // context cancelled
		}
		offset := e.clk.Now().Sub(start)
		fanout := 1 + e.rng.Intn(e.cfg.MaxParallel)
		for i := 0; i < fanout; i++ {
			e.step(ctx, offset)
		}
	}
	e.wg.Wait()
	return e.result()
}

// drawDelay draws the inter-tick delay uniformly from [MinInterval, MaxInterval].
func (e *engine) drawDelay() time.Duration {
	span := int64(e.cfg.MaxInterval - e.cfg.MinInterval)
	return e.cfg.MinInterval + time.Duration(e.rng.Int63n(span+1))
}

// step makes and dispatches one churn decision. It picks a create or a delete
// from the currently valid candidates, biased by the controller, transitions the
// logical inventory, records the decision, and launches the operation. With no
// valid action it records a no-op.
func (e *engine) step(ctx context.Context, offset time.Duration) {
	creates := e.createCandidates()
	deletes := e.deleteCandidates()

	var action string
	var idx int
	switch {
	case len(creates) == 0 && len(deletes) == 0:
		e.decisions = append(e.decisions, Decision{Offset: offset, Action: "noop"})
		e.samplePopulation()
		return
	case len(deletes) == 0:
		action, idx = "create", creates[e.rng.Intn(len(creates))]
	case len(creates) == 0:
		action, idx = "delete", deletes[e.rng.Intn(len(deletes))]
	case e.rng.Float64() < e.pCreate():
		action, idx = "create", creates[e.rng.Intn(len(creates))]
	default:
		action, idx = "delete", deletes[e.rng.Intn(len(deletes))]
	}

	nd := e.nodes[idx]
	e.decisions = append(e.decisions, Decision{Offset: offset, Action: action, Kind: nd.Kind, Key: nd.Key})
	if action == "create" {
		e.dispatchCreate(ctx, idx, offset)
	} else {
		e.dispatchDelete(ctx, idx, offset)
	}
	e.samplePopulation()
}

// pCreate is the controller's create probability: the churn ratio plus the gap
// between the target fill and the current fill, clamped to [0,1]. At equilibrium
// (current fill == target fill) it is exactly the churn ratio; below target it
// rises toward 1, above target it falls toward 0.
func (e *engine) pCreate() float64 {
	fill := float64(e.present) / float64(len(e.nodes))
	p := e.cfg.ChurnRatio + (e.cfg.TargetFill - fill)
	switch {
	case p < 0:
		return 0
	case p > 1:
		return 1
	default:
		return p
	}
}

// createCandidates returns the indices of absent nodes whose parents are all
// present — the nodes that may be created without a dependency violation.
func (e *engine) createCandidates() []int {
	var out []int
	for i := range e.nodes {
		if e.states[i].present {
			continue
		}
		ready := true
		for _, pi := range e.parents[i] {
			if !e.states[pi].present {
				ready = false
				break
			}
		}
		if ready {
			out = append(out, i)
		}
	}
	return out
}

// deleteCandidates returns the indices of present nodes whose dependents are all
// absent — the nodes that may be deleted without a dependency violation.
func (e *engine) deleteCandidates() []int {
	var out []int
	for i := range e.nodes {
		if !e.states[i].present {
			continue
		}
		free := true
		for _, ci := range e.children[i] {
			if e.states[ci].present {
				free = false
				break
			}
		}
		if free {
			out = append(out, i)
		}
	}
	return out
}

// samplePopulation records the live-node count after a decision into the
// population series summary.
func (e *engine) samplePopulation() {
	if e.popSamples == 0 || e.present < e.popMin {
		e.popMin = e.present
	}
	if e.popSamples == 0 || e.present > e.popMax {
		e.popMax = e.present
	}
	e.popSum += e.present
	e.popSamples++
}

// dispatchCreate marks node idx present and launches its create. The create
// waits for the node's previous operation (serialization) and its parents'
// creates (so parent cloud ids are resolved) before acquiring a slot.
func (e *engine) dispatchCreate(ctx context.Context, idx int, offset time.Duration) {
	nd := e.nodes[idx]
	st := &e.states[idx]
	newOp := &op{done: make(chan struct{})}

	deps := make([]*op, 0, len(e.parents[idx])+1)
	if st.last != nil {
		deps = append(deps, st.last)
	}
	parentKeys, parentOps := e.parentOps(idx, &deps)

	st.present = true
	st.create = newOp
	st.last = newOp
	e.present++

	e.launch(ctx, newOp, offset, func() {
		if !e.await(ctx, deps) {
			return
		}
		defer func() { <-e.sem }()
		ids := resolveIDs(parentKeys, parentOps)
		if missingParentID(ids) {
			return // a parent's create failed (no cloud id); skip the doomed
			// child create instead of recording a bookkeeping-artifact failure
		}
		t0 := time.Now()
		var res neutron.Resource
		err := executor.WithRetry(ctx, e.cfg.OpTimeout, func(ctx context.Context) error {
			r, createErr := nd.Create(ctx, e.n, ids)
			if createErr != nil {
				return createErr
			}
			res = r
			return nil
		})
		newOp.res = res
		e.res.add(outcome{offset: offset, latency: time.Since(t0), success: err == nil, errKind: classify(err)})
	})
}

// dispatchDelete marks node idx absent and launches its delete. The delete waits
// for the node's create (resource source), its parents' creates (cloud ids for a
// router-interface removal), and the deletes of any former dependents (so a
// parent is never deleted while a child's cloud delete is still in flight).
func (e *engine) dispatchDelete(ctx context.Context, idx int, offset time.Duration) {
	nd := e.nodes[idx]
	st := &e.states[idx]
	newOp := &op{done: make(chan struct{})}
	createOp := st.create // node is present, so this is its current create

	deps := make([]*op, 0, len(e.parents[idx])+len(e.children[idx])+1)
	deps = append(deps, st.last) // == createOp: serializes and provides the resource
	parentKeys, parentOps := e.parentOps(idx, &deps)
	for _, ci := range e.children[idx] {
		cs := &e.states[ci]
		if !cs.present && cs.last != nil {
			deps = append(deps, cs.last)
		}
	}

	st.present = false
	st.last = newOp
	e.present--

	e.launch(ctx, newOp, offset, func() {
		if !e.await(ctx, deps) {
			return
		}
		defer func() { <-e.sem }()
		ids := resolveIDs(parentKeys, parentOps)
		res := createOp.res
		if res.ID == "" {
			return // the create never produced a cloud resource; nothing to delete
		}
		// Assume the resource survives until the delete confirms otherwise, so a
		// failed (or panicking) delete keeps it in the run record rather than
		// leaking it — address scopes can only be reclaimed by recorded id.
		newOp.deleteFailed = true
		t0 := time.Now()
		err := executor.WithRetry(ctx, e.cfg.OpTimeout, func(ctx context.Context) error {
			return nd.Delete(ctx, e.n, ids, res)
		})
		if err != nil && neutron.IsNotFound(err) {
			err = nil // a resource already gone is a successful delete (idempotent)
		}
		newOp.deleteFailed = err != nil
		e.res.add(outcome{offset: offset, latency: time.Since(t0), success: err == nil, errKind: classify(err)})
		if err == nil {
			e.res.cycle() // a create and its delete both succeeded: one full cycle
		}
	})
}

// parentOps gathers node idx's parents' keys and current create ops, appending
// those ops to deps so the caller waits on them before resolving parent ids.
func (e *engine) parentOps(idx int, deps *[]*op) (keys []string, ops []*op) {
	keys = make([]string, len(e.parents[idx]))
	ops = make([]*op, len(e.parents[idx]))
	for j, pi := range e.parents[idx] {
		keys[j] = e.nodes[pi].Key
		ops[j] = e.states[pi].create
		*deps = append(*deps, e.states[pi].create)
	}
	return keys, ops
}

// launch starts an operation goroutine, tracking it on the wait group and
// closing its done channel when it finishes so dependents (and the drain) can
// proceed. The done channel closes even on early cancellation.
//
// It first blocks the scheduler on the pending pool, giving the scheduler
// backpressure: the pool caps how many operations are launched-but-unfinished,
// so the scheduler cannot outrun the worker pool and accumulate parked
// goroutines until the process runs out of memory. While shutting down (ctx
// cancelled) it launches anyway without a token, so the drain and bookkeeping
// stay consistent; the operation then early-returns on the cancelled context.
//
// A deferred recover keeps a panic in a cloud-call path (e.g. a nil-deref on a
// malformed API response) from unwinding the goroutine and crashing the whole
// run before the record is written: it logs the panic, records it as a failed
// operation, and lets the wait group, pending pool, and done channel release as
// usual. Any concurrency slot acquired in work is released by work's own defer
// during the unwind.
func (e *engine) launch(ctx context.Context, o *op, offset time.Duration, work func()) {
	acquired := false
	select {
	case e.pending <- struct{}{}:
		acquired = true
	case <-ctx.Done():
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer close(o.done)
		if acquired {
			defer func() { <-e.pending }()
		}
		defer func() {
			if r := recover(); r != nil {
				slog.Error("chaos operation panicked; recording it as a failed operation",
					"panic", r, "stack", string(debug.Stack()))
				e.res.add(outcome{offset: offset, success: false, errKind: "panic"})
			}
		}()
		work()
	}()
}

// await blocks until every dependency operation has finished, then acquires a
// concurrency slot. It returns false if the context is cancelled first, in which
// case no slot was acquired and the operation must not run. Acquiring the slot
// only after dependencies are satisfied keeps a blocked dependent from holding a
// slot its dependency needs, so the bounded pool cannot deadlock.
func (e *engine) await(ctx context.Context, deps []*op) bool {
	for _, d := range deps {
		select {
		case <-d.done:
		case <-ctx.Done():
			return false
		}
	}
	select {
	case e.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// result assembles the run summary after all operations have drained.
func (e *engine) result() *Result {
	r := &Result{
		Decisions:  e.decisions,
		Created:    e.liveResources(),
		TargetFill: e.cfg.TargetFill,
		PopMin:     e.popMin,
		PopMax:     e.popMax,
	}
	for _, d := range e.decisions {
		switch d.Action {
		case "create":
			r.Creates++
		case "delete":
			r.Deletes++
		}
	}
	if e.popSamples > 0 {
		r.PopMean = float64(e.popSum) / float64(e.popSamples)
	}

	e.res.mu.Lock()
	r.Cycles = e.res.cycles
	outcomes := append([]outcome(nil), e.res.outcomes...)
	e.res.mu.Unlock()
	r.Buckets = e.buckets(outcomes)
	return r
}

// liveResources returns the cloud resources that may still exist at the end of
// the run, the run record's Created list. It runs after the drain, so every
// node's last operation has finished and its outcome is published. A node is
// recorded when it is logically present, or when its last delete did not confirm
// removal: the resource may still be in the cloud, and dropping it would leak a
// kind cleanup can only reclaim by recorded id (address scopes) silently.
func (e *engine) liveResources() []neutron.Resource {
	var live []neutron.Resource
	for i := range e.nodes {
		st := &e.states[i]
		if st.create == nil || st.create.res.ID == "" {
			continue
		}
		if st.present || (st.last != nil && st.last.deleteFailed) {
			live = append(live, st.create.res)
		}
	}
	return live
}

// buckets distributes outcomes into equal-width time buckets over the run's
// duration and summarizes each, exposing latency and error degradation over
// time rather than only an aggregate.
func (e *engine) buckets(outcomes []outcome) []Bucket {
	width := e.cfg.Duration / bucketCount
	if width <= 0 {
		width = 1 // a sub-bucketCount duration: collapse to unit-width buckets
	}

	durs := make([][]time.Duration, bucketCount)
	succeeded := make([]int, bucketCount)
	errs := make([]map[string]int, bucketCount)
	for i := range errs {
		errs[i] = make(map[string]int)
	}
	for _, o := range outcomes {
		b := int(o.offset / width)
		if b >= bucketCount {
			b = bucketCount - 1
		}
		if b < 0 {
			b = 0
		}
		durs[b] = append(durs[b], o.latency)
		if o.success {
			succeeded[b]++
		} else {
			errs[b][o.errKind]++
		}
	}

	buckets := make([]Bucket, bucketCount)
	for i := range buckets {
		attempted := len(durs[i])
		buckets[i] = Bucket{
			Start: time.Duration(i) * width,
			Stats: metrics.Stats{
				Attempted:  attempted,
				Succeeded:  succeeded[i],
				Failed:     attempted - succeeded[i],
				Throughput: float64(succeeded[i]) / width.Seconds(),
				Latency:    metrics.ComputeLatency(durs[i]),
			},
			Errors: sortedErrorCounts(errs[i]),
		}
	}
	return buckets
}

// missingParentID reports whether any resolved parent id is empty. An empty id
// means that parent's create failed (a node logically present with no cloud id),
// so an operation that resolves it can only fail; callers skip such operations
// to keep the cascade of a failed create out of the latency/error report.
func missingParentID(ids map[string]string) bool {
	for _, id := range ids {
		if id == "" {
			return true
		}
	}
	return false
}

// resolveIDs maps each parent key to its created cloud id. It is called after
// the parent ops have finished, so reading their resources is safe.
func resolveIDs(keys []string, ops []*op) map[string]string {
	ids := make(map[string]string, len(keys))
	for i, k := range keys {
		ids[k] = ops[i].res.ID
	}
	return ids
}

// sortedErrorCounts turns an error-kind tally into a slice sorted by kind, for a
// deterministic report.
func sortedErrorCounts(m map[string]int) []metrics.ErrorCount {
	if len(m) == 0 {
		return nil
	}
	out := make([]metrics.ErrorCount, 0, len(m))
	for kind, count := range m {
		out = append(out, metrics.ErrorCount{Kind: kind, Count: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// classify labels an operation error for the per-bucket error breakdown, reusing
// the neutron classification helpers so the labels match the kinds operators
// already see in the metrics report.
func classify(err error) string {
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
