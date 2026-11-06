package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/octavia"
	octaviascenario "github.com/B42Labs/dizzy/internal/octavia/scenario"
)

// The list paths of the fake Octavia cloud: Octavia's below the version the
// load-balancer client appends, Neutron's below the doubled v2.0 the network
// client produces from the versioned catalog URL.
const (
	octaviaLBListPath      = "/load-balancer/v2.0/lbaas/loadbalancers"
	octaviaSubnetListPath  = "/network/v2.0/v2.0/subnets"
	octaviaNetworkListPath = "/network/v2.0/v2.0/networks"
	octaviaQuotaPath       = "/load-balancer/v2.0/quotas/proj-oct"
)

// octaviaTightQuota allows one load balancer, fewer than the two
// sampleOctaviaScenarioYAML plans, and leaves every other quota unlimited.
const octaviaTightQuota = `{"quota":{"loadbalancer":1,"listener":-1,"pool":-1,"member":-1,"healthmonitor":-1}}`

// octaviaSweepListings are the three listings one pre-flight sweep sends, in
// its load balancers, subnets, networks order: each scoped to the token's
// project and to the type tag of its kind.
var octaviaSweepListings = []string{
	"GET " + octaviaLBListPath + "?project_id=proj-oct&tags=dizzy%3Atype%3Dloadbalancer",
	"GET " + octaviaSubnetListPath + "?project_id=proj-oct&tags=dizzy%3Atype%3Dsubnet",
	"GET " + octaviaNetworkListPath + "?project_id=proj-oct&tags=dizzy%3Atype%3Dnetwork",
}

// octaviaCleanupListings are the three listings run0's own cleanup sends, in
// the same order: by the run tag only.
var octaviaCleanupListings = []string{
	"GET " + octaviaLBListPath + "?tags=dizzy%3Arun%3Drun0",
	"GET " + octaviaSubnetListPath + "?tags=dizzy%3Arun%3Drun0",
	"GET " + octaviaNetworkListPath + "?tags=dizzy%3Arun%3Drun0",
}

// octaviaStackRequests are the requests building the load-balancer stack
// sends: the token, then Octavia's version document.
var octaviaStackRequests = []string{"POST /v3/auth/tokens", "GET /load-balancer/"}

func TestOctaviaMonitorFlags(t *testing.T) {
	octaviaCmd := findSubcommand(newRootCmd(), "octavia")
	if octaviaCmd == nil {
		t.Fatal("octavia command not registered on root")
	}
	monitor := findSubcommand(octaviaCmd, "monitor")
	if monitor == nil {
		t.Fatal("octavia monitor not registered")
	}
	for _, name := range []string{"scenario", "set"} {
		if monitor.Flags().Lookup(name) == nil {
			t.Errorf("octavia monitor has no --%s flag", name)
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
			t.Errorf("octavia monitor has no --%s flag", name)
			continue
		}
		if f.DefValue != want {
			t.Errorf("--%s default = %q, want %q", name, f.DefValue, want)
		}
	}
	for _, name := range []string{"reclaim-orphans", "dry-run", "keep-on-abort"} {
		if monitor.Flags().Lookup(name) != nil {
			t.Errorf("octavia monitor has a --%s flag, which a monitor does not take", name)
		}
	}
}

// TestOctaviaMonitorHelpWarnsOnePerProject confirms the monitor's help tells
// the operator that its always-on sweep tolerates no second dizzy process in
// the project.
func TestOctaviaMonitorHelpWarnsOnePerProject(t *testing.T) {
	out, err := execRoot(t, "octavia", "monitor", "--help")
	if err != nil {
		t.Fatalf("octavia monitor --help: %v", err)
	}
	if !strings.Contains(out, "Run only one dizzy process per project") {
		t.Errorf("octavia monitor --help does not warn about one dizzy process per project:\n%s", out)
	}
}

func TestOctaviaMonitorRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "octavia", "monitor", "--interval", "1m"); err == nil {
		t.Error("octavia monitor without --scenario: expected an error, got nil")
	}
}

func TestOctaviaMonitorRejectsNegativeInterval(t *testing.T) {
	path := writeScenario(t, sampleOctaviaScenarioYAML)
	_, err := execRoot(t, "octavia", "monitor", "--scenario", path, "--interval=-1m")
	if err == nil {
		t.Fatal("octavia monitor with a negative --interval: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "--interval") {
		t.Errorf("error = %q, want it to mention --interval", err.Error())
	}
}

func TestOctaviaMonitorWithValidConfigRequiresCloud(t *testing.T) {
	// Point clouds.yaml resolution at a nonexistent file so auth fails
	// deterministically, proving config validation and telemetry setup precede
	// authentication: the command fails only at load balancer client creation.
	noOctaviaCloud(t)

	path := writeScenario(t, sampleOctaviaScenarioYAML)
	_, err := execRoot(t, "octavia", "monitor", "--scenario", path, "--iterations", "1")
	if err == nil {
		t.Fatal("octavia monitor with a valid config but no reachable cloud: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "load balancer clients") {
		t.Errorf("error = %q, want it to mention the load balancer client (auth) step", err.Error())
	}
}

// TestOctaviaMonitorStartupRefusesWithoutProject confirms a credential whose
// auth result names no project stops the monitor right after the stack is
// built: no quota read, no iteration, so no sweep ever lists unscoped.
func TestOctaviaMonitorStartupRefusesWithoutProject(t *testing.T) {
	fc := newFakeOctaviaCloudInProject(t, "", nil)
	path := writeScenario(t, sampleOctaviaScenarioYAML)

	_, err := execRoot(t, "octavia", "monitor", "--os-cloud", "oct", "--scenario", path, "--iterations", "1")
	const want = "octavia monitor needs a project-scoped credential: the auth result carries no project id, and the pre-flight sweep never lists beyond the token's project"
	if err == nil || err.Error() != want {
		t.Errorf("octavia monitor = %v, want %q", err, want)
	}
	if got := fc.requests(); !slices.Equal(got, octaviaStackRequests) {
		t.Errorf("requests = %q, want %q (no quota read, no iteration)", got, octaviaStackRequests)
	}
}

// TestOctaviaMonitorStartupPrecheckAborts confirms an oversized plan fails the
// startup pre-check with the itemized message before the loop begins.
func TestOctaviaMonitorStartupPrecheckAborts(t *testing.T) {
	fc := newFakeOctaviaCloud(t, map[string]octaviaReply{
		"GET " + octaviaQuotaPath: {http.StatusOK, octaviaTightQuota},
	})
	path := writeScenario(t, sampleOctaviaScenarioYAML)

	_, err := execRoot(t, "octavia", "monitor", "--os-cloud", "oct", "--scenario", path, "--iterations", "1")
	const want = "plan exceeds project quota; raise these quotas before applying: loadbalancer need 2, quota 1"
	if err == nil || err.Error() != want {
		t.Errorf("octavia monitor = %v, want %q", err, want)
	}
	wantLog := append(slices.Clone(octaviaStackRequests), "GET "+octaviaQuotaPath)
	if got := fc.requests(); !slices.Equal(got, wantLog) {
		t.Errorf("requests = %q, want %q (no iteration)", got, wantLog)
	}
}

// TestOctaviaMonitorStartupPrecheckReadFailure confirms a quota read that
// fails with anything but a 403 aborts the monitor before the loop begins.
func TestOctaviaMonitorStartupPrecheckReadFailure(t *testing.T) {
	fc := newFakeOctaviaCloud(t, map[string]octaviaReply{
		"GET " + octaviaQuotaPath: {http.StatusInternalServerError, `{"faultstring":"boom"}`},
	})
	path := writeScenario(t, sampleOctaviaScenarioYAML)

	_, err := execRoot(t, "octavia", "monitor", "--os-cloud", "oct", "--scenario", path, "--iterations", "1")
	const want = "reading Octavia quotas for pre-check: "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("octavia monitor = %v, want an error starting %q", err, want)
	}
	wantLog := append(slices.Clone(octaviaStackRequests), "GET "+octaviaQuotaPath)
	if got := fc.requests(); !slices.Equal(got, wantLog) {
		t.Errorf("requests = %q, want %q (no iteration)", got, wantLog)
	}
}

// octaviaTestDeps starts a fake Octavia cloud answering from replies, builds
// the load-balancer stack against it, and returns the iteration phases for run0
// over sampleOctaviaScenarioYAML's plan (1 network, 2 load balancers, 2
// listeners, 2 pools, 4 members, 1 health monitor), built exactly as
// octaviaMonitorRunOnce builds them, together with the fake.
func octaviaTestDeps(t *testing.T, replies map[string]octaviaReply) (iterationDeps, *fakeOctaviaCloud) {
	t.Helper()
	fc := newFakeOctaviaCloud(t, replies)
	stack, err := config.NewLoadBalancerStack(context.Background(), "oct")
	if err != nil {
		t.Fatalf("building the load-balancer stack: %v", err)
	}
	s, err := octaviascenario.Parse([]byte(sampleOctaviaScenarioYAML))
	if err != nil {
		t.Fatalf("parsing sample scenario: %v", err)
	}
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("generating sample plan: %v", err)
	}
	client := octavia.New(stack.LoadBalancer, stack.Network, "run0", metrics.NewCollector())
	return octaviaIterationDeps(stack.LoadBalancer, client, p, "run0", 2, time.Second), fc
}

// afterStack returns the requests the fake received after the two that built
// the stack, failing the test when the log does not open with them.
func afterStack(t *testing.T, fc *fakeOctaviaCloud) []string {
	t.Helper()
	got := fc.requests()
	if len(got) < len(octaviaStackRequests) || !slices.Equal(got[:len(octaviaStackRequests)], octaviaStackRequests) {
		t.Fatalf("requests = %q, want them to open with %q", got, octaviaStackRequests)
	}
	return got[len(octaviaStackRequests):]
}

// withEmptyOctaviaListings returns replies that answer the load balancer,
// subnet, and network listings with empty lists, plus extra.
func withEmptyOctaviaListings(extra map[string]octaviaReply) map[string]octaviaReply {
	replies := map[string]octaviaReply{
		"GET " + octaviaLBListPath:      {http.StatusOK, `{"loadbalancers":[]}`},
		"GET " + octaviaSubnetListPath:  {http.StatusOK, `{"subnets":[]}`},
		"GET " + octaviaNetworkListPath: {http.StatusOK, `{"networks":[]}`},
	}
	for k, v := range extra {
		replies[k] = v
	}
	return replies
}

// hasPost reports whether any request in log is a POST.
func hasPost(log []string) bool {
	return slices.ContainsFunc(log, func(line string) bool { return strings.HasPrefix(line, "POST ") })
}

// TestOctaviaPreflightSweepsAnyRunInProject confirms the sweep lists by the
// type tag within the token's project and reclaims a foreign run's leftovers
// there in dependency order: the load balancer with cascade, waited on until
// gone, then its subnet, then its network. A load balancer of another project
// that the answer carried anyway is never touched.
func TestOctaviaPreflightSweepsAnyRunInProject(t *testing.T) {
	deps, fc := octaviaTestDeps(t, map[string]octaviaReply{
		"GET " + octaviaLBListPath: {http.StatusOK, `{"loadbalancers":[` +
			`{"id":"lb9","name":"dizzy-run9-lb-0001","project_id":"proj-oct","provisioning_status":"ACTIVE","tags":["dizzy:run=run9","dizzy:type=loadbalancer"]},` +
			`{"id":"lbx","name":"dizzy-run7-lb-0001","project_id":"proj-other","provisioning_status":"ACTIVE","tags":["dizzy:run=run7","dizzy:type=loadbalancer"]}]}`},
		"DELETE " + octaviaLBListPath + "/lb9": {http.StatusNoContent, ""},
		"GET " + octaviaSubnetListPath: {http.StatusOK,
			`{"subnets":[{"id":"sub9","name":"dizzy-run9-sub-0001","project_id":"proj-oct","tags":["dizzy:run=run9","dizzy:type=subnet"]}]}`},
		"DELETE " + octaviaSubnetListPath + "/sub9": {http.StatusNoContent, ""},
		"GET " + octaviaNetworkListPath: {http.StatusOK,
			`{"networks":[{"id":"net9","name":"dizzy-run9-net-0001","project_id":"proj-oct","tags":["dizzy:run=run9","dizzy:type=network"]}]}`},
		"DELETE " + octaviaNetworkListPath + "/net9": {http.StatusNoContent, ""},
	})

	swept, err := deps.preflight(context.Background())
	if err != nil || swept != 3 {
		t.Errorf("preflight = (%d, %v), want (3, nil)", swept, err)
	}
	want := []string{
		octaviaSweepListings[0],
		"DELETE " + octaviaLBListPath + "/lb9?cascade=true",
		"GET " + octaviaLBListPath + "/lb9", // answered 404: gone
		octaviaSweepListings[1],
		"DELETE " + octaviaSubnetListPath + "/sub9",
		octaviaSweepListings[2],
		"DELETE " + octaviaNetworkListPath + "/net9",
	}
	if got := afterStack(t, fc); !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q", got, want)
	}
}

// TestOctaviaPreflightEmptyProject confirms a sweep of a project with no
// leftovers lists each kind once and deletes nothing.
func TestOctaviaPreflightEmptyProject(t *testing.T) {
	deps, fc := octaviaTestDeps(t, withEmptyOctaviaListings(nil))

	swept, err := deps.preflight(context.Background())
	if err != nil || swept != 0 {
		t.Errorf("preflight = (%d, %v), want (0, nil)", swept, err)
	}
	if got := afterStack(t, fc); !slices.Equal(got, octaviaSweepListings) {
		t.Errorf("requests = %q, want %q", got, octaviaSweepListings)
	}
}

// TestOctaviaIterationPrecheckAbortsBeforeCreate confirms an iteration reads
// the quota after its sweep and, for an oversized plan, fails its apply with
// the itemized message before any create, still running its cleanup.
func TestOctaviaIterationPrecheckAbortsBeforeCreate(t *testing.T) {
	deps, fc := octaviaTestDeps(t, withEmptyOctaviaListings(map[string]octaviaReply{
		"GET " + octaviaQuotaPath: {http.StatusOK, octaviaTightQuota},
	}))
	res := runIteration(context.Background(), deps)

	const want = "apply: plan exceeds project quota; raise these quotas before applying: loadbalancer need 2, quota 1"
	if res.ok || res.err == nil || res.err.Error() != want {
		t.Errorf("iteration = ok %v, err %v; want a failed iteration with %q", res.ok, res.err, want)
	}
	if len(res.created) != 0 {
		t.Errorf("created = %v, want nothing", res.created)
	}
	wantLog := slices.Concat(octaviaSweepListings, []string{"GET " + octaviaQuotaPath}, octaviaCleanupListings)
	if got := afterStack(t, fc); !slices.Equal(got, wantLog) {
		t.Errorf("requests = %q, want %q (sweep, quota read, cleanup; no create)", got, wantLog)
	}
}

// TestOctaviaIterationPrecheckReadFailure confirms a quota read that fails
// with anything but a 403 fails the iteration's apply before any create.
func TestOctaviaIterationPrecheckReadFailure(t *testing.T) {
	deps, fc := octaviaTestDeps(t, withEmptyOctaviaListings(map[string]octaviaReply{
		"GET " + octaviaQuotaPath: {http.StatusInternalServerError, `{"faultstring":"boom"}`},
	}))
	res := runIteration(context.Background(), deps)

	const want = "apply: reading Octavia quotas for pre-check: "
	if res.ok || res.err == nil || !strings.HasPrefix(res.err.Error(), want) {
		t.Errorf("iteration = ok %v, err %v; want a failed iteration starting %q", res.ok, res.err, want)
	}
	if got := afterStack(t, fc); hasPost(got) {
		t.Errorf("requests = %q, want no create after a failed quota read", got)
	}
}

// TestOctaviaIterationPrecheckDeniedProceeds confirms a denied (403) quota
// read skips the check, so the iteration goes on to create its first VIP
// network; the fake answers that create 404, which fails the apply.
func TestOctaviaIterationPrecheckDeniedProceeds(t *testing.T) {
	deps, fc := octaviaTestDeps(t, withEmptyOctaviaListings(map[string]octaviaReply{
		"GET " + octaviaQuotaPath: {http.StatusForbidden, `{"faultstring":"Policy does not allow this request to be performed."}`},
	}))
	res := runIteration(context.Background(), deps)

	if got := afterStack(t, fc); !slices.Contains(got, "POST "+octaviaNetworkListPath) {
		t.Errorf("requests = %q, want a network create after the denied quota read", got)
	}
	if res.err == nil || !strings.HasPrefix(res.err.Error(), "apply: ") || strings.Contains(res.err.Error(), "pre-check") {
		t.Errorf("iteration err = %v, want the create's apply error, not a pre-check error", res.err)
	}
}

// TestOctaviaIterationSweepFailureSkipsApply confirms a failed sweep fails the
// iteration, skips the apply (no quota read, no create), and still runs the
// cleanup, whose own load balancer listing then fails too.
func TestOctaviaIterationSweepFailureSkipsApply(t *testing.T) {
	deps, fc := octaviaTestDeps(t, map[string]octaviaReply{
		"GET " + octaviaLBListPath: {http.StatusInternalServerError, `{"faultstring":"boom"}`},
	})
	res := runIteration(context.Background(), deps)

	const want = "pre-flight cleanup: listing loadbalancer by tag: "
	if res.ok || res.err == nil || !strings.HasPrefix(res.err.Error(), want) {
		t.Errorf("iteration = ok %v, err %v; want a failed iteration starting %q", res.ok, res.err, want)
	}
	wantLog := []string{octaviaSweepListings[0], octaviaCleanupListings[0]}
	if got := afterStack(t, fc); !slices.Equal(got, wantLog) {
		t.Errorf("requests = %q, want %q (the sweep's listing, then the cleanup's)", got, wantLog)
	}
}
