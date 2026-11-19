package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/B42Labs/dizzy/internal/c5c3"
	"github.com/B42Labs/dizzy/internal/resource"
)

// cleanupRun is the run id the cleanup tests discover by.
const cleanupRun = "run0"

// fiveOrders are the logical names of a run's five orders.
var fiveOrders = []string{"user", "project", "role", "catalog", "appcred"}

// fakeOrder is one order the fake cleaner holds, with what the operator, the
// garbage collector and Keystone do with it.
type fakeOrder struct {
	res resource.Resource
	// unlisted keeps the order out of ListByRun, so only a run record reaches
	// it.
	unlisted bool
	// deleting is set by Delete, gone once the order has disappeared.
	deleting, gone bool
	// stuck keeps the order present after its Delete: the operator never
	// releases its finalizer.
	stuck bool
	// reason is what Observe reports for the order while no other order holds
	// it; empty reads as Ready.
	reason string
	// specGen and gen are the order's spec and status credential generations.
	specGen, gen int64
	// secret is true while the Secret beside a user or application credential
	// order exists; secretStays keeps it after the order is gone.
	secret, secretStays bool
	// accepted makes Keystone accept the order's credential after the order is
	// gone.
	accepted bool
}

// credential is what the Secret beside o carries. Its resourceVersion is the
// order's logical name, so a login names the order it belongs to.
func (o *fakeOrder) credential() c5c3.Credential {
	l := o.res.Logical
	if o.res.Kind == c5c3.KindUser {
		password := userPasswordPrefix + l
		return c5c3.Credential{
			CloudsYAML:      []byte("clouds:\n  openstack:\n    auth:\n      password: " + password + "\n"),
			Cloud:           "openstack",
			ResourceVersion: l,
			Password:        password,
		}
	}
	secret := appcredSecretPrefix + l
	return c5c3.Credential{
		CloudsYAML:                  []byte("clouds:\n  openstack:\n    auth:\n      application_credential_secret: " + secret + "\n"),
		Cloud:                       "openstack",
		ResourceVersion:             l,
		ApplicationCredentialID:     "appcred-id-" + l,
		ApplicationCredentialSecret: secret,
	}
}

// fakeCleaner is an in-memory Cleaner that keeps the operator's holds. A
// deleted order disappears at its first read once nothing holds it: an
// application credential order holds the user, the project and the role
// assignment, a role assignment holds the user and the project. Its Secret
// goes with it unless secretStays. A credential is rejected once its order is
// gone, unless accepted is set. Every call is recorded as one event, its key,
// verb:<logical> for a call on an order, list:<kind> for a listing and
// reject:<logical> for a login, followed by its arguments, all joined by
// colons. The fake records gone:<logical> when an order disappears and
// ended:<key> when a wait's context ends. A call answers with the context's
// error once its context has ended, otherwise with the next error queued for
// its key in fail, otherwise with the error of the hook installed for its key,
// and otherwise does what the order's state says.
type fakeCleaner struct {
	mu     sync.Mutex
	events []string
	// changed is closed and replaced on every event and state change, so a
	// wait can block until the next one.
	changed chan struct{}
	// deadlines maps an event key to the time left on the context of its
	// first call, or -1 for a context without a deadline.
	deadlines map[string]time.Duration
	fail      map[string][]error
	hooks     map[string]func(context.Context) error
	orders    []*fakeOrder
}

// newFakeCleaner returns a fake holding the orders the fake Orders creates for
// logicals, each live, listed by its run label, settled at generation 1 and,
// for a user or application credential, with its Secret delivered.
func newFakeCleaner(logicals ...string) *fakeCleaner {
	f := &fakeCleaner{
		changed:   make(chan struct{}),
		deadlines: map[string]time.Duration{},
		fail:      map[string][]error{},
		hooks:     map[string]func(context.Context) error{},
	}
	for _, r := range orders(logicals...) {
		f.add(r)
	}
	return f
}

// add holds r as a live order settled at generation 1, with a delivered Secret
// for a user or application credential order.
func (f *fakeCleaner) add(r resource.Resource) *fakeOrder {
	o := &fakeOrder{
		res:     r,
		specGen: 1,
		gen:     1,
		secret:  r.Kind == c5c3.KindUser || r.Kind == c5c3.KindApplicationCredential,
	}
	f.orders = append(f.orders, o)
	return o
}

// order returns the order named logical. Tests set its fields before Cleanup
// starts, or through update while it runs.
func (f *fakeCleaner) order(logical string) *fakeOrder {
	for _, o := range f.orders {
		if o.res.Logical == logical {
			return o
		}
	}
	panic("fake cleaner holds no order " + logical)
}

// find returns the order with id, or nil. The caller holds f.mu.
func (f *fakeCleaner) find(id string) *fakeOrder {
	for _, o := range f.orders {
		if o.res.ID == id {
			return o
		}
	}
	return nil
}

// heldBy returns the reason the operator reports for o while another order
// holds its deletion, and "" when none does. The caller holds f.mu.
func (f *fakeCleaner) heldBy(o *fakeOrder) string {
	exists := func(kind resource.Kind) bool {
		return slices.ContainsFunc(f.orders, func(h *fakeOrder) bool { return h.res.Kind == kind && !h.gone })
	}
	switch o.res.Kind {
	case c5c3.KindUser, c5c3.KindProject:
		if exists(c5c3.KindRoleAssignment) {
			return "ReferencedByRoleAssignments"
		}
		if exists(c5c3.KindApplicationCredential) {
			return "ReferencedByApplicationCredentials"
		}
	case c5c3.KindRoleAssignment:
		if exists(c5c3.KindApplicationCredential) {
			return "ReferencedByApplicationCredentials"
		}
	}
	return ""
}

// present reports whether o exists when it is read. A deleted order that is
// neither stuck nor held disappears here, and its Secret with it unless
// secretStays. The caller holds f.mu.
func (f *fakeCleaner) present(o *fakeOrder) bool {
	if o.gone {
		return false
	}
	if !o.deleting || o.stuck || f.heldBy(o) != "" {
		return true
	}
	o.gone = true
	o.secret = o.secret && o.secretStays
	f.record("gone:" + o.res.Logical)
	return false
}

// signal wakes every wait. The caller holds f.mu.
func (f *fakeCleaner) signal() {
	close(f.changed)
	f.changed = make(chan struct{})
}

// record appends ev to the events. The caller holds f.mu.
func (f *fakeCleaner) record(ev string) {
	f.events = append(f.events, ev)
	f.signal()
}

// update changes the fake's state through fn while Cleanup may run, and
// records ev.
func (f *fakeCleaner) update(ev string, fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
	f.record(ev)
}

// failWith queues errs as the answers of the next calls recorded under key.
func (f *fakeCleaner) failWith(key string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[key] = append(f.fail[key], errs...)
}

// on installs hook as the answer of every call recorded under key that no
// queued error answers. A nil answer lets the call go on as usual.
func (f *fakeCleaner) on(key string, hook func(context.Context) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks[key] = hook
}

// mark records ev without answering a call.
func (f *fakeCleaner) mark(ev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ev)
}

// call records the call key with its arguments as one event and answers it
// from the context, the queued errors and the hooks. A nil answer means the
// method goes on with the order's state.
func (f *fakeCleaner) call(ctx context.Context, key string, args ...string) error {
	f.mu.Lock()
	f.record(strings.Join(append([]string{key}, args...), ":"))
	if _, seen := f.deadlines[key]; !seen {
		left := time.Duration(-1)
		if d, ok := ctx.Deadline(); ok {
			left = time.Until(d)
		}
		f.deadlines[key] = left
	}
	var queued error
	if q := f.fail[key]; len(q) > 0 {
		queued, f.fail[key] = q[0], q[1:]
	}
	hook := f.hooks[key]
	f.mu.Unlock()

	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case queued != nil:
		return queued
	case hook != nil:
		return hook(ctx)
	}
	return nil
}

// until blocks until done, evaluated under f.mu on every change, reports the
// wait over or returns an error. When ctx ends first it records ended:<key>
// and returns ctx.Err().
func (f *fakeCleaner) until(ctx context.Context, key string, done func() (bool, error)) error {
	for {
		f.mu.Lock()
		ok, err := done()
		changed := f.changed
		f.mu.Unlock()
		if ok || err != nil {
			return err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			f.mark("ended:" + key)
			return ctx.Err()
		}
	}
}

func (f *fakeCleaner) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.events)
}

// waitFor blocks until cond holds for the events recorded so far, and returns
// ctx.Err() when ctx ends first.
func (f *fakeCleaner) waitFor(ctx context.Context, cond func([]string) bool) error {
	for {
		f.mu.Lock()
		ok, changed := cond(f.events), f.changed
		f.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// orderNotFound is the 404 the API server answers for r once it is gone.
func orderNotFound(r resource.Resource) error {
	return apierrors.NewNotFound(schema.GroupResource{Group: "c5c3.io", Resource: string(r.Kind)}, r.Name)
}

func (f *fakeCleaner) ListByRun(ctx context.Context, kind resource.Kind, runID string) ([]resource.Resource, error) {
	if err := f.call(ctx, "list:"+string(kind), runID); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []resource.Resource
	for _, o := range f.orders {
		if o.res.Kind == kind && !o.unlisted && f.present(o) {
			out = append(out, o.res)
		}
	}
	return out, nil
}

func (f *fakeCleaner) Status(ctx context.Context, r resource.Resource) (c5c3.OrderStatus, error) {
	if err := f.call(ctx, "status:"+r.Logical); err != nil {
		return c5c3.OrderStatus{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.find(r.ID)
	if o == nil || !f.present(o) {
		return c5c3.OrderStatus{}, fmt.Errorf("reading order %s: %w", r.ID, orderNotFound(r))
	}
	return c5c3.OrderStatus{
		Ready:          !o.deleting && o.reason == "",
		Deleting:       o.deleting,
		Reason:         o.reason,
		SpecGeneration: o.specGen,
		Generation:     o.gen,
	}, nil
}

func (f *fakeCleaner) LookupCredential(ctx context.Context, order resource.Resource) (c5c3.Credential, bool, error) {
	if err := f.call(ctx, "lookup:"+order.Logical); err != nil {
		return c5c3.Credential{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.find(order.ID)
	if o == nil || !o.secret {
		return c5c3.Credential{}, false, nil
	}
	return o.credential(), true, nil
}

func (f *fakeCleaner) WaitGenerationSettled(ctx context.Context, r resource.Resource) error {
	key := "waitsettled:" + r.Logical
	if err := f.call(ctx, key); err != nil {
		return err
	}
	return f.until(ctx, key, func() (bool, error) {
		o := f.find(r.ID)
		if o == nil || !f.present(o) {
			return false, fmt.Errorf("order %s is gone while waiting for the generation to settle: %w", r.ID, orderNotFound(r))
		}
		return o.gen >= o.specGen || o.deleting, nil
	})
}

func (f *fakeCleaner) Delete(ctx context.Context, r resource.Resource) error {
	if err := f.call(ctx, "delete:"+r.Logical); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if o := f.find(r.ID); o != nil && !o.gone {
		o.deleting = true
		f.signal()
	}
	return nil
}

func (f *fakeCleaner) WaitGone(ctx context.Context, r resource.Resource) error {
	key := "waitgone:" + r.Logical
	if err := f.call(ctx, key); err != nil {
		return err
	}
	return f.until(ctx, key, func() (bool, error) {
		o := f.find(r.ID)
		return o == nil || !f.present(o), nil
	})
}

func (f *fakeCleaner) Observe(ctx context.Context, r resource.Resource) (string, bool, error) {
	if err := f.call(ctx, "observe:"+r.Logical); err != nil {
		return "", false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.find(r.ID)
	if o == nil || !f.present(o) {
		return "", false, nil
	}
	if held := f.heldBy(o); o.deleting && held != "" {
		return held, true, nil
	}
	if o.reason != "" {
		return o.reason, true, nil
	}
	return "Ready", true, nil
}

func (f *fakeCleaner) WaitSecretGone(ctx context.Context, order resource.Resource) error {
	key := "waitsecretgone:" + order.Logical
	if err := f.call(ctx, key); err != nil {
		return err
	}
	err := f.until(ctx, key, func() (bool, error) {
		o := f.find(order.ID)
		return o == nil || !o.secret, nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: secret %s-credentials is still present after order %s is gone: %w", c5c3.ErrMismatch, order.ID, order.ID, err)
	}
	return err
}

func (f *fakeCleaner) Reject(ctx context.Context, kind resource.Kind, cred c5c3.Credential) error {
	if err := f.call(ctx, "reject:"+cred.ResourceVersion, string(kind)); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.orders, func(o *fakeOrder) bool { return o.res.Logical == cred.ResourceVersion })
	if i < 0 || f.orders[i].res.Kind != kind {
		return fmt.Errorf("fake: a %s login with %s, which no order of that kind delivered", kind, cred)
	}
	if o := f.orders[i]; !o.gone || o.accepted {
		return fmt.Errorf("%w: keystone issued a token for %s", c5c3.ErrAccepted, cred)
	}
	return nil
}

// The fake must satisfy the seam.
var _ Cleaner = (*fakeCleaner)(nil)

// orderOf returns the resource of an order of kind named after logical, as the
// fake Orders creates it.
func orderOf(kind resource.Kind, logical string) resource.Resource {
	name := "dizzyorder-run0-" + logical
	return resource.Resource{Kind: kind, Logical: logical, Name: name, ID: testNamespace + "/" + name}
}

// idOf returns the id of the order named after logical.
func idOf(logical string) string {
	return testNamespace + "/dizzyorder-run0-" + logical
}

// runCleanup cleans up the run against f with the tests' bounds and two
// workers.
func runCleanup(f *fakeCleaner, recorded []resource.Resource) (int, error) {
	return Cleanup(context.Background(), f, cleanupRun, recorded, 2, testOpTimeout, testReadyTimeout, testGoneTimeout)
}

// checkBounds checks that the first call of each verb in bounds ran with just
// under its bound left on its context.
func checkBounds(t *testing.T, f *fakeCleaner, bounds map[string]time.Duration) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	for key, left := range f.deadlines {
		verb, _, _ := strings.Cut(key, ":")
		want, ok := bounds[verb]
		if !ok {
			continue
		}
		seen[verb] = true
		if left > want || left < want-500*time.Millisecond {
			t.Errorf("%s ran with %s left on its context, want just under %s", key, left, want)
		}
	}
	for verb := range bounds {
		if !seen[verb] {
			t.Errorf("no %s call was made", verb)
		}
	}
}

// hasEvent returns a condition for waitFor that holds once ev is recorded.
func hasEvent(ev string) func([]string) bool {
	return func(e []string) bool { return slices.Contains(e, ev) }
}

// fullCleanup is every call a Cleanup of the five settled orders makes, with
// the fake's gone events.
var fullCleanup = []string{
	"list:keystone_application_credential:run0",
	"list:keystone_role_assignment:run0",
	"list:keystone_catalog_entry:run0",
	"list:keystone_project:run0",
	"list:keystone_user:run0",
	"lookup:appcred", "lookup:user",
	"waitsettled:user",
	"delete:appcred", "waitgone:appcred", "gone:appcred",
	"delete:role", "waitgone:role", "gone:role",
	"delete:catalog", "waitgone:catalog", "gone:catalog",
	"delete:project", "waitgone:project", "gone:project",
	"delete:user", "waitgone:user", "gone:user",
	"waitsecretgone:appcred", "reject:appcred:keystone_application_credential",
	"waitsecretgone:user", "reject:user:keystone_user",
}

func TestCleanupOrder(t *testing.T) {
	f := newFakeCleaner(fiveOrders...)
	deleted, err := runCleanup(f, nil)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want 5", deleted)
	}

	e := f.snapshot()
	var deletes []string
	for _, ev := range e {
		if strings.HasPrefix(ev, "delete:") {
			deletes = append(deletes, ev)
		}
	}
	sequence := []string{"appcred", "role", "catalog", "project", "user"}
	want := make([]string, 0, len(sequence))
	for _, l := range sequence {
		want = append(want, "delete:"+l)
	}
	if !slices.Equal(deletes, want) {
		t.Errorf("deletes = %v, want %v", deletes, want)
	}
	// Each kind is deleted only once the kind before it is gone.
	for i := 1; i < len(sequence); i++ {
		if gone, next := index(t, e, "gone:"+sequence[i-1]), index(t, e, "delete:"+sequence[i]); gone > next {
			t.Errorf("delete:%s at %d comes before %s is gone at %d: %v", sequence[i], next, sequence[i-1], gone, e)
		}
	}
	if !slices.Equal(e, fullCleanup) {
		t.Errorf("calls =\n%s\nwant\n%s", strings.Join(e, "\n"), strings.Join(fullCleanup, "\n"))
	}
}

func TestCleanupEmpty(t *testing.T) {
	f := newFakeCleaner()
	deleted, err := runCleanup(f, nil)
	if err != nil || deleted != 0 {
		t.Fatalf("Cleanup() = %d, %v, want 0, nil", deleted, err)
	}
	e := f.snapshot()
	if n := countPrefix(e, "list:"); n != 5 || len(e) != 5 {
		t.Errorf("calls = %v, want only the five listings", e)
	}
}

func TestCleanupRefusesEmptyRunID(t *testing.T) {
	f := newFakeCleaner(fiveOrders...)
	_, err := Cleanup(context.Background(), f, "", orders(fiveOrders...), 2, testOpTimeout, testReadyTimeout, testGoneTimeout)
	const want = "cleanup: empty run id; refusing to delete by empty identity"
	if err == nil || err.Error() != want {
		t.Fatalf("Cleanup with an empty run id = %v, want %q", err, want)
	}
	if e := f.snapshot(); len(e) != 0 {
		t.Errorf("Cleanup with an empty run id made %d calls: %v", len(e), e)
	}
}

func TestCleanupIdempotent(t *testing.T) {
	f := newFakeCleaner(fiveOrders...)
	recorded := orders(fiveOrders...)
	if deleted, err := runCleanup(f, recorded); err != nil || deleted != 5 {
		t.Fatalf("first Cleanup() = %d, %v, want 5, nil", deleted, err)
	}

	// Rediscovered by label, the run has no order left.
	before := len(f.snapshot())
	deleted, err := runCleanup(f, nil)
	if err != nil || deleted != 0 {
		t.Fatalf("second Cleanup() = %d, %v, want 0, nil", deleted, err)
	}
	if again := f.snapshot()[before:]; countPrefix(again, "list:") != len(again) {
		t.Errorf("second Cleanup made calls beyond the listings: %v", again)
	}

	// Through the record, each Delete reaches an order that is already gone:
	// nothing is deleted, nothing is checked, and each order counts as
	// confirmed gone.
	before = len(f.snapshot())
	deleted, err = runCleanup(f, recorded)
	if err != nil {
		t.Fatalf("Cleanup() with the record = %v, want nil", err)
	}
	if deleted != 5 {
		t.Errorf("Cleanup() with the record deleted = %d, want 5 orders confirmed gone", deleted)
	}
	again := f.snapshot()[before:]
	if n := countPrefix(again, "delete:"); n != 5 {
		t.Errorf("Cleanup() with the record sent %d deletes, want 5: %v", n, again)
	}
	noneWithPrefix(t, again, "gone:", "waitsecretgone:", "reject:")
}

func TestCleanupUnionsRecord(t *testing.T) {
	f := newFakeCleaner(fiveOrders...)
	// The label listing misses the role assignment; only the record reaches
	// it. The record also repeats the listed user and holds an entry of
	// another kind and one without an id, which are dropped.
	f.order("role").unlisted = true
	recorded := append(orders("role", "user"),
		resource.Resource{Kind: c5c3.KindSecret, Logical: "user", ID: idOf("user") + "-credentials"},
		resource.Resource{Kind: c5c3.KindProject, Logical: "project"},
	)

	deleted, err := runCleanup(f, recorded)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want 5", deleted)
	}
	e := f.snapshot()
	for _, l := range fiveOrders {
		if n := count(e, "delete:"+l); n != 1 {
			t.Errorf("delete:%s sent %d times, want once: %v", l, n, e)
		}
	}
	if n := countPrefix(e, "delete:"); n != 5 {
		t.Errorf("%d deletes sent, want 5: %v", n, e)
	}
}

func TestCleanupHeldOrder(t *testing.T) {
	f := newFakeCleaner(fiveOrders...)
	appcred := f.order("appcred")
	appcred.stuck = true
	appcred.reason = "CredentialFailed"
	const goneTimeout = 100 * time.Millisecond

	deleted, err := Cleanup(context.Background(), f, cleanupRun, nil, 2, testOpTimeout, testReadyTimeout, goneTimeout)
	if err == nil {
		t.Fatal("Cleanup() = nil, want an error naming the orders still present")
	}
	for l, state := range map[string]string{
		"appcred": "keystone_application_credential, CredentialFailed",
		"role":    "keystone_role_assignment, ReferencedByApplicationCredentials",
		"project": "keystone_project, ReferencedByRoleAssignments",
		"user":    "keystone_user, ReferencedByRoleAssignments",
	} {
		if want := idOf(l) + " (" + state + ")"; !strings.Contains(err.Error(), want) {
			t.Errorf("Cleanup() = %q, want it to name %s", err, want)
		}
	}
	if strings.Contains(err.Error(), idOf("catalog")) {
		t.Errorf("Cleanup() = %q, want it not to name the catalog entry, which is gone", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (the catalog entry, which nothing holds)", deleted)
	}

	e := f.snapshot()
	timedOut := index(t, e, "ended:waitgone:appcred")
	for _, l := range []string{"role", "catalog", "project", "user"} {
		if i := index(t, e, "delete:"+l); i < timedOut {
			t.Errorf("delete:%s at %d comes before the credential order's wait ended at %d: %v", l, i, timedOut, e)
		}
	}
	if n := countPrefix(e, "waitgone:"); n != 1 {
		t.Errorf("%d waits for an order to be gone ran, want only the credential order's: %v", n, e)
	}
	noneWithPrefix(t, e, "waitsecretgone:", "reject:")
	checkBounds(t, f, map[string]time.Duration{"waitgone": goneTimeout, "observe": testOpTimeout})

	// Once the operator releases the finalizer, the held orders go on their
	// own, and a second Cleanup finds nothing left.
	f.update("released:appcred", func() { appcred.stuck = false })
	before := len(f.snapshot())
	deleted, err = Cleanup(context.Background(), f, cleanupRun, nil, 2, testOpTimeout, testReadyTimeout, goneTimeout)
	if err != nil || deleted != 0 {
		t.Fatalf("Cleanup() after the hold lifted = %d, %v, want 0, nil", deleted, err)
	}
	again := f.snapshot()[before:]
	noneWithPrefix(t, again, "delete:")
	for _, l := range []string{"appcred", "role", "project", "user"} {
		if !slices.Contains(again, "gone:"+l) {
			t.Errorf("%s did not go once the hold lifted: %v", l, again)
		}
	}
}

func TestCleanupWaitsForPasswordRotation(t *testing.T) {
	const unsettled = "did not settle"

	t.Run("the status catches up", func(t *testing.T) {
		logs := captureLogs(t)
		f := newFakeCleaner(fiveOrders...)
		user := f.order("user")
		user.specGen = 2

		type result struct {
			deleted int
			err     error
		}
		done := make(chan result, 1)
		go func() {
			deleted, err := runCleanup(f, nil)
			done <- result{deleted, err}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), testReadyTimeout)
		defer cancel()
		if err := f.waitFor(ctx, hasEvent("waitsettled:user")); err != nil {
			t.Fatalf("no wait for the user's generation: %v", f.snapshot())
		}
		// While the spec generation is above the status, no delete is sent.
		quiet, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := f.waitFor(quiet, func(e []string) bool { return countPrefix(e, "delete:") > 0 })
		stop()
		if err == nil {
			t.Errorf("a delete was sent while the password rotated: %v", f.snapshot())
		}
		f.update("caught-up:user", func() { user.gen = 2 })

		var res result
		select {
		case res = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Cleanup did not return")
		}
		if res.err != nil || res.deleted != 5 {
			t.Fatalf("Cleanup() = %d, %v, want 5, nil", res.deleted, res.err)
		}
		e := f.snapshot()
		if caught, del := index(t, e, "caught-up:user"), index(t, e, "delete:appcred"); del < caught {
			t.Errorf("delete:appcred at %d comes before the status caught up at %d: %v", del, caught, e)
		}
		if strings.Contains(logs.String(), unsettled) {
			t.Errorf("a settled generation was logged as unsettled:\n%s", logs)
		}
		checkBounds(t, f, map[string]time.Duration{"waitsettled": testReadyTimeout})
	})

	t.Run("readyTimeout passes first", func(t *testing.T) {
		logs := captureLogs(t)
		f := newFakeCleaner(fiveOrders...)
		f.order("user").specGen = 2
		const readyTimeout = 150 * time.Millisecond

		start := time.Now()
		deleted, err := Cleanup(context.Background(), f, cleanupRun, nil, 2, testOpTimeout, readyTimeout, testGoneTimeout)
		if err != nil || deleted != 5 {
			t.Fatalf("Cleanup() = %d, %v, want 5, nil", deleted, err)
		}
		if took := time.Since(start); took < readyTimeout {
			t.Errorf("Cleanup took %s, want at least readyTimeout %s", took, readyTimeout)
		}
		e := f.snapshot()
		if ended, del := index(t, e, "ended:waitsettled:user"), index(t, e, "delete:appcred"); del < ended {
			t.Errorf("delete:appcred at %d comes before the wait for the user's generation ended at %d: %v", del, ended, e)
		}
		out := logs.String()
		for _, want := range []string{unsettled, `"id":"` + idOf("user") + `"`, `"specGeneration":2`, `"generation":1`} {
			if !strings.Contains(out, want) {
				t.Errorf("log lacks %s:\n%s", want, out)
			}
		}
		checkBounds(t, f, map[string]time.Duration{"waitsettled": readyTimeout, "status": testOpTimeout})
	})

	t.Run("a user order already gone counts as settled", func(t *testing.T) {
		logs := captureLogs(t)
		f := newFakeCleaner("appcred")
		user := f.add(orderOf(c5c3.KindUser, "user"))
		user.gone, user.secret = true, false

		deleted, err := runCleanup(f, orders("user"))
		if err != nil || deleted != 2 {
			t.Fatalf("Cleanup() = %d, %v, want 2, nil (the user order was already gone)", deleted, err)
		}
		e := f.snapshot()
		index(t, e, "waitsettled:user")
		noneWithPrefix(t, e, "status:", "reject:user")
		if strings.Contains(logs.String(), unsettled) {
			t.Errorf("a gone user order was logged as unsettled:\n%s", logs)
		}
	})
}

// TestCleanupSkipsSettleWithoutCredential confirms the wait for a user
// order's generation only runs when there is an application credential order
// to delete: a user order that never got provisioned has a spec generation of
// 1 and a status generation of 0, and waiting on it would only run into
// readyTimeout.
func TestCleanupSkipsSettleWithoutCredential(t *testing.T) {
	f := newFakeCleaner("user", "project")
	f.order("user").gen = 0
	deleted, err := runCleanup(f, nil)
	if err != nil || deleted != 2 {
		t.Fatalf("Cleanup() = %d, %v, want 2, nil", deleted, err)
	}
	noneWithPrefix(t, f.snapshot(), "waitsettled:")
}

func TestCleanupAcceptedCredential(t *testing.T) {
	t.Run("the second credential is still checked", func(t *testing.T) {
		f := newFakeCleaner(fiveOrders...)
		f.order("appcred").accepted = true
		deleted, err := runCleanup(f, nil)
		if !errors.Is(err, c5c3.ErrAccepted) {
			t.Fatalf("Cleanup() = %v, want ErrAccepted", err)
		}
		if !strings.Contains(err.Error(), idOf("appcred")) {
			t.Errorf("Cleanup() = %q, want it to name the credential order", err)
		}
		if deleted != 5 {
			t.Errorf("deleted = %d, want 5", deleted)
		}
		e := f.snapshot()
		if n := count(e, "reject:appcred:keystone_application_credential"); n != 1 {
			t.Errorf("the accepted credential was sent %d times, want once", n)
		}
		if first, second := index(t, e, "reject:appcred:keystone_application_credential"), index(t, e, "reject:user:keystone_user"); second < first {
			t.Errorf("the user's login at %d comes before the credential's at %d: %v", second, first, e)
		}
	})

	t.Run("the first error is returned and the second logged", func(t *testing.T) {
		logs := captureLogs(t)
		f := newFakeCleaner(fiveOrders...)
		f.order("appcred").accepted = true
		f.order("user").accepted = true
		_, err := runCleanup(f, nil)
		if !errors.Is(err, c5c3.ErrAccepted) {
			t.Fatalf("Cleanup() = %v, want ErrAccepted", err)
		}
		if !strings.Contains(err.Error(), idOf("appcred")) || strings.Contains(err.Error(), idOf("user")) {
			t.Errorf("Cleanup() = %q, want it to name the credential order only", err)
		}
		out := logs.String()
		if !strings.Contains(out, "credential check after the delete failed") || !strings.Contains(out, idOf("user")) {
			t.Errorf("log lacks the user's failed check:\n%s", out)
		}
		index(t, f.snapshot(), "reject:user:keystone_user")
	})
}

func TestCleanupSecretStillPresent(t *testing.T) {
	f := newFakeCleaner(fiveOrders...)
	f.order("appcred").secretStays = true
	const opTimeout = 100 * time.Millisecond

	deleted, err := Cleanup(context.Background(), f, cleanupRun, nil, 2, opTimeout, testReadyTimeout, testGoneTimeout)
	if !errors.Is(err, c5c3.ErrMismatch) {
		t.Fatalf("Cleanup() = %v, want ErrMismatch", err)
	}
	if errors.Is(err, c5c3.ErrAccepted) {
		t.Errorf("Cleanup() = %v, want no ErrAccepted: Keystone rejects both credentials", err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want 5", deleted)
	}
	e := f.snapshot()
	index(t, e, "ended:waitsecretgone:appcred")
	index(t, e, "reject:user:keystone_user")
	checkBounds(t, f, map[string]time.Duration{"waitsecretgone": opTimeout})
}

func TestCleanupMissingSecretSkipsVerify(t *testing.T) {
	tests := []struct {
		name    string
		removed []string
	}{
		{"the user's secret", []string{"user"}},
		{"both secrets", []string{"user", "appcred"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeCleaner(fiveOrders...)
			for _, l := range tt.removed {
				f.order(l).secret = false
			}
			deleted, err := runCleanup(f, nil)
			if err != nil || deleted != 5 {
				t.Fatalf("Cleanup() = %d, %v, want 5, nil", deleted, err)
			}
			e := f.snapshot()
			for _, l := range []string{"user", "appcred"} {
				index(t, e, "lookup:"+l)
				want := !slices.Contains(tt.removed, l)
				rejected := slices.ContainsFunc(e, func(ev string) bool { return strings.HasPrefix(ev, "reject:"+l+":") })
				if rejected != want {
					t.Errorf("credential of %s sent to Keystone: %t, want %t: %v", l, rejected, want, e)
				}
				if waited := slices.Contains(e, "waitsecretgone:"+l); waited != want {
					t.Errorf("secret of %s waited on: %t, want %t: %v", l, waited, want, e)
				}
			}
		})
	}
}

// TestCleanupDeleteFailure confirms a Delete that fails after its retries is
// handled like an order that stays: the deletes of the remaining kinds are
// still sent without their waits, and the error names the orders still
// present and wraps the Delete's error.
func TestCleanupDeleteFailure(t *testing.T) {
	f := newFakeCleaner(fiveOrders...)
	denied := apierrors.NewForbidden(schema.GroupResource{Group: "c5c3.io", Resource: "keystoneroleassignments"}, "dizzyorder-run0-role", errors.New("cannot delete"))
	f.failWith("delete:role", fmt.Errorf("deleting order %s: %w", idOf("role"), denied))

	deleted, err := runCleanup(f, nil)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("Cleanup() = %v, want it to wrap the 403", err)
	}
	for l, state := range map[string]string{
		"role":    "keystone_role_assignment, Ready",
		"project": "keystone_project, ReferencedByRoleAssignments",
		"user":    "keystone_user, ReferencedByRoleAssignments",
	} {
		if want := idOf(l) + " (" + state + ")"; !strings.Contains(err.Error(), want) {
			t.Errorf("Cleanup() = %q, want it to name %s", err, want)
		}
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (the credential order and the catalog entry)", deleted)
	}
	e := f.snapshot()
	if n := count(e, "delete:role"); n != 1 {
		t.Errorf("the forbidden delete was sent %d times, want once", n)
	}
	for _, l := range []string{"catalog", "project", "user"} {
		index(t, e, "delete:"+l)
	}
	if n := countPrefix(e, "waitgone:"); n != 1 {
		t.Errorf("%d waits for an order to be gone ran, want only the credential order's: %v", n, e)
	}
	index(t, e, "reject:appcred:keystone_application_credential")
	noneWithPrefix(t, e, "reject:user")
}

func TestCleanupRetriesDelete(t *testing.T) {
	f := newFakeCleaner(fiveOrders...)
	f.failWith("delete:role", apierrors.NewServiceUnavailable("the API server is restarting"))
	deleted, err := runCleanup(f, nil)
	if err != nil || deleted != 5 {
		t.Fatalf("Cleanup() = %d, %v, want 5, nil after the retry", deleted, err)
	}
	if n := count(f.snapshot(), "delete:role"); n != 2 {
		t.Errorf("delete:role sent %d times, want 2", n)
	}
	checkBounds(t, f, map[string]time.Duration{"delete": testOpTimeout})
}

func TestCleanupListingErrorStopsBeforeDelete(t *testing.T) {
	f := newFakeCleaner(fiveOrders...)
	listErr := errors.New("listing keystone_catalog_entry orders of run run0: boom")
	f.failWith("list:keystone_catalog_entry", listErr)
	deleted, err := runCleanup(f, orders(fiveOrders...))
	if !errors.Is(err, listErr) {
		t.Fatalf("Cleanup() = %v, want the listing error", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
	if e := f.snapshot(); countPrefix(e, "list:") != len(e) {
		t.Errorf("calls = %v, want only listings", e)
	}
}

// TestCleanupLookupFailureStillDeletes confirms a Secret that cannot be read
// leaves its credential unchecked, does not hold up the deletes, and is
// reported once the cleanup is done.
func TestCleanupLookupFailureStillDeletes(t *testing.T) {
	f := newFakeCleaner(fiveOrders...)
	f.failWith("lookup:user", apierrors.NewServiceUnavailable("the API server is restarting"))
	deleted, err := runCleanup(f, nil)
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("Cleanup() = %v, want the lookup's 503", err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want 5", deleted)
	}
	e := f.snapshot()
	index(t, e, "reject:appcred:keystone_application_credential")
	noneWithPrefix(t, e, "waitsecretgone:user", "reject:user")
}

// TestCleanupDeletesAKindInParallel confirms the Delete calls of one kind run
// in parallel up to concurrency, and that its waits for the orders to be gone
// run in parallel whatever the concurrency.
func TestCleanupDeletesAKindInParallel(t *testing.T) {
	twoRoles := func() *fakeCleaner {
		f := newFakeCleaner("user", "project")
		f.add(orderOf(c5c3.KindRoleAssignment, "role"))
		f.add(orderOf(c5c3.KindRoleAssignment, "role-b"))
		return f
	}
	waitsTogether := func(f *fakeCleaner) {
		f.on("waitgone:role", func(ctx context.Context) error {
			return f.waitFor(ctx, hasEvent("waitgone:role-b"))
		})
	}

	t.Run("concurrency 2", func(t *testing.T) {
		f := twoRoles()
		f.on("delete:role", func(ctx context.Context) error {
			return f.waitFor(ctx, hasEvent("delete:role-b"))
		})
		waitsTogether(f)
		deleted, err := Cleanup(context.Background(), f, cleanupRun, nil, 2, testOpTimeout, testReadyTimeout, testGoneTimeout)
		if err != nil || deleted != 4 {
			t.Fatalf("Cleanup() = %d, %v, want 4, nil", deleted, err)
		}
	})

	t.Run("concurrency 1", func(t *testing.T) {
		f := twoRoles()
		f.on("delete:role", func(ctx context.Context) error {
			quiet, stop := context.WithTimeout(ctx, 100*time.Millisecond)
			defer stop()
			if err := f.waitFor(quiet, hasEvent("delete:role-b")); err == nil {
				f.mark("parallel:delete:role-b")
			}
			return nil
		})
		waitsTogether(f)
		deleted, err := Cleanup(context.Background(), f, cleanupRun, nil, 1, testOpTimeout, testReadyTimeout, testGoneTimeout)
		if err != nil || deleted != 4 {
			t.Fatalf("Cleanup() = %d, %v, want 4, nil", deleted, err)
		}
		noneWithPrefix(t, f.snapshot(), "parallel:")
	})
}

func TestCleanupCancelled(t *testing.T) {
	f := newFakeCleaner(fiveOrders...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.on("waitgone:appcred", func(waitCtx context.Context) error {
		cancel()
		<-waitCtx.Done()
		return waitCtx.Err()
	})
	deleted, err := Cleanup(ctx, f, cleanupRun, nil, 2, testOpTimeout, testReadyTimeout, testGoneTimeout)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Cleanup() = %v, want context.Canceled", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
	noneWithPrefix(t, f.snapshot(), "delete:role", "observe:", "waitsecretgone:", "reject:")
}

// TestCleanupBoundsEachCall confirms each call runs under the bound boundary 8
// names for it: a request under opTimeout, the wait for a user's generation
// under readyTimeout and a wait for an order to be gone under goneTimeout.
// The status read and Observe, which only an unsettled generation and an
// order that stays reach, are checked in their tests.
func TestCleanupBoundsEachCall(t *testing.T) {
	f := newFakeCleaner(fiveOrders...)
	if _, err := runCleanup(f, nil); err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}
	bounds := map[string]time.Duration{
		"list":           testOpTimeout,
		"lookup":         testOpTimeout,
		"waitsettled":    testReadyTimeout,
		"delete":         testOpTimeout,
		"waitgone":       testGoneTimeout,
		"waitsecretgone": testOpTimeout,
		"reject":         testOpTimeout,
	}
	checkBounds(t, f, bounds)
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.deadlines {
		if verb, _, _ := strings.Cut(key, ":"); bounds[verb] == 0 {
			t.Errorf("call %s has no expected bound", key)
		}
	}
}

// TestCleanupKeepsSecretsOutOfLogsAndErrors runs failing cleanups that log and
// return errors about credentials, and greps every log line and error text for
// the passwords and the credential secrets the fake delivers.
func TestCleanupKeepsSecretsOutOfLogsAndErrors(t *testing.T) {
	logs := captureLogs(t)
	var texts []string

	for _, tt := range []struct {
		name    string
		setup   func(*fakeCleaner)
		timeout time.Duration
	}{
		{"both credentials accepted", func(f *fakeCleaner) {
			f.order("appcred").accepted = true
			f.order("user").accepted = true
		}, testOpTimeout},
		{"secrets still present", func(f *fakeCleaner) {
			f.order("appcred").secretStays = true
			f.order("user").secretStays = true
		}, 50 * time.Millisecond},
		{"held order", func(f *fakeCleaner) { f.order("appcred").stuck = true }, 50 * time.Millisecond},
		{"unreadable secret", func(f *fakeCleaner) {
			f.failWith("lookup:user", apierrors.NewServiceUnavailable("the API server is restarting"))
		}, testOpTimeout},
	} {
		f := newFakeCleaner(fiveOrders...)
		tt.setup(f)
		_, err := Cleanup(context.Background(), f, cleanupRun, nil, 2, tt.timeout, testReadyTimeout, tt.timeout)
		if err == nil {
			t.Errorf("%s: Cleanup() = nil, want an error", tt.name)
			continue
		}
		texts = append(texts, err.Error())
	}

	out := logs.String() + strings.Join(texts, "\n")
	if !strings.Contains(out, "deleting resource") {
		t.Fatalf("captured no log line:\n%s", out)
	}
	for _, secret := range []string{userPasswordPrefix, appcredSecretPrefix} {
		if strings.Contains(out, secret) {
			t.Errorf("a log line or error text carries a %s value:\n%s", secret, out)
		}
	}
}
