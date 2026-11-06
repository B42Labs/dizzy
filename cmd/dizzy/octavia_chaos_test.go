package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/octavia"
	octaviascenario "github.com/B42Labs/dizzy/internal/octavia/scenario"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/scenarios"
)

// octaviaChaosScenarioYAML is sampleOctaviaScenarioYAML extended with a chaos
// block (including the load balancer update_ratio), used to exercise the
// octavia chaos command's config merge.
const octaviaChaosScenarioYAML = sampleOctaviaScenarioYAML + `
chaos:
  duration: 1m
  interval: { min: 5s, max: 10s }
  parallel: { max: 3 }
  churn_ratio: 0.5
  target_fill: 0.8
  update_ratio: 0.3
`

// shippedOctaviaProfile copies the shipped scenarios/octavia/<name>.yaml
// profile into a temporary file and returns its path, so a test exercises the
// exact bytes the repository ships without depending on the working directory.
func shippedOctaviaProfile(t *testing.T, name string) string {
	t.Helper()
	data, err := scenarios.Files.ReadFile("octavia/" + name + ".yaml")
	if err != nil {
		t.Fatalf("reading shipped profile %s.yaml: %v", name, err)
	}
	return writeScenario(t, string(data))
}

func TestOctaviaChaosRequiresScenario(t *testing.T) {
	_, err := execRoot(t, "octavia", "chaos")
	if err == nil || !strings.Contains(err.Error(), `"scenario" not set`) {
		t.Fatalf("octavia chaos without --scenario = %v, want the missing-flag error", err)
	}
}

func TestOctaviaChaosRequiresDuration(t *testing.T) {
	// A scenario with no chaos block and no --duration flag has no duration, so
	// the merged config is rejected before any cloud call.
	path := writeScenario(t, sampleOctaviaScenarioYAML)
	_, err := execRoot(t, "octavia", "chaos", "--scenario", path)
	if want := "chaos duration must be set and positive, got 0s"; err == nil || err.Error() != want {
		t.Fatalf("octavia chaos without a duration = %v, want %q", err, want)
	}
}

func TestOctaviaChaosUpdateRatioFlagOverridesBlock(t *testing.T) {
	// The block sets a valid update_ratio 0.3; --update-ratio 2 overrides it with
	// an out-of-range value the merged config rejects — proving the flag
	// overrides the block and reaches Validate before any cloud call.
	path := writeScenario(t, octaviaChaosScenarioYAML)
	_, err := execRoot(t, "octavia", "chaos", "--scenario", path, "--update-ratio", "2")
	if err == nil {
		t.Fatal("octavia chaos with --update-ratio 2: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "resize-ratio") {
		// The update ratio maps onto the engine's ResizeRatio knob, whose
		// validation names it "resize-ratio".
		t.Errorf("error %q does not mention the rejected ratio", err.Error())
	}
}

// TestOctaviaChaosMergeUpdateRatio proves the pointer-valued update_ratio
// merges correctly: an explicit 0 in the chaos block disables the toggle — the
// block value is distinguishable from an omitted key — while an omitted key
// falls back to the default, and --update-ratio overrides either.
func TestOctaviaChaosMergeUpdateRatio(t *testing.T) {
	tests := []struct {
		name  string
		block string
		flag  string
		want  float64
	}{
		{"explicit zero disables the toggle", "  update_ratio: 0\n", "", 0},
		{"omitted falls back to default", "", "", defaultChaosUpdateRatio},
		{"flag overrides an explicit block value", "  update_ratio: 0\n", "0.7", 0.7},
		{"flag overrides an omitted key", "", "0.7", 0.7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := octaviascenario.Parse([]byte(sampleOctaviaScenarioYAML + "\nchaos:\n  duration: 1m\n" + tc.block))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			opts := &globalOptions{concurrency: 8}
			cmd := newOctaviaChaosCmd(opts)
			if tc.flag != "" {
				if err := cmd.Flags().Set("update-ratio", tc.flag); err != nil {
					t.Fatalf("setting --update-ratio %s: %v", tc.flag, err)
				}
			}
			updateRatio, err := cmd.Flags().GetFloat64("update-ratio")
			if err != nil {
				t.Fatalf("reading --update-ratio: %v", err)
			}
			cfg := mergeOctaviaChaosConfig(cmd, opts, s, chaosFlags{}, updateRatio)
			if cfg.ResizeRatio != tc.want {
				t.Errorf("merged ResizeRatio = %v, want %v", cfg.ResizeRatio, tc.want)
			}
		})
	}
}

// TestOctaviaChaosMergeDurationAndBucketWidth proves --duration 0 given as a
// flag selects the unbounded mode while the block's duration never does, and
// that the bucket width falls back to 1h unless the block or the flag sets it.
func TestOctaviaChaosMergeDurationAndBucketWidth(t *testing.T) {
	for _, tc := range chaosModeCases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := octaviascenario.Parse([]byte(sampleOctaviaScenarioYAML + "\n" + tc.block))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			opts := &globalOptions{concurrency: 8}
			cmd := newOctaviaChaosCmd(opts)
			cfg := mergeOctaviaChaosConfig(cmd, opts, s, setChaosFlags(t, cmd, tc.flags), defaultChaosUpdateRatio)
			checkChaosMode(t, cfg, tc.wantUnbounded, tc.wantDuration, tc.wantWidth)
		})
	}
}

// TestOctaviaChaosShippedProfilesReachCloud confirms each built-in Octavia
// profile ships a chaos block, so octavia chaos needs no --duration: the
// merged config validates, alone and with --duration 0 for an unbounded run,
// and the run proceeds to authenticate, failing only while creating the
// clients with no reachable cloud, before any run record is written.
func TestOctaviaChaosShippedProfilesReachCloud(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")
	t.Chdir(t.TempDir())

	for _, name := range []string{"small", "medium", "large"} {
		for _, extra := range [][]string{nil, {"--duration", "0"}} {
			args := append([]string{"octavia", "chaos", "--scenario", shippedOctaviaProfile(t, name)}, extra...)
			_, err := execRoot(t, args...)
			if err == nil || !strings.HasPrefix(err.Error(), "creating load balancer clients: ") {
				t.Errorf("octavia chaos %s %v = %v, want a failure while creating the load balancer clients", name, extra, err)
			}
		}
	}
	records, err := filepath.Glob("run-*.json")
	if err != nil {
		t.Fatalf("listing run records: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("run records %v were written, want none", records)
	}
}

// chaosOctaviaCreated is the Created list of a churn run: a VIP network and its
// subnet, a load balancer, and one listener, pool, member, and health monitor,
// in the engine's node order.
func chaosOctaviaCreated() []resource.Resource {
	return []resource.Resource{
		{Kind: octavia.KindNetwork, Logical: "net-0001", ID: "net1"},
		{Kind: octavia.KindSubnet, Logical: "sub-0001", ID: "sub1"},
		{Kind: octavia.KindLoadBalancer, Logical: "lb-0001", ID: "lb1"},
		{Kind: octavia.KindListener, Logical: "lsn-0001", ID: "lsn1"},
		{Kind: octavia.KindPool, Logical: "pool-0001", ID: "pool1"},
		{Kind: octavia.KindMember, Logical: "pool-0001-member-01", ID: "mem1"},
		{Kind: octavia.KindHealthMonitor, Logical: "hm-0001", ID: "hm1"},
	}
}

func TestFinishOctaviaChurnNoCleanup(t *testing.T) {
	tests := []struct {
		name        string
		interrupted bool
		recordPath  string
		want        string
	}{
		{"interrupted with a record", true, "run-run1234.json",
			"churn interrupted; resources left in place — reclaim with: octavia cleanup --run run-run1234.json\n"},
		{"complete without a record", false, "",
			"churn complete; resources left in place — reclaim with: octavia cleanup --run-id run1234\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingOctaviaCleaner{}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)

			if err := finishOctaviaChurn(context.Background(), cmd, c, "run1234", tc.recordPath, chaosOctaviaCreated(), tc.interrupted, true, 1, time.Second); err != nil {
				t.Fatalf("finishOctaviaChurn: %v", err)
			}
			if out.String() != tc.want {
				t.Errorf("output = %q, want %q", out.String(), tc.want)
			}
			if len(c.calls) != 0 {
				t.Errorf("--no-cleanup made cloud calls %v, want none", c.calls)
			}
		})
	}
}

// TestFinishOctaviaChurnTearsDown confirms a churn run, completed or
// interrupted, tears down on a live, bounded context: the recorded load
// balancer is deleted with the cascade and waited on before the subnet and the
// network, the children are left to the cascade, and the leak check lists the
// three discoverable kinds.
func TestFinishOctaviaChurnTearsDown(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		ctx := context.Background()
		if interrupted {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel() // a first signal cancelled the run
		}
		c := &recordingOctaviaCleaner{}
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)

		if err := finishOctaviaChurn(ctx, cmd, c, "run1234", "run-run1234.json", chaosOctaviaCreated(), interrupted, false, 1, time.Second); err != nil {
			t.Fatalf("finishOctaviaChurn (interrupted %t): %v", interrupted, err)
		}
		if c.sawCancelled {
			t.Error("teardown ran with a cancelled context; it must run on context.WithoutCancel")
		}
		if c.sawNoDeadline {
			t.Error("a teardown call ran without a deadline; every call must be bounded by octaviaTimeoutCleaner")
		}
		want := []string{
			"list loadbalancer", "delete loadbalancer lb1", "wait loadbalancer lb1",
			"list subnet", "delete subnet sub1",
			"list network", "delete network net1",
			"list loadbalancer", "list subnet", "list network",
		}
		if !slices.Equal(c.calls, want) {
			t.Errorf("calls = %v, want %v", c.calls, want)
		}
		if want := "deleted 3 resource(s) for run run1234\nleak check: no run-tagged resources remain\n"; out.String() != want {
			t.Errorf("output = %q, want %q", out.String(), want)
		}
	}
}

// leakyOctaviaCleaner is a recordingOctaviaCleaner whose listings return
// leftover for their kind, and whose listing of failKind fails from its
// failFrom-th call on.
type leakyOctaviaCleaner struct {
	recordingOctaviaCleaner
	leftover map[resource.Kind][]resource.Resource
	failKind resource.Kind
	failFrom int

	mu     sync.Mutex
	listed map[resource.Kind]int
}

func (c *leakyOctaviaCleaner) ListByTag(ctx context.Context, kind resource.Kind, runID string) ([]resource.Resource, error) {
	if _, err := c.recordingOctaviaCleaner.ListByTag(ctx, kind, runID); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.listed == nil {
		c.listed = map[resource.Kind]int{}
	}
	c.listed[kind]++
	if kind == c.failKind && c.listed[kind] >= c.failFrom {
		return nil, errors.New("list boom")
	}
	return c.leftover[kind], nil
}

func TestFinishOctaviaChurnReportsLeak(t *testing.T) {
	c := &leakyOctaviaCleaner{leftover: map[resource.Kind][]resource.Resource{
		octavia.KindLoadBalancer: {{Kind: octavia.KindLoadBalancer, Logical: "lb-0002", ID: "lb2"}},
	}}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := finishOctaviaChurn(context.Background(), cmd, c, "run1234", "run-run1234.json", chaosOctaviaCreated(), false, false, 1, time.Second); err != nil {
		t.Fatalf("finishOctaviaChurn: %v", err)
	}
	if want := "leak check: 1 run-tagged resource(s) still present after teardown\n"; !strings.HasSuffix(out.String(), want) {
		t.Errorf("output = %q, want it to end with %q", out.String(), want)
	}
}

func TestFinishOctaviaChurnLeakCheckListingError(t *testing.T) {
	// Teardown lists subnets once; the leak check's listing is the second.
	c := &leakyOctaviaCleaner{failKind: octavia.KindSubnet, failFrom: 2}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})

	err := finishOctaviaChurn(context.Background(), cmd, c, "run1234", "run-run1234.json", chaosOctaviaCreated(), false, false, 1, time.Second)
	if err == nil || !strings.HasPrefix(err.Error(), "leak check listing subnet: ") {
		t.Fatalf("finishOctaviaChurn = %v, want a leak check listing error for subnets", err)
	}
}

func TestFinishOctaviaChurnTeardownError(t *testing.T) {
	c := &recordingOctaviaCleaner{
		failKinds: map[resource.Kind]bool{octavia.KindLoadBalancer: true},
		deleteErr: errors.New("delete boom"),
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := finishOctaviaChurn(context.Background(), cmd, c, "run1234", "run-run1234.json", chaosOctaviaCreated(), false, false, 1, time.Second)
	if err == nil || !strings.HasPrefix(err.Error(), "tearing down run run1234: ") || !strings.Contains(err.Error(), "delete boom") {
		t.Fatalf("finishOctaviaChurn = %v, want a teardown error naming the run and the delete failure", err)
	}
	if want := "deleted 0 resource(s) for run run1234\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}
