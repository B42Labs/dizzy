package c5c3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/B42Labs/dizzy/internal/c5c3/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// shortWait returns a context that ends after d, cancelled when the test ends.
func shortWait(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// deletedAt returns a deletion timestamp for an order being deleted.
func deletedAt() *metav1.Time {
	now := metav1.Now()
	return &now
}

// createdObject returns the one object the fake stored for a create of kind.
func createdObject(t *testing.T, dyn interface{ Actions() []k8stesting.Action }, resourceName string) *unstructured.Unstructured {
	t.Helper()
	var objs []*unstructured.Unstructured
	for _, a := range dyn.Actions() {
		if create, ok := a.(k8stesting.CreateActionImpl); ok && create.GetResource().Resource == resourceName {
			objs = append(objs, create.GetObject().(*unstructured.Unstructured))
		}
	}
	if len(objs) != 1 {
		t.Fatalf("creates of %s = %d, want 1", resourceName, len(objs))
	}
	return objs[0]
}

// specJSON renders the spec of obj as JSON with sorted keys, for comparison.
func specJSON(t *testing.T, obj *unstructured.Unstructured) string {
	t.Helper()
	b, err := json.Marshal(obj.Object["spec"])
	if err != nil {
		t.Fatalf("encoding spec: %v", err)
	}
	return string(b)
}

// TestCreateOrderSpecPerKind confirms each kind is created in the client's
// namespace as a c5c3.io/v1alpha1 object of its Kubernetes kind, named
// dizzyorder-<runid>-<logical>, with the dizzy/run and dizzy/type labels and
// the spec of its kind, and returned as <namespace>/<name> with one create
// sample of its kind.
func TestCreateOrderSpecPerKind(t *testing.T) {
	const controlPlane = `"controlPlaneRef":{"name":"controlplane","namespace":"openstack"}`
	for _, tc := range []struct {
		order    plan.Order
		resource string
		k8sKind  string
		spec     string
	}{
		{plan.Order{Kind: plan.KindUser, Name: "user"}, "keystoneusers", "KeystoneUser",
			`{` + controlPlane + `}`},
		{plan.Order{Kind: plan.KindProject, Name: "project"}, "keystoneprojects", "KeystoneProject",
			`{` + controlPlane + `}`},
		{plan.Order{Kind: plan.KindRoleAssignment, Name: "role", User: "user", Project: "project", Role: "member"},
			"keystoneroleassignments", "KeystoneRoleAssignment",
			`{` + controlPlane + `,"projectRef":{"name":"dizzyorder-run0-project"},"role":"member","userRef":{"name":"dizzyorder-run0-user"}}`},
		{plan.Order{Kind: plan.KindCatalogEntry, Name: "catalog", ServiceType: "dizzy-probe", EndpointURL: "https://dizzy-probe.invalid/v1"},
			"keystonecatalogentries", "KeystoneCatalogEntry",
			`{` + controlPlane + `,"endpoints":[{"interface":"public","url":"https://dizzy-probe.invalid/v1"}],"serviceType":"dizzy-probe"}`},
		{plan.Order{Kind: plan.KindApplicationCredential, Name: "appcred", User: "user", Project: "project"},
			"keystoneapplicationcredentials", "KeystoneApplicationCredential",
			`{` + controlPlane + `,"projectRef":{"name":"dizzyorder-run0-project"},"rotation":{"gracePeriod":"2m0s"},"userRef":{"name":"dizzyorder-run0-user"}}`},
	} {
		t.Run(tc.order.Kind, func(t *testing.T) {
			c, dyn, m := testClient()
			r, err := c.CreateOrder(context.Background(), tc.order)
			if err != nil {
				t.Fatalf("CreateOrder: %v", err)
			}
			name := "dizzyorder-run0-" + tc.order.Name
			want := resource.Resource{Kind: resource.Kind(tc.order.Kind), Logical: tc.order.Name, Name: name, ID: "dizzy-orders/" + name}
			if r != want {
				t.Errorf("CreateOrder = %+v, want %+v", r, want)
			}

			obj := createdObject(t, dyn, tc.resource)
			if create := dyn.Actions()[0]; create.GetNamespace() != testNamespace {
				t.Errorf("created in namespace %q, want %q", create.GetNamespace(), testNamespace)
			}
			if obj.GetAPIVersion() != "c5c3.io/v1alpha1" || obj.GetKind() != tc.k8sKind {
				t.Errorf("apiVersion/kind = %s/%s, want c5c3.io/v1alpha1/%s", obj.GetAPIVersion(), obj.GetKind(), tc.k8sKind)
			}
			if obj.GetName() != name || obj.GetNamespace() != testNamespace {
				t.Errorf("metadata = %s/%s, want %s/%s", obj.GetNamespace(), obj.GetName(), testNamespace, name)
			}
			wantLabels := map[string]string{"dizzy/run": "run0", "dizzy/type": tc.order.Kind}
			if fmt.Sprint(obj.GetLabels()) != fmt.Sprint(wantLabels) {
				t.Errorf("labels = %v, want %v", obj.GetLabels(), wantLabels)
			}
			if got := specJSON(t, obj); got != tc.spec {
				t.Errorf("spec = %s\nwant   %s", got, tc.spec)
			}
			if st := statsOf(m.Aggregate(time.Second), r.Kind); st.Attempted != 1 || st.Succeeded != 1 {
				t.Errorf("%s samples = %+v, want 1 succeeded", r.Kind, st)
			}
			// The order is stored where the listings and the waits look for it.
			stored(t, dyn, r.Kind, name)
		})
	}
}

// TestCreateOrderWithoutControlPlaneNamespace confirms an empty
// control_plane.namespace leaves no namespace key in spec.controlPlaneRef of
// any kind.
func TestCreateOrderWithoutControlPlaneNamespace(t *testing.T) {
	c, dyn, _ := testClient()
	p := testPlan()
	p.ControlPlane.Namespace = ""
	c.SetPlan(p)

	for _, o := range p.Orders {
		r, err := c.CreateOrder(context.Background(), o)
		if err != nil {
			t.Fatalf("CreateOrder(%s): %v", o.Name, err)
		}
		k, _ := lookupKind(r.Kind)
		ref, found, err := unstructured.NestedMap(createdObject(t, dyn, k.resource).Object, "spec", "controlPlaneRef")
		if err != nil || !found {
			t.Fatalf("%s spec.controlPlaneRef = %v (found %v, error %v)", o.Kind, ref, found, err)
		}
		if _, ok := ref["namespace"]; ok {
			t.Errorf("%s spec.controlPlaneRef = %v, want no namespace key", o.Kind, ref)
		}
		if ref["name"] != "controlplane" {
			t.Errorf("%s spec.controlPlaneRef.name = %v, want controlplane", o.Kind, ref["name"])
		}
	}
}

// TestCreateOrderLogsCreation confirms a create logs the order's kind,
// logical name and id.
func TestCreateOrderLogsCreation(t *testing.T) {
	logs := captureLog(t)
	c, _, _ := testClient()
	if _, err := c.CreateOrder(context.Background(), testPlan().Orders[1]); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	for _, want := range []string{"created resource", "kind=keystone_project", "logical=project", "id=dizzy-orders/dizzyorder-run0-project"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q lacks %q", logs.String(), want)
		}
	}
}

// TestCreateOrderFailure confirms a refused create returns the API error
// wrapped, names the order, and logs its id as possibly present.
func TestCreateOrderFailure(t *testing.T) {
	logs := captureLog(t)
	c, dyn, m := testClient()
	failVerb(dyn, "create", "keystoneusers", forbidden("keystoneusers", "dizzyorder-run0-user"))

	_, err := c.CreateOrder(context.Background(), testPlan().Orders[0])
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), `"user"`) {
		t.Fatalf("CreateOrder = %v, want the 403 naming the order", err)
	}
	if !strings.Contains(logs.String(), "id=dizzy-orders/dizzyorder-run0-user") {
		t.Errorf("log %q does not name the order's id", logs.String())
	}
	if got := errorCount(m.Aggregate(time.Second), "http_403"); got != 1 {
		t.Errorf("http_403 samples = %d, want 1", got)
	}
}

// TestCreateOrderRefusedWithoutRequest confirms an unknown kind and a client
// without a plan are refused before any request.
func TestCreateOrderRefusedWithoutRequest(t *testing.T) {
	c, dyn, _ := testClient()
	if _, err := c.CreateOrder(context.Background(), plan.Order{Kind: "volume", Name: "vol"}); err == nil || !strings.Contains(err.Error(), `"volume"`) {
		t.Errorf("CreateOrder(volume) = %v, want an error naming the kind", err)
	}
	c.SetPlan(nil)
	if _, err := c.CreateOrder(context.Background(), testPlan().Orders[0]); err == nil || !strings.Contains(err.Error(), "SetPlan") {
		t.Errorf("CreateOrder without a plan = %v, want an error naming SetPlan", err)
	}
	if got := len(dyn.Actions()); got != 0 {
		t.Errorf("requests = %d, want 0", got)
	}
}

// TestReadyRule confirms an order is ready only when its Ready condition is
// True and status.observedGeneration equals metadata.generation.
func TestReadyRule(t *testing.T) {
	r := orderRef(KindUser, "user")
	for _, tc := range []struct {
		name string
		obj  *unstructured.Unstructured
		want bool
	}{
		{"ready", readyObject(r, nil), true},
		{"ready at generation 3", withStatus(orderObject(r, 3), 3, nil, cond("Ready", "True", "AllReady")), true},
		{"missing status", orderObject(r, 1), false},
		{"empty conditions", withStatus(orderObject(r, 1), 1, nil), false},
		{"stale observedGeneration", withStatus(orderObject(r, 2), 1, nil, cond("Ready", "True", "AllReady")), false},
		{"no observedGeneration", func() *unstructured.Unstructured {
			u := withStatus(orderObject(r, 1), 0, nil, cond("Ready", "True", "AllReady"))
			unstructured.RemoveNestedField(u.Object, "status", "observedGeneration")
			return u
		}(), false},
		{"Ready False", pendingObject(r, "WaitingForServiceAccounts"), false},
		{"Ready Unknown", withStatus(orderObject(r, 1), 1, nil, cond("Ready", "Unknown", "Reconciling")), false},
		{"only a sub-condition True", withStatus(orderObject(r, 1), 1, nil, cond("UserReady", "True", "UserProvisioned")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := parseOrder(KindUser, tc.obj)
			if err != nil {
				t.Fatalf("parseOrder: %v", err)
			}
			if s.status.Ready != tc.want {
				t.Errorf("Ready = %v, want %v", s.status.Ready, tc.want)
			}
		})
	}
}

// TestStatus confirms Status reads the fields of each kind, leaves the
// fields a kind lacks at their zero value, and is one get sample.
func TestStatus(t *testing.T) {
	deleteAt := "2026-11-19T10:02:00Z"
	extra := map[string]any{
		"passwordGeneration":         int64(3),
		"credentialGeneration":       int64(3),
		"credentialID":               "ac-new",
		"previousCredentialID":       "ac-old",
		"previousCredentialDeleteAt": deleteAt,
		"projectName":                "dizzyorder-run0-project",
		"domainName":                 "Default",
		"serviceName":                "dizzyorder-run0-catalog",
	}
	at, _ := time.Parse(time.RFC3339, deleteAt)
	for _, tc := range []struct {
		kind    resource.Kind
		logical string
		want    OrderStatus
	}{
		{KindUser, "user", OrderStatus{Ready: true, SpecGeneration: 4, Generation: 3}},
		{KindApplicationCredential, "appcred", OrderStatus{Ready: true, SpecGeneration: 4, Generation: 3,
			CredentialID: "ac-new", PreviousCredentialID: "ac-old", PreviousCredentialDeleteAt: at}},
		{KindProject, "project", OrderStatus{Ready: true, ProjectName: "dizzyorder-run0-project", DomainName: "Default"}},
		{KindCatalogEntry, "catalog", OrderStatus{Ready: true, ServiceName: "dizzyorder-run0-catalog"}},
		{KindRoleAssignment, "role", OrderStatus{Ready: true}},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			c, dyn, m := testClient()
			r := orderRef(tc.kind, tc.logical)
			obj := readyObject(r, extra)
			obj.Object["spec"] = map[string]any{"passwordGeneration": int64(4), "credentialGeneration": int64(4)}
			seed(t, dyn, tc.kind, obj)

			got, err := c.Status(context.Background(), r)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if got != tc.want {
				t.Errorf("Status = %+v\nwant     %+v", got, tc.want)
			}
			if st := statsOf(m.Aggregate(time.Second), tc.kind); st.Attempted != 1 || st.Succeeded != 1 {
				t.Errorf("%s samples = %+v, want 1 succeeded", tc.kind, st)
			}
		})
	}

	t.Run("reason and deleting", func(t *testing.T) {
		c, dyn, _ := testClient()
		r := orderRef(KindUser, "user")
		obj := withStatus(orderObject(r, 1), 1, nil,
			cond("UserReady", "True", "UserProvisioned"),
			cond("DeliveryReady", "False", "BackupNotSynced"),
			cond("Ready", "False", "NotAllReady"))
		obj.SetDeletionTimestamp(deletedAt())
		seed(t, dyn, KindUser, obj)
		got, err := c.Status(context.Background(), r)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if got.Ready || !got.Deleting || got.Reason != "BackupNotSynced" {
			t.Errorf("Status = %+v, want not ready, deleting, reason BackupNotSynced", got)
		}
	})

	t.Run("404", func(t *testing.T) {
		c, _, m := testClient()
		_, err := c.Status(context.Background(), orderRef(KindUser, "user"))
		if !IsNotFound(err) {
			t.Errorf("Status of a missing order = %v, want a 404", err)
		}
		if got := errorCount(m.Aggregate(time.Second), "http_404"); got != 1 {
			t.Errorf("http_404 samples = %d, want 1", got)
		}
	})

	t.Run("malformed delete time", func(t *testing.T) {
		c, dyn, _ := testClient()
		r := orderRef(KindApplicationCredential, "appcred")
		seed(t, dyn, r.Kind, readyObject(r, map[string]any{"previousCredentialDeleteAt": "tomorrow"}))
		if _, err := c.Status(context.Background(), r); err == nil || !strings.Contains(err.Error(), "previousCredentialDeleteAt") {
			t.Errorf("Status = %v, want an error naming previousCredentialDeleteAt", err)
		}
	})
}

// TestWaitReadyReady confirms a ready order ends the wait at the first poll
// with one get sample and one successful time-to-ready sample.
func TestWaitReadyReady(t *testing.T) {
	c, dyn, m, reader := telemetryTestClient(t)
	r := orderRef(KindUser, "user")
	seed(t, dyn, KindUser, readyObject(r, nil))

	if err := c.WaitReady(shortWait(t, 5*time.Second), r); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	agg := m.Aggregate(time.Second)
	if rd := readinessOf(agg, KindUser); rd.Count != 1 || rd.OK != 1 {
		t.Errorf("readiness = %+v, want 1 ok", rd)
	}
	if st := statsOf(agg, KindUser); st.Attempted != 1 || st.Succeeded != 1 || countActions(dyn, "get", "keystoneusers") != 1 {
		t.Errorf("samples = %+v after %d gets, want 1 succeeded get", st, countActions(dyn, "get", "keystoneusers"))
	}
	if got := countOf(metricPoints(t, reader, "dizzy.resource.time_to_ready"),
		map[string]string{"kind": "keystone_user", "outcome": "success"}); got != 1 {
		t.Errorf("time_to_ready successes = %d, want 1", got)
	}
}

// TestWaitReadyPollsUntilReady confirms the wait polls through a pending
// order and a failed GET until the order is ready, records every poll as one
// get sample, the failed one as http_500, and one successful time-to-ready
// sample.
func TestWaitReadyPollsUntilReady(t *testing.T) {
	c, dyn, m := testClient()
	r := orderRef(KindRoleAssignment, "role")
	script(t, dyn, "keystoneroleassignments",
		orderObject(r, 1), internalError(), pendingObject(r, "WaitingForServiceAccounts"), readyObject(r, nil))

	if err := c.WaitReady(shortWait(t, 5*time.Second), r); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	agg := m.Aggregate(time.Second)
	gets := countActions(dyn, "get", "keystoneroleassignments")
	if gets != 4 {
		t.Errorf("gets = %d, want 4", gets)
	}
	if st := statsOf(agg, KindRoleAssignment); st.Attempted != gets || st.Failed != 1 {
		t.Errorf("samples = %+v, want %d with 1 failed", st, gets)
	}
	if got := errorCount(agg, "http_500"); got != 1 {
		t.Errorf("http_500 samples = %d, want 1", got)
	}
	if rd := readinessOf(agg, KindRoleAssignment); rd.Count != 1 || rd.OK != 1 {
		t.Errorf("readiness = %+v, want 1 ok", rd)
	}
}

// TestWaitReadyKeepsPollingUntilDeadline confirms an order that is not ready,
// or reports a reason the operator retries or its cache can report for a
// moment, keeps the wait polling until the deadline, which returns
// context.DeadlineExceeded with one failed time-to-ready sample of outcome
// timeout and one get sample per poll.
func TestWaitReadyKeepsPollingUntilDeadline(t *testing.T) {
	r := orderRef(KindUser, "user")
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
	}{
		{"missing status", orderObject(r, 1)},
		{"empty conditions", withStatus(orderObject(r, 1), 1, nil)},
		{"stale observedGeneration", withStatus(orderObject(r, 2), 1, nil, cond("Ready", "True", "AllReady"))},
	}
	for _, reason := range []string{"WaitingForServiceAccounts", "ServiceAccountsFailed", "NoRoleOnProject", "UserNotFound",
		"ProjectNotFound", "ServiceAccountError", "DeliveryError", "BackupNotSynced", "ProbingForCollision"} {
		cases = append(cases, struct {
			name string
			obj  *unstructured.Unstructured
		}{reason, pendingObject(r, reason)})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, dyn, m, reader := telemetryTestClient(t)
			seed(t, dyn, KindUser, tc.obj)

			err := c.WaitReady(shortWait(t, 100*time.Millisecond), r)
			if err != context.DeadlineExceeded {
				t.Fatalf("WaitReady = %v, want context.DeadlineExceeded", err)
			}
			agg := m.Aggregate(time.Second)
			gets := countActions(dyn, "get", "keystoneusers")
			if gets < 2 {
				t.Errorf("gets = %d, want the wait to keep polling", gets)
			}
			if st := statsOf(agg, KindUser); st.Attempted != gets || st.Succeeded != gets {
				t.Errorf("samples = %+v, want one successful get per poll (%d)", st, gets)
			}
			if rd := readinessOf(agg, KindUser); rd.Count != 1 || rd.OK != 0 {
				t.Errorf("readiness = %+v, want 1 failed", rd)
			}
			ready := metricPoints(t, reader, "dizzy.resource.time_to_ready")
			if len(ready) != 1 || countOf(ready, map[string]string{"kind": "keystone_user", "outcome": "timeout"}) != 1 {
				t.Errorf("time_to_ready points = %v, want one keystone_user timeout", ready)
			}
		})
	}
}

// TestWaitReadyRefusal confirms each of the twelve refusal reasons ends the
// wait on the first poll that sees it with ErrRefused quoting the reason and
// message, records that poll as a failed get with error kind refused, and
// records one failed time-to-ready sample.
func TestWaitReadyRefusal(t *testing.T) {
	r := orderRef(KindRoleAssignment, "role")
	for _, reason := range []string{"NamespaceNotAssigned", "ControlPlaneNotFound", "ClusterNameTooLong", "ControlPlaneMismatch",
		"ServiceAccountCollision", "ProjectCollision", "ServiceCollision", "RoleNotAllowed", "CatalogNotAllowed",
		"DuplicateRoleAssignment", "KeystoneNotPublished", "DeliveryRefused"} {
		t.Run(reason, func(t *testing.T) {
			c, dyn, m, reader := telemetryTestClient(t)
			script(t, dyn, "keystoneroleassignments", orderObject(r, 1), pendingObject(r, "WaitingForServiceAccounts"),
				withStatus(orderObject(r, 1), 1, nil, cond("AssignmentReady", "True", "Assigned"),
					cond("DeliveryReady", "False", reason), cond("Ready", "False", "NotAllReady")))

			err := c.WaitReady(shortWait(t, 5*time.Second), r)
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("WaitReady = %v, want ErrRefused", err)
			}
			if !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), "message of "+reason) {
				t.Errorf("error %q does not quote the reason and message", err)
			}
			if gets := countActions(dyn, "get", "keystoneroleassignments"); gets != 3 {
				t.Errorf("gets = %d, want 3: the wait ends on the poll that sees the refusal", gets)
			}
			agg := m.Aggregate(time.Second)
			if st := statsOf(agg, KindRoleAssignment); st.Attempted != 3 || st.Failed != 1 {
				t.Errorf("samples = %+v, want 3 gets with 1 failed", st)
			}
			if got := errorCount(agg, "refused"); got != 1 {
				t.Errorf("refused samples = %d, want 1", got)
			}
			if rd := readinessOf(agg, KindRoleAssignment); rd.Count != 1 || rd.OK != 0 {
				t.Errorf("readiness = %+v, want 1 failed", rd)
			}
			if got := countOf(metricPoints(t, reader, "dizzy.operation.errors"),
				map[string]string{"kind": "keystone_role_assignment", "operation": "get", "error.kind": "refused"}); got != 1 {
				t.Errorf("get refused errors = %d, want 1", got)
			}
		})
	}
}

// TestWaitReadyNotFoundMidWait confirms an order the fake deletes mid-wait
// ends the wait with an error wrapping the 404 and a failed time-to-ready
// sample.
func TestWaitReadyNotFoundMidWait(t *testing.T) {
	c, dyn, m := testClient()
	r := orderRef(KindProject, "project")
	seed(t, dyn, KindProject, orderObject(r, 1))
	gets := 0
	dyn.PrependReactor("get", "keystoneprojects", func(k8stesting.Action) (bool, runtime.Object, error) {
		if gets++; gets == 3 {
			if err := dyn.Tracker().Delete(gvrOf(KindProject), testNamespace, r.Name); err != nil {
				t.Errorf("deleting the order: %v", err)
			}
		}
		return false, nil, nil
	})

	err := c.WaitReady(shortWait(t, 5*time.Second), r)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("WaitReady = %v, want an error wrapping the 404", err)
	}
	if gets != 3 {
		t.Errorf("gets = %d, want 3", gets)
	}
	agg := m.Aggregate(time.Second)
	if rd := readinessOf(agg, KindProject); rd.Count != 1 || rd.OK != 0 {
		t.Errorf("readiness = %+v, want 1 failed", rd)
	}
	if got := errorCount(agg, "http_404"); got != 1 {
		t.Errorf("http_404 samples = %d, want 1", got)
	}
}

// TestWaitReason confirms the wait for a refusal reason: nil once a
// condition with status False carries it, ErrMismatch when the order turns
// ready or reports another refusal reason, an error wrapping a 404, and
// context.DeadlineExceeded at the deadline, each with one refuse sample.
func TestWaitReason(t *testing.T) {
	r := orderRef(KindApplicationCredential, "refused-appcred")
	for _, tc := range []struct {
		name     string
		answers  []any
		check    func(error) bool
		wantGets int
		errKind  string
	}{
		{"reason reported", []any{orderObject(r, 1), pendingObject(r, "WaitingForServiceAccounts"), pendingObject(r, "NoRoleOnProject")},
			func(err error) bool { return err == nil }, 3, ""},
		{"ready instead", []any{orderObject(r, 1), readyObject(r, nil)},
			func(err error) bool { return errors.Is(err, ErrMismatch) && strings.Contains(err.Error(), "ready") }, 2, "mismatch"},
		{"another refusal reason", []any{pendingObject(r, "DeliveryRefused")},
			func(err error) bool {
				return errors.Is(err, ErrMismatch) && strings.Contains(err.Error(), "DeliveryRefused")
			}, 1, "mismatch"},
		{"gone", []any{orderObject(r, 1), notFound("keystoneapplicationcredentials", r.Name)},
			func(err error) bool { return apierrors.IsNotFound(err) && !errors.Is(err, ErrMismatch) }, 2, "http_404"},
		{"deadline", []any{pendingObject(r, "WaitingForServiceAccounts")},
			func(err error) bool { return err == context.DeadlineExceeded }, -1, "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, dyn, _, reader := telemetryTestClient(t)
			script(t, dyn, "keystoneapplicationcredentials", tc.answers...)

			err := c.WaitReason(shortWait(t, 100*time.Millisecond), r, "NoRoleOnProject")
			if !tc.check(err) {
				t.Fatalf("WaitReason = %v", err)
			}
			gets := countActions(dyn, "get", "keystoneapplicationcredentials")
			if tc.wantGets >= 0 && gets != tc.wantGets {
				t.Errorf("gets = %d, want %d", gets, tc.wantGets)
			}
			ops := metricPoints(t, reader, "dizzy.operation.duration")
			var refuse int64
			for _, p := range ops {
				if p.attrs["operation"] == "refuse" {
					refuse += p.count
				}
			}
			if refuse != 1 {
				t.Errorf("refuse samples = %d, want 1", refuse)
			}
			if tc.errKind != "" {
				if got := countOf(metricPoints(t, reader, "dizzy.operation.errors"), map[string]string{
					"kind": "keystone_application_credential", "operation": "refuse", "error.kind": tc.errKind}); got != 1 {
					t.Errorf("refuse %s errors = %d, want 1", tc.errKind, got)
				}
			}
		})
	}
}

// TestWaitGone confirms the wait polls a present order until its 404, records
// each poll as one get sample and the final 404 as a successful one, and one
// finalize sample over the whole wait; at the deadline it returns
// context.DeadlineExceeded with a failed finalize sample.
func TestWaitGone(t *testing.T) {
	r := orderRef(KindUser, "user")
	t.Run("gone", func(t *testing.T) {
		c, dyn, m, reader := telemetryTestClient(t)
		deleting := orderObject(r, 1)
		deleting.SetDeletionTimestamp(deletedAt())
		script(t, dyn, "keystoneusers", deleting, internalError(), deleting, notFound("keystoneusers", r.Name))

		if err := c.WaitGone(shortWait(t, 5*time.Second), r); err != nil {
			t.Fatalf("WaitGone: %v", err)
		}
		agg := m.Aggregate(time.Second)
		// Four gets, the 500 the only failure, and one finalize.
		if st := statsOf(agg, KindUser); st.Attempted != 5 || st.Failed != 1 {
			t.Errorf("samples = %+v, want 4 gets and a finalize with 1 failed", st)
		}
		ops := metricPoints(t, reader, "dizzy.operation.duration")
		if got := countOf(ops, map[string]string{"kind": "keystone_user", "operation": "get", "outcome": "success"}); got != 3 {
			t.Errorf("successful gets = %d, want 3, the final 404 among them", got)
		}
		if got := countOf(ops, map[string]string{"kind": "keystone_user", "operation": "finalize", "outcome": "success"}); got != 1 {
			t.Errorf("successful finalizes = %d, want 1", got)
		}
		if got := errorCount(agg, "http_404"); got != 0 {
			t.Errorf("http_404 samples = %d, want 0", got)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		c, dyn, _, reader := telemetryTestClient(t)
		seed(t, dyn, KindUser, orderObject(r, 1))
		if err := c.WaitGone(shortWait(t, 50*time.Millisecond), r); err != context.DeadlineExceeded {
			t.Fatalf("WaitGone = %v, want context.DeadlineExceeded", err)
		}
		if got := countOf(metricPoints(t, reader, "dizzy.operation.duration"),
			map[string]string{"kind": "keystone_user", "operation": "finalize", "outcome": "timeout"}); got != 1 {
			t.Errorf("finalize timeouts = %d, want 1", got)
		}
	})
}

// TestDelete confirms Delete removes the order its id names as one delete
// sample, treats a 404 as success, and returns any other failure.
func TestDelete(t *testing.T) {
	c, dyn, m := testClient()
	r := orderRef(KindCatalogEntry, "catalog")
	seed(t, dyn, KindCatalogEntry, orderObject(r, 1))

	if err := c.Delete(context.Background(), r); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := dyn.Tracker().Get(gvrOf(KindCatalogEntry), testNamespace, r.Name); !apierrors.IsNotFound(err) {
		t.Errorf("order after Delete: %v, want gone", err)
	}
	if err := c.Delete(context.Background(), r); err != nil {
		t.Errorf("Delete of a gone order = %v, want nil", err)
	}
	if st := statsOf(m.Aggregate(time.Second), KindCatalogEntry); st.Attempted != 2 || st.Succeeded != 2 {
		t.Errorf("samples = %+v, want 2 succeeded deletes", st)
	}

	failVerb(dyn, "delete", "keystonecatalogentries", internalError())
	if err := c.Delete(context.Background(), r); !apierrors.IsInternalError(err) {
		t.Errorf("Delete = %v, want the 500", err)
	}
}

// TestMalformedResourceSendsNoRequest confirms every method that takes an
// order refuses an id without a slash and the kind volume without sending a
// request.
func TestMalformedResourceSendsNoRequest(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(*Client, resource.Resource) error{
		"Status":  func(c *Client, r resource.Resource) error { _, err := c.Status(ctx, r); return err },
		"Delete":  func(c *Client, r resource.Resource) error { return c.Delete(ctx, r) },
		"Observe": func(c *Client, r resource.Resource) error { _, _, err := c.Observe(ctx, r); return err },
		"WaitReady": func(c *Client, r resource.Resource) error {
			return c.WaitReady(ctx, r)
		},
		"WaitReason": func(c *Client, r resource.Resource) error { return c.WaitReason(ctx, r, "RoleNotAllowed") },
		"WaitGone":   func(c *Client, r resource.Resource) error { return c.WaitGone(ctx, r) },
		"RaiseGeneration": func(c *Client, r resource.Resource) error {
			_, err := c.RaiseGeneration(ctx, r)
			return err
		},
		"WaitRotated": func(c *Client, r resource.Resource) error {
			_, err := c.WaitRotated(ctx, r, 2, Credential{})
			return err
		},
		"WaitPreviousGone":      func(c *Client, r resource.Resource) error { return c.WaitPreviousGone(ctx, r) },
		"WaitGenerationSettled": func(c *Client, r resource.Resource) error { return c.WaitGenerationSettled(ctx, r) },
		"ReadCredential": func(c *Client, r resource.Resource) error {
			_, err := c.ReadCredential(ctx, r)
			return err
		},
		"LookupCredential": func(c *Client, r resource.Resource) error {
			_, _, err := c.LookupCredential(ctx, r)
			return err
		},
		"EditSecret":         func(c *Client, r resource.Resource) error { return c.EditSecret(ctx, r) },
		"DeleteSecret":       func(c *Client, r resource.Resource) error { return c.DeleteSecret(ctx, r) },
		"WaitSecretRestored": func(c *Client, r resource.Resource) error { return c.WaitSecretRestored(ctx, r, Credential{}) },
		"WaitSecretGone":     func(c *Client, r resource.Resource) error { return c.WaitSecretGone(ctx, r) },
	}
	user := orderRef(KindUser, "user")
	for _, bad := range []struct {
		name string
		r    resource.Resource
	}{
		{"id without slash", resource.Resource{Kind: KindUser, Logical: "user", Name: user.Name, ID: user.Name}},
		{"id with two slashes", resource.Resource{Kind: KindUser, Logical: "user", Name: user.Name, ID: "a/b/" + user.Name}},
		{"empty namespace", resource.Resource{Kind: KindUser, Logical: "user", Name: user.Name, ID: "/" + user.Name}},
		{"kind volume", resource.Resource{Kind: "volume", Logical: "user", Name: user.Name, ID: user.ID}},
	} {
		for name, call := range calls {
			t.Run(bad.name+"/"+name, func(t *testing.T) {
				c, dyn, _ := testClient()
				if err := call(c, bad.r); err == nil {
					t.Errorf("%s(%+v) = nil, want an error", name, bad.r)
				}
				if got := dyn.Actions(); len(got) != 0 {
					t.Errorf("%s sent %d requests, want none", name, len(got))
				}
			})
		}
	}
}

// TestRaiseGeneration confirms a user at status generation 1 gets
// spec.passwordGeneration 2 and the call returns 2, as one get and one update
// sample, that a credential order's spec.credentialGeneration is raised the
// same way, and that a project order is refused without a request.
func TestRaiseGeneration(t *testing.T) {
	t.Run("user", func(t *testing.T) {
		c, dyn, m := testClient()
		r := orderRef(KindUser, "user")
		obj := readyObject(r, map[string]any{"passwordGeneration": int64(1)})
		obj.Object["spec"] = map[string]any{"controlPlaneRef": map[string]any{"name": "controlplane"}, "passwordGeneration": int64(1)}
		seed(t, dyn, KindUser, obj)

		got, err := c.RaiseGeneration(context.Background(), r)
		if err != nil || got != 2 {
			t.Fatalf("RaiseGeneration = %d, %v, want 2, nil", got, err)
		}
		gen, _, _ := unstructured.NestedInt64(stored(t, dyn, KindUser, r.Name).Object, "spec", "passwordGeneration")
		if gen != 2 {
			t.Errorf("spec.passwordGeneration = %d, want 2", gen)
		}
		var patches []string
		for _, a := range dyn.Actions() {
			if p, ok := a.(k8stesting.PatchActionImpl); ok {
				patches = append(patches, string(p.GetPatchType())+" "+string(p.GetPatch()))
			}
		}
		if want := []string{`application/merge-patch+json {"spec":{"passwordGeneration":2}}`}; !slices.Equal(patches, want) {
			t.Errorf("patches = %v, want %v", patches, want)
		}
		if st := statsOf(m.Aggregate(time.Second), KindUser); st.Attempted != 2 || st.Succeeded != 2 {
			t.Errorf("samples = %+v, want a get and an update", st)
		}
	})
	t.Run("application credential", func(t *testing.T) {
		c, dyn, _ := testClient()
		r := orderRef(KindApplicationCredential, "appcred")
		seed(t, dyn, r.Kind, readyObject(r, map[string]any{"credentialGeneration": int64(4)}))
		if got, err := c.RaiseGeneration(context.Background(), r); err != nil || got != 5 {
			t.Fatalf("RaiseGeneration = %d, %v, want 5, nil", got, err)
		}
		gen, _, _ := unstructured.NestedInt64(stored(t, dyn, r.Kind, r.Name).Object, "spec", "credentialGeneration")
		if gen != 5 {
			t.Errorf("spec.credentialGeneration = %d, want 5", gen)
		}
	})
	t.Run("project", func(t *testing.T) {
		c, dyn, _ := testClient()
		r := orderRef(KindProject, "project")
		seed(t, dyn, KindProject, readyObject(r, nil))
		if _, err := c.RaiseGeneration(context.Background(), r); err == nil {
			t.Error("RaiseGeneration(project) = nil, want an error")
		}
		if got := len(dyn.Actions()); got != 0 {
			t.Errorf("requests = %d, want 0", got)
		}
	})
	t.Run("409 is retryable", func(t *testing.T) {
		c, dyn, _ := testClient()
		r := orderRef(KindUser, "user")
		seed(t, dyn, KindUser, readyObject(r, map[string]any{"passwordGeneration": int64(1)}))
		failVerb(dyn, "patch", "keystoneusers", conflict("keystoneusers", r.Name))
		_, err := c.RaiseGeneration(context.Background(), r)
		if !apierrors.IsConflict(err) || !IsRetryable(err) {
			t.Errorf("RaiseGeneration = %v (retryable %v), want a retryable 409", err, IsRetryable(err))
		}
	})
}

// TestWaitRotated confirms the wait for a rotation polls until the order is
// ready at the raised generation, returns the new credential, and returns
// ErrMismatch when the Secret still carries the old password or credential
// id, each with one rotate sample.
func TestWaitRotated(t *testing.T) {
	user := orderRef(KindUser, "user")
	t.Run("user rotated", func(t *testing.T) {
		c, dyn, _, reader := telemetryTestClient(t)
		seed(t, dyn, KindSecret, userSecret("pw-two", "7"))
		script(t, dyn, "keystoneusers",
			readyObject(user, map[string]any{"passwordGeneration": int64(1)}),
			withStatus(orderObject(user, 2), 1, map[string]any{"passwordGeneration": int64(1)}, cond("Ready", "True", "AllReady")),
			readyAt(user, 2, map[string]any{"passwordGeneration": int64(2)}))

		got, err := c.WaitRotated(shortWait(t, 5*time.Second), user, 2, Credential{Password: "pw-one"})
		if err != nil {
			t.Fatalf("WaitRotated: %v", err)
		}
		if got.Password != "pw-two" || got.ResourceVersion != "7" {
			t.Errorf("WaitRotated = %v with password %q, want the new credential", got, got.Password)
		}
		if gets := countActions(dyn, "get", "keystoneusers"); gets != 3 {
			t.Errorf("order gets = %d, want 3", gets)
		}
		if got := countOf(metricPoints(t, reader, "dizzy.operation.duration"),
			map[string]string{"kind": "keystone_user", "operation": "rotate", "outcome": "success"}); got != 1 {
			t.Errorf("successful rotates = %d, want 1", got)
		}
	})
	t.Run("user password unchanged", func(t *testing.T) {
		c, dyn, _, reader := telemetryTestClient(t)
		seed(t, dyn, KindSecret, userSecret("pw-one", "7"))
		seed(t, dyn, KindUser, readyAt(user, 2, map[string]any{"passwordGeneration": int64(2)}))

		_, err := c.WaitRotated(shortWait(t, 5*time.Second), user, 2, Credential{Password: "pw-one"})
		if !errors.Is(err, ErrMismatch) {
			t.Fatalf("WaitRotated = %v, want ErrMismatch", err)
		}
		if strings.Contains(err.Error(), "pw-one") {
			t.Errorf("error %q carries the password", err)
		}
		if got := countOf(metricPoints(t, reader, "dizzy.operation.errors"),
			map[string]string{"kind": "keystone_user", "operation": "rotate", "error.kind": "mismatch"}); got != 1 {
			t.Errorf("rotate mismatches = %d, want 1", got)
		}
	})
	t.Run("credential id unchanged", func(t *testing.T) {
		c, dyn, _ := testClient()
		appcred := orderRef(KindApplicationCredential, "appcred")
		seed(t, dyn, KindSecret, appcredSecret("ac-old", "s3cret-new", "4"))
		seed(t, dyn, appcred.Kind, readyAt(appcred, 2, map[string]any{"credentialGeneration": int64(2)}))

		_, err := c.WaitRotated(shortWait(t, 5*time.Second), appcred, 2, Credential{ApplicationCredentialID: "ac-old"})
		if !errors.Is(err, ErrMismatch) || !strings.Contains(err.Error(), "ac-old") {
			t.Fatalf("WaitRotated = %v, want ErrMismatch naming ac-old", err)
		}
	})
	t.Run("credential rotated", func(t *testing.T) {
		c, dyn, _ := testClient()
		appcred := orderRef(KindApplicationCredential, "appcred")
		seed(t, dyn, KindSecret, appcredSecret("ac-new", "s3cret-new", "4"))
		seed(t, dyn, appcred.Kind, readyAt(appcred, 2, map[string]any{"credentialGeneration": int64(2)}))

		got, err := c.WaitRotated(shortWait(t, 5*time.Second), appcred, 2, Credential{ApplicationCredentialID: "ac-old"})
		if err != nil || got.ApplicationCredentialID != "ac-new" {
			t.Fatalf("WaitRotated = %v, %v, want the credential ac-new", got, err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		c, dyn, _ := testClient()
		seed(t, dyn, KindUser, readyObject(user, map[string]any{"passwordGeneration": int64(1)}))
		if _, err := c.WaitRotated(shortWait(t, 50*time.Millisecond), user, 2, Credential{}); err != context.DeadlineExceeded {
			t.Fatalf("WaitRotated = %v, want context.DeadlineExceeded", err)
		}
	})
	t.Run("project", func(t *testing.T) {
		c, dyn, _ := testClient()
		if _, err := c.WaitRotated(context.Background(), orderRef(KindProject, "project"), 2, Credential{}); err == nil {
			t.Error("WaitRotated(project) = nil, want an error")
		}
		if got := len(dyn.Actions()); got != 0 {
			t.Errorf("requests = %d, want 0", got)
		}
	})
}

// readyAt returns the order r addresses ready at metadata.generation
// generation, with the extra status fields.
func readyAt(r resource.Resource, generation int64, extra map[string]any) *unstructured.Unstructured {
	return withStatus(orderObject(r, generation), generation, extra, cond("Ready", "True", "AllReady"))
}

// TestWaitPreviousGone confirms the wait polls until the status names no
// superseded credential.
func TestWaitPreviousGone(t *testing.T) {
	c, dyn, m := testClient()
	r := orderRef(KindApplicationCredential, "appcred")
	script(t, dyn, "keystoneapplicationcredentials",
		readyObject(r, map[string]any{"credentialID": "ac-new", "previousCredentialID": "ac-old"}),
		readyObject(r, map[string]any{"credentialID": "ac-new", "previousCredentialID": "ac-old"}),
		readyObject(r, map[string]any{"credentialID": "ac-new"}))

	if err := c.WaitPreviousGone(shortWait(t, 5*time.Second), r); err != nil {
		t.Fatalf("WaitPreviousGone: %v", err)
	}
	if st := statsOf(m.Aggregate(time.Second), r.Kind); st.Attempted != 3 || st.Succeeded != 3 {
		t.Errorf("samples = %+v, want 3 successful gets", st)
	}

	c, dyn, _ = testClient()
	seed(t, dyn, r.Kind, readyObject(r, map[string]any{"previousCredentialID": "ac-old"}))
	if err := c.WaitPreviousGone(shortWait(t, 50*time.Millisecond), r); err != context.DeadlineExceeded {
		t.Errorf("WaitPreviousGone = %v, want context.DeadlineExceeded", err)
	}
}

// TestWaitGenerationSettled confirms the wait polls until the status
// generation reaches the spec generation, or the order is being deleted.
func TestWaitGenerationSettled(t *testing.T) {
	r := orderRef(KindUser, "user")
	rotating := func(spec, status int64) *unstructured.Unstructured {
		u := readyObject(r, map[string]any{"passwordGeneration": status})
		u.Object["spec"] = map[string]any{"passwordGeneration": spec}
		return u
	}
	t.Run("caught up", func(t *testing.T) {
		c, dyn, _ := testClient()
		script(t, dyn, "keystoneusers", rotating(2, 1), rotating(2, 1), rotating(2, 2))
		if err := c.WaitGenerationSettled(shortWait(t, 5*time.Second), r); err != nil {
			t.Fatalf("WaitGenerationSettled: %v", err)
		}
		if gets := countActions(dyn, "get", "keystoneusers"); gets != 3 {
			t.Errorf("gets = %d, want 3", gets)
		}
	})
	t.Run("deleting", func(t *testing.T) {
		c, dyn, _ := testClient()
		deleting := rotating(2, 1)
		deleting.SetDeletionTimestamp(deletedAt())
		script(t, dyn, "keystoneusers", rotating(2, 1), deleting)
		if err := c.WaitGenerationSettled(shortWait(t, 5*time.Second), r); err != nil {
			t.Fatalf("WaitGenerationSettled: %v", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		c, dyn, _ := testClient()
		seed(t, dyn, KindUser, rotating(3, 2))
		if err := c.WaitGenerationSettled(shortWait(t, 50*time.Millisecond), r); err != context.DeadlineExceeded {
			t.Fatalf("WaitGenerationSettled = %v, want context.DeadlineExceeded", err)
		}
	})
}

// labelled returns an order of kind named name in testNamespace with labels.
func labelled(kind resource.Kind, name string, labels map[string]any) *unstructured.Unstructured {
	u := orderObject(resource.Resource{Kind: kind, Name: name}, 1)
	if labels == nil {
		unstructured.RemoveNestedField(u.Object, "metadata", "labels")
	} else {
		u.Object["metadata"].(map[string]any)["labels"] = labels
	}
	return u
}

// TestListByRun confirms ListByRun lists in the client's namespace with the
// selector dizzy/run=<runid>, returns only that run's orders with their
// logical name, name and id, sorted by name, as one list sample.
func TestListByRun(t *testing.T) {
	c, dyn, m := testClient()
	seed(t, dyn, KindUser, labelled(KindUser, "dizzyorder-run0-user", map[string]any{labelRun: "run0", labelType: "keystone_user"}))
	seed(t, dyn, KindUser, labelled(KindUser, "dizzyorder-run0-another", map[string]any{labelRun: "run0", labelType: "keystone_user"}))
	seed(t, dyn, KindUser, labelled(KindUser, "dizzyorder-run1-user", map[string]any{labelRun: "run1", labelType: "keystone_user"}))
	seed(t, dyn, KindUser, labelled(KindUser, "someone-else", nil))

	found, err := c.ListByRun(context.Background(), KindUser, "run0")
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	want := []resource.Resource{
		{Kind: KindUser, Logical: "another", Name: "dizzyorder-run0-another", ID: "dizzy-orders/dizzyorder-run0-another"},
		{Kind: KindUser, Logical: "user", Name: "dizzyorder-run0-user", ID: "dizzy-orders/dizzyorder-run0-user"},
	}
	if !slices.Equal(found, want) {
		t.Errorf("ListByRun = %+v\nwant         %+v", found, want)
	}
	list := dyn.Actions()[0].(k8stesting.ListActionImpl)
	if list.GetNamespace() != testNamespace || list.ListOptions.LabelSelector != "dizzy/run=run0" {
		t.Errorf("list in %q with selector %q, want %q with dizzy/run=run0", list.GetNamespace(), list.ListOptions.LabelSelector, testNamespace)
	}
	if st := statsOf(m.Aggregate(time.Second), KindUser); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("samples = %+v, want 1 list", st)
	}
}

// TestListByAnyRun confirms ListByAnyRun selects every order that carries a
// dizzy/type label, whatever its run, and takes each logical name from that
// order's own run label.
func TestListByAnyRun(t *testing.T) {
	c, dyn, _ := testClient()
	seed(t, dyn, KindProject, labelled(KindProject, "dizzyorder-run0-project", map[string]any{labelRun: "run0", labelType: "keystone_project"}))
	seed(t, dyn, KindProject, labelled(KindProject, "dizzyorder-run1-project", map[string]any{labelRun: "run1", labelType: "keystone_project"}))
	seed(t, dyn, KindProject, labelled(KindProject, "dizzyorder-run2-mislabelled", map[string]any{labelRun: "run9", labelType: "keystone_project"}))
	seed(t, dyn, KindProject, labelled(KindProject, "someone-else", map[string]any{"app": "x"}))

	found, err := c.ListByAnyRun(context.Background(), KindProject)
	if err != nil {
		t.Fatalf("ListByAnyRun: %v", err)
	}
	want := []resource.Resource{
		{Kind: KindProject, Logical: "project", Name: "dizzyorder-run0-project", ID: "dizzy-orders/dizzyorder-run0-project"},
		{Kind: KindProject, Logical: "project", Name: "dizzyorder-run1-project", ID: "dizzy-orders/dizzyorder-run1-project"},
		{Kind: KindProject, Logical: "", Name: "dizzyorder-run2-mislabelled", ID: "dizzy-orders/dizzyorder-run2-mislabelled"},
	}
	if !slices.Equal(found, want) {
		t.Errorf("ListByAnyRun = %+v\nwant            %+v", found, want)
	}
	if sel := dyn.Actions()[0].(k8stesting.ListActionImpl).ListOptions.LabelSelector; sel != "dizzy/type" {
		t.Errorf("selector = %q, want dizzy/type", sel)
	}
}

// TestListRefusesUnknownKindAndWrapsErrors confirms an unknown kind is refused
// without a request and a failed list is returned wrapped.
func TestListRefusesUnknownKindAndWrapsErrors(t *testing.T) {
	c, dyn, _ := testClient()
	if _, err := c.ListByRun(context.Background(), "volume", testRun); err == nil {
		t.Error("ListByRun(volume) = nil, want an error")
	}
	if _, err := c.ListByAnyRun(context.Background(), KindSecret); err == nil {
		t.Error("ListByAnyRun(secret) = nil, want an error")
	}
	if got := len(dyn.Actions()); got != 0 {
		t.Errorf("requests = %d, want 0", got)
	}
	failVerb(dyn, "list", "keystoneusers", forbidden("keystoneusers", ""))
	if _, err := c.ListByRun(context.Background(), KindUser, testRun); !apierrors.IsForbidden(err) {
		t.Errorf("ListByRun = %v, want the 403", err)
	}
}

// TestObserve confirms Observe reports Ready, a reason, Pending, and a 404 as
// gone, each as one get sample.
func TestObserve(t *testing.T) {
	r := orderRef(KindRoleAssignment, "role")
	for _, tc := range []struct {
		name       string
		answer     any
		want       string
		wantExists bool
	}{
		{"ready", readyObject(r, nil), "Ready", true},
		{"reason", pendingObject(r, "RoleNotAllowed"), "RoleNotAllowed", true},
		{"stale ready", withStatus(orderObject(r, 2), 1, nil, cond("Ready", "True", "AllReady")), "Pending", true},
		{"no status", orderObject(r, 1), "Pending", true},
		{"gone", notFound("keystoneroleassignments", r.Name), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, dyn, m := testClient()
			script(t, dyn, "keystoneroleassignments", tc.answer)
			got, exists, err := c.Observe(context.Background(), r)
			if got != tc.want || exists != tc.wantExists || err != nil {
				t.Errorf("Observe = (%q, %v, %v), want (%q, %v, nil)", got, exists, err, tc.want, tc.wantExists)
			}
			if st := statsOf(m.Aggregate(time.Second), r.Kind); st.Attempted != 1 {
				t.Errorf("samples = %+v, want 1", st)
			}
		})
	}
	t.Run("500 is an error", func(t *testing.T) {
		c, dyn, _ := testClient()
		script(t, dyn, "keystoneroleassignments", internalError())
		if _, exists, err := c.Observe(context.Background(), r); exists || !apierrors.IsInternalError(err) {
			t.Errorf("Observe = (%v, %v), want (false, the 500)", exists, err)
		}
	})
}
