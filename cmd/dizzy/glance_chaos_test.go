package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/glance"
	glancescenario "github.com/B42Labs/dizzy/internal/glance/scenario"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/scenarios"
)

// glanceChaosScenarioYAML is sampleGlanceScenarioYAML extended with a chaos block
// (including the image lifecycle_ratio), used to exercise the glance chaos
// command's config merge.
const glanceChaosScenarioYAML = sampleGlanceScenarioYAML + `
chaos:
  duration: 1m
  interval: { min: 5s, max: 10s }
  parallel: { max: 3 }
  churn_ratio: 0.5
  target_fill: 0.8
  lifecycle_ratio: 0.3
`

func TestGlanceChaosRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "glance", "chaos"); err == nil {
		t.Fatal("glance chaos without --scenario: expected error, got nil")
	}
}

func TestGlanceChaosRequiresDuration(t *testing.T) {
	// A scenario with no chaos block and no --duration flag has no duration, so
	// the merged config is rejected before any cloud call.
	path := writeScenario(t, sampleGlanceScenarioYAML)
	_, err := execRoot(t, "glance", "chaos", "--scenario", path)
	if err == nil {
		t.Fatal("glance chaos without a duration: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Errorf("error %q does not mention the missing duration", err.Error())
	}
}

func TestGlanceChaosLifecycleRatioFlagOverridesBlock(t *testing.T) {
	// The block sets a valid lifecycle_ratio 0.3; --lifecycle-ratio 2 overrides it
	// with an out-of-range value the merged config rejects — proving the flag
	// overrides the block and reaches Validate before any cloud call.
	path := writeScenario(t, glanceChaosScenarioYAML)
	_, err := execRoot(t, "glance", "chaos", "--scenario", path, "--lifecycle-ratio", "2")
	if err == nil {
		t.Fatal("glance chaos with --lifecycle-ratio 2: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "resize-ratio") {
		// The lifecycle ratio maps onto the engine's ResizeRatio knob, whose
		// validation names it "resize-ratio".
		t.Errorf("error %q does not mention the rejected ratio", err.Error())
	}
}

// TestGlanceChaosMergeLifecycleRatio proves the pointer-valued lifecycle_ratio
// merges correctly: an explicit 0 in the chaos block disables mutations — the
// block value is distinguishable from an omitted key — while an omitted key falls
// back to the default.
func TestGlanceChaosMergeLifecycleRatio(t *testing.T) {
	tests := []struct {
		name  string
		block string
		want  float64
	}{
		{"explicit zero disables mutations", "  lifecycle_ratio: 0\n", 0},
		{"omitted falls back to default", "", defaultChaosLifecycleRatio},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := glancescenario.Parse([]byte(sampleGlanceScenarioYAML + "\nchaos:\n  duration: 1m\n" + tc.block))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			opts := &globalOptions{concurrency: 8}
			cmd := newGlanceChaosCmd(opts)
			cfg := mergeGlanceChaosConfig(cmd, opts, s, chaosFlags{}, defaultChaosLifecycleRatio)
			if cfg.ResizeRatio != tc.want {
				t.Errorf("merged ResizeRatio = %v, want %v", cfg.ResizeRatio, tc.want)
			}
		})
	}
}

func TestGlanceChaosShippedProfilesRunWithoutDuration(t *testing.T) {
	// Each built-in Glance profile ships a chaos block, so `glance chaos --scenario
	// scenarios/glance/<profile>.yaml` needs no --duration: the merged config
	// validates and the run proceeds to authenticate, failing only at client
	// creation with no reachable cloud.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	for _, name := range []string{"small", "medium", "large"} {
		t.Run(name, func(t *testing.T) {
			data, err := scenarios.Files.ReadFile("glance/" + name + ".yaml")
			if err != nil {
				t.Fatalf("reading shipped profile %s.yaml: %v", name, err)
			}
			path := writeScenario(t, string(data))

			_, err = execRoot(t, "glance", "chaos", "--scenario", path)
			if err == nil {
				t.Fatalf("glance chaos %s without --duration: expected a cloud-auth failure, got nil", name)
			}
			if !strings.Contains(err.Error(), "image client") {
				t.Errorf("glance chaos %s failed before reaching cloud auth: %q; the profile's chaos block should supply the duration", name, err.Error())
			}
		})
	}
}

func TestFinishGlanceChurnTearsDownOnInterrupt(t *testing.T) {
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
			c := &recordingGlanceCleaner{}
			created := []resource.Resource{
				{Kind: glance.KindImage, ID: "i1"},
				{Kind: glance.KindImage, ID: "i2"},
			}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)

			err := finishGlanceChurn(ctx, cmd, c, "run1234", "run-run1234.json", created, tc.interrupted, false, 1, time.Second)
			if err != nil {
				t.Fatalf("finishGlanceChurn: %v", err)
			}
			if c.sawCancelled {
				t.Error("teardown ran with a cancelled context; it must run on context.WithoutCancel")
			}
			if len(c.deleted) != 2 {
				t.Errorf("deleted = %v, want both images torn down", c.deleted)
			}
			s := out.String()
			if !strings.Contains(s, "deleted 2 resource(s)") {
				t.Errorf("output %q missing the deletion count", s)
			}
			if !strings.Contains(s, "leak check: no run-tagged images remain") {
				t.Errorf("output %q missing the leak-check line", s)
			}
		})
	}
}

func TestFinishGlanceChurnNoCleanupSkipsTeardown(t *testing.T) {
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
			c := &recordingGlanceCleaner{}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)

			err := finishGlanceChurn(context.Background(), cmd, c, "run1234", "run-run1234.json", nil, tc.interrupted, true, 1, time.Second)
			if err != nil {
				t.Fatalf("finishGlanceChurn: %v", err)
			}
			if c.calls != 0 {
				t.Errorf("cleaner was called %d times; --no-cleanup must leave everything in place", c.calls)
			}
			s := out.String()
			if !strings.Contains(s, tc.wantReason) {
				t.Errorf("output %q missing reason %q", s, tc.wantReason)
			}
			if !strings.Contains(s, "glance cleanup --run run-run1234.json") {
				t.Errorf("output %q missing the reclaim hint", s)
			}
		})
	}
}
