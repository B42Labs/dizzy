package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/keystone"
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
// what its apply created, whichever phase failed first, and a successful one
// queues nothing.
func TestKeystoneCleanupStepQueuesFailedCleanup(t *testing.T) {
	u1 := []resource.Resource{{Kind: keystone.KindUser, ID: "u1"}}
	boom := errors.New("boom")
	for _, tc := range []struct {
		name                   string
		preflightErr, applyErr error
		cleanupErr             error
		wantErrPrefix          string
		wantPending            []keystonePendingCleanup
	}{
		{"failed cleanup", nil, nil, boom, "cleanup: ", []keystonePendingCleanup{{"run0", u1}}},
		{"failed apply and cleanup", nil, boom, boom, "apply: ", []keystonePendingCleanup{{"run0", u1}}},
		{"failed pre-flight and cleanup", boom, nil, boom, "pre-flight cleanup: ", []keystonePendingCleanup{{"run0", nil}}},
		{"successful cleanup", nil, nil, nil, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending := &keystonePendingCleanups{max: maxPendingKeystoneCleanups}
			res := runIteration(context.Background(), iterationDeps{
				preflight: func(context.Context) (int, error) { return 0, tc.preflightErr },
				apply:     func(context.Context) ([]resource.Resource, error) { return u1, tc.applyErr },
				cleanup: keystoneCleanupStep(pending, "run0", func(context.Context, []resource.Resource) (int, error) {
					return 0, tc.cleanupErr
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
	runID    string
	recorded []resource.Resource
}

func TestKeystonePendingCleanupsReclaim(t *testing.T) {
	recA := []resource.Resource{{Kind: keystone.KindUser, ID: "ua"}}
	recB := []resource.Resource{{Kind: keystone.KindUser, ID: "ub"}}
	p := &keystonePendingCleanups{max: maxPendingKeystoneCleanups}
	p.add("A", recA)
	p.add("B", recB)

	var calls []pendingCall
	p.reclaim(context.Background(), func(_ context.Context, runID string, recorded []resource.Resource) (int, error) {
		calls = append(calls, pendingCall{runID, recorded})
		if runID == "A" {
			return 0, errors.New("still failing")
		}
		return 1, nil
	})

	if want := []pendingCall{{"A", recA}, {"B", recB}}; !reflect.DeepEqual(calls, want) {
		t.Errorf("clean calls = %+v, want %+v", calls, want)
	}
	if want := []keystonePendingCleanup{{"A", recA}}; !reflect.DeepEqual(p.entries, want) {
		t.Errorf("pending = %+v, want only A", p.entries)
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
	p.reclaim(context.Background(), func(context.Context, string, []resource.Resource) (int, error) {
		t.Error("reclaim on an empty list called clean")
		return 0, nil
	})
}

func TestKeystonePendingCleanupsStopsOnCancel(t *testing.T) {
	p := &keystonePendingCleanups{max: maxPendingKeystoneCleanups}
	p.add("A", nil)
	p.add("B", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p.reclaim(ctx, func(context.Context, string, []resource.Resource) (int, error) {
		t.Error("reclaim on a cancelled context called clean")
		return 0, nil
	})
	if want := []keystonePendingCleanup{{"A", nil}, {"B", nil}}; !reflect.DeepEqual(p.entries, want) {
		t.Errorf("pending = %+v, want A and B kept", p.entries)
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
