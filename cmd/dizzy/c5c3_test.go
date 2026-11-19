package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/B42Labs/dizzy/internal/c5c3"
	c5c3plan "github.com/B42Labs/dizzy/internal/c5c3/plan"
	c5c3scenario "github.com/B42Labs/dizzy/internal/c5c3/scenario"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/scenarios"
)

// c5c3GoldenPlan is the committed plan of the bundled small profile, relative
// to this package's directory.
const c5c3GoldenPlan = "../../internal/c5c3/scenario/testdata/golden/small.plan.json"

// c5c3TestNamespace is the namespace of the bundled profile, which the test
// orders live in.
const c5c3TestNamespace = "dizzy-orders"

// c5c3OrderResources lists the five order kinds with the resource and the
// Kubernetes kind each is served under, in the order Precheck lists them.
var c5c3OrderResources = []struct {
	kind     resource.Kind
	resource string
	k8sKind  string
}{
	{c5c3.KindUser, "keystoneusers", "KeystoneUser"},
	{c5c3.KindProject, "keystoneprojects", "KeystoneProject"},
	{c5c3.KindRoleAssignment, "keystoneroleassignments", "KeystoneRoleAssignment"},
	{c5c3.KindCatalogEntry, "keystonecatalogentries", "KeystoneCatalogEntry"},
	{c5c3.KindApplicationCredential, "keystoneapplicationcredentials", "KeystoneApplicationCredential"},
}

// c5c3ResourceOf returns the resource and the Kubernetes kind orders of kind
// are served under.
func c5c3ResourceOf(t *testing.T, kind resource.Kind) (res, k8sKind string) {
	t.Helper()
	for _, r := range c5c3OrderResources {
		if r.kind == kind {
			return r.resource, r.k8sKind
		}
	}
	t.Fatalf("no c5c3 order resource for kind %q", kind)
	return "", ""
}

// c5c3GVR addresses one of the order resources of c5c3.io/v1alpha1.
func c5c3GVR(res string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "c5c3.io", Version: "v1alpha1", Resource: res}
}

// newC5C3FakeDynamic returns a fake dynamic client that serves the five order
// kinds and Secrets, listings included.
func newC5C3FakeDynamic() *fake.FakeDynamicClient {
	listKinds := map[schema.GroupVersionResource]string{{Version: "v1", Resource: "secrets"}: "SecretList"}
	for _, r := range c5c3OrderResources {
		listKinds[c5c3GVR(r.resource)] = r.k8sKind + "List"
	}
	return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
}

// seedC5C3Order stores an order of kind named name in c5c3TestNamespace with
// labels, without a status, in the fake's tracker without recording an
// action, and returns the resource it is recorded as. The logical name is the
// part of name after the run prefix the dizzy/run label names, if any.
func seedC5C3Order(t *testing.T, dyn *fake.FakeDynamicClient, kind resource.Kind, name string, labels map[string]any) resource.Resource {
	t.Helper()
	res, k8sKind := c5c3ResourceOf(t, kind)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "c5c3.io/v1alpha1",
		"kind":       k8sKind,
		"metadata": map[string]any{
			"name":       name,
			"namespace":  c5c3TestNamespace,
			"generation": int64(1),
			"labels":     labels,
		},
		"spec": map[string]any{"controlPlaneRef": map[string]any{"name": "controlplane"}},
	}}
	if err := dyn.Tracker().Create(c5c3GVR(res), obj, c5c3TestNamespace); err != nil {
		t.Fatalf("seeding %s %s: %v", kind, name, err)
	}
	logical := ""
	if runID, ok := labels["dizzy/run"].(string); ok {
		logical = strings.TrimPrefix(name, "dizzyorder-"+runID+"-")
	}
	return resource.Resource{Kind: kind, Logical: logical, Name: name, ID: c5c3TestNamespace + "/" + name}
}

// seedC5C3RunOrders stores the five orders of the bundled profile for runID,
// labelled as CreateOrder labels them, and returns them in creation order.
func seedC5C3RunOrders(t *testing.T, dyn *fake.FakeDynamicClient, runID string) []resource.Resource {
	t.Helper()
	var created []resource.Resource
	for _, o := range c5c3SmallPlan(t).Orders {
		created = append(created, seedC5C3Order(t, dyn, resource.Kind(o.Kind), "dizzyorder-"+runID+"-"+o.Name,
			map[string]any{"dizzy/run": runID, "dizzy/type": o.Kind}))
	}
	return created
}

// c5c3Stored reports whether the fake's tracker still holds the order r
// addresses.
func c5c3Stored(t *testing.T, dyn *fake.FakeDynamicClient, r resource.Resource) bool {
	t.Helper()
	res, _ := c5c3ResourceOf(t, r.Kind)
	_, err := dyn.Tracker().Get(c5c3GVR(res), c5c3TestNamespace, r.Name)
	return err == nil
}

// c5c3RequestLog renders the requests the fake received, in order, as
// "<verb> <resource>" followed by a listing's label selector and limit, or by
// the name a get or a delete addresses.
func c5c3RequestLog(dyn *fake.FakeDynamicClient) []string {
	var out []string
	for _, a := range dyn.Actions() {
		s := a.GetVerb() + " " + a.GetResource().Resource
		switch a := a.(type) {
		case k8stesting.ListActionImpl:
			opts := a.GetListOptions()
			if opts.LabelSelector != "" {
				s += " labelSelector=" + opts.LabelSelector
			}
			if opts.Limit != 0 {
				s += fmt.Sprintf(" limit=%d", opts.Limit)
			}
		case k8stesting.GetActionImpl:
			s += " " + a.GetName()
		case k8stesting.DeleteActionImpl:
			s += " " + a.GetName()
		}
		out = append(out, s)
	}
	return out
}

// c5c3SmallPlan expands the bundled small profile.
func c5c3SmallPlan(t *testing.T) *c5c3plan.Plan {
	t.Helper()
	data, err := scenarios.Files.ReadFile("c5c3/small.yaml")
	if err != nil {
		t.Fatalf("reading shipped profile: %v", err)
	}
	s, err := c5c3scenario.Parse(data)
	if err != nil {
		t.Fatalf("parsing shipped profile: %v", err)
	}
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("generating shipped profile: %v", err)
	}
	return p
}

// c5c3ShortPlan is the bundled small plan with waits short enough that a test
// that goes wrong fails within seconds instead of hanging for 15 minutes.
func c5c3ShortPlan(t *testing.T) *c5c3plan.Plan {
	t.Helper()
	p := c5c3SmallPlan(t)
	p.ReadyTimeout, p.GoneTimeout = 2*time.Second, 2*time.Second
	return p
}

// shippedC5C3Profile copies the shipped scenarios/c5c3/small.yaml profile into
// a temporary file and returns its path.
func shippedC5C3Profile(t *testing.T) string {
	t.Helper()
	data, err := scenarios.Files.ReadFile("c5c3/small.yaml")
	if err != nil {
		t.Fatalf("reading shipped profile: %v", err)
	}
	return writeScenario(t, string(data))
}

// writeC5C3Clouds writes a clouds.yaml holding body under "clouds:" and points
// OS_CLIENT_CONFIG_FILE at it, with OS_CLOUD empty.
func writeC5C3Clouds(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clouds.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing clouds.yaml: %v", err)
	}
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", path)
}

// c5c3NoAuthURLClouds is a clouds.yaml whose entry noauth has credentials but
// no auth_url.
const c5c3NoAuthURLClouds = `clouds:
  noauth:
    auth:
      username: dizzy
      password: s3cr3t-pw
      project_name: p
      user_domain_name: Default
`

// writeKubeconfig writes a kubeconfig whose one context points at server and
// returns its path.
func writeKubeconfig(t *testing.T, server string) string {
	t.Helper()
	body := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster:
    server: %s
contexts:
- name: fake
  context:
    cluster: fake
    user: fake
current-context: fake
users:
- name: fake
  user: {}
`, server)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}

// fakeKubeAPI is a Kubernetes API server for the command tests that logs every
// request as "<method> <path>[?<query>]". A GET of an object in objects
// (keyed by path) answers that object, and a GET of a collection answers an
// empty list unless notServed holds its resource. Every other request answers
// 404 with a Status, as an API server does for an object that does not exist
// and for a resource it does not serve.
type fakeKubeAPI struct {
	objects   map[string]string
	notServed map[string]bool
	// store makes the fake keep the orders it is sent: a POST to an order
	// collection stores the object it carries and answers it with 201, a GET
	// of a stored order answers it as it was sent, without a status, so it
	// never turns ready, and a DELETE removes it. Listings stay empty, so a
	// cleanup reaches a stored order only through the orders it was handed.
	store bool
	// listDelay delays the answer to every listing with a limit, as each list
	// of a pre-check is.
	listDelay time.Duration

	mu       sync.Mutex
	requests []string
	stored   map[string]string
}

func (f *fakeKubeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	entry := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		entry += "?" + r.URL.RawQuery
	}
	f.mu.Lock()
	f.requests = append(f.requests, entry)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if f.store && f.serveStored(w, r) {
		return
	}
	if body, ok := f.objects[r.URL.Path]; ok && r.Method == http.MethodGet {
		_, _ = io.WriteString(w, body)
		return
	}
	if apiVersion, listKind, res, ok := collection(r.URL.Path); ok && r.Method == http.MethodGet && !f.notServed[res] {
		if r.URL.Query().Has("limit") && f.listDelay > 0 {
			select {
			case <-time.After(f.listDelay):
			case <-r.Context().Done():
				return
			}
		}
		_, _ = fmt.Fprintf(w, `{"apiVersion":%q,"kind":%q,"metadata":{"resourceVersion":"1"},"items":[]}`, apiVersion, listKind)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","reason":"NotFound","code":404}`)
}

// serveStored answers r from the stored orders, as store describes, and
// reports whether it did.
func (f *fakeKubeAPI) serveStored(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPost:
		if _, _, res, ok := collection(r.URL.Path); !ok || res == "secrets" {
			return false
		}
		body, err := io.ReadAll(r.Body)
		var obj struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err == nil {
			err = json.Unmarshal(body, &obj)
		}
		if err != nil || obj.Metadata.Name == "" {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		if f.stored == nil {
			f.stored = map[string]string{}
		}
		f.stored[r.URL.Path+"/"+obj.Metadata.Name] = string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
		return true
	case http.MethodGet:
		body, ok := f.stored[r.URL.Path]
		if ok {
			_, _ = io.WriteString(w, body)
		}
		return ok
	case http.MethodDelete:
		if _, ok := f.stored[r.URL.Path]; !ok {
			return false
		}
		delete(f.stored, r.URL.Path)
		_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Success"}`)
		return true
	}
	return false
}

// collection reports whether path addresses a namespaced collection of an
// order resource or of Secrets, with the apiVersion and list kind it is served
// under and its resource name.
func collection(path string) (apiVersion, listKind, res string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(parts) == 5 && parts[0] == "api" && parts[2] == "namespaces" && parts[4] == "secrets":
		return "v1", "SecretList", "secrets", true
	case len(parts) == 6 && parts[0] == "apis" && parts[1] == "c5c3.io" && parts[3] == "namespaces":
		for _, r := range c5c3OrderResources {
			if r.resource == parts[5] {
				return "c5c3.io/v1alpha1", r.k8sKind + "List", r.resource, true
			}
		}
	}
	return "", "", "", false
}

// log returns the requests the fake received so far, in order.
func (f *fakeKubeAPI) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// serveKubeAPI serves f and returns the path of a kubeconfig pointing at it.
func serveKubeAPI(t *testing.T, f http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return writeKubeconfig(t, srv.URL)
}

// unreachableKubeconfig returns a kubeconfig whose server fails the test on
// any request, proving a command sends none.
func unreachableKubeconfig(t *testing.T) string {
	t.Helper()
	return serveKubeAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Kubernetes request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusInternalServerError)
	}))
}

// TestC5C3HelpListsSubcommands confirms dizzy c5c3 --help lists the six
// subcommands the namespace offers, and no chaos.
func TestC5C3HelpListsSubcommands(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"c5c3", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("c5c3 --help: %v", err)
	}
	listed := map[string]bool{}
	for _, line := range strings.Split(out.String(), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && strings.HasPrefix(line, "  ") {
			listed[fields[0]] = true
		}
	}
	for _, name := range []string{"generate", "apply", "monitor", "status", "report", "cleanup"} {
		if !listed[name] {
			t.Errorf("c5c3 --help does not list %q:\n%s", name, out.String())
		}
	}
	if listed["chaos"] {
		t.Errorf("c5c3 --help lists chaos; the namespace has none:\n%s", out.String())
	}
	if !strings.Contains(strings.Join(strings.Fields(newRootCmd().Long), " "), "dizzy c5c3 orders Keystone resources") {
		t.Errorf("root Long does not name the c5c3 namespace:\n%s", newRootCmd().Long)
	}
}

// TestC5C3GenerateSmallProfileIsDeterministic covers generating the shipped
// small profile twice: the JSON is byte-identical and equals the committed
// golden plan, and the global --seed changes nothing.
func TestC5C3GenerateSmallProfileIsDeterministic(t *testing.T) {
	path := shippedC5C3Profile(t)

	first, err := execRoot(t, "c5c3", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate (first): %v", err)
	}
	second, err := execRoot(t, "c5c3", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate (second): %v", err)
	}
	if first != second {
		t.Errorf("two generations of the small profile differ:\n%s\n---\n%s", first, second)
	}

	golden, err := os.ReadFile(c5c3GoldenPlan)
	if err != nil {
		t.Fatalf("reading golden plan: %v", err)
	}
	if first != string(golden) {
		t.Errorf("generated plan differs from %s:\ngot:\n%s\nwant:\n%s", c5c3GoldenPlan, first, golden)
	}

	seeded, err := execRoot(t, "c5c3", "generate", "--scenario", path, "--seed", "999")
	if err != nil {
		t.Fatalf("generate --seed: %v", err)
	}
	if seeded != first {
		t.Errorf("--seed changed the plan; it must have no effect:\n%s", seeded)
	}

	outPath := filepath.Join(t.TempDir(), "plan.json")
	if _, err := execRoot(t, "c5c3", "generate", "--scenario", path, "--out", outPath); err != nil {
		t.Fatalf("generate --out: %v", err)
	}
	written, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading written plan: %v", err)
	}
	if string(written) != string(golden) {
		t.Errorf("--out wrote a plan that differs from %s:\n%s", c5c3GoldenPlan, written)
	}
}

// TestC5C3GenerateSetOverride confirms --set role=reader puts reader on the
// role order and --set with an unknown key fails naming the key.
func TestC5C3GenerateSetOverride(t *testing.T) {
	path := shippedC5C3Profile(t)

	out, err := execRoot(t, "c5c3", "generate", "--scenario", path, "--set", "role=reader")
	if err != nil {
		t.Fatalf("generate --set role=reader: %v", err)
	}
	var p c5c3plan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	var roles []string
	for _, o := range p.Orders {
		if o.Name == "role" {
			roles = append(roles, o.Role)
		}
	}
	if !slices.Equal(roles, []string{"reader"}) {
		t.Errorf("role order roles = %q, want [reader]", roles)
	}

	_, err = execRoot(t, "c5c3", "generate", "--scenario", path, "--set", "nope=1")
	if err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("generate --set nope=1: err = %v, want an error naming \"nope\"", err)
	}
}

// TestC5C3ApplyDryRunSummaryNoAPICall confirms apply --dry-run prints exactly
// the plan summary and exits 0 with a kubeconfig and a clouds.yaml that do not
// exist, so it builds no Kubernetes client and reads no clouds.yaml.
func TestC5C3ApplyDryRunSummaryNoAPICall(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing-kubeconfig"))
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", filepath.Join(t.TempDir(), "missing-clouds.yaml"))
	path := shippedC5C3Profile(t)
	// Run from a fresh directory so a run record written to "." would show up.
	t.Chdir(t.TempDir())

	out, err := execRoot(t, "c5c3", "apply", "--scenario", path, "--dry-run", "--keep-on-abort")
	if err != nil {
		t.Fatalf("apply --dry-run: %v", err)
	}
	if want := c5c3SmallPlan(t).Summary(); out != want {
		t.Errorf("dry-run output is not the plan summary:\ngot:\n%s\nwant:\n%s", out, want)
	}
	if records, _ := filepath.Glob("run-*"); len(records) != 0 {
		t.Errorf("apply --dry-run wrote %v, want no run record", records)
	}
}

// TestC5C3ApplyNoAuthURLFailsBeforeKubernetes confirms apply with --os-cloud
// naming an entry without auth_url fails with config's message before it
// sends any Kubernetes request, and writes no run record.
func TestC5C3ApplyNoAuthURLFailsBeforeKubernetes(t *testing.T) {
	writeC5C3Clouds(t, c5c3NoAuthURLClouds)
	kubeconfig := unreachableKubeconfig(t)
	path := shippedC5C3Profile(t)
	t.Chdir(t.TempDir())

	_, err := execRoot(t, "c5c3", "apply", "--os-cloud", "noauth", "--scenario", path, "--kubeconfig", kubeconfig)
	const want = `clouds.yaml entry "noauth" has no auth_url`
	if err == nil || err.Error() != want {
		t.Errorf("apply = %v, want %q", err, want)
	}
	if records, _ := filepath.Glob("run-*"); len(records) != 0 {
		t.Errorf("apply that failed before any order wrote %v, want no run record", records)
	}
}

// TestC5C3ApplyPrecheckStopsBeforeCreate confirms apply against a cluster that
// does not serve keystoneusers stops at the pre-check with ErrKindNotServed:
// it sends nothing but that listing and writes no run record.
func TestC5C3ApplyPrecheckStopsBeforeCreate(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	api := &fakeKubeAPI{notServed: map[string]bool{"keystoneusers": true}}
	kubeconfig := serveKubeAPI(t, api)
	path := shippedC5C3Profile(t)
	t.Chdir(t.TempDir())

	_, err := execRoot(t, "c5c3", "apply", "--scenario", path, "--kubeconfig", kubeconfig)
	if !errors.Is(err, c5c3.ErrKindNotServed) || !strings.Contains(err.Error(), "keystoneusers.c5c3.io") {
		t.Errorf("apply = %v, want ErrKindNotServed naming keystoneusers.c5c3.io", err)
	}
	want := []string{"GET /apis/c5c3.io/v1alpha1/namespaces/dizzy-orders/keystoneusers?limit=1"}
	if got := api.log(); !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q", got, want)
	}
	if records, _ := filepath.Glob("run-*"); len(records) != 0 {
		t.Errorf("apply that failed its pre-check wrote %v, want no run record", records)
	}
}

// c5c3PrecheckRequests are the five listings of one pre-check in
// c5c3TestNamespace as fakeKubeAPI logs them, in the order Precheck sends them.
func c5c3PrecheckRequests() []string {
	var out []string
	for _, r := range c5c3OrderResources {
		out = append(out, "GET /apis/c5c3.io/v1alpha1/namespaces/"+c5c3TestNamespace+"/"+r.resource+"?limit=1")
	}
	return out
}

// TestC5C3ApplyPrecheckBoundsEachList confirms --timeout bounds each list of
// apply's pre-check on its own rather than the pre-check as a whole: five
// lists of 300ms each take longer than --timeout 1s together, and the
// pre-check still passes, so apply goes on to its first create.
func TestC5C3ApplyPrecheckBoundsEachList(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	api := &fakeKubeAPI{listDelay: 300 * time.Millisecond}
	kubeconfig := serveKubeAPI(t, api)
	path := shippedC5C3Profile(t)
	t.Chdir(t.TempDir())

	_, err := execRoot(t, "c5c3", "apply", "--timeout", "1s", "--scenario", path, "--kubeconfig", kubeconfig)

	// The fake answers each create 404, which fails stage 1.
	if err == nil || !strings.Contains(err.Error(), "stage 1 (user and project)") {
		t.Errorf("apply = %v, want a failure in stage 1, after the pre-check", err)
	}
	got := api.log()
	if want := c5c3PrecheckRequests(); len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("requests = %q, want the five pre-check lists %q first", got, want)
	}
	if !slices.Contains(got, "POST /apis/c5c3.io/v1alpha1/namespaces/dizzy-orders/keystoneusers") {
		t.Errorf("requests = %q, want the create of the user order after the pre-check", got)
	}
}

// TestC5C3ApplyRecordsCreatedOrders confirms the run record apply writes lists
// the orders Apply created. The fake API server stores the user and the project
// order stage 1 creates and never makes them ready, so the stage fails at
// timeouts.ready with both created; the record carries service c5c3, seed 0
// and both orders with their <namespace>/<name> ids, and apply leaves them in
// place.
func TestC5C3ApplyRecordsCreatedOrders(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	api := &fakeKubeAPI{store: true}
	kubeconfig := serveKubeAPI(t, api)
	path := shippedC5C3Profile(t)
	t.Chdir(t.TempDir())

	out, err := execRoot(t, "c5c3", "apply", "--scenario", path, "--set", "timeouts.ready=1s", "--kubeconfig", kubeconfig)
	if err == nil || !strings.Contains(err.Error(), "stage 1 (user and project)") {
		t.Errorf("apply = %v, want a failure in stage 1", err)
	}

	records, _ := filepath.Glob("run-*.json")
	if len(records) != 1 {
		t.Fatalf("apply wrote %v, want one run record", records)
	}
	if !strings.Contains(out, "run record written to "+records[0]) {
		t.Errorf("output does not name the record %s:\n%s", records[0], out)
	}
	raw, err := os.ReadFile(records[0])
	if err != nil {
		t.Fatalf("reading record: %v", err)
	}
	for _, want := range []string{`"service": "c5c3"`, `"seed": 0`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("record lacks %s:\n%s", want, raw)
		}
	}
	rec, err := run.Load(records[0])
	if err != nil {
		t.Fatalf("loading record: %v", err)
	}
	var want []resource.Resource
	for _, o := range []struct {
		kind    resource.Kind
		logical string
	}{{c5c3.KindUser, "user"}, {c5c3.KindProject, "project"}} {
		name := "dizzyorder-" + rec.RunID + "-" + o.logical
		want = append(want, resource.Resource{Kind: o.kind, Logical: o.logical, Name: name, ID: c5c3TestNamespace + "/" + name})
	}
	if !slices.Equal(rec.Created, want) {
		t.Errorf("record lists %+v, want %+v", rec.Created, want)
	}
	if !strings.Contains(rec.Error, "stage 1 (user and project)") {
		t.Errorf("record error = %q, want stage 1's failure", rec.Error)
	}
	for _, r := range api.log() {
		if strings.HasPrefix(r, "DELETE ") {
			t.Errorf("apply sent %q; an apply that was not interrupted leaves its orders", r)
		}
	}
}

// TestC5C3RunRecordOfCompletedApply confirms the record of a completed apply
// carries service c5c3, seed 0 and the five orders the client created, each
// with its <namespace>/<name> id, and that it survives a write and a load.
func TestC5C3RunRecordOfCompletedApply(t *testing.T) {
	p := c5c3SmallPlan(t)
	client := c5c3.New(newC5C3FakeDynamic(), p.Namespace, "run0", metrics.NewCollector())
	client.SetPlan(p)
	var created []resource.Resource
	for _, o := range p.Orders {
		r, err := client.CreateOrder(context.Background(), o)
		if err != nil {
			t.Fatalf("CreateOrder %s: %v", o.Name, err)
		}
		created = append(created, r)
	}

	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	rec := c5c3RunRecord("run0", p, start, start.Add(3*time.Minute), created, metrics.Aggregate{}, nil)
	path, err := run.Write(t.TempDir(), rec)
	if err != nil {
		t.Fatalf("writing record: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading record: %v", err)
	}
	for _, want := range []string{`"service": "c5c3"`, `"seed": 0`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("record lacks %s:\n%s", want, raw)
		}
	}
	got, err := run.Load(path)
	if err != nil {
		t.Fatalf("loading record: %v", err)
	}
	if got.Service != "c5c3" || got.Seed != 0 || got.Scenario != "small" || got.Error != "" {
		t.Errorf("record = service %q, seed %d, scenario %q, error %q; want c5c3, 0, small and no error",
			got.Service, got.Seed, got.Scenario, got.Error)
	}
	if len(got.Created) != 5 {
		t.Fatalf("record lists %d orders, want 5: %+v", len(got.Created), got.Created)
	}
	for i, o := range p.Orders {
		r := got.Created[i]
		name := "dizzyorder-run0-" + o.Name
		if r.Kind != resource.Kind(o.Kind) || r.Logical != o.Name || r.Name != name || r.ID != "dizzy-orders/"+name {
			t.Errorf("created[%d] = %+v, want kind %s, logical %s, id dizzy-orders/%s", i, r, o.Kind, o.Name, name)
		}
	}

	failed := c5c3RunRecord("run0", p, start, start, created[:2], metrics.Aggregate{}, errors.New("stage 3: boom"))
	if failed.Error != "stage 3: boom" || len(failed.Created) != 2 {
		t.Errorf("failed record = error %q with %d orders, want the error and 2 orders", failed.Error, len(failed.Created))
	}
}

// TestC5C3CleanupRequiresRunOrRunID confirms cleanup needs exactly one of
// --run and --run-id.
func TestC5C3CleanupRequiresRunOrRunID(t *testing.T) {
	const want = "exactly one of --run or --run-id is required"
	if _, err := execRoot(t, "c5c3", "cleanup"); err == nil || err.Error() != want {
		t.Errorf("cleanup with neither: err = %v, want %q", err, want)
	}
	rec, err := run.Write(t.TempDir(), &run.Record{RunID: "c5c30001", Service: "c5c3"})
	if err != nil {
		t.Fatalf("seeding record: %v", err)
	}
	if _, err := execRoot(t, "c5c3", "cleanup", "--run", rec, "--run-id", "c5c30001"); err == nil || err.Error() != want {
		t.Errorf("cleanup with both: err = %v, want %q", err, want)
	}
}

// TestC5C3CleanupRunIDRequiresNamespace confirms cleanup by --run-id without
// --namespace, and by a record that lists no order, fails with the message of
// the namespace rule before it reads a clouds.yaml or a kubeconfig: both name
// files that do not exist, so either read would fail with another message.
func TestC5C3CleanupRunIDRequiresNamespace(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", filepath.Join(t.TempDir(), "missing-clouds.yaml"))
	kubeconfig := filepath.Join(t.TempDir(), "missing-kubeconfig")
	const want = "--namespace is required with --run-id, or with a run record that lists no order"

	if _, err := execRoot(t, "c5c3", "cleanup", "--os-cloud", "any", "--run-id", "c5c30001", "--kubeconfig", kubeconfig); err == nil || err.Error() != want {
		t.Errorf("cleanup --run-id: err = %v, want %q", err, want)
	}

	empty, err := run.Write(t.TempDir(), &run.Record{RunID: "c5c30002", Service: "c5c3"})
	if err != nil {
		t.Fatalf("seeding record: %v", err)
	}
	if _, err := execRoot(t, "c5c3", "cleanup", "--os-cloud", "any", "--run", empty, "--kubeconfig", kubeconfig); err == nil || err.Error() != want {
		t.Errorf("cleanup --run with an empty record: err = %v, want %q", err, want)
	}

	// With the namespace given, the same command reaches the clouds.yaml read,
	// which shows the two failures above came first.
	if _, err := execRoot(t, "c5c3", "cleanup", "--os-cloud", "any", "--run-id", "c5c30001", "--namespace", "orders-a", "--kubeconfig", kubeconfig); err == nil || err.Error() == want {
		t.Errorf("cleanup --run-id --namespace: err = %v, want the clouds.yaml error", err)
	}
}

// TestC5C3CleanupNamespaceFromRecord confirms the namespace rule: --namespace
// wins, a record supplies the namespace half of its first order id, and a
// cleanup by --run without --namespace sends every request to that namespace.
func TestC5C3CleanupNamespaceFromRecord(t *testing.T) {
	rec := &run.Record{RunID: "c5c30003", Service: "c5c3", Created: []resource.Resource{
		{Kind: c5c3.KindCatalogEntry, Logical: "catalog", Name: "dizzyorder-c5c30003-catalog", ID: "orders-a/dizzyorder-c5c30003-catalog"},
		{Kind: c5c3.KindUser, Logical: "user", Name: "dizzyorder-c5c30003-user", ID: "orders-b/dizzyorder-c5c30003-user"},
	}}
	tests := []struct {
		name      string
		namespace string
		rec       *run.Record
		want      string
		wantErr   string
	}{
		{"first created id", "", rec, "orders-a", ""},
		{"flag wins over the record", "override", rec, "override", ""},
		{"flag without a record", "override", nil, "override", ""},
		{"no record", "", nil, "", errC5C3CleanupNamespace.Error()},
		{"record without orders", "", &run.Record{RunID: "x"}, "", errC5C3CleanupNamespace.Error()},
		{"malformed first id", "", &run.Record{Created: []resource.Resource{{Kind: c5c3.KindUser, ID: "no-slash"}}}, "",
			`the run record's first order id "no-slash" is not <namespace>/<name>; pass --namespace`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c5c3CleanupNamespace(tc.namespace, tc.rec)
			switch {
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
				t.Errorf("c5c3CleanupNamespace = %q, %v; want error %q", got, err, tc.wantErr)
			case tc.wantErr == "" && (err != nil || got != tc.want):
				t.Errorf("c5c3CleanupNamespace = %q, %v; want %q", got, err, tc.want)
			}
		})
	}

	t.Run("requests", func(t *testing.T) {
		t.Setenv("OS_CLOUD", "")
		api := &fakeKubeAPI{}
		kubeconfig := serveKubeAPI(t, api)
		path, err := run.Write(t.TempDir(), &run.Record{RunID: "c5c30003", Service: "c5c3", Created: rec.Created[:1]})
		if err != nil {
			t.Fatalf("seeding record: %v", err)
		}

		out, err := execRoot(t, "c5c3", "cleanup", "--run", path, "--kubeconfig", kubeconfig)
		if err != nil {
			t.Fatalf("cleanup --run: %v", err)
		}
		if want := "deleted 1 resource(s) for run c5c30003\n"; out != want {
			t.Errorf("output = %q, want %q", out, want)
		}
		got := api.log()
		if len(got) == 0 {
			t.Fatal("cleanup sent no Kubernetes request")
		}
		for _, r := range got {
			if !strings.Contains(r, "/namespaces/orders-a/") {
				t.Errorf("request %q does not address the record's namespace orders-a", r)
			}
		}
		if !slices.Contains(got, "GET /apis/c5c3.io/v1alpha1/namespaces/orders-a/keystoneusers?labelSelector=dizzy%2Frun%3Dc5c30003") {
			t.Errorf("requests = %q, want a listing of keystoneusers by dizzy/run=c5c30003", got)
		}
		if !slices.Contains(got, "DELETE /apis/c5c3.io/v1alpha1/namespaces/orders-a/keystonecatalogentries/dizzyorder-c5c30003-catalog") {
			t.Errorf("requests = %q, want the delete of the recorded catalog entry", got)
		}
	})
}

// TestC5C3RejectsBarbicanRecord confirms the service guard stops c5c3 status
// and cleanup on a barbican record, whose kinds the c5c3 client cannot handle.
func TestC5C3RejectsBarbicanRecord(t *testing.T) {
	rec, err := run.Write(t.TempDir(), &run.Record{RunID: "bar00001", Service: "barbican", Created: []resource.Resource{
		{Kind: "secret", Logical: "sec-0001", ID: "s1"},
	}})
	if err != nil {
		t.Fatalf("seeding barbican record: %v", err)
	}
	const want = `run record is for service "barbican", not "c5c3"`
	for _, verb := range []string{"status", "cleanup"} {
		if _, err := execRoot(t, "c5c3", verb, "--run", rec); err == nil || err.Error() != want {
			t.Errorf("c5c3 %s on a barbican record: err = %v, want %q", verb, err, want)
		}
	}
}

// TestC5C3StatusRequiresRunFlag confirms status needs --run.
func TestC5C3StatusRequiresRunFlag(t *testing.T) {
	if _, err := execRoot(t, "c5c3", "status"); err == nil || !strings.Contains(err.Error(), `"run" not set`) {
		t.Errorf("status without --run: err = %v, want the required-flag error", err)
	}
}

// silentKubeconfig returns a kubeconfig whose server accepts every connection
// and never answers on it.
func silentKubeconfig(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return writeKubeconfig(t, "http://"+ln.Addr().String())
}

// execRootWithin runs the root command with args as execRoot does and fails
// the test when it has not returned after limit. The command keeps running in
// the background then, until the test's cleanup ends what it waits for.
func execRootWithin(t *testing.T, limit time.Duration, args ...string) (string, error) {
	t.Helper()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := execRoot(t, args...)
		done <- result{out, err}
	}()
	select {
	case res := <-done:
		return res.out, res.err
	case <-time.After(limit):
		t.Fatalf("%q had not returned after %s", args, limit)
		return "", nil
	}
}

// TestC5C3StatusTimeoutBoundsEachGet confirms --timeout bounds each GET of
// status: against an API server that accepts the connection and never
// answers, status with --timeout 1s fails the order's row and returns within
// a few seconds instead of hanging.
func TestC5C3StatusTimeoutBoundsEachGet(t *testing.T) {
	kubeconfig := silentKubeconfig(t)
	name := "dizzyorder-c5c30006-user"
	path, err := run.Write(t.TempDir(), &run.Record{RunID: "c5c30006", Service: "c5c3", Created: []resource.Resource{
		{Kind: c5c3.KindUser, Logical: "user", Name: name, ID: c5c3TestNamespace + "/" + name},
	}})
	if err != nil {
		t.Fatalf("seeding record: %v", err)
	}

	out, err := execRootWithin(t, 10*time.Second, "c5c3", "status", "--timeout", "1s", "--run", path, "--kubeconfig", kubeconfig)
	const want = "re-querying 1 of 1 resources failed"
	if err == nil || err.Error() != want {
		t.Errorf("status = %v, want %q", err, want)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("status table does not show the failed row as error:\n%s", out)
	}
}

// TestC5C3PrecheckTimeoutAgainstSilentServer confirms apply and monitor bound
// the lists of their pre-check by --timeout: against an API server that
// accepts the connection and never answers, each fails at its first list
// with the deadline within a few seconds instead of hanging.
func TestC5C3PrecheckTimeoutAgainstSilentServer(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	path := shippedC5C3Profile(t)
	for _, verb := range [][]string{{"apply"}, {"monitor", "--iterations", "1"}} {
		t.Run(verb[0], func(t *testing.T) {
			kubeconfig := silentKubeconfig(t)
			t.Chdir(t.TempDir())
			args := slices.Concat([]string{"c5c3"}, verb, []string{"--timeout", "1s", "--scenario", path, "--kubeconfig", kubeconfig})
			_, err := execRootWithin(t, 10*time.Second, args...)
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "keystoneusers") {
				t.Errorf("c5c3 %s = %v, want the deadline of the pre-check's list of keystoneusers", verb[0], err)
			}
			if records, _ := filepath.Glob("run-*"); len(records) != 0 {
				t.Errorf("c5c3 %s that failed its pre-check wrote %v, want no run record", verb[0], records)
			}
		})
	}
}

// TestC5C3StatusPrintsOrderStates confirms status prints one row per recorded
// order: Ready, the reason of a False condition, Pending, and gone, reading
// each order in the namespace of its id. It names a cloud with --os-cloud
// while OS_CLIENT_CONFIG_FILE names a file that does not exist, so a status
// that read the entry would fail: status reads no clouds.yaml.
func TestC5C3StatusPrintsOrderStates(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", filepath.Join(t.TempDir(), "missing-clouds.yaml"))
	const base = "/apis/c5c3.io/v1alpha1/namespaces/orders-a/"
	object := func(kind, name, status string) string {
		return fmt.Sprintf(`{"apiVersion":"c5c3.io/v1alpha1","kind":%q,"metadata":{"name":%q,"namespace":"orders-a","generation":1},"spec":{}%s}`,
			kind, name, status)
	}
	api := &fakeKubeAPI{objects: map[string]string{
		base + "keystoneusers/dizzyorder-c5c30004-user": object("KeystoneUser", "dizzyorder-c5c30004-user",
			`,"status":{"observedGeneration":1,"conditions":[{"type":"UserReady","status":"True","reason":"UserProvisioned"},{"type":"Ready","status":"True","reason":"AllReady"}]}`),
		base + "keystoneprojects/dizzyorder-c5c30004-project": object("KeystoneProject", "dizzyorder-c5c30004-project",
			`,"status":{"observedGeneration":1,"conditions":[{"type":"ProjectReady","status":"False","reason":"WaitingForAdminCredential"},{"type":"Ready","status":"False","reason":"NotAllReady"}]}`),
		base + "keystoneroleassignments/dizzyorder-c5c30004-role": object("KeystoneRoleAssignment", "dizzyorder-c5c30004-role", ""),
	}}
	kubeconfig := serveKubeAPI(t, api)

	var created []resource.Resource
	for _, o := range []struct {
		kind    resource.Kind
		logical string
	}{{c5c3.KindUser, "user"}, {c5c3.KindProject, "project"}, {c5c3.KindRoleAssignment, "role"}, {c5c3.KindCatalogEntry, "catalog"}} {
		name := "dizzyorder-c5c30004-" + o.logical
		created = append(created, resource.Resource{Kind: o.kind, Logical: o.logical, Name: name, ID: "orders-a/" + name})
	}
	path, err := run.Write(t.TempDir(), &run.Record{RunID: "c5c30004", Service: "c5c3", Created: created})
	if err != nil {
		t.Fatalf("seeding record: %v", err)
	}

	out, err := execRoot(t, "c5c3", "status", "--os-cloud", "any", "--run", path, "--kubeconfig", kubeconfig)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	states := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		fields := strings.Fields(line)
		states[fields[0]] = fields[len(fields)-1]
	}
	want := map[string]string{"user": "Ready", "project": "WaitingForAdminCredential", "role": "Pending", "catalog": "gone"}
	for logical, state := range want {
		if states[logical] != state {
			t.Errorf("state of %s = %q, want %q:\n%s", logical, states[logical], state, out)
		}
	}
	if len(states) != len(want) {
		t.Errorf("status printed %d rows, want %d:\n%s", len(states), len(want), out)
	}
}

// writeC5C3ReportRecord seeds a c5c3 run record with the five orders, a
// sample of every order kind and of the delivered Secrets, a mismatch, and
// the order kinds' time to ready, and returns its path.
func writeC5C3ReportRecord(t *testing.T) string {
	t.Helper()
	c := metrics.NewCollector()
	for _, kind := range c5c3OrderResources {
		c.Record(metrics.Sample{Type: string(kind.kind), Duration: 20 * time.Millisecond, Success: true})
		c.RecordReadiness(metrics.Readiness{Type: string(kind.kind), Duration: 40 * time.Second, OK: true})
	}
	c.Record(metrics.Sample{Type: string(c5c3.KindSecret), Duration: 5 * time.Millisecond, Success: true})
	c.Record(metrics.Sample{Type: string(c5c3.KindUser), Duration: 30 * time.Millisecond, Success: false, ErrKind: "mismatch"})

	p := c5c3SmallPlan(t)
	var created []resource.Resource
	for _, o := range p.Orders {
		name := "dizzyorder-c5c30005-" + o.Name
		created = append(created, resource.Resource{Kind: resource.Kind(o.Kind), Logical: o.Name, Name: name, ID: p.Namespace + "/" + name})
	}
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	rec := c5c3RunRecord("c5c30005", p, start, start.Add(2*time.Second), created, c.Aggregate(2*time.Second),
		errors.New("stage 1 (user and project): c5c3: mismatch"))
	path, err := run.Write(t.TempDir(), rec)
	if err != nil {
		t.Fatalf("seeding c5c3 record: %v", err)
	}
	return path
}

// TestC5C3ReportRendersRecord confirms c5c3 report renders a c5c3 record with
// five orders as a table, JSON, CSV and HTML, each carrying every order kind.
func TestC5C3ReportRendersRecord(t *testing.T) {
	path := writeC5C3ReportRecord(t)
	report := func(t *testing.T, format string) string {
		t.Helper()
		out, err := execRoot(t, "c5c3", "report", "--run", path, "--format", format)
		if err != nil {
			t.Fatalf("c5c3 report --format %s: %v", format, err)
		}
		return out
	}
	var kinds []string
	for _, r := range c5c3OrderResources {
		kinds = append(kinds, string(r.kind))
	}

	t.Run("table", func(t *testing.T) {
		out := report(t, "table")
		if !strings.HasPrefix(out, "Run metrics (wall 2s)") {
			t.Errorf("report does not open with the metrics header:\n%s", out)
		}
		rows := map[string]int{}
		for _, line := range strings.Split(out, "\n") {
			if fields := strings.Fields(line); len(fields) > 0 {
				rows[fields[0]]++
			}
		}
		// An order kind appears in the operations table and in time to ready.
		for _, kind := range kinds {
			if rows[kind] != 2 {
				t.Errorf("table has %d rows for %q, want 2:\n%s", rows[kind], kind, out)
			}
		}
		for _, label := range []string{"overall", "secret", "mismatch"} {
			if rows[label] != 1 {
				t.Errorf("table has %d rows for %q, want 1:\n%s", rows[label], label, out)
			}
		}
	})
	t.Run("json", func(t *testing.T) {
		var agg metrics.Aggregate
		if err := json.Unmarshal([]byte(report(t, "json")), &agg); err != nil {
			t.Fatalf("report is not valid metrics JSON: %v", err)
		}
		var types, ready []string
		for _, s := range agg.ByType {
			types = append(types, s.Type)
		}
		for _, r := range agg.Readiness {
			ready = append(ready, r.Type)
		}
		for _, kind := range kinds {
			if !slices.Contains(types, kind) || !slices.Contains(ready, kind) {
				t.Errorf("JSON byType %v or readiness %v lacks %q", types, ready, kind)
			}
		}
	})
	t.Run("csv", func(t *testing.T) {
		records, err := csv.NewReader(strings.NewReader(report(t, "csv"))).ReadAll()
		if err != nil {
			t.Fatalf("report is not valid CSV: %v", err)
		}
		var labels []string
		for _, r := range records[1:] {
			labels = append(labels, r[0])
		}
		for _, kind := range append([]string{"overall", "secret"}, kinds...) {
			if !slices.Contains(labels, kind) {
				t.Errorf("CSV rows %v lack %q", labels, kind)
			}
		}
	})
	t.Run("html", func(t *testing.T) {
		out := report(t, "html")
		if !strings.Contains(strings.ToLower(out), "<html") {
			t.Fatalf("report is not an HTML page:\n%.200s", out)
		}
		for _, want := range append([]string{"mismatch"}, kinds...) {
			if !strings.Contains(out, want) {
				t.Errorf("HTML report lacks %q", want)
			}
		}
	})
}

// TestC5C3KeystoneEndpointUnset confirms that with neither --os-cloud nor
// OS_CLOUD the endpoint override is empty and no clouds.yaml is read: the file
// OS_CLIENT_CONFIG_FILE names cannot be parsed, which a named cloud proves.
func TestC5C3KeystoneEndpointUnset(t *testing.T) {
	writeC5C3Clouds(t, "clouds: [this is: not: a map\n")

	authURL, tlsConfig, err := c5c3KeystoneEndpoint(&globalOptions{})
	if authURL != "" || tlsConfig != nil || err != nil {
		t.Errorf("c5c3KeystoneEndpoint = %q, %v, %v; want \"\", nil, nil", authURL, tlsConfig, err)
	}

	if _, _, err := c5c3KeystoneEndpoint(&globalOptions{osCloud: "any"}); err == nil {
		t.Error("c5c3KeystoneEndpoint with a named cloud parsed the broken clouds.yaml; the test proves nothing")
	}
}

// TestC5C3KeystoneEndpointNoAuthURL confirms an entry without auth_url fails
// with config's exact message, which carries no credential of the entry, and
// that $OS_CLOUD names the entry as --os-cloud does.
func TestC5C3KeystoneEndpointNoAuthURL(t *testing.T) {
	writeC5C3Clouds(t, c5c3NoAuthURLClouds)
	const want = `clouds.yaml entry "noauth" has no auth_url`

	authURL, _, err := c5c3KeystoneEndpoint(&globalOptions{osCloud: "noauth"})
	if err == nil || err.Error() != want {
		t.Errorf("c5c3KeystoneEndpoint = %q, %v; want error %q", authURL, err, want)
	}
	if err != nil && strings.Contains(err.Error(), "s3cr3t-pw") {
		t.Errorf("error %q carries the entry's password", err)
	}

	t.Setenv("OS_CLOUD", "noauth")
	if _, _, err := c5c3KeystoneEndpoint(&globalOptions{}); err == nil || err.Error() != want {
		t.Errorf("c5c3KeystoneEndpoint with OS_CLOUD=noauth: err = %v, want %q", err, want)
	}
}

// TestC5C3KeystoneEndpointFromEntry confirms an entry's auth_url and TLS
// settings are returned without a request to Keystone.
func TestC5C3KeystoneEndpointFromEntry(t *testing.T) {
	writeC5C3Clouds(t, `clouds:
  override:
    verify: false
    auth:
      auth_url: https://keystone.invalid:5000/v3
      username: dizzy
      password: s3cr3t-pw
`)
	authURL, tlsConfig, err := c5c3KeystoneEndpoint(&globalOptions{osCloud: "override"})
	if err != nil {
		t.Fatalf("c5c3KeystoneEndpoint: %v", err)
	}
	if authURL != "https://keystone.invalid:5000/v3" {
		t.Errorf("auth URL = %q, want the entry's auth_url", authURL)
	}
	if tlsConfig == nil || !tlsConfig.InsecureSkipVerify {
		t.Errorf("TLS config = %+v, want verification off as the entry says", tlsConfig)
	}
}

// TestC5C3TelemetryConfig confirms c5c3 apply and monitor tag their telemetry
// with service c5c3, the plan's scenario and the cloud --os-cloud names, and
// that both commands set telemetry up with c5c3TelemetryConfig.
func TestC5C3TelemetryConfig(t *testing.T) {
	cfg := c5c3TelemetryConfig(&globalOptions{osCloud: "prod"}, c5c3SmallPlan(t))
	if cfg.Service != "c5c3" || cfg.Scenario != "small" || cfg.Cloud != "prod" {
		t.Errorf("telemetry config = service %q, scenario %q, cloud %q; want c5c3, small, prod", cfg.Service, cfg.Scenario, cfg.Cloud)
	}

	const call = "setupTelemetry(ctx, opts, c5c3TelemetryConfig(opts, p))"
	for _, f := range []string{"c5c3_apply.go", "c5c3_monitor.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		if !strings.Contains(string(src), call) {
			t.Errorf("%s does not set telemetry up with %s", f, call)
		}
	}
}

// TestFinishAbortedC5C3ApplyTearsDown confirms an interrupted apply writes its
// record before the teardown's first request, tears down on a context the
// signal did not cancel, and deletes every order of the run.
func TestFinishAbortedC5C3ApplyTearsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a first signal cancelled the run

	p := c5c3ShortPlan(t)
	dyn := newC5C3FakeDynamic()
	created := seedC5C3RunOrders(t, dyn, "abcd1234")
	recordWritten := false
	var firstSawRecord, sawFirst bool
	dyn.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !sawFirst {
			sawFirst, firstSawRecord = true, recordWritten
		}
		return false, nil, nil
	})
	client := c5c3.New(dyn, p.Namespace, "abcd1234", metrics.NewCollector())
	var out bytes.Buffer
	writeRecord := func() (string, error) {
		recordWritten = true
		return "run-abcd1234.json", nil
	}

	err := finishAbortedC5C3Apply(ctx, &out, client, p, "abcd1234", created[:2], false, context.Canceled, 2, time.Second, writeRecord)

	if err == nil {
		t.Fatal("finishAbortedC5C3Apply returned nil; an interrupted apply must exit non-zero")
	}
	for _, want := range []string{"abcd1234", "interrupted", "5 created resource(s) torn down"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if !sawFirst || !firstSawRecord {
		t.Error("the run record was not written before the teardown's first request")
	}
	for _, r := range created {
		if c5c3Stored(t, dyn, r) {
			t.Errorf("order %s is still present after the teardown", r.ID)
		}
	}
	if want := "deleted 5 resource(s) for run abcd1234\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
	var deletes []string
	for _, r := range c5c3RequestLog(dyn) {
		if strings.HasPrefix(r, "delete ") {
			deletes = append(deletes, r)
		}
	}
	want := []string{
		"delete keystoneapplicationcredentials dizzyorder-abcd1234-appcred",
		"delete keystoneroleassignments dizzyorder-abcd1234-role",
		"delete keystonecatalogentries dizzyorder-abcd1234-catalog",
		"delete keystoneprojects dizzyorder-abcd1234-project",
		"delete keystoneusers dizzyorder-abcd1234-user",
	}
	if !slices.Equal(deletes, want) {
		t.Errorf("deletes = %q, want %q", deletes, want)
	}
}

// TestFinishAbortedC5C3ApplyKeepOnAbort confirms --keep-on-abort sends no
// request, leaves every order in place and prints the reclaim hint: the
// record path when it was written, otherwise the run id and the namespace.
func TestFinishAbortedC5C3ApplyKeepOnAbort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name       string
		recordPath string
		wantHint   string
	}{
		{"with record path", "run-keep.json", "c5c3 cleanup --run run-keep.json"},
		{"empty record path falls back to run-id", "", "c5c3 cleanup --run-id keep1234 --namespace dizzy-orders"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dyn := newC5C3FakeDynamic()
			created := seedC5C3RunOrders(t, dyn, "keep1234")
			client := c5c3.New(dyn, c5c3TestNamespace, "keep1234", metrics.NewCollector())
			var out bytes.Buffer
			recordWritten := false
			writeRecord := func() (string, error) {
				recordWritten = true
				return tc.recordPath, nil
			}

			err := finishAbortedC5C3Apply(ctx, &out, client, c5c3ShortPlan(t), "keep1234", created, true, context.Canceled, 2, time.Second, writeRecord)

			if err == nil || !strings.Contains(err.Error(), "keep1234") {
				t.Errorf("error = %v, want a non-nil error naming the run id", err)
			}
			if !recordWritten {
				t.Error("the run record was not written")
			}
			if got := c5c3RequestLog(dyn); len(got) != 0 {
				t.Errorf("requests = %q; --keep-on-abort must not tear anything down", got)
			}
			for _, r := range created {
				if !c5c3Stored(t, dyn, r) {
					t.Errorf("order %s was deleted despite --keep-on-abort", r.ID)
				}
			}
			if !strings.Contains(out.String(), tc.wantHint) {
				t.Errorf("output %q missing hint %q", out.String(), tc.wantHint)
			}
		})
	}
}
