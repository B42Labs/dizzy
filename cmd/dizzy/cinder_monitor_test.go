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

	"github.com/B42Labs/dizzy/internal/cinder"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

func TestCinderMonitorRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "cinder", "monitor", "--interval", "1m"); err == nil {
		t.Error("cinder monitor without --scenario: expected an error, got nil")
	}
}

func TestCinderMonitorRejectsNegativeInterval(t *testing.T) {
	path := writeScenario(t, sampleCinderScenarioYAML)
	_, err := execRoot(t, "cinder", "monitor", "--scenario", path, "--interval=-1m")
	if err == nil {
		t.Fatal("cinder monitor with a negative --interval: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "--interval") {
		t.Errorf("error = %q, want it to mention --interval", err.Error())
	}
}

func TestCinderMonitorWithValidConfigRequiresCloud(t *testing.T) {
	// Point clouds.yaml resolution at a nonexistent file so auth fails
	// deterministically, proving config validation and telemetry setup precede
	// authentication — a missing, zero, or positive interval all validate, yet
	// the command still fails only at block-storage client creation.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleCinderScenarioYAML)
	tests := []struct {
		name string
		args []string
	}{
		{"paced", []string{"cinder", "monitor", "--scenario", path, "--iterations", "1", "--interval", "1m"}},
		{"continuous by default", []string{"cinder", "monitor", "--scenario", path, "--iterations", "1"}},
		{"explicit zero interval", []string{"cinder", "monitor", "--scenario", path, "--iterations", "1", "--interval", "0"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execRoot(t, tc.args...)
			if err == nil {
				t.Fatal("cinder monitor with a valid config but no reachable cloud: expected an error, got nil")
			}
			if !strings.Contains(err.Error(), "block storage client") {
				t.Errorf("error = %q, want it to mention the block storage client (auth) step", err.Error())
			}
		})
	}
}

// blockingCinderCleaner mimics a wedged Cinder: every operation blocks until its
// context is done. Cleanup drives it on the monitor's deadline-free context, so
// without a per-operation timeout it would block forever.
type blockingCinderCleaner struct{}

func (blockingCinderCleaner) ListVolumesByMetadata(ctx context.Context, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingCinderCleaner) ListSnapshotsByMetadata(ctx context.Context, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingCinderCleaner) Delete(ctx context.Context, _ resource.Resource) error {
	<-ctx.Done()
	return ctx.Err()
}
func (blockingCinderCleaner) WaitForGone(ctx context.Context, _ resource.Resource) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestCinderTimeoutCleanerBoundsWedgedOperation is the regression guard for the
// monitor loop hanging forever on a wedged cleanup call: a blocking operation
// invoked through cinderTimeoutCleaner on a deadline-free parent context must
// return promptly with the deadline error instead of blocking indefinitely.
func TestCinderTimeoutCleanerBoundsWedgedOperation(t *testing.T) {
	tc := cinderTimeoutCleaner{inner: blockingCinderCleaner{}, opTimeout: 10 * time.Millisecond}

	done := make(chan error, 1)
	go func() {
		done <- tc.Delete(context.Background(), resource.Resource{Kind: cinder.KindVolume, ID: "v1"})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Delete err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Delete did not return; cinderTimeoutCleaner failed to bound the wedged operation")
	}
}

// TestCinderOrphanCleanerListsAnyRunByType confirms the sweep adapter ignores the
// run id it is handed and discovers this tool's volumes by the dizzy:type
// metadata across every run — the crash-recovery contract, since the pre-flight
// sweep runs with a brand-new run id but must reclaim leftovers tagged with a
// previous, now-unknown one.
func TestCinderOrphanCleanerListsAnyRunByType(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"volumes":[
			{"id":"v-foreign","name":"dizzy-run9-vol-0001","metadata":{"dizzy:run":"run9","dizzy:type":"volume"}},
			{"id":"v-untagged","name":"untagged","metadata":{}}
		]}`)
	}))
	defer ts.Close()

	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	orphan := cinderOrphanCleaner{cinder.New(gc, "run0", metrics.NewCollector())}

	// The run id passed here is deliberately not run9's — the sweep must still
	// return the foreign volume and drop the untagged one.
	got, err := orphan.ListVolumesByMetadata(context.Background(), "some-other-run")
	if err != nil {
		t.Fatalf("ListVolumesByMetadata: %v", err)
	}
	if len(got) != 1 || got[0].ID != "v-foreign" {
		t.Fatalf("orphan sweep returned %+v, want the foreign-run type-tagged volume regardless of run id", got)
	}
}
