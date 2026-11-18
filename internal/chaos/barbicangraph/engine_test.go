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
	mismatch bool            // every read-back returns a wrapped mismatch of its kind

	liveSecret    map[string]bool     // secret id -> live
	liveContainer map[string][]string // container id -> the secret ids it references
	liveACL       map[string]bool     // secret id -> an ACL is set on it
	liveConsumer  map[string]string   // consumer id -> the container id it is registered on
	readBacks     map[string]int      // secret id -> read-backs of that instance
	retrieveCalls int                 // total RetrievePayload calls

	// calls per ACL and consumer operation
	aclSets, aclVerifies, aclDeletes                int
	consumerRegs, consumerVerifies, consumerDeletes int

	// live counts and high-water marks
	curS, maxS int
	curC, maxC int

	// invariant violations the graph must never cause
	stampAbsent              bool // a stamp referenced a secret that was not live
	containerOnAbsent        bool // a container create referenced a secret id that was not live
	secretDeleteReferenced   bool // a secret was deleted while a live container referenced it
	readBackAbsent           bool // a read-back referenced a secret that was not live
	readBackUnmarked         bool // a read-back referenced a secret the plan does not mark Retrieve
	aclOnAbsent              bool // an ACL was set on a secret that was not live
	consumerOnAbsent         bool // a consumer was registered on a container that was not live
	secretDeleteWithACL      bool // a secret was deleted while an ACL on it was live
	containerDeleteConsumed  bool // a container was deleted while a consumer on it was live
	aclReadBackAbsent        bool // an ACL read-back targeted an ACL that was not live
	consumerReadBackAbsent   bool // a consumer read-back targeted a consumer that was not live on that container
	consumerReadBackNotAlone bool // a consumer read-back was not for exactly itself, without exact
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
		liveACL:       make(map[string]bool),
		liveConsumer:  make(map[string]string),
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

func (f *fakeBarbican) SetACL(_ context.Context, a barbicanplan.ACL, secret resource.Resource) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aclSets++
	if !f.liveSecret[secret.ID] {
		f.aclOnAbsent = true
	}
	f.liveACL[secret.ID] = true
	return resource.Resource{Kind: barbican.KindACL, Logical: a.Name, ID: secret.ID}, nil
}

func (f *fakeBarbican) RegisterConsumer(_ context.Context, con barbicanplan.Consumer, container resource.Resource) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consumerRegs++
	if _, ok := f.liveContainer[container.ID]; !ok {
		f.consumerOnAbsent = true
	}
	name := "dizzy-run0-" + con.Name
	id := container.ID + ":" + name
	f.liveConsumer[id] = container.ID
	return resource.Resource{Kind: barbican.KindConsumer, Logical: con.Name, Name: name, ID: id}, nil
}

func (f *fakeBarbican) VerifyACL(_ context.Context, a barbicanplan.ACL, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aclVerifies++
	if !f.liveACL[r.ID] {
		f.aclReadBackAbsent = true
	}
	if f.mismatch {
		return fmt.Errorf("%w: acl %q reads back users [], want %v", barbican.ErrACLMismatch, a.Name, a.Users)
	}
	return nil
}

func (f *fakeBarbican) VerifyConsumers(_ context.Context, container resource.Resource, want []resource.Resource, exact bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consumerVerifies++
	if len(want) != 1 || exact {
		f.consumerReadBackNotAlone = true
	}
	for _, w := range want {
		if ctr, ok := f.liveConsumer[w.ID]; !ok || ctr != container.ID {
			f.consumerReadBackAbsent = true
		}
	}
	if f.mismatch {
		return fmt.Errorf("%w: container %s lacks consumer %q", barbican.ErrConsumerMismatch, container.ID, want[0].Name)
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
		if f.liveACL[r.ID] {
			f.secretDeleteWithACL = true
		}
		if f.liveSecret[r.ID] {
			delete(f.liveSecret, r.ID)
			f.curS--
		}
	case barbican.KindContainer:
		for _, ctr := range f.liveConsumer {
			if ctr == r.ID {
				f.containerDeleteConsumed = true
			}
		}
		if _, ok := f.liveContainer[r.ID]; ok {
			delete(f.liveContainer, r.ID)
			f.curC--
		}
	case barbican.KindACL:
		f.aclDeletes++
		delete(f.liveACL, r.ID)
	case barbican.KindConsumer:
		f.consumerDeletes++
		delete(f.liveConsumer, r.ID)
	}
	return nil
}

// churnPlan is a plan with three stored secrets (two marked for read-back), one
// key order the graph must ignore, two containers that share a secret, so a
// shared parent is held by more than one live container, an ACL on each of two
// secrets, and three consumers, two of them on the same container.
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
		ACLs: []barbicanplan.ACL{
			{Name: "acl-0001", Secret: "sec-0002", Users: []string{"u1"}},
			{Name: "acl-0002", Secret: "sec-0003", Users: []string{"u2", "u3"}},
		},
		Consumers: []barbicanplan.Consumer{
			{Name: "con-0001", Container: "ctr-0001"},
			{Name: "con-0002", Container: "ctr-0002"},
			{Name: "con-0003", Container: "ctr-0001"},
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
// stayed within the plan envelope. For the ACLs and consumers it checks that no
// ACL was set on an absent secret, no consumer registered on an absent
// container, no secret deleted while its ACL was live, no container deleted
// while a consumer of it was live, and that every ACL and consumer read-back
// targeted a live one, a consumer's for itself alone and not exact; and that
// both kinds were created, read back, and deleted.
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

	for _, v := range []struct {
		violated bool
		what     string
	}{
		{f.aclOnAbsent, "an ACL was set on a secret that was not live"},
		{f.consumerOnAbsent, "a consumer was registered on a container that was not live"},
		{f.secretDeleteWithACL, "a secret was deleted while its ACL was live"},
		{f.containerDeleteConsumed, "a container was deleted while a consumer of it was live"},
		{f.aclReadBackAbsent, "an ACL read-back targeted an ACL that was not live"},
		{f.consumerReadBackAbsent, "a consumer read-back targeted a consumer that was not live on that container"},
		{f.consumerReadBackNotAlone, "a consumer read-back was not for itself alone without exact"},
	} {
		if v.violated {
			t.Error(v.what)
		}
	}
	for _, c := range []struct {
		n    int
		what string
	}{
		{f.aclSets, "ACL sets"}, {f.aclVerifies, "ACL read-backs"}, {f.aclDeletes, "ACL deletes"},
		{f.consumerRegs, "consumer registrations"}, {f.consumerVerifies, "consumer read-backs"}, {f.consumerDeletes, "consumer deletes"},
	} {
		if c.n == 0 {
			t.Errorf("no %s happened; the run does not exercise them", c.what)
		}
	}
}

// TestRunRetrieveRatioZeroNeverReadsBack confirms retrieve_ratio 0 reads
// nothing back: no payload, although the plan marks secrets for read-back, and
// no ACL or consumer.
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
	if f.aclVerifies != 0 || f.consumerVerifies != 0 {
		t.Errorf("VerifyACL calls = %d, VerifyConsumers calls = %d, want 0 at retrieve_ratio 0", f.aclVerifies, f.consumerVerifies)
	}
	if f.aclSets == 0 || f.consumerRegs == 0 {
		t.Errorf("ACL sets = %d, consumer registrations = %d; the run must still churn both kinds", f.aclSets, f.consumerRegs)
	}
}

// TestRunMismatchIsAFailedOutcome confirms a read-back that returns different
// data (here every payload, ACL, and consumer read-back) is a failed operation
// the engine tolerates, not a stop: the run keeps creating and deleting after
// the first read-back, returns no error, and the per-bucket error breakdown
// labels the failures mismatch, the value the chaos command counts to exit
// non-zero.
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
	mismatches := 0
	for _, b := range r.Buckets {
		for _, e := range b.Errors {
			if e.Kind == barbican.ErrKindMismatch {
				sawMismatch = true
				mismatches += e.Count
			}
		}
	}
	if !sawMismatch {
		t.Errorf("no bucket recorded a %q error; buckets = %+v", barbican.ErrKindMismatch, r.Buckets)
	}
	// Every mutation is a read-back, and every read-back mismatches, so each
	// one, whatever its kind, is labeled mismatch.
	if mismatches != r.Mutates {
		t.Errorf("buckets label %d errors %q, want one per mutation (%d)", mismatches, barbican.ErrKindMismatch, r.Mutates)
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
