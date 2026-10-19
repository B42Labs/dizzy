package pressure

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/metrics"
)

// validConfig is a minimal well-formed bounded config the validation cases
// mutate one field of at a time.
func validConfig() Config {
	return Config{
		Duration:  time.Second,
		Workers:   4,
		Seed:      7,
		OpTimeout: time.Second,
	}
}

// noop is an operation body that succeeds at once.
func noop(context.Context, *rand.Rand) error { return nil }

// sleepFor returns an operation body that waits d or until its context ends.
func sleepFor(d time.Duration) func(context.Context, *rand.Rand) error {
	return func(ctx context.Context, _ *rand.Rand) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// TestConfigValidate covers every rule of Config.Validate with its exact
// message, in the order the rules are checked, and the configs it accepts.
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
		{name: "valid checkpoint", mutate: func(c *Config) { c.OnCheckpoint, c.CheckpointInterval = func(*Result) {}, time.Minute }},
		{name: "valid upper worker bound", mutate: func(c *Config) { c.Workers = 1024 }},
		{name: "valid lower worker bound", mutate: func(c *Config) { c.Workers = 1 }},
		{
			name:    "unbounded with a duration",
			mutate:  func(c *Config) { unbounded(c); c.Duration = time.Minute },
			wantErr: "pressure duration must be 0 for an unbounded run, got 1m0s",
		},
		{
			name:    "bounded zero duration",
			mutate:  func(c *Config) { c.Duration = 0 },
			wantErr: "pressure duration must be set and positive, got 0s",
		},
		{
			name:    "bounded negative duration",
			mutate:  func(c *Config) { c.Duration = -time.Second },
			wantErr: "pressure duration must be set and positive, got -1s",
		},
		{
			name:    "unbounded without bucket width",
			mutate:  func(c *Config) { unbounded(c); c.BucketWidth = 0 },
			wantErr: "pressure bucket-width must be set for an unbounded run",
		},
		{
			name:    "unbounded narrow bucket width",
			mutate:  func(c *Config) { unbounded(c); c.BucketWidth = 30 * time.Second },
			wantErr: "pressure bucket-width must be at least 1m0s, got 30s",
		},
		{
			name:    "bounded narrow bucket width",
			mutate:  func(c *Config) { c.BucketWidth = 30 * time.Second },
			wantErr: "pressure bucket-width must be at least 1m0s, got 30s",
		},
		{
			name:    "checkpoint without interval",
			mutate:  func(c *Config) { c.OnCheckpoint = func(*Result) {} },
			wantErr: "pressure checkpoint interval must be positive, got 0s",
		},
		{
			name:    "zero workers",
			mutate:  func(c *Config) { c.Workers = 0 },
			wantErr: "pressure workers must be between 1 and 1024, got 0",
		},
		{
			name:    "workers above the ceiling",
			mutate:  func(c *Config) { c.Workers = 1025 },
			wantErr: "pressure workers must be between 1 and 1024, got 1025",
		},
		{
			name:    "zero op-timeout",
			mutate:  func(c *Config) { c.OpTimeout = 0 },
			wantErr: "pressure op-timeout must be positive, got 0s",
		},
		{
			name:    "negative op-timeout",
			mutate:  func(c *Config) { c.OpTimeout = -time.Second },
			wantErr: "pressure op-timeout must be positive, got -1s",
		},
		{
			name:    "duration is checked before workers",
			mutate:  func(c *Config) { c.Duration = 0; c.Workers = 0 },
			wantErr: "pressure duration must be set and positive, got 0s",
		},
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
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("Validate() = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateOperations covers every rule of ValidateOperations with its
// exact message, and that an all-zero mix returns ErrNoWeight itself.
func TestValidateOperations(t *testing.T) {
	tests := []struct {
		name    string
		ops     []Operation
		wantErr string
		wantIs  error
	}{
		{name: "valid", ops: []Operation{{Name: "validate", Weight: 3, Do: noop}, {Name: "revoke", Weight: 0, Do: noop}}},
		{name: "nil list", ops: nil, wantErr: "pressure: no operations"},
		{name: "empty list", ops: []Operation{}, wantErr: "pressure: no operations"},
		{
			name:    "nil Do",
			ops:     []Operation{{Name: "validate", Weight: 1, Do: noop}, {Name: "revoke", Weight: 1}},
			wantErr: `pressure operation "revoke" has no Do`,
		},
		{
			name:    "negative weight",
			ops:     []Operation{{Name: "validate", Weight: -2, Do: noop}},
			wantErr: `pressure operation "validate" has a negative weight -2`,
		},
		{
			name:    "duplicate name",
			ops:     []Operation{{Name: "validate", Weight: 1, Do: noop}, {Name: "validate", Weight: 2, Do: noop}},
			wantErr: `pressure operation "validate" is listed twice`,
		},
		{
			name:    "every weight is zero",
			ops:     []Operation{{Name: "validate", Do: noop}, {Name: "revoke", Do: noop}},
			wantErr: "pressure mix: every weight is 0; at least one operation needs a positive weight",
			wantIs:  ErrNoWeight,
		},
		{
			name:    "weights overflow",
			ops:     []Operation{{Name: "validate", Weight: math.MaxInt, Do: noop}, {Name: "revoke", Weight: 1, Do: noop}},
			wantErr: fmt.Sprintf("pressure mix: the weights sum past %d", math.MaxInt),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateOperations(tc.ops)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateOperations() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("ValidateOperations() = %v, want %q", err, tc.wantErr)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("errors.Is(%v, %v) = false, want true", err, tc.wantIs)
			}
		})
	}
}

// TestRunRejectsInvalidInputBeforeAnyCall confirms Run returns a config or
// operation error before any worker starts, so no operation is called.
func TestRunRejectsInvalidInputBeforeAnyCall(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		ops     func(do func(context.Context, *rand.Rand) error) []Operation
		wantErr string
	}{
		{
			name:   "zero workers",
			mutate: func(c *Config) { c.Workers = 0 },
			ops: func(do func(context.Context, *rand.Rand) error) []Operation {
				return []Operation{{Name: "validate", Weight: 1, Do: do}}
			},
			wantErr: "pressure workers must be between 1 and 1024, got 0",
		},
		{
			name:    "no operations",
			mutate:  func(*Config) {},
			ops:     func(func(context.Context, *rand.Rand) error) []Operation { return nil },
			wantErr: "pressure: no operations",
		},
		{
			name:   "nil Do",
			mutate: func(*Config) {},
			ops: func(do func(context.Context, *rand.Rand) error) []Operation {
				return []Operation{{Name: "revoke", Weight: 1, Do: do}, {Name: "validate", Weight: 1}}
			},
			wantErr: `pressure operation "validate" has no Do`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int64
			do := func(context.Context, *rand.Rand) error {
				calls.Add(1)
				return nil
			}
			cfg := validConfig()
			tc.mutate(&cfg)
			res, err := Run(context.Background(), tc.ops(do), cfg)
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("Run() error = %v, want %q", err, tc.wantErr)
			}
			if res != nil {
				t.Errorf("Run() result = %+v, want nil", res)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("operations called %d times, want 0", n)
			}
		})
	}
}

// drawnNames runs a one-worker run with seed until the worker has drawn n
// operations and returns their names in draw order. extraDraws is how many
// values each Do draws from its generator, standing in for an operation whose
// use of the generator depends on the cloud's answers.
func drawnNames(t *testing.T, ops []Operation, seed int64, n, extraDraws int) []string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var names []string // only the single worker appends, and Run's return orders the read
	wrapped := make([]Operation, len(ops))
	for i, op := range ops {
		name := op.Name
		wrapped[i] = op
		wrapped[i].Do = func(_ context.Context, rng *rand.Rand) error {
			for range extraDraws {
				rng.Int63()
			}
			if len(names) < n {
				names = append(names, name)
			}
			if len(names) == n {
				cancel()
			}
			return nil
		}
	}
	cfg := Config{Duration: time.Hour, Workers: 1, Seed: seed, OpTimeout: time.Second}
	if _, err := Run(ctx, wrapped, cfg); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(names) != n {
		t.Fatalf("drew %d operations, want %d", len(names), n)
	}
	return names
}

// TestWorkerDrawsArePureFunctionOfSeed confirms the operations worker 0 draws
// depend only on the seed and its index: two runs with the same seed draw the
// same 1000 names, also when Do draws from its generator in one run and not
// in the other, the names equal those of a picker built from the derived pick
// seed, and another seed draws a different sequence.
func TestWorkerDrawsArePureFunctionOfSeed(t *testing.T) {
	const n = 1000
	ops := []Operation{
		{Name: "validate", Weight: 60, Do: noop},
		{Name: "effective", Weight: 5, Do: noop},
		{Name: "revoke", Weight: 2, Do: noop},
		{Name: "grant_churn", Weight: 2, Do: noop},
		{Name: "issue", Weight: 1, Do: noop},
	}

	first := drawnNames(t, ops, 42, n, 0)
	second := drawnNames(t, ops, 42, n, 0)
	if !reflect.DeepEqual(first, second) {
		t.Error("two runs with seed 42 drew different sequences")
	}
	busy := drawnNames(t, ops, 42, n, 3)
	if !reflect.DeepEqual(first, busy) {
		t.Error("a Do that draws from its generator changed the drawn sequence")
	}

	p := newPicker(ops, workerSeeds(42, 1)[0].pick)
	for i := range n {
		if got := ops[p.pick()].Name; got != first[i] {
			t.Fatalf("draw %d: picker drew %q, run drew %q", i, got, first[i])
		}
	}

	if other := drawnNames(t, ops, 43, n, 0); reflect.DeepEqual(first, other) {
		t.Error("seeds 42 and 43 drew the same sequence")
	}
}

// TestWorkerSeedsDependOnIndexOnly confirms a worker's seeds do not change
// with the worker count and differ between workers.
func TestWorkerSeedsDependOnIndexOnly(t *testing.T) {
	few := workerSeeds(42, 2)
	many := workerSeeds(42, 8)
	if !reflect.DeepEqual(few, many[:2]) {
		t.Errorf("workerSeeds(42, 2) = %v, want the first two of workerSeeds(42, 8) %v", few, many[:2])
	}
	seen := make(map[int64]bool)
	for i, s := range many {
		for _, v := range []int64{s.pick, s.op} {
			if seen[v] {
				t.Errorf("worker %d reuses seed %d", i, v)
			}
			seen[v] = true
		}
	}
}

// TestZeroWeightOperationNeverDrawn confirms an operation with weight 0 is
// never drawn, by the picker over many draws and by a short run.
func TestZeroWeightOperationNeverDrawn(t *testing.T) {
	var zeroCalls atomic.Int64
	ops := []Operation{
		{Name: "never-first", Weight: 0, Do: func(context.Context, *rand.Rand) error { zeroCalls.Add(1); return nil }},
		{Name: "validate", Weight: 60, Do: sleepFor(100 * time.Microsecond)},
		{Name: "never-middle", Weight: 0, Do: func(context.Context, *rand.Rand) error { zeroCalls.Add(1); return nil }},
		{Name: "issue", Weight: 1, Do: sleepFor(100 * time.Microsecond)},
	}

	p := newPicker(ops, 7)
	for range 100000 {
		if i := p.pick(); ops[i].Weight == 0 {
			t.Fatalf("picker drew %q, which has weight 0", ops[i].Name)
		}
	}

	cfg := validConfig()
	cfg.Duration = 30 * time.Millisecond
	res, err := Run(context.Background(), ops, cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if n := zeroCalls.Load(); n != 0 {
		t.Errorf("operations with weight 0 were called %d times, want 0", n)
	}
	if res.Operations.Overall.Attempted == 0 {
		t.Error("the run recorded no operation")
	}
	for _, s := range res.Operations.ByType {
		if s.Type != "validate" && s.Type != "issue" {
			t.Errorf("the run recorded type %q, want only validate and issue", s.Type)
		}
	}
}

// TestRunBoundedBuckets confirms a bounded run has ten buckets a tenth of its
// duration wide, a BucketWidth of 0, buckets that together hold every
// operation of the run total, and that each operation is filed under the
// offset at which it started, not the one at which it finished.
func TestRunBoundedBuckets(t *testing.T) {
	var (
		mu      sync.Mutex
		entered []time.Duration // each Do's entry offset from start
	)
	start := time.Now()
	sleep := sleepFor(time.Millisecond)
	do := func(ctx context.Context, rng *rand.Rand) error {
		mu.Lock()
		entered = append(entered, time.Since(start))
		mu.Unlock()
		return sleep(ctx, rng)
	}
	ops := []Operation{
		{Name: "validate", Weight: 1, Do: do},
		{Name: "revoke", Weight: 1, Do: do},
	}
	cfg := validConfig()
	cfg.Duration = 50 * time.Millisecond
	cfg.Workers = 2
	res, err := Run(context.Background(), ops, cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Workers != 2 {
		t.Errorf("Workers = %d, want 2", res.Workers)
	}
	if res.BucketWidth != 0 {
		t.Errorf("BucketWidth = %s, want 0 for a bounded run", res.BucketWidth)
	}
	if len(res.Buckets) != 10 {
		t.Fatalf("got %d buckets, want 10", len(res.Buckets))
	}
	sum := 0
	for i, b := range res.Buckets {
		if want := time.Duration(i) * 5 * time.Millisecond; b.Start != want {
			t.Errorf("bucket %d starts at %s, want %s", i, b.Start, want)
		}
		if b.Operations.Wall != 5*time.Millisecond {
			t.Errorf("bucket %d wall = %s, want the bucket width 5ms", i, b.Operations.Wall)
		}
		sum += b.Operations.Overall.Attempted
	}
	if total := res.Operations.Overall.Attempted; sum != total || total == 0 {
		t.Errorf("buckets hold %d operations, run total %d; want the same positive count", sum, total)
	}

	// The run started no earlier than start, and a worker notes an
	// operation's offset before it enters Do, so every operation's bucket is
	// at or before the bucket of its entry offset. Counted cumulatively, the
	// buckets 0..k hold at least as many operations as entered Do in them.
	// Filing an operation under the offset at which it finished, about 1ms
	// later, would move some operations past their entry bucket.
	mu.Lock()
	defer mu.Unlock()
	if len(entered) != res.Operations.Overall.Attempted {
		t.Fatalf("Do was entered %d times, run total %d", len(entered), res.Operations.Overall.Attempted)
	}
	width := cfg.Duration / bucketCount
	enteredBy := make([]int, bucketCount)
	for _, d := range entered {
		enteredBy[min(int(d/width), bucketCount-1)]++
	}
	filed, entries := 0, 0
	for k, b := range res.Buckets {
		filed += b.Operations.Overall.Attempted
		entries += enteredBy[k]
		if filed < entries {
			t.Errorf("buckets 0..%d hold %d operations, but %d entered Do by the end of bucket %d; an operation is filed after its start offset", k, filed, entries, k)
		}
	}
}

// TestBucketsPlaceSamplesByOffset confirms a sample lands in the bucket of its
// offset: a bounded run clamps an offset at or past its end into the last of
// its ten buckets, a sub-ten-nanosecond duration gets one-nanosecond buckets,
// and an unbounded run appends empty buckets up to an offset and up to the
// elapsed time of a snapshot. Every sample also lands in the run total.
func TestBucketsPlaceSamplesByOffset(t *testing.T) {
	t.Run("bounded", func(t *testing.T) {
		b := newBuckets(Config{Duration: 100 * time.Millisecond})
		offsets := []struct {
			offset time.Duration
			bucket int
		}{
			{0, 0},
			{10*time.Millisecond - 1, 0},
			{10 * time.Millisecond, 1},
			{55 * time.Millisecond, 5},
			{99 * time.Millisecond, 9},
			{100 * time.Millisecond, 9},
			{250 * time.Millisecond, 9},
		}
		want := make([]int, bucketCount)
		for _, o := range offsets {
			b.record(o.offset, metrics.Sample{Type: "validate", Duration: time.Millisecond, Success: true})
			want[o.bucket]++
		}
		res := b.snapshot(100 * time.Millisecond)
		if res.BucketWidth != 0 {
			t.Errorf("BucketWidth = %s, want 0", res.BucketWidth)
		}
		if len(res.Buckets) != bucketCount {
			t.Fatalf("got %d buckets, want %d", len(res.Buckets), bucketCount)
		}
		for i, bk := range res.Buckets {
			if bk.Start != time.Duration(i)*10*time.Millisecond {
				t.Errorf("bucket %d starts at %s, want %s", i, bk.Start, time.Duration(i)*10*time.Millisecond)
			}
			if got := bk.Operations.Overall.Attempted; got != want[i] {
				t.Errorf("bucket %d holds %d samples, want %d", i, got, want[i])
			}
		}
		if got := res.Operations.Overall.Attempted; got != len(offsets) {
			t.Errorf("run total holds %d samples, want %d", got, len(offsets))
		}
	})

	t.Run("tiny duration", func(t *testing.T) {
		b := newBuckets(Config{Duration: 5})
		b.record(3, metrics.Sample{Type: "validate", Success: true})
		b.record(time.Second, metrics.Sample{Type: "validate", Success: true})
		res := b.snapshot(time.Second)
		if len(res.Buckets) != bucketCount {
			t.Fatalf("got %d buckets, want %d", len(res.Buckets), bucketCount)
		}
		if res.Buckets[1].Start != 1 {
			t.Errorf("bucket 1 starts at %s, want 1ns", res.Buckets[1].Start)
		}
		if res.Buckets[3].Operations.Overall.Attempted != 1 || res.Buckets[9].Operations.Overall.Attempted != 1 {
			t.Errorf("buckets 3 and 9 = %+v and %+v, want one sample each", res.Buckets[3], res.Buckets[9])
		}
	})

	t.Run("unbounded", func(t *testing.T) {
		b := newBuckets(Config{Unbounded: true, BucketWidth: time.Minute})
		b.record(0, metrics.Sample{Type: "validate", Success: true})
		b.record(150*time.Second, metrics.Sample{Type: "revoke", Success: false, ErrKind: "timeout"})
		if n := len(b.slots); n != 3 {
			t.Errorf("after an offset of 2m30s there are %d buckets, want 3", n)
		}
		res := b.snapshot(4 * time.Minute)
		if res.BucketWidth != time.Minute {
			t.Errorf("BucketWidth = %s, want 1m0s", res.BucketWidth)
		}
		want := []int{1, 0, 1, 0, 0}
		if len(res.Buckets) != len(want) {
			t.Fatalf("got %d buckets, want %d", len(res.Buckets), len(want))
		}
		for i, bk := range res.Buckets {
			if bk.Start != time.Duration(i)*time.Minute {
				t.Errorf("bucket %d starts at %s, want %dm", i, bk.Start, i)
			}
			if bk.Operations.Wall != time.Minute {
				t.Errorf("bucket %d wall = %s, want 1m0s", i, bk.Operations.Wall)
			}
			if got := bk.Operations.Overall.Attempted; got != want[i] {
				t.Errorf("bucket %d holds %d samples, want %d", i, got, want[i])
			}
		}
		if got := res.Buckets[2].Operations.Errors; !reflect.DeepEqual(got, []metrics.ErrorCount{{Kind: "timeout", Count: 1}}) {
			t.Errorf("bucket 2 errors = %+v, want one timeout", got)
		}
		if got := res.Operations.Overall.Attempted; got != 2 {
			t.Errorf("run total holds %d samples, want 2", got)
		}
	})
}

// TestRunUnboundedSingleBucket confirms an unbounded run cancelled after a few
// operations, well within its first minute, reports its bucket width and
// exactly one bucket holding every operation.
func TestRunUnboundedSingleBucket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int64
	ops := []Operation{{Name: "validate", Weight: 1, Do: func(context.Context, *rand.Rand) error {
		if calls.Add(1) == 5 {
			cancel()
		}
		return nil
	}}}
	cfg := Config{Unbounded: true, BucketWidth: time.Minute, Workers: 2, Seed: 7, OpTimeout: time.Second}
	res, err := Run(ctx, ops, cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.BucketWidth != time.Minute {
		t.Errorf("BucketWidth = %s, want 1m0s", res.BucketWidth)
	}
	if len(res.Buckets) != 1 {
		t.Fatalf("got %d buckets, want 1", len(res.Buckets))
	}
	if res.Buckets[0].Start != 0 {
		t.Errorf("bucket 0 starts at %s, want 0s", res.Buckets[0].Start)
	}
	total := res.Operations.Overall.Attempted
	if got := res.Buckets[0].Operations.Overall.Attempted; got != total || int64(total) != calls.Load() || total < 5 {
		t.Errorf("bucket holds %d, run total %d, calls %d; want the same count of at least 5", got, total, calls.Load())
	}
}

// TestRunWaitsForBlockedOperation confirms an operation that started before
// a bounded run ended is not cut off by that end: it runs until its own
// OpTimeout, Run returns only after it returned, its sample is recorded
// with the error kind timeout, and the sample is filed under the bucket of
// its start offset, not the last bucket, where it finished.
func TestRunWaitsForBlockedOperation(t *testing.T) {
	var (
		returned atomic.Bool
		entered  atomic.Int64 // Do's entry offset from start, in nanoseconds
	)
	start := time.Now()
	ops := []Operation{{Name: "validate", Weight: 1, Do: func(ctx context.Context, _ *rand.Rand) error {
		entered.Store(int64(time.Since(start)))
		<-ctx.Done()
		returned.Store(true)
		return ctx.Err()
	}}}
	cfg := validConfig()
	cfg.Duration = 20 * time.Millisecond
	cfg.OpTimeout = 50 * time.Millisecond
	cfg.Workers = 1
	began := time.Now()
	res, err := Run(context.Background(), ops, cfg)
	elapsed := time.Since(began)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !returned.Load() {
		t.Error("Run returned before the blocked operation returned")
	}
	if elapsed < cfg.OpTimeout {
		t.Errorf("Run returned after %s, before the operation's %s timeout", elapsed, cfg.OpTimeout)
	}
	if got := res.Operations.Overall; got.Attempted != 1 || got.Failed != 1 {
		t.Errorf("run total = %+v, want one failed operation", got)
	}
	if got := res.Operations.Errors; !reflect.DeepEqual(got, []metrics.ErrorCount{{Kind: "timeout", Count: 1}}) {
		t.Errorf("run errors = %+v, want one timeout", got)
	}

	// The run started no earlier than start and the worker noted the
	// offset before it entered Do, so the sample's bucket is at or before
	// the bucket of the entry offset, almost always bucket 0. The operation
	// finished at least 50ms in, past the end, which is the last bucket.
	latest := min(int(time.Duration(entered.Load())/(cfg.Duration/bucketCount)), bucketCount-1)
	for i, b := range res.Buckets {
		if n := b.Operations.Overall.Attempted; n != 0 && i > latest {
			t.Errorf("bucket %d holds %d samples, want the sample at or before bucket %d, where the operation started", i, n, latest)
		}
	}
}

// TestRunCheckpoints confirms a run with a checkpoint callback hands it a
// snapshot every interval, and that the snapshots never lose operations.
func TestRunCheckpoints(t *testing.T) {
	var (
		mu        sync.Mutex
		attempted []int
	)
	ops := []Operation{{Name: "validate", Weight: 1, Do: sleepFor(time.Millisecond)}}
	cfg := validConfig()
	cfg.Duration = 200 * time.Millisecond
	cfg.Workers = 2
	cfg.CheckpointInterval = 10 * time.Millisecond
	cfg.OnCheckpoint = func(r *Result) {
		mu.Lock()
		defer mu.Unlock()
		if r.Workers != 2 || len(r.Buckets) != bucketCount {
			t.Errorf("checkpoint snapshot has %d workers and %d buckets, want 2 and %d", r.Workers, len(r.Buckets), bucketCount)
		}
		attempted = append(attempted, r.Operations.Overall.Attempted)
	}
	res, err := Run(context.Background(), ops, cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempted) < 3 {
		t.Fatalf("checkpoint called %d times, want at least 3", len(attempted))
	}
	for i := 1; i < len(attempted); i++ {
		if attempted[i] < attempted[i-1] {
			t.Errorf("checkpoint %d attempted %d after %d, want a non-decreasing count", i, attempted[i], attempted[i-1])
		}
	}
	if last := attempted[len(attempted)-1]; last > res.Operations.Overall.Attempted {
		t.Errorf("last checkpoint attempted %d, more than the final %d", last, res.Operations.Overall.Attempted)
	}
}

// TestRunWaitsForCheckpoint confirms Run returns only after the checkpoint
// goroutine has returned: a checkpoint call still running when the workers
// stop has finished by the time Run returns, and no call starts after the run
// ended. The first call ends the run by cancelling its context and then stays
// busy for 100ms, standing in for a slow checkpoint write; the workers' 1ms
// operations stop long before that.
func TestRunWaitsForCheckpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		calls    atomic.Int64
		finished atomic.Bool
	)
	ops := []Operation{{Name: "validate", Weight: 1, Do: sleepFor(time.Millisecond)}}
	cfg := validConfig()
	cfg.Duration = time.Hour
	cfg.Workers = 2
	cfg.CheckpointInterval = 5 * time.Millisecond
	cfg.OnCheckpoint = func(*Result) {
		if calls.Add(1) > 1 {
			return
		}
		cancel()
		<-time.After(100 * time.Millisecond)
		finished.Store(true)
	}
	if _, err := Run(ctx, ops, cfg); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !finished.Load() {
		t.Error("Run returned while a checkpoint call was still running")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("checkpoint called %d times, want 1: none after the run ended", n)
	}
}

// TestRunCustomClassify confirms Config.Classify labels the failed
// operations in the run's error breakdown.
func TestRunCustomClassify(t *testing.T) {
	boom := errors.New("boom")
	ops := []Operation{{Name: "validate", Weight: 1, Do: func(context.Context, *rand.Rand) error { return boom }}}
	cfg := validConfig()
	cfg.Duration = 10 * time.Millisecond
	cfg.Workers = 1
	cfg.Classify = func(err error) string {
		if errors.Is(err, boom) {
			return "custom-kind"
		}
		return "unexpected"
	}
	res, err := Run(context.Background(), ops, cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	failed := res.Operations.Overall.Failed
	if failed == 0 {
		t.Fatal("the run recorded no failed operation")
	}
	if got := res.Operations.Errors; !reflect.DeepEqual(got, []metrics.ErrorCount{{Kind: "custom-kind", Count: failed}}) {
		t.Errorf("run errors = %+v, want %d custom-kind", got, failed)
	}
}

// TestDefaultClassify confirms the labels used when Config.Classify is nil.
func TestDefaultClassify(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{fmt.Errorf("call: %w", context.Canceled), "canceled"},
		{fmt.Errorf("call: %w", context.DeadlineExceeded), "timeout"},
		{errors.New("boom"), "other"},
	}
	for _, tc := range tests {
		if got := defaultClassify(tc.err); got != tc.want {
			t.Errorf("defaultClassify(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
