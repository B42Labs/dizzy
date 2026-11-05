package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/barbican"
	"github.com/B42Labs/dizzy/internal/resource"
)

// notFound is a gophercloud 404, the error a second delete of an already-gone
// resource returns so cleanup treats it as an idempotent no-op.
var notFound = gophercloud.ErrUnexpectedResponseCode{Actual: 404}

// fakeCleaner is an in-memory Cleaner: each listing returns the live resources
// of its kind that discovery can see, and deletes remove from a shared live
// set, so a second Cleanup is a no-op. The order listing also returns the
// generated secret linked to each live order it reports. Each call is recorded
// as one event, list:<kind>/<run id> or delete:<kind>/<id>, so tests can assert
// on the order in which kinds are torn down. Cleanup deletes concurrently, so
// mu guards the shared state.
type fakeCleaner struct {
	mu        sync.Mutex
	events    []string
	discover  []resource.Resource          // resources the listings can see while they are live
	linked    map[string]resource.Resource // order id -> the generated secret its secret_ref names
	live      map[string]bool
	deleted   []string
	listErr   map[resource.Kind]error
	deleteErr map[string]error // id -> the non-404 error its delete returns
}

// newFakeCleaner returns a fake holding rs as live and discoverable.
func newFakeCleaner(rs ...resource.Resource) *fakeCleaner {
	f := &fakeCleaner{
		discover:  rs,
		linked:    map[string]resource.Resource{},
		live:      map[string]bool{},
		listErr:   map[resource.Kind]error{},
		deleteErr: map[string]error{},
	}
	for _, r := range rs {
		f.live[r.ID] = true
	}
	return f
}

func (f *fakeCleaner) list(kind resource.Kind, runID string) ([]resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "list:"+string(kind)+"/"+runID)
	if err := f.listErr[kind]; err != nil {
		return nil, err
	}
	var out []resource.Resource
	for _, r := range f.discover {
		if r.Kind == kind && f.live[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeCleaner) ListContainersByRun(_ context.Context, runID string) ([]resource.Resource, error) {
	return f.list(barbican.KindContainer, runID)
}

func (f *fakeCleaner) ListOrdersByRun(_ context.Context, runID string) (orders, secrets []resource.Resource, err error) {
	orders, err = f.list(barbican.KindOrder, runID)
	if err != nil {
		return nil, nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range orders {
		if s, ok := f.linked[o.ID]; ok {
			secrets = append(secrets, s)
		}
	}
	return orders, secrets, nil
}

// linkSecret makes s the generated secret of order, live but neither
// discoverable by metadata nor recorded unless the test adds it there.
func (f *fakeCleaner) linkSecret(order string, s resource.Resource) {
	f.linked[order] = s
	f.live[s.ID] = true
}

func (f *fakeCleaner) ListSecretsByRun(_ context.Context, runID string) ([]resource.Resource, error) {
	return f.list(barbican.KindSecret, runID)
}

func (f *fakeCleaner) Delete(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "delete:"+string(r.Kind)+"/"+r.ID)
	if err := f.deleteErr[r.ID]; err != nil {
		return err
	}
	if !f.live[r.ID] {
		return notFound
	}
	f.live[r.ID] = false
	f.deleted = append(f.deleted, r.ID)
	return nil
}

func (f *fakeCleaner) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func ctr(id string) resource.Resource { return resource.Resource{Kind: barbican.KindContainer, ID: id} }
func ord(id string) resource.Resource { return resource.Resource{Kind: barbican.KindOrder, ID: id} }
func sec(id string) resource.Resource { return resource.Resource{Kind: barbican.KindSecret, ID: id} }

func TestCleanupDeletesKindsInDependencyOrder(t *testing.T) {
	f := newFakeCleaner(sec("s1"), ord("o1"), ctr("c1"), sec("s2"), ord("o2"), ctr("c2"), sec("s3"))
	deleted, err := Cleanup(context.Background(), f, "run0", nil, 4)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}
	if deleted != 7 {
		t.Errorf("deleted = %d, want 7", deleted)
	}

	// Each step's events must all come after every event of the step before it:
	// every container is deleted before the order listing, every order before the
	// secret listing.
	steps := []string{"list:container/run0", "delete:container/", "list:order/run0", "delete:order/", "list:secret/run0", "delete:secret/"}
	e := f.snapshot()
	prev := 0
	for _, ev := range e {
		step := -1
		for i, prefix := range steps {
			if strings.HasPrefix(ev, prefix) {
				step = i
				break
			}
		}
		if step < 0 {
			t.Fatalf("unexpected event %q: %v", ev, e)
		}
		if step < prev {
			t.Errorf("event %q (step %q) came after step %q: %v", ev, steps[step], steps[prev], e)
		}
		prev = step
	}
	if prev != len(steps)-1 {
		t.Errorf("cleanup ended at step %q, want it to reach %q: %v", steps[prev], steps[len(steps)-1], e)
	}
}

func TestCleanupUnionsRecord(t *testing.T) {
	// Discovery finds c1 and s1. The record holds s1 again, an order discovery
	// missed, and a stored secret whose stamp never landed, which secret
	// discovery cannot confirm and only the record reaches.
	f := newFakeCleaner(ctr("c1"), sec("s1"))
	f.live["o-rec"] = true
	f.live["s-unstamped"] = true
	recorded := []resource.Resource{ctr("c1"), sec("s1"), ord("o-rec"), sec("s-unstamped")}

	deleted, err := Cleanup(context.Background(), f, "run0", recorded, 4)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}
	if deleted != 4 {
		t.Errorf("deleted = %d, want 4", deleted)
	}
	e := f.snapshot()
	for _, r := range recorded {
		ev := "delete:" + string(r.Kind) + "/" + r.ID
		if n := count(e, ev); n != 1 {
			t.Errorf("%s issued %d times, want once: %v", ev, n, e)
		}
	}
}

// TestCleanupDeletesOrderLinkedSecret confirms a generated secret reached only
// through its order's secret_ref, carrying no run metadata and absent from the
// record (its order turned ACTIVE after the wait gave up), is deleted once,
// after every container and before any order, so its order stays a handle on it
// until it is gone.
func TestCleanupDeletesOrderLinkedSecret(t *testing.T) {
	f := newFakeCleaner(ctr("c1"), ord("o1"), ord("o2"))
	f.linkSecret("o1", sec("g1"))
	f.linkSecret("o2", sec("g2"))

	deleted, err := Cleanup(context.Background(), f, "run0", nil, 4)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want 5 (a container, two orders, and their generated secrets)", deleted)
	}

	e := f.snapshot()
	lastContainer, firstOrder := indexOf(e, "delete:container/c1"), indexOfPrefix(e, "delete:order/")
	if lastContainer < 0 || firstOrder < 0 {
		t.Fatalf("container or order deletes missing: %v", e)
	}
	for _, id := range []string{"g1", "g2"} {
		ev := "delete:secret/" + id
		if n := count(e, ev); n != 1 {
			t.Errorf("%s issued %d times, want once: %v", ev, n, e)
			continue
		}
		if i := indexOf(e, ev); i < lastContainer || i > firstOrder {
			t.Errorf("%s at %d is not between the container delete (%d) and the first order delete (%d): %v", ev, i, lastContainer, firstOrder, e)
		}
	}
}

// TestCleanupFailedSecretListingKeepsNoLinkedSecret confirms an order-linked
// secret is already gone when the later secret listing fails: the orders phase
// deletes it before its order, so the order is never removed while it is the
// only handle on the secret.
func TestCleanupFailedSecretListingKeepsNoLinkedSecret(t *testing.T) {
	f := newFakeCleaner(ord("o1"))
	f.linkSecret("o1", sec("g1"))
	listErr := errors.New("listing secrets by run: boom")
	f.listErr[barbican.KindSecret] = listErr

	deleted, err := Cleanup(context.Background(), f, "run0", []resource.Resource{ord("o1")}, 4)
	if !errors.Is(err, listErr) {
		t.Fatalf("Cleanup() = %v, want the secret listing error", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (the order and its generated secret)", deleted)
	}
	if f.live["g1"] {
		t.Errorf("generated secret g1 survived a cleanup whose secret listing failed: %v", f.snapshot())
	}
}

// TestCleanupLinkedSecretDeleteErrorKeepsOrder confirms a failed delete of an
// order's generated secret stops the cleanup before the order is deleted, so a
// rerun still finds the secret through its order.
func TestCleanupLinkedSecretDeleteErrorKeepsOrder(t *testing.T) {
	f := newFakeCleaner(ord("o1"))
	f.linkSecret("o1", sec("g1"))
	f.deleteErr["g1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 500}

	_, err := Cleanup(context.Background(), f, "run0", nil, 4)
	if !gophercloud.ResponseCodeIs(err, 500) {
		t.Fatalf("Cleanup() = %v, want the generated secret delete's 500", err)
	}
	if !strings.HasPrefix(err.Error(), "deleting secret g1: ") {
		t.Errorf("error = %q, want it to name the generated secret delete", err)
	}
	if !f.live["o1"] {
		t.Errorf("order o1 was deleted although its generated secret's delete failed: %v", f.snapshot())
	}
	delete(f.deleteErr, "g1")
	if _, err := Cleanup(context.Background(), f, "run0", nil, 4); err != nil {
		t.Fatalf("rerun Cleanup() = %v, want nil", err)
	}
	if f.live["g1"] || f.live["o1"] {
		t.Errorf("rerun left g1 live=%v, o1 live=%v; want both deleted: %v", f.live["g1"], f.live["o1"], f.snapshot())
	}
}

// TestCleanupDeletesLinkedAndStampedSecretOnce confirms a generated secret found
// both as its order's secret_ref and by its dizzy_run metadata is deleted only
// once.
func TestCleanupDeletesLinkedAndStampedSecretOnce(t *testing.T) {
	f := newFakeCleaner(ord("o1"), sec("g1"))
	f.linkSecret("o1", sec("g1"))

	deleted, err := Cleanup(context.Background(), f, "run0", nil, 4)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	if n := count(f.snapshot(), "delete:secret/g1"); n != 1 {
		t.Errorf("delete:secret/g1 issued %d times, want once: %v", n, f.snapshot())
	}
}

func TestCleanupIsIdempotent(t *testing.T) {
	rs := []resource.Resource{ctr("c1"), ord("o1"), sec("s1")}
	f := newFakeCleaner(rs...)
	if _, err := Cleanup(context.Background(), f, "run0", rs, 4); err != nil {
		t.Fatalf("first Cleanup() = %v", err)
	}
	before := len(f.snapshot())

	// The second sweep reaches every id again through the record, and each
	// delete answers 404.
	deleted, err := Cleanup(context.Background(), f, "run0", rs, 4)
	if err != nil {
		t.Fatalf("second Cleanup() = %v, want nil", err)
	}
	if deleted != 0 {
		t.Errorf("second Cleanup deleted = %d, want 0", deleted)
	}
	var deletes int
	for _, ev := range f.snapshot()[before:] {
		if strings.HasPrefix(ev, "delete:") {
			deletes++
		}
	}
	if deletes != len(rs) {
		t.Errorf("second Cleanup issued %d deletes, want %d answered with 404", deletes, len(rs))
	}
}

func TestCleanupRefusesEmptyRunID(t *testing.T) {
	f := newFakeCleaner(ctr("c1"), ord("o1"), sec("s1"))
	_, err := Cleanup(context.Background(), f, "", []resource.Resource{sec("s1")}, 4)
	const want = "cleanup: empty run id; refusing to delete by empty identity"
	if err == nil || err.Error() != want {
		t.Fatalf("Cleanup with an empty run id = %v, want %q", err, want)
	}
	if e := f.snapshot(); len(e) != 0 {
		t.Errorf("Cleanup with an empty run id made %d seam calls: %v", len(e), e)
	}
}

func TestCleanupOrderDeleteErrorSparesSecrets(t *testing.T) {
	f := newFakeCleaner(ctr("c1"), ord("o1"), ord("o2"), sec("s1"))
	f.deleteErr["o1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 500}
	deleted, err := Cleanup(context.Background(), f, "run0", []resource.Resource{sec("s1")}, 4)
	if !gophercloud.ResponseCodeIs(err, 500) {
		t.Fatalf("Cleanup() = %v, want the order delete's 500", err)
	}
	if !strings.HasPrefix(err.Error(), "deleting order o1: ") {
		t.Errorf("error = %q, want it to name the order delete", err)
	}
	e := f.snapshot()
	if i := indexOfPrefix(e, "list:secret/"); i >= 0 {
		t.Errorf("secrets were listed after an order delete failed: %v", e)
	}
	if i := indexOfPrefix(e, "delete:secret/"); i >= 0 {
		t.Errorf("a secret was deleted after an order delete failed: %v", e)
	}
	// The failing delete is never counted; the concurrent o2 may or may not be
	// reached before the stage is cancelled, but the count always matches what
	// was actually removed.
	if deleted != len(f.deleted) {
		t.Errorf("deleted = %d, want %d (the resources actually removed)", deleted, len(f.deleted))
	}
}

func TestCleanupListingErrorStopsBeforeDelete(t *testing.T) {
	f := newFakeCleaner(ctr("c1"), ord("o1"), sec("s1"))
	listErr := errors.New("listing containers by run: boom")
	f.listErr[barbican.KindContainer] = listErr
	deleted, err := Cleanup(context.Background(), f, "run0", []resource.Resource{ctr("c1"), sec("s1")}, 4)
	if !errors.Is(err, listErr) {
		t.Fatalf("Cleanup() = %v, want the listing error", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
	if e := f.snapshot(); len(e) != 1 || e[0] != "list:container/run0" {
		t.Errorf("events = %v, want only the failed container listing", e)
	}
}
