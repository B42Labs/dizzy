// Package pressure runs a closed-loop load against a cloud service: a fixed
// number of workers each send one request after another, drawing every request
// from a weighted list of operations, for a configured duration or, in an
// unbounded run, until the context is cancelled. Unlike the churn engine in
// internal/chaos, it models no resources and keeps no schedule of its own; the
// load is as high as the cloud's answers allow. It records the latency and the
// outcome of each operation in the run total and in the time bucket of the
// operation's start offset, and can hand a snapshot of the result to a
// checkpoint callback at a fixed interval. A bounded run may be divided into
// stages: each is entered by a callback that runs while no operation is in
// flight, and the run then also records each operation in the stage it
// started in. The engine is service-neutral: a per-service builder supplies
// the operations and the stage callbacks as closures that capture their own
// cloud client.
package pressure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
//
// Stages, when set, divide a bounded run into consecutive stages whose
// durations add up to Duration. Stage i owns the window of offsets from the
// end of stage i-1's window, 0 for the first stage, to that plus its own
// Duration. A worker draws only while its offset lies inside the current
// stage's window; past it, the worker parks until the next stage is entered,
// so no operation is in flight while that stage's Enter runs. Enter's wall
// time comes out of its stage's window, and the run still lasts Duration.
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
	Stages             []Stage
}

// Stage is one stage of a staged run: the workers run Duration in it, and
// Enter, when non-nil, runs at its start while no operation is in flight. The
// first stage's Enter runs before the first worker starts, every later one
// once every worker has parked at the end of the previous stage's window.
// Enter receives the run context and must return once that context ends.
type Stage struct {
	Duration time.Duration
	Enter    func(ctx context.Context) error
}

// StageResult is the outcome of one stage. Start is the offset at which the
// workers resumed after Enter returned, End the offset at which the stage's
// window ended, or the run ended. Operations are the operations that
// started in [Start, End); its Wall is End minus Start. A stage without an
// Enter starts with its window, unless an earlier stage held the workers past
// that, and a stage entered after its window ended has Start equal to End.
type StageResult struct {
	Start      time.Duration
	End        time.Duration
	Operations metrics.Aggregate
}

// Validate checks the merged config (defaults, scenario block and flag
// overrides combined) for consistency. Its rules on the duration, the bucket
// width and the checkpoint are those of the churn engine's config, so the two
// verbs reject the same input with the same words. Stages need a bounded run
// and positive durations that add up to Duration exactly.
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
	if len(c.Stages) > 0 && c.Unbounded {
		return errors.New("pressure stages need a bounded run")
	}
	var sum time.Duration
	for i, st := range c.Stages {
		if st.Duration <= 0 {
			return fmt.Errorf("pressure stage %d must have a positive duration, got %s", i+1, st.Duration)
		}
		// The sum saturates rather than wraps, so durations that overflow
		// it cannot wrap around to the run duration and pass.
		if st.Duration > math.MaxInt64-sum {
			sum = math.MaxInt64
		} else {
			sum += st.Duration
		}
	}
	if len(c.Stages) > 0 && sum != c.Duration {
		return fmt.Errorf("pressure stages must add up to the duration: the stages sum to %s, the duration is %s", sum, c.Duration)
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
// bounded run, whose buckets are a tenth of its duration. Stages holds, in
// stage order, the stages a staged run has entered so far, and is empty for a
// run without stages; the run total holds the operations of every stage.
type Result struct {
	Workers     int
	Operations  metrics.Aggregate
	Buckets     []Bucket
	BucketWidth time.Duration
	Stages      []StageResult
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
// finishes or times out on its own, and Run returns only after every worker,
// the stage controller and the checkpoint goroutine have returned. Operation
// failures are recorded in the result, not returned.
//
// A staged run calls the first stage's Enter before the first worker starts,
// and a controller goroutine calls every later stage's Enter at the end of
// the previous stage's window, once every worker has parked. An Enter error
// while the run is still going ends the run early: Run cancels it, waits as
// above and returns the result so far together with the error, wrapped as
// "entering stage N". An Enter that fails after the run ended, for example
// because ctx was cancelled during it, is the end showing through; Run then
// returns the result and no error, as for any interrupted run, and the result
// does not list that stage. When the run's duration ran out during the
// entry, Run also logs a warning that the stage is not recorded; a
// cancellation of ctx, such as a signal, is not logged.
func Run(ctx context.Context, ops []Operation, cfg Config) (*Result, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := ValidateOperations(ops); err != nil {
		return nil, err
	}

	// A bounded run's deadline is the engine's start plus Duration, so a
	// run that reaches it has lasted Duration in the offsets the workers
	// and the stages read, and the last stage's window ends with the run.
	start := time.Now()
	runCtx := ctx
	var cancel context.CancelFunc
	if !cfg.Unbounded {
		runCtx, cancel = context.WithDeadline(ctx, start.Add(cfg.Duration))
		defer cancel()
	}

	e := &engine{
		ops:      ops,
		cfg:      cfg,
		classify: cfg.Classify,
		buckets:  newBuckets(cfg),
		start:    start,
		cancel:   cancel,
	}
	if e.classify == nil {
		e.classify = defaultClassify
	}
	if len(cfg.Stages) > 0 {
		e.stages = newStages(cfg.Stages, cfg.Workers)
	}

	var checkpoints sync.WaitGroup
	if cfg.OnCheckpoint != nil {
		checkpoints.Add(1)
		go func() {
			defer checkpoints.Done()
			e.checkpoint(runCtx)
		}()
	}

	var (
		control  sync.WaitGroup
		stageErr error
	)
	startWorkers := true
	if e.stages != nil {
		startWorkers, stageErr = e.enterStage(runCtx, 0)
		if startWorkers {
			control.Add(1)
			go func() {
				defer control.Done()
				stageErr = e.control(runCtx)
			}()
		}
	}

	var workers sync.WaitGroup
	if startWorkers {
		for _, s := range workerSeeds(cfg.Seed, cfg.Workers) {
			workers.Add(1)
			go func() {
				defer workers.Done()
				e.work(runCtx, newPicker(ops, s.pick), rand.New(rand.NewSource(s.op)))
			}()
		}
	}
	workers.Wait()
	control.Wait()
	checkpoints.Wait()
	return e.snapshot(), stageErr
}

// engine drives one pressure run. Its fields are set before the first worker
// starts and never change; the buckets synchronize the workers' records and
// the stages, in a staged run, the workers' passage from stage to stage.
type engine struct {
	ops      []Operation
	cfg      Config
	classify func(error) string
	buckets  *buckets
	stages   *stages // nil for a run without stages
	start    time.Time
	cancel   context.CancelFunc // ends a bounded run early; nil for an unbounded run
}

// work is one worker's closed loop: while the run context has no error and
// the worker is admitted to a draw, it draws an operation with pick, calls it
// with rng and records the outcome in the bucket of the offset at which the
// worker was admitted and, in a staged run, in the stage it was admitted to.
func (e *engine) work(runCtx context.Context, pick *picker, rng *rand.Rand) {
	for runCtx.Err() == nil {
		offset, stage, ok := e.admit(runCtx)
		if !ok {
			return
		}
		op := &e.ops[pick.pick()]
		s := e.call(runCtx, op, rng)
		e.buckets.record(offset, s)
		if stage != nil {
			stage.Record(s)
		}
	}
}

// admit returns the offset of a worker's next draw and, in a staged run, the
// collector of the stage the draw belongs to. A run without stages admits at
// once and takes no lock. A staged run admits through its stages, and admit
// reports false when the worker is to stop instead.
func (e *engine) admit(runCtx context.Context) (time.Duration, *metrics.Collector, bool) {
	if e.stages == nil {
		return time.Since(e.start), nil, true
	}
	return e.stages.admit(runCtx, e.start)
}

// control is the stage controller of a staged run. For every stage after the
// first it waits until the previous stage's window has ended and, when the
// stage has an Enter, until every worker has parked, then enters the stage.
// It returns the stage error Run is to return, or nil once the last stage was
// entered or the run context ended.
func (e *engine) control(runCtx context.Context) error {
	for i := 1; i < len(e.cfg.Stages); i++ {
		if !sleepUntil(runCtx, e.start.Add(e.stages.ends[i-1])) {
			return nil
		}
		if e.cfg.Stages[i].Enter != nil && !e.stages.waitParked(runCtx) {
			return nil
		}
		if ok, err := e.enterStage(runCtx, i); !ok {
			return err
		}
	}
	return nil
}

// enterStage runs stage i's Enter, when it has one, and then releases the
// workers into stage i. It reports false when Enter failed, which ends the
// run: when the run context had no error yet, enterStage cancels the run and
// returns the error wrapped with the stage's 1-based number; when it had, the
// failure is the run's end showing through Enter and the error is nil. The
// stage is then not recorded, since its Enter did not finish. When the run
// ended by its own deadline, enterStage logs that the stage is not recorded;
// a cancellation of the parent context, such as a signal, stays silent.
func (e *engine) enterStage(runCtx context.Context, i int) (bool, error) {
	var took time.Duration
	enter := e.cfg.Stages[i].Enter
	if enter != nil {
		began := time.Now()
		err := enter(runCtx)
		took = time.Since(began)
		if err != nil {
			if runErr := runCtx.Err(); runErr != nil {
				if errors.Is(runErr, context.DeadlineExceeded) {
					slog.Warn("the run ended while a stage was being entered; the stage is not recorded", "stage", i+1, "took", took, "error", err)
				}
				return false, nil
			}
			e.cancel()
			return false, fmt.Errorf("entering stage %d: %w", i+1, err)
		}
	}
	e.stages.begin(i, time.Since(e.start), took, enter != nil)
	return true, nil
}

// sleepUntil blocks until t or until ctx ends, and reports whether ctx is
// still live, so a t that arrives together with the end does not count.
func sleepUntil(ctx context.Context, t time.Time) bool {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
	return ctx.Err() == nil
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
	if e.stages != nil {
		r.Stages = e.stages.snapshot(e.start)
	}
	return r
}

// stages holds the state of a staged run. Stage i owns the offsets from
// windowStart(i) to ends[i]. The current stage is the last one entered, and a
// worker is admitted to a draw only while its offset lies inside the current
// stage's window. Past it, the worker stops when the current stage is the
// last; otherwise it parks until the controller enters the next stage. Every
// stage records into a collector of its own, which is not a child of the run
// collector, so the run total does not count a sample twice.
type stages struct {
	ends       []time.Duration      // ends[i] is the offset at which stage i's window ends
	collectors []*metrics.Collector // one per stage, created up front
	workers    int

	mu        sync.Mutex
	entered   int             // the number of stages entered; the current one is entered-1
	starts    []time.Duration // starts[i] is stage i's Start once it was entered
	released  time.Duration   // the offset at which the workers were last released
	parked    int             // the workers parked since the current stage was entered
	allParked chan struct{}   // closed once parked reaches workers
	resume    chan struct{}   // closed when the next stage is entered
}

// newStages builds the state of a run of workers workers over the stages of
// a validated config, before any stage is entered.
func newStages(cfg []Stage, workers int) *stages {
	s := &stages{
		ends:       make([]time.Duration, len(cfg)),
		collectors: make([]*metrics.Collector, len(cfg)),
		workers:    workers,
		starts:     make([]time.Duration, len(cfg)),
		allParked:  make(chan struct{}),
		resume:     make(chan struct{}),
	}
	var end time.Duration
	for i, st := range cfg {
		end += st.Duration
		s.ends[i] = end
		s.collectors[i] = metrics.NewCollector()
	}
	return s
}

// windowStart returns the offset at which stage i's window starts: 0 for the
// first stage, the end of the previous stage's window for every later one.
func (s *stages) windowStart(i int) time.Duration {
	if i == 0 {
		return 0
	}
	return s.ends[i-1]
}

// admit admits a worker to its next draw. It takes the worker's offset under
// the lock, so the offset of a draw admitted to a stage is never below that
// stage's Start, and returns it with the current stage's collector while the
// offset lies inside the current stage's window. Past the window it reports
// false when the current stage is the last, so the worker stops; otherwise
// the worker parks until the next stage is entered and is admitted anew, or
// until the run context ends, which also stops it, even when the next stage
// was entered at the same time.
func (s *stages) admit(runCtx context.Context, start time.Time) (time.Duration, *metrics.Collector, bool) {
	for {
		s.mu.Lock()
		offset := time.Since(start)
		cur := s.entered - 1
		if offset < s.ends[cur] {
			c := s.collectors[cur]
			s.mu.Unlock()
			return offset, c, true
		}
		if cur == len(s.ends)-1 {
			s.mu.Unlock()
			return 0, nil, false
		}
		s.parked++
		if s.parked == s.workers {
			close(s.allParked)
		}
		resume := s.resume
		s.mu.Unlock()
		select {
		case <-resume:
		case <-runCtx.Done():
		}
		if runCtx.Err() != nil {
			return 0, nil, false
		}
	}
}

// waitParked blocks until every worker has parked at the end of the current
// stage's window, or until the run context ends, and reports whether the run
// context is still live, which then means every worker parked.
func (s *stages) waitParked(runCtx context.Context) bool {
	s.mu.Lock()
	parked := s.allParked
	s.mu.Unlock()
	select {
	case <-parked:
	case <-runCtx.Done():
	}
	return runCtx.Err() == nil
}

// begin enters stage i and releases the parked workers into it. offset is
// the offset at which they are released and took the time stage i's Enter
// ran. The stage's Start is offset. A stage without an Enter holds no worker
// back, so its Start is its window's start instead, or the offset of the last
// release when an earlier stage held the workers past that. A Start at or past
// the window's end is clamped to it and logged, since the stage then records
// no operations.
func (s *stages) begin(i int, offset, took time.Duration, hasEnter bool) {
	s.mu.Lock()
	start := offset
	if !hasEnter {
		start = max(s.windowStart(i), s.released)
	}
	late := start >= s.ends[i]
	if late {
		start = s.ends[i]
	}
	s.starts[i] = start
	s.entered = i + 1
	s.released = offset
	s.parked = 0
	s.allParked = make(chan struct{})
	close(s.resume)
	s.resume = make(chan struct{})
	s.mu.Unlock()
	if late {
		slog.Warn("stage entered after its window ended; it records no operations", "stage", i+1, "took", took)
	}
}

// snapshot returns one StageResult per stage entered so far. A stage's End is
// the end of its window, or the time since start while the window still
// runs, and its operations are aggregated over End minus Start. It is safe to
// call while workers still record.
func (s *stages) snapshot(start time.Time) []StageResult {
	s.mu.Lock()
	elapsed := time.Since(start)
	starts := append([]time.Duration(nil), s.starts[:s.entered]...)
	s.mu.Unlock()
	out := make([]StageResult, len(starts))
	for i, st := range starts {
		end := min(elapsed, s.ends[i])
		out[i] = StageResult{Start: st, End: end, Operations: s.collectors[i].Aggregate(end - st)}
	}
	return out
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
