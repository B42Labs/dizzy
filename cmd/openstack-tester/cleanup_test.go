package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/B42Labs/openstack-tester/internal/run"
)

func TestResolveRunID(t *testing.T) {
	dir := t.TempDir()
	if _, err := run.Write(dir, &run.Record{RunID: "fromrecord"}); err != nil {
		t.Fatalf("seeding run record: %v", err)
	}
	recordPath := filepath.Join(dir, "run-fromrecord.json")

	tests := []struct {
		name    string
		runPath string
		runID   string
		want    string
		wantErr bool
	}{
		{name: "neither", wantErr: true},
		{name: "both", runPath: recordPath, runID: "x", wantErr: true},
		{name: "id only", runID: "direct", want: "direct"},
		{name: "record only", runPath: recordPath, want: "fromrecord"},
		{name: "missing record", runPath: filepath.Join(dir, "nope.json"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveRunID(tc.runPath, tc.runID)
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
				t.Errorf("resolveRunID = %q, want %q", got, tc.want)
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
