// Package pressure runs a closed-loop load against a cloud service: a fixed
// number of workers each send one request after another, drawing every request
// from a weighted list of operations, for a configured duration or, in an
// unbounded run, until the context is cancelled. Unlike the churn engine in
// internal/chaos, it models no resources and keeps no schedule of its own; the
// load is as high as the cloud's answers allow. It records the latency and the
// outcome of each operation in the run total and in the time bucket of the
// operation's start offset, and can hand a snapshot of the result to a
// checkpoint callback at a fixed interval. The engine is service-neutral: a
// per-service builder supplies the operations as closures that capture their
// own cloud client.
package pressure

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/B42Labs/dizzy/internal/metrics"
)

// bucketCount is the number of equal-width time buckets a bounded run's
// duration is divided into, as in the churn engine.
const bucketCount = 10

// minBucketWidth is the narrowest time bucket a run may ask for. It keeps the
// series of an unbounded run, which gains one bucket per width, at no more
// than 1,440 buckets per day.
const minBucketWidth = time.Minute

// maxWorkers bounds the worker count from above, so that valid-but-absurd
// operator input cannot start a runaway number of goroutines. It has the
// value of the churn engine's maxParallelCeiling.
const maxWorkers = 1024

// ErrNoWeight is returned by ValidateOperations when no operation has a
// positive weight, so a worker could never draw one. Callers match it with
// errors.Is.
var ErrNoWeight = errors.New("pressure mix: every weight is 0; at least one operation needs a positive weight")

// Operation is one weighted, repeatable request a worker can draw. A worker
// draws it with probability Weight divided by the sum of all weights, so an
// operation with weight 0 is never drawn. Do makes one attempt; rng is the
// calling worker's own generator, which no other worker shares, so Do may draw
// from it without locking.
type Operation struct {
	Name   string
	Weight int
	Do     func(ctx context.Context, rng *rand.Rand) error
}

// Config holds the resolved pressure knobs.
//
// A bounded run lasts Duration and slices its time series into bucketCount
// equal buckets. Unbounded selects a run without an end: it keeps going until
// its context is cancelled, Duration must be 0, and its time series is sliced
// into buckets of BucketWidth.
//
// Workers is the number of closed-loop workers. Seed derives every worker's
// generators, so the sequence of operations a worker draws depends on nothing
// but Seed and the worker's index. OpTimeout bounds each operation; an
// operation that started before the run ended still gets its full OpTimeout.
// Classify optionally labels an operation error for the error breakdown; when
// nil, a default labels only canceled, timeout and other.
//
// OnCheckpoint, when non-nil, receives a snapshot of the result every
// CheckpointInterval on the wall clock until the run ends. It is called on
// its own goroutine, one call at a time, and must not block on the run.
type Config struct {
	Duration           time.Duration
	Unbounded          bool
	BucketWidth        time.Duration
	Workers            int
	Seed               int64
	OpTimeout          time.Duration
	Classify           func(error) string
	CheckpointInterval time.Duration
	OnCheckpoint       func(*Result)
}

// Validate checks the merged config (defaults, scenario block and flag
// overrides combined) for consistency. Its rules on the duration, the bucket
// width and the checkpoint are those of the churn engine's config, so the two
// verbs reject the same input with the same words.
func (c Config) Validate() error {
	if c.Unbounded && c.Duration != 0 {
		return fmt.Errorf("pressure duration must be 0 for an unbounded run, got %s", c.Duration)
	}
	if !c.Unbounded && c.Duration <= 0 {
		return fmt.Errorf("pressure duration must be set and positive, got %s", c.Duration)
	}
	if c.Unbounded && c.BucketWidth == 0 {
		return errors.New("pressure bucket-width must be set for an unbounded run")
	}
	if c.BucketWidth != 0 && c.BucketWidth < minBucketWidth {
		return fmt.Errorf("pressure bucket-width must be at least %s, got %s", minBucketWidth, c.BucketWidth)
	}
	if c.OnCheckpoint != nil && c.CheckpointInterval <= 0 {
		return fmt.Errorf("pressure checkpoint interval must be positive, got %s", c.CheckpointInterval)
	}
	if c.Workers < 1 || c.Workers > maxWorkers {
		return fmt.Errorf("pressure workers must be between 1 and %d, got %d", maxWorkers, c.Workers)
	}
	if c.OpTimeout <= 0 {
		return fmt.Errorf("pressure op-timeout must be positive, got %s", c.OpTimeout)
	}
	return nil
}

// ValidateOperations checks that ops can be drawn from: the list is not empty,
// every operation has a Do, no weight is negative, no name is listed twice,
// and at least one weight is positive. The weights must also sum to no more
// than the largest int, so the draw cannot overflow.
func ValidateOperations(ops []Operation) error {
	if len(ops) == 0 {
		return errors.New("pressure: no operations")
	}
	seen := make(map[string]bool, len(ops))
	total := 0
	for _, op := range ops {
		if op.Do == nil {
			return fmt.Errorf("pressure operation %q has no Do", op.Name)
		}
		if op.Weight < 0 {
			return fmt.Errorf("pressure operation %q has a negative weight %d", op.Name, op.Weight)
		}
		if seen[op.Name] {
			return fmt.Errorf("pressure operation %q is listed twice", op.Name)
		}
		seen[op.Name] = true
		if op.Weight > math.MaxInt-total {
			return fmt.Errorf("pressure mix: the weights sum past %d", math.MaxInt)
		}
		total += op.Weight
	}
	if total == 0 {
		return ErrNoWeight
	}
	return nil
}

// Result is the outcome of a pressure run: the worker count, every operation's
// outcome over the run, and the same outcomes sliced into time buckets by the
// offset at which each operation started. Operations' Type is the operation
// name. BucketWidth is the width of an unbounded run's buckets and 0 for a
// bounded run, whose buckets are a tenth of its duration.
type Result struct {
	Workers     int
	Operations  metrics.Aggregate
	Buckets     []Bucket
	BucketWidth time.Duration
}

// Bucket summarizes the operations that started in one time slice of the run.
// Operations' Type is the operation name and its Wall is the bucket width, so
// the throughput of the last, possibly partial bucket of an unbounded run
// still divides by the full width.
type Bucket struct {
	Start      time.Duration
	Operations metrics.Aggregate
}

// Run executes a closed-loop pressure run of cfg.Workers workers over ops. It
// validates cfg and ops first and returns the error before any worker starts.
// A bounded run ends after cfg.Duration or when ctx is cancelled, an unbounded
// run only when ctx is cancelled. An operation that started before the end
// finishes or times out on its own, and Run returns only after every worker
// and the checkpoint goroutine have returned. Operation failures are recorded
// in the result, not returned.
func Run(ctx context.Context, ops []Operation, cfg Config) (*Result, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := ValidateOperations(ops); err != nil {
		return nil, err
	}

	runCtx := ctx
	if !cfg.Unbounded {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, cfg.Duration)
		defer cancel()
	}

	e := &engine{
		ops:      ops,
		cfg:      cfg,
		classify: cfg.Classify,
		buckets:  newBuckets(cfg),
		start:    time.Now(),
	}
	if e.classify == nil {
		e.classify = defaultClassify
	}

	var checkpoints sync.WaitGroup
	if cfg.OnCheckpoint != nil {
		checkpoints.Add(1)
		go func() {
			defer checkpoints.Done()
			e.checkpoint(runCtx)
		}()
	}

	var workers sync.WaitGroup
	for _, s := range workerSeeds(cfg.Seed, cfg.Workers) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			e.work(runCtx, newPicker(ops, s.pick), rand.New(rand.NewSource(s.op)))
		}()
	}
	workers.Wait()
	checkpoints.Wait()
	return e.snapshot(), nil
}

// engine drives one pressure run. Its fields are set before the first worker
// starts and never change; the buckets synchronize the workers' records.
type engine struct {
	ops      []Operation
	cfg      Config
	classify func(error) string
	buckets  *buckets
	start    time.Time
}

// work is one worker's closed loop: while the run context has no error, it
// draws an operation with pick, calls it with rng and records the outcome in
// the bucket of the offset at which the call started.
func (e *engine) work(runCtx context.Context, pick *picker, rng *rand.Rand) {
	for runCtx.Err() == nil {
		op := &e.ops[pick.pick()]
		offset := time.Since(e.start)
		e.buckets.record(offset, e.call(runCtx, op, rng))
	}
}

// call makes one attempt of op and returns its sample. The call runs on a
// context detached from the run's end and bounded by OpTimeout, so an
// operation that started before the run ended is not cut off by that end.
func (e *engine) call(runCtx context.Context, op *Operation, rng *rand.Rand) metrics.Sample {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), e.cfg.OpTimeout)
	defer cancel()
	began := time.Now()
	err := op.Do(ctx, rng)
	s := metrics.Sample{Type: op.Name, Duration: time.Since(began), Success: err == nil}
	if err != nil {
		s.ErrKind = e.classify(err)
	}
	return s
}

// checkpoint hands a snapshot to OnCheckpoint every CheckpointInterval until
// the run context is done. A tick that arrives together with the end is
// dropped, so no checkpoint is taken after the run ended.
func (e *engine) checkpoint(runCtx context.Context) {
	ticker := time.NewTicker(e.cfg.CheckpointInterval)
	defer ticker.Stop()
	for {
		select {
		case <-runCtx.Done():
			return
		case <-ticker.C:
			if runCtx.Err() != nil {
				return
			}
			e.cfg.OnCheckpoint(e.snapshot())
		}
	}
}

// snapshot returns the result as it stands now, with the time since the run
// started as the wall clock of the run total.
func (e *engine) snapshot() *Result {
	r := e.buckets.snapshot(time.Since(e.start))
	r.Workers = e.cfg.Workers
	return r
}

// workerSeed holds the two seeds of one worker. pick seeds the generator that
// draws the worker's operations and op the generator handed to Do, so the
// draws of an operation's Do, which may depend on the cloud's answers, never
// shift the sequence of operations the worker draws.
type workerSeed struct {
	pick int64
	op   int64
}

// workerSeeds derives the seeds of workers workers from seed: one generator
// seeded with seed yields a pick seed and then an op seed per worker, in
// worker order. A worker's seeds therefore depend only on seed and its index,
// not on the worker count.
func workerSeeds(seed int64, workers int) []workerSeed {
	src := rand.New(rand.NewSource(seed))
	out := make([]workerSeed, workers)
	for i := range out {
		out[i].pick = src.Int63()
		out[i].op = src.Int63()
	}
	return out
}

// picker draws operation indexes with probability weight divided by the sum
// of the weights, from its own generator. It belongs to one worker and is not
// safe for concurrent use.
type picker struct {
	rng   *rand.Rand
	cum   []int // cum[i] is the sum of the weights of ops[0..i]
	total int
}

// newPicker builds a picker over the weights of ops, which ValidateOperations
// has accepted, with a generator seeded by seed.
func newPicker(ops []Operation, seed int64) *picker {
	p := &picker{rng: rand.New(rand.NewSource(seed)), cum: make([]int, len(ops))}
	for i, op := range ops {
		p.total += op.Weight
		p.cum[i] = p.total
	}
	return p
}

// pick draws one operation index. It returns the first index whose cumulative
// weight exceeds a uniform draw from [0, total), so an operation with weight 0,
// whose cumulative weight equals its predecessor's, is never returned.
func (p *picker) pick() int {
	n := p.rng.Intn(p.total)
	return sort.Search(len(p.cum), func(i int) bool { return p.cum[i] > n })
}

// buckets holds the per-bucket collectors of a run. Every bucket collector is
// a child of one run collector, so a sample recorded into a bucket also lands
// in the run total, and memory stays fixed however many samples arrive. A
// bounded run has bucketCount buckets created up front, and an offset at or
// past the end lands in the last one. An unbounded run appends a bucket of
// width whenever an offset or a snapshot reaches a new multiple of it, so a
// bucket without operations is still present.
type buckets struct {
	run   *metrics.Collector
	width time.Duration
	fixed bool // the bucket count is fixed and the last bucket takes later offsets

	mu    sync.Mutex // guards slots in an unbounded run
	slots []*metrics.Collector
}

// newBuckets builds the bucket collectors for cfg.
func newBuckets(cfg Config) *buckets {
	b := &buckets{run: metrics.NewCollector()}
	if cfg.Unbounded {
		b.width = cfg.BucketWidth
		return b
	}
	b.width = cfg.Duration / bucketCount
	if b.width <= 0 {
		b.width = 1 // a sub-bucketCount duration: collapse to unit-width buckets
	}
	b.fixed = true
	b.slots = make([]*metrics.Collector, bucketCount)
	for i := range b.slots {
		b.slots[i] = b.run.Child()
	}
	return b
}

// index maps an offset onto its bucket index. A bounded run clamps it into
// the fixed buckets; an unbounded run's index may lie past the last bucket.
func (b *buckets) index(offset time.Duration) int {
	i := int(offset / b.width)
	if i < 0 {
		i = 0
	}
	if b.fixed && i >= len(b.slots) {
		i = len(b.slots) - 1
	}
	return i
}

// slot returns the collector of the bucket at offset. In an unbounded run it
// first appends the buckets up to that offset. The caller must not hold mu.
func (b *buckets) slot(offset time.Duration) *metrics.Collector {
	i := b.index(offset)
	if b.fixed {
		return b.slots[i]
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.extend(i)
	return b.slots[i]
}

// extend appends empty buckets until index i exists. The caller holds mu.
func (b *buckets) extend(i int) {
	for len(b.slots) <= i {
		b.slots = append(b.slots, b.run.Child())
	}
}

// record adds the sample of an operation that started at offset to that
// offset's bucket and, through it, to the run total.
func (b *buckets) record(offset time.Duration, s metrics.Sample) {
	b.slot(offset).Record(s)
}

// snapshot builds a Result from the samples recorded so far, without Workers:
// the run total over elapsed and one Bucket per bucket over the bucket width.
// An unbounded run first appends the buckets up to elapsed. It is safe to call
// while workers still record.
func (b *buckets) snapshot(elapsed time.Duration) *Result {
	slots := b.slots
	if !b.fixed {
		b.mu.Lock()
		b.extend(b.index(elapsed))
		slots = append([]*metrics.Collector(nil), b.slots...)
		b.mu.Unlock()
	}
	r := &Result{Operations: b.run.Aggregate(elapsed), Buckets: make([]Bucket, len(slots))}
	if !b.fixed {
		r.BucketWidth = b.width
	}
	for i, c := range slots {
		r.Buckets[i] = Bucket{Start: time.Duration(i) * b.width, Operations: c.Aggregate(b.width)}
	}
	return r
}

// defaultClassify labels an operation error when Config.Classify is nil. It
// covers only the service-agnostic outcomes, with the labels of the churn
// engine's default.
func defaultClassify(err error) string {
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
