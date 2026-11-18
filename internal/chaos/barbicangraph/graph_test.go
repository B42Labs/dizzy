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
	aclErr, consumerErr, verifyACLErr, verifyConsumersErr    error

	calls          []string // method names, in call order
	storeSeeds     []int64
	stamped        []resource.Resource
	stored         map[string]resource.Resource // the stored map CreateContainer last received
	retrieves      []retrieveCall
	deleted        []resource.Resource
	aclSecrets     []resource.Resource // the secret each SetACL call targeted
	consumerCtrs   []resource.Resource // the container each RegisterConsumer call targeted
	aclChecks      []resource.Resource // the resource each VerifyACL call was handed
	consumerChecks []consumersCall
}

// consumersCall is one recorded VerifyConsumers call.
type consumersCall struct {
	container resource.Resource
	want      []resource.Resource
	exact     bool
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

func (f *recordingBarbican) SetACL(_ context.Context, a barbicanplan.ACL, secret resource.Resource) (resource.Resource, error) {
	f.calls = append(f.calls, "SetACL")
	f.aclSecrets = append(f.aclSecrets, secret)
	if f.aclErr != nil {
		return resource.Resource{}, f.aclErr
	}
	return resource.Resource{Kind: barbican.KindACL, Logical: a.Name, ID: secret.ID}, nil
}

func (f *recordingBarbican) RegisterConsumer(_ context.Context, con barbicanplan.Consumer, container resource.Resource) (resource.Resource, error) {
	f.calls = append(f.calls, "RegisterConsumer")
	f.consumerCtrs = append(f.consumerCtrs, container)
	if f.consumerErr != nil {
		return resource.Resource{}, f.consumerErr
	}
	name := "dizzy-run0-" + con.Name
	return resource.Resource{Kind: barbican.KindConsumer, Logical: con.Name, Name: name, ID: container.ID + ":" + name}, nil
}

func (f *recordingBarbican) VerifyACL(_ context.Context, _ barbicanplan.ACL, r resource.Resource) error {
	f.calls = append(f.calls, "VerifyACL")
	f.aclChecks = append(f.aclChecks, r)
	return f.verifyACLErr
}

func (f *recordingBarbican) VerifyConsumers(_ context.Context, container resource.Resource, want []resource.Resource, exact bool) error {
	f.calls = append(f.calls, "VerifyConsumers")
	f.consumerChecks = append(f.consumerChecks, consumersCall{container: container, want: want, exact: exact})
	return f.verifyConsumersErr
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

// TestBuildShape confirms the graph is one node per stored secret, container,
// ACL, and consumer, in that order and each in plan order, with no node for the
// plan's order; that secrets are parentless, each container is parented on its
// secrets in reference order, each ACL on its secret alone, and each consumer
// on its container alone; and that Mutate is set exactly on the secrets the plan
// marks for read-back and on every ACL and consumer.
func TestBuildShape(t *testing.T) {
	p := churnPlan()
	nodes, err := Build(p, &recordingBarbican{}, p.Seed, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// 3 secrets + 2 containers + 2 ACLs + 3 consumers = 10. The order is not a
	// node.
	if len(nodes) != 10 {
		t.Fatalf("built %d nodes, want 10 (secrets, containers, ACLs, consumers; no order node)", len(nodes))
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
	wantKeys := []string{"sec-0001", "sec-0002", "sec-0003", "ctr-0001", "ctr-0002", "acl-0001", "acl-0002", "con-0001", "con-0002", "con-0003"}
	if !slices.Equal(keys, wantKeys) {
		t.Errorf("node keys = %v, want %v (secrets, containers, ACLs, consumers, each in plan order)", keys, wantKeys)
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

	for _, a := range p.ACLs {
		n := nodeByKey(t, nodes, a.Name)
		if n.Kind != barbican.KindACL || !slices.Equal(n.Parents, []string{a.Secret}) || n.Mutate == nil {
			t.Errorf("ACL node %q kind=%q parents=%v mutable=%v, want acl parented on [%s] and mutable", a.Name, n.Kind, n.Parents, n.Mutate != nil, a.Secret)
		}
	}
	for _, con := range p.Consumers {
		n := nodeByKey(t, nodes, con.Name)
		if n.Kind != barbican.KindConsumer || !slices.Equal(n.Parents, []string{con.Container}) || n.Mutate == nil {
			t.Errorf("consumer node %q kind=%q parents=%v mutable=%v, want consumer parented on [%s] and mutable", con.Name, n.Kind, n.Parents, n.Mutate != nil, con.Container)
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
// resource as success and returns any other error, for a secret, a container,
// an ACL, and a consumer alike.
func TestDeleteFoldsNotFound(t *testing.T) {
	for _, key := range []string{"sec-0001", "ctr-0001", "acl-0001", "con-0001"} {
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

// TestACLCreateResolvesSecretID confirms an ACL node's create sets the ACL on
// the current instance of its secret, from the engine's parent ids.
func TestACLCreateResolvesSecretID(t *testing.T) {
	f := &recordingBarbican{}
	node := nodeByKey(t, mustBuildRecording(t, f), "acl-0001")

	res, err := node.Create(context.Background(), map[string]string{"sec-0002": "id-2"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := []resource.Resource{{Kind: barbican.KindSecret, Logical: "sec-0002", ID: "id-2"}}
	if !reflect.DeepEqual(f.aclSecrets, want) {
		t.Errorf("SetACL targets = %v, want %v", f.aclSecrets, want)
	}
	if res != (resource.Resource{Kind: barbican.KindACL, Logical: "acl-0001", ID: "id-2"}) {
		t.Errorf("Create returned %+v, want the ACL under its secret's id", res)
	}
}

// TestConsumerCreateResolvesContainerID confirms a consumer node's create
// registers the consumer on the current instance of its container, from the
// engine's parent ids.
func TestConsumerCreateResolvesContainerID(t *testing.T) {
	f := &recordingBarbican{}
	node := nodeByKey(t, mustBuildRecording(t, f), "con-0002")

	res, err := node.Create(context.Background(), map[string]string{"ctr-0002": "id-ctr-2"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := []resource.Resource{{Kind: barbican.KindContainer, Logical: "ctr-0002", ID: "id-ctr-2"}}
	if !reflect.DeepEqual(f.consumerCtrs, want) {
		t.Errorf("RegisterConsumer targets = %v, want %v", f.consumerCtrs, want)
	}
	if res.ID != "id-ctr-2:dizzy-run0-con-0002" {
		t.Errorf("Create returned %+v, want the consumer registered on id-ctr-2", res)
	}
}

// TestACLMutateReadsBack confirms an ACL's mutate reads that ACL back, and that
// a mismatch is returned after one call, never retried.
func TestACLMutateReadsBack(t *testing.T) {
	res := resource.Resource{Kind: barbican.KindACL, Logical: "acl-0002", ID: "id-sec-0003"}

	f := &recordingBarbican{}
	if err := nodeByKey(t, mustBuildRecording(t, f), "acl-0002").Mutate(context.Background(), nil, res); err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if want := []resource.Resource{res}; !reflect.DeepEqual(f.aclChecks, want) {
		t.Errorf("VerifyACL calls = %v, want %v", f.aclChecks, want)
	}

	mismatch := fmt.Errorf("%w: x", barbican.ErrACLMismatch)
	failing := &recordingBarbican{verifyACLErr: mismatch}
	if err := nodeByKey(t, mustBuildRecording(t, failing), "acl-0002").Mutate(context.Background(), nil, res); !errors.Is(err, mismatch) {
		t.Errorf("Mutate error = %v, want %v", err, mismatch)
	}
	if len(failing.aclChecks) != 1 {
		t.Errorf("VerifyACL called %d times, want exactly 1 (a mismatch is never retried)", len(failing.aclChecks))
	}
}

// TestConsumerMutateVerifiesPresence confirms a consumer's mutate checks the
// current instance of its container for that consumer alone, without exact,
// since other consumers of the container come and go meanwhile, and that a
// mismatch is returned after one call, never retried.
func TestConsumerMutateVerifiesPresence(t *testing.T) {
	res := resource.Resource{Kind: barbican.KindConsumer, Logical: "con-0001", Name: "dizzy-run0-con-0001", ID: "id-ctr-now:dizzy-run0-con-0001"}
	ids := map[string]string{"ctr-0001": "id-ctr-now"}

	f := &recordingBarbican{}
	if err := nodeByKey(t, mustBuildRecording(t, f), "con-0001").Mutate(context.Background(), ids, res); err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	want := []consumersCall{{
		container: resource.Resource{Kind: barbican.KindContainer, Logical: "ctr-0001", ID: "id-ctr-now"},
		want:      []resource.Resource{res},
		exact:     false,
	}}
	if !reflect.DeepEqual(f.consumerChecks, want) {
		t.Errorf("VerifyConsumers calls = %+v, want %+v", f.consumerChecks, want)
	}

	mismatch := fmt.Errorf("%w: x", barbican.ErrConsumerMismatch)
	failing := &recordingBarbican{verifyConsumersErr: mismatch}
	if err := nodeByKey(t, mustBuildRecording(t, failing), "con-0001").Mutate(context.Background(), ids, res); !errors.Is(err, mismatch) {
		t.Errorf("Mutate error = %v, want %v", err, mismatch)
	}
	if len(failing.consumerChecks) != 1 {
		t.Errorf("VerifyConsumers called %d times, want exactly 1 (a mismatch is never retried)", len(failing.consumerChecks))
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"mismatch", fmt.Errorf("%w: secret %q differs", barbican.ErrPayloadMismatch, "sec-0001"), "mismatch"},
		{"acl mismatch", fmt.Errorf("%w: acl %q reads back users [], want [u1]", barbican.ErrACLMismatch, "acl-0001"), "mismatch"},
		{"consumer mismatch", fmt.Errorf("%w: container c1 lacks consumer %q", barbican.ErrConsumerMismatch, "dizzy-run0-con-0001"), "mismatch"},
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
