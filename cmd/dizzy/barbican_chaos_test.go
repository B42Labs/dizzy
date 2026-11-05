package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/barbican"
	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	barbicanscenario "github.com/B42Labs/dizzy/internal/barbican/scenario"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// barbicanChaosScenarioYAML is sampleBarbicanScenarioYAML extended with a chaos
// block (including the key manager retrieve_ratio), used to exercise the
// barbican chaos command's config merge.
const barbicanChaosScenarioYAML = sampleBarbicanScenarioYAML + `
chaos:
  duration: 1m
  interval: { min: 5s, max: 10s }
  parallel: { max: 3 }
  churn_ratio: 0.5
  target_fill: 0.8
  retrieve_ratio: 0.3
`

func TestBarbicanChaosRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "barbican", "chaos"); err == nil {
		t.Fatal("barbican chaos without --scenario: expected error, got nil")
	}
}

func TestBarbicanChaosRequiresDuration(t *testing.T) {
	// A scenario with no chaos block and no --duration flag has no duration, so
	// the merged config is rejected before any cloud call.
	path := writeScenario(t, sampleBarbicanScenarioYAML)
	_, err := execRoot(t, "barbican", "chaos", "--scenario", path)
	if err == nil {
		t.Fatal("barbican chaos without a duration: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Errorf("error %q does not mention the missing duration", err.Error())
	}
}

func TestBarbicanChaosRetrieveRatioFlagOverridesBlock(t *testing.T) {
	// The block sets a valid retrieve_ratio 0.3; --retrieve-ratio 2 overrides it
	// with an out-of-range value the merged config rejects — proving the flag
	// overrides the block and reaches Validate before any cloud call.
	path := writeScenario(t, barbicanChaosScenarioYAML)
	_, err := execRoot(t, "barbican", "chaos", "--scenario", path, "--retrieve-ratio", "2")
	if err == nil {
		t.Fatal("barbican chaos with --retrieve-ratio 2: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "resize-ratio") {
		// The retrieve ratio maps onto the engine's ResizeRatio knob, whose
		// validation names it "resize-ratio".
		t.Errorf("error %q does not mention the rejected ratio", err.Error())
	}
}

// TestBarbicanChaosMergeRetrieveRatio proves the pointer-valued retrieve_ratio
// merges correctly: an explicit 0 in the chaos block disables read-backs — the
// block value is distinguishable from an omitted key — while an omitted key
// falls back to the default, and --retrieve-ratio overrides either.
func TestBarbicanChaosMergeRetrieveRatio(t *testing.T) {
	tests := []struct {
		name  string
		block string
		flag  string
		want  float64
	}{
		{"explicit zero disables read-backs", "  retrieve_ratio: 0\n", "", 0},
		{"omitted falls back to default", "", "", defaultChaosRetrieveRatio},
		{"flag overrides an explicit block value", "  retrieve_ratio: 0\n", "0.7", 0.7},
		{"flag overrides an omitted key", "", "0.7", 0.7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := barbicanscenario.Parse([]byte(sampleBarbicanScenarioYAML + "\nchaos:\n  duration: 1m\n" + tc.block))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			opts := &globalOptions{concurrency: 8}
			cmd := newBarbicanChaosCmd(opts)
			if tc.flag != "" {
				if err := cmd.Flags().Set("retrieve-ratio", tc.flag); err != nil {
					t.Fatalf("setting --retrieve-ratio %s: %v", tc.flag, err)
				}
			}
			retrieveRatio, err := cmd.Flags().GetFloat64("retrieve-ratio")
			if err != nil {
				t.Fatalf("reading --retrieve-ratio: %v", err)
			}
			cfg := mergeBarbicanChaosConfig(cmd, opts, s, chaosFlags{}, retrieveRatio)
			if cfg.ResizeRatio != tc.want {
				t.Errorf("merged ResizeRatio = %v, want %v", cfg.ResizeRatio, tc.want)
			}
		})
	}
}

// TestBarbicanChaosMergeDurationAndBucketWidth proves --duration 0 given as a
// flag selects the unbounded mode while the block's duration never does, and
// that the bucket width falls back to 1h unless the block or the flag sets it.
func TestBarbicanChaosMergeDurationAndBucketWidth(t *testing.T) {
	for _, tc := range chaosModeCases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := barbicanscenario.Parse([]byte(sampleBarbicanScenarioYAML + "\n" + tc.block))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			opts := &globalOptions{concurrency: 8}
			cmd := newBarbicanChaosCmd(opts)
			cfg := mergeBarbicanChaosConfig(cmd, opts, s, setChaosFlags(t, cmd, tc.flags), defaultChaosRetrieveRatio)
			checkChaosMode(t, cfg, tc.wantUnbounded, tc.wantDuration, tc.wantWidth)
		})
	}
}

func TestBarbicanChaosShippedProfilesRunWithoutDuration(t *testing.T) {
	// Each built-in Barbican profile ships a chaos block, so `barbican chaos
	// --scenario scenarios/barbican/<profile>.yaml` needs no --duration: the
	// merged config validates and the run proceeds to authenticate, failing only
	// at client creation with no reachable cloud.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	for _, name := range []string{"small", "medium", "large"} {
		t.Run(name, func(t *testing.T) {
			path := shippedBarbicanProfile(t, name)

			_, err := execRoot(t, "barbican", "chaos", "--scenario", path)
			if err == nil {
				t.Fatalf("barbican chaos %s without --duration: expected a cloud-auth failure, got nil", name)
			}
			if !strings.Contains(err.Error(), "key manager client") {
				t.Errorf("barbican chaos %s failed before reaching cloud auth: %q; the profile's chaos block should supply the duration", name, err.Error())
			}
		})
	}
}

func TestBarbicanChaosDurationZeroReachesCloud(t *testing.T) {
	// --duration 0 over each shipped profile's block duration selects an
	// unbounded run: the merged config validates and the run proceeds to
	// authenticate, failing only at client creation with no reachable cloud.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	for _, name := range []string{"small", "medium", "large"} {
		t.Run(name, func(t *testing.T) {
			path := shippedBarbicanProfile(t, name)

			_, err := execRoot(t, "barbican", "chaos", "--scenario", path, "--duration", "0")
			if err == nil {
				t.Fatalf("barbican chaos %s --duration 0 without a cloud: expected a client-creation failure, got nil", name)
			}
			if !strings.Contains(err.Error(), "key manager client") {
				t.Errorf("barbican chaos %s --duration 0 failed before reaching cloud auth: %q", name, err.Error())
			}
		})
	}
}

// TestBarbicanChurnEnvelope confirms the quota pre-check's envelope drops the
// orders the churn never creates, keeps the secrets and containers it does, and
// leaves the plan the graph is built from untouched.
func TestBarbicanChurnEnvelope(t *testing.T) {
	p := &barbicanplan.Plan{
		Scenario: "env",
		Seed:     7,
		Secrets: []barbicanplan.Secret{
			{Name: "sec-0001", PayloadBytes: 32, Retrieve: true},
			{Name: "sec-0002", PayloadBytes: 64},
		},
		Orders:     []barbicanplan.Order{{Name: "ord-0001"}, {Name: "ord-0002"}},
		Containers: []barbicanplan.Container{{Name: "ctr-0001", Secrets: []string{"sec-0001", "sec-0002"}}},
	}
	wantOrders := append([]barbicanplan.Order(nil), p.Orders...)

	env := barbicanChurnEnvelope(p)

	if env.Orders != nil {
		t.Errorf("envelope Orders = %v, want nil", env.Orders)
	}
	if !reflect.DeepEqual(env.Secrets, p.Secrets) {
		t.Errorf("envelope Secrets = %v, want %v", env.Secrets, p.Secrets)
	}
	if !reflect.DeepEqual(env.Containers, p.Containers) {
		t.Errorf("envelope Containers = %v, want %v", env.Containers, p.Containers)
	}
	if !reflect.DeepEqual(p.Orders, wantOrders) {
		t.Errorf("input plan Orders = %v after building the envelope, want %v unchanged", p.Orders, wantOrders)
	}
}

func TestBarbicanMismatchError(t *testing.T) {
	tests := []struct {
		name    string
		errs    []metrics.ErrorCount
		wantErr string // "" for a nil error
	}{
		{"nil errors", nil, ""},
		{"no mismatch entry", []metrics.ErrorCount{{Kind: "http_503", Count: 3}, {Kind: "quota", Count: 1}}, ""},
		{
			"mismatch entry counted",
			[]metrics.ErrorCount{{Kind: barbican.ErrKindMismatch, Count: 2}, {Kind: "http_503", Count: 1}},
			"payload read-back mismatch (run run1234): 2 read-back(s) returned different bytes",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := barbicanMismatchError("run1234", metrics.Aggregate{Errors: tc.errs})
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("barbicanMismatchError = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("barbicanMismatchError = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// churnBarbicanCreated is the Created list of a churn run that ended with one
// stored secret and one container referencing it still live.
func churnBarbicanCreated() []resource.Resource {
	return []resource.Resource{
		{Kind: barbican.KindSecret, Logical: "sec-0001", Name: "dizzy-run1234-sec-0001", ID: "s1"},
		{Kind: barbican.KindContainer, Logical: "ctr-0001", Name: "dizzy-run1234-ctr-0001", ID: "c1"},
	}
}

func TestFinishBarbicanChurnTearsDownOnInterrupt(t *testing.T) {
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
			c := &recordingBarbicanCleaner{}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)

			err := finishBarbicanChurn(ctx, cmd, c, "run1234", "run-run1234.json", churnBarbicanCreated(), tc.interrupted, false, nil, 1, time.Second)
			if err != nil {
				t.Fatalf("finishBarbicanChurn: %v", err)
			}
			if c.sawCancelled {
				t.Error("teardown ran with a cancelled context; it must run on context.WithoutCancel")
			}
			// The container goes before the secret it references.
			var gotIDs []string
			for _, r := range c.deleted {
				gotIDs = append(gotIDs, r.ID)
			}
			if want := []string{"c1", "s1"}; !reflect.DeepEqual(gotIDs, want) {
				t.Errorf("deleted ids = %v, want %v", gotIDs, want)
			}
			want := "deleted 2 resource(s) for run run1234\n" +
				"leak check: no run-named resources remain\n"
			if out.String() != want {
				t.Errorf("output = %q, want %q", out.String(), want)
			}
		})
	}
}

// TestFinishBarbicanChurnReportsLeak confirms a container the run-identity
// listing still finds after teardown is reported by the leak check, which
// leaves the exit status alone.
func TestFinishBarbicanChurnReportsLeak(t *testing.T) {
	c := &recordingBarbicanCleaner{
		leftover: []resource.Resource{{Kind: barbican.KindContainer, Logical: "ctr-0002", Name: "dizzy-run1234-ctr-0002", ID: "c2"}},
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := finishBarbicanChurn(context.Background(), cmd, c, "run1234", "run-run1234.json", churnBarbicanCreated(), false, false, nil, 1, time.Second)
	if err != nil {
		t.Fatalf("finishBarbicanChurn: %v", err)
	}
	if want := "leak check: 1 run-named resource(s) still present after teardown\n"; !strings.Contains(out.String(), want) {
		t.Errorf("output %q missing the leak line %q", out.String(), want)
	}
}

// TestFinishBarbicanChurnTeardownError confirms a listing failure during
// teardown is returned naming the run, wrapping the listing error, and that the
// leak check never runs after it.
func TestFinishBarbicanChurnTeardownError(t *testing.T) {
	listErr := errors.New("container listing boom")
	c := &recordingBarbicanCleaner{listErr: listErr}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := finishBarbicanChurn(context.Background(), cmd, c, "run1234", "run-run1234.json", churnBarbicanCreated(), false, false, nil, 1, time.Second)
	if err == nil {
		t.Fatal("finishBarbicanChurn returned nil for a failed teardown")
	}
	if !strings.HasPrefix(err.Error(), "tearing down run run1234:") {
		t.Errorf("error %q does not start with %q", err.Error(), "tearing down run run1234:")
	}
	if !errors.Is(err, listErr) {
		t.Errorf("error %q does not wrap the listing error", err.Error())
	}
	if strings.Contains(out.String(), "leak check") {
		t.Errorf("output %q carries a leak-check line after a failed teardown", out.String())
	}
}

func TestFinishBarbicanChurnNoCleanupSkipsTeardown(t *testing.T) {
	tests := []struct {
		name        string
		interrupted bool
		recordPath  string
		wantReason  string
		wantHint    string
	}{
		{"completed run", false, "run-run1234.json", "churn complete", "barbican cleanup --run run-run1234.json"},
		{"interrupted run", true, "run-run1234.json", "churn interrupted", "barbican cleanup --run run-run1234.json"},
		{"empty record path falls back to run-id", false, "", "churn complete", "barbican cleanup --run-id run1234"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingBarbicanCleaner{}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)

			err := finishBarbicanChurn(context.Background(), cmd, c, "run1234", tc.recordPath, churnBarbicanCreated(), tc.interrupted, true, nil, 1, time.Second)
			if err != nil {
				t.Fatalf("finishBarbicanChurn: %v", err)
			}
			if c.calls != 0 {
				t.Errorf("cleaner was called %d times; --no-cleanup must leave everything in place", c.calls)
			}
			s := out.String()
			if !strings.Contains(s, tc.wantReason) {
				t.Errorf("output %q missing reason %q", s, tc.wantReason)
			}
			if !strings.Contains(s, tc.wantHint) {
				t.Errorf("output %q missing the reclaim hint %q", s, tc.wantHint)
			}
		})
	}
}

// TestFinishBarbicanChurnReturnsMismatch confirms a read-back mismatch never
// skips the teardown policy: the run still tears down (or, with --no-cleanup,
// prints the hint) and then returns the mismatch, and a teardown failure is
// reported together with it.
func TestFinishBarbicanChurnReturnsMismatch(t *testing.T) {
	mismatchErr := barbicanMismatchError("run1234", metrics.Aggregate{Errors: []metrics.ErrorCount{{Kind: barbican.ErrKindMismatch, Count: 1}}})

	t.Run("teardown then mismatch", func(t *testing.T) {
		c := &recordingBarbicanCleaner{}
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)

		err := finishBarbicanChurn(context.Background(), cmd, c, "run1234", "run-run1234.json", churnBarbicanCreated(), false, false, mismatchErr, 1, time.Second)
		if err == nil || !strings.Contains(err.Error(), "payload read-back mismatch") {
			t.Fatalf("finishBarbicanChurn = %v, want the read-back mismatch error", err)
		}
		if len(c.deleted) != 2 {
			t.Errorf("deleted = %v, want both resources torn down before the mismatch is returned", c.deleted)
		}
		if !strings.Contains(out.String(), "leak check: no run-named resources remain") {
			t.Errorf("output %q missing the leak-check line", out.String())
		}
	})

	t.Run("no-cleanup prints the hint then mismatch", func(t *testing.T) {
		c := &recordingBarbicanCleaner{}
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)

		err := finishBarbicanChurn(context.Background(), cmd, c, "run1234", "run-run1234.json", churnBarbicanCreated(), false, true, mismatchErr, 1, time.Second)
		if err == nil || !strings.Contains(err.Error(), "payload read-back mismatch") {
			t.Fatalf("finishBarbicanChurn = %v, want the read-back mismatch error", err)
		}
		if c.calls != 0 {
			t.Errorf("cleaner was called %d times; --no-cleanup must leave everything in place", c.calls)
		}
		if !strings.Contains(out.String(), "barbican cleanup --run run-run1234.json") {
			t.Errorf("output %q missing the reclaim hint", out.String())
		}
	})

	t.Run("teardown failure and mismatch both reported", func(t *testing.T) {
		c := &recordingBarbicanCleaner{listErr: errors.New("container listing boom")}
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)

		err := finishBarbicanChurn(context.Background(), cmd, c, "run1234", "run-run1234.json", churnBarbicanCreated(), false, false, mismatchErr, 1, time.Second)
		if err == nil {
			t.Fatal("finishBarbicanChurn returned nil for a failed teardown with a mismatch")
		}
		for _, want := range []string{"tearing down run run1234:", "payload read-back mismatch"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err.Error(), want)
			}
		}
		if !errors.Is(err, mismatchErr) {
			t.Errorf("error %q does not wrap the mismatch error", err.Error())
		}
	})
}
