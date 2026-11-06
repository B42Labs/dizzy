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

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/octavia"
	"github.com/B42Labs/dizzy/internal/resource"
)

// notFound is a gophercloud 404, the error a delete of an already-gone resource
// returns so cleanup treats it as an idempotent no-op.
var notFound = gophercloud.ErrUnexpectedResponseCode{Actual: 404}

// forever makes a resource's delete answer 409 on every attempt.
const forever = -1

// fakeCleaner is an in-memory Cleaner: it holds the resources the tag
// discovery finds per kind and deletes from a shared live set, so a second
// Cleanup is a no-op. Like the real client it refuses to delete any kind but a
// load balancer, subnet, or network. calls records every call in order; the
// load balancer deletes run concurrently, so it is guarded by mu.
type fakeCleaner struct {
	mu sync.Mutex

	listed    map[resource.Kind][]resource.Resource // what ListByTag finds while live
	listErr   map[resource.Kind]error
	live      map[string]bool
	conflicts map[string]int   // id → 409s its delete answers before it is accepted; forever for always
	deleteErr map[string]error // id → error its delete returns
	waitErr   error            // WaitForGone returns this
	onDelete  func()           // called as every delete starts

	calls []string
}

// newFakeCleaner returns a fakeCleaner whose tag discovery finds listed, each
// live until it is deleted.
func newFakeCleaner(listed ...resource.Resource) *fakeCleaner {
	f := &fakeCleaner{
		listed:    map[resource.Kind][]resource.Resource{},
		listErr:   map[resource.Kind]error{},
		live:      map[string]bool{},
		conflicts: map[string]int{},
		deleteErr: map[string]error{},
	}
	for _, r := range listed {
		f.listed[r.Kind] = append(f.listed[r.Kind], r)
		f.live[r.ID] = true
	}
	return f
}

func (f *fakeCleaner) ListByTag(_ context.Context, kind resource.Kind, runID string) ([]resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "list:"+string(kind)+":"+runID)
	if err := f.listErr[kind]; err != nil {
		return nil, err
	}
	var out []resource.Resource
	for _, r := range f.listed[kind] {
		if f.live[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeCleaner) Delete(_ context.Context, r resource.Resource) error {
	if f.onDelete != nil {
		f.onDelete()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "delete:"+string(r.Kind)+":"+r.ID)
	switch r.Kind {
	case octavia.KindLoadBalancer, octavia.KindSubnet, octavia.KindNetwork:
	default:
		return fmt.Errorf("delete not supported for kind %q", r.Kind)
	}
	if err := f.deleteErr[r.ID]; err != nil {
		return err
	}
	if n := f.conflicts[r.ID]; n != 0 {
		if n > 0 {
			f.conflicts[r.ID]--
		}
		return conflict
	}
	if !f.live[r.ID] {
		return notFound
	}
	f.live[r.ID] = false
	return nil
}

func (f *fakeCleaner) WaitForGone(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "wait:"+string(r.Kind)+":"+r.ID)
	return f.waitErr
}

func (f *fakeCleaner) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// r returns a resource of kind with id and its logical name equal to the id.
func r(kind resource.Kind, id string) resource.Resource {
	return resource.Resource{Kind: kind, Logical: id, ID: id}
}

// indexes returns the positions of the calls that start with prefix.
func indexes(calls []string, prefix string) []int {
	var out []int
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, i)
		}
	}
	return out
}

func TestCleanupRefusesEmptyRunID(t *testing.T) {
	f := newFakeCleaner(r(octavia.KindLoadBalancer, "lb1"), r(octavia.KindNetwork, "net1"))
	n, err := Cleanup(context.Background(), f, "", []resource.Resource{r(octavia.KindLoadBalancer, "lb1")}, 4, time.Second)
	if want := "cleanup: empty run id; refusing to delete by empty identity"; err == nil || err.Error() != want {
		t.Errorf("Cleanup() = %v, want %q", err, want)
	}
	if n != 0 {
		t.Errorf("deleted = %d, want 0", n)
	}
	if calls := f.snapshot(); len(calls) != 0 {
		t.Errorf("calls = %v, want none", calls)
	}
}

func TestCleanupDeletesLoadBalancersOnceThenSubnetsThenNetworks(t *testing.T) {
	f := newFakeCleaner(
		r(octavia.KindLoadBalancer, "lb1"), r(octavia.KindLoadBalancer, "lb2"),
		r(octavia.KindSubnet, "sub1"),
		r(octavia.KindNetwork, "net1"),
	)
	// The record repeats lb2, sub1, and net1 and adds what discovery missed.
	for _, id := range []string{"lb3", "sub2", "net2"} {
		f.live[id] = true
	}
	recorded := []resource.Resource{
		r(octavia.KindNetwork, "net1"), r(octavia.KindSubnet, "sub1"), r(octavia.KindLoadBalancer, "lb2"),
		r(octavia.KindNetwork, "net2"), r(octavia.KindSubnet, "sub2"), r(octavia.KindLoadBalancer, "lb3"),
	}

	deleted, err := Cleanup(context.Background(), f, "run0", recorded, 4, time.Second)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}
	if deleted != 7 {
		t.Errorf("deleted = %d, want 7 (three load balancers, two subnets, two networks)", deleted)
	}

	calls := f.snapshot()
	for _, id := range []string{"lb1", "lb2", "lb3"} {
		if n := len(indexes(calls, "delete:loadbalancer:"+id)); n != 1 {
			t.Errorf("load balancer %s deleted %d times, want once: %v", id, n, calls)
		}
		if n := len(indexes(calls, "wait:loadbalancer:"+id)); n != 1 {
			t.Errorf("load balancer %s waited on %d times, want once: %v", id, n, calls)
		}
	}
	lbs, waits := indexes(calls, "delete:loadbalancer:"), indexes(calls, "wait:loadbalancer:")
	subnets, networks := indexes(calls, "delete:subnet:"), indexes(calls, "delete:network:")
	if len(subnets) != 2 || len(networks) != 2 {
		t.Fatalf("subnet deletes %v, network deletes %v, want two each: %v", subnets, networks, calls)
	}
	if last := max(slices.Max(lbs), slices.Max(waits)); last > subnets[0] {
		t.Errorf("a load balancer delete or wait (at %d) came after the first subnet delete (at %d): %v", last, subnets[0], calls)
	}
	if slices.Max(subnets) > networks[0] {
		t.Errorf("a subnet delete came after the first network delete: %v", calls)
	}
	if want := []string{"delete:subnet:sub1", "delete:subnet:sub2", "list:network:run0", "delete:network:net1", "delete:network:net2"}; !slices.Equal(calls[len(calls)-5:], want) {
		t.Errorf("last calls = %v, want %v", calls[len(calls)-5:], want)
	}
}

func TestCleanupResendsLoadBalancerDeleteWhileConflicting(t *testing.T) {
	shrinkDelays(t)
	f := newFakeCleaner(r(octavia.KindLoadBalancer, "lb1"))
	f.conflicts["lb1"] = 2
	deleted, err := Cleanup(context.Background(), f, "run0", nil, 4, time.Second)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	calls := f.snapshot()
	if n := len(indexes(calls, "delete:loadbalancer:lb1")); n != 3 {
		t.Errorf("lb1 delete sent %d times, want 3 (two 409s, then accepted): %v", n, calls)
	}
	if w := indexes(calls, "wait:loadbalancer:lb1"); len(w) != 1 || w[0] < slices.Max(indexes(calls, "delete:loadbalancer:lb1")) {
		t.Errorf("lb1 was not waited on once after its accepted delete: %v", calls)
	}
}

func TestCleanupLoadBalancerStillPendingAtTimeout(t *testing.T) {
	shrinkDelays(t)
	f := newFakeCleaner(r(octavia.KindLoadBalancer, "lb1"), r(octavia.KindSubnet, "sub1"), r(octavia.KindNetwork, "net1"))
	f.conflicts["lb1"] = forever
	opTimeout := 30 * time.Millisecond
	deleted, err := Cleanup(context.Background(), f, "run0", nil, 4, opTimeout)
	if want := "deleting loadbalancer lb1: still in a PENDING status after 30ms: "; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("Cleanup() = %v, want an error starting with %q", err, want)
	}
	if !gophercloud.ResponseCodeIs(err, 409) {
		t.Errorf("Cleanup() = %v, want it to wrap the last 409", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
	calls := f.snapshot()
	if n := len(indexes(calls, "delete:loadbalancer:lb1")); n < 2 {
		t.Errorf("lb1 delete sent %d times, want it re-sent until the timeout", n)
	}
	if later := append(indexes(calls, "wait:"), indexes(calls, "list:subnet:")...); len(later) > 0 {
		t.Errorf("cleanup went on past the stuck load balancer: %v", calls)
	}
}

func TestCleanupNotFoundCountsNothingAndStopsNothing(t *testing.T) {
	f := newFakeCleaner(r(octavia.KindLoadBalancer, "lb1"), r(octavia.KindSubnet, "sub1"), r(octavia.KindNetwork, "net1"))
	// The record names a load balancer, subnet, and network already gone.
	recorded := []resource.Resource{
		r(octavia.KindLoadBalancer, "lb-gone"), r(octavia.KindSubnet, "sub-gone"), r(octavia.KindNetwork, "net-gone"),
	}
	deleted, err := Cleanup(context.Background(), f, "run0", recorded, 4, time.Second)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3 (the 404s count nothing)", deleted)
	}
	calls := f.snapshot()
	for _, c := range []string{"delete:loadbalancer:lb-gone", "delete:subnet:sub-gone", "delete:network:net-gone", "delete:network:net1"} {
		if !slices.Contains(calls, c) {
			t.Errorf("calls lack %q: %v", c, calls)
		}
	}
	if slices.Contains(calls, "wait:loadbalancer:lb-gone") {
		t.Errorf("a load balancer whose delete answered 404 was waited on: %v", calls)
	}
}

func TestCleanupSubnetStillInUseLeavesItsNetworkUntouched(t *testing.T) {
	shrinkDelays(t)
	f := newFakeCleaner(r(octavia.KindLoadBalancer, "lb1"), r(octavia.KindSubnet, "sub1"), r(octavia.KindNetwork, "net1"))
	f.conflicts["sub1"] = forever
	deleted, err := Cleanup(context.Background(), f, "run0", []resource.Resource{r(octavia.KindNetwork, "net1")}, 4, 30*time.Millisecond)
	want := "deleting subnet sub1: still in use after 30ms; a port dizzy did not create remains on it: "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("Cleanup() = %v, want an error starting with %q", err, want)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (the load balancer)", deleted)
	}
	calls := f.snapshot()
	if net := append(indexes(calls, "delete:network:"), indexes(calls, "list:network:")...); len(net) > 0 {
		t.Errorf("the network was touched after its subnet stayed in use: %v", calls)
	}
	if !f.live["net1"] {
		t.Error("net1 was deleted")
	}
}

func TestCleanupNeverDeletesRecordedChildren(t *testing.T) {
	f := newFakeCleaner(r(octavia.KindLoadBalancer, "lb1"))
	recorded := []resource.Resource{
		r(octavia.KindLoadBalancer, "lb1"),
		r(octavia.KindListener, "lsn1"),
		r(octavia.KindPool, "pool1"),
		r(octavia.KindMember, "pool1-member-01"),
		r(octavia.KindHealthMonitor, "hm1"),
	}
	deleted, err := Cleanup(context.Background(), f, "run0", recorded, 4, time.Second)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (the load balancer, whose cascade removes the rest)", deleted)
	}
	calls := f.snapshot()
	for _, kind := range []resource.Kind{octavia.KindListener, octavia.KindPool, octavia.KindMember, octavia.KindHealthMonitor} {
		if got := indexes(calls, "delete:"+string(kind)+":"); len(got) > 0 {
			t.Errorf("a recorded %s was deleted on its own: %v", kind, calls)
		}
	}
}

func TestCleanupListingErrorReturnsTheCountSoFar(t *testing.T) {
	f := newFakeCleaner(r(octavia.KindLoadBalancer, "lb1"), r(octavia.KindSubnet, "sub1"), r(octavia.KindNetwork, "net1"))
	listErr := errors.New("listing subnet by tag: boom")
	f.listErr[octavia.KindSubnet] = listErr
	deleted, err := Cleanup(context.Background(), f, "run0", nil, 4, time.Second)
	if !errors.Is(err, listErr) {
		t.Fatalf("Cleanup() = %v, want the listing error", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (the load balancer, before the listing failed)", deleted)
	}
	calls := f.snapshot()
	if later := append(indexes(calls, "delete:subnet:"), indexes(calls, "list:network:")...); len(later) > 0 {
		t.Errorf("cleanup went on past the listing error: %v", calls)
	}
}

func TestCleanupLoadBalancerListingErrorDeletesNothing(t *testing.T) {
	f := newFakeCleaner(r(octavia.KindLoadBalancer, "lb1"))
	listErr := errors.New("listing loadbalancer by tag: boom")
	f.listErr[octavia.KindLoadBalancer] = listErr
	deleted, err := Cleanup(context.Background(), f, "run0", []resource.Resource{r(octavia.KindLoadBalancer, "lb1")}, 4, time.Second)
	if !errors.Is(err, listErr) || deleted != 0 {
		t.Errorf("Cleanup() = %d, %v, want 0 and the listing error", deleted, err)
	}
	if calls := f.snapshot(); !slices.Equal(calls, []string{"list:loadbalancer:run0"}) {
		t.Errorf("calls = %v, want only the load balancer listing", calls)
	}
}

func TestCleanupSecondRunDeletesNothing(t *testing.T) {
	f := newFakeCleaner(r(octavia.KindLoadBalancer, "lb1"), r(octavia.KindSubnet, "sub1"), r(octavia.KindNetwork, "net1"))
	recorded := []resource.Resource{
		r(octavia.KindNetwork, "net1"), r(octavia.KindSubnet, "sub1"), r(octavia.KindLoadBalancer, "lb1"), r(octavia.KindListener, "lsn1"),
	}
	if deleted, err := Cleanup(context.Background(), f, "run0", recorded, 4, time.Second); err != nil || deleted != 3 {
		t.Fatalf("first Cleanup() = %d, %v, want 3, nil", deleted, err)
	}
	deleted, err := Cleanup(context.Background(), f, "run0", recorded, 4, time.Second)
	if err != nil {
		t.Fatalf("second Cleanup() = %v, want nil", err)
	}
	if deleted != 0 {
		t.Errorf("second Cleanup deleted = %d, want 0", deleted)
	}
}

func TestCleanupLoadBalancerDeleteErrorStopsBeforeSubnets(t *testing.T) {
	f := newFakeCleaner(r(octavia.KindLoadBalancer, "lb1"), r(octavia.KindSubnet, "sub1"))
	f.deleteErr["lb1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 500}
	deleted, err := Cleanup(context.Background(), f, "run0", nil, 4, time.Second)
	if want := "deleting loadbalancer lb1: "; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("Cleanup() = %v, want an error starting with %q", err, want)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
	if later := indexes(f.snapshot(), "list:subnet:"); len(later) > 0 {
		t.Errorf("cleanup went on to the subnets: %v", f.snapshot())
	}
}

func TestCleanupWaitForGoneFailureCountsTheAcceptedDelete(t *testing.T) {
	f := newFakeCleaner(r(octavia.KindLoadBalancer, "lb1"), r(octavia.KindSubnet, "sub1"))
	f.waitErr = context.DeadlineExceeded
	deleted, err := Cleanup(context.Background(), f, "run0", nil, 4, time.Second)
	if want := "waiting for loadbalancer lb1 to be deleted: context deadline exceeded"; err == nil || err.Error() != want {
		t.Fatalf("Cleanup() = %v, want %q", err, want)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (the delete was accepted)", deleted)
	}
	if later := indexes(f.snapshot(), "delete:subnet:"); len(later) > 0 {
		t.Errorf("a subnet was deleted while its load balancer may still hold the VIP port: %v", f.snapshot())
	}
}

func TestCleanupParentCancellationStopsTheConflictLoop(t *testing.T) {
	shrinkDelays(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFakeCleaner(r(octavia.KindNetwork, "net1"))
	f.conflicts["net1"] = forever
	var sends int
	f.onDelete = func() {
		if sends++; sends == 3 {
			cancel()
		}
	}
	_, err := Cleanup(ctx, f, "run0", nil, 4, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Cleanup() = %v, want an error wrapping context.Canceled", err)
	}
	if strings.Contains(err.Error(), "still in use") {
		t.Errorf("Cleanup() = %q, want a cancellation, not the leak message", err)
	}
}
