package main

import (
	"strings"
	"testing"

	cinderscenario "github.com/B42Labs/openstack-tester/internal/cinder/scenario"
	"github.com/B42Labs/openstack-tester/scenarios"
)

// cinderChaosScenarioYAML is sampleCinderScenarioYAML extended with a chaos
// block (including the block-storage resize_ratio), used to exercise the cinder
// chaos command's config merge.
const cinderChaosScenarioYAML = sampleCinderScenarioYAML + `
chaos:
  duration: 1m
  interval: { min: 5s, max: 10s }
  parallel: { max: 3 }
  churn_ratio: 0.5
  target_fill: 0.8
  resize_ratio: 0.3
`

func TestCinderChaosRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "cinder", "chaos"); err == nil {
		t.Fatal("cinder chaos without --scenario: expected error, got nil")
	}
}

func TestCinderChaosRequiresDuration(t *testing.T) {
	// A scenario with no chaos block and no --duration flag has no duration, so
	// the merged config is rejected before any cloud call.
	path := writeScenario(t, sampleCinderScenarioYAML)
	_, err := execRoot(t, "cinder", "chaos", "--scenario", path)
	if err == nil {
		t.Fatal("cinder chaos without a duration: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Errorf("error %q does not mention the missing duration", err.Error())
	}
}

func TestCinderChaosDurationFlagOverridesBlock(t *testing.T) {
	// The chaos block sets a valid 1m duration; --duration 0s overrides it,
	// producing an invalid merged duration — proving the flag wins over the block.
	path := writeScenario(t, cinderChaosScenarioYAML)
	_, err := execRoot(t, "cinder", "chaos", "--scenario", path, "--duration", "0s")
	if err == nil {
		t.Fatal("cinder chaos with --duration 0s overriding the block: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Errorf("error %q does not mention the duration", err.Error())
	}
}

func TestCinderChaosFlagOverrideProducesInvalidInterval(t *testing.T) {
	// The block sets interval min 5s / max 10s; --max-interval 1s overrides only
	// the max, leaving min (5s) > max (1s), which the merged config rejects.
	path := writeScenario(t, cinderChaosScenarioYAML)
	_, err := execRoot(t, "cinder", "chaos", "--scenario", path, "--max-interval", "1s")
	if err == nil {
		t.Fatal("cinder chaos with min-interval > max-interval after override: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "interval") {
		t.Errorf("error %q does not mention the interval", err.Error())
	}
}

func TestCinderChaosResizeRatioFlagOverridesBlock(t *testing.T) {
	// The block sets a valid resize_ratio 0.3; --resize-ratio 2 overrides it with
	// an out-of-range value the merged config rejects — proving the flag overrides
	// the block's resize ratio and reaches Validate before any cloud call.
	path := writeScenario(t, cinderChaosScenarioYAML)
	_, err := execRoot(t, "cinder", "chaos", "--scenario", path, "--resize-ratio", "2")
	if err == nil {
		t.Fatal("cinder chaos with --resize-ratio 2: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "resize-ratio") {
		t.Errorf("error %q does not mention the resize ratio", err.Error())
	}
}

// TestCinderChaosMergeResizeRatio proves the pointer-valued resize_ratio merges
// correctly: an explicit 0 in the chaos block disables extends — the block value
// is distinguishable from an omitted key — while an omitted key falls back to the
// default. Regression guard against a block-level resize_ratio: 0 being silently
// overridden by the default (which would extend volumes anyway).
func TestCinderChaosMergeResizeRatio(t *testing.T) {
	tests := []struct {
		name  string
		block string
		want  float64
	}{
		{"explicit zero disables extends", "  resize_ratio: 0\n", 0},
		{"omitted falls back to default", "", defaultChaosResizeRatio},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := cinderscenario.Parse([]byte(sampleCinderScenarioYAML + "\nchaos:\n  duration: 1m\n" + tc.block))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			opts := &globalOptions{concurrency: 8}
			cmd := newCinderChaosCmd(opts)
			cfg := mergeCinderChaosConfig(cmd, opts, s, chaosFlags{}, defaultChaosResizeRatio)
			if cfg.ResizeRatio != tc.want {
				t.Errorf("merged ResizeRatio = %v, want %v", cfg.ResizeRatio, tc.want)
			}
		})
	}
}

func TestCinderChaosValidatesScenarioBeforeCloud(t *testing.T) {
	// An invalid scenario must fail during plan expansion, before any cloud call.
	path := writeScenario(t, "name: bad\nresources:\n  volumes: -1\n")
	if _, err := execRoot(t, "cinder", "chaos", "--scenario", path, "--duration", "1m"); err == nil {
		t.Fatal("cinder chaos with an invalid scenario: expected error, got nil")
	}
}

func TestCinderChaosShippedProfilesRunWithoutDuration(t *testing.T) {
	// Each built-in Cinder profile ships a chaos block, so `cinder chaos
	// --scenario scenarios/cinder/<profile>.yaml` needs no --duration: the merged
	// config validates and the run proceeds to authenticate, failing only at
	// client creation with no reachable cloud. A missing or invalid chaos block
	// would instead fail on the duration before any cloud call.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	for _, name := range []string{"small", "medium", "large"} {
		t.Run(name, func(t *testing.T) {
			data, err := scenarios.Files.ReadFile("cinder/" + name + ".yaml")
			if err != nil {
				t.Fatalf("reading shipped profile %s.yaml: %v", name, err)
			}
			path := writeScenario(t, string(data))

			_, err = execRoot(t, "cinder", "chaos", "--scenario", path)
			if err == nil {
				t.Fatalf("cinder chaos %s without --duration: expected a cloud-auth failure, got nil", name)
			}
			if !strings.Contains(err.Error(), "block storage client") {
				t.Errorf("cinder chaos %s failed before reaching cloud auth: %q; the profile's chaos block should supply the duration", name, err.Error())
			}
		})
	}
}

func TestCinderChaosWithValidConfigRequiresCloud(t *testing.T) {
	// A valid merged config (duration from the chaos block) passes validation and
	// proceeds to authenticate, failing at client creation with no reachable
	// cloud — never reaching a real API.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, cinderChaosScenarioYAML)
	_, err := execRoot(t, "cinder", "chaos", "--scenario", path)
	if err == nil {
		t.Fatal("cinder chaos with a reachable-cloud-free config: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "block storage client") {
		t.Errorf("error %q does not mention block storage client creation", err.Error())
	}
}
