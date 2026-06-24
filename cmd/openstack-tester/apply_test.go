package main

import (
	"strings"
	"testing"
)

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
