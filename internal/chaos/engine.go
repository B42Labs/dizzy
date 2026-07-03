// Package chaos runs a random churn/soak load against an OpenStack service.
// Instead of building a topology once and stopping, it keeps creating and
// deleting resources at random, seeded intervals and parallelism for a
// configured duration, using a scenario plan as the spatial envelope: the live
// population never exceeds the plan's resource set, and only planned resources
// whose parents exist are ever created. It also draws an optional mutate action
// — an in-place change of a live instance (e.g. a volume extend) that is neither
// a create nor a delete, bounded to at most once per instance lifetime. The
// schedule of decisions is deterministic for a given seed and config, while the
// concurrent cloud-call completion order is not. The engine is service-neutral:
// per-service builders under subpackages (neutrongraph, cindergraph) turn a plan
// into the create/delete/mutate closures it schedules, each capturing its own
// cloud client, so nothing in the engine names a specific service.
package chaos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/B42Labs/openstack-tester/internal/metrics"
	"github.com/B42Labs/openstack-tester/internal/resource"
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
// controller pulls toward. ResizeRatio is the per-step probability of drawing a
// mutation (a live node's in-place change, e.g. a volume extend) instead of a
// create/delete, and is only consulted when the graph has mutable nodes.
// Classify optionally labels an operation error for the per-bucket error
// breakdown; a service builder sets it to its own classifier so the labels
// match the metrics report, and when nil a minimal default labels only
// canceled/timeout/other. Per-operation retry and timeout are the builder's
// concern, captured inside the node closures.
type Config struct {
	Duration    time.Duration
	MinInterval time.Duration
	MaxInterval time.Duration
	MaxParallel int
	ChurnRatio  float64
	TargetFill  float64
	ResizeRatio float64
	Concurrency int
	Classify    func(error) string
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
	if math.IsNaN(c.ChurnRatio) || c.ChurnRatio < 0 || c.ChurnRatio > 1 {
		return fmt.Errorf("chaos churn-ratio must be between 0 and 1, got %v", c.ChurnRatio)
	}
	if math.IsNaN(c.TargetFill) || c.TargetFill < 0 || c.TargetFill > 1 {
		return fmt.Errorf("chaos target-fill must be between 0 and 1, got %v", c.TargetFill)
	}
	if math.IsNaN(c.ResizeRatio) || c.ResizeRatio < 0 || c.ResizeRatio > 1 {
		return fmt.Errorf("chaos resize-ratio must be between 0 and 1, got %v", c.ResizeRatio)
	}
	return nil
}

// Result is the outcome of a churn run: the deterministic decision log, the
// resources still live at the end (for the run record and cleanup), churn
// counters, the population series summary, and per-time-bucket latency/error
// statistics.
type Result struct {
	Decisions  []Decision
	Created    []resource.Resource
	Creates    int
	Deletes    int
	Mutates    int
	Cycles     int
	PopMin     int
	PopMax     int
	PopMean    float64
	TargetFill float64
	Buckets    []Bucket
}

// Decision is one scheduled action in the run's deterministic schedule. Action
// is "create", "delete", "mutate", or "noop"; Kind and Key are empty for a noop.
type Decision struct {
	Offset time.Duration
	Action string
	Kind   resource.Kind
	Key    string
}

// Bucket summarizes the operations whose decision offset fell in one time slice
// of the run, so latency/error degradation over time is visible.
type Bucket struct {
	Start  time.Duration
	Stats  metrics.Stats
	Errors []metrics.ErrorCount
}

// Node is one planned resource in the churn graph: a unit the engine can create
// and later delete. Key uniquely identifies it; Parents lists the keys of the
// nodes whose creation it depends on (and which must outlive it). Create and
// Delete are closures a per-service builder supplies, each capturing its own
// cloud client; they resolve parent cloud ids from ids (keyed by parent key,
// which equals the parent's plan logical name). A create returns the cloud
// identity of the resource; a delete removes it. Mutate, when non-nil, is an
// in-place change of a live instance (e.g. a volume extend to its planned
// target) that is neither a create nor a delete; the engine draws it at most
// once per instance lifetime. A nil Mutate marks a node the engine never
// mutates.
//
// Gate, when non-nil, serializes every operation of the nodes that share it: a
// capacity-1 channel the engine acquires before granting a concurrency slot, so
// at most one op per gate is ever in flight. A builder points a family of nodes
// at one gate when the backend rejects concurrent operations on them (e.g. a
// Cinder volume and its snapshots). Because the gate is taken before the slot, an
// op parked behind a busy family never occupies a slot the rest of the pool could
// use. A nil Gate (the common case) imposes no cross-node serialization.
type Node struct {
	Key     string
	Kind    resource.Kind
	Parents []string
	Gate    chan struct{}
	Create  func(ctx context.Context, ids map[string]string) (resource.Resource, error)
	Delete  func(ctx context.Context, ids map[string]string, res resource.Resource) error
	Mutate  func(ctx context.Context, ids map[string]string, res resource.Resource) error
}

// Run executes a churn/soak run over nodes, bounded temporally by cfg and
// drawing every decision from seed. nodes come from a per-service builder, each
// carrying create/delete closures that capture their cloud client. It returns
// when cfg.Duration elapses on clk or ctx is cancelled, after letting in-flight
// operations drain. A non-nil error means the config was rejected before any
// work started; operation-level failures are tolerated and reported in the
// result.
func Run(ctx context.Context, nodes []Node, seed int64, cfg Config, clk Clock) (*Result, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return newEngine(nodes, seed, cfg, clk).run(ctx), nil
}

// op is one in-flight create or delete of a node instance. done is closed when
// the operation finishes (success, failure, or cancellation); the close
// publishes res to any goroutine that reads it after waiting on done. failed is
// set on a create op when the create closure returned an error, so a doomed
// child (whose parent create failed) is skipped even when the failed create
// still published a partial resource. deleteFailed is set on a delete op when
// the cloud delete did not confirm the resource was removed (so it may still
// exist); it is read by liveResources after the drain to keep the run record
// authoritative.
type op struct {
	done         chan struct{}
	res          resource.Resource
	failed       bool
	deleteFailed bool
}

// nodeState is the scheduler's per-node bookkeeping, mutated only by the single
// scheduler goroutine. present is the logical inventory; create points at the
// current instance's create op (the resource source for the node and its
// children); last is the most recent op (create, delete, or mutate) and
// serializes a node's own operations so its instance history stays linear.
// mutated records that the current instance has already been mutated, so the
// engine draws at most one mutation per lifetime; it is re-armed when the node
// is created again.
type nodeState struct {
	present bool
	mutated bool
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
	mutable  bool

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
func newEngine(nodes []Node, seed int64, cfg Config, clk Clock) *engine {
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

	// A graph is mutable when any node carries a Mutate closure. A non-mutable
	// graph (every Neutron graph) never consults ResizeRatio, so its RNG stream —
	// and thus its decision schedule — is untouched by the mutate action.
	mutable := false
	for _, nd := range nodes {
		if nd.Mutate != nil {
			mutable = true
			break
		}
	}

	return &engine{
		nodes:    nodes,
		parents:  parents,
		children: children,
		cfg:      cfg,
		clk:      clk,
		rng:      rand.New(rand.NewSource(seed)),
		mutable:  mutable,
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

// step makes and dispatches one churn decision. When the graph is mutable it
// first draws, with probability ResizeRatio, a mutation of a live, not-yet-
// mutated node; otherwise it picks a create or a delete from the currently valid
// candidates, biased by the controller. It transitions the logical inventory,
// records the decision, and launches the operation. With no valid action it
// records a no-op.
//
// The mutate draw is double-gated on mutable and ResizeRatio so a non-mutable
// graph never touches the RNG here, keeping the create/delete decision schedule
// for such a graph byte-for-byte what it was before mutations existed.
func (e *engine) step(ctx context.Context, offset time.Duration) {
	if e.mutable && e.cfg.ResizeRatio > 0 && e.rng.Float64() < e.cfg.ResizeRatio {
		if cands := e.mutateCandidates(); len(cands) > 0 {
			idx := cands[e.rng.Intn(len(cands))]
			nd := e.nodes[idx]
			e.decisions = append(e.decisions, Decision{Offset: offset, Action: "mutate", Kind: nd.Kind, Key: nd.Key})
			slog.Info("churn mutate", "kind", nd.Kind, "key", nd.Key, "offset", offset.Round(time.Millisecond))
			e.dispatchMutate(ctx, idx, offset)
			e.samplePopulation()
			return
		}
		// No live, un-mutated candidate this tick: fall through to create/delete.
	}

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
	// Announce each scheduled action so a churn run shows what it is doing
	// instead of going silent until its final report. Logged at info (per
	// action); silence it with --log-level warn. No-ops are not logged.
	slog.Info("churn "+action, "kind", nd.Kind, "key", nd.Key, "offset", offset.Round(time.Millisecond))
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

// mutateCandidates returns the indices of present, mutable nodes whose current
// instance has not yet been mutated — the nodes a mutation may target. A node
// stays a candidate across dependency changes (a mutation does not touch the
// graph), so the only bounds are liveness and the once-per-lifetime flag.
func (e *engine) mutateCandidates() []int {
	var out []int
	for i := range e.nodes {
		if e.states[i].present && e.nodes[i].Mutate != nil && !e.states[i].mutated {
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
	st.mutated = false // a fresh instance re-arms its at-most-once mutation
	st.create = newOp
	st.last = newOp
	e.present++

	e.launch(ctx, newOp, offset, func() {
		if !e.await(ctx, deps, nd.Gate) {
			return
		}
		defer e.release(nd.Gate)
		if parentOpFailed(parentOps) {
			return // a parent's create failed (or produced no cloud id); skip the
			// doomed child create instead of recording a bookkeeping-artifact failure
		}
		ids := resolveIDs(parentKeys, parentOps)
		t0 := time.Now()
		// Publish whatever the closure returned even on failure: a create that
		// yielded a resource but failed readiness (a cinder volume stuck at
		// error) must stay deletable and in the run record, while a create that
		// yielded nothing publishes a zero resource. failed skips its doomed
		// children regardless of whether an id was produced.
		res, err := nd.Create(ctx, ids)
		newOp.res = res
		newOp.failed = err != nil
		e.res.add(outcome{offset: offset, latency: time.Since(t0), success: err == nil, errKind: e.classify(err)})
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
		if !e.await(ctx, deps, nd.Gate) {
			return
		}
		defer e.release(nd.Gate)
		ids := resolveIDs(parentKeys, parentOps)
		res := createOp.res
		if res.ID == "" {
			return // the create never produced a cloud resource; nothing to delete
		}
		// Assume the resource survives until the delete confirms otherwise, so a
		// failed (or panicking) delete keeps it in the run record rather than
		// leaking it — address scopes can only be reclaimed by recorded id. The
		// delete closure owns retry and already-gone (404) tolerance.
		newOp.deleteFailed = true
		t0 := time.Now()
		err := nd.Delete(ctx, ids, res)
		newOp.deleteFailed = err != nil
		e.res.add(outcome{offset: offset, latency: time.Since(t0), success: err == nil, errKind: e.classify(err)})
		if err == nil {
			e.res.cycle() // a create and its delete both succeeded: one full cycle
		}
	})
}

// dispatchMutate marks node idx mutated and launches its in-place change. It
// mirrors dispatchDelete minus the population change and the dependent-delete
// waits: the mutation waits for the node's previous operation (serialization and
// resource source) and its parents' creates (cloud ids). Marking mutated at
// decision time bounds attempts, not just successes, so the schedule stays
// deterministic even when the mutation fails; the population is untouched, so
// the churn controller's economics are unchanged.
func (e *engine) dispatchMutate(ctx context.Context, idx int, offset time.Duration) {
	nd := e.nodes[idx]
	st := &e.states[idx]
	newOp := &op{done: make(chan struct{})}
	createOp := st.create // node is present, so this is its current create

	deps := make([]*op, 0, len(e.parents[idx])+1)
	deps = append(deps, st.last) // serializes this node's operations
	parentKeys, parentOps := e.parentOps(idx, &deps)

	st.mutated = true
	st.last = newOp

	e.launch(ctx, newOp, offset, func() {
		if !e.await(ctx, deps, nd.Gate) {
			return
		}
		defer e.release(nd.Gate)
		if parentOpFailed(parentOps) {
			return
		}
		res := createOp.res
		if createOp.failed || res.ID == "" {
			return // the create failed (e.g. never reached available) or produced no
			// resource; a not-ready instance cannot be reliably mutated
		}
		ids := resolveIDs(parentKeys, parentOps)
		t0 := time.Now()
		err := nd.Mutate(ctx, ids, res)
		e.res.add(outcome{offset: offset, latency: time.Since(t0), success: err == nil, errKind: e.classify(err)})
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

// await blocks until every dependency operation has finished, then takes the
// node's family gate (if any) and a concurrency slot. It returns false if the
// context is cancelled first, in which case nothing was acquired and the
// operation must not run. Acquiring the slot only after dependencies are
// satisfied keeps a blocked dependent from holding a slot its dependency needs,
// so the bounded pool cannot deadlock. Taking the gate before the slot keeps an
// op parked behind a busy family from occupying a slot: only the family's running
// op holds one, leaving the pool free for other families to make progress.
func (e *engine) await(ctx context.Context, deps []*op, gate chan struct{}) bool {
	for _, d := range deps {
		select {
		case <-d.done:
		case <-ctx.Done():
			return false
		}
	}
	if gate != nil {
		select {
		case gate <- struct{}{}:
		case <-ctx.Done():
			return false
		}
	}
	select {
	case e.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		if gate != nil {
			<-gate // release the gate we took; no slot was acquired
		}
		return false
	}
}

// release returns the concurrency slot and, when the node has a family gate, the
// gate — the mirror of a successful await, run once per admitted operation. The
// slot is returned before the gate, the reverse of the acquire order.
func (e *engine) release(gate chan struct{}) {
	<-e.sem
	if gate != nil {
		<-gate
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
		case "mutate":
			r.Mutates++
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
func (e *engine) liveResources() []resource.Resource {
	var live []resource.Resource
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

// parentOpFailed reports whether any parent's create op failed or produced no
// cloud id. Such a parent cannot support a child operation, so callers skip the
// child to keep the cascade of a failed create out of the latency/error report.
func parentOpFailed(ops []*op) bool {
	for _, o := range ops {
		if o.failed || o.res.ID == "" {
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

// classify labels an operation error for the per-bucket error breakdown. It
// defers to the service-supplied Config.Classify when set so the labels match
// the kinds operators already see in that service's metrics report; otherwise a
// minimal default covers only the service-agnostic outcomes.
func (e *engine) classify(err error) string {
	if e.cfg.Classify != nil {
		return e.cfg.Classify(err)
	}
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "other"
	}
}
