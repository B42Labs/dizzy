package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/cinder"
	cinderplan "github.com/B42Labs/dizzy/internal/cinder/plan"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
)

// recordingCinderCleaner is a fake cinderexec.Cleaner that records what it
// deletes and whether the contexts it is handed were cancelled, the
// block-storage twin of recordingCleaner. Its metadata listings return nothing,
// so the only deletes are the recorded resources passed through the created
// list — which lets a test assert snapshots are deleted before their volumes.
type recordingCinderCleaner struct {
	recordWritten      *bool
	firstCallSawRecord bool
	calls              int
	sawCancelled       bool
	deleted            []resource.Resource
}

func (c *recordingCinderCleaner) observe(ctx context.Context) {
	c.calls++
	if c.calls == 1 && c.recordWritten != nil {
		c.firstCallSawRecord = *c.recordWritten
	}
	if ctx.Err() != nil {
		c.sawCancelled = true
	}
}

func (c *recordingCinderCleaner) ListVolumesByMetadata(ctx context.Context, _ string) ([]resource.Resource, error) {
	c.observe(ctx)
	return nil, nil
}

func (c *recordingCinderCleaner) ListSnapshotsByMetadata(ctx context.Context, _ string) ([]resource.Resource, error) {
	c.observe(ctx)
	return nil, nil
}

func (c *recordingCinderCleaner) Delete(ctx context.Context, r resource.Resource) error {
	c.observe(ctx)
	c.deleted = append(c.deleted, r)
	return nil
}

func (c *recordingCinderCleaner) WaitForGone(ctx context.Context, _ resource.Resource) error {
	c.observe(ctx)
	return nil
}

// sampleCinderScenarioYAML is a small but complete Cinder scenario used by the
// cinder command tests.
const sampleCinderScenarioYAML = `
name: cli
seed: 5
resources:
  volumes: 4
distribution:
  volume_size_gib:      { min: 1, max: 3 }
  volume_resized_ratio: 0.5
  resize_growth_gib:    { min: 1, max: 2 }
  snapshots_per_volume: { min: 0, max: 2 }
`

func TestCinderSubcommandsRegistered(t *testing.T) {
	root := newRootCmd()
	cinder := findSubcommand(root, "cinder")
	if cinder == nil {
		t.Fatal("cinder command not registered on root")
	}

	want := []string{"generate", "apply", "chaos", "monitor", "status", "report", "cleanup"}
	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			if findSubcommand(cinder, name) == nil {
				t.Errorf("cinder subcommand %q not registered", name)
			}
		})
	}
}

func TestCinderGenerateStdout(t *testing.T) {
	path := writeScenario(t, sampleCinderScenarioYAML)

	out, err := execRoot(t, "cinder", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	var p cinderplan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if len(p.Volumes) != 4 {
		t.Errorf("volumes = %d, want 4", len(p.Volumes))
	}
}

func TestCinderGenerateSetOverride(t *testing.T) {
	path := writeScenario(t, sampleCinderScenarioYAML)

	out, err := execRoot(t, "cinder", "generate", "--scenario", path, "--set", "resources.volumes=2")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var p cinderplan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if len(p.Volumes) != 2 {
		t.Errorf("volumes = %d, want 2 after override", len(p.Volumes))
	}
}

func TestCinderGenerateSeedOverride(t *testing.T) {
	path := writeScenario(t, sampleCinderScenarioYAML)

	base, err := execRoot(t, "cinder", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate (file seed): %v", err)
	}
	overridden, err := execRoot(t, "cinder", "generate", "--scenario", path, "--seed", "999")
	if err != nil {
		t.Fatalf("generate (--seed): %v", err)
	}
	if base == overridden {
		t.Error("global --seed did not change the generated plan")
	}
}

func TestCinderGenerateErrors(t *testing.T) {
	valid := writeScenario(t, sampleCinderScenarioYAML)
	// A neutron key in a cinder scenario must fail strict parse.
	wrongSchema := writeScenario(t, "name: bad\nresources:\n  networks: 1\n")

	tests := []struct {
		name string
		args []string
	}{
		{"missing scenario flag", []string{"cinder", "generate"}},
		{"nonexistent file", []string{"cinder", "generate", "--scenario", filepath.Join(t.TempDir(), "nope.yaml")}},
		{"wrong-schema scenario", []string{"cinder", "generate", "--scenario", wrongSchema}},
		{"unknown set key", []string{"cinder", "generate", "--scenario", valid, "--set", "resources.nope=1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := execRoot(t, tc.args...); err == nil {
				t.Errorf("expected error for args %v, got nil", tc.args)
			}
		})
	}
}

func TestCinderApplyDryRunSummaryNoAPICall(t *testing.T) {
	// Point cloud configuration at nothing: succeeding without a reachable cloud
	// proves the dry-run path makes zero auth/API calls.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleCinderScenarioYAML)

	out, err := execRoot(t, "cinder", "apply", "--scenario", path, "--dry-run")
	if err != nil {
		t.Fatalf("apply --dry-run: %v", err)
	}
	for _, want := range []string{`scenario "cli"`, "volumes:", "snapshots:", "total GiB:"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestCinderApplyWithoutDryRunRequiresCloud(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleCinderScenarioYAML)

	_, err := execRoot(t, "cinder", "apply", "--scenario", path)
	if err == nil {
		t.Fatal("apply without --dry-run: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "block storage client") {
		t.Errorf("error %q does not mention block storage client creation", err.Error())
	}
}

func TestCinderApplyKeepOnAbortFlagParses(t *testing.T) {
	// --dry-run returns before signal setup, so a successful parse proves the
	// flag is registered without needing a cloud.
	path := writeScenario(t, sampleCinderScenarioYAML)
	if _, err := execRoot(t, "cinder", "apply", "--scenario", path, "--dry-run", "--keep-on-abort"); err != nil {
		t.Fatalf("cinder apply --dry-run --keep-on-abort: %v", err)
	}
}

func TestFinishAbortedCinderApplyTearsDownOnLiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a first signal cancelled the run

	recordWritten := false
	c := &recordingCinderCleaner{recordWritten: &recordWritten}
	// Snapshots must be deleted before their source volumes.
	created := []resource.Resource{
		{Kind: cinder.KindVolume, ID: "vol1"},
		{Kind: cinder.KindSnapshot, ID: "snap1"},
	}
	var out bytes.Buffer
	writeRecord := func() (string, error) {
		recordWritten = true
		return "run-abcd1234.json", nil
	}

	err := finishAbortedCinderApply(ctx, &out, c, "abcd1234", created, false, errors.New("apply boom"), time.Second, writeRecord)

	if err == nil {
		t.Fatal("finishAbortedCinderApply returned nil; an interrupted apply must exit non-zero")
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
	if len(c.deleted) != 2 || c.deleted[0].ID != "snap1" || c.deleted[1].ID != "vol1" {
		t.Errorf("deleted = %v, want the snapshot deleted before its volume", c.deleted)
	}
	if !strings.Contains(out.String(), "deleted 2 resource(s)") {
		t.Errorf("output %q missing the deletion count", out.String())
	}
}

func TestFinishAbortedCinderApplyBoundsWedgedTeardown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the signal context is already cancelled at teardown time

	writeRecord := func() (string, error) { return "run-wedged.json", nil }

	done := make(chan error, 1)
	go func() {
		done <- finishAbortedCinderApply(ctx, io.Discard, blockingCinderCleaner{}, "wedged", nil, false, errors.New("apply boom"), 10*time.Millisecond, writeRecord)
	}()

	select {
	case err := <-done:
		// DeadlineExceeded (not Canceled) proves both the context.WithoutCancel and
		// the per-op timeout bound.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("finishAbortedCinderApply did not return; teardown was not bounded")
	}
}

func TestFinishAbortedCinderApplyKeepOnAbortLeavesResources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name       string
		recordPath string
		wantHint   string
	}{
		{"with record path", "run-keep.json", "cinder cleanup --run run-keep.json"},
		{"empty record path falls back to run-id", "", "cinder cleanup --run-id keep1234"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingCinderCleaner{}
			var out bytes.Buffer
			writeRecord := func() (string, error) { return tc.recordPath, nil }

			err := finishAbortedCinderApply(ctx, &out, c, "keep1234", nil, true, errors.New("apply boom"), time.Second, writeRecord)

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

func TestCinderStatusRequiresRunFlag(t *testing.T) {
	if _, err := execRoot(t, "cinder", "status"); err == nil {
		t.Fatal("status without --run: expected error, got nil")
	}
}

func TestCinderCleanupRequiresRunOrRunID(t *testing.T) {
	if _, err := execRoot(t, "cinder", "cleanup"); err == nil {
		t.Fatal("cleanup with neither --run nor --run-id: expected error, got nil")
	}
}

// TestCinderRejectsNeutronRecord confirms the service guard stops a cinder
// command from operating on a neutron run record (whose resource kinds the
// cinder client cannot handle), and vice versa.
func TestCinderRejectsNeutronRecord(t *testing.T) {
	dir := t.TempDir()
	if _, err := run.Write(dir, &run.Record{RunID: "neu00001", Service: "neutron"}); err != nil {
		t.Fatalf("seeding neutron record: %v", err)
	}
	if _, err := run.Write(dir, &run.Record{RunID: "cin00001", Service: "cinder"}); err != nil {
		t.Fatalf("seeding cinder record: %v", err)
	}
	neutronRec := filepath.Join(dir, "run-neu00001.json")
	cinderRec := filepath.Join(dir, "run-cin00001.json")

	// cinder status/cleanup must reject a neutron record before touching a cloud.
	if _, err := execRoot(t, "cinder", "status", "--run", neutronRec); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("cinder status on a neutron record: err = %v, want a service mismatch error", err)
	}
	if _, err := execRoot(t, "cinder", "cleanup", "--run", neutronRec); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("cinder cleanup on a neutron record: err = %v, want a service mismatch error", err)
	}
	// neutron status/cleanup must reject a cinder record symmetrically.
	if _, err := execRoot(t, "neutron", "status", "--run", cinderRec); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("neutron status on a cinder record: err = %v, want a service mismatch error", err)
	}
	if _, err := execRoot(t, "neutron", "cleanup", "--run", cinderRec); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("neutron cleanup on a cinder record: err = %v, want a service mismatch error", err)
	}
}

// TestNeutronAcceptsLegacyRecord confirms a pre-Cinder record (empty service
// field) still passes the neutron service guard, so old records keep working.
func TestNeutronAcceptsLegacyRecord(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	dir := t.TempDir()
	// No Service field: a legacy record read as neutron.
	if _, err := run.Write(dir, &run.Record{RunID: "legacy01"}); err != nil {
		t.Fatalf("seeding legacy record: %v", err)
	}
	rec := filepath.Join(dir, "run-legacy01.json")

	// The guard must pass; the command then fails only at cloud-client creation.
	_, err := execRoot(t, "neutron", "status", "--run", rec)
	if err == nil {
		t.Fatal("expected a cloud-auth failure, got nil")
	}
	if strings.Contains(err.Error(), "service") {
		t.Errorf("legacy record rejected by the service guard: %v", err)
	}
	if !strings.Contains(err.Error(), "network client") {
		t.Errorf("error %q does not mention network client creation", err.Error())
	}
}
