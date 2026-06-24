package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/B42Labs/openstack-tester/internal/run"
)

func TestStatusRequiresRunFlag(t *testing.T) {
	if _, err := execRoot(t, "neutron", "status"); err == nil {
		t.Fatal("status without --run: expected error, got nil")
	}
}

func TestStatusRequiresCloud(t *testing.T) {
	// Point cloud configuration at nothing: status must load the record and then
	// fail at client creation, never reaching a real cloud.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	dir := t.TempDir()
	if _, err := run.Write(dir, &run.Record{RunID: "abcd1234"}); err != nil {
		t.Fatalf("seeding run record: %v", err)
	}

	_, err := execRoot(t, "neutron", "status", "--run", filepath.Join(dir, "run-abcd1234.json"))
	if err == nil {
		t.Fatal("status: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "network client") {
		t.Errorf("error %q does not mention network client creation", err.Error())
	}
}

func TestStatusMissingRecordErrors(t *testing.T) {
	_, err := execRoot(t, "neutron", "status", "--run", filepath.Join(t.TempDir(), "run-nope.json"))
	if err == nil {
		t.Fatal("status with a missing record: expected error, got nil")
	}
}
