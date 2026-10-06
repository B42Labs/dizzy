package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/neutron"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/scenario"
	"github.com/B42Labs/dizzy/scenarios"
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
	// The scenario chaos block sets a valid 1m duration; --duration -1s overrides
	// it, producing an invalid merged duration — proving the flag wins over the
	// block. (--duration 0 would select an unbounded run instead.)
	path := writeScenario(t, chaosScenarioYAML)
	_, err := execRoot(t, "neutron", "chaos", "--scenario", path, "--duration", "-1s")
	if err == nil {
		t.Fatal("chaos with --duration -1s overriding the block: expected error, got nil")
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
	// scenarios/neutron/<profile>.yaml` needs no --duration: the merged config
	// validates and the run proceeds to authenticate, failing only at client
	// creation with no reachable cloud. A missing or invalid chaos block would
	// instead fail on the duration before any cloud call.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	for _, name := range []string{"small", "medium", "large"} {
		t.Run(name, func(t *testing.T) {
			data, err := scenarios.Files.ReadFile("neutron/" + name + ".yaml")
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

func TestFinishChurnTearsDownOnInterrupt(t *testing.T) {
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
			c := &recordingCleaner{}
			// The stateless fake lists nothing by tag, so use an address scope —
			// reclaimed from the created list by id — to give teardown one thing to
			// delete without the leak check then rediscovering it by tag.
			created := []neutron.Resource{{Kind: neutron.KindAddressScope, ID: "as1"}}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)

			err := finishChurn(ctx, cmd, c, "run1234", "run-run1234.json", created, tc.interrupted, false, time.Second)
			if err != nil {
				t.Fatalf("finishChurn: %v", err)
			}
			// An interrupted run behaves as if the duration elapsed: teardown runs
			// on a live context (never seeing the parent's cancellation).
			if c.sawCancelled {
				t.Error("teardown ran with a cancelled context; it must run on context.WithoutCancel")
			}
			s := out.String()
			if !strings.Contains(s, "deleted 1 resource(s)") {
				t.Errorf("output %q missing the deletion count", s)
			}
			// The leak-check line only prints once the leak check ran to completion.
			if !strings.Contains(s, "leak check: no run-tagged resources remain") {
				t.Errorf("output %q missing the leak-check line", s)
			}
		})
	}
}

func TestFinishChurnNoCleanupSkipsTeardown(t *testing.T) {
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
			c := &recordingCleaner{}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)

			err := finishChurn(context.Background(), cmd, c, "run1234", "run-run1234.json", nil, tc.interrupted, true, time.Second)
			if err != nil {
				t.Fatalf("finishChurn: %v", err)
			}
			if c.calls != 0 {
				t.Errorf("cleaner was called %d times; --no-cleanup must leave everything in place", c.calls)
			}
			s := out.String()
			if !strings.Contains(s, tc.wantReason) {
				t.Errorf("output %q missing reason %q", s, tc.wantReason)
			}
			if !strings.Contains(s, "neutron cleanup --run run-run1234.json") {
				t.Errorf("output %q missing the reclaim hint", s)
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

// chaosModeCases are the run-mode and bucket-width merge cases every chaos
// command shares. block is appended to the namespace's sample scenario ("" for
// no chaos block at all), and flags are the chaos flags set on the command.
var chaosModeCases = []struct {
	name          string
	block         string
	flags         map[string]string
	wantUnbounded bool
	wantDuration  time.Duration
	wantWidth     time.Duration
}{
	{"--duration 0 over a block duration", "chaos:\n  duration: 1m\n", map[string]string{"duration": "0"}, true, 0, time.Hour},
	{"--duration 2h over a block duration", "chaos:\n  duration: 1m\n", map[string]string{"duration": "2h"}, false, 2 * time.Hour, time.Hour},
	{"no duration anywhere", "chaos:\n  churn_ratio: 0.5\n", nil, false, 0, time.Hour},
	{"block bucket width", "chaos:\n  duration: 1m\n  bucket_width: 30m\n", nil, false, time.Minute, 30 * time.Minute},
	{"--bucket-width over the block", "chaos:\n  duration: 1m\n  bucket_width: 30m\n", map[string]string{"bucket-width": "10m"}, false, time.Minute, 10 * time.Minute},
	{"zero block bucket width is unset", "chaos:\n  duration: 1m\n  bucket_width: 0\n", nil, false, time.Minute, time.Hour},
	{"no chaos block", "", nil, false, 0, time.Hour},
}

// setChaosFlags sets flags on cmd as a command line would and returns the
// duration and bucket width the command's flag variables then hold, the
// chaosFlags value its merge function receives.
func setChaosFlags(t *testing.T, cmd *cobra.Command, flags map[string]string) chaosFlags {
	t.Helper()
	for name, value := range flags {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("setting --%s %s: %v", name, value, err)
		}
	}
	duration, err := cmd.Flags().GetDuration("duration")
	if err != nil {
		t.Fatalf("reading --duration: %v", err)
	}
	bucketWidth, err := cmd.Flags().GetDuration("bucket-width")
	if err != nil {
		t.Fatalf("reading --bucket-width: %v", err)
	}
	return chaosFlags{duration: duration, bucketWidth: bucketWidth}
}

// checkChaosMode fails the test when cfg's run mode, duration or bucket width
// differs from the wanted ones.
func checkChaosMode(t *testing.T, cfg chaos.Config, unbounded bool, duration, width time.Duration) {
	t.Helper()
	if cfg.Unbounded != unbounded || cfg.Duration != duration || cfg.BucketWidth != width {
		t.Errorf("merged Unbounded/Duration/BucketWidth = %v/%s/%s, want %v/%s/%s",
			cfg.Unbounded, cfg.Duration, cfg.BucketWidth, unbounded, duration, width)
	}
}

// TestChaosMergeDurationAndBucketWidth proves --duration 0 given as a flag
// selects the unbounded mode while the block's duration never does, and that
// the bucket width falls back to 1h unless the block or the flag sets it.
func TestChaosMergeDurationAndBucketWidth(t *testing.T) {
	for _, tc := range chaosModeCases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := scenario.Parse([]byte(sampleScenarioYAML + "\n" + tc.block))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			opts := &globalOptions{concurrency: 8}
			cmd := newChaosCmd(opts)
			cfg := mergeChaosConfig(cmd, opts, s, setChaosFlags(t, cmd, tc.flags))
			checkChaosMode(t, cfg, tc.wantUnbounded, tc.wantDuration, tc.wantWidth)
		})
	}
}

func TestChaosDurationZeroReachesCloud(t *testing.T) {
	// --duration 0 over a block duration selects an unbounded run: the merged
	// config validates and the run proceeds to authenticate, failing only at
	// client creation with no reachable cloud.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, chaosScenarioYAML)
	_, err := execRoot(t, "neutron", "chaos", "--scenario", path, "--duration", "0")
	if err == nil {
		t.Fatal("chaos --duration 0 without a cloud: expected a client-creation failure, got nil")
	}
	if !strings.Contains(err.Error(), "network client") {
		t.Errorf("chaos --duration 0 failed before reaching cloud auth: %q", err.Error())
	}
}

func TestChaosRejectsNarrowBucketWidth(t *testing.T) {
	// A bucket width below a minute is rejected by the merged config, before
	// any cloud call.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, chaosScenarioYAML)
	_, err := execRoot(t, "neutron", "chaos", "--scenario", path, "--duration", "0", "--bucket-width", "30s")
	if err == nil {
		t.Fatal("chaos --bucket-width 30s: expected a validation error, got nil")
	}
	if !strings.Contains(err.Error(), "bucket-width must be at least 1m0s") {
		t.Errorf("error %q does not reject the narrow bucket width", err.Error())
	}
}

func TestChaosHelpListsBucketWidth(t *testing.T) {
	for _, ns := range []string{"neutron", "cinder", "glance", "keystone", "nova"} {
		t.Run(ns, func(t *testing.T) {
			out, err := execRoot(t, ns, "chaos", "--help")
			if err != nil {
				t.Fatalf("%s chaos --help: %v", ns, err)
			}
			wantLines := map[string]string{
				"--bucket-width": "(default 1h0m0s)",
				"--duration":     "0 runs until interrupted",
			}
			for flag, want := range wantLines {
				found := false
				for _, line := range strings.Split(out, "\n") {
					if strings.Contains(line, flag+" ") && strings.Contains(line, want) {
						found = true
					}
				}
				if !found {
					t.Errorf("%s chaos --help has no %s line with %q:\n%s", ns, flag, want, out)
				}
			}
		})
	}
}

// TestChaosCheckpointWritesIncompleteRecord confirms the checkpoint callback
// writes the record its build function returns, marked incomplete.
func TestChaosCheckpointWritesIncompleteRecord(t *testing.T) {
	dir := t.TempDir()
	var finished time.Time
	build := func(r *chaos.Result, at time.Time) *run.Record {
		finished = at
		return &run.Record{RunID: "ckpt0001", Service: "neutron", FinishedAt: at, Created: r.Created, Chaos: chaosStats(r)}
	}

	chaosCheckpoint(dir, build)(&chaos.Result{
		Creates:     3,
		Created:     []neutron.Resource{{Kind: neutron.KindNetwork, Logical: "net-0001", ID: "n1"}},
		BucketWidth: time.Hour,
	})

	rec, err := run.Load(filepath.Join(dir, "run-ckpt0001.json"))
	if err != nil {
		t.Fatalf("loading the checkpoint: %v", err)
	}
	if !rec.Incomplete {
		t.Error("checkpoint record is not marked incomplete")
	}
	if finished.IsZero() || !rec.FinishedAt.Equal(finished) {
		t.Errorf("checkpoint finishedAt = %s, want the time build received (%s)", rec.FinishedAt, finished)
	}
	if rec.Service != "neutron" || len(rec.Created) != 1 || rec.Created[0].ID != "n1" {
		t.Errorf("checkpoint record = %+v, want the fields build returned", rec)
	}
	if rec.Chaos == nil || rec.Chaos.Creates != 3 || rec.Chaos.BucketWidth != time.Hour {
		t.Errorf("checkpoint chaos stats = %+v, want creates 3 and bucket width 1h", rec.Chaos)
	}
}

// TestChaosCheckpointSurvivesWriteError confirms a checkpoint that cannot be
// written logs one warning and returns, so the run goes on, and prints nothing
// to stdout.
func TestChaosCheckpointSurvivesWriteError(t *testing.T) {
	var logs bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	stdout, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatalf("creating the stdout stand-in: %v", err)
	}
	prevStdout := os.Stdout
	os.Stdout = stdout
	t.Cleanup(func() { os.Stdout = prevStdout })

	build := func(_ *chaos.Result, at time.Time) *run.Record {
		return &run.Record{RunID: "ckpt0002", FinishedAt: at}
	}
	chaosCheckpoint(filepath.Join(t.TempDir(), "missing"), build)(&chaos.Result{})
	os.Stdout = prevStdout

	if got := strings.Count(logs.String(), "level=WARN"); got != 1 {
		t.Errorf("got %d warnings, want 1:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), `msg="writing run record checkpoint failed; the run continues"`) {
		t.Errorf("warning does not carry the expected message:\n%s", logs.String())
	}
	info, err := stdout.Stat()
	if err != nil {
		t.Fatalf("stat of the stdout stand-in: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("checkpoint wrote %d bytes to stdout, want 0", info.Size())
	}
}

func TestChaosDurationLabel(t *testing.T) {
	if got := chaosDurationLabel(chaos.Config{Unbounded: true, BucketWidth: time.Hour}); got != "unbounded" {
		t.Errorf("label of an unbounded config = %v, want unbounded", got)
	}
	if got := chaosDurationLabel(chaos.Config{Duration: 2 * time.Hour}); got != 2*time.Hour {
		t.Errorf("label of a bounded config = %v (%T), want the time.Duration 2h", got, got)
	}
}

func TestChaosStatsBucketWidth(t *testing.T) {
	if got := chaosStats(&chaos.Result{BucketWidth: time.Hour}).BucketWidth; got != time.Hour {
		t.Errorf("BucketWidth = %s, want 1h", got)
	}
	if got := chaosStats(&chaos.Result{}).BucketWidth; got != 0 {
		t.Errorf("BucketWidth of a bounded run = %s, want 0", got)
	}
}

// TestChaosInterrupted confirms a cancelled bounded run counts as interrupted,
// while the stop signal that ends an unbounded run does not.
func TestChaosInterrupted(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name      string
		ctx       context.Context
		unbounded bool
		want      bool
	}{
		{name: "bounded run reaching its duration", ctx: context.Background(), want: false},
		{name: "bounded run cancelled", ctx: cancelled, want: true},
		{name: "unbounded run stopped by signal", ctx: cancelled, unbounded: true, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := chaosInterrupted(tc.ctx, chaos.Config{Unbounded: tc.unbounded}); got != tc.want {
				t.Errorf("chaosInterrupted = %v, want %v", got, tc.want)
			}
		})
	}
}
