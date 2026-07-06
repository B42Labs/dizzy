package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/neutron"
)

// recordingCleaner is a fake executor.Cleaner that records what it deletes and
// whether the contexts it is handed were cancelled, so the aborted-apply and
// chaos teardown tests can assert teardown ran on a live context. Its ListByTag
// returns nothing, so the only deletes are the recorded resources (address
// scopes) passed through the created list. When recordWritten is non-nil it
// captures, on the first cleaner call, whether the run record had already been
// written — the record-before-teardown ordering guarantee.
type recordingCleaner struct {
	recordWritten      *bool
	firstCallSawRecord bool
	calls              int
	sawCancelled       bool
	deleted            []neutron.Resource
}

func (c *recordingCleaner) observe(ctx context.Context) {
	c.calls++
	if c.calls == 1 && c.recordWritten != nil {
		c.firstCallSawRecord = *c.recordWritten
	}
	if ctx.Err() != nil {
		c.sawCancelled = true
	}
}

func (c *recordingCleaner) ListByTag(ctx context.Context, _ neutron.Kind, _ string) ([]neutron.Resource, error) {
	c.observe(ctx)
	return nil, nil
}

func (c *recordingCleaner) DetachRouterInterfaces(ctx context.Context, _ string) (int, error) {
	c.observe(ctx)
	return 0, nil
}

func (c *recordingCleaner) DeleteNetworkPorts(ctx context.Context, _ string) (int, error) {
	c.observe(ctx)
	return 0, nil
}

func (c *recordingCleaner) Delete(ctx context.Context, r neutron.Resource) error {
	c.observe(ctx)
	c.deleted = append(c.deleted, r)
	return nil
}

func TestApplyDryRunSummaryNoAPICall(t *testing.T) {
	// Point cloud configuration at nothing: succeeding without a reachable
	// cloud proves the dry-run path makes zero auth/API calls.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleScenarioYAML)

	out, err := execRoot(t, "neutron", "apply", "--scenario", path, "--dry-run")
	if err != nil {
		t.Fatalf("apply --dry-run: %v", err)
	}
	if !strings.Contains(out, `scenario "cli"`) {
		t.Errorf("summary missing scenario name:\n%s", out)
	}
	if !strings.Contains(out, "networks:") {
		t.Errorf("summary missing network count:\n%s", out)
	}
}

func TestApplyWithoutDryRunRequiresCloud(t *testing.T) {
	// Point cloud configuration at nothing: the non-dry-run path must attempt
	// to authenticate and fail at client creation, never reaching a real cloud.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleScenarioYAML)

	_, err := execRoot(t, "neutron", "apply", "--scenario", path)
	if err == nil {
		t.Fatal("apply without --dry-run: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "network client") {
		t.Errorf("error %q does not mention network client creation", err.Error())
	}
}

func TestApplyDryRunValidatesScenario(t *testing.T) {
	path := writeScenario(t, "name: bad\nresources:\n  networks: -1\n")

	if _, err := execRoot(t, "neutron", "apply", "--scenario", path, "--dry-run"); err == nil {
		t.Fatal("apply --dry-run with invalid scenario: expected error, got nil")
	}
}

func TestApplyKeepOnAbortFlagParses(t *testing.T) {
	// --dry-run returns before signal setup, so a successful parse proves the
	// flag is registered without needing a cloud.
	path := writeScenario(t, sampleScenarioYAML)
	if _, err := execRoot(t, "neutron", "apply", "--scenario", path, "--dry-run", "--keep-on-abort"); err != nil {
		t.Fatalf("apply --dry-run --keep-on-abort: %v", err)
	}
}

func TestRunAborted(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name     string
		ctx      context.Context
		applyErr error
		want     bool
	}{
		{"clean run, live ctx", live, nil, false},
		{"failed run, no signal keeps resources for inspection", live, errors.New("boom"), false},
		{"failed run, signalled tears down", cancelled, errors.New("boom"), true},
		{"clean run, signal after success keeps resources", cancelled, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := runAborted(tc.ctx, tc.applyErr); got != tc.want {
				t.Errorf("runAborted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFinishAbortedApplyTearsDownOnLiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a first signal cancelled the run

	recordWritten := false
	c := &recordingCleaner{recordWritten: &recordWritten}
	// An address scope carries no tag, so it is reclaimed only via the created
	// list — the id-based path this exercises.
	created := []neutron.Resource{{Kind: neutron.KindAddressScope, ID: "as1"}}
	var out bytes.Buffer
	writeRecord := func() (string, error) {
		recordWritten = true
		return "run-abcd1234.json", nil
	}

	err := finishAbortedApply(ctx, &out, c, "abcd1234", created, false, errors.New("apply boom"), time.Second, writeRecord)

	if err == nil {
		t.Fatal("finishAbortedApply returned nil; an interrupted apply must exit non-zero")
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
	if len(c.deleted) != 1 || c.deleted[0].ID != "as1" {
		t.Errorf("deleted = %v, want the address scope reclaimed by id from the created list", c.deleted)
	}
	if !strings.Contains(out.String(), "deleted 1 resource(s)") {
		t.Errorf("output %q missing the deletion count", out.String())
	}
}

func TestFinishAbortedApplyBoundsWedgedTeardown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the signal context is already cancelled at teardown time

	writeRecord := func() (string, error) { return "run-wedged.json", nil }

	done := make(chan error, 1)
	go func() {
		done <- finishAbortedApply(ctx, io.Discard, blockingCleaner{}, "wedged", nil, false, errors.New("apply boom"), 10*time.Millisecond, writeRecord)
	}()

	select {
	case err := <-done:
		// DeadlineExceeded (not Canceled) proves both the context.WithoutCancel —
		// else the already-cancelled parent would surface as Canceled instantly —
		// and the per-op timeout bound.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("finishAbortedApply did not return; teardown was not bounded")
	}
}

func TestFinishAbortedApplyKeepOnAbortLeavesResources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name       string
		recordPath string
		wantHint   string
	}{
		{"with record path", "run-keep.json", "neutron cleanup --run run-keep.json"},
		{"empty record path falls back to run-id", "", "neutron cleanup --run-id keep1234"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingCleaner{}
			var out bytes.Buffer
			writeRecord := func() (string, error) { return tc.recordPath, nil }

			err := finishAbortedApply(ctx, &out, c, "keep1234", nil, true, errors.New("apply boom"), time.Second, writeRecord)

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

func TestFinishAbortedApplyRecordWriteFailureStillTearsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &recordingCleaner{}
	created := []neutron.Resource{{Kind: neutron.KindAddressScope, ID: "as1"}}
	var out bytes.Buffer
	writeRecord := func() (string, error) { return "", errors.New("disk full") }

	err := finishAbortedApply(ctx, &out, c, "rec1234", created, false, errors.New("apply boom"), time.Second, writeRecord)

	if err == nil {
		t.Fatal("finishAbortedApply returned nil")
	}
	if len(c.deleted) != 1 {
		t.Errorf("teardown deleted %d resources; it must still run when the record write fails", len(c.deleted))
	}
}
