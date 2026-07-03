package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/openstack-tester/internal/keystone"
	"github.com/B42Labs/openstack-tester/internal/metrics"
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
			{"id":"u-a","name":"ostester-runA-user-0001"},
			{"id":"u-b","name":"ostester-runB-user-0001"},
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
