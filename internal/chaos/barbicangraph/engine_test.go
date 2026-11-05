package barbicangraph

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/barbican"
	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeBarbican is an in-process Barbican that tracks the live population and
// the lifecycle invariants the churn graph must never violate, so the barbican
// chaos graph can be checked without a cloud. It is safe for concurrent use by
// the engine's operation tasks.
type fakeBarbican struct {
	mu     sync.Mutex
	nextID int

	retrieve map[string]bool // logical -> the plan marks the secret for read-back
	mismatch bool            // every read-back returns a wrapped ErrPayloadMismatch

	liveSecret    map[string]bool     // secret id -> live
	liveContainer map[string][]string // container id -> the secret ids it references
	readBacks     map[string]int      // secret id -> read-backs of that instance
	retrieveCalls int                 // total RetrievePayload calls

	// live counts and high-water marks
	curS, maxS int
	curC, maxC int

	// invariant violations the graph must never cause
	stampAbsent            bool // a stamp referenced a secret that was not live
	containerOnAbsent      bool // a container create referenced a secret id that was not live
	secretDeleteReferenced bool // a secret was deleted while a live container referenced it
	readBackAbsent         bool // a read-back referenced a secret that was not live
	readBackUnmarked       bool // a read-back referenced a secret the plan does not mark Retrieve
}

func newFakeBarbican(p *barbicanplan.Plan) *fakeBarbican {
	retrieve := make(map[string]bool, len(p.Secrets))
	for _, s := range p.Secrets {
		retrieve[s.Name] = s.Retrieve
	}
	return &fakeBarbican{
		retrieve:      retrieve,
		liveSecret:    make(map[string]bool),
		liveContainer: make(map[string][]string),
		readBacks:     make(map[string]int),
	}
}

func (f *fakeBarbican) StoreSecret(_ context.Context, s barbicanplan.Secret, _ int64) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("s-%d", f.nextID)
	f.liveSecret[id] = true
	f.curS++
	if f.curS > f.maxS {
		f.maxS = f.curS
	}
	return resource.Resource{Kind: barbican.KindSecret, Logical: s.Name, Name: "dizzy-run0-" + s.Name, ID: id}, nil
}

func (f *fakeBarbican) StampSecret(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.liveSecret[r.ID] {
		f.stampAbsent = true
	}
	return nil
}

func (f *fakeBarbican) CreateContainer(_ context.Context, ct barbicanplan.Container, stored map[string]resource.Resource) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	refs := make([]string, 0, len(ct.Secrets))
	for _, logical := range ct.Secrets {
		id := stored[logical].ID
		if !f.liveSecret[id] {
			f.containerOnAbsent = true
		}
		refs = append(refs, id)
	}
	f.nextID++
	id := fmt.Sprintf("c-%d", f.nextID)
	f.liveContainer[id] = refs
	f.curC++
	if f.curC > f.maxC {
		f.maxC = f.curC
	}
	return resource.Resource{Kind: barbican.KindContainer, Logical: ct.Name, Name: "dizzy-run0-" + ct.Name, ID: id}, nil
}

func (f *fakeBarbican) RetrievePayload(_ context.Context, r resource.Resource, _ int64, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retrieveCalls++
	f.readBacks[r.ID]++
	if !f.liveSecret[r.ID] {
		f.readBackAbsent = true
	}
	if !f.retrieve[r.Logical] {
		f.readBackUnmarked = true
	}
	if f.mismatch {
		return fmt.Errorf("%w: secret %q returned 1 bytes, want 2, first difference at byte 0", barbican.ErrPayloadMismatch, r.Logical)
	}
	return nil
}

func (f *fakeBarbican) Delete(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Kind {
	case barbican.KindSecret:
		for _, refs := range f.liveContainer {
			for _, id := range refs {
				if id == r.ID {
					f.secretDeleteReferenced = true
				}
			}
		}
		if f.liveSecret[r.ID] {
			delete(f.liveSecret, r.ID)
			f.curS--
		}
	case barbican.KindContainer:
		if _, ok := f.liveContainer[r.ID]; ok {
			delete(f.liveContainer, r.ID)
			f.curC--
		}
	}
	return nil
}

// churnPlan is a plan with three stored secrets (two marked for read-back), one
// key order the graph must ignore, and two containers that share a secret, so a
// shared parent is held by more than one live container.
func churnPlan() *barbicanplan.Plan {
	return &barbicanplan.Plan{
		Scenario: "churn", Seed: 7,
		Secrets: []barbicanplan.Secret{
			{Name: "sec-0001", PayloadBytes: 32, Retrieve: true},
			{Name: "sec-0002", PayloadBytes: 64},
			{Name: "sec-0003", PayloadBytes: 16, Retrieve: true},
		},
		Orders: []barbicanplan.Order{{Name: "ord-0001"}},
		Containers: []barbicanplan.Container{
			{Name: "ctr-0001", Secrets: []string{"sec-0001", "sec-0002"}},
			{Name: "ctr-0002", Secrets: []string{"sec-0003", "sec-0001"}},
		},
	}
}

// fakeClock is a virtual clock: Sleep advances time instantly, so the scheduler
// emits a deterministic number of ticks while the operation tasks still run
// concurrently on real goroutines.
type fakeClock struct{ cur time.Time }

func newFakeClock() *fakeClock      { return &fakeClock{cur: time.Unix(0, 0)} }
func (c *fakeClock) Now() time.Time { return c.cur }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.cur = c.cur.Add(d)
	return nil
}

func barbicanCfg(retrieveRatio float64) chaos.Config {
	return chaos.Config{
		Duration:    2 * time.Second,
		MinInterval: 10 * time.Millisecond,
		MaxInterval: 40 * time.Millisecond,
		MaxParallel: 4,
		ChurnRatio:  0.5,
		TargetFill:  0.7,
		ResizeRatio: retrieveRatio, // the engine's mutate-draw probability, fed from retrieve_ratio
		Concurrency: 8,
		Classify:    Classify,
	}
}

func mustBuild(t *testing.T, p *barbicanplan.Plan, c Barbican) []chaos.Node {
	t.Helper()
	nodes, err := Build(p, c, p.Seed, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return nodes
}

// TestRunLifecycleInvariants drives the full churn graph and checks the
// key-manager lifecycle invariants: no container grouped a secret that was not
// live, no secret was deleted while a live container referenced it, every
// read-back targeted a live secret the plan marks for read-back, no secret
// instance was read back more than once, and the live secrets and containers
// stayed within the plan envelope.
func TestRunLifecycleInvariants(t *testing.T) {
	p := churnPlan()
	f := newFakeBarbican(p)

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, barbicanCfg(0.5), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Creates == 0 {
		t.Fatal("no creates were scheduled; the test exercises nothing")
	}
	if r.Mutates == 0 {
		t.Fatal("no read-backs were scheduled; the test exercises nothing")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stampAbsent {
		t.Error("a stamp referenced a secret that was not live")
	}
	if f.containerOnAbsent {
		t.Error("a container create referenced a secret id that was not live")
	}
	if f.secretDeleteReferenced {
		t.Error("a secret was deleted while a live container referenced it")
	}
	if f.readBackAbsent {
		t.Error("a read-back referenced a secret that was not live")
	}
	if f.readBackUnmarked {
		t.Error("a read-back referenced a secret the plan does not mark Retrieve")
	}
	for id, n := range f.readBacks {
		if n > 1 {
			t.Errorf("secret instance %s read back %d times, want at most once per lifetime", id, n)
		}
	}
	if f.maxS > len(p.Secrets) {
		t.Errorf("live secrets peaked at %d, exceeding the envelope %d", f.maxS, len(p.Secrets))
	}
	if f.maxC > len(p.Containers) {
		t.Errorf("live containers peaked at %d, exceeding the envelope %d", f.maxC, len(p.Containers))
	}
}

// TestRunRetrieveRatioZeroNeverReadsBack confirms retrieve_ratio 0 reads no
// payload back, even though the plan marks secrets for read-back.
func TestRunRetrieveRatioZeroNeverReadsBack(t *testing.T) {
	p := churnPlan()
	f := newFakeBarbican(p)

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, barbicanCfg(0), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Mutates != 0 {
		t.Errorf("Result.Mutates = %d, want 0 at retrieve_ratio 0", r.Mutates)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retrieveCalls != 0 {
		t.Errorf("RetrievePayload calls = %d, want 0 at retrieve_ratio 0", f.retrieveCalls)
	}
}

// TestRunMismatchIsAFailedOutcome confirms a read-back that returns different
// bytes is a failed operation the engine tolerates, not a stop: the run keeps
// creating and deleting after the first read-back, returns no error, and the
// per-bucket error breakdown labels the failure mismatch, the value the chaos
// command counts to exit non-zero.
func TestRunMismatchIsAFailedOutcome(t *testing.T) {
	p := churnPlan()
	f := newFakeBarbican(p)
	f.mismatch = true

	r, err := chaos.Run(context.Background(), mustBuild(t, p, f), p.Seed, barbicanCfg(0.5), newFakeClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	firstMutate := -1
	for i, d := range r.Decisions {
		if d.Action == "mutate" {
			firstMutate = i
			break
		}
	}
	if firstMutate < 0 {
		t.Fatal("no read-back was scheduled; the test exercises nothing")
	}
	churnedAfter := false
	for _, d := range r.Decisions[firstMutate+1:] {
		if d.Action == "create" || d.Action == "delete" {
			churnedAfter = true
			break
		}
	}
	if !churnedAfter {
		t.Error("no create or delete was scheduled after the first read-back; a mismatch must not stop the churn")
	}

	sawMismatch := false
	for _, b := range r.Buckets {
		for _, e := range b.Errors {
			if e.Kind == barbican.ErrKindMismatch {
				sawMismatch = true
			}
		}
	}
	if !sawMismatch {
		t.Errorf("no bucket recorded a %q error; buckets = %+v", barbican.ErrKindMismatch, r.Buckets)
	}
}

// TestRunDeterministicSchedule confirms the decision schedule, read-backs
// included, is reproducible for a given seed and config, independent of the
// concurrent cloud completions.
func TestRunDeterministicSchedule(t *testing.T) {
	p := churnPlan()
	cfg := barbicanCfg(0.5)

	r1, err := chaos.Run(context.Background(), mustBuild(t, p, newFakeBarbican(p)), p.Seed, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	r2, err := chaos.Run(context.Background(), mustBuild(t, p, newFakeBarbican(p)), p.Seed, cfg, newFakeClock())
	if err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if len(r1.Decisions) == 0 {
		t.Fatal("no decisions were scheduled")
	}
	mutates := 0
	for _, d := range r1.Decisions {
		if d.Action == "mutate" {
			mutates++
		}
	}
	if mutates == 0 {
		t.Error("the schedule holds no mutate decision; read-backs are not covered")
	}
	if !reflect.DeepEqual(r1.Decisions, r2.Decisions) {
		t.Error("decision schedules differ for the same seed/config")
	}
}
