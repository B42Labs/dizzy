package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	cinderplan "github.com/B42Labs/openstack-tester/internal/cinder/plan"
	"github.com/B42Labs/openstack-tester/internal/run"
)

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

	want := []string{"generate", "apply", "status", "report", "cleanup"}
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
