package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/B42Labs/openstack-tester/internal/plan"
)

// sampleScenarioYAML is a small but complete scenario used by the generate and
// apply command tests.
const sampleScenarioYAML = `
name: cli
seed: 5
resources:
  subnet_pools: 1
  address_scopes: 0
  networks: 3
  routers: 1
  security_groups: 1
distribution:
  subnets_per_network: { min: 1, max: 2 }
  ports_per_network: { min: 0, max: 2 }
  rules_per_security_group: { min: 1, max: 2 }
  subnet_from_pool_ratio: 0.5
  ipv6_ratio: 0.2
  subnets_attached_to_router_ratio: 0.5
topology:
  router_attach_strategy: random
  port_security_group_count: { min: 1, max: 1 }
`

// writeScenario writes body to a temp file and returns its path.
func writeScenario(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing scenario: %v", err)
	}
	return path
}

// execRoot runs the root command with args, capturing stdout and returning it
// together with the command error.
func execRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestGenerateCommandStdout(t *testing.T) {
	path := writeScenario(t, sampleScenarioYAML)

	out, err := execRoot(t, "neutron", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	var p plan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if len(p.Networks) != 3 {
		t.Errorf("networks = %d, want 3", len(p.Networks))
	}
}

func TestGenerateCommandWritesPlanFile(t *testing.T) {
	path := writeScenario(t, sampleScenarioYAML)
	outPath := filepath.Join(t.TempDir(), "plan.json")

	if _, err := execRoot(t, "neutron", "generate", "--scenario", path, "--out", outPath); err != nil {
		t.Fatalf("generate: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading plan file: %v", err)
	}
	var p plan.Plan
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("plan file is not valid JSON: %v", err)
	}
	if len(p.Networks) != 3 {
		t.Errorf("networks = %d, want 3", len(p.Networks))
	}
}

func TestGenerateCommandSetOverride(t *testing.T) {
	path := writeScenario(t, sampleScenarioYAML)

	out, err := execRoot(t, "neutron", "generate", "--scenario", path, "--set", "resources.networks=2")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	var p plan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if len(p.Networks) != 2 {
		t.Errorf("networks = %d, want 2 after override", len(p.Networks))
	}
}

func TestGenerateCommandSeedOverride(t *testing.T) {
	path := writeScenario(t, sampleScenarioYAML)

	base, err := execRoot(t, "neutron", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate (file seed): %v", err)
	}
	overridden, err := execRoot(t, "neutron", "generate", "--scenario", path, "--seed", "999")
	if err != nil {
		t.Fatalf("generate (--seed): %v", err)
	}

	if base == overridden {
		t.Error("global --seed did not change the generated plan")
	}
}

func TestGenerateCommandErrors(t *testing.T) {
	valid := writeScenario(t, sampleScenarioYAML)
	negative := writeScenario(t, "name: bad\nresources:\n  networks: -1\n")

	tests := []struct {
		name string
		args []string
	}{
		{"missing scenario flag", []string{"neutron", "generate"}},
		{"nonexistent file", []string{"neutron", "generate", "--scenario", filepath.Join(t.TempDir(), "nope.yaml")}},
		{"invalid scenario", []string{"neutron", "generate", "--scenario", negative}},
		{"bad set format", []string{"neutron", "generate", "--scenario", valid, "--set", "resources.networks"}},
		{"unknown set key", []string{"neutron", "generate", "--scenario", valid, "--set", "resources.nope=1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := execRoot(t, tc.args...); err == nil {
				t.Errorf("expected error for args %v, got nil", tc.args)
			}
		})
	}
}
