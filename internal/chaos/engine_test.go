package chaos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/B42Labs/openstack-tester/internal/resource"
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
