package executor

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/openstack-tester/internal/neutron"
)

// fakeCleaner is an in-process Cleaner that serves tagged resources per kind,
// records detach and delete events in call order, and can fail a specific
// delete. It lets Cleanup's ordering, idempotency, and 404 handling be exercised
// without a cloud, mirroring the fakeNeutron pattern.
type fakeCleaner struct {
	byKind       map[neutron.Kind][]neutron.Resource
	gone         map[string]bool  // ids already deleted (a re-delete 404s)
	failDelete   map[string]error // id -> error Delete returns instead of succeeding
	networkPorts map[string]int   // network id -> plain (orphan) ports still on it
	events       []string         // "detach:<id>" / "sweep:<netID>" / "delete:<kind>" in call order
}

func newFakeCleaner() *fakeCleaner {
	return &fakeCleaner{
		byKind:       make(map[neutron.Kind][]neutron.Resource),
		gone:         make(map[string]bool),
		failDelete:   make(map[string]error),
		networkPorts: make(map[string]int),
	}
}

func (f *fakeCleaner) ListByTag(ctx context.Context, kind neutron.Kind, runID string) ([]neutron.Resource, error) {
	var out []neutron.Resource
	for _, r := range f.byKind[kind] {
		if !f.gone[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeCleaner) DetachRouterInterfaces(ctx context.Context, routerID string) (int, error) {
	f.events = append(f.events, "detach:"+routerID)
	return 1, nil
}

func (f *fakeCleaner) DeleteNetworkPorts(ctx context.Context, networkID string) (int, error) {
	f.events = append(f.events, "sweep:"+networkID)
	swept := f.networkPorts[networkID]
	f.networkPorts[networkID] = 0
	return swept, nil
}

func (f *fakeCleaner) Delete(ctx context.Context, r neutron.Resource) error {
	if err := f.failDelete[r.ID]; err != nil {
		return err
	}
	if f.gone[r.ID] {
		return gophercloud.ErrUnexpectedResponseCode{Actual: 404}
	}
	f.gone[r.ID] = true
	f.events = append(f.events, "delete:"+string(r.Kind))
	return nil
}

// seedFullTopology stocks one resource of every cleanup-relevant kind.
func seedFullTopology() *fakeCleaner {
	f := newFakeCleaner()
	f.byKind[neutron.KindPort] = []neutron.Resource{{Kind: neutron.KindPort, ID: "p1"}}
	f.byKind[neutron.KindRouter] = []neutron.Resource{{Kind: neutron.KindRouter, ID: "r1"}}
	f.byKind[neutron.KindSubnet] = []neutron.Resource{{Kind: neutron.KindSubnet, ID: "s1"}}
	f.byKind[neutron.KindSecurityGroup] = []neutron.Resource{{Kind: neutron.KindSecurityGroup, ID: "g1"}}
	f.byKind[neutron.KindNetwork] = []neutron.Resource{{Kind: neutron.KindNetwork, ID: "n1"}}
	f.byKind[neutron.KindSubnetPool] = []neutron.Resource{{Kind: neutron.KindSubnetPool, ID: "sp1"}}
	return f
}

// idx returns the position of event in the log, or -1 if it never occurred.
func idx(events []string, event string) int {
	return slices.Index(events, event)
}

// TestCleanupLogsEachDelete confirms the teardown announces every resource it
// removes, so an operator watching cleanup run sees what it is deleting rather
// than only the final count. It captures the info-level logs (the package's
// TestMain discards them by default) and checks a line per deleted id.
func TestCleanupLogsEachDelete(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(old)

	f := seedFullTopology()
	if _, err := Cleanup(context.Background(), f, "run0", nil); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	logged := buf.String()
	for _, id := range []string{"p1", "r1", "s1", "g1", "n1", "sp1"} {
		if !strings.Contains(logged, "id="+id) {
			t.Errorf("no delete log line for id %q; log=%q", id, logged)
		}
	}
	if got := strings.Count(logged, "deleting resource"); got != 6 {
		t.Errorf("logged %d delete lines, want 6; log=%q", got, logged)
	}
}

// TestCleanupReverseDependencyOrder confirms the teardown order: our own ports
// and detached interfaces precede the network delete (which cascades the subnet
// service ports), networks precede subnets so that cascade can clear the DHCP
// port that would otherwise block an explicit subnet delete, and subnets precede
// the subnet pools they allocate from.
func TestCleanupReverseDependencyOrder(t *testing.T) {
	f := seedFullTopology()
	deleted, err := Cleanup(context.Background(), f, "run0", nil)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if deleted != 6 {
		t.Errorf("deleted %d resources, want 6", deleted)
	}

	port := idx(f.events, "delete:port")
	detach := idx(f.events, "detach:r1")
	subnet := idx(f.events, "delete:subnet")
	router := idx(f.events, "delete:router")
	network := idx(f.events, "delete:network")
	pool := idx(f.events, "delete:subnet-pool")

	for _, e := range []string{"delete:port", "detach:r1", "delete:subnet", "delete:router", "delete:network", "delete:subnet-pool"} {
		if idx(f.events, e) < 0 {
			t.Fatalf("event %q never happened; log=%v", e, f.events)
		}
	}
	if port >= network {
		t.Errorf("our ports must be deleted before their networks (a network delete fails while user ports remain); log=%v", f.events)
	}
	if detach >= network || detach >= router {
		t.Errorf("interfaces must be detached before networks and routers are deleted; log=%v", f.events)
	}
	if network >= subnet {
		t.Errorf("networks must be deleted before subnets so the cascade removes the DHCP ports; log=%v", f.events)
	}
	if subnet >= pool {
		t.Errorf("subnets must be deleted before the subnet pools they allocate from; log=%v", f.events)
	}
}

// TestCleanupSweepsOrphanNetworkPorts confirms that plain ports left on a tagged
// network — untagged orphans a cancelled run can leave behind, which tag-based
// discovery misses and which block the network delete — are swept (and counted)
// before the network is deleted.
func TestCleanupSweepsOrphanNetworkPorts(t *testing.T) {
	f := seedFullTopology()
	f.networkPorts["n1"] = 2

	deleted, err := Cleanup(context.Background(), f, "run0", nil)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if deleted != 8 {
		t.Errorf("deleted %d resources, want 8 (6 tagged + 2 swept orphan ports)", deleted)
	}
	sweep := idx(f.events, "sweep:n1")
	network := idx(f.events, "delete:network")
	if sweep < 0 || network < 0 || sweep >= network {
		t.Errorf("orphan ports must be swept before the network is deleted; log=%v", f.events)
	}
}

// TestCleanupOnlyTaggedResources confirms detach targets only the tagged router
// and nothing outside the seeded (tagged) set is touched.
func TestCleanupOnlyTaggedResources(t *testing.T) {
	f := seedFullTopology()
	if _, err := Cleanup(context.Background(), f, "run0", nil); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	var detaches []string
	for _, e := range f.events {
		if strings.HasPrefix(e, "detach:") {
			detaches = append(detaches, e)
		}
	}
	if len(detaches) != 1 || detaches[0] != "detach:r1" {
		t.Errorf("detached %v, want exactly [detach:r1]", detaches)
	}
}

// TestCleanupIdempotent covers the "running cleanup twice is a no-op" acceptance
// criterion: the second sweep finds every resource gone and deletes nothing.
func TestCleanupIdempotent(t *testing.T) {
	f := seedFullTopology()

	first, err := Cleanup(context.Background(), f, "run0", nil)
	if err != nil {
		t.Fatalf("first Cleanup: %v", err)
	}
	if first == 0 {
		t.Fatal("first Cleanup deleted nothing; expected the seeded resources")
	}

	second, err := Cleanup(context.Background(), f, "run0", nil)
	if err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
	if second != 0 {
		t.Errorf("second Cleanup deleted %d resources, want 0 (a no-op)", second)
	}
}

// TestCleanupIgnoresNotFound confirms a 404 on delete (a resource removed out of
// band) is treated as success rather than failing the sweep.
func TestCleanupIgnoresNotFound(t *testing.T) {
	f := seedFullTopology()
	f.failDelete["n1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 404}

	deleted, err := Cleanup(context.Background(), f, "run0", nil)
	if err != nil {
		t.Fatalf("Cleanup must ignore a 404, got %v", err)
	}
	// Five real deletes plus the network that 404'd (not counted).
	if deleted != 5 {
		t.Errorf("deleted %d resources, want 5 (the 404 network is not counted)", deleted)
	}
	if slices.Contains(f.events, "delete:network") {
		t.Error("the 404 network must not be recorded as deleted")
	}
}

// TestCleanupPropagatesError confirms a non-404 delete error stops the sweep and
// is returned with the count deleted so far.
func TestCleanupPropagatesError(t *testing.T) {
	f := seedFullTopology()
	f.failDelete["p1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 500}

	if _, err := Cleanup(context.Background(), f, "run0", nil); err == nil {
		t.Fatal("expected the 500 delete error to propagate")
	}
}

// TestCleanupReclaimsRecordedAddressScopes confirms an address scope — which
// cannot be discovered by tag — is reclaimed from the run record by id, deleted
// after the subnet pools that reference it, and that a second sweep is a no-op.
func TestCleanupReclaimsRecordedAddressScopes(t *testing.T) {
	f := seedFullTopology()
	recorded := []neutron.Resource{
		{Kind: neutron.KindNetwork, ID: "n1"}, // also recorded, but found by tag
		{Kind: neutron.KindAddressScope, ID: "as1"},
	}

	deleted, err := Cleanup(context.Background(), f, "run0", recorded)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	// Six tag-discovered resources plus the recorded address scope.
	if deleted != 7 {
		t.Errorf("deleted %d resources, want 7", deleted)
	}

	pool := idx(f.events, "delete:subnet-pool")
	as := idx(f.events, "delete:address-scope")
	if as < 0 {
		t.Fatalf("address scope never deleted; log=%v", f.events)
	}
	if pool < 0 || pool >= as {
		t.Errorf("subnet pools must be deleted before address scopes; log=%v", f.events)
	}

	second, err := Cleanup(context.Background(), f, "run0", recorded)
	if err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
	if second != 0 {
		t.Errorf("second Cleanup deleted %d resources, want 0 (a no-op)", second)
	}
}

// TestCleanupWithoutRecordSkipsAddressScopes confirms that without a run record
// (recorded is nil, the --run-id path) address scopes are left untouched: they
// cannot be discovered by tag, so there is nothing to reclaim.
func TestCleanupWithoutRecordSkipsAddressScopes(t *testing.T) {
	f := seedFullTopology()

	deleted, err := Cleanup(context.Background(), f, "run0", nil)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if deleted != 6 {
		t.Errorf("deleted %d resources, want 6 (no address scope without a record)", deleted)
	}
	if slices.Contains(f.events, "delete:address-scope") {
		t.Error("address scope must not be deleted without a run record")
	}
}
