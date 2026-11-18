package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/barbican"
	"github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/payload"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeBarbican records every operation the executor drives, so tests can assert
// on call order and coverage. It is the in-test implementation of the
// executor's Barbican seam. Each call is recorded as one event, verb:<logical>
// for a create and verb:<id> for a call on an existing resource, and answers
// with the next error queued for that event in fail, succeeding once the queue
// is empty. blockWait names an order id whose wait blocks until its context
// ends, standing in for an order that never turns ACTIVE. onSetACL, when set,
// runs inside every SetACL call, standing in for an interrupt mid-stage.
type fakeBarbican struct {
	mu     sync.Mutex
	events []string
	fail   map[string][]error

	blockWait string
	onSetACL  func()

	storeSeeds    map[string]int64             // secret logical name -> the seed the store was handed
	reads         map[string]readCall          // secret logical name -> the read-back's arguments
	containerRefs map[string][]string          // container logical name -> the stored secret ids it references
	aclTargets    map[string]string            // acl logical name -> the secret id SetACL was handed
	consumerCtrs  map[string]string            // consumer logical name -> the container id RegisterConsumer was handed
	aclChecks     map[string]resource.Resource // acl logical name -> the resource VerifyACL was handed
	consumerCalls []consumersCall              // every VerifyConsumers call, in call order
}

// consumersCall is the arguments one VerifyConsumers call was handed: the
// container's id and the logical names of the wanted consumers, in order.
type consumersCall struct {
	container string
	want      []string
	exact     bool
}

// readCall is the arguments one RetrievePayload call was handed.
type readCall struct {
	id   string
	seed int64
	n    int
}

func newFakeBarbican() *fakeBarbican {
	return &fakeBarbican{
		fail:          map[string][]error{},
		storeSeeds:    map[string]int64{},
		reads:         map[string]readCall{},
		containerRefs: map[string][]string{},
		aclTargets:    map[string]string{},
		consumerCtrs:  map[string]string{},
		aclChecks:     map[string]resource.Resource{},
	}
}

// failWith queues errs as the answers of the next calls recorded as ev.
func (f *fakeBarbican) failWith(ev string, errs ...error) {
	f.fail[ev] = append(f.fail[ev], errs...)
}

// call records ev and returns the next error queued for it, or nil.
func (f *fakeBarbican) call(ev string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	q := f.fail[ev]
	if len(q) == 0 {
		return nil
	}
	f.fail[ev] = q[1:]
	return q[0]
}

func (f *fakeBarbican) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func storedRes(logical string) resource.Resource {
	return resource.Resource{Kind: barbican.KindSecret, Logical: logical, Name: "dizzy-run0-" + logical, ID: "secret-" + logical}
}

func orderRes(logical string) resource.Resource {
	return resource.Resource{Kind: barbican.KindOrder, Logical: logical, Name: "dizzy-run0-" + logical, ID: "order-" + logical}
}

// generatedRes is the secret Barbican generates for an order: it shares the
// order's logical and cloud name and differs in kind and id.
func generatedRes(logical string) resource.Resource {
	return resource.Resource{Kind: barbican.KindSecret, Logical: logical, Name: "dizzy-run0-" + logical, ID: "gen-" + logical}
}

func containerRes(logical string) resource.Resource {
	return resource.Resource{Kind: barbican.KindContainer, Logical: logical, Name: "dizzy-run0-" + logical, ID: "container-" + logical}
}

// aclRes is the resource SetACL returns for an ACL on the stored secret with
// logical name secret: it carries the secret's id and no name.
func aclRes(logical, secret string) resource.Resource {
	return resource.Resource{Kind: barbican.KindACL, Logical: logical, ID: storedRes(secret).ID}
}

// consumerRes is the resource RegisterConsumer returns for a consumer on the
// container with logical name container.
func consumerRes(logical, container string) resource.Resource {
	name := "dizzy-run0-" + logical
	return resource.Resource{Kind: barbican.KindConsumer, Logical: logical, Name: name, ID: containerRes(container).ID + ":" + name}
}

func (f *fakeBarbican) StoreSecret(_ context.Context, s plan.Secret, seed int64) (resource.Resource, error) {
	f.mu.Lock()
	f.storeSeeds[s.Name] = seed
	f.mu.Unlock()
	if err := f.call("store:" + s.Name); err != nil {
		return resource.Resource{}, err
	}
	return storedRes(s.Name), nil
}

func (f *fakeBarbican) StampSecret(_ context.Context, r resource.Resource) error {
	return f.call("stamp:" + r.ID)
}

func (f *fakeBarbican) CreateOrder(_ context.Context, o plan.Order) (resource.Resource, error) {
	if err := f.call("order:" + o.Name); err != nil {
		return resource.Resource{}, err
	}
	return orderRes(o.Name), nil
}

func (f *fakeBarbican) WaitForOrder(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	if err := f.call("wait:" + r.ID); err != nil {
		return resource.Resource{}, err
	}
	if r.ID == f.blockWait {
		<-ctx.Done()
		return resource.Resource{}, ctx.Err()
	}
	return generatedRes(r.Logical), nil
}

func (f *fakeBarbican) CreateContainer(_ context.Context, ct plan.Container, stored map[string]resource.Resource) (resource.Resource, error) {
	ids := make([]string, 0, len(ct.Secrets))
	for _, logical := range ct.Secrets {
		s, ok := stored[logical]
		if !ok {
			return resource.Resource{}, fmt.Errorf("container %q references secret %q that was not stored", ct.Name, logical)
		}
		ids = append(ids, s.ID)
	}
	f.mu.Lock()
	f.containerRefs[ct.Name] = ids
	f.mu.Unlock()
	if err := f.call("container:" + ct.Name); err != nil {
		return resource.Resource{}, err
	}
	return containerRes(ct.Name), nil
}

func (f *fakeBarbican) RetrievePayload(_ context.Context, r resource.Resource, seed int64, n int) error {
	f.mu.Lock()
	f.reads[r.Logical] = readCall{id: r.ID, seed: seed, n: n}
	f.mu.Unlock()
	return f.call("retrieve:" + r.ID)
}

func (f *fakeBarbican) SetACL(_ context.Context, a plan.ACL, secret resource.Resource) (resource.Resource, error) {
	if secret.ID == "" {
		return resource.Resource{}, fmt.Errorf("acl %q targets secret %q that was not stored", a.Name, a.Secret)
	}
	f.mu.Lock()
	f.aclTargets[a.Name] = secret.ID
	hook := f.onSetACL
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err := f.call("acl:" + a.Name); err != nil {
		return resource.Resource{}, err
	}
	return resource.Resource{Kind: barbican.KindACL, Logical: a.Name, ID: secret.ID}, nil
}

func (f *fakeBarbican) RegisterConsumer(_ context.Context, con plan.Consumer, container resource.Resource) (resource.Resource, error) {
	if container.ID == "" {
		return resource.Resource{}, fmt.Errorf("consumer %q targets container %q that was not created", con.Name, con.Container)
	}
	f.mu.Lock()
	f.consumerCtrs[con.Name] = container.ID
	f.mu.Unlock()
	if err := f.call("consumer:" + con.Name); err != nil {
		return resource.Resource{}, err
	}
	name := "dizzy-run0-" + con.Name
	return resource.Resource{Kind: barbican.KindConsumer, Logical: con.Name, Name: name, ID: container.ID + ":" + name}, nil
}

func (f *fakeBarbican) VerifyACL(_ context.Context, a plan.ACL, r resource.Resource) error {
	f.mu.Lock()
	f.aclChecks[a.Name] = r
	f.mu.Unlock()
	return f.call("verifyacl:" + r.ID)
}

func (f *fakeBarbican) VerifyConsumers(_ context.Context, container resource.Resource, want []resource.Resource, exact bool) error {
	logicals := make([]string, 0, len(want))
	for _, w := range want {
		logicals = append(logicals, w.Logical)
	}
	f.mu.Lock()
	f.consumerCalls = append(f.consumerCalls, consumersCall{container: container.ID, want: logicals, exact: exact})
	f.mu.Unlock()
	return f.call("verifyconsumers:" + container.ID)
}

// indexOf returns the position of the first event equal to want, or -1.
func indexOf(events []string, want string) int {
	return slices.Index(events, want)
}

// indexOfPrefix returns the position of the first event with prefix, or -1.
func indexOfPrefix(events []string, prefix string) int {
	return slices.IndexFunc(events, func(e string) bool { return strings.HasPrefix(e, prefix) })
}

// count returns how many events equal want.
func count(events []string, want string) int {
	var n int
	for _, e := range events {
		if e == want {
			n++
		}
	}
	return n
}

// fullPlan builds a plan that exercises every stage: three stored secrets, two
// of them read back, two key orders, two containers, two ACLs, and three
// consumers spread over both containers.
func fullPlan() *plan.Plan {
	return &plan.Plan{
		Scenario: "t",
		Seed:     5,
		Secrets: []plan.Secret{
			{Name: "sec-0001", PayloadBytes: 64, Retrieve: true},
			{Name: "sec-0002", PayloadBytes: 128},
			{Name: "sec-0003", PayloadBytes: 8192, Retrieve: true},
		},
		Orders: []plan.Order{{Name: "ord-0001"}, {Name: "ord-0002"}},
		Containers: []plan.Container{
			{Name: "ctr-0001", Secrets: []string{"sec-0001", "sec-0003"}},
			{Name: "ctr-0002", Secrets: []string{"sec-0002"}},
		},
		ACLs: []plan.ACL{
			{Name: "acl-0001", Secret: "sec-0001", Users: []string{"u1", "u2"}},
			{Name: "acl-0002", Secret: "sec-0003", Users: []string{"u3"}},
		},
		Consumers: []plan.Consumer{
			{Name: "con-0001", Container: "ctr-0002"},
			{Name: "con-0002", Container: "ctr-0001"},
			{Name: "con-0003", Container: "ctr-0002"},
		},
	}
}

// basePlan is fullPlan without ACLs or consumers: the plan every stage of
// today's apply runs on.
func basePlan() *plan.Plan {
	p := fullPlan()
	p.ACLs, p.Consumers = nil, nil
	return p
}

// newStagePrefixes are the event prefixes of the calls the ACL and consumer
// stages make.
var newStagePrefixes = []string{"acl:", "consumer:", "verifyacl:", "verifyconsumers:"}

// policyDenied is a 403 without a quota in its body: a policy denial, which is
// neither a quota rejection nor retryable.
var policyDenied = gophercloud.ErrUnexpectedResponseCode{Actual: 403, Body: []byte(`{"title": "Forbidden"}`)}

func TestApplyStageOrdering(t *testing.T) {
	f := newFakeBarbican()
	p := fullPlan()
	if _, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	e := f.snapshot()

	// Every event of a stage must come after every event of the stage before it.
	stages := [][]string{
		{
			"store:sec-0001", "stamp:secret-sec-0001",
			"store:sec-0002", "stamp:secret-sec-0002",
			"store:sec-0003", "stamp:secret-sec-0003",
		},
		{
			"order:ord-0001", "wait:order-ord-0001", "stamp:gen-ord-0001",
			"order:ord-0002", "wait:order-ord-0002", "stamp:gen-ord-0002",
		},
		{"container:ctr-0001", "container:ctr-0002"},
		{"acl:acl-0001", "acl:acl-0002"},
		{"consumer:con-0001", "consumer:con-0002", "consumer:con-0003"},
		{"retrieve:secret-sec-0001", "retrieve:secret-sec-0003"},
		{"verifyacl:secret-sec-0001", "verifyacl:secret-sec-0003"},
		{"verifyconsumers:container-ctr-0001", "verifyconsumers:container-ctr-0002"},
	}
	var total int
	prevLast := -1
	for n, stage := range stages {
		first, last := len(e), -1
		for _, ev := range stage {
			i := indexOf(e, ev)
			if i < 0 {
				t.Fatalf("event %q not found in %v", ev, e)
			}
			first, last = min(first, i), max(last, i)
		}
		if first <= prevLast {
			t.Errorf("stage %d starts at event %d, before the previous stage ended at %d: %v", n+1, first, prevLast, e)
		}
		prevLast = last
		total += len(stage)
	}
	if len(e) != total {
		t.Errorf("got %d events, want exactly the %d staged ones: %v", len(e), total, e)
	}
}

func TestApplyStampsFollowTheirCreates(t *testing.T) {
	f := newFakeBarbican()
	p := fullPlan()
	if _, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	e := f.snapshot()

	// Each stamp is handed the secret its own store returned, after that store.
	for _, s := range p.Secrets {
		store, stamp := indexOf(e, "store:"+s.Name), indexOf(e, "stamp:secret-"+s.Name)
		if store < 0 || stamp < 0 {
			t.Fatalf("store or stamp of %s missing: %v", s.Name, e)
		}
		if stamp < store {
			t.Errorf("stamp of %s at %d precedes its store at %d: %v", s.Name, stamp, store, e)
		}
		if n := count(e, "stamp:secret-"+s.Name); n != 1 {
			t.Errorf("%s stamped %d times, want 1", s.Name, n)
		}
	}

	// Each generated secret is stamped once its order's wait has named it.
	for _, o := range p.Orders {
		create, wait, stamp := indexOf(e, "order:"+o.Name), indexOf(e, "wait:order-"+o.Name), indexOf(e, "stamp:gen-"+o.Name)
		if create < 0 || wait < 0 || stamp < 0 {
			t.Fatalf("create, wait, or generated-secret stamp of %s missing: %v", o.Name, e)
		}
		if create >= wait || wait >= stamp {
			t.Errorf("%s: create %d, wait %d, stamp %d are out of order: %v", o.Name, create, wait, stamp, e)
		}
	}
}

func TestApplyReadsBackOnlyRetrieveSecrets(t *testing.T) {
	f := newFakeBarbican()
	p := fullPlan()
	if _, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}

	want := map[string]readCall{}
	for _, s := range p.Secrets {
		if s.Retrieve {
			want[s.Name] = readCall{id: "secret-" + s.Name, seed: payload.Seed(p.Seed, s.Name), n: s.PayloadBytes}
		}
	}
	if len(want) == 0 || len(want) == len(p.Secrets) {
		t.Fatalf("fixture must mark some but not all secrets for read-back, got %d of %d", len(want), len(p.Secrets))
	}
	if !maps.Equal(f.reads, want) {
		t.Errorf("read-backs = %+v, want %+v", f.reads, want)
	}
	if n := count(f.snapshot(), "retrieve:secret-sec-0002"); n != 0 {
		t.Errorf("sec-0002 is not marked for read-back but was read %d times", n)
	}
}

// TestApplyStoreUsesDeterministicSeed confirms the payload seed each store is
// handed is derived from the plan seed and the logical name, so two runs of the
// same plan store byte-identical payloads and the read-back compares against
// the same bytes.
func TestApplyStoreUsesDeterministicSeed(t *testing.T) {
	p := fullPlan()
	f1 := newFakeBarbican()
	if _, err := Apply(context.Background(), f1, p, 4, time.Second, p.Seed); err != nil {
		t.Fatalf("Apply() #1 = %v", err)
	}
	f2 := newFakeBarbican()
	if _, err := Apply(context.Background(), f2, p, 4, time.Second, p.Seed); err != nil {
		t.Fatalf("Apply() #2 = %v", err)
	}
	if !maps.Equal(f1.storeSeeds, f2.storeSeeds) {
		t.Errorf("store seeds differed across runs: %v vs %v", f1.storeSeeds, f2.storeSeeds)
	}
	for _, s := range p.Secrets {
		if got, want := f1.storeSeeds[s.Name], payload.Seed(p.Seed, s.Name); got != want {
			t.Errorf("store seed of %s = %d, want payload.Seed(seed, name) = %d", s.Name, got, want)
		}
	}
}

func TestApplyCreatedOrder(t *testing.T) {
	f := newFakeBarbican()
	p := fullPlan()
	result, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	want := []resource.Resource{
		storedRes("sec-0001"), storedRes("sec-0002"), storedRes("sec-0003"),
		orderRes("ord-0001"), generatedRes("ord-0001"),
		orderRes("ord-0002"), generatedRes("ord-0002"),
		containerRes("ctr-0001"), containerRes("ctr-0002"),
		aclRes("acl-0001", "sec-0001"), aclRes("acl-0002", "sec-0003"),
		consumerRes("con-0001", "ctr-0002"), consumerRes("con-0002", "ctr-0001"), consumerRes("con-0003", "ctr-0002"),
	}
	if !slices.Equal(result.Created, want) {
		t.Errorf("Created = %+v\nwant      %+v", result.Created, want)
	}
}

// TestApplyWithoutACLsOrConsumers confirms a plan with neither kind drives none
// of the four ACL and consumer seam calls and records the Created list apply
// recorded before they existed.
func TestApplyWithoutACLsOrConsumers(t *testing.T) {
	f := newFakeBarbican()
	p := basePlan()
	result, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	e := f.snapshot()
	for _, prefix := range newStagePrefixes {
		if i := indexOfPrefix(e, prefix); i >= 0 {
			t.Errorf("%s ran for a plan without ACLs or consumers: %v", e[i], e)
		}
	}
	want := []resource.Resource{
		storedRes("sec-0001"), storedRes("sec-0002"), storedRes("sec-0003"),
		orderRes("ord-0001"), generatedRes("ord-0001"),
		orderRes("ord-0002"), generatedRes("ord-0002"),
		containerRes("ctr-0001"), containerRes("ctr-0002"),
	}
	if !slices.Equal(result.Created, want) {
		t.Errorf("Created = %+v\nwant      %+v", result.Created, want)
	}
}

// TestApplyACLsAndConsumersTargetTheirParents confirms each ACL is set on the
// secret stage 1 stored for it and read back with the resource stage 4
// returned, and each consumer is registered on the container stage 3 created
// for it.
func TestApplyACLsAndConsumersTargetTheirParents(t *testing.T) {
	f := newFakeBarbican()
	p := fullPlan()
	if _, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	wantTargets := map[string]string{"acl-0001": "secret-sec-0001", "acl-0002": "secret-sec-0003"}
	if !maps.Equal(f.aclTargets, wantTargets) {
		t.Errorf("ACL targets = %v, want %v", f.aclTargets, wantTargets)
	}
	wantChecks := map[string]resource.Resource{
		"acl-0001": aclRes("acl-0001", "sec-0001"),
		"acl-0002": aclRes("acl-0002", "sec-0003"),
	}
	if !maps.Equal(f.aclChecks, wantChecks) {
		t.Errorf("ACL read-backs = %v, want %v", f.aclChecks, wantChecks)
	}
	wantCtrs := map[string]string{"con-0001": "container-ctr-0002", "con-0002": "container-ctr-0001", "con-0003": "container-ctr-0002"}
	if !maps.Equal(f.consumerCtrs, wantCtrs) {
		t.Errorf("consumer containers = %v, want %v", f.consumerCtrs, wantCtrs)
	}
}

// TestApplyVerifiesConsumersPerContainer confirms stage 8 reads back the
// consumer list once per container that has consumers, in container order,
// each call handed that container's consumers in plan order and exact true,
// and that a container without a consumer gets no call.
func TestApplyVerifiesConsumersPerContainer(t *testing.T) {
	f := newFakeBarbican()
	p := basePlan()
	p.Containers = append(p.Containers, plan.Container{Name: "ctr-0003", Secrets: []string{"sec-0002"}})
	p.Consumers = []plan.Consumer{
		{Name: "con-0001", Container: "ctr-0003"},
		{Name: "con-0002", Container: "ctr-0001"},
		{Name: "con-0003", Container: "ctr-0003"},
	}
	if _, err := Apply(context.Background(), f, p, 1, time.Second, p.Seed); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	want := []consumersCall{
		{container: "container-ctr-0001", want: []string{"con-0002"}, exact: true},
		{container: "container-ctr-0003", want: []string{"con-0001", "con-0003"}, exact: true},
	}
	if !reflect.DeepEqual(f.consumerCalls, want) {
		t.Errorf("VerifyConsumers calls = %+v, want %+v (none for ctr-0002)", f.consumerCalls, want)
	}
}

// TestApplySetACLRetriesTransient confirms an ACL write that fails twice with a
// 503 is retried and the third attempt's ACL is recorded.
func TestApplySetACLRetriesTransient(t *testing.T) {
	f := newFakeBarbican()
	unavailable := gophercloud.ErrUnexpectedResponseCode{Actual: 503}
	f.failWith("acl:acl-0001", unavailable, unavailable)
	p := fullPlan()
	result, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	if n := count(f.snapshot(), "acl:acl-0001"); n != 3 {
		t.Errorf("SetACL of acl-0001 attempted %d times, want 3 (two 503s, then success)", n)
	}
	if !slices.Contains(result.Created, aclRes("acl-0001", "sec-0001")) {
		t.Errorf("Created = %+v, want it to hold acl-0001", result.Created)
	}
}

// TestApplyConsumerQuotaFailsFast confirms a consumer registration rejected by
// a quota stops the run without a retry and without any read-back.
func TestApplyConsumerQuotaFailsFast(t *testing.T) {
	f := newFakeBarbican()
	f.failWith("consumer:con-0002", fmt.Errorf("creating consumer %q: %w", "con-0002", barbican.ErrQuota))
	p := fullPlan()
	result, err := Apply(context.Background(), f, p, 1, time.Second, p.Seed)
	if !errors.Is(err, barbican.ErrQuota) {
		t.Fatalf("Apply() = %v, want barbican.ErrQuota", err)
	}
	e := f.snapshot()
	if n := count(e, "consumer:con-0002"); n != 1 {
		t.Errorf("the quota-rejected registration was attempted %d times, want 1", n)
	}
	for _, prefix := range []string{"retrieve:", "verifyacl:", "verifyconsumers:"} {
		if i := indexOfPrefix(e, prefix); i >= 0 {
			t.Errorf("%s ran after a quota rejection in stage 5: %v", e[i], e)
		}
	}
	if !slices.Contains(result.Created, consumerRes("con-0001", "ctr-0002")) {
		t.Errorf("Created = %+v, want it to hold the consumer registered before the rejection", result.Created)
	}
}

// TestApplyCanceledDuringACLStage confirms an interrupt while the ACLs are set
// returns context.Canceled and starts no later stage.
func TestApplyCanceledDuringACLStage(t *testing.T) {
	f := newFakeBarbican()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onSetACL = cancel
	p := fullPlan()
	_, err := Apply(ctx, f, p, 4, time.Second, p.Seed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply() = %v, want context.Canceled", err)
	}
	e := f.snapshot()
	for _, prefix := range []string{"consumer:", "retrieve:", "verifyacl:", "verifyconsumers:"} {
		if i := indexOfPrefix(e, prefix); i >= 0 {
			t.Errorf("%s ran after the ACL stage was cancelled: %v", e[i], e)
		}
	}
}

// TestApplyACLMismatchFailsAfterOneAttempt confirms an ACL that reads back
// other users fails the run after one attempt, with the ACL still recorded.
func TestApplyACLMismatchFailsAfterOneAttempt(t *testing.T) {
	f := newFakeBarbican()
	f.failWith("verifyacl:secret-sec-0003",
		fmt.Errorf("%w: acl %q reads back users [], want [u3]", barbican.ErrACLMismatch, "acl-0002"))
	p := fullPlan()
	result, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed)
	if !errors.Is(err, barbican.ErrACLMismatch) {
		t.Fatalf("Apply() = %v, want barbican.ErrACLMismatch", err)
	}
	if n := count(f.snapshot(), "verifyacl:secret-sec-0003"); n != 1 {
		t.Errorf("the mismatching ACL read-back was attempted %d times, want 1", n)
	}
	if !slices.Contains(result.Created, aclRes("acl-0002", "sec-0003")) {
		t.Errorf("Created = %+v, want it to hold acl-0002", result.Created)
	}
}

// TestApplyConsumerMismatchFailsAfterOneAttempt confirms a consumer list that
// reads back wrong fails the run after one attempt, with the consumers still
// recorded.
func TestApplyConsumerMismatchFailsAfterOneAttempt(t *testing.T) {
	f := newFakeBarbican()
	f.failWith("verifyconsumers:container-ctr-0002",
		fmt.Errorf("%w: container container-ctr-0002 lacks consumer %q", barbican.ErrConsumerMismatch, "dizzy-run0-con-0003"))
	p := fullPlan()
	result, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed)
	if !errors.Is(err, barbican.ErrConsumerMismatch) {
		t.Fatalf("Apply() = %v, want barbican.ErrConsumerMismatch", err)
	}
	if n := count(f.snapshot(), "verifyconsumers:container-ctr-0002"); n != 1 {
		t.Errorf("the mismatching consumer read-back was attempted %d times, want 1", n)
	}
	for _, r := range []resource.Resource{consumerRes("con-0001", "ctr-0002"), consumerRes("con-0002", "ctr-0001"), consumerRes("con-0003", "ctr-0002")} {
		if !slices.Contains(result.Created, r) {
			t.Errorf("Created = %+v, want it to hold %s", result.Created, r.Logical)
		}
	}
}

// TestApplyContainersReferenceStoredSecrets confirms each container is handed
// the stage-1 map, so its references resolve to the stored secrets in plan
// order.
func TestApplyContainersReferenceStoredSecrets(t *testing.T) {
	f := newFakeBarbican()
	p := fullPlan()
	if _, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	want := map[string][]string{
		"ctr-0001": {"secret-sec-0001", "secret-sec-0003"},
		"ctr-0002": {"secret-sec-0002"},
	}
	if !maps.EqualFunc(f.containerRefs, want, slices.Equal) {
		t.Errorf("container references = %v, want %v", f.containerRefs, want)
	}
}

// TestApplyLogsEachCreate confirms every created resource, the generated
// secrets included, is logged as "created resource" with its kind, logical
// name, and id.
func TestApplyLogsEachCreate(t *testing.T) {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newFakeBarbican()
	p := fullPlan()
	result, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}

	var got []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec struct{ Msg, Kind, Logical, ID string }
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if rec.Msg == "created resource" {
			got = append(got, rec.Kind+"/"+rec.Logical+"/"+rec.ID)
		}
	}
	var want []string
	for _, r := range result.Created {
		want = append(want, string(r.Kind)+"/"+r.Logical+"/"+r.ID)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("created resource lines = %v, want %v", got, want)
	}
}

// syncBuffer is a bytes.Buffer safe for the concurrent writes of a stage's
// workers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestApplyQuotaFailsFast(t *testing.T) {
	f := newFakeBarbican()
	f.failWith("store:sec-0002", fmt.Errorf("creating secret %q: %w", "sec-0002", barbican.ErrQuota))
	p := fullPlan()
	result, err := Apply(context.Background(), f, p, 1, time.Second, p.Seed)
	if !errors.Is(err, barbican.ErrQuota) {
		t.Fatalf("Apply() = %v, want barbican.ErrQuota", err)
	}
	e := f.snapshot()
	for _, prefix := range append([]string{"order:", "wait:", "container:", "retrieve:"}, newStagePrefixes...) {
		if i := indexOfPrefix(e, prefix); i >= 0 {
			t.Errorf("%s ran after a quota rejection in stage 1: %v", e[i], e)
		}
	}
	if n := count(e, "store:sec-0002"); n != 1 {
		t.Errorf("the quota-rejected store was attempted %d times, want 1", n)
	}
	// sec-0001 was stored before sec-0002 hit the quota; it is recorded so cleanup
	// can reclaim it.
	if !slices.Contains(result.Created, storedRes("sec-0001")) {
		t.Errorf("Created = %+v, want it to hold the secret stored before the rejection", result.Created)
	}
}

func TestApplyOrderFailureFailsRun(t *testing.T) {
	f := newFakeBarbican()
	f.failWith("wait:order-ord-0001", fmt.Errorf("%w: order order-ord-0001: boom (status 500)", barbican.ErrOrderFailed))
	p := fullPlan()
	result, err := Apply(context.Background(), f, p, 1, time.Second, p.Seed)
	if !errors.Is(err, barbican.ErrOrderFailed) {
		t.Fatalf("Apply() = %v, want barbican.ErrOrderFailed", err)
	}
	for _, s := range p.Secrets {
		if !slices.Contains(result.Created, storedRes(s.Name)) {
			t.Errorf("Created lacks stored secret %s: %+v", s.Name, result.Created)
		}
	}
	if !slices.Contains(result.Created, orderRes("ord-0001")) {
		t.Errorf("Created lacks the failed order: %+v", result.Created)
	}
	if slices.Contains(result.Created, generatedRes("ord-0001")) {
		t.Errorf("Created holds a generated secret for an order that never turned ACTIVE: %+v", result.Created)
	}
	e := f.snapshot()
	if n := count(e, "wait:order-ord-0001"); n != 1 {
		t.Errorf("the failed order was waited on %d times, want 1", n)
	}
	for _, prefix := range append([]string{"stamp:gen-ord-0001", "container:", "retrieve:"}, newStagePrefixes...) {
		if i := indexOfPrefix(e, prefix); i >= 0 {
			t.Errorf("%s ran after the order failed: %v", e[i], e)
		}
	}
}

// TestApplyOrderWaitDeadlineIsFatal confirms the order wait is bounded by
// opTimeout and that a wait that runs out of time fails the run with the order
// still recorded.
func TestApplyOrderWaitDeadlineIsFatal(t *testing.T) {
	f := newFakeBarbican()
	f.blockWait = "order-ord-0001"
	p := &plan.Plan{Scenario: "t", Seed: 1, Orders: []plan.Order{{Name: "ord-0001"}}}
	result, err := Apply(context.Background(), f, p, 1, 50*time.Millisecond, p.Seed)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Apply() = %v, want context.DeadlineExceeded", err)
	}
	if want := []resource.Resource{orderRes("ord-0001")}; !slices.Equal(result.Created, want) {
		t.Errorf("Created = %+v, want %+v", result.Created, want)
	}
}

func TestApplyMismatchFailsAfterOneAttempt(t *testing.T) {
	f := newFakeBarbican()
	f.failWith("retrieve:secret-sec-0003",
		fmt.Errorf("%w: secret %q returned 8192 bytes, want 8192, first difference at byte 17", barbican.ErrPayloadMismatch, "sec-0003"))
	p := fullPlan()
	_, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed)
	if !errors.Is(err, barbican.ErrPayloadMismatch) {
		t.Fatalf("Apply() = %v, want barbican.ErrPayloadMismatch", err)
	}
	if n := count(f.snapshot(), "retrieve:secret-sec-0003"); n != 1 {
		t.Errorf("the mismatching read-back was attempted %d times, want 1", n)
	}
	for _, prefix := range []string{"verifyacl:", "verifyconsumers:"} {
		if i := indexOfPrefix(f.snapshot(), prefix); i >= 0 {
			t.Errorf("%s ran after a payload mismatch in stage 6", prefix)
		}
	}
}

func TestApplyStampFailureKeepsStoredSecret(t *testing.T) {
	f := newFakeBarbican()
	f.failWith("stamp:secret-sec-0002", policyDenied)
	p := fullPlan()
	result, err := Apply(context.Background(), f, p, 1, time.Second, p.Seed)
	if !gophercloud.ResponseCodeIs(err, 403) {
		t.Fatalf("Apply() = %v, want the stamp's 403", err)
	}
	if !slices.Contains(result.Created, storedRes("sec-0002")) {
		t.Errorf("Created = %+v, want it to hold the secret whose stamp failed", result.Created)
	}
	e := f.snapshot()
	if n := count(e, "stamp:secret-sec-0002"); n != 1 {
		t.Errorf("the denied stamp was attempted %d times, want 1", n)
	}
	if i := indexOfPrefix(e, "order:"); i >= 0 {
		t.Errorf("an order was created after a stage-1 failure: %v", e)
	}
}

// TestApplyGeneratedStampFailureKeepsBoth confirms a failed stamp of an order's
// generated secret still records both the order and the secret, the order
// first, so cleanup can reclaim both.
func TestApplyGeneratedStampFailureKeepsBoth(t *testing.T) {
	f := newFakeBarbican()
	f.failWith("stamp:gen-ord-0001", policyDenied)
	p := fullPlan()
	result, err := Apply(context.Background(), f, p, 1, time.Second, p.Seed)
	if !gophercloud.ResponseCodeIs(err, 403) {
		t.Fatalf("Apply() = %v, want the stamp's 403", err)
	}
	order, secret := slices.Index(result.Created, orderRes("ord-0001")), slices.Index(result.Created, generatedRes("ord-0001"))
	if order < 0 || secret < 0 {
		t.Fatalf("Created = %+v, want both the order and its generated secret", result.Created)
	}
	if secret != order+1 {
		t.Errorf("generated secret at %d does not directly follow its order at %d: %+v", secret, order, result.Created)
	}
	if i := indexOfPrefix(f.snapshot(), "container:"); i >= 0 {
		t.Errorf("a container was created after a stage-2 failure: %v", f.snapshot())
	}
}

// TestApplyRetriesTransient confirms a transient error is retried with backoff
// under the barbican classifiers and the store ultimately succeeds.
func TestApplyRetriesTransient(t *testing.T) {
	f := newFakeBarbican()
	f.failWith("store:sec-0001", gophercloud.ErrUnexpectedResponseCode{Actual: 503})
	p := &plan.Plan{Scenario: "t", Seed: 1, Secrets: []plan.Secret{{Name: "sec-0001", PayloadBytes: 1}}}
	result, err := Apply(context.Background(), f, p, 1, time.Second, p.Seed)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	if n := count(f.snapshot(), "store:sec-0001"); n != 2 {
		t.Errorf("store attempted %d times, want 2 (one 503, then success)", n)
	}
	if want := []resource.Resource{storedRes("sec-0001")}; !slices.Equal(result.Created, want) {
		t.Errorf("Created = %+v, want %+v", result.Created, want)
	}
}

func TestApplyEmptyPlan(t *testing.T) {
	f := newFakeBarbican()
	p := &plan.Plan{Scenario: "t", Seed: 1, Secrets: []plan.Secret{}, Orders: []plan.Order{}, Containers: []plan.Container{}}
	result, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	if len(result.Created) != 0 {
		t.Errorf("Created = %+v, want empty", result.Created)
	}
	if e := f.snapshot(); len(e) != 0 {
		t.Errorf("an empty plan drove %d seam calls: %v", len(e), e)
	}
}

func TestApplyCanceledBeforeStart(t *testing.T) {
	f := newFakeBarbican()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := fullPlan()
	_, err := Apply(ctx, f, p, 4, time.Second, p.Seed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply() = %v, want context.Canceled", err)
	}
	if e := f.snapshot(); len(e) != 0 {
		t.Errorf("a cancelled apply drove %d seam calls: %v", len(e), e)
	}
}
