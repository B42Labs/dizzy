package barbicangraph

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/barbican"
	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/payload"
	"github.com/B42Labs/dizzy/internal/resource"
)

// retrieveCall is one recorded RetrievePayload call.
type retrieveCall struct {
	res  resource.Resource
	seed int64
	n    int
}

// recordingBarbican is a Barbican that records every call and returns the
// error configured for it, so a node closure's calls can be checked one by one.
// The closure tests call it from a single goroutine, so it takes no lock.
type recordingBarbican struct {
	storeErr, stampErr, containerErr, retrieveErr, deleteErr error

	calls      []string // method names, in call order
	storeSeeds []int64
	stamped    []resource.Resource
	stored     map[string]resource.Resource // the stored map CreateContainer last received
	retrieves  []retrieveCall
	deleted    []resource.Resource
}

func (f *recordingBarbican) StoreSecret(_ context.Context, s barbicanplan.Secret, seed int64) (resource.Resource, error) {
	f.calls = append(f.calls, "StoreSecret")
	f.storeSeeds = append(f.storeSeeds, seed)
	if f.storeErr != nil {
		return resource.Resource{}, f.storeErr
	}
	return resource.Resource{Kind: barbican.KindSecret, Logical: s.Name, Name: "dizzy-run0-" + s.Name, ID: "id-" + s.Name}, nil
}

func (f *recordingBarbican) StampSecret(_ context.Context, r resource.Resource) error {
	f.calls = append(f.calls, "StampSecret")
	f.stamped = append(f.stamped, r)
	return f.stampErr
}

func (f *recordingBarbican) CreateContainer(_ context.Context, ct barbicanplan.Container, stored map[string]resource.Resource) (resource.Resource, error) {
	f.calls = append(f.calls, "CreateContainer")
	f.stored = stored
	if f.containerErr != nil {
		return resource.Resource{}, f.containerErr
	}
	return resource.Resource{Kind: barbican.KindContainer, Logical: ct.Name, Name: "dizzy-run0-" + ct.Name, ID: "id-" + ct.Name}, nil
}

func (f *recordingBarbican) RetrievePayload(_ context.Context, r resource.Resource, seed int64, n int) error {
	f.calls = append(f.calls, "RetrievePayload")
	f.retrieves = append(f.retrieves, retrieveCall{res: r, seed: seed, n: n})
	return f.retrieveErr
}

func (f *recordingBarbican) Delete(_ context.Context, r resource.Resource) error {
	f.calls = append(f.calls, "Delete")
	f.deleted = append(f.deleted, r)
	return f.deleteErr
}

// respErr is a gophercloud HTTP error carrying status.
func respErr(status int) error {
	return gophercloud.ErrUnexpectedResponseCode{Actual: status}
}

func nodeByKey(t *testing.T, nodes []chaos.Node, key string) chaos.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Key == key {
			return n
		}
	}
	t.Fatalf("no node with key %q in %d nodes", key, len(nodes))
	return chaos.Node{}
}

func mustBuildRecording(t *testing.T, f *recordingBarbican) []chaos.Node {
	t.Helper()
	p := churnPlan()
	nodes, err := Build(p, f, p.Seed, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return nodes
}

// TestBuildShape confirms the graph is one node per stored secret and one per
// container, secrets first in plan order and then containers, with no node for
// the plan's order; that secrets are parentless and each container is parented
// on its secrets in reference order; and that Mutate is set exactly on the
// secrets the plan marks for read-back.
func TestBuildShape(t *testing.T) {
	p := churnPlan()
	nodes, err := Build(p, &recordingBarbican{}, p.Seed, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// 3 secrets + 2 containers = 5. The order is not a node.
	if len(nodes) != 5 {
		t.Fatalf("built %d nodes, want 5 (secrets + containers; no order node)", len(nodes))
	}

	var keys []string
	for _, n := range nodes {
		keys = append(keys, n.Key)
		if n.Kind == barbican.KindOrder {
			t.Errorf("node %q has kind order; orders are not churned", n.Key)
		}
		if n.Gate != nil {
			t.Errorf("node %q has a gate, want none", n.Key)
		}
	}
	wantKeys := []string{"sec-0001", "sec-0002", "sec-0003", "ctr-0001", "ctr-0002"}
	if !slices.Equal(keys, wantKeys) {
		t.Errorf("node keys = %v, want %v (secrets then containers, in plan order)", keys, wantKeys)
	}

	mutable := 0
	for _, s := range p.Secrets {
		n := nodeByKey(t, nodes, s.Name)
		if n.Kind != barbican.KindSecret || len(n.Parents) != 0 {
			t.Errorf("secret node %q kind=%q parents=%v, want secret and parentless", s.Name, n.Kind, n.Parents)
		}
		if (n.Mutate != nil) != s.Retrieve {
			t.Errorf("secret node %q mutable=%v, want %v (the plan's Retrieve)", s.Name, n.Mutate != nil, s.Retrieve)
		}
		if n.Mutate != nil {
			mutable++
		}
	}
	if mutable != 2 {
		t.Errorf("%d secret nodes are mutable, want the 2 marked Retrieve", mutable)
	}

	for _, ct := range p.Containers {
		n := nodeByKey(t, nodes, ct.Name)
		if n.Kind != barbican.KindContainer {
			t.Errorf("container node %q kind = %q, want %q", ct.Name, n.Kind, barbican.KindContainer)
		}
		if !slices.Equal(n.Parents, ct.Secrets) {
			t.Errorf("container node %q parents = %v, want %v", ct.Name, n.Parents, ct.Secrets)
		}
		if n.Mutate != nil {
			t.Errorf("container node %q is mutable, want no mutate", ct.Name)
		}
	}

	// Parents is a copy: changing it leaves the plan untouched.
	nodeByKey(t, nodes, "ctr-0001").Parents[0] = "changed"
	if p.Containers[0].Secrets[0] != "sec-0001" {
		t.Errorf("changing a container node's parents changed the plan: %v", p.Containers[0].Secrets)
	}
}

// TestBuildEmptyPlan confirms a plan with nothing to churn builds an empty graph
// without error.
func TestBuildEmptyPlan(t *testing.T) {
	nodes, err := Build(&barbicanplan.Plan{}, &recordingBarbican{}, 1, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("built %d nodes, want 0", len(nodes))
	}
}

// TestBuildOrdersOnlyPlan confirms orders yield no node: a plan of orders alone
// builds an empty graph without error.
func TestBuildOrdersOnlyPlan(t *testing.T) {
	p := &barbicanplan.Plan{Orders: []barbicanplan.Order{{Name: "ord-0001"}, {Name: "ord-0002"}}}
	nodes, err := Build(p, &recordingBarbican{}, 1, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("built %d nodes, want 0 (orders are not churned)", len(nodes))
	}
}

// TestBuildRejectsInvalidPlan confirms Build surfaces a plan validation error
// (here a container referencing a secret the plan does not store) rather than
// emitting a container node that could never be created.
func TestBuildRejectsInvalidPlan(t *testing.T) {
	p := churnPlan()
	p.Containers[1].Secrets = append(p.Containers[1].Secrets, "sec-9999")
	nodes, err := Build(p, &recordingBarbican{}, p.Seed, time.Minute)
	if err == nil {
		t.Fatal("Build of a plan with a dangling container reference: expected an error, got nil")
	}
	if nodes != nil {
		t.Errorf("Build returned %d nodes with the error, want nil", len(nodes))
	}
	for _, want := range []string{"invalid plan:", `references unknown secret "sec-9999"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// TestSecretCreateStoresThenStamps confirms a secret node's create stores the
// secret with the payload seed apply derives, stamps the stored resource, and
// returns it.
func TestSecretCreateStoresThenStamps(t *testing.T) {
	f := &recordingBarbican{}
	node := nodeByKey(t, mustBuildRecording(t, f), "sec-0001")

	res, err := node.Create(context.Background(), nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if want := []string{"StoreSecret", "StampSecret"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
	if want := payload.Seed(churnPlan().Seed, "sec-0001"); len(f.storeSeeds) != 1 || f.storeSeeds[0] != want {
		t.Errorf("StoreSecret seeds = %v, want [%d]", f.storeSeeds, want)
	}
	if len(f.stamped) != 1 || f.stamped[0] != res {
		t.Errorf("stamped %v, want the stored resource %v", f.stamped, res)
	}
	if res.ID != "id-sec-0001" || res.Kind != barbican.KindSecret || res.Logical != "sec-0001" {
		t.Errorf("Create returned %+v, want the stored secret", res)
	}
}

// TestSecretCreateStoreErrorSkipsStamp confirms a failed store returns the zero
// resource and the error without stamping anything.
func TestSecretCreateStoreErrorSkipsStamp(t *testing.T) {
	f := &recordingBarbican{storeErr: respErr(400)}
	node := nodeByKey(t, mustBuildRecording(t, f), "sec-0001")

	res, err := node.Create(context.Background(), nil)
	if !gophercloud.ResponseCodeIs(err, 400) {
		t.Errorf("Create error = %v, want the store's 400", err)
	}
	if res != (resource.Resource{}) {
		t.Errorf("Create returned %+v, want the zero resource", res)
	}
	if len(f.stamped) != 0 {
		t.Errorf("StampSecret was called %d time(s) after a failed store, want 0", len(f.stamped))
	}
}

// TestSecretCreateStampErrorReturnsStoredResource confirms a failed stamp still
// returns the stored resource with the error, so the engine keeps the secret in
// the run record and deletes it later.
func TestSecretCreateStampErrorReturnsStoredResource(t *testing.T) {
	f := &recordingBarbican{stampErr: respErr(400)}
	node := nodeByKey(t, mustBuildRecording(t, f), "sec-0001")

	res, err := node.Create(context.Background(), nil)
	if !gophercloud.ResponseCodeIs(err, 400) {
		t.Errorf("Create error = %v, want the stamp's 400", err)
	}
	if res.ID == "" {
		t.Errorf("Create returned %+v, want the stored resource with its id", res)
	}
}

// TestContainerCreateResolvesSecretIDs confirms a container node's create hands
// CreateContainer the engine's parent ids as stored secrets, keyed by logical
// name.
func TestContainerCreateResolvesSecretIDs(t *testing.T) {
	f := &recordingBarbican{}
	node := nodeByKey(t, mustBuildRecording(t, f), "ctr-0001")

	ids := map[string]string{"sec-0001": "id-1", "sec-0002": "id-2"}
	res, err := node.Create(context.Background(), ids)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.ID != "id-ctr-0001" {
		t.Errorf("Create returned %+v, want the created container", res)
	}
	want := map[string]resource.Resource{
		"sec-0001": {Kind: barbican.KindSecret, Logical: "sec-0001", ID: "id-1"},
		"sec-0002": {Kind: barbican.KindSecret, Logical: "sec-0002", ID: "id-2"},
	}
	if !reflect.DeepEqual(f.stored, want) {
		t.Errorf("CreateContainer stored = %v, want %v", f.stored, want)
	}
}

// TestDeleteFoldsNotFound confirms a node's delete treats an already-gone (404)
// resource as success and returns any other error, for a secret and a
// container alike.
func TestDeleteFoldsNotFound(t *testing.T) {
	for _, key := range []string{"sec-0001", "ctr-0001"} {
		t.Run(key, func(t *testing.T) {
			res := resource.Resource{Logical: key, ID: "id-" + key}

			gone := &recordingBarbican{deleteErr: respErr(404)}
			if err := nodeByKey(t, mustBuildRecording(t, gone), key).Delete(context.Background(), nil, res); err != nil {
				t.Errorf("Delete of a gone resource = %v, want nil", err)
			}
			if len(gone.deleted) != 1 || gone.deleted[0] != res {
				t.Errorf("deleted %v, want [%v]", gone.deleted, res)
			}

			failing := &recordingBarbican{deleteErr: respErr(400)}
			if err := nodeByKey(t, mustBuildRecording(t, failing), key).Delete(context.Background(), nil, res); !gophercloud.ResponseCodeIs(err, 400) {
				t.Errorf("Delete error = %v, want the delete's 400", err)
			}
		})
	}
}

// TestRetrieveMutateReadsBack confirms a Retrieve secret's mutate reads its
// payload back with the store's seed and the plan's payload size, and that a
// mismatch is returned after one call, never retried.
func TestRetrieveMutateReadsBack(t *testing.T) {
	p := churnPlan()
	res := resource.Resource{Kind: barbican.KindSecret, Logical: "sec-0003", ID: "id-sec-0003"}

	t.Run("read-back", func(t *testing.T) {
		f := &recordingBarbican{}
		node := nodeByKey(t, mustBuildRecording(t, f), "sec-0003")
		if err := node.Mutate(context.Background(), nil, res); err != nil {
			t.Fatalf("Mutate: %v", err)
		}
		want := []retrieveCall{{res: res, seed: payload.Seed(p.Seed, "sec-0003"), n: 16}}
		if !reflect.DeepEqual(f.retrieves, want) {
			t.Errorf("RetrievePayload calls = %+v, want %+v", f.retrieves, want)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		mismatch := fmt.Errorf("%w: x", barbican.ErrPayloadMismatch)
		f := &recordingBarbican{retrieveErr: mismatch}
		node := nodeByKey(t, mustBuildRecording(t, f), "sec-0003")
		err := node.Mutate(context.Background(), nil, res)
		if !errors.Is(err, mismatch) {
			t.Errorf("Mutate error = %v, want %v", err, mismatch)
		}
		if len(f.retrieves) != 1 {
			t.Errorf("RetrievePayload called %d times, want exactly 1 (a mismatch is never retried)", len(f.retrieves))
		}
	})
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"mismatch", fmt.Errorf("%w: secret %q differs", barbican.ErrPayloadMismatch, "sec-0001"), "mismatch"},
		{"canceled", context.Canceled, "canceled"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"quota", fmt.Errorf("creating secret %q: %w: %w", "sec-0001", barbican.ErrQuota, respErr(403)), "quota"},
		{"not found", respErr(404), "not-found"},
		{"transient", respErr(503), "transient"},
		{"other", errors.New("boom"), "other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
