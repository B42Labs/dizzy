package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/neutron"
	"github.com/B42Labs/dizzy/internal/run"
)

func TestResolveRun(t *testing.T) {
	dir := t.TempDir()
	recorded := []neutron.Resource{{Kind: neutron.KindAddressScope, ID: "as1"}}
	if _, err := run.Write(dir, &run.Record{RunID: "fromrecord", Created: recorded}); err != nil {
		t.Fatalf("seeding run record: %v", err)
	}
	recordPath := filepath.Join(dir, "run-fromrecord.json")

	tests := []struct {
		name         string
		runPath      string
		runID        string
		want         string
		wantRecorded bool // expect the record's created list to be threaded through
		wantErr      bool
	}{
		{name: "neither", wantErr: true},
		{name: "both", runPath: recordPath, runID: "x", wantErr: true},
		{name: "id only", runID: "direct", want: "direct", wantRecorded: false},
		{name: "record only", runPath: recordPath, want: "fromrecord", wantRecorded: true},
		{name: "missing record", runPath: filepath.Join(dir, "nope.json"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, gotRec, err := resolveRun(tc.runPath, tc.runID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got id %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveRun id = %q, want %q", got, tc.want)
			}
			if gotHas := gotRec != nil && len(gotRec.Created) > 0; gotHas != tc.wantRecorded {
				t.Errorf("resolveRun recorded = %v, want present=%v", gotRec, tc.wantRecorded)
			}
		})
	}
}

func TestCleanupRequiresRunOrRunID(t *testing.T) {
	if _, err := execRoot(t, "neutron", "cleanup"); err == nil {
		t.Fatal("cleanup with neither --run nor --run-id: expected error, got nil")
	}
}

func TestCleanupRequiresCloud(t *testing.T) {
	// Point cloud configuration at nothing: with a run id resolved, cleanup must
	// fail at client creation, never reaching a real cloud.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	_, err := execRoot(t, "neutron", "cleanup", "--run-id", "abcd1234")
	if err == nil {
		t.Fatal("cleanup: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "network client") {
		t.Errorf("error %q does not mention network client creation", err.Error())
	}
}

// TestWarnUnreclaimable confirms cleanup warns that address scopes will be left
// behind when it has no run record or only a mid-run checkpoint, and stays
// quiet for a final record.
func TestWarnUnreclaimable(t *testing.T) {
	tests := []struct {
		name string
		rec  *run.Record
		want string // the warning's message, or empty for none
	}{
		{name: "no record", want: "cleaning up by id without a run record"},
		{name: "checkpoint record", rec: &run.Record{RunID: "r1", Incomplete: true, FinishedAt: time.Unix(60, 0)}, want: "run record is a mid-run checkpoint"},
		{name: "final record", rec: &run.Record{RunID: "r1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			warnUnreclaimable("r1", tc.rec)

			got := logs.String()
			if tc.want == "" {
				if got != "" {
					t.Errorf("logged %q, want nothing", got)
				}
				return
			}
			if strings.Count(got, "level=WARN") != 1 || !strings.Contains(got, tc.want) {
				t.Errorf("logged %q, want one warning containing %q", got, tc.want)
			}
		})
	}
}
