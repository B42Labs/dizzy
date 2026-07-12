package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/nova"
	"github.com/B42Labs/dizzy/internal/resource"
)

// notFound is a gophercloud 404, the error a second delete of an already-gone
// resource returns so cleanup treats it as an idempotent no-op.
var notFound = gophercloud.ErrUnexpectedResponseCode{Actual: 404}

// fakeCleaner is an in-memory Cleaner: it holds the resources discoverable by
// each identity path and deletes from a shared live set, so a second Cleanup is
// a no-op. deleteEvents records the delete order for the ordering assertion.
type fakeCleaner struct {
	servers  []resource.Resource
	volumes  []resource.Resource
	ports    []resource.Resource
	networks []resource.Resource
	subnets  []resource.Resource

	live         map[string]bool
	deleteEvents []string
	failDeleteID string // id whose delete returns a non-404 error
}

func newFakeCleaner(servers, volumes, ports, networks, subnets []resource.Resource) *fakeCleaner {
	f := &fakeCleaner{servers: servers, volumes: volumes, ports: ports, networks: networks, subnets: subnets, live: map[string]bool{}}
	for _, list := range [][]resource.Resource{servers, volumes, ports, networks, subnets} {
		for _, r := range list {
			f.live[r.ID] = true
		}
	}
	return f
}

func (f *fakeCleaner) ListServersByMetadata(_ context.Context, _ string) ([]resource.Resource, error) {
	return f.stillLive(f.servers), nil
}
func (f *fakeCleaner) ListVolumesByMetadata(_ context.Context, _ string) ([]resource.Resource, error) {
	return f.stillLive(f.volumes), nil
}
func (f *fakeCleaner) ListByTag(_ context.Context, kind resource.Kind, _ string) ([]resource.Resource, error) {
	switch kind {
	case nova.KindPort:
		return f.stillLive(f.ports), nil
	case nova.KindNetwork:
		return f.stillLive(f.networks), nil
	case nova.KindSubnet:
		return f.stillLive(f.subnets), nil
	default:
		return nil, nil
	}
}
func (f *fakeCleaner) DeleteNetworkPorts(_ context.Context, _ string) (int, error) { return 0, nil }

func (f *fakeCleaner) Delete(_ context.Context, r resource.Resource) error {
	if r.ID == f.failDeleteID {
		return errors.New("boom")
	}
	if !f.live[r.ID] {
		return notFound
	}
	f.live[r.ID] = false
	f.deleteEvents = append(f.deleteEvents, string(r.Kind)+":"+r.ID)
	return nil
}

func (f *fakeCleaner) WaitForGone(context.Context, resource.Resource) error { return nil }

func (f *fakeCleaner) stillLive(list []resource.Resource) []resource.Resource {
	var out []resource.Resource
	for _, r := range list {
		if f.live[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

func r(kind resource.Kind, id string) resource.Resource {
	return resource.Resource{Kind: kind, ID: id}
}

func TestCleanupOrderAndCount(t *testing.T) {
	f := newFakeCleaner(
		[]resource.Resource{r(nova.KindServer, "s1")},
		[]resource.Resource{r(nova.KindVolume, "v1")},
		[]resource.Resource{r(nova.KindPort, "p1")},
		[]resource.Resource{r(nova.KindNetwork, "n1")},
		[]resource.Resource{r(nova.KindSubnet, "sub1")},
	)
	deleted, err := Cleanup(context.Background(), f, "run0", nil, time.Second)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want 5", deleted)
	}
	want := []string{"server:s1", "port:p1", "volume:v1", "network:n1", "subnet:sub1"}
	if len(f.deleteEvents) != len(want) {
		t.Fatalf("delete events = %v, want %v", f.deleteEvents, want)
	}
	for i := range want {
		if f.deleteEvents[i] != want[i] {
			t.Errorf("delete order[%d] = %q, want %q (full: %v)", i, f.deleteEvents[i], want[i], f.deleteEvents)
		}
	}
}

func TestCleanupIsIdempotent(t *testing.T) {
	f := newFakeCleaner(
		[]resource.Resource{r(nova.KindServer, "s1")},
		nil, nil,
		[]resource.Resource{r(nova.KindNetwork, "n1")},
		nil,
	)
	if _, err := Cleanup(context.Background(), f, "run0", nil, time.Second); err != nil {
		t.Fatalf("first Cleanup() = %v", err)
	}
	deleted, err := Cleanup(context.Background(), f, "run0", nil, time.Second)
	if err != nil {
		t.Fatalf("second Cleanup() = %v", err)
	}
	if deleted != 0 {
		t.Errorf("second Cleanup deleted = %d, want 0", deleted)
	}
}

func TestCleanupUnionsRecord(t *testing.T) {
	// Discovery misses the volume; the run record supplies it.
	f := newFakeCleaner(nil, nil, nil, nil, nil)
	f.live["v-rec"] = true
	recorded := []resource.Resource{r(nova.KindVolume, "v-rec")}
	deleted, err := Cleanup(context.Background(), f, "run0", recorded, time.Second)
	if err != nil {
		t.Fatalf("Cleanup() = %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (from the record)", deleted)
	}
}

func TestCleanupRefusesEmptyRunID(t *testing.T) {
	f := newFakeCleaner(nil, nil, nil, nil, nil)
	if _, err := Cleanup(context.Background(), f, "", nil, time.Second); err == nil {
		t.Fatal("Cleanup with an empty run id: expected an error, got nil")
	}
}

func TestCleanupStopsOnFirstError(t *testing.T) {
	f := newFakeCleaner(
		[]resource.Resource{r(nova.KindServer, "s1")},
		nil,
		[]resource.Resource{r(nova.KindPort, "p1")},
		nil, nil,
	)
	f.failDeleteID = "p1"
	deleted, err := Cleanup(context.Background(), f, "run0", nil, time.Second)
	if err == nil {
		t.Fatal("Cleanup() = nil, want the port delete error")
	}
	// The server was deleted before the failing port.
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (the server, before the port failed)", deleted)
	}
}
