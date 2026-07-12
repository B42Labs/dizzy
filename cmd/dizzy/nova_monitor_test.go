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

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/nova"
	"github.com/B42Labs/dizzy/internal/resource"
)

func TestNovaSubcommandsRegistered(t *testing.T) {
	root := newRootCmd()
	novaCmd := findSubcommand(root, "nova")
	if novaCmd == nil {
		t.Fatal("nova command not registered on root")
	}

	want := []string{"generate", "apply", "chaos", "monitor", "status", "report", "cleanup"}
	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			if findSubcommand(novaCmd, name) == nil {
				t.Errorf("nova subcommand %q not registered", name)
			}
		})
	}
}

func TestNovaMonitorRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "nova", "monitor", "--interval", "1m"); err == nil {
		t.Error("nova monitor without --scenario: expected an error, got nil")
	}
}

func TestNovaMonitorRejectsNegativeInterval(t *testing.T) {
	path := writeScenario(t, sampleNovaScenarioYAML)
	_, err := execRoot(t, "nova", "monitor", "--scenario", path, "--interval=-1m")
	if err == nil {
		t.Fatal("nova monitor with a negative --interval: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "--interval") {
		t.Errorf("error = %q, want it to mention --interval", err.Error())
	}
}

func TestNovaMonitorWithValidConfigRequiresCloud(t *testing.T) {
	// Point clouds.yaml resolution at a nonexistent file so auth fails
	// deterministically, proving config validation and telemetry setup precede
	// authentication — the command fails only at compute client creation.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleNovaScenarioYAML)
	_, err := execRoot(t, "nova", "monitor", "--scenario", path, "--iterations", "1")
	if err == nil {
		t.Fatal("nova monitor with a valid config but no reachable cloud: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "compute clients") {
		t.Errorf("error = %q, want it to mention the compute client (auth) step", err.Error())
	}
}

// blockingNovaCleaner mimics a wedged compute stack: every operation blocks until
// its context is done. Cleanup drives it on the monitor's deadline-free context,
// so without a per-operation timeout it would block forever.
type blockingNovaCleaner struct{}

func (blockingNovaCleaner) ListServersByMetadata(ctx context.Context, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingNovaCleaner) ListVolumesByMetadata(ctx context.Context, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingNovaCleaner) ListByTag(ctx context.Context, _ resource.Kind, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingNovaCleaner) DeleteNetworkPorts(ctx context.Context, _ string) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
func (blockingNovaCleaner) Delete(ctx context.Context, _ resource.Resource) error {
	<-ctx.Done()
	return ctx.Err()
}
func (blockingNovaCleaner) WaitForGone(ctx context.Context, _ resource.Resource) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestNovaTimeoutCleanerBoundsWedgedOperation is the regression guard for the
// monitor loop hanging forever on a wedged cleanup call: a blocking operation
// invoked through novaTimeoutCleaner on a deadline-free parent context must
// return promptly with the deadline error instead of blocking indefinitely.
func TestNovaTimeoutCleanerBoundsWedgedOperation(t *testing.T) {
	tc := novaTimeoutCleaner{inner: blockingNovaCleaner{}, opTimeout: 10 * time.Millisecond}

	done := make(chan error, 1)
	go func() {
		done <- tc.Delete(context.Background(), resource.Resource{Kind: nova.KindServer, ID: "s1"})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Delete err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Delete did not return; novaTimeoutCleaner failed to bound the wedged operation")
	}
}

// TestNovaOrphanCleanerListsAnyRunByType confirms the sweep adapter ignores the
// run id it is handed and discovers this tool's resources by the dizzy:type
// identity across every run — the crash-recovery contract, since the pre-flight
// sweep runs with a brand-new run id but must reclaim leftovers tagged with a
// previous, now-unknown one. It covers a metadata kind (servers), a metadata
// kind (volumes), and a tag kind (networks).
func TestNovaOrphanCleanerListsAnyRunByType(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "servers"):
			_, _ = io.WriteString(w, `{"servers":[
				{"id":"s-foreign","name":"dizzy-run9-srv-0001","metadata":{"dizzy:run":"run9","dizzy:type":"server"}},
				{"id":"s-untagged","name":"untagged","metadata":{}}
			]}`)
		case strings.Contains(r.URL.Path, "volumes"):
			_, _ = io.WriteString(w, `{"volumes":[
				{"id":"v-foreign","name":"dizzy-run9-vol-0001","metadata":{"dizzy:run":"run9","dizzy:type":"volume"}},
				{"id":"v-untagged","name":"untagged","metadata":{}}
			]}`)
		case strings.Contains(r.URL.Path, "networks"):
			// Neutron filters by tag server-side, so every returned network matches.
			_, _ = io.WriteString(w, `{"networks":[{"id":"n-foreign","name":"dizzy-run9-net-0001"}]}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer ts.Close()

	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	// All three clients share the endpoint; the handler routes by path.
	client := nova.New(gc, gc, gc, "run0", metrics.NewCollector())
	orphan := novaOrphanCleaner{client}

	// The run id passed here is deliberately not run9's — the sweep must still
	// return the foreign resources and drop the untagged ones.
	servers, err := orphan.ListServersByMetadata(context.Background(), "some-other-run")
	if err != nil {
		t.Fatalf("ListServersByMetadata: %v", err)
	}
	if len(servers) != 1 || servers[0].ID != "s-foreign" {
		t.Fatalf("server sweep returned %+v, want the foreign type-tagged server regardless of run id", servers)
	}

	volumes, err := orphan.ListVolumesByMetadata(context.Background(), "some-other-run")
	if err != nil {
		t.Fatalf("ListVolumesByMetadata: %v", err)
	}
	if len(volumes) != 1 || volumes[0].ID != "v-foreign" {
		t.Fatalf("volume sweep returned %+v, want the foreign type-tagged volume", volumes)
	}

	networks, err := orphan.ListByTag(context.Background(), nova.KindNetwork, "some-other-run")
	if err != nil {
		t.Fatalf("ListByTag(network): %v", err)
	}
	if len(networks) != 1 || networks[0].ID != "n-foreign" {
		t.Fatalf("network sweep returned %+v, want the foreign type-tagged network", networks)
	}
}
