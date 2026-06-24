package main

import (
	"strings"
	"testing"

	"github.com/B42Labs/openstack-tester/scenarios"
)

// chaosScenarioYAML is sampleScenarioYAML extended with a chaos block, used to
// exercise the chaos command's config merge.
const chaosScenarioYAML = sampleScenarioYAML + `
chaos:
  duration: 1m
  interval: { min: 5s, max: 10s }
  parallel: { max: 3 }
  churn_ratio: 0.5
  target_fill: 0.8
`

func TestChaosRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "neutron", "chaos"); err == nil {
		t.Fatal("chaos without --scenario: expected error, got nil")
	}
}

func TestChaosRequiresDuration(t *testing.T) {
	// A scenario with no chaos block and no --duration flag has no duration, so
	// the merged config is rejected before any cloud call.
	path := writeScenario(t, sampleScenarioYAML)
	_, err := execRoot(t, "neutron", "chaos", "--scenario", path)
	if err == nil {
		t.Fatal("chaos without a duration: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Errorf("error %q does not mention the missing duration", err.Error())
	}
}

func TestChaosDurationFlagOverridesBlock(t *testing.T) {
	// The scenario chaos block sets a valid 1m duration; --duration 0s overrides
	// it, producing an invalid merged duration — proving the flag wins over the
	// block.
	path := writeScenario(t, chaosScenarioYAML)
	_, err := execRoot(t, "neutron", "chaos", "--scenario", path, "--duration", "0s")
	if err == nil {
		t.Fatal("chaos with --duration 0s overriding the block: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Errorf("error %q does not mention the duration", err.Error())
	}
}

func TestChaosFlagOverrideProducesInvalidInterval(t *testing.T) {
	// The block sets interval min 5s / max 10s; --max-interval 1s overrides only
	// the max, leaving min (5s) > max (1s), which the merged config rejects. This
	// shows the flag overrides one field of the block while the block supplies
	// the other.
	path := writeScenario(t, chaosScenarioYAML)
	_, err := execRoot(t, "neutron", "chaos", "--scenario", path, "--max-interval", "1s")
	if err == nil {
		t.Fatal("chaos with min-interval > max-interval after override: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "interval") {
		t.Errorf("error %q does not mention the interval", err.Error())
	}
}

func TestChaosValidatesScenarioBeforeCloud(t *testing.T) {
	// An invalid scenario must fail during plan expansion, before any cloud call.
	path := writeScenario(t, "name: bad\nresources:\n  networks: -1\n")
	if _, err := execRoot(t, "neutron", "chaos", "--scenario", path, "--duration", "1m"); err == nil {
		t.Fatal("chaos with an invalid scenario: expected error, got nil")
	}
}

func TestChaosShippedProfilesRunWithoutDuration(t *testing.T) {
	// Each built-in profile ships a chaos block, so `neutron chaos --scenario
	// scenarios/<profile>.yaml` needs no --duration: the merged config validates
	// and the run proceeds to authenticate, failing only at client creation with
	// no reachable cloud. A missing or invalid chaos block would instead fail on
	// the duration before any cloud call.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	for _, name := range []string{"small", "medium", "large"} {
		t.Run(name, func(t *testing.T) {
			data, err := scenarios.Files.ReadFile(name + ".yaml")
			if err != nil {
				t.Fatalf("reading shipped profile %s.yaml: %v", name, err)
			}
			path := writeScenario(t, string(data))

			_, err = execRoot(t, "neutron", "chaos", "--scenario", path)
			if err == nil {
				t.Fatalf("chaos %s without --duration: expected a cloud-auth failure, got nil", name)
			}
			if !strings.Contains(err.Error(), "network client") {
				t.Errorf("chaos %s failed before reaching cloud auth: %q; the profile's chaos block should supply the duration", name, err.Error())
			}
		})
	}
}

func TestChaosWithValidConfigRequiresCloud(t *testing.T) {
	// A valid merged config (duration from the chaos block) passes validation and
	// proceeds to authenticate, failing at client creation with no reachable
	// cloud — never reaching a real API.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, chaosScenarioYAML)
	_, err := execRoot(t, "neutron", "chaos", "--scenario", path)
	if err == nil {
		t.Fatal("chaos with a reachable-cloud-free config: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "network client") {
		t.Errorf("error %q does not mention network client creation", err.Error())
	}
}
