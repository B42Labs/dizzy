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
