package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
)

// sampleMixScenarioYAML is a small but complete mix scenario used by the mix
// command tests.
const sampleMixScenarioYAML = `
name: cli
seed: 5
image: cirros
flavor: m1.tiny
services: []
resources:
  servers: 4
personas:
  ci:
    share: 1
    networks: 2
    volumes_per_server: { min: 0, max: 1 }
    volume_gib: { min: 1, max: 2 }
`

// decodeMixPlan decodes the mix plan JSON generate wrote.
func decodeMixPlan(t *testing.T, data string) mixplan.Plan {
	t.Helper()
	var p mixplan.Plan
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		t.Fatalf("output is not valid mix plan JSON: %v", err)
	}
	return p
}

func TestMixGenerateStdout(t *testing.T) {
	path := writeScenario(t, sampleMixScenarioYAML)

	out, err := execRoot(t, "mix", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	p := decodeMixPlan(t, out)
	if len(p.Personas) != 1 || p.Personas[0].Name != "ci" || len(p.Personas[0].Nova.Servers) != 4 {
		t.Errorf("plan personas = %+v, want ci with 4 servers", p.Personas)
	}
}

func TestMixGenerateOut(t *testing.T) {
	path := writeScenario(t, sampleMixScenarioYAML)
	dest := filepath.Join(t.TempDir(), "plan.json")

	out, err := execRoot(t, "mix", "generate", "--scenario", path, "--out", dest)
	if err != nil {
		t.Fatalf("generate --out: %v", err)
	}
	if out != "" {
		t.Errorf("generate --out wrote %q to stdout, want nothing", out)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading the plan file: %v", err)
	}
	decodeMixPlan(t, string(data))
}

func TestMixGenerateOverrides(t *testing.T) {
	path := writeScenario(t, sampleMixScenarioYAML)

	base, err := execRoot(t, "mix", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	seeded, err := execRoot(t, "mix", "generate", "--scenario", path, "--seed", "7")
	if err != nil {
		t.Fatalf("generate --seed 7: %v", err)
	}
	if base == seeded {
		t.Error("--seed 7 did not change the generated plan")
	}

	out, err := execRoot(t, "mix", "generate", "--scenario", path, "--set", "resources.servers=3")
	if err != nil {
		t.Fatalf("generate --set resources.servers=3: %v", err)
	}
	if p := decodeMixPlan(t, out); len(p.Personas) != 1 || p.Personas[0].Servers != 3 || len(p.Personas[0].Nova.Servers) != 3 {
		t.Errorf("plan personas = %+v, want ci with 3 servers", p.Personas)
	}
}

func TestMixGenerateErrors(t *testing.T) {
	valid := writeScenario(t, sampleMixScenarioYAML)

	tests := []struct {
		name   string
		args   []string
		want   string
		prefix bool
	}{
		{"missing scenario flag", []string{"mix", "generate"}, `required flag(s) "scenario" not set`, false},
		{"missing file", []string{"mix", "generate", "--scenario", filepath.Join(t.TempDir(), "nope.yaml")}, "reading scenario:", true},
		{"set without value", []string{"mix", "generate", "--scenario", valid, "--set", "nokey"}, `invalid --set "nokey": want key=value`, false},
		{"unknown set key", []string{"mix", "generate", "--scenario", valid, "--set", "personas.ci.nope=1"}, `unknown override key "personas.ci.nope"`, false},
		{"unsupported service", []string{"mix", "generate", "--scenario", valid, "--set", "services=octavia"},
			`opt-in service "octavia" is not supported by this build of dizzy (supported: none)`, false},
		{"no server to divide", []string{"mix", "generate", "--scenario", valid, "--set", "resources.servers=0"},
			"generated plan failed validation: plan has no persona with at least one server", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execRoot(t, tc.args...)
			switch {
			case err == nil:
				t.Fatalf("expected error %q, got nil", tc.want)
			case tc.prefix && !strings.HasPrefix(err.Error(), tc.want):
				t.Errorf("error %q does not start with %q", err, tc.want)
			case !tc.prefix && err.Error() != tc.want:
				t.Errorf("error = %q, want %q", err, tc.want)
			}
		})
	}
}
