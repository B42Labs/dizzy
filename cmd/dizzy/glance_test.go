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

	"github.com/B42Labs/dizzy/internal/glance"
	glanceplan "github.com/B42Labs/dizzy/internal/glance/plan"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
)

// recordingGlanceCleaner is a fake glanceexec.Cleaner that records what it
// deletes and whether the contexts it is handed were cancelled. Its tag listing
// returns nothing, so the only deletes are the recorded images passed through the
// created list.
type recordingGlanceCleaner struct {
	recordWritten      *bool
	firstCallSawRecord bool
	calls              int
	sawCancelled       bool
	deleted            []resource.Resource
}

func (c *recordingGlanceCleaner) observe(ctx context.Context) {
	c.calls++
	if c.calls == 1 && c.recordWritten != nil {
		c.firstCallSawRecord = *c.recordWritten
	}
	if ctx.Err() != nil {
		c.sawCancelled = true
	}
}

func (c *recordingGlanceCleaner) ListImagesByTag(ctx context.Context, _ string) ([]resource.Resource, error) {
	c.observe(ctx)
	return nil, nil
}
func (c *recordingGlanceCleaner) Delete(ctx context.Context, r resource.Resource) error {
	c.observe(ctx)
	c.deleted = append(c.deleted, r)
	return nil
}
func (c *recordingGlanceCleaner) WaitForGone(ctx context.Context, _ resource.Resource) error {
	c.observe(ctx)
	return nil
}

// sampleGlanceScenarioYAML is a small but complete Glance scenario used by the
// glance command tests.
const sampleGlanceScenarioYAML = `
name: cli
seed: 5
resources:
  images: 4
distribution:
  image_size_mib:        { min: 1, max: 2 }
  metadata_update_ratio: 0.5
  shared_ratio:          0.5
  member_accept_ratio:   0.5
  member_remove_ratio:   0.5
  community_ratio:       0.25
  deactivate_ratio:      0.3
  deleted_ratio:         0.3
`

func TestGlanceGenerateStdout(t *testing.T) {
	path := writeScenario(t, sampleGlanceScenarioYAML)

	out, err := execRoot(t, "glance", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	var p glanceplan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if len(p.Images) != 4 {
		t.Errorf("images = %d, want 4", len(p.Images))
	}
}

func TestGlanceGenerateSetOverride(t *testing.T) {
	path := writeScenario(t, sampleGlanceScenarioYAML)

	out, err := execRoot(t, "glance", "generate", "--scenario", path, "--set", "resources.images=2")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var p glanceplan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if len(p.Images) != 2 {
		t.Errorf("images = %d, want 2 after override", len(p.Images))
	}
}

func TestGlanceGenerateSeedOverride(t *testing.T) {
	path := writeScenario(t, sampleGlanceScenarioYAML)

	base, err := execRoot(t, "glance", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate (file seed): %v", err)
	}
	overridden, err := execRoot(t, "glance", "generate", "--scenario", path, "--seed", "999")
	if err != nil {
		t.Fatalf("generate (--seed): %v", err)
	}
	if base == overridden {
		t.Error("global --seed did not change the generated plan")
	}
}

func TestGlanceGenerateErrors(t *testing.T) {
	valid := writeScenario(t, sampleGlanceScenarioYAML)
	// A nova key in a glance scenario must fail strict parse.
	wrongSchema := writeScenario(t, "name: bad\nresources:\n  servers: 1\n")

	tests := []struct {
		name string
		args []string
	}{
		{"missing scenario flag", []string{"glance", "generate"}},
		{"nonexistent file", []string{"glance", "generate", "--scenario", filepath.Join(t.TempDir(), "nope.yaml")}},
		{"wrong-schema scenario", []string{"glance", "generate", "--scenario", wrongSchema}},
		{"unknown set key", []string{"glance", "generate", "--scenario", valid, "--set", "resources.nope=1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := execRoot(t, tc.args...); err == nil {
				t.Errorf("expected error for args %v, got nil", tc.args)
			}
		})
	}
}

func TestGlanceApplyDryRunSummaryNoAPICall(t *testing.T) {
	// Point cloud configuration at nothing: succeeding without a reachable cloud
	// proves the dry-run path makes zero auth/API calls.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleGlanceScenarioYAML)

	out, err := execRoot(t, "glance", "apply", "--scenario", path, "--dry-run")
	if err != nil {
		t.Fatalf("apply --dry-run: %v", err)
	}
	for _, want := range []string{`scenario "cli"`, "images:", "upload total:", "shared:", "deletes:"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestGlanceApplyWithoutDryRunRequiresCloud(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleGlanceScenarioYAML)

	_, err := execRoot(t, "glance", "apply", "--scenario", path)
	if err == nil {
		t.Fatal("apply without --dry-run: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "image client") {
		t.Errorf("error %q does not mention image client creation", err.Error())
	}
}

func TestGlanceApplyKeepOnAbortFlagParses(t *testing.T) {
	// --dry-run returns before signal setup, so a successful parse proves the flag
	// is registered without needing a cloud.
	path := writeScenario(t, sampleGlanceScenarioYAML)
	if _, err := execRoot(t, "glance", "apply", "--scenario", path, "--dry-run", "--keep-on-abort"); err != nil {
		t.Fatalf("glance apply --dry-run --keep-on-abort: %v", err)
	}
}

func TestFinishAbortedGlanceApplyTearsDownOnLiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a first signal cancelled the run

	recordWritten := false
	c := &recordingGlanceCleaner{recordWritten: &recordWritten}
	created := []resource.Resource{
		{Kind: glance.KindImage, ID: "i1"},
		{Kind: glance.KindImage, ID: "i2"},
	}
	var out bytes.Buffer
	writeRecord := func() (string, error) {
		recordWritten = true
		return "run-abcd1234.json", nil
	}

	err := finishAbortedGlanceApply(ctx, &out, c, "abcd1234", created, false, errors.New("apply boom"), 1, time.Second, writeRecord)

	if err == nil {
		t.Fatal("finishAbortedGlanceApply returned nil; an interrupted apply must exit non-zero")
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
	if len(c.deleted) != 2 {
		t.Errorf("deleted = %v, want both images torn down", c.deleted)
	}
}

func TestFinishAbortedGlanceApplyKeepOnAbortLeavesResources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name       string
		recordPath string
		wantHint   string
	}{
		{"with record path", "run-keep.json", "glance cleanup --run run-keep.json"},
		{"empty record path falls back to run-id", "", "glance cleanup --run-id keep1234"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingGlanceCleaner{}
			var out bytes.Buffer
			writeRecord := func() (string, error) { return tc.recordPath, nil }

			err := finishAbortedGlanceApply(ctx, &out, c, "keep1234", nil, true, errors.New("apply boom"), 1, time.Second, writeRecord)

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

func TestGlanceStatusRequiresRunFlag(t *testing.T) {
	if _, err := execRoot(t, "glance", "status"); err == nil {
		t.Fatal("status without --run: expected error, got nil")
	}
}

func TestGlanceCleanupRequiresRunOrRunID(t *testing.T) {
	if _, err := execRoot(t, "glance", "cleanup"); err == nil {
		t.Fatal("cleanup with neither --run nor --run-id: expected error, got nil")
	}
}

// TestGlanceRejectsNovaRecord confirms the service guard stops a glance command
// from operating on a nova run record, whose resource kinds the glance client
// cannot handle.
func TestGlanceRejectsNovaRecord(t *testing.T) {
	dir := t.TempDir()
	if _, err := run.Write(dir, &run.Record{RunID: "nov00001", Service: "nova"}); err != nil {
		t.Fatalf("seeding nova record: %v", err)
	}
	novaRec := filepath.Join(dir, "run-nov00001.json")

	if _, err := execRoot(t, "glance", "status", "--run", novaRec); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("glance status on a nova record: err = %v, want a service mismatch error", err)
	}
	if _, err := execRoot(t, "glance", "cleanup", "--run", novaRec); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("glance cleanup on a nova record: err = %v, want a service mismatch error", err)
	}
}
