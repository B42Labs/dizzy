package chaos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeClock is a virtual clock: Sleep advances time instantly and records the
// requested delay, so the schedule is deterministic and the drawn delays can be
// inspected. Only the scheduler goroutine touches it.
type fakeClock struct {
	cur    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{cur: time.Unix(0, 0)} }

func (c *fakeClock) Now() time.Time { return c.cur }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.sleeps = append(c.sleeps, d)
	c.cur = c.cur.Add(d)
	return nil
}

// validConfig is a minimal well-formed churn config the config-validation cases
// mutate one field of at a time.
func validConfig() Config {
	return Config{
		Duration:    2 * time.Second,
		MinInterval: 10 * time.Millisecond,
		MaxInterval: 40 * time.Millisecond,
		MaxParallel: 4,
		ChurnRatio:  0.5,
		TargetFill:  0.7,
		Concurrency: 8,
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	// Each case violates exactly one rule of Config.Validate, including the upper
	// ceilings that keep absurd-but-typed operator input from driving the
	// scheduler into runaway fan-out or an overflowed interval span. The config
	// is checked before the nodes are touched, so a nil node slice is enough.
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"zero duration", func(c *Config) { c.Duration = 0 }},
		{"non-positive min-interval", func(c *Config) { c.MinInterval = 0 }},
		{"min-interval above max-interval", func(c *Config) { c.MinInterval = c.MaxInterval + time.Millisecond }},
		{"max-interval above ceiling", func(c *Config) { c.MaxInterval = maxIntervalCeiling + time.Minute }},
		{"zero max-parallel", func(c *Config) { c.MaxParallel = 0 }},
		{"max-parallel above ceiling", func(c *Config) { c.MaxParallel = maxParallelCeiling + 1 }},
		{"zero concurrency", func(c *Config) { c.Concurrency = 0 }},
		{"churn-ratio above one", func(c *Config) { c.ChurnRatio = 1.5 }},
		{"target-fill below zero", func(c *Config) { c.TargetFill = -0.1 }},
		{"churn-ratio NaN", func(c *Config) { c.ChurnRatio = math.NaN() }},
		{"target-fill NaN", func(c *Config) { c.TargetFill = math.NaN() }},
		{"resize-ratio NaN", func(c *Config) { c.ResizeRatio = math.NaN() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			if _, err := Run(context.Background(), nil, 7, cfg, newFakeClock()); err == nil {
				t.Fatal("expected Run to reject the config, got nil error")
			}
		})
	}
}

// mutFake is an in-process backend for the engine's mutate-action tests. It
// hands out a fresh cloud id per create, counts creates/deletes/mutations, and
// records how many times each cloud id was mutated so the once-per-lifetime
// bound can be checked. failCreate makes every create fail with no resource.
type mutFake struct {
	mu          sync.Mutex
	nextID      int
	creates     int
	deletes     int
	mutates     int
	mutatesByID map[string]int
	failCreate  bool // create returns no resource and an error
	failReady   bool // create returns a resource but an error (readiness failure)
}

func newMutFake() *mutFake { return &mutFake{mutatesByID: make(map[string]int)} }

func (f *mutFake) create(logical string) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate {
		return resource.Resource{}, errors.New("simulated create failure")
	}
	f.creates++
	f.nextID++
	res := resource.Resource{Kind: "volume", Logical: logical, ID: fmt.Sprintf("id-%d", f.nextID)}
	if f.failReady {
		// The resource exists but the operation failed, as when a volume is
		// created yet never reaches available.
		return res, errors.New("simulated readiness failure")
	}
	return res, nil
}

func (f *mutFake) mutate(res resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutates++
	f.mutatesByID[res.ID]++
	return nil
}

func (f *mutFake) delete(_ resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	return nil
}

// mutableNodes builds n independent, parentless mutable nodes backed by f — the
// shape of a Cinder volume graph without snapshots.
func mutableNodes(f *mutFake, n int) []Node {
	nodes := make([]Node, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("vol-%d", i)
		nodes[i] = Node{
			Key: key, Kind: resource.Kind("volume"),
			Create: func(_ context.Context, _ map[string]string) (resource.Resource, error) {
				return f.create(key)
			},
			Delete: func(_ context.Context, _ map[string]string, res resource.Resource) error {
				return f.delete(res)
			},
			Mutate: func(_ context.Context, _ map[string]string, res resource.Resource) error {
				return f.mutate(res)
			},
		}
	}
	return nodes
}

// plainNodes builds n mutation-free nodes with the same keys/kinds as
// mutableNodes, so a mutable graph's schedule can be compared against a
// non-mutable one of identical shape.
func plainNodes(f *mutFake, n int) []Node {
	nodes := mutableNodes(f, n)
	for i := range nodes {
		nodes[i].Mutate = nil
	}
	return nodes
}

// mutConfig is validConfig with a non-zero resize ratio so the mutate gate fires.
func mutConfig() Config {
	c := validConfig()
	c.ResizeRatio = 0.5
	return c
}

// TestRunMutateDeterministicSchedule confirms two runs of a mutable graph with
// the same seed and config draw the identical schedule, including the mutate
// decisions.
func TestRunMutateDeterministicSchedule(t *testing.T) {
	cfg := mutConfig()
	r1, err := Run(context.Background(), mutableNodes(newMutFake(), 4), 7, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	r2, err := Run(context.Background(), mutableNodes(newMutFake(), 4), 7, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if r1.Mutates == 0 {
		t.Fatal("no mutations were scheduled; the test exercises nothing")
	}
	if !reflect.DeepEqual(r1.Decisions, r2.Decisions) {
		t.Error("mutation decision schedules differ for the same seed/config")
	}
}

// TestRunMutateAtMostOncePerLifetime drives a single mutable node and confirms
// no live instance is mutated more than once, while the node is still mutated in
// multiple lifetimes — proving the bound re-arms after delete + re-create.
func TestRunMutateAtMostOncePerLifetime(t *testing.T) {
	f := newMutFake()
	cfg := mutConfig()
	cfg.ResizeRatio = 0.8
	r, err := Run(context.Background(), mutableNodes(f, 1), 7, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Each create hands out a fresh id, so a per-id mutate count above one would
	// mean the same live instance was extended twice.
	for id, n := range f.mutatesByID {
		if n > 1 {
			t.Errorf("cloud id %s mutated %d times, want at most 1 per lifetime", id, n)
		}
	}
	// With a single node, more than one total mutation can only come from a fresh
	// instance being mutated again after a delete + re-create.
	if r.Mutates < 2 {
		t.Fatalf("single-node run mutated %d times; need >= 2 to prove the bound re-arms", r.Mutates)
	}
	// Every drawn mutation reached the cloud (creates always succeed here), each
	// against a distinct lifetime's id.
	if f.mutates != r.Mutates {
		t.Errorf("%d mutations reached the cloud but %d were scheduled", f.mutates, r.Mutates)
	}
}

// TestRunMutateRatioZeroDrawsNothing confirms both closed-gate paths — a mutable
// graph at ResizeRatio 0 and a non-mutable graph at ResizeRatio > 0 — draw no
// mutations and the exact create/delete schedule of a mutation-free run, so the
// gate never perturbs the RNG stream.
func TestRunMutateRatioZeroDrawsNothing(t *testing.T) {
	cfg := validConfig()

	zeroCfg := cfg
	zeroCfg.ResizeRatio = 0
	mutableZero, err := Run(context.Background(), mutableNodes(newMutFake(), 4), 7, zeroCfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run(mutable, ratio 0): %v", err)
	}

	ratioCfg := cfg
	ratioCfg.ResizeRatio = 0.9
	plainRatio, err := Run(context.Background(), plainNodes(newMutFake(), 4), 7, ratioCfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run(plain, ratio 0.9): %v", err)
	}

	if mutableZero.Mutates != 0 || plainRatio.Mutates != 0 {
		t.Fatalf("mutations drawn when the gate should be closed: mutableZero=%d plainRatio=%d", mutableZero.Mutates, plainRatio.Mutates)
	}
	if !reflect.DeepEqual(mutableZero.Decisions, plainRatio.Decisions) {
		t.Error("closed-gate schedules differ; the mutate gate perturbed the create/delete stream")
	}
}

// TestRunMutateIsPopulationNeutral confirms a mutation never changes the live
// population — the live-resource count still equals creates minus deletes — and
// that Result.Mutates matches the mutate decisions in the schedule.
func TestRunMutateIsPopulationNeutral(t *testing.T) {
	f := newMutFake()
	r, err := Run(context.Background(), mutableNodes(f, 5), 7, mutConfig(), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Mutates == 0 {
		t.Fatal("no mutations were scheduled; the test exercises nothing")
	}
	if got := len(r.Created); got != r.Creates-r.Deletes {
		t.Errorf("live resources = %d, want creates-deletes = %d; a mutation changed the population", got, r.Creates-r.Deletes)
	}
	var mutateDecisions int
	for _, d := range r.Decisions {
		if d.Action == "mutate" {
			mutateDecisions++
		}
	}
	if mutateDecisions != r.Mutates {
		t.Errorf("Result.Mutates = %d but the schedule has %d mutate decisions", r.Mutates, mutateDecisions)
	}
	if r.PopMin < 0 || r.PopMax > 5 {
		t.Errorf("population escaped the envelope: min=%d max=%d (nodes=5)", r.PopMin, r.PopMax)
	}
}

// TestGatedFamilyDoesNotHoldSlotWhileParked pins the throughput contract of the
// family gate: an operation waiting behind a busy gate must NOT occupy a
// concurrency slot, so unrelated families keep churning at full concurrency. It
// fails if the gate is ever acquired after the slot (the ordering that let a
// blocked family op sit on a slot and collapse cross-family throughput to
// serial). The existing family-violation check covers mutual exclusion; this one
// covers that the exclusion is not paid for with a stalled slot.
func TestGatedFamilyDoesNotHoldSlotWhileParked(t *testing.T) {
	cfg := validConfig()
	cfg.MaxParallel = 2
	cfg.Concurrency = 2 // two concurrency slots
	e := newEngine(nil, 7, cfg, newFakeClock())
	ctx := context.Background()
	gate := make(chan struct{}, 1)

	// First family op takes the gate and one of the two slots.
	if !e.await(ctx, nil, gate) {
		t.Fatal("first await was not admitted")
	}
	if got := len(e.sem); got != 1 {
		t.Fatalf("slots in use after one admitted op = %d, want 1", got)
	}

	// A second op on the same busy gate parks: the gate is taken before the slot,
	// so it blocks on the held gate and never reaches the slot acquire.
	started := make(chan struct{})
	parked := make(chan bool, 1)
	go func() {
		close(started)
		parked <- e.await(ctx, nil, gate)
	}()
	<-started
	time.Sleep(50 * time.Millisecond) // let the parked op reach its blocking point

	// It must still be parked, holding no slot: the second of the two slots stays
	// free for an unrelated family. If the slot were taken before the gate, this
	// op would sit on a slot while blocked and len(e.sem) would read 2.
	select {
	case <-parked:
		t.Fatal("a same-family op was admitted while the gate was held; the gate is not serializing")
	default:
	}
	if got := len(e.sem); got != 1 {
		t.Fatalf("slots in use while a same-family op is parked = %d, want 1 "+
			"(a parked op must not occupy a concurrency slot)", got)
	}

	// Freeing the first op releases the gate; the parked op then proceeds.
	e.release(gate)
	if !<-parked {
		t.Fatal("the parked op was never admitted after the gate freed")
	}
	e.release(gate) // the formerly parked op's slot and gate
}

// TestRunMutateSkipsFailedCreate confirms a node whose create failed is still
// drawn as a mutate candidate (it is optimistically present) but no mutation
// reaches the cloud, since the failed create published no resource id.
func TestRunMutateSkipsFailedCreate(t *testing.T) {
	f := newMutFake()
	f.failCreate = true
	cfg := mutConfig()
	cfg.ResizeRatio = 0.9

	r, err := Run(context.Background(), mutableNodes(f, 3), 7, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Mutates == 0 {
		t.Fatal("no mutate decisions were drawn; the test exercises nothing")
	}
	if f.mutates != 0 {
		t.Errorf("%d mutations reached the cloud for volumes whose create failed, want 0", f.mutates)
	}
}

// TestRunMutateSkipsNotReadyCreate confirms a node whose create produced a
// resource but failed (a volume that never reached available) is not mutated,
// even though it is optimistically present and carries a cloud id — the failed
// create flag alone gates the extend.
func TestRunMutateSkipsNotReadyCreate(t *testing.T) {
	f := newMutFake()
	f.failReady = true
	cfg := mutConfig()
	cfg.ResizeRatio = 0.9

	r, err := Run(context.Background(), mutableNodes(f, 3), 7, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Mutates == 0 {
		t.Fatal("no mutate decisions were drawn; the test exercises nothing")
	}
	if f.mutates != 0 {
		t.Errorf("%d mutations reached the cloud for volumes that never became ready, want 0", f.mutates)
	}
}

// blockingNode is a single parentless node whose create signals started and
// then waits for release before returning a resource with a cloud id.
func blockingNode(started chan<- struct{}, release <-chan struct{}) Node {
	return Node{
		Key: "vol-0", Kind: resource.Kind("volume"),
		Create: func(context.Context, map[string]string) (resource.Resource, error) {
			started <- struct{}{}
			<-release
			return resource.Resource{Kind: "volume", Logical: "vol-0", ID: "id-0"}, nil
		},
		Delete: func(context.Context, map[string]string, resource.Resource) error { return nil },
	}
}

// TestSlotStaysOpenWhileOperationInFlight confirms a bucket is not sealed while
// one of its operations is still running, even after the scheduler has moved
// on to the next bucket: the late outcome lands in the bucket of its decision
// offset, and the next advance seals it.
func TestSlotStaysOpenWhileOperationInFlight(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	e := newEngine([]Node{blockingNode(started, release)}, 7, validConfig(), newFakeClock())
	ctx := context.Background()
	width := validConfig().Duration / bucketCount

	e.res.advance(0)
	e.dispatchCreate(ctx, 0, 0)
	<-started

	e.res.advance(width + width/2) // the scheduler is in bucket 1 now
	if e.res.slots[0].sealed {
		t.Fatal("bucket 0 was sealed while its create was still in flight")
	}

	close(release)
	<-e.states[0].create.done
	if got := e.res.buckets()[0].Stats.Attempted; got != 1 {
		t.Errorf("bucket 0 attempted = %d after the create finished, want 1", got)
	}
	if got := e.res.buckets()[1].Stats.Attempted; got != 0 {
		t.Errorf("bucket 1 attempted = %d, want 0: the outcome belongs to its decision bucket", got)
	}

	e.res.advance(width + width/2)
	if s := e.res.slots[0]; !s.sealed || s.latencies != nil {
		t.Errorf("bucket 0 sealed=%v latencies=%v after the next advance, want sealed with no raw data", s.sealed, s.latencies)
	}
	if got := e.res.buckets()[0].Stats.Attempted; got != 1 {
		t.Errorf("sealed bucket 0 attempted = %d, want 1", got)
	}
}

// TestPanicCountsInDecisionBucket confirms a panicking operation is counted as
// one failed operation of kind "panic" in the bucket of its decision offset,
// and that it no longer counts as in flight once it is done.
func TestPanicCountsInDecisionBucket(t *testing.T) {
	node := Node{
		Key: "vol-0", Kind: resource.Kind("volume"),
		Create: func(context.Context, map[string]string) (resource.Resource, error) {
			panic("malformed response")
		},
		Delete: func(context.Context, map[string]string, resource.Resource) error { return nil },
	}
	e := newEngine([]Node{node}, 7, validConfig(), newFakeClock())
	offset := 3 * validConfig().Duration / bucketCount

	e.res.advance(offset)
	e.dispatchCreate(context.Background(), 0, offset)
	<-e.states[0].create.done

	b := e.res.buckets()[3]
	if b.Stats.Attempted != 1 || b.Stats.Failed != 1 {
		t.Errorf("bucket 3 stats = %+v, want 1 attempted / 1 failed", b.Stats)
	}
	if want := []metrics.ErrorCount{{Kind: "panic", Count: 1}}; !reflect.DeepEqual(b.Errors, want) {
		t.Errorf("bucket 3 errors = %+v, want %+v", b.Errors, want)
	}
	e.res.mu.Lock()
	inFlight := e.res.slots[3].inFlight
	e.res.mu.Unlock()
	if inFlight != 0 {
		t.Errorf("bucket 3 in-flight count = %d after the panic, want 0", inFlight)
	}
}

// cancelClock is a fakeClock that ends an unbounded run: once limit of virtual
// time has passed, its Sleep cancels the run's context and returns the
// context's error without advancing.
type cancelClock struct {
	*fakeClock
	start  time.Time
	limit  time.Duration
	cancel context.CancelFunc
}

func newCancelClock(limit time.Duration, cancel context.CancelFunc) *cancelClock {
	c := newFakeClock()
	return &cancelClock{fakeClock: c, start: c.cur, limit: limit, cancel: cancel}
}

func (c *cancelClock) Sleep(ctx context.Context, d time.Duration) error {
	if c.cur.Sub(c.start) >= c.limit {
		c.cancel()
		return ctx.Err()
	}
	return c.fakeClock.Sleep(ctx, d)
}

// unboundedConfig is validConfig as an unbounded run with hourly buckets and
// intervals long enough that hours of virtual time take a few hundred ticks.
func unboundedConfig() Config {
	c := validConfig()
	c.Unbounded = true
	c.Duration = 0
	c.BucketWidth = time.Hour
	c.MinInterval = 30 * time.Second
	c.MaxInterval = 2 * time.Minute
	return c
}

// TestConfigValidate covers the rules that select and shape an unbounded run:
// its duration must be 0 and its bucket width set, a bucket width must be at
// least a minute in either mode, a bounded run still needs a positive
// duration, and a checkpoint callback needs a positive interval.
func TestConfigValidate(t *testing.T) {
	unbounded := func(c *Config) {
		c.Unbounded = true
		c.Duration = 0
		c.BucketWidth = time.Hour
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid bounded", mutate: func(*Config) {}},
		{name: "valid unbounded", mutate: unbounded},
		{name: "bounded zero duration", mutate: func(c *Config) { c.Duration = 0 }, wantErr: "chaos duration must be set and positive"},
		{name: "unbounded with a duration", mutate: func(c *Config) { unbounded(c); c.Duration = time.Minute }, wantErr: "must be 0 for an unbounded run"},
		{name: "unbounded without bucket width", mutate: func(c *Config) { unbounded(c); c.BucketWidth = 0 }, wantErr: "bucket-width must be set"},
		{name: "unbounded narrow bucket width", mutate: func(c *Config) { unbounded(c); c.BucketWidth = 30 * time.Second }, wantErr: "bucket-width must be at least 1m0s"},
		{name: "bounded narrow bucket width", mutate: func(c *Config) { c.BucketWidth = 30 * time.Second }, wantErr: "bucket-width must be at least 1m0s"},
		{name: "checkpoint without interval", mutate: func(c *Config) { c.OnCheckpoint = func(*Result) {} }, wantErr: "checkpoint interval must be positive"},
		{name: "checkpoint with interval", mutate: func(c *Config) { c.OnCheckpoint, c.CheckpointInterval = func(*Result) {}, time.Minute }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestRunUnboundedStopsOnCancel confirms an unbounded run has no end of its
// own: it returns a result and no error once its context is cancelled, on the
// virtual clock and on the real one, and not before.
func TestRunUnboundedStopsOnCancel(t *testing.T) {
	t.Run("virtual clock", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r, err := Run(ctx, plainNodes(newMutFake(), 4), 7, unboundedConfig(), newCancelClock(10*time.Minute, cancel))
		if err != nil || r == nil {
			t.Fatalf("Run = %v, %v; want a result and a nil error", r, err)
		}
		if r.Creates == 0 {
			t.Error("the unbounded run created nothing in 10 virtual minutes")
		}
	})

	t.Run("real clock", func(t *testing.T) {
		cfg := unboundedConfig()
		cfg.MinInterval, cfg.MaxInterval = time.Millisecond, 5*time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type outcome struct {
			r   *Result
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			r, err := Run(ctx, plainNodes(newMutFake(), 4), 7, cfg, RealClock{})
			done <- outcome{r, err}
		}()

		select {
		case <-done:
			t.Fatal("unbounded Run returned before its context was cancelled")
		case <-time.After(200 * time.Millisecond):
		}
		cancel()
		select {
		case got := <-done:
			if got.err != nil || got.r == nil {
				t.Errorf("Run = %v, %v after cancel; want a result and a nil error", got.r, got.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("unbounded Run did not return within 5s of the cancel")
		}
	})
}

// TestStopLetsUnboundedCallInFlightFinish confirms the stop signal of an
// unbounded run, its normal end, lets a create already in flight finish with
// its resource and count as a success, while the cancel of a bounded run, an
// interruption, still cuts the call short.
func TestStopLetsUnboundedCallInFlightFinish(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		wantFailed int
		wantID     string
	}{
		{name: "unbounded run stopped", cfg: unboundedConfig(), wantFailed: 0, wantID: "id-0"},
		{name: "bounded run interrupted", cfg: validConfig(), wantFailed: 1, wantID: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			node := Node{
				Key: "vol-0", Kind: resource.Kind("volume"),
				Create: func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
					started <- struct{}{}
					<-release
					if err := ctx.Err(); err != nil {
						return resource.Resource{}, err
					}
					return resource.Resource{Kind: "volume", Logical: "vol-0", ID: "id-0"}, nil
				},
				Delete: func(context.Context, map[string]string, resource.Resource) error { return nil },
			}
			e := newEngine([]Node{node}, 7, tc.cfg, newFakeClock())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			e.res.advance(0)
			e.dispatchCreate(ctx, 0, 0)
			<-started
			cancel()
			close(release)
			e.wg.Wait()

			if got := e.res.buckets()[0].Stats.Failed; got != tc.wantFailed {
				t.Errorf("bucket 0 failed = %d, want %d", got, tc.wantFailed)
			}
			if got := e.states[0].create.res.ID; got != tc.wantID {
				t.Errorf("created id = %q, want %q", got, tc.wantID)
			}
		})
	}
}

// TestNoCallStartsAfterUnboundedStop confirms an operation dispatched once an
// unbounded run is stopped never reaches the cloud and holds neither its family
// gate nor a slot, even though both are free: an admitted call would ignore the
// stop and run to completion. One dispatch could pass by chance, since select
// picks at random among ready cases; a hundred cannot.
func TestNoCallStartsAfterUnboundedStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	creates := 0
	gate := make(chan struct{}, 1)
	node := Node{
		Key: "vol-0", Kind: resource.Kind("volume"), Gate: gate,
		Create: func(context.Context, map[string]string) (resource.Resource, error) {
			creates++
			return resource.Resource{Kind: "volume", Logical: "vol-0", ID: "id-0"}, nil
		},
		Delete: func(context.Context, map[string]string, resource.Resource) error { return nil },
	}
	for i := 0; i < 100; i++ {
		e := newEngine([]Node{node}, 7, unboundedConfig(), newFakeClock())
		e.res.advance(0)
		e.dispatchCreate(ctx, 0, 0)
		e.wg.Wait()
		if len(gate) != 0 || len(e.sem) != 0 {
			t.Fatalf("dispatch %d left gate=%d slots=%d held, want none", i, len(gate), len(e.sem))
		}
	}
	if creates != 0 {
		t.Errorf("%d creates reached the cloud after the stop, want 0", creates)
	}
}

// TestRunUnboundedMatchesBoundedSchedule confirms an unbounded run stopped
// after 10 virtual minutes makes the same decisions as a bounded 10-minute
// run of the same seed and nodes, while keeping no decision log and slicing
// its series at the configured width.
func TestRunUnboundedMatchesBoundedSchedule(t *testing.T) {
	bcfg := mutConfig()
	bcfg.Duration = 10 * time.Minute
	bcfg.MinInterval, bcfg.MaxInterval = time.Second, 5*time.Second
	bounded, err := Run(context.Background(), mutableNodes(newMutFake(), 4), 7, bcfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run(bounded): %v", err)
	}

	ucfg := bcfg
	ucfg.Unbounded, ucfg.Duration, ucfg.BucketWidth = true, 0, time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unbounded, err := Run(ctx, mutableNodes(newMutFake(), 4), 7, ucfg, newCancelClock(10*time.Minute, cancel))
	if err != nil {
		t.Fatalf("Run(unbounded): %v", err)
	}

	if bounded.Mutates == 0 {
		t.Fatal("no mutations were scheduled; the comparison exercises less than it should")
	}
	type summary struct {
		Creates, Deletes, Mutates, PopMin, PopMax int
		PopMean                                   float64
	}
	sum := func(r *Result) summary {
		return summary{r.Creates, r.Deletes, r.Mutates, r.PopMin, r.PopMax, r.PopMean}
	}
	if sum(bounded) != sum(unbounded) {
		t.Errorf("unbounded summary %+v differs from bounded %+v", sum(unbounded), sum(bounded))
	}
	if unbounded.Decisions != nil || unbounded.BucketWidth != time.Hour {
		t.Errorf("unbounded Decisions=%d BucketWidth=%s, want nil and 1h", len(unbounded.Decisions), unbounded.BucketWidth)
	}
	if len(bounded.Decisions) == 0 || bounded.BucketWidth != 0 || len(bounded.Buckets) != bucketCount {
		t.Errorf("bounded Decisions=%d BucketWidth=%s buckets=%d, want a log, 0 and %d",
			len(bounded.Decisions), bounded.BucketWidth, len(bounded.Buckets), bucketCount)
	}
}

// TestRunUnboundedFixedWidthBuckets confirms an unbounded run slices its series
// at the bucket width, one bucket per width reached including the partial last
// one, and that the buckets count every operation that ran.
func TestRunUnboundedFixedWidthBuckets(t *testing.T) {
	f := newMutFake()
	cfg := unboundedConfig()
	cfg.ResizeRatio = 0.5
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := Run(ctx, mutableNodes(f, 4), 7, cfg, newCancelClock(3*time.Hour+10*time.Minute, cancel))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(r.Buckets) != 4 {
		t.Fatalf("got %d buckets, want 4", len(r.Buckets))
	}
	attempted := 0
	for i, b := range r.Buckets {
		if want := time.Duration(i) * time.Hour; b.Start != want {
			t.Errorf("bucket %d starts at %s, want %s", i, b.Start, want)
		}
		attempted += b.Stats.Attempted
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if calls := f.creates + f.deletes + f.mutates; attempted != calls {
		t.Errorf("buckets count %d operations, want the %d closure calls that returned", attempted, calls)
	}
}

// TestResultsAdvanceAppendsEmptyBuckets confirms the series of an unbounded run
// keeps a bucket for every width reached, even one without operations.
func TestResultsAdvanceAppendsEmptyBuckets(t *testing.T) {
	r := newResults(Config{Unbounded: true, BucketWidth: time.Hour})
	if got := r.buckets(); got != nil {
		t.Errorf("buckets() before the first advance = %+v, want nil", got)
	}
	r.advance(0)
	r.advance(3 * time.Hour)

	got := r.buckets()
	if len(got) != 4 {
		t.Fatalf("got %d buckets, want 4", len(got))
	}
	for i, b := range got {
		if b.Start != time.Duration(i)*time.Hour || b.Stats.Attempted != 0 || b.Stats.Latency != (metrics.Latency{}) {
			t.Errorf("bucket %d = %+v, want an empty bucket starting at %dh", i, b, i)
		}
	}
}

// TestRunUnboundedCancelledBeforeStart confirms an unbounded run whose context
// is already cancelled returns an empty result with no buckets and writes no
// checkpoint.
func TestRunUnboundedCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := unboundedConfig()
	checkpoints := 0
	cfg.CheckpointInterval = time.Nanosecond
	cfg.OnCheckpoint = func(*Result) { checkpoints++ }
	r, err := Run(ctx, plainNodes(newMutFake(), 4), 7, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if checkpoints != 0 {
		t.Errorf("OnCheckpoint was called %d times, want 0", checkpoints)
	}
	if r.Creates != 0 || r.Deletes != 0 || r.Mutates != 0 || r.Cycles != 0 {
		t.Errorf("counters = %d/%d/%d/%d, want all zero", r.Creates, r.Deletes, r.Mutates, r.Cycles)
	}
	if r.Buckets != nil || len(r.Created) != 0 {
		t.Errorf("buckets=%+v created=%+v, want none", r.Buckets, r.Created)
	}
}

// TestRunUnboundedReleasesRawData confirms a long unbounded run keeps raw
// latencies only for the bucket that can still receive outcomes and keeps no
// decision log. A pool of one finishes each operation before the next launch,
// and the half hour past six hours gives the last bucket several ticks, so
// every earlier bucket is sealed by the end.
func TestRunUnboundedReleasesRawData(t *testing.T) {
	cfg := unboundedConfig()
	cfg.MaxParallel, cfg.Concurrency = 1, 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := newEngine(plainNodes(newMutFake(), 4), 7, cfg, newCancelClock(6*time.Hour+30*time.Minute, cancel))
	e.run(ctx)

	if len(e.res.slots) != 7 {
		t.Fatalf("got %d slots, want 7", len(e.res.slots))
	}
	for i, s := range e.res.slots[:len(e.res.slots)-1] {
		if !s.sealed || s.latencies != nil || s.errs != nil {
			t.Errorf("slot %d sealed=%v latencies=%d, want sealed with no raw data", i, s.sealed, len(s.latencies))
		}
	}
	if len(e.decisions) != 0 {
		t.Errorf("the unbounded engine logged %d decisions, want 0", len(e.decisions))
	}
	if e.creates == 0 {
		t.Error("the run created nothing; the test exercises nothing")
	}
}

// TestRunCheckpointsOncePerInterval confirms the scheduler hands out a snapshot
// about once per checkpoint interval of virtual time, never twice within one
// interval, and that the snapshots' counters only grow. Nine maximum intervals
// stay under a minute, which keeps the count at 9 or 10 over 10 minutes.
func TestRunCheckpointsOncePerInterval(t *testing.T) {
	cfg := validConfig()
	cfg.Duration = 10 * time.Minute
	cfg.MinInterval, cfg.MaxInterval = time.Second, 2*time.Second
	cfg.CheckpointInterval = time.Minute
	clk := newFakeClock()
	var at []time.Time
	var creates []int
	cfg.OnCheckpoint = func(r *Result) {
		at = append(at, clk.Now())
		creates = append(creates, r.Creates)
		if r.Decisions != nil {
			t.Error("a checkpoint snapshot carries the decision log")
		}
	}

	if _, err := Run(context.Background(), plainNodes(newMutFake(), 4), 7, cfg, clk); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(at) < 9 || len(at) > 10 {
		t.Fatalf("OnCheckpoint was called %d times in 10 virtual minutes, want 9 or 10", len(at))
	}
	prev := time.Unix(0, 0)
	for i := range at {
		if gap := at[i].Sub(prev); gap < time.Minute {
			t.Errorf("checkpoint %d came %s after the previous one, want at least 1m", i, gap)
		}
		if i > 0 && creates[i] < creates[i-1] {
			t.Errorf("checkpoint %d has %d creates, fewer than the %d before it", i, creates[i], creates[i-1])
		}
		prev = at[i]
	}
}

// TestCheckpointOmitsCreateInFlight confirms a checkpoint taken while a create
// is still running does not list its resource (it has no cloud id yet), and
// that the final result does once the create has finished.
func TestCheckpointOmitsCreateInFlight(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	cfg := validConfig()
	cfg.MaxParallel, cfg.Concurrency = 1, 1
	cfg.CheckpointInterval = time.Nanosecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var snap *Result
	cfg.OnCheckpoint = func(r *Result) {
		if snap != nil {
			return
		}
		snap = r
		<-started
		close(release)
		cancel()
	}

	final, err := Run(ctx, []Node{blockingNode(started, release)}, 7, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if snap == nil {
		t.Fatal("OnCheckpoint was never called")
	}
	for _, res := range snap.Created {
		if res.ID == "" || res.Logical == "vol-0" {
			t.Errorf("checkpoint lists %+v, want neither the in-flight create nor an empty id", res)
		}
	}
	if len(final.Created) != 1 || final.Created[0].ID != "id-0" {
		t.Errorf("final Created = %+v, want the finished create id-0", final.Created)
	}
}

// TestStopCheckpointsBeforeDrain confirms a stopped run hands out a checkpoint
// before it waits for the create still in flight, which in an unbounded run
// may take several op timeouts, so a kill during the drain leaves a recent
// record. The interval is far beyond the run, so no periodic checkpoint fires.
func TestStopCheckpointsBeforeDrain(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	cfg := unboundedConfig()
	cfg.CheckpointInterval = 24 * time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var snap *Result
	cfg.OnCheckpoint = func(r *Result) {
		snap = r
		finish()
	}
	go func() {
		<-started
		cancel()
	}()

	done := make(chan *Result, 1)
	go func() {
		r, _ := Run(ctx, []Node{blockingNode(started, release)}, 7, cfg, newFakeClock())
		done <- r
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		finish()
		<-done
		t.Fatal("the stopped run waited for the create in flight without a checkpoint first")
	}
	if snap == nil {
		t.Fatal("OnCheckpoint was never called")
	}
	if snap.Creates == 0 || len(snap.Created) != 0 {
		t.Errorf("checkpoint has %d creates and lists %+v, want the create counted but not listed while in flight",
			snap.Creates, snap.Created)
	}
}

// liveIDs returns the cloud ids of e's live resources, in record order.
func liveIDs(e *engine) []string {
	var ids []string
	for _, r := range e.liveResources() {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestLiveResourcesKeepsInstanceUntilDeleteConfirms confirms the run record
// lists an instance while its delete is in flight, also when the node's
// re-create already waits behind that delete, drops it once the delete
// confirms removal, and keeps it next to its re-created successor when the
// delete fails.
func TestLiveResourcesKeepsInstanceUntilDeleteConfirms(t *testing.T) {
	tests := []struct {
		name      string
		recreate  bool
		deleteErr error
		want      []string // ids listed after the drain
	}{
		{name: "delete confirms", want: nil},
		{name: "delete confirms, node created again", recreate: true, want: []string{"id-1"}},
		{name: "delete fails, node created again", recreate: true, deleteErr: errors.New("conflict"), want: []string{"id-0", "id-1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			creates := 0 // the node's ops run one after another
			node := Node{
				Key: "vol-0", Kind: resource.Kind("volume"),
				Create: func(context.Context, map[string]string) (resource.Resource, error) {
					id := fmt.Sprintf("id-%d", creates)
					creates++
					return resource.Resource{Kind: "volume", Logical: "vol-0", ID: id}, nil
				},
				Delete: func(context.Context, map[string]string, resource.Resource) error {
					started <- struct{}{}
					<-release
					return tc.deleteErr
				},
			}
			e := newEngine([]Node{node}, 7, validConfig(), newFakeClock())
			ctx := context.Background()
			e.res.advance(0)
			e.dispatchCreate(ctx, 0, 0)
			<-e.states[0].create.done
			e.dispatchDelete(ctx, 0, 0)
			<-started
			if tc.recreate {
				e.dispatchCreate(ctx, 0, 0) // waits for the delete in flight
			}

			if got := liveIDs(e); !reflect.DeepEqual(got, []string{"id-0"}) {
				t.Errorf("liveResources with the delete in flight = %v, want [id-0]", got)
			}
			close(release)
			e.wg.Wait()
			if got := liveIDs(e); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("liveResources after the drain = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDeleteCancelledBeforeItRunsKeepsResource confirms a delete whose context
// is cancelled while it waits to be admitted, here behind its busy family
// gate, never reaches the cloud and leaves its resource in the run record.
func TestDeleteCancelledBeforeItRunsKeepsResource(t *testing.T) {
	deletes := 0
	gate := make(chan struct{}, 1)
	node := Node{
		Key: "vol-0", Kind: resource.Kind("volume"), Gate: gate,
		Create: func(context.Context, map[string]string) (resource.Resource, error) {
			return resource.Resource{Kind: "volume", Logical: "vol-0", ID: "id-0"}, nil
		},
		Delete: func(context.Context, map[string]string, resource.Resource) error {
			deletes++
			return nil
		},
	}
	e := newEngine([]Node{node}, 7, validConfig(), newFakeClock())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.res.advance(0)
	e.dispatchCreate(ctx, 0, 0)
	<-e.states[0].create.done

	gate <- struct{}{} // another op of the family holds the gate
	e.dispatchDelete(ctx, 0, 0)
	cancel()
	e.wg.Wait()

	if deletes != 0 {
		t.Fatalf("the cancelled delete reached the cloud %d times, want 0", deletes)
	}
	if got := liveIDs(e); !reflect.DeepEqual(got, []string{"id-0"}) {
		t.Errorf("liveResources after the cancelled delete = %v, want [id-0]", got)
	}
}

// pinnedNodes builds n independent, parentless, pinned and repeatable mutable
// nodes backed by f, the shape of a long-lived graph.
func pinnedNodes(f *mutFake, n int) []Node {
	nodes := mutableNodes(f, n)
	for i := range nodes {
		nodes[i].Pinned = true
	}
	return nodes
}

// countActions counts the decisions of r per action.
func countActions(r *Result) map[string]int {
	n := make(map[string]int)
	for _, d := range r.Decisions {
		n[d.Action]++
	}
	return n
}

// TestRunPinnedNodesCreatedFirstAndKept confirms a graph of pinned nodes gets
// one create per node as its first decisions and never a delete, so every
// node stays live until the run ends.
func TestRunPinnedNodesCreatedFirstAndKept(t *testing.T) {
	for _, n := range []int{1, 4} {
		t.Run(fmt.Sprintf("%d nodes", n), func(t *testing.T) {
			nodes := mutableNodes(newMutFake(), n)
			for i := range nodes {
				nodes[i].Pinned = true
			}
			r, err := Run(context.Background(), nodes, 7, mutConfig(), newFakeClock())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(r.Decisions) <= n {
				t.Fatalf("the run made %d decisions, want more than %d", len(r.Decisions), n)
			}
			keys := make(map[string]bool)
			for i, d := range r.Decisions[:n] {
				if d.Action != "create" {
					t.Errorf("decision %d = %s %s, want a create", i, d.Action, d.Key)
				}
				keys[d.Key] = true
			}
			if len(keys) != n {
				t.Errorf("the first %d decisions created %d distinct nodes, want %d", n, len(keys), n)
			}
			if r.Deletes != 0 || r.PopMax != n || len(r.Created) != n {
				t.Errorf("deletes=%d popMax=%d created=%d, want 0, %d and %d", r.Deletes, r.PopMax, len(r.Created), n, n)
			}
		})
	}
}

// TestRunNeverDeletesPinnedInMixedGraph confirms a graph of pinned and plain
// nodes deletes plain nodes but never a pinned one.
func TestRunNeverDeletesPinnedInMixedGraph(t *testing.T) {
	nodes := plainNodes(newMutFake(), 6)
	pinned := map[string]bool{}
	for i := range nodes[:3] {
		nodes[i].Pinned = true
		pinned[nodes[i].Key] = true
	}
	r, err := Run(context.Background(), nodes, 7, validConfig(), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Deletes == 0 {
		t.Fatal("the run deleted nothing; the test exercises nothing")
	}
	for _, d := range r.Decisions {
		if d.Action == "delete" && pinned[d.Key] {
			t.Errorf("decision at %s deletes the pinned node %s", d.Offset, d.Key)
		}
	}
}

// TestRunKeepsParentOfPinnedNode confirms a plain parent of a pinned node is
// never deleted: the pinned child is created right after the parent, and from
// then on the parent has a present dependent.
func TestRunKeepsParentOfPinnedNode(t *testing.T) {
	nodes := plainNodes(newMutFake(), 3)
	parent, child := nodes[0].Key, nodes[1].Key
	nodes[1].Parents = []string{parent}
	nodes[1].Pinned = true
	r, err := Run(context.Background(), nodes, 7, validConfig(), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Deletes == 0 {
		t.Fatal("the run deleted nothing; the test exercises nothing")
	}
	childCreated := false
	for _, d := range r.Decisions {
		if d.Action == "delete" && (d.Key == parent || d.Key == child) {
			t.Errorf("decision at %s deletes %s, the pinned node or its parent", d.Offset, d.Key)
		}
		childCreated = childCreated || (d.Action == "create" && d.Key == child)
	}
	if !childCreated {
		t.Error("the pinned child was never created")
	}
}

// TestRunPinnedMutatesOneInstanceRepeatedly confirms a pinned node is mutated
// more than once within one instance lifetime, where an unpinned node is
// mutated at most once (TestRunMutateAtMostOncePerLifetime).
func TestRunPinnedMutatesOneInstanceRepeatedly(t *testing.T) {
	f := newMutFake()
	nodes := mutableNodes(f, 1)
	nodes[0].Pinned = true
	cfg := mutConfig()
	cfg.ResizeRatio = 0.8
	if _, err := Run(context.Background(), nodes, 7, cfg, newFakeClock()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	most := 0
	for _, n := range f.mutatesByID {
		most = max(most, n)
	}
	if most < 2 {
		t.Errorf("the most mutations of one cloud id = %d, want at least 2", most)
	}
}

// TestRunPinnedRepeatDeterministicSchedule confirms two runs of a pinned,
// repeatable graph with the same seed and config draw the same schedule.
func TestRunPinnedRepeatDeterministicSchedule(t *testing.T) {
	r1, err := Run(context.Background(), pinnedNodes(newMutFake(), 4), 7, mutConfig(), newFakeClock())
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	r2, err := Run(context.Background(), pinnedNodes(newMutFake(), 4), 7, mutConfig(), newFakeClock())
	if err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if r1.Mutates == 0 {
		t.Fatal("no mutations were scheduled; the test exercises nothing")
	}
	if !reflect.DeepEqual(r1.Decisions, r2.Decisions) {
		t.Error("the schedules of a pinned, repeatable graph differ for the same seed/config")
	}
}

// TestRunZeroNodesOnlyNoops confirms an empty graph only ever records no-ops,
// whatever the mutate probability.
func TestRunZeroNodesOnlyNoops(t *testing.T) {
	for _, ratio := range []float64{0, 0.5, 1} {
		t.Run(fmt.Sprintf("resize ratio %v", ratio), func(t *testing.T) {
			cfg := validConfig()
			cfg.ResizeRatio = ratio
			r, err := Run(context.Background(), []Node{}, 7, cfg, newFakeClock())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(r.Decisions) == 0 {
				t.Fatal("the run made no decisions")
			}
			if got := countActions(r); got["noop"] != len(r.Decisions) {
				t.Errorf("decisions = %v, want only noops", got)
			}
		})
	}
}

// TestRunPinnedFailedCreateIsNotRepaired confirms a pinned node whose create
// failed is neither created again nor deleted, and that its mutations are
// drawn but never reach the cloud.
func TestRunPinnedFailedCreateIsNotRepaired(t *testing.T) {
	f := newMutFake()
	f.failCreate = true
	cfg := mutConfig()
	cfg.ResizeRatio = 0.9
	r, err := Run(context.Background(), pinnedNodes(f, 1), 7, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := countActions(r)
	if got["create"] != 1 || got["delete"] != 0 || got["mutate"] == 0 {
		t.Fatalf("decisions = %v, want one create, no delete and some mutates", got)
	}
	width := cfg.Duration / bucketCount
	createBucket := int(r.Decisions[0].Offset / width)
	for i, b := range r.Buckets {
		want := metrics.Stats{}
		if i == createBucket {
			want = metrics.Stats{Attempted: 1, Failed: 1}
		}
		if b.Stats.Attempted != want.Attempted || b.Stats.Failed != want.Failed {
			t.Errorf("bucket %d attempted/failed = %d/%d, want %d/%d", i, b.Stats.Attempted, b.Stats.Failed, want.Attempted, want.Failed)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mutates != 0 {
		t.Errorf("%d mutations reached the cloud for a node whose create failed, want 0", f.mutates)
	}
}

// TestRunUnboundedPinnedRepeat confirms an unbounded run over pinned,
// repeatable nodes keeps no decision log, mutates and never deletes.
func TestRunUnboundedPinnedRepeat(t *testing.T) {
	f := newMutFake()
	cfg := unboundedConfig()
	cfg.ResizeRatio = 0.5
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := Run(ctx, pinnedNodes(f, 4), 7, cfg, newCancelClock(time.Hour, cancel))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Decisions != nil || r.Mutates == 0 || r.Deletes != 0 {
		t.Errorf("decisions=%d mutates=%d deletes=%d, want nil, above 0 and 0", len(r.Decisions), r.Mutates, r.Deletes)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deletes != 0 {
		t.Errorf("%d deletes reached the cloud, want 0", f.deletes)
	}
}
