package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/nova"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
)

// recordingNovaCleaner is a fake novaexec.Cleaner that records what it deletes
// and whether the contexts it is handed were cancelled. Its identity listings
// return nothing, so the only deletes are the recorded resources passed through
// the created list — which lets a test assert servers are deleted before their
// companions.
type recordingNovaCleaner struct {
	recordWritten      *bool
	firstCallSawRecord bool
	calls              int
	sawCancelled       bool
	deleted            []resource.Resource
}

func (c *recordingNovaCleaner) observe(ctx context.Context) {
	c.calls++
	if c.calls == 1 && c.recordWritten != nil {
		c.firstCallSawRecord = *c.recordWritten
	}
	if ctx.Err() != nil {
		c.sawCancelled = true
	}
}

func (c *recordingNovaCleaner) ListServersByMetadata(ctx context.Context, _ string) ([]resource.Resource, error) {
	c.observe(ctx)
	return nil, nil
}
func (c *recordingNovaCleaner) ListVolumesByMetadata(ctx context.Context, _ string) ([]resource.Resource, error) {
	c.observe(ctx)
	return nil, nil
}
func (c *recordingNovaCleaner) ListByTag(ctx context.Context, _ resource.Kind, _ string) ([]resource.Resource, error) {
	c.observe(ctx)
	return nil, nil
}
func (c *recordingNovaCleaner) DeleteNetworkPorts(ctx context.Context, _ string) (int, error) {
	c.observe(ctx)
	return 0, nil
}
func (c *recordingNovaCleaner) Delete(ctx context.Context, r resource.Resource) error {
	c.observe(ctx)
	c.deleted = append(c.deleted, r)
	return nil
}
func (c *recordingNovaCleaner) WaitForGone(ctx context.Context, _ resource.Resource) error {
	c.observe(ctx)
	return nil
}

// sampleNovaScenarioYAML is a small but complete Nova scenario used by the nova
// command tests.
const sampleNovaScenarioYAML = `
name: cli
seed: 5
image: cirros
flavor: m1.tiny
resize_flavor: m1.small
resources:
  servers: 4
  networks: 2
distribution:
  networks_per_server:    { min: 1, max: 2 }
  volumes_per_server:     { min: 0, max: 1 }
  ports_per_server:       { min: 0, max: 1 }
  attached_volume_gib:    { min: 1, max: 2 }
  root_volume_gib:        { min: 2, max: 4 }
  boot_from_volume_ratio: 0.3
  resized_ratio:          0.3
`

func TestNovaGenerateStdout(t *testing.T) {
	path := writeScenario(t, sampleNovaScenarioYAML)

	out, err := execRoot(t, "nova", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	var p novaplan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if len(p.Servers) != 4 {
		t.Errorf("servers = %d, want 4", len(p.Servers))
	}
}

func TestNovaGenerateSetOverride(t *testing.T) {
	path := writeScenario(t, sampleNovaScenarioYAML)

	out, err := execRoot(t, "nova", "generate", "--scenario", path, "--set", "resources.servers=2")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var p novaplan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if len(p.Servers) != 2 {
		t.Errorf("servers = %d, want 2 after override", len(p.Servers))
	}
}

func TestNovaGenerateSeedOverride(t *testing.T) {
	path := writeScenario(t, sampleNovaScenarioYAML)

	base, err := execRoot(t, "nova", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate (file seed): %v", err)
	}
	overridden, err := execRoot(t, "nova", "generate", "--scenario", path, "--seed", "999")
	if err != nil {
		t.Fatalf("generate (--seed): %v", err)
	}
	if base == overridden {
		t.Error("global --seed did not change the generated plan")
	}
}

func TestNovaGenerateErrors(t *testing.T) {
	valid := writeScenario(t, sampleNovaScenarioYAML)
	// A cinder key in a nova scenario must fail strict parse.
	wrongSchema := writeScenario(t, "name: bad\nresources:\n  volumes: 1\n")

	tests := []struct {
		name string
		args []string
	}{
		{"missing scenario flag", []string{"nova", "generate"}},
		{"nonexistent file", []string{"nova", "generate", "--scenario", filepath.Join(t.TempDir(), "nope.yaml")}},
		{"wrong-schema scenario", []string{"nova", "generate", "--scenario", wrongSchema}},
		{"unknown set key", []string{"nova", "generate", "--scenario", valid, "--set", "resources.nope=1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := execRoot(t, tc.args...); err == nil {
				t.Errorf("expected error for args %v, got nil", tc.args)
			}
		})
	}
}

func TestNovaApplyDryRunSummaryNoAPICall(t *testing.T) {
	// Point cloud configuration at nothing: succeeding without a reachable cloud
	// proves the dry-run path makes zero auth/API calls.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleNovaScenarioYAML)

	out, err := execRoot(t, "nova", "apply", "--scenario", path, "--dry-run")
	if err != nil {
		t.Fatalf("apply --dry-run: %v", err)
	}
	for _, want := range []string{`scenario "cli"`, "servers:", "networks:", "resizes:", "image:"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestNovaApplyWithoutDryRunRequiresCloud(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleNovaScenarioYAML)

	_, err := execRoot(t, "nova", "apply", "--scenario", path)
	if err == nil {
		t.Fatal("apply without --dry-run: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "compute clients") {
		t.Errorf("error %q does not mention compute client creation", err.Error())
	}
}

func TestNovaApplyKeepOnAbortFlagParses(t *testing.T) {
	// --dry-run returns before signal setup, so a successful parse proves the flag
	// is registered without needing a cloud.
	path := writeScenario(t, sampleNovaScenarioYAML)
	if _, err := execRoot(t, "nova", "apply", "--scenario", path, "--dry-run", "--keep-on-abort"); err != nil {
		t.Fatalf("nova apply --dry-run --keep-on-abort: %v", err)
	}
}

func TestFinishAbortedNovaApplyTearsDownOnLiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a first signal cancelled the run

	recordWritten := false
	c := &recordingNovaCleaner{recordWritten: &recordWritten}
	// Servers must be deleted before their companion networks/volumes.
	created := []resource.Resource{
		{Kind: nova.KindNetwork, ID: "net1"},
		{Kind: nova.KindServer, ID: "srv1"},
	}
	var out bytes.Buffer
	writeRecord := func() (string, error) {
		recordWritten = true
		return "run-abcd1234.json", nil
	}

	err := finishAbortedNovaApply(ctx, &out, c, "abcd1234", created, false, errors.New("apply boom"), time.Second, writeRecord)

	if err == nil {
		t.Fatal("finishAbortedNovaApply returned nil; an interrupted apply must exit non-zero")
	}
	if !strings.Contains(err.Error(), "abcd1234") {
		t.Errorf("error %q does not name the run id", err)
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("error %q does not signal the interruption", err)
	}
	if c.sawCancelled {
		t.Error("teardown ran with a cancelled context; it must run on context.WithoutCancel")
	}
	if !c.firstCallSawRecord {
		t.Error("the run record was not written before teardown began")
	}
	// The server was deleted before the network.
	if len(c.deleted) != 2 || c.deleted[0].ID != "srv1" || c.deleted[1].ID != "net1" {
		t.Errorf("deleted = %v, want the server deleted before its network", c.deleted)
	}
}

func TestFinishAbortedNovaApplyKeepOnAbortLeavesResources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name       string
		recordPath string
		wantHint   string
	}{
		{"with record path", "run-keep.json", "nova cleanup --run run-keep.json"},
		{"empty record path falls back to run-id", "", "nova cleanup --run-id keep1234"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingNovaCleaner{}
			var out bytes.Buffer
			writeRecord := func() (string, error) { return tc.recordPath, nil }

			err := finishAbortedNovaApply(ctx, &out, c, "keep1234", nil, true, errors.New("apply boom"), time.Second, writeRecord)

			if err == nil || !strings.Contains(err.Error(), "keep1234") {
				t.Errorf("error = %v, want a non-nil error naming the run id", err)
			}
			if c.calls != 0 {
				t.Errorf("cleaner was called %d times; --keep-on-abort must not tear anything down", c.calls)
			}
			if !strings.Contains(out.String(), tc.wantHint) {
				t.Errorf("output %q missing hint %q", out.String(), tc.wantHint)
			}
		})
	}
}

func TestNovaStatusRequiresRunFlag(t *testing.T) {
	if _, err := execRoot(t, "nova", "status"); err == nil {
		t.Fatal("status without --run: expected error, got nil")
	}
}

func TestNovaCleanupRequiresRunOrRunID(t *testing.T) {
	if _, err := execRoot(t, "nova", "cleanup"); err == nil {
		t.Fatal("cleanup with neither --run nor --run-id: expected error, got nil")
	}
}

// TestNovaRejectsCinderRecord confirms the service guard stops a nova command
// from operating on a cinder run record, whose resource kinds the nova client
// cannot handle.
func TestNovaRejectsCinderRecord(t *testing.T) {
	dir := t.TempDir()
	if _, err := run.Write(dir, &run.Record{RunID: "cin00001", Service: "cinder"}); err != nil {
		t.Fatalf("seeding cinder record: %v", err)
	}
	cinderRec := filepath.Join(dir, "run-cin00001.json")

	if _, err := execRoot(t, "nova", "status", "--run", cinderRec); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("nova status on a cinder record: err = %v, want a service mismatch error", err)
	}
	if _, err := execRoot(t, "nova", "cleanup", "--run", cinderRec); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("nova cleanup on a cinder record: err = %v, want a service mismatch error", err)
	}
}
