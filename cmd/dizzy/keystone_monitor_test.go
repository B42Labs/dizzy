package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneexec "github.com/B42Labs/dizzy/internal/keystone/executor"
	keystonescenario "github.com/B42Labs/dizzy/internal/keystone/scenario"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

func TestKeystoneMonitorRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "keystone", "monitor"); err == nil {
		t.Fatal("keystone monitor without --scenario: expected error, got nil")
	}
}

func TestKeystoneMonitorRejectsNegativeInterval(t *testing.T) {
	path := writeScenario(t, sampleKeystoneScenarioYAML)
	if _, err := execRoot(t, "keystone", "monitor", "--scenario", path, "--interval", "-1s"); err == nil {
		t.Fatal("keystone monitor with a negative interval: expected error, got nil")
	}
}

func TestKeystoneMonitorWithValidConfigRequiresCloud(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleKeystoneScenarioYAML)
	_, err := execRoot(t, "keystone", "monitor", "--scenario", path, "--iterations", "1")
	if err == nil {
		t.Fatal("keystone monitor with a valid config: expected a cloud-auth failure, got nil")
	}
	if !strings.Contains(err.Error(), "identity client") {
		t.Errorf("error %q does not mention identity client creation", err.Error())
	}
}

func TestKeystoneMonitorCleanupRetryBudgetFlag(t *testing.T) {
	f := newKeystoneMonitorCmd(&globalOptions{}).Flags().Lookup("cleanup-retry-budget")
	if f == nil {
		t.Fatal("keystone monitor has no --cleanup-retry-budget flag")
	}
	if f.DefValue != "5m0s" {
		t.Errorf("--cleanup-retry-budget default = %q, want 5m0s", f.DefValue)
	}
}

// TestKeystoneMonitorRejectsNegativeCleanupRetryBudget confirms a negative
// budget fails before any cloud call: the error names the flag, not the
// identity client the missing clouds.yaml would fail.
func TestKeystoneMonitorRejectsNegativeCleanupRetryBudget(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleKeystoneScenarioYAML)
	_, err := execRoot(t, "keystone", "monitor", "--scenario", path, "--cleanup-retry-budget", "-1s")
	if err == nil || !strings.Contains(err.Error(), "--cleanup-retry-budget must not be negative") {
		t.Fatalf("keystone monitor --cleanup-retry-budget -1s: error = %v, want one naming the flag", err)
	}
	if strings.Contains(err.Error(), "identity client") {
		t.Errorf("error %q reached the identity client; the budget must be checked first", err.Error())
	}
}

// TestKeystoneCleanupStepQueuesFailedCleanup drives runIteration with a
// keystoneCleanupStep-wrapped cleanup: a failed cleanup queues the run with
// what the cleanup left behind, not what its apply created, whichever phase
// failed first, and a successful one queues nothing.
func TestKeystoneCleanupStepQueuesFailedCleanup(t *testing.T) {
	u1 := []resource.Resource{{Kind: keystone.KindUser, ID: "u1"}}
	p1 := []resource.Resource{{Kind: keystone.KindProject, ID: "p1"}}
	boom := errors.New("boom")
	for _, tc := range []struct {
		name                   string
		preflightErr, applyErr error
		remaining              []resource.Resource
		cleanupErr             error
		wantErrPrefix          string
		wantPending            []keystonePendingCleanup
	}{
		{"failed cleanup", nil, nil, p1, boom, "cleanup: ", []keystonePendingCleanup{{"run0", p1}}},
		{"failed apply and cleanup", nil, boom, p1, boom, "apply: ", []keystonePendingCleanup{{"run0", p1}}},
		{"failed pre-flight and cleanup", boom, nil, nil, boom, "pre-flight cleanup: ", []keystonePendingCleanup{{"run0", nil}}},
		{"successful cleanup", nil, nil, nil, nil, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending := &keystonePendingCleanups{max: maxPendingKeystoneCleanups}
			res := runIteration(context.Background(), iterationDeps{
				preflight: func(context.Context) (int, error) { return 0, tc.preflightErr },
				apply:     func(context.Context) ([]resource.Resource, error) { return u1, tc.applyErr },
				cleanup: keystoneCleanupStep(pending, "run0", func(context.Context, []resource.Resource) (int, []resource.Resource, error) {
					return 0, tc.remaining, tc.cleanupErr
				}),
			})
			if !reflect.DeepEqual(pending.entries, tc.wantPending) {
				t.Errorf("pending = %+v, want %+v", pending.entries, tc.wantPending)
			}
			if tc.wantErrPrefix == "" {
				if res.err != nil || pending.len() != 0 {
					t.Errorf("res.err = %v, pending.len() = %d; want nil and 0", res.err, pending.len())
				}
				return
			}
			if res.err == nil || !strings.HasPrefix(res.err.Error(), tc.wantErrPrefix) {
				t.Errorf("res.err = %v, want one starting with %q", res.err, tc.wantErrPrefix)
			}
		})
	}
}

// pendingCall is one call reclaim made to its clean function.
type pendingCall struct {
	runID     string
	remaining []resource.Resource
}

func TestKeystonePendingCleanupsReclaim(t *testing.T) {
	ua2 := resource.Resource{Kind: keystone.KindUser, ID: "ua2"}
	recA := []resource.Resource{{Kind: keystone.KindUser, ID: "ua1"}, ua2}
	recB := []resource.Resource{{Kind: keystone.KindUser, ID: "ub"}}
	p := &keystonePendingCleanups{max: maxPendingKeystoneCleanups}
	p.add("A", recA)
	p.add("B", recB)

	var calls []pendingCall
	p.reclaim(context.Background(), func(_ context.Context, runID string, remaining []resource.Resource) (int, []resource.Resource, error) {
		calls = append(calls, pendingCall{runID, remaining})
		if runID == "A" {
			return 1, []resource.Resource{ua2}, errors.New("still failing")
		}
		return 1, nil, nil
	})

	if want := []pendingCall{{"A", recA}, {"B", recB}}; !reflect.DeepEqual(calls, want) {
		t.Errorf("clean calls = %+v, want %+v", calls, want)
	}
	if want := []keystonePendingCleanup{{"A", []resource.Resource{ua2}}}; !reflect.DeepEqual(p.entries, want) {
		t.Errorf("pending = %+v, want only A, narrowed to ua2", p.entries)
	}
}

func TestKeystonePendingCleanupsEvictsOldest(t *testing.T) {
	p := &keystonePendingCleanups{max: 2}
	for _, id := range []string{"A", "B", "C"} {
		p.add(id, nil)
	}
	if want := []keystonePendingCleanup{{"B", nil}, {"C", nil}}; !reflect.DeepEqual(p.entries, want) {
		t.Errorf("pending = %+v, want B and C", p.entries)
	}
}

func TestKeystonePendingCleanupsEmpty(t *testing.T) {
	p := &keystonePendingCleanups{max: maxPendingKeystoneCleanups}
	p.reclaim(context.Background(), func(context.Context, string, []resource.Resource) (int, []resource.Resource, error) {
		t.Error("reclaim on an empty list called clean")
		return 0, nil, nil
	})
}

func TestKeystonePendingCleanupsStopsOnCancel(t *testing.T) {
	p := &keystonePendingCleanups{max: maxPendingKeystoneCleanups}
	p.add("A", nil)
	p.add("B", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p.reclaim(ctx, func(context.Context, string, []resource.Resource) (int, []resource.Resource, error) {
		t.Error("reclaim on a cancelled context called clean")
		return 0, nil, nil
	})
	if want := []keystonePendingCleanup{{"A", nil}, {"B", nil}}; !reflect.DeepEqual(p.entries, want) {
		t.Errorf("pending = %+v, want A and B kept", p.entries)
	}
}

// TestKeystoneMonitorRunOnceStopsAfterCancelledReclaim is the regression
// guard for a signal that lands while an iteration cleans up a pending run: the
// iteration must stop there instead of applying on the cancelled context,
// where Apply indexes an empty stage result and panics, and the pending run
// must be kept for the shutdown log.
func TestKeystoneMonitorRunOnceStopsAfterCancelledReclaim(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		mu       sync.Mutex
		requests []string
	)
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/v3/auth/tokens" {
			w.Header().Set("X-Subject-Token", "tok")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":{"expires_at":"2030-01-01T00:00:00Z","project":{"id":"p1"},"catalog":[{"type":"identity","endpoints":[{"interface":"public","region":"RegionOne","url":%q}]}]}}`, ts.URL+"/v3/")
			return
		}
		if r.Method == http.MethodDelete {
			cancel() // the signal lands while the reclaim deletes
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(ts.Close)
	tenantClouds(t, ts.URL+"/v3")

	s, err := keystonescenario.Parse([]byte(sampleKeystoneScenarioYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	opts := &globalOptions{osCloud: "tenant-ci", concurrency: 1, timeout: time.Second}
	recorded := []resource.Resource{{Kind: keystone.KindUser, ID: "u1", Name: "dizzy-oldrun-user-0001"}}
	pending := &keystonePendingCleanups{max: maxPendingKeystoneCleanups}
	pending.add("oldrun", recorded)

	ok := keystoneMonitorRunOnce(opts, p, nil, keystone.TierAdmin, keystone.Resolution{}, false, false, 0, pending)(ctx, 1)
	if ok {
		t.Error("an iteration cancelled during the reclaim reported ok")
	}
	if want := []keystonePendingCleanup{{"oldrun", recorded}}; !reflect.DeepEqual(pending.entries, want) {
		t.Errorf("pending = %+v, want the failed reclaim kept", pending.entries)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, r := range requests {
		if strings.HasPrefix(r, "POST ") && r != "POST /v3/auth/tokens" {
			t.Errorf("the iteration sent %s after the reclaim was cancelled", r)
		}
	}
}

// TestKeystoneReclaimDeletesOnlyWhatCleanupLeft is the regression guard for
// the reclaim of a failed iteration deleting every resource of it again: the
// iteration's cleanup deletes two users and fails on the project, so the
// reclaim sends one DELETE, for the project, and records no failed
// operation, where a DELETE of either user would answer 404.
func TestKeystoneReclaimDeletesOnlyWhatCleanupLeft(t *testing.T) {
	logs := captureLogs(t)
	var (
		mu       sync.Mutex
		deletes  []string
		deleted  = make(map[string]bool)
		p1Failed bool
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			// Every listing is empty; its JSON key is its path.
			switch r.URL.Path {
			case "/users", "/projects", "/roles", "/domains", "/role_assignments":
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, "{%q:[]}", strings.TrimPrefix(r.URL.Path, "/"))
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		mu.Lock()
		defer mu.Unlock()
		deletes = append(deletes, r.URL.Path)
		switch {
		case r.URL.Path == "/projects/p1" && !p1Failed:
			p1Failed = true
			w.WriteHeader(http.StatusServiceUnavailable)
		case deleted[r.URL.Path]:
			w.WriteHeader(http.StatusNotFound)
		default:
			deleted[r.URL.Path] = true
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer ts.Close()
	newClient := func(collector *metrics.Collector) *keystone.Client {
		gc := &gophercloud.ServiceClient{
			ProviderClient: &gophercloud.ProviderClient{},
			Endpoint:       ts.URL + "/",
		}
		return keystone.New(gc, "run0", collector)
	}

	p1 := resource.Resource{Kind: keystone.KindProject, ID: "p1", Name: "dizzy-run0-proj-0001"}
	created := []resource.Resource{
		{Kind: keystone.KindUser, ID: "u1", Name: "dizzy-run0-user-0001"},
		{Kind: keystone.KindUser, ID: "u2", Name: "dizzy-run0-user-0002"},
		p1,
	}
	pending := &keystonePendingCleanups{max: maxPendingKeystoneCleanups}
	client := newClient(metrics.NewCollector())
	step := keystoneCleanupStep(pending, "run0", func(ctx context.Context, created []resource.Resource) (int, []resource.Resource, error) {
		return keystoneexec.CleanupWithRetry(ctx, client, "run0", created, time.Second, 0)
	})
	if _, err := step(context.Background(), created); err == nil {
		t.Fatal("the iteration's cleanup: expected the project's 503 to be reported")
	}
	if want := []keystonePendingCleanup{{"run0", []resource.Resource{p1}}}; !reflect.DeepEqual(pending.entries, want) {
		t.Errorf("pending = %+v, want only the project", pending.entries)
	}
	if want := `msg="cleanup failed; retrying it before the next iteration" run=run0 pending=1 remaining=1 `; !strings.Contains(logs.String(), want) {
		t.Errorf("log %q missing %q", logs.String(), want)
	}

	mu.Lock()
	before := len(deletes)
	mu.Unlock()
	reclaimCollector := metrics.NewCollector()
	reclaimClient := newClient(reclaimCollector)
	pending.reclaim(context.Background(), func(ctx context.Context, id string, remaining []resource.Resource) (int, []resource.Resource, error) {
		return keystoneexec.CleanupWithRetry(ctx, reclaimClient, id, remaining, time.Second, 0)
	})

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"/projects/p1"}; !reflect.DeepEqual(deletes[before:], want) {
		t.Errorf("reclaim DELETEs = %v, want %v", deletes[before:], want)
	}
	if failed := reclaimCollector.Aggregate(time.Second).Overall.Failed; failed != 0 {
		t.Errorf("reclaim recorded %d failed operations, want 0", failed)
	}
	if n := pending.len(); n != 0 {
		t.Errorf("pending.len() = %d after the reclaim, want 0", n)
	}
}

// TestKeystoneTimeoutCleanerBoundsWedgedOperation is the regression guard for the
// monitor loop hanging forever on a wedged cleanup call: a blocking operation
// must be bounded by the per-op timeout.
func TestKeystoneTimeoutCleanerBoundsWedgedOperation(t *testing.T) {
	tc := keystoneTimeoutCleaner{inner: blockingKeystoneCleaner{}, opTimeout: 10 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		_, err := tc.ListUsersByPrefix(context.Background(), "run0")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the timeout cleaner did not bound a wedged operation")
	}
}

// TestKeystoneOrphanCleanerListsAnyRunByPrefix confirms the pre-flight sweep's
// adapter discovers users across every tester run (by the any-run name prefix),
// regardless of the run id it is handed.
func TestKeystoneOrphanCleanerListsAnyRunByPrefix(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"users":[
			{"id":"u-a","name":"dizzy-runA-user-0001"},
			{"id":"u-b","name":"dizzy-runB-user-0001"},
			{"id":"u-foreign","name":"admin"}
		]}`)
	}))
	defer ts.Close()

	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	orphan := keystoneOrphanCleaner{keystone.New(gc, "current-run", metrics.NewCollector())}

	// The run id argument is ignored: the sweep reclaims users from any run.
	got, err := orphan.ListUsersByPrefix(context.Background(), "current-run")
	if err != nil {
		t.Fatalf("ListUsersByPrefix: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("orphan sweep found %d users, want 2 (both tester runs, dropping the foreign admin): %+v", len(got), got)
	}
}

// TestKeystonePreflightSweepIsOptIn is the regression guard for the pre-flight
// orphan sweep deleting a concurrent tester run's resources cloud-wide: the
// cloud-wide sweep must be opt-in, so with --reclaim-orphans off the pre-flight
// is a no-op that issues no identity calls at all.
func TestKeystonePreflightSweepIsOptIn(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("pre-flight issued an identity call with --reclaim-orphans off: %s %s", r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"users":[]}`)
	}))
	defer ts.Close()

	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	client := keystone.New(gc, "run0", metrics.NewCollector())

	preflight := keystonePreflight(client, time.Second, "run0", false)
	swept, err := preflight(context.Background())
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if swept != 0 {
		t.Fatalf("swept = %d, want 0 when --reclaim-orphans is off", swept)
	}
}

// TestKeystonePreflightSweepsWhenOptedIn confirms --reclaim-orphans wires the
// any-run orphan sweep back in: the pre-flight then issues identity calls.
func TestKeystonePreflightSweepsWhenOptedIn(t *testing.T) {
	var called bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "users"):
			_, _ = io.WriteString(w, `{"users":[]}`)
		case strings.Contains(r.URL.Path, "projects"):
			_, _ = io.WriteString(w, `{"projects":[]}`)
		case strings.Contains(r.URL.Path, "roles"):
			_, _ = io.WriteString(w, `{"roles":[]}`)
		case strings.Contains(r.URL.Path, "domains"):
			_, _ = io.WriteString(w, `{"domains":[]}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer ts.Close()

	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	client := keystone.New(gc, "run0", metrics.NewCollector())

	preflight := keystonePreflight(client, time.Second, "run0", true)
	if _, err := preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !called {
		t.Fatal("pre-flight issued no identity calls with --reclaim-orphans on, want the any-run sweep")
	}
}
