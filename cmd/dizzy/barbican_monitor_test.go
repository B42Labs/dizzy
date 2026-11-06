package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	barbicanscenario "github.com/B42Labs/dizzy/internal/barbican/scenario"
	"github.com/B42Labs/dizzy/internal/metrics"
)

func TestBarbicanMonitorFlags(t *testing.T) {
	barbicanCmd := findSubcommand(newRootCmd(), "barbican")
	if barbicanCmd == nil {
		t.Fatal("barbican command not registered on root")
	}
	monitor := findSubcommand(barbicanCmd, "monitor")
	if monitor == nil {
		t.Fatal("barbican monitor not registered")
	}
	for _, name := range []string{"scenario", "set"} {
		if monitor.Flags().Lookup(name) == nil {
			t.Errorf("barbican monitor has no --%s flag", name)
		}
	}
	for name, want := range map[string]string{
		"interval":         "0s",
		"iterations":       "0",
		"error-wait":       "0s",
		"keep-run-records": "false",
	} {
		f := monitor.Flags().Lookup(name)
		if f == nil {
			t.Errorf("barbican monitor has no --%s flag", name)
			continue
		}
		if f.DefValue != want {
			t.Errorf("--%s default = %q, want %q", name, f.DefValue, want)
		}
	}
	if monitor.Flags().Lookup("reclaim-orphans") != nil {
		t.Error("barbican monitor has a --reclaim-orphans flag; its sweep is always on")
	}
}

func TestBarbicanMonitorRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "barbican", "monitor", "--interval", "1m"); err == nil {
		t.Error("barbican monitor without --scenario: expected an error, got nil")
	}
}

func TestBarbicanMonitorRejectsNegativeInterval(t *testing.T) {
	path := writeScenario(t, sampleBarbicanScenarioYAML)
	_, err := execRoot(t, "barbican", "monitor", "--scenario", path, "--interval=-1m")
	if err == nil {
		t.Fatal("barbican monitor with a negative --interval: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "--interval") {
		t.Errorf("error = %q, want it to mention --interval", err.Error())
	}
}

func TestBarbicanMonitorWithValidConfigRequiresCloud(t *testing.T) {
	// Point clouds.yaml resolution at a nonexistent file so auth fails
	// deterministically, proving config validation and telemetry setup precede
	// authentication — the command fails only at key manager client creation.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleBarbicanScenarioYAML)
	_, err := execRoot(t, "barbican", "monitor", "--scenario", path, "--iterations", "1")
	if err == nil {
		t.Fatal("barbican monitor with a valid config but no reachable cloud: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "key manager client") {
		t.Errorf("error = %q, want it to mention the key manager client (auth) step", err.Error())
	}
}

// startupRequests is the request log of a monitor that authenticated, resolved
// the key-manager endpoint, read the quota once, and stopped there: a second
// token request would mean an iteration started.
var startupRequests = []string{"POST /v3/auth/tokens", "GET /key-manager/", "GET /key-manager/v1/quotas"}

// TestBarbicanMonitorStartupPrecheckAborts confirms an oversized plan fails the
// startup pre-check with the itemized message before the loop begins.
func TestBarbicanMonitorStartupPrecheckAborts(t *testing.T) {
	url, requests := fakeKeystoneLog(t, "p1", map[string]string{
		"GET /key-manager/v1/quotas": `{"quotas":{"secrets":1,"orders":-1,"containers":-1,"consumers":-1,"cas":-1}}`,
	})
	tenantClouds(t, url)
	path := writeScenario(t, sampleBarbicanScenarioYAML)

	_, err := execRoot(t, "barbican", "monitor", "--os-cloud", "tenant-ci", "--scenario", path, "--iterations", "1")
	const want = "plan exceeds project quota; raise these quotas before applying: secrets need 6, quota 1"
	if err == nil || err.Error() != want {
		t.Errorf("barbican monitor = %v, want %q", err, want)
	}
	if got := requests(); !reflect.DeepEqual(got, startupRequests) {
		t.Errorf("requests = %q, want %q (no iteration)", got, startupRequests)
	}
}

// TestBarbicanMonitorStartupPrecheckReadFailure confirms a quota read that
// fails with anything but a 403 aborts the monitor before the loop begins.
func TestBarbicanMonitorStartupPrecheckReadFailure(t *testing.T) {
	url, requests := fakeKeystoneLog(t, "p1", nil) // the quota read answers 404
	tenantClouds(t, url)
	path := writeScenario(t, sampleBarbicanScenarioYAML)

	_, err := execRoot(t, "barbican", "monitor", "--os-cloud", "tenant-ci", "--scenario", path, "--iterations", "1")
	if err == nil || !strings.HasPrefix(err.Error(), "reading Barbican quotas for pre-check:") {
		t.Errorf("barbican monitor = %v, want an error starting %q", err, "reading Barbican quotas for pre-check:")
	}
	if got := requests(); !reflect.DeepEqual(got, startupRequests) {
		t.Errorf("requests = %q, want %q (no iteration)", got, startupRequests)
	}
}

// fakeKeyManager is a Barbican v1 endpoint for the iteration tests that logs
// every request as "<method> <path>". Its listings answer one page of the
// entries a test sets (empty by default); a secret's metadata answers what the
// test sets for its id, or 404; a secret store answers 400; every delete
// answers 204; the quota read answers quotaStatus (200 by default) with
// quotaBody (every quota unlimited by default). containerListStatus, when set,
// fails the container listing with that status.
type fakeKeyManager struct {
	t *testing.T

	containers          []string
	orders              []string
	secrets             []string
	metadata            map[string]string
	containerListStatus int
	quotaStatus         int
	quotaBody           string

	mu       sync.Mutex
	requests []string
}

func (f *fakeKeyManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	list := func(key string, entries []string) {
		writeKeyManagerJSON(w, http.StatusOK, fmt.Sprintf(`{%q:[%s],"total":%d}`, key, strings.Join(entries, ","), len(entries)))
	}
	switch path := r.URL.Path; {
	case r.Method == http.MethodGet && path == "/v1/quotas":
		status, body := f.quotaStatus, f.quotaBody
		if status == 0 {
			status = http.StatusOK
		}
		if body == "" {
			body = `{"quotas":{"secrets":-1,"orders":-1,"containers":-1,"consumers":-1,"cas":-1}}`
		}
		writeKeyManagerJSON(w, status, body)
	case r.Method == http.MethodGet && path == "/v1/containers":
		if f.containerListStatus != 0 {
			writeKeyManagerJSON(w, f.containerListStatus, fmt.Sprintf(`{"code":%d}`, f.containerListStatus))
			return
		}
		list("containers", f.containers)
	case r.Method == http.MethodGet && path == "/v1/orders":
		list("orders", f.orders)
	case r.Method == http.MethodGet && path == "/v1/secrets":
		list("secrets", f.secrets)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/secrets/") && strings.HasSuffix(path, "/metadata"):
		md, ok := f.metadata[strings.TrimSuffix(strings.TrimPrefix(path, "/v1/secrets/"), "/metadata")]
		if !ok {
			writeKeyManagerJSON(w, http.StatusNotFound, `{"code":404,"title":"Not Found"}`)
			return
		}
		writeKeyManagerJSON(w, http.StatusOK, md)
	case r.Method == http.MethodPost && path == "/v1/secrets":
		writeKeyManagerJSON(w, http.StatusBadRequest, `{"code":400,"title":"Bad Request"}`)
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// log returns the requests the fake received so far, in order.
func (f *fakeKeyManager) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// deletes returns the delete requests the fake received so far, in order.
func (f *fakeKeyManager) deletes() []string {
	var out []string
	for _, r := range f.log() {
		if strings.HasPrefix(r, http.MethodDelete+" ") {
			out = append(out, r)
		}
	}
	return out
}

// sawMethod reports whether the fake received any request with method.
func (f *fakeKeyManager) sawMethod(method string) bool {
	for _, r := range f.log() {
		if strings.HasPrefix(r, method+" ") {
			return true
		}
	}
	return false
}

func writeKeyManagerJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// Entries of the fake's listings, in the shape Barbican lists them. Barbican
// identifies each by an href whose last segment is the id.
func kmContainer(name, id string) string {
	return fmt.Sprintf(`{"type":"generic","status":"ACTIVE","name":%q,"container_ref":"https://kv/v1/containers/%s","secret_refs":[]}`, name, id)
}

func kmOrder(metaName, id, secretID string) string {
	return fmt.Sprintf(`{"type":"key","status":"ACTIVE","order_ref":"https://kv/v1/orders/%s","secret_ref":"https://kv/v1/secrets/%s","meta":{"name":%q}}`, id, secretID, metaName)
}

func kmSecret(name, id string) string {
	return fmt.Sprintf(`{"name":%q,"secret_ref":"https://kv/v1/secrets/%s","status":"ACTIVE"}`, name, id)
}

// sampleBarbicanPlan expands sampleBarbicanScenarioYAML: 4 secrets, 2 orders,
// and 2 containers, so the plan needs 6 secrets of quota.
func sampleBarbicanPlan(t *testing.T) *barbicanplan.Plan {
	t.Helper()
	s, err := barbicanscenario.Parse([]byte(sampleBarbicanScenarioYAML))
	if err != nil {
		t.Fatalf("parsing sample scenario: %v", err)
	}
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("generating sample plan: %v", err)
	}
	return p
}

// barbicanTestDeps serves f and returns the iteration phases for run0 against
// it, built exactly as barbicanMonitorRunOnce builds them.
func barbicanTestDeps(t *testing.T, f *fakeKeyManager) iterationDeps {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	kv := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: ts.URL + "/", ResourceBase: ts.URL + "/v1/"}
	client := newBarbicanClient(kv, "run0", metrics.NewCollector(), time.Second)
	return barbicanIterationDeps(kv, client, sampleBarbicanPlan(t), "run0", 2, time.Second)
}

// listingRequests are the three discovery listings one Cleanup pass makes, in
// its containers, orders, secrets order.
var listingRequests = []string{"GET /v1/containers", "GET /v1/orders", "GET /v1/secrets"}

// TestBarbicanIterationPrecheckAbortsBeforeCreate confirms an iteration reads
// the quota after its sweep and, for an oversized plan, fails its apply with
// the itemized message before any create, still running its cleanup.
func TestBarbicanIterationPrecheckAbortsBeforeCreate(t *testing.T) {
	f := &fakeKeyManager{t: t, quotaBody: `{"quotas":{"secrets":1,"orders":-1,"containers":-1,"consumers":-1,"cas":-1}}`}
	res := runIteration(context.Background(), barbicanTestDeps(t, f))

	const want = "apply: plan exceeds project quota; raise these quotas before applying: secrets need 6, quota 1"
	if res.ok || res.err == nil || res.err.Error() != want {
		t.Errorf("iteration = ok %v, err %v; want a failed iteration with %q", res.ok, res.err, want)
	}
	if len(res.created) != 0 {
		t.Errorf("created = %v, want nothing", res.created)
	}
	wantLog := slices.Concat(listingRequests, []string{"GET /v1/quotas"}, listingRequests)
	if got := f.log(); !reflect.DeepEqual(got, wantLog) {
		t.Errorf("requests = %q, want %q (sweep, quota read, cleanup; no create)", got, wantLog)
	}
}

// TestBarbicanIterationPrecheckReadFailure confirms a quota read that fails
// with anything but a 403 fails the iteration's apply before any create.
func TestBarbicanIterationPrecheckReadFailure(t *testing.T) {
	f := &fakeKeyManager{t: t, quotaStatus: http.StatusInternalServerError, quotaBody: `{"code":500}`}
	res := runIteration(context.Background(), barbicanTestDeps(t, f))

	const want = "apply: reading Barbican quotas for pre-check:"
	if res.ok || res.err == nil || !strings.HasPrefix(res.err.Error(), want) {
		t.Errorf("iteration = ok %v, err %v; want a failed iteration starting %q", res.ok, res.err, want)
	}
	if f.sawMethod(http.MethodPost) {
		t.Errorf("requests = %q, want no create after a failed quota read", f.log())
	}
}

// TestBarbicanIterationPrecheckMissingQuotas confirms a quota answer without a
// quotas object fails the iteration's apply before any create.
func TestBarbicanIterationPrecheckMissingQuotas(t *testing.T) {
	f := &fakeKeyManager{t: t, quotaBody: `{}`}
	res := runIteration(context.Background(), barbicanTestDeps(t, f))

	const want = "apply: reading Barbican quotas for pre-check: response carries no quotas object"
	if res.ok || res.err == nil || res.err.Error() != want {
		t.Errorf("iteration = ok %v, err %v; want a failed iteration with %q", res.ok, res.err, want)
	}
	if f.sawMethod(http.MethodPost) {
		t.Errorf("requests = %q, want no create after a quota answer without quotas", f.log())
	}
}

// TestBarbicanIterationPrecheckDeniedProceeds confirms a denied (403) quota
// read skips the check, so the iteration goes on to store secrets.
func TestBarbicanIterationPrecheckDeniedProceeds(t *testing.T) {
	f := &fakeKeyManager{t: t, quotaStatus: http.StatusForbidden, quotaBody: `{"code":403,"title":"Forbidden"}`}
	res := runIteration(context.Background(), barbicanTestDeps(t, f))

	if !slices.Contains(f.log(), "POST /v1/secrets") {
		t.Errorf("requests = %q, want a secret store after the denied quota read", f.log())
	}
	if res.err == nil || !strings.HasPrefix(res.err.Error(), "apply: ") || strings.Contains(res.err.Error(), "pre-check") {
		t.Errorf("iteration err = %v, want the store's apply error, not a pre-check error", res.err)
	}
}

// TestBarbicanPreflightSweepsAnyRun confirms the sweep reclaims a foreign run's
// leftovers by the any-run identity: the dizzy- container, the dizzy- order
// after the secret it generated, and the stamped dizzy- secret, while it never
// deletes an unprefixed container, order, or secret, the secret an unprefixed
// order generated, or a prefixed secret without dizzy_type metadata.
func TestBarbicanPreflightSweepsAnyRun(t *testing.T) {
	f := &fakeKeyManager{
		t:          t,
		containers: []string{kmContainer("dizzy-run9-ctr-0001", "c1"), kmContainer("unrelated", "c2")},
		orders:     []string{kmOrder("dizzy-run9-ord-0001", "o1", "g1"), kmOrder("other-order", "o2", "g2")},
		secrets: []string{
			kmSecret("dizzy-run9-sec-0001", "s1"),
			kmSecret("dizzy-run9-sec-0002", "s2"),
			kmSecret("dizzy-run9-ord-0001", "g1"),
			kmSecret("unrelated", "s3"),
		},
		metadata: map[string]string{
			"s1": `{"metadata":{"dizzy_run":"run9","dizzy_type":"secret"}}`,
			"s2": `{"metadata":{}}`,
			"g1": `{"metadata":{"dizzy_run":"run9","dizzy_type":"secret"}}`,
			"s3": `{"metadata":{"dizzy_type":"secret"}}`,
		},
	}
	swept, err := barbicanTestDeps(t, f).preflight(context.Background())
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if swept != 4 {
		t.Errorf("swept = %d, want 4", swept)
	}
	want := []string{
		"DELETE /v1/containers/c1",
		"DELETE /v1/secrets/g1",
		"DELETE /v1/orders/o1",
		"DELETE /v1/secrets/s1",
	}
	if got := f.deletes(); !reflect.DeepEqual(got, want) {
		t.Errorf("deletes = %q, want %q", got, want)
	}
}

// TestBarbicanPreflightEmptyProject confirms a sweep of a project with no
// leftovers lists each kind once and deletes nothing.
func TestBarbicanPreflightEmptyProject(t *testing.T) {
	f := &fakeKeyManager{t: t}
	swept, err := barbicanTestDeps(t, f).preflight(context.Background())
	if err != nil || swept != 0 {
		t.Errorf("preflight = (%d, %v), want (0, nil)", swept, err)
	}
	if got := f.log(); !reflect.DeepEqual(got, listingRequests) {
		t.Errorf("requests = %q, want %q", got, listingRequests)
	}
}

// TestBarbicanIterationSweepFailureSkipsApply confirms a failed sweep fails the
// iteration, skips the apply (no quota read, no create), and still runs the
// cleanup, whose own container listing then fails too.
func TestBarbicanIterationSweepFailureSkipsApply(t *testing.T) {
	f := &fakeKeyManager{t: t, containerListStatus: http.StatusInternalServerError}
	res := runIteration(context.Background(), barbicanTestDeps(t, f))

	const want = "pre-flight cleanup: listing containers by any run:"
	if res.ok || res.err == nil || !strings.HasPrefix(res.err.Error(), want) {
		t.Errorf("iteration = ok %v, err %v; want a failed iteration starting %q", res.ok, res.err, want)
	}
	if got, wantLog := f.log(), []string{"GET /v1/containers", "GET /v1/containers"}; !reflect.DeepEqual(got, wantLog) {
		t.Errorf("requests = %q, want %q (the sweep's listing, then the cleanup's)", got, wantLog)
	}
}
