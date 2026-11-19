package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/B42Labs/dizzy/internal/c5c3"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
)

// c5c3SweepListings are the five listings of one pre-flight sweep, by the
// presence of the dizzy/type label, in the order Cleanup deletes the kinds.
var c5c3SweepListings = []string{
	"list keystoneapplicationcredentials labelSelector=dizzy/type",
	"list keystoneroleassignments labelSelector=dizzy/type",
	"list keystonecatalogentries labelSelector=dizzy/type",
	"list keystoneprojects labelSelector=dizzy/type",
	"list keystoneusers labelSelector=dizzy/type",
}

// c5c3PrecheckListings are the five listings of one pre-check, each with a
// limit of 1, in the order Precheck lists the kinds.
var c5c3PrecheckListings = []string{
	"list keystoneusers limit=1",
	"list keystoneprojects limit=1",
	"list keystoneroleassignments limit=1",
	"list keystonecatalogentries limit=1",
	"list keystoneapplicationcredentials limit=1",
}

// c5c3CleanupListings are the five listings of the cleanup of run0, by its
// dizzy/run label, in the order Cleanup deletes the kinds.
var c5c3CleanupListings = []string{
	"list keystoneapplicationcredentials labelSelector=dizzy/run=run0",
	"list keystoneroleassignments labelSelector=dizzy/run=run0",
	"list keystonecatalogentries labelSelector=dizzy/run=run0",
	"list keystoneprojects labelSelector=dizzy/run=run0",
	"list keystoneusers labelSelector=dizzy/run=run0",
}

// c5c3TestDeps returns the iteration phases for run0 over dyn, built as
// c5c3MonitorRunOnce builds them, with the bundled plan's waits shortened.
// The pre-check and the create of stage 1 run one at a time, so the order of
// the requests the fake receives is fixed.
func c5c3TestDeps(t *testing.T, dyn *fake.FakeDynamicClient) iterationDeps {
	t.Helper()
	return c5c3IterationDeps(c5c3Cluster{dyn: dyn}, c5c3ShortPlan(t), "run0", metrics.NewCollector(), nil, 1, time.Second)
}

// c5c3SecretsGVR addresses Secrets.
var c5c3SecretsGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// rejectingKeystone starts a Keystone that answers every login with 401 and
// returns its auth URL and a function that reports how many logins it
// answered.
func rejectingKeystone(t *testing.T) (authURL string, logins func() int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v3/auth/tokens" {
			t.Errorf("Keystone got %s %s, want POST /v3/auth/tokens", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":401,"message":"The request you have made requires authentication.","title":"Unauthorized"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v3", n.Load
}

// seedC5C3UserWithSecret stores runID's user order in dyn, labelled as
// CreateOrder labels it, and the Secret the operator delivers beside it, whose
// clouds.yaml logs in at authURL with a password, and returns the order.
func seedC5C3UserWithSecret(t *testing.T, dyn *fake.FakeDynamicClient, runID, authURL string) resource.Resource {
	t.Helper()
	name := "dizzyorder-" + runID + "-user"
	order := seedC5C3Order(t, dyn, c5c3.KindUser, name, map[string]any{"dizzy/run": runID, "dizzy/type": string(c5c3.KindUser)})
	password := "pw-" + runID
	doc := fmt.Sprintf("clouds:\n  admin:\n    auth:\n      auth_url: %q\n      username: %q\n      password: %q\n      user_domain_name: Default\n    identity_api_version: 3\n",
		authURL, name, password)
	secret := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name + "-credentials", "namespace": c5c3TestNamespace, "resourceVersion": "1"},
		"type":       "Opaque",
		"data": map[string]any{
			"clouds.yaml": base64.StdEncoding.EncodeToString([]byte(doc)),
			"password":    base64.StdEncoding.EncodeToString([]byte(password)),
		},
	}}
	if err := dyn.Tracker().Create(c5c3SecretsGVR, secret, c5c3TestNamespace); err != nil {
		t.Fatalf("seeding the secret of %s: %v", name, err)
	}
	return order
}

// TestC5C3SweepHasItsOwnPasswordLoginLimit confirms the pre-flight sweep logs
// in through a client of its own. The sweep deletes the user orders five
// other runs left and sends one login per user that Keystone rejects, which
// is the limit of rejected password logins one client takes. The iteration's
// cleanup then still sends the login that checks its own deleted user's
// password is rejected.
func TestC5C3SweepHasItsOwnPasswordLoginLimit(t *testing.T) {
	authURL, logins := rejectingKeystone(t)
	dyn := newC5C3FakeDynamic()
	// The garbage collector deletes a user order's Secret with the order.
	dyn.PrependReactor("delete", "keystoneusers", func(a k8stesting.Action) (bool, runtime.Object, error) {
		_ = dyn.Tracker().Delete(c5c3SecretsGVR, a.GetNamespace(), a.(k8stesting.DeleteAction).GetName()+"-credentials")
		return false, nil, nil
	})
	for i := range 5 {
		seedC5C3UserWithSecret(t, dyn, fmt.Sprintf("left%04d", i), authURL)
	}
	deps := c5c3TestDeps(t, dyn)
	ctx := context.Background()

	swept, err := deps.preflight(ctx)
	if err != nil || swept != 5 {
		t.Fatalf("preflight = %d, %v; want the five leftover users swept", swept, err)
	}
	if got := logins(); got != 5 {
		t.Fatalf("the sweep sent %d logins, want one rejected login per leftover user, 5", got)
	}

	// The iteration's own user, as an apply that failed after stage 1 leaves it.
	own := seedC5C3UserWithSecret(t, dyn, "run0", authURL)
	deleted, err := deps.cleanup(ctx, []resource.Resource{own})
	if err != nil || deleted != 1 {
		t.Errorf("cleanup = %d, %v; want the iteration's user deleted and its password rejected", deleted, err)
	}
	if got := logins(); got != 6 {
		t.Errorf("Keystone answered %d logins, want 6: the cleanup's password login was not sent after the sweep's "+
			"five rejected logins", got)
	}
}

// TestC5C3MonitorIterationAgainstAPI runs one c5c3 monitor iteration, the
// production closure included, against an API server that stores the orders
// it is sent and never makes them ready. With --concurrency 1, stage 1 creates
// the user order, polls it until timeouts.ready and fails. The requests come
// in the iteration's order: the startup pre-check, the sweep by the dizzy/type
// label, the iteration's pre-check, the create and its polls, and the cleanup
// by dizzy/run=<id>, which deletes the created order although the listings
// stay empty. The iteration record lists the created order.
func TestC5C3MonitorIterationAgainstAPI(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	api := &fakeKubeAPI{store: true}
	kubeconfig := serveKubeAPI(t, api)
	path := shippedC5C3Profile(t)
	t.Chdir(t.TempDir())

	if _, err := execRoot(t, "c5c3", "monitor", "--scenario", path, "--set", "timeouts.ready=1s", "--iterations", "1",
		"--keep-run-records", "--concurrency", "1", "--kubeconfig", kubeconfig); err != nil {
		t.Fatalf("c5c3 monitor: %v", err)
	}

	records, _ := filepath.Glob("run-*.json")
	if len(records) != 1 {
		t.Fatalf("monitor wrote %v, want one iteration record", records)
	}
	rec, err := run.Load(records[0])
	if err != nil {
		t.Fatalf("loading the iteration record: %v", err)
	}
	user := "dizzyorder-" + rec.RunID + "-user"
	want := []resource.Resource{{Kind: c5c3.KindUser, Logical: "user", Name: user, ID: c5c3TestNamespace + "/" + user}}
	if rec.Service != "c5c3" || rec.Seed != 0 || !slices.Equal(rec.Created, want) {
		t.Errorf("record = service %q, seed %d, created %+v; want c5c3, 0, %+v", rec.Service, rec.Seed, rec.Created, want)
	}
	if !strings.HasPrefix(rec.Error, "apply: stage 1 (user and project): ") {
		t.Errorf("record error = %q, want stage 1's failure", rec.Error)
	}

	const base = "/apis/c5c3.io/v1alpha1/namespaces/" + c5c3TestNamespace + "/"
	listings := func(query string) []string {
		var out []string
		for _, res := range []string{"keystoneapplicationcredentials", "keystoneroleassignments", "keystonecatalogentries", "keystoneprojects", "keystoneusers"} {
			out = append(out, "GET "+base+res+"?labelSelector="+query)
		}
		return out
	}
	getUser := "GET " + base + "keystoneusers/" + user
	head := slices.Concat(c5c3PrecheckRequests(), listings("dizzy%2Ftype"), c5c3PrecheckRequests(),
		[]string{"POST " + base + "keystoneusers"})
	got := api.log()
	if len(got) < len(head) || !slices.Equal(got[:len(head)], head) {
		t.Fatalf("requests = %q, want them to start %q", got, head)
	}
	rest := got[len(head):]
	polls := 0
	for polls < len(rest) && rest[polls] == getUser {
		polls++
	}
	if polls == 0 {
		t.Errorf("requests = %q; the apply did not poll the created user order", got)
	}
	rest = rest[polls:]
	cleanup := listings("dizzy%2Frun%3D" + rec.RunID)
	if len(rest) < len(cleanup) || !slices.Equal(rest[:len(cleanup)], cleanup) {
		t.Fatalf("requests after the polls = %q, want the cleanup's listings %q first", rest, cleanup)
	}
	wantTail := []string{
		getUser, // the status the password generation wait reads
		"GET /api/v1/namespaces/" + c5c3TestNamespace + "/secrets/" + user + "-credentials",
		"DELETE " + base + "keystoneusers/" + user,
		getUser, // the wait for the order to be gone
	}
	if tail := rest[len(cleanup):]; !slices.Equal(tail, wantTail) {
		t.Errorf("cleanup requests after its listings = %q, want %q", tail, wantTail)
	}
}

// listOptionsOf returns the label selector and the limit of a list action.
func listOptionsOf(a k8stesting.Action) (selector string, limit int64) {
	l, ok := a.(k8stesting.ListActionImpl)
	if !ok {
		return "", 0
	}
	return l.GetListOptions().LabelSelector, l.GetListOptions().Limit
}

func TestC5C3MonitorFlags(t *testing.T) {
	c5c3Cmd := findSubcommand(newRootCmd(), "c5c3")
	if c5c3Cmd == nil {
		t.Fatal("c5c3 command not registered on root")
	}
	monitor := findSubcommand(c5c3Cmd, "monitor")
	if monitor == nil {
		t.Fatal("c5c3 monitor not registered")
	}
	for name, want := range map[string]string{
		"scenario":         "",
		"set":              "[]",
		"interval":         "0s",
		"iterations":       "0",
		"error-wait":       "0s",
		"keep-run-records": "false",
		"kubeconfig":       "",
		"kube-context":     "",
	} {
		f := monitor.Flags().Lookup(name)
		if f == nil {
			t.Errorf("c5c3 monitor has no --%s flag", name)
			continue
		}
		if f.DefValue != want {
			t.Errorf("--%s default = %q, want %q", name, f.DefValue, want)
		}
	}
	if monitor.Flags().Lookup("reclaim-orphans") != nil {
		t.Error("c5c3 monitor has a --reclaim-orphans flag; its sweep is always on")
	}
	if !strings.Contains(monitor.Long, "one dizzy process per Kubernetes namespace") {
		t.Errorf("c5c3 monitor Long does not say to run one process per namespace:\n%s", monitor.Long)
	}
}

func TestC5C3MonitorRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "c5c3", "monitor", "--interval", "1m"); err == nil || !strings.Contains(err.Error(), `"scenario" not set`) {
		t.Errorf("c5c3 monitor without --scenario: err = %v, want the required-flag error", err)
	}
}

// TestC5C3MonitorStartupPrecheckAborts confirms the monitor runs the pre-check
// once at startup, so a cluster that does not serve keystoneusers fails it
// before the first iteration sweeps anything.
func TestC5C3MonitorStartupPrecheckAborts(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	api := &fakeKubeAPI{notServed: map[string]bool{"keystoneusers": true}}
	kubeconfig := serveKubeAPI(t, api)
	path := shippedC5C3Profile(t)

	_, err := execRoot(t, "c5c3", "monitor", "--scenario", path, "--iterations", "1", "--kubeconfig", kubeconfig)
	if !errors.Is(err, c5c3.ErrKindNotServed) {
		t.Errorf("c5c3 monitor = %v, want ErrKindNotServed", err)
	}
	want := []string{"GET /apis/c5c3.io/v1alpha1/namespaces/dizzy-orders/keystoneusers?limit=1"}
	if got := api.log(); !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q (no iteration)", got, want)
	}
}

// TestC5C3MonitorNoAuthURLFailsBeforeKubernetes confirms the monitor resolves
// the Keystone endpoint at startup, so an entry without auth_url fails before
// any Kubernetes request.
func TestC5C3MonitorNoAuthURLFailsBeforeKubernetes(t *testing.T) {
	writeC5C3Clouds(t, c5c3NoAuthURLClouds)
	kubeconfig := unreachableKubeconfig(t)
	path := shippedC5C3Profile(t)

	_, err := execRoot(t, "c5c3", "monitor", "--os-cloud", "noauth", "--scenario", path, "--iterations", "1", "--kubeconfig", kubeconfig)
	if want := `clouds.yaml entry "noauth" has no auth_url`; err == nil || err.Error() != want {
		t.Errorf("c5c3 monitor = %v, want %q", err, want)
	}
}

// TestC5C3IterationRequestOrder confirms the order of one iteration's
// requests: the sweep lists every kind by the dizzy/type label, the pre-check
// lists every kind with a limit before the first create, the apply's first
// create follows, and the cleanup lists every kind by dizzy/run=run0. The fake
// refuses every create, so the iteration needs no Keystone.
func TestC5C3IterationRequestOrder(t *testing.T) {
	dyn := newC5C3FakeDynamic()
	dyn.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "c5c3.io", Resource: a.GetResource().Resource}, "",
			errors.New("the test refuses every create"))
	})

	res := runIteration(context.Background(), c5c3TestDeps(t, dyn))

	if res.ok || res.err == nil || !strings.HasPrefix(res.err.Error(), "apply: stage 1 (user and project): ") || !apierrors.IsForbidden(res.err) {
		t.Errorf("iteration = ok %v, err %v; want a failed iteration with stage 1's forbidden create", res.ok, res.err)
	}
	if len(res.created) != 0 || res.swept != 0 || res.deleted != 0 {
		t.Errorf("iteration created %v, swept %d, deleted %d; want nothing", res.created, res.swept, res.deleted)
	}
	want := slices.Concat(c5c3SweepListings, c5c3PrecheckListings, []string{"create keystoneusers"}, c5c3CleanupListings)
	if got := c5c3RequestLog(dyn); !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q", got, want)
	}
}

// TestC5C3IterationPrecheckAbortsBeforeCreate confirms an iteration's apply
// runs the pre-check after the sweep and, when the cluster does not serve
// keystoneusers, fails with ErrKindNotServed before any create, still running
// its cleanup. The sweep's listings are answered, so the iteration reaches its
// apply; only the pre-check's listing, the one with a limit, is refused.
func TestC5C3IterationPrecheckAbortsBeforeCreate(t *testing.T) {
	dyn := newC5C3FakeDynamic()
	dyn.PrependReactor("list", "keystoneusers", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if _, limit := listOptionsOf(a); limit == 1 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "c5c3.io", Resource: "keystoneusers"}, "")
		}
		return false, nil, nil
	})

	res := runIteration(context.Background(), c5c3TestDeps(t, dyn))

	if res.ok || !errors.Is(res.err, c5c3.ErrKindNotServed) || !strings.HasPrefix(res.err.Error(), "apply: ") {
		t.Errorf("iteration = ok %v, err %v; want a failed apply wrapping ErrKindNotServed", res.ok, res.err)
	}
	if len(res.created) != 0 {
		t.Errorf("created = %v, want nothing", res.created)
	}
	want := slices.Concat(c5c3SweepListings, c5c3PrecheckListings[:1], c5c3CleanupListings)
	if got := c5c3RequestLog(dyn); !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q (sweep, refused pre-check, cleanup; no create)", got, want)
	}
}

// TestC5C3PreflightSweepsAnyRun confirms the sweep deletes the orders of other
// runs that carry the dizzy/type label, in the order the operator's holds
// require, and leaves an object without the labels alone.
func TestC5C3PreflightSweepsAnyRun(t *testing.T) {
	dyn := newC5C3FakeDynamic()
	appcred := seedC5C3Order(t, dyn, c5c3.KindApplicationCredential, "dizzyorder-run9-appcred",
		map[string]any{"dizzy/run": "run9", "dizzy/type": string(c5c3.KindApplicationCredential)})
	project := seedC5C3Order(t, dyn, c5c3.KindProject, "dizzyorder-run8-project",
		map[string]any{"dizzy/run": "run8", "dizzy/type": string(c5c3.KindProject)})
	user := seedC5C3Order(t, dyn, c5c3.KindUser, "dizzyorder-run9-user",
		map[string]any{"dizzy/run": "run9", "dizzy/type": string(c5c3.KindUser)})
	unrelated := seedC5C3Order(t, dyn, c5c3.KindProject, "unrelated", nil)

	swept, err := c5c3TestDeps(t, dyn).preflight(context.Background())
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if swept != 3 {
		t.Errorf("swept = %d, want 3", swept)
	}
	for _, r := range []struct {
		order   string
		present bool
	}{
		{appcred.ID, c5c3Stored(t, dyn, appcred)},
		{project.ID, c5c3Stored(t, dyn, project)},
		{user.ID, c5c3Stored(t, dyn, user)},
	} {
		if r.present {
			t.Errorf("order %s of another run is still present after the sweep", r.order)
		}
	}
	if !c5c3Stored(t, dyn, unrelated) {
		t.Error("the sweep deleted an object without the dizzy labels")
	}

	var deletes []string
	for _, r := range c5c3RequestLog(dyn) {
		if strings.HasPrefix(r, "delete ") {
			deletes = append(deletes, r)
		}
	}
	want := []string{
		"delete keystoneapplicationcredentials dizzyorder-run9-appcred",
		"delete keystoneprojects dizzyorder-run8-project",
		"delete keystoneusers dizzyorder-run9-user",
	}
	if !slices.Equal(deletes, want) {
		t.Errorf("deletes = %q, want %q", deletes, want)
	}
}

// TestC5C3IterationSweepFailureSkipsApply confirms a failed sweep fails the
// iteration, skips the apply (no pre-check, no create), and still runs the
// cleanup of the iteration's run.
func TestC5C3IterationSweepFailureSkipsApply(t *testing.T) {
	dyn := newC5C3FakeDynamic()
	dyn.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if selector, _ := listOptionsOf(a); selector == "dizzy/type" {
			return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
		}
		return false, nil, nil
	})

	res := runIteration(context.Background(), c5c3TestDeps(t, dyn))

	const want = "pre-flight cleanup: listing keystone_application_credential orders of any run:"
	if res.ok || res.err == nil || !strings.HasPrefix(res.err.Error(), want) {
		t.Errorf("iteration = ok %v, err %v; want a failed iteration starting %q", res.ok, res.err, want)
	}
	wantLog := slices.Concat(c5c3SweepListings[:1], c5c3CleanupListings)
	if got := c5c3RequestLog(dyn); !slices.Equal(got, wantLog) {
		t.Errorf("requests = %q, want %q (the sweep's failed listing, then the cleanup's)", got, wantLog)
	}
}
