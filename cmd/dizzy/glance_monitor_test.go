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

	"github.com/B42Labs/dizzy/internal/glance"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

func TestGlanceSubcommandsRegistered(t *testing.T) {
	root := newRootCmd()
	glanceCmd := findSubcommand(root, "glance")
	if glanceCmd == nil {
		t.Fatal("glance command not registered on root")
	}

	want := []string{"generate", "apply", "chaos", "monitor", "status", "report", "cleanup"}
	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			if findSubcommand(glanceCmd, name) == nil {
				t.Errorf("glance subcommand %q not registered", name)
			}
		})
	}
}

func TestGlanceMonitorRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "glance", "monitor", "--interval", "1m"); err == nil {
		t.Error("glance monitor without --scenario: expected an error, got nil")
	}
}

func TestGlanceMonitorRejectsNegativeInterval(t *testing.T) {
	path := writeScenario(t, sampleGlanceScenarioYAML)
	_, err := execRoot(t, "glance", "monitor", "--scenario", path, "--interval=-1m")
	if err == nil {
		t.Fatal("glance monitor with a negative --interval: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "--interval") {
		t.Errorf("error = %q, want it to mention --interval", err.Error())
	}
}

func TestGlanceMonitorWithValidConfigRequiresCloud(t *testing.T) {
	// Point clouds.yaml resolution at a nonexistent file so auth fails
	// deterministically, proving config validation and telemetry setup precede
	// authentication — the command fails only at image client creation.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleGlanceScenarioYAML)
	_, err := execRoot(t, "glance", "monitor", "--scenario", path, "--iterations", "1")
	if err == nil {
		t.Fatal("glance monitor with a valid config but no reachable cloud: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "image client") {
		t.Errorf("error = %q, want it to mention the image client (auth) step", err.Error())
	}
}

// blockingGlanceCleaner mimics a wedged image stack: every operation blocks until
// its context is done. Cleanup drives it on the monitor's deadline-free context,
// so without a per-operation timeout it would block forever.
type blockingGlanceCleaner struct{}

func (blockingGlanceCleaner) ListImagesByTag(ctx context.Context, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingGlanceCleaner) Delete(ctx context.Context, _ resource.Resource) error {
	<-ctx.Done()
	return ctx.Err()
}
func (blockingGlanceCleaner) WaitForGone(ctx context.Context, _ resource.Resource) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestGlanceTimeoutCleanerBoundsWedgedOperation is the regression guard for the
// monitor loop hanging forever on a wedged cleanup call: a blocking operation
// invoked through glanceTimeoutCleaner on a deadline-free parent context must
// return promptly with the deadline error instead of blocking indefinitely.
func TestGlanceTimeoutCleanerBoundsWedgedOperation(t *testing.T) {
	tc := glanceTimeoutCleaner{inner: blockingGlanceCleaner{}, opTimeout: 10 * time.Millisecond}

	done := make(chan error, 1)
	go func() {
		done <- tc.Delete(context.Background(), resource.Resource{Kind: glance.KindImage, ID: "i1"})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Delete err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Delete did not return; glanceTimeoutCleaner failed to bound the wedged operation")
	}
}

// TestGlanceOrphanCleanerListsAnyRunByType confirms the sweep adapter ignores the
// run id it is handed and discovers this tool's images by the dizzy:type identity
// across every run — the crash-recovery contract, since the pre-flight sweep runs
// with a brand-new run id but must reclaim leftovers tagged with a previous, now-
// unknown one.
func TestGlanceOrphanCleanerListsAnyRunByType(t *testing.T) {
	var gotTags []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		gotTags = r.URL.Query()["tag"]
		// Glance filters by tag server-side, so every returned image matches.
		_, _ = io.WriteString(w, `{"images":[{"id":"i-foreign","name":"dizzy-run9-img-0001","status":"active"}]}`)
	}))
	defer ts.Close()

	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	client := glance.New(gc, "run0", metrics.NewCollector())
	orphan := glanceOrphanCleaner{client}

	// The run id passed here is deliberately not run9's — the sweep must still
	// return the foreign image, discovered by the dizzy:type tag.
	images, err := orphan.ListImagesByTag(context.Background(), "some-other-run")
	if err != nil {
		t.Fatalf("ListImagesByTag: %v", err)
	}
	if len(images) != 1 || images[0].ID != "i-foreign" {
		t.Fatalf("image sweep returned %+v, want the foreign type-tagged image regardless of run id", images)
	}
	if len(gotTags) != 1 || gotTags[0] != "dizzy:type=image" {
		t.Errorf("tag query = %v, want [dizzy:type=image] (the type sweep, not a run tag)", gotTags)
	}
}
