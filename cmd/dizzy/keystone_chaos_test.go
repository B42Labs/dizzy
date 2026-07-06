package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/keystone"
	keystonescenario "github.com/B42Labs/dizzy/internal/keystone/scenario"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/scenarios"
)

// keystoneChaosScenarioYAML is sampleKeystoneScenarioYAML extended with a chaos
// block (including the identity token_ratio), used to exercise the keystone
// chaos command's config merge.
const keystoneChaosScenarioYAML = sampleKeystoneScenarioYAML + `
chaos:
  duration: 1m
  interval: { min: 5s, max: 10s }
  parallel: { max: 3 }
  churn_ratio: 0.5
  target_fill: 0.8
  token_ratio: 0.3
`

func TestKeystoneChaosRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "keystone", "chaos"); err == nil {
		t.Fatal("keystone chaos without --scenario: expected error, got nil")
	}
}

func TestKeystoneChaosRequiresDuration(t *testing.T) {
	// A scenario with no chaos block and no --duration flag has no duration, so
	// the merged config is rejected before any cloud call.
	path := writeScenario(t, sampleKeystoneScenarioYAML)
	_, err := execRoot(t, "keystone", "chaos", "--scenario", path)
	if err == nil {
		t.Fatal("keystone chaos without a duration: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Errorf("error %q does not mention the missing duration", err.Error())
	}
}

func TestKeystoneChaosTokenRatioFlagOverridesBlock(t *testing.T) {
	// The block sets a valid token_ratio 0.3; --token-ratio 2 overrides it with an
	// out-of-range value the merged config rejects — proving the flag overrides
	// the block and reaches Validate before any cloud call.
	path := writeScenario(t, keystoneChaosScenarioYAML)
	_, err := execRoot(t, "keystone", "chaos", "--scenario", path, "--token-ratio", "2")
	if err == nil {
		t.Fatal("keystone chaos with --token-ratio 2: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "resize-ratio") {
		t.Errorf("error %q does not mention the ratio (fed through the engine's mutate-draw knob)", err.Error())
	}
}

// TestKeystoneChaosMergeTokenRatio proves the pointer-valued token_ratio merges
// correctly: an explicit 0 in the chaos block disables token issues — the block
// value is distinguishable from an omitted key — while an omitted key falls back
// to the default.
func TestKeystoneChaosMergeTokenRatio(t *testing.T) {
	tests := []struct {
		name  string
		block string
		want  float64
	}{
		{"explicit zero disables token issues", "  token_ratio: 0\n", 0},
		{"omitted falls back to default", "", defaultChaosTokenRatio},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := keystonescenario.Parse([]byte(sampleKeystoneScenarioYAML + "\nchaos:\n  duration: 1m\n" + tc.block))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			opts := &globalOptions{concurrency: 8}
			cmd := newKeystoneChaosCmd(opts)
			cfg := mergeKeystoneChaosConfig(cmd, opts, s, chaosFlags{}, defaultChaosTokenRatio)
			if cfg.ResizeRatio != tc.want {
				t.Errorf("merged token ratio = %v, want %v", cfg.ResizeRatio, tc.want)
			}
		})
	}
}

func TestKeystoneChaosShippedProfilesRunWithoutDuration(t *testing.T) {
	// Each built-in Keystone profile ships a chaos block, so `keystone chaos
	// --scenario scenarios/keystone/<profile>.yaml` needs no --duration: the
	// merged config validates and the run proceeds to authenticate, failing only
	// at client creation with no reachable cloud.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	for _, name := range []string{"small", "medium", "large"} {
		t.Run(name, func(t *testing.T) {
			data, err := scenarios.Files.ReadFile("keystone/" + name + ".yaml")
			if err != nil {
				t.Fatalf("reading shipped profile %s.yaml: %v", name, err)
			}
			path := writeScenario(t, string(data))

			_, err = execRoot(t, "keystone", "chaos", "--scenario", path)
			if err == nil {
				t.Fatalf("keystone chaos %s without --duration: expected a cloud-auth failure, got nil", name)
			}
			if !strings.Contains(err.Error(), "identity client") {
				t.Errorf("keystone chaos %s failed before reaching cloud auth: %q; the profile's chaos block should supply the duration", name, err.Error())
			}
		})
	}
}

func TestFinishKeystoneChurnTearsDownOnInterrupt(t *testing.T) {
	tests := []struct {
		name        string
		interrupted bool
	}{
		{"completed run tears down", false},
		{"interrupted run tears down too", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.interrupted {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel() // a first signal cancelled the run
			}
			c := &recordingKeystoneCleaner{}
			// Reverse dependency order: assignment, user, project, role, domain.
			created := []resource.Resource{
				{Kind: keystone.KindDomain, ID: "d1", Name: "ostester-run1234-dom-0001"},
				{Kind: keystone.KindRole, ID: "r1", Name: "ostester-run1234-role-0001"},
				{Kind: keystone.KindProject, ID: "p1", Name: "ostester-run1234-proj-0001"},
				{Kind: keystone.KindUser, ID: "u1", Name: "ostester-run1234-user-0001"},
				{Kind: keystone.KindAssignment, ID: "u1:project:p1:r1"},
			}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)

			err := finishKeystoneChurn(ctx, cmd, c, "run1234", "run-run1234.json", created, tc.interrupted, false, time.Second)
			if err != nil {
				t.Fatalf("finishKeystoneChurn: %v", err)
			}
			if c.sawCancelled {
				t.Error("teardown ran with a cancelled context; it must run on context.WithoutCancel")
			}
			// Reverse dependency order.
			wantOrder := []resource.Kind{keystone.KindAssignment, keystone.KindUser, keystone.KindProject, keystone.KindRole, keystone.KindDomain}
			if len(c.deleted) != len(wantOrder) {
				t.Fatalf("deleted %d resources, want %d: %+v", len(c.deleted), len(wantOrder), c.deleted)
			}
			for i, want := range wantOrder {
				if c.deleted[i].Kind != want {
					t.Errorf("deleted[%d] kind = %q, want %q", i, c.deleted[i].Kind, want)
				}
			}
			s := out.String()
			if !strings.Contains(s, "deleted 5 resource(s)") {
				t.Errorf("output %q missing the deletion count", s)
			}
			if !strings.Contains(s, "leak check: no run-named resources remain") {
				t.Errorf("output %q missing the leak-check line", s)
			}
		})
	}
}

func TestFinishKeystoneChurnNoCleanupSkipsTeardown(t *testing.T) {
	tests := []struct {
		name        string
		interrupted bool
		wantReason  string
	}{
		{"completed run", false, "churn complete"},
		{"interrupted run", true, "churn interrupted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingKeystoneCleaner{}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)

			err := finishKeystoneChurn(context.Background(), cmd, c, "run1234", "run-run1234.json", nil, tc.interrupted, true, time.Second)
			if err != nil {
				t.Fatalf("finishKeystoneChurn: %v", err)
			}
			if c.calls != 0 {
				t.Errorf("cleaner was called %d times; --no-cleanup must leave everything in place", c.calls)
			}
			s := out.String()
			if !strings.Contains(s, tc.wantReason) {
				t.Errorf("output %q missing reason %q", s, tc.wantReason)
			}
			if !strings.Contains(s, "keystone cleanup --run run-run1234.json") {
				t.Errorf("output %q missing the reclaim hint", s)
			}
		})
	}
}

func TestKeystoneChaosWithValidConfigRequiresCloud(t *testing.T) {
	// A valid merged config (duration from the chaos block) passes validation and
	// proceeds to authenticate, failing at client creation with no reachable
	// cloud — never reaching a real API.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, keystoneChaosScenarioYAML)
	_, err := execRoot(t, "keystone", "chaos", "--scenario", path)
	if err == nil {
		t.Fatal("keystone chaos with a reachable-cloud-free config: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "identity client") {
		t.Errorf("error %q does not mention identity client creation", err.Error())
	}
}
