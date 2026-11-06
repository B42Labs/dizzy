package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/octavia"
	octaviaplan "github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
)

// octaviaSmallProfile is the shipped small Octavia profile, addressed from this
// package's directory, where go test runs, so a test drives the exact file the
// acceptance criteria name.
var octaviaSmallProfile = filepath.Join("..", "..", "scenarios", "octavia", "small.yaml")

// octaviaSmallGolden is the golden plan of the small profile, which only
// go test ./internal/octavia/scenario/ -update regenerates.
var octaviaSmallGolden = filepath.Join("..", "..", "internal", "octavia", "scenario", "testdata", "golden", "small.plan.json")

// octaviaSmallSummary is the exact dry-run summary of the small profile.
const octaviaSmallSummary = `Plan for scenario "small" (seed 42)
  provider:         cloud default
  networks:         1
  load balancers:   2
  listeners:        4
  pools:            4
  members:          8
  health monitors:  2
`

// sampleOctaviaScenarioYAML is a small but complete Octavia scenario used by
// the octavia command tests.
const sampleOctaviaScenarioYAML = `
name: cli
seed: 5
provider: ""
resources:
  networks:        1
  loadbalancers:   2
  listeners:       2
  members:         4
  health_monitors: 1
distribution:
  listener_port: { min: 1024, max: 65535 }
  member_port:   { min: 1024, max: 65535 }
`

// noOctaviaCloud points cloud configuration at nothing, with OS_CLOUD unset, so
// a command that reaches for a cloud fails parsing clouds.yaml before any
// request.
func noOctaviaCloud(t *testing.T) {
	t.Helper()
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")
}

// octaviaHelpListing runs "dizzy octavia --help" and returns the first word of
// every indented help line, which covers each listed subcommand.
func octaviaHelpListing(t *testing.T) (map[string]bool, string) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"octavia", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("octavia --help: %v", err)
	}
	listed := map[string]bool{}
	for _, line := range strings.Split(out.String(), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && strings.HasPrefix(line, "  ") {
			listed[fields[0]] = true
		}
	}
	return listed, out.String()
}

// TestOctaviaHelpListsSingleShotVerbsOnly confirms dizzy octavia --help lists
// generate, apply, status, report, and cleanup, and neither chaos nor monitor,
// which the namespace does not offer yet.
func TestOctaviaHelpListsSingleShotVerbsOnly(t *testing.T) {
	listed, out := octaviaHelpListing(t)
	for _, name := range []string{"generate", "apply", "status", "report", "cleanup"} {
		if !listed[name] {
			t.Errorf("octavia --help does not list %q:\n%s", name, out)
		}
	}
	for _, name := range []string{"chaos", "monitor"} {
		if listed[name] {
			t.Errorf("octavia --help lists %q, which the namespace does not offer:\n%s", name, out)
		}
	}
}

// TestRootRegistersOctaviaNamespace confirms the root command registers
// the octavia namespace with its short description, and names Octavia load
// balancer lifecycles in its long description.
func TestRootRegistersOctaviaNamespace(t *testing.T) {
	root := newRootCmd()
	cmd := findSubcommand(root, "octavia")
	if cmd == nil {
		t.Fatal("octavia command not registered on root")
	}
	if want := "Octavia (load balancer) lifecycle load and consistency commands"; cmd.Short != want {
		t.Errorf("octavia Short = %q, want %q", cmd.Short, want)
	}
	if !strings.Contains(strings.Join(strings.Fields(root.Long), " "), "Octavia load balancer lifecycles") {
		t.Errorf("root Long does not name Octavia load balancer lifecycles:\n%s", root.Long)
	}
}

// TestOctaviaGenerateSmallProfileMatchesGolden covers running dizzy octavia
// generate --scenario scenarios/octavia/small.yaml twice: both outputs are
// byte-identical and equal to the golden plan, and --out writes the same
// bytes to a file.
func TestOctaviaGenerateSmallProfileMatchesGolden(t *testing.T) {
	first, err := execRoot(t, "octavia", "generate", "--scenario", octaviaSmallProfile)
	if err != nil {
		t.Fatalf("generate (first): %v", err)
	}
	second, err := execRoot(t, "octavia", "generate", "--scenario", octaviaSmallProfile)
	if err != nil {
		t.Fatalf("generate (second): %v", err)
	}
	if first != second {
		t.Errorf("two generations of the small profile differ:\n%s\n---\n%s", first, second)
	}

	golden, err := os.ReadFile(octaviaSmallGolden)
	if err != nil {
		t.Fatalf("reading golden plan: %v", err)
	}
	if first != string(golden) {
		t.Errorf("generated plan differs from the golden plan %s:\ngot:\n%s\nwant:\n%s", octaviaSmallGolden, first, golden)
	}

	outPath := filepath.Join(t.TempDir(), "plan.json")
	stdout, err := execRoot(t, "octavia", "generate", "--scenario", octaviaSmallProfile, "--out", outPath)
	if err != nil {
		t.Fatalf("generate --out: %v", err)
	}
	if stdout != "" {
		t.Errorf("generate --out wrote %q to stdout, want nothing", stdout)
	}
	written, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading --out plan: %v", err)
	}
	if !bytes.Equal(written, golden) {
		t.Errorf("--out plan differs from the golden plan:\n%s", written)
	}
}

// TestOctaviaGenerateSetOverrides confirms --set reaches the plan: the provider
// lands in the plan JSON, and a resource count changes its collection.
func TestOctaviaGenerateSetOverrides(t *testing.T) {
	path := writeScenario(t, sampleOctaviaScenarioYAML)

	out, err := execRoot(t, "octavia", "generate", "--scenario", path,
		"--set", "provider=ovn", "--set", "resources.members=6")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var p octaviaplan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if p.Provider != "ovn" {
		t.Errorf("provider = %q, want ovn after override", p.Provider)
	}
	if !strings.Contains(out, `"provider": "ovn"`) {
		t.Errorf("plan JSON does not carry the provider:\n%s", out)
	}
	if len(p.Members) != 6 {
		t.Errorf("members = %d, want 6 after override", len(p.Members))
	}
}

// TestOctaviaGenerateSeedOverride confirms the global --seed replaces the
// scenario seed and changes the drawn plan.
func TestOctaviaGenerateSeedOverride(t *testing.T) {
	path := writeScenario(t, sampleOctaviaScenarioYAML)

	base, err := execRoot(t, "octavia", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate (file seed): %v", err)
	}
	overridden, err := execRoot(t, "octavia", "generate", "--scenario", path, "--seed", "7")
	if err != nil {
		t.Fatalf("generate (--seed): %v", err)
	}
	if base == overridden {
		t.Error("global --seed did not change the generated plan")
	}
	var p octaviaplan.Plan
	if err := json.Unmarshal([]byte(overridden), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if p.Seed != 7 {
		t.Errorf("plan seed = %d, want 7", p.Seed)
	}
}

// TestOctaviaGenerateIgnoresChaosBlock confirms generate accepts an Octavia
// scenario carrying a chaos block and prints the same plan as for the scenario
// without it: the block configures octavia chaos, and generate ignores it.
func TestOctaviaGenerateIgnoresChaosBlock(t *testing.T) {
	without, err := execRoot(t, "octavia", "generate", "--scenario", writeScenario(t, sampleOctaviaScenarioYAML))
	if err != nil {
		t.Fatalf("generate without a chaos block: %v", err)
	}
	with, err := execRoot(t, "octavia", "generate", "--scenario", writeScenario(t, sampleOctaviaScenarioYAML+"chaos:\n  duration: 1m\n"))
	if err != nil {
		t.Fatalf("generate with a chaos block: %v", err)
	}
	var p octaviaplan.Plan
	if err := json.Unmarshal([]byte(without), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if with != without {
		t.Errorf("generate with a chaos block printed:\n%s\nwant the plan printed without it:\n%s", with, without)
	}
}

// TestOctaviaGenerateErrors confirms generate rejects a missing or unreadable
// scenario, another service's scenario, an unknown override key, and a
// malformed --set.
func TestOctaviaGenerateErrors(t *testing.T) {
	valid := writeScenario(t, sampleOctaviaScenarioYAML)
	// A Glance scenario's keys are unknown to the Octavia schema, so strict
	// parsing must reject it.
	glanceScenario := writeScenario(t, sampleGlanceScenarioYAML)

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"missing scenario flag", []string{"octavia", "generate"}, `"scenario" not set`},
		{"nonexistent file", []string{"octavia", "generate", "--scenario", filepath.Join(t.TempDir(), "nope.yaml")}, "reading scenario"},
		{"glance scenario", []string{"octavia", "generate", "--scenario", glanceScenario}, "not found"},
		{"unknown set key", []string{"octavia", "generate", "--scenario", valid, "--set", "pools=3"}, `unknown override key "pools"`},
		{"malformed set", []string{"octavia", "generate", "--scenario", valid, "--set", "provider"}, `invalid --set "provider": want key=value`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execRoot(t, tc.args...)
			if err == nil {
				t.Fatalf("expected error for args %v, got nil", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestOctaviaApplyDryRunPrintsExactSummaryWithoutRequest covers dizzy octavia
// apply --dry-run --scenario scenarios/octavia/small.yaml: with OS_CLOUD unset
// and no clouds.yaml it succeeds and prints exactly the plan summary, and even
// with a reachable cloud configured it sends that cloud no request.
func TestOctaviaApplyDryRunPrintsExactSummaryWithoutRequest(t *testing.T) {
	t.Run("no cloud configured", func(t *testing.T) {
		noOctaviaCloud(t)
		out, err := execRoot(t, "octavia", "apply", "--dry-run", "--scenario", octaviaSmallProfile)
		if err != nil {
			t.Fatalf("apply --dry-run: %v", err)
		}
		if out != octaviaSmallSummary {
			t.Errorf("dry-run output:\n%s\nwant exactly:\n%s", out, octaviaSmallSummary)
		}
	})
	t.Run("configured cloud receives no request", func(t *testing.T) {
		fc := newFakeOctaviaCloud(t, nil)
		out, err := execRoot(t, "octavia", "apply", "--dry-run", "--scenario", octaviaSmallProfile, "--os-cloud", "oct")
		if err != nil {
			t.Fatalf("apply --dry-run: %v", err)
		}
		if out != octaviaSmallSummary {
			t.Errorf("dry-run output:\n%s\nwant exactly:\n%s", out, octaviaSmallSummary)
		}
		if got := fc.requests(); len(got) != 0 {
			t.Errorf("dry-run sent %v, want no request", got)
		}
	})
	t.Run("provider override", func(t *testing.T) {
		noOctaviaCloud(t)
		out, err := execRoot(t, "octavia", "apply", "--dry-run", "--scenario", octaviaSmallProfile, "--set", "provider=amphora")
		if err != nil {
			t.Fatalf("apply --dry-run: %v", err)
		}
		if want := strings.Replace(octaviaSmallSummary, "cloud default", "amphora", 1); out != want {
			t.Errorf("dry-run output:\n%s\nwant exactly:\n%s", out, want)
		}
	})
}

// TestOctaviaApplyWithoutCloudFailsCreatingClientsBeforeRecord confirms apply
// without --dry-run and without a cloud fails while creating the load
// balancer clients, before any run record is written.
func TestOctaviaApplyWithoutCloudFailsCreatingClientsBeforeRecord(t *testing.T) {
	noOctaviaCloud(t)
	path := writeScenario(t, sampleOctaviaScenarioYAML)
	// Run from a fresh directory so a run record written to "." would show up.
	t.Chdir(t.TempDir())

	_, err := execRoot(t, "octavia", "apply", "--scenario", path)
	if err == nil {
		t.Fatal("apply without --dry-run: expected error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "creating load balancer clients: ") {
		t.Errorf("error %q does not come from creating the load balancer clients", err.Error())
	}
	assertNoRunRecord(t)
}

// TestOctaviaApplyQuotaReadFailureAbortsBeforeAnyWrite confirms apply runs the
// quota pre-check after creating the clients and returns its error as is: a
// quota read answered 500 aborts the run before any Octavia or Neutron write,
// without a run record.
func TestOctaviaApplyQuotaReadFailureAbortsBeforeAnyWrite(t *testing.T) {
	fc := newFakeOctaviaCloud(t, map[string]octaviaReply{
		"GET /load-balancer/v2.0/quotas/proj-oct": {http.StatusInternalServerError, `{"faultstring":"boom"}`},
	})
	path := writeScenario(t, sampleOctaviaScenarioYAML)
	t.Chdir(t.TempDir())

	_, err := execRoot(t, "octavia", "apply", "--scenario", path, "--os-cloud", "oct")
	if err == nil || !strings.HasPrefix(err.Error(), "reading Octavia quotas for pre-check: ") {
		t.Fatalf("apply = %v, want the quota read error as is", err)
	}
	// Authentication and the load-balancer version document precede the quota
	// read; nothing follows it.
	want := []string{"POST /v3/auth/tokens", "GET /load-balancer/", "GET /load-balancer/v2.0/quotas/proj-oct"}
	if got := fc.requests(); !slices.Equal(got, want) {
		t.Errorf("requests = %v, want only %v", got, want)
	}
	assertNoRunRecord(t)
}

// TestOctaviaApplyKeepOnAbortFlagParses confirms --keep-on-abort is
// registered: --dry-run returns before signal setup, so a successful parse
// needs no cloud.
func TestOctaviaApplyKeepOnAbortFlagParses(t *testing.T) {
	path := writeScenario(t, sampleOctaviaScenarioYAML)
	if _, err := execRoot(t, "octavia", "apply", "--scenario", path, "--dry-run", "--keep-on-abort"); err != nil {
		t.Fatalf("octavia apply --dry-run --keep-on-abort: %v", err)
	}
}

// assertNoRunRecord fails the test when the working directory holds a run
// record.
func assertNoRunRecord(t *testing.T) {
	t.Helper()
	records, err := filepath.Glob("run-*")
	if err != nil {
		t.Fatalf("listing run records: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("apply that failed before any write left %v, want no run record", records)
	}
}

// recordingOctaviaCleaner is a fake octaviaexec.Cleaner that records every
// call as "<op> <kind> <id>", whether the contexts it is handed were cancelled
// or carried no deadline, and whether the run record existed at its first
// call. Its listings return nothing, so the only deletes are the recorded
// resources passed through the created list. A delete of a kind in failKinds
// returns deleteErr instead. Cleanup deletes load balancers from a worker
// pool, so the fake is safe for concurrent use.
type recordingOctaviaCleaner struct {
	mu                 sync.Mutex
	recordWritten      *bool
	firstCallSawRecord bool
	calls              []string
	sawCancelled       bool
	sawNoDeadline      bool
	failKinds          map[resource.Kind]bool
	deleteErr          error
}

// observe records one call of op on kind and id under ctx.
func (c *recordingOctaviaCleaner) observe(ctx context.Context, op string, kind resource.Kind, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 && c.recordWritten != nil {
		c.firstCallSawRecord = *c.recordWritten
	}
	c.calls = append(c.calls, strings.TrimSpace(op+" "+string(kind)+" "+id))
	if ctx.Err() != nil {
		c.sawCancelled = true
	}
	if _, ok := ctx.Deadline(); !ok {
		c.sawNoDeadline = true
	}
}

// ListByTag records the listing and discovers nothing.
func (c *recordingOctaviaCleaner) ListByTag(ctx context.Context, kind resource.Kind, _ string) ([]resource.Resource, error) {
	c.observe(ctx, "list", kind, "")
	return nil, nil
}

// Delete records the delete and fails it when its kind is in failKinds.
func (c *recordingOctaviaCleaner) Delete(ctx context.Context, r resource.Resource) error {
	c.observe(ctx, "delete", r.Kind, r.ID)
	if c.failKinds[r.Kind] {
		return c.deleteErr
	}
	return nil
}

// WaitForGone records the wait and reports the resource gone.
func (c *recordingOctaviaCleaner) WaitForGone(ctx context.Context, r resource.Resource) error {
	c.observe(ctx, "wait", r.Kind, r.ID)
	return nil
}

// abortedOctaviaCreated is the partial Created list of an interrupted run, in
// the order apply records it: a VIP network and its subnet, a load balancer,
// and one listener, pool, member, and health monitor.
func abortedOctaviaCreated() []resource.Resource {
	return []resource.Resource{
		{Kind: octavia.KindNetwork, Logical: "net-0001", ID: "net1"},
		{Kind: octavia.KindSubnet, Logical: "net-0001", ID: "sub1"},
		{Kind: octavia.KindLoadBalancer, Logical: "lb-0001", ID: "lb1"},
		{Kind: octavia.KindListener, Logical: "lsn-0001", ID: "lsn1"},
		{Kind: octavia.KindPool, Logical: "pool-0001", ID: "pool1"},
		{Kind: octavia.KindMember, Logical: "pool-0001-member-01", ID: "mem1"},
		{Kind: octavia.KindHealthMonitor, Logical: "hm-0001", ID: "hm1"},
	}
}

// TestFinishAbortedOctaviaApplyTearsDownOnLiveContext confirms an interrupted
// apply writes its record first, then tears the run down on a context that is
// not cancelled and bounds every call: the load balancer is deleted and waited
// on before the subnet and the network, the children are left to the cascade,
// and the count is printed.
func TestFinishAbortedOctaviaApplyTearsDownOnLiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a first signal cancelled the run

	recordWritten := false
	c := &recordingOctaviaCleaner{recordWritten: &recordWritten}
	var out bytes.Buffer
	writeRecord := func() (string, error) {
		recordWritten = true
		return "run-abcd1234.json", nil
	}

	err := finishAbortedOctaviaApply(ctx, &out, c, "abcd1234", abortedOctaviaCreated(), false, errors.New("apply boom"), 4, time.Second, writeRecord)

	if err == nil {
		t.Fatal("finishAbortedOctaviaApply returned nil; an interrupted apply must exit non-zero")
	}
	for _, want := range []string{"abcd1234", "apply boom", "interrupted; 3 created resource(s) torn down"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if c.sawCancelled {
		t.Error("teardown ran with a cancelled context; it must run on context.WithoutCancel")
	}
	if c.sawNoDeadline {
		t.Error("a teardown call ran without a deadline; every call must be bounded by octaviaTimeoutCleaner")
	}
	if !c.firstCallSawRecord {
		t.Error("the run record was not written before teardown began")
	}
	want := []string{
		"list loadbalancer", "delete loadbalancer lb1", "wait loadbalancer lb1",
		"list subnet", "delete subnet sub1",
		"list network", "delete network net1",
	}
	if !slices.Equal(c.calls, want) {
		t.Errorf("teardown calls = %v, want %v", c.calls, want)
	}
	if want := "deleted 3 resource(s) for run abcd1234\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// TestFinishAbortedOctaviaApplyKeepOnAbortLeavesResources confirms that with
// keepOnAbort nothing is torn down and the reclaim hint names the record, or
// the run id when no record was written.
func TestFinishAbortedOctaviaApplyKeepOnAbortLeavesResources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name       string
		recordPath string
		wantHint   string
	}{
		{"with record path", "run-keep.json", "reclaim with: octavia cleanup --run run-keep.json"},
		{"empty record path falls back to run-id", "", "reclaim with: octavia cleanup --run-id keep1234"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingOctaviaCleaner{}
			var out bytes.Buffer
			writeRecord := func() (string, error) { return tc.recordPath, nil }

			err := finishAbortedOctaviaApply(ctx, &out, c, "keep1234", abortedOctaviaCreated(), true, errors.New("apply boom"), 4, time.Second, writeRecord)

			if err == nil || !strings.Contains(err.Error(), "keep1234") {
				t.Errorf("error = %v, want a non-nil error naming the run id", err)
			}
			if len(c.calls) != 0 {
				t.Errorf("cleaner was called %v; --keep-on-abort must not tear anything down", c.calls)
			}
			if !strings.Contains(out.String(), tc.wantHint) {
				t.Errorf("output %q missing hint %q", out.String(), tc.wantHint)
			}
		})
	}
}

// TestFinishAbortedOctaviaApplyReportsIncompleteTeardown confirms a teardown
// that fails partway returns an error naming the run id, the count deleted so
// far, the failure, and the hint to reclaim the rest, and leaves the network
// untouched once its subnet delete fails.
func TestFinishAbortedOctaviaApplyReportsIncompleteTeardown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &recordingOctaviaCleaner{
		failKinds: map[resource.Kind]bool{octavia.KindSubnet: true},
		deleteErr: errors.New("subnet boom"),
	}
	var out bytes.Buffer
	writeRecord := func() (string, error) { return "run-abcd1234.json", nil }

	err := finishAbortedOctaviaApply(ctx, &out, c, "abcd1234", abortedOctaviaCreated(), false, errors.New("apply boom"), 4, time.Second, writeRecord)

	if err == nil {
		t.Fatal("finishAbortedOctaviaApply returned nil for a failed teardown")
	}
	for _, want := range []string{"abcd1234", "teardown incomplete after deleting 1 resource(s)", "subnet boom", "reclaim the rest with: octavia cleanup --run run-abcd1234.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
	for _, call := range c.calls {
		if strings.HasPrefix(call, "delete network") || strings.HasPrefix(call, "list network") {
			t.Errorf("teardown reached %q after a subnet delete failed", call)
		}
	}
	if want := "deleted 1 resource(s) for run abcd1234\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// blockingOctaviaCleaner mimics a wedged load balancer or network API: every
// operation blocks until its context is done. The abort teardown drives it on
// a deadline-free context, so without a per-operation timeout it would block
// forever.
type blockingOctaviaCleaner struct{}

// ListByTag blocks until ctx is done.
func (blockingOctaviaCleaner) ListByTag(ctx context.Context, _ resource.Kind, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// Delete blocks until ctx is done.
func (blockingOctaviaCleaner) Delete(ctx context.Context, _ resource.Resource) error {
	<-ctx.Done()
	return ctx.Err()
}

// WaitForGone blocks until ctx is done.
func (blockingOctaviaCleaner) WaitForGone(ctx context.Context, _ resource.Resource) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestOctaviaTimeoutCleanerBoundsWedgedOperation confirms every operation of
// octaviaTimeoutCleaner, invoked on a deadline-free parent context, returns
// promptly with the deadline error instead of blocking indefinitely: a wedged
// Delete, listing, or deletion wait is bounded by the timeout.
func TestOctaviaTimeoutCleanerBoundsWedgedOperation(t *testing.T) {
	tc := octaviaTimeoutCleaner{inner: blockingOctaviaCleaner{}, opTimeout: 10 * time.Millisecond}
	lb := resource.Resource{Kind: octavia.KindLoadBalancer, ID: "lb1"}

	ops := map[string]func(context.Context) error{
		"Delete": func(ctx context.Context) error { return tc.Delete(ctx, lb) },
		"ListByTag": func(ctx context.Context) error {
			_, err := tc.ListByTag(ctx, octavia.KindLoadBalancer, "r1")
			return err
		},
		"WaitForGone": func(ctx context.Context) error { return tc.WaitForGone(ctx, lb) },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- op(context.Background()) }()

			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("%s err = %v, want context.DeadlineExceeded", name, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s did not return; octaviaTimeoutCleaner failed to bound the wedged operation", name)
			}
		})
	}
}

// TestOctaviaStatusRequiresRunFlag confirms status refuses to run without
// --run.
func TestOctaviaStatusRequiresRunFlag(t *testing.T) {
	_, err := execRoot(t, "octavia", "status")
	if err == nil || !strings.Contains(err.Error(), `"run" not set`) {
		t.Fatalf("status without --run = %v, want the required-flag error", err)
	}
}

// TestOctaviaCleanupRequiresRunOrRunID confirms cleanup needs exactly one of
// --run and --run-id.
func TestOctaviaCleanupRequiresRunOrRunID(t *testing.T) {
	for name, args := range map[string][]string{
		"neither": {"octavia", "cleanup"},
		"both":    {"octavia", "cleanup", "--run", "run-x.json", "--run-id", "x"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := execRoot(t, args...)
			if want := "exactly one of --run or --run-id is required"; err == nil || err.Error() != want {
				t.Errorf("cleanup %v = %v, want %q", args[2:], err, want)
			}
		})
	}
}

// TestOctaviaRejectsGlanceRecord confirms the service guard stops octavia
// status and cleanup from operating on a glance run record, whose resource
// kinds the octavia client cannot handle, before either reaches for a cloud.
func TestOctaviaRejectsGlanceRecord(t *testing.T) {
	noOctaviaCloud(t)
	glanceRec, err := run.Write(t.TempDir(), &run.Record{RunID: "gla00001", Service: "glance"})
	if err != nil {
		t.Fatalf("seeding glance record: %v", err)
	}

	want := `run record is for service "glance", not "octavia"`
	for _, verb := range []string{"status", "cleanup"} {
		if _, err := execRoot(t, "octavia", verb, "--run", glanceRec); err == nil || err.Error() != want {
			t.Errorf("octavia %s on a glance record = %v, want %q", verb, err, want)
		}
	}
}

// octaviaReply is a canned answer of the fake Octavia cloud: an HTTP status and
// a JSON body, none when empty.
type octaviaReply struct {
	status int
	body   string
}

// fakeOctaviaCloud is a Keystone, Octavia, and Neutron in one httptest server.
// Its token is scoped to the project proj-oct, and its catalog points the
// load-balancer service at /load-balancer/, without a version as Octavia's
// catalog URL carries none, so the fake answers Octavia's version document and
// the load-balancer client appends v2.0/ to it. The network endpoint
// /network/v2.0/ carries its major version, so the client skips discovery, and
// Neutron's client appends v2.0/ once more. Every other request is answered
// from replies, keyed by "<method> <path>", or else with 404.
// newFakeOctaviaCloud names it "oct" in clouds.yaml.
type fakeOctaviaCloud struct {
	mu  sync.Mutex
	log []string
}

// newFakeOctaviaCloud starts a fake Octavia cloud answering from replies and
// points clouds.yaml discovery at it under the cloud name "oct", with OS_CLOUD
// unset.
func newFakeOctaviaCloud(t *testing.T, replies map[string]octaviaReply) *fakeOctaviaCloud {
	t.Helper()
	fc := &fakeOctaviaCloud{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		line := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			line += "?" + r.URL.RawQuery
		}
		fc.mu.Lock()
		fc.log = append(fc.log, line)
		fc.mu.Unlock()

		if r.Method == http.MethodPost && r.URL.Path == "/v3/auth/tokens" {
			catalog := fmt.Sprintf(`[{"type":"load-balancer","endpoints":[{"interface":"public","region":"RegionOne","url":%q}]},`+
				`{"type":"network","endpoints":[{"interface":"public","region":"RegionOne","url":%q}]}]`,
				srv.URL+"/load-balancer/", srv.URL+"/network/v2.0/")
			w.Header().Set("X-Subject-Token", "tok")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":{"expires_at":"2030-01-01T00:00:00Z","project":{"id":"proj-oct"},"catalog":%s}}`, catalog)
			return
		}
		// Octavia answers its unversioned root with 200 and a flat list of its v2
		// minor versions, the newest marked CURRENT; the load-balancer client
		// reads it once while it is built.
		if r.Method == http.MethodGet && r.URL.Path == "/load-balancer/" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"versions":[{"id":"v2.0","status":"SUPPORTED"},{"id":"v2.27","status":"CURRENT"}]}`)
			return
		}
		rep, ok := replies[r.Method+" "+r.URL.Path]
		if !ok {
			rep = octaviaReply{http.StatusNotFound, `{}`}
		}
		if rep.body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "clouds.yaml")
	data := "clouds:\n  oct:\n    auth:\n      auth_url: " + srv.URL + "/v3\n" +
		"      username: u\n      password: p\n      project_name: p\n      user_domain_name: Default\n      project_domain_name: Default\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("writing clouds.yaml: %v", err)
	}
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", path)
	return fc
}

// requests lists every request the fake received, in order, as
// "<method> <path>", with "?<query>" appended when the request carried one.
func (fc *fakeOctaviaCloud) requests() []string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return slices.Clone(fc.log)
}

// statusRecordedOctaviaRun is a completed run's created list for the status
// tests: a VIP network and subnet, a load balancer with one listener, its
// pool, a health monitor, a member of the recorded pool, a member of a pool
// the record lacks, and a member whose logical name names no pool.
func statusRecordedOctaviaRun() []resource.Resource {
	return []resource.Resource{
		{Kind: octavia.KindNetwork, Logical: "net-0001", ID: "net1"},
		{Kind: octavia.KindSubnet, Logical: "net-0001", ID: "sub1"},
		{Kind: octavia.KindLoadBalancer, Logical: "lb-0001", ID: "lb1"},
		{Kind: octavia.KindListener, Logical: "lsn-0001", ID: "lsn1"},
		{Kind: octavia.KindPool, Logical: "pool-0001", ID: "pool1"},
		{Kind: octavia.KindMember, Logical: "pool-0001-member-01", ID: "mem1"},
		{Kind: octavia.KindMember, Logical: "pool-0002-member-01", ID: "mem2"},
		{Kind: octavia.KindMember, Logical: "member-01", ID: "mem3"},
		{Kind: octavia.KindHealthMonitor, Logical: "hm-0001", ID: "hm1"},
	}
}

// TestOctaviaStatusObservesMembersThroughRecordedPool confirms octavia status
// re-queries every kind of a record with service "octavia" and reads a member
// below the id its recorded pool carries, and renders state "error" for a
// member whose pool the record lacks, without a request for it.
func TestOctaviaStatusObservesMembersThroughRecordedPool(t *testing.T) {
	const lbBase = "/load-balancer/v2.0/lbaas/"
	fc := newFakeOctaviaCloud(t, map[string]octaviaReply{
		"GET /network/v2.0/v2.0/networks/net1":       {http.StatusOK, `{"network":{"id":"net1","status":"ACTIVE"}}`},
		"GET /network/v2.0/v2.0/subnets/sub1":        {http.StatusOK, `{"subnet":{"id":"sub1"}}`},
		"GET " + lbBase + "loadbalancers/lb1":        {http.StatusOK, `{"loadbalancer":{"id":"lb1","provisioning_status":"ACTIVE"}}`},
		"GET " + lbBase + "listeners/lsn1":           {http.StatusOK, `{"listener":{"id":"lsn1","provisioning_status":"ACTIVE"}}`},
		"GET " + lbBase + "pools/pool1":              {http.StatusOK, `{"pool":{"id":"pool1","provisioning_status":"ACTIVE"}}`},
		"GET " + lbBase + "pools/pool1/members/mem1": {http.StatusOK, `{"member":{"id":"mem1","provisioning_status":"PENDING_UPDATE"}}`},
		"GET " + lbBase + "healthmonitors/hm1":       {http.StatusOK, `{"healthmonitor":{"id":"hm1","provisioning_status":"ACTIVE"}}`},
		// Were a member read below its pool's logical name instead of the
		// recorded id, mem2 would find this answer and read as ACTIVE.
		"GET " + lbBase + "pools/pool-0002/members/mem2": {http.StatusOK, `{"member":{"id":"mem2","provisioning_status":"ACTIVE"}}`},
	})
	path, err := run.Write(t.TempDir(), &run.Record{RunID: "oct00001", Service: "octavia", Created: statusRecordedOctaviaRun()})
	if err != nil {
		t.Fatalf("seeding octavia record: %v", err)
	}

	out, err := execRoot(t, "octavia", "status", "--run", path, "--os-cloud", "oct")
	if want := "re-querying 2 of 9 resources failed"; err == nil || err.Error() != want {
		t.Errorf("status = %v, want %q", err, want)
	}

	got := map[string]string{}
	for _, line := range strings.Split(out, "\n")[1:] {
		if f := strings.Fields(line); len(f) == 4 {
			got[f[0]+" "+f[1]+" "+f[2]] = f[3]
		}
	}
	want := map[string]string{
		"net-0001 network net1":           "ACTIVE",
		"net-0001 subnet sub1":            "present",
		"lb-0001 loadbalancer lb1":        "ACTIVE",
		"lsn-0001 listener lsn1":          "ACTIVE",
		"pool-0001 pool pool1":            "ACTIVE",
		"pool-0001-member-01 member mem1": "PENDING_UPDATE",
		"pool-0002-member-01 member mem2": "error",
		"member-01 member mem3":           "error",
		"hm-0001 healthmonitor hm1":       "ACTIVE",
	}
	if len(got) != len(want) {
		t.Errorf("status table has %d rows, want %d:\n%s", len(got), len(want), out)
	}
	for row, state := range want {
		if got[row] != state {
			t.Errorf("state of %q = %q, want %q:\n%s", row, got[row], state, out)
		}
	}
	for _, req := range fc.requests() {
		if strings.Contains(req, "mem2") || strings.Contains(req, "mem3") {
			t.Errorf("status sent %q for a member whose pool the record lacks", req)
		}
	}
}

// TestOctaviaObserverMember confirms octaviaObserver reads a member below its
// recorded pool's id, and returns the not-in-record error without a request
// when the record lacks the pool or the member's name names none.
func TestOctaviaObserverMember(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/v2.0/lbaas/pools/pool1/members/mem-id-1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"member":{"id":"mem-id-1","provisioning_status":"ACTIVE"}}`)
	}))
	t.Cleanup(srv.Close)
	lb := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: srv.URL + "/", ResourceBase: srv.URL + "/v2.0/"}
	network := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: srv.URL + "/"}
	obs := octaviaObserver{
		c:       octavia.New(lb, network, "run0", metrics.NewCollector()),
		poolIDs: recordedPoolIDs(statusRecordedOctaviaRun()),
	}

	status, exists, err := obs.Observe(context.Background(), resource.Resource{Kind: octavia.KindMember, Logical: "pool-0001-member-01", ID: "mem-id-1"})
	if status != "ACTIVE" || !exists || err != nil {
		t.Errorf("Observe(member of a recorded pool) = (%q, %v, %v), want (ACTIVE, true, nil)", status, exists, err)
	}
	// The record maps pool-0001 to the id pool1, so the member is read below it.
	if want := []string{"GET /v2.0/lbaas/pools/pool1/members/mem-id-1"}; !slices.Equal(requests, want) {
		t.Errorf("requests = %v, want %v", requests, want)
	}

	for _, tc := range []struct {
		logical string
		wantErr string
	}{
		{"pool-0002-member-01", `member "pool-0002-member-01": its pool "pool-0002" is not in the run record`},
		{"member-01", `member "member-01": its logical name names no pool`},
	} {
		mu.Lock()
		requests = nil
		mu.Unlock()
		_, _, err := obs.Observe(context.Background(), resource.Resource{Kind: octavia.KindMember, Logical: tc.logical, ID: "mem-x"})
		if err == nil || err.Error() != tc.wantErr {
			t.Errorf("Observe(%s) = %v, want %q", tc.logical, err, tc.wantErr)
		}
		if len(requests) != 0 {
			t.Errorf("Observe(%s) sent %v, want no request", tc.logical, requests)
		}
	}
}

// TestRecordedPoolIDsMapsPoolsOnly confirms the observer's pool lookup holds
// exactly the record's pool entries, by logical name.
func TestRecordedPoolIDsMapsPoolsOnly(t *testing.T) {
	got := recordedPoolIDs(append(statusRecordedOctaviaRun(), resource.Resource{Kind: octavia.KindPool, Logical: "pool-0003", ID: "pool3"}))
	want := map[string]string{"pool-0001": "pool1", "pool-0003": "pool3"}
	if len(got) != len(want) || got["pool-0001"] != "pool1" || got["pool-0003"] != "pool3" {
		t.Errorf("recordedPoolIDs = %v, want %v", got, want)
	}
}

// TestOctaviaCleanupByRunIDDeletesByTagInOrder confirms octavia cleanup
// --run-id discovers the run's load balancers, subnets, and networks by tag,
// deletes the load balancer with cascade and waits for it to be gone before
// deleting the subnet and then the network, and prints the count.
func TestOctaviaCleanupByRunIDDeletesByTagInOrder(t *testing.T) {
	const lbBase = "/load-balancer/v2.0/lbaas/"
	fc := newFakeOctaviaCloud(t, map[string]octaviaReply{
		"GET " + lbBase + "loadbalancers": {http.StatusOK,
			`{"loadbalancers":[{"id":"lb1","name":"dizzy-run1-lb-0001","provisioning_status":"ACTIVE","tags":["dizzy:run=run1","dizzy:type=loadbalancer"]}]}`},
		"DELETE " + lbBase + "loadbalancers/lb1": {http.StatusNoContent, ""},
		"GET " + lbBase + "loadbalancers/lb1":    {http.StatusNotFound, `{}`},
		"GET /network/v2.0/v2.0/subnets": {http.StatusOK,
			`{"subnets":[{"id":"sub1","name":"dizzy-run1-sub-0001","tags":["dizzy:run=run1","dizzy:type=subnet"]}]}`},
		"DELETE /network/v2.0/v2.0/subnets/sub1": {http.StatusNoContent, ""},
		"GET /network/v2.0/v2.0/networks": {http.StatusOK,
			`{"networks":[{"id":"net1","name":"dizzy-run1-net-0001","tags":["dizzy:run=run1","dizzy:type=network"]}]}`},
		"DELETE /network/v2.0/v2.0/networks/net1": {http.StatusNoContent, ""},
	})

	out, err := execRoot(t, "octavia", "cleanup", "--run-id", "run1", "--os-cloud", "oct")
	if err != nil {
		t.Fatalf("cleanup --run-id: %v", err)
	}
	if want := "deleted 3 resource(s) for run run1\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}

	var writes []string
	for _, req := range fc.requests() {
		if strings.HasPrefix(req, "DELETE ") || strings.HasPrefix(req, "GET "+lbBase+"loadbalancers/") {
			writes = append(writes, req)
		}
	}
	want := []string{
		"DELETE " + lbBase + "loadbalancers/lb1?cascade=true",
		"GET " + lbBase + "loadbalancers/lb1",
		"DELETE /network/v2.0/v2.0/subnets/sub1",
		"DELETE /network/v2.0/v2.0/networks/net1",
	}
	if !slices.Equal(writes, want) {
		t.Errorf("deletes and waits = %v, want %v", writes, want)
	}
}

// writeOctaviaReportRecord seeds a record with service "octavia" whose metrics
// carry a sample of every Octavia kind, a provisioning_error failure, and the
// time-to-ready of every written kind, and returns its path.
func writeOctaviaReportRecord(t *testing.T) string {
	t.Helper()
	c := metrics.NewCollector()
	for _, s := range []metrics.Sample{
		{Type: "network", Duration: 30 * time.Millisecond, Success: true},
		{Type: "subnet", Duration: 25 * time.Millisecond, Success: true},
		{Type: "loadbalancer", Duration: 40 * time.Millisecond, Success: true},
		{Type: "loadbalancer", Duration: 15 * time.Millisecond, Success: false, ErrKind: "provisioning_error"},
		{Type: "listener", Duration: 20 * time.Millisecond, Success: true},
		{Type: "pool", Duration: 20 * time.Millisecond, Success: true},
		{Type: "member", Duration: 10 * time.Millisecond, Success: true},
		{Type: "healthmonitor", Duration: 10 * time.Millisecond, Success: true},
	} {
		c.Record(s)
	}
	for _, kind := range []string{"loadbalancer", "listener", "pool", "member", "healthmonitor"} {
		c.RecordReadiness(metrics.Readiness{Type: kind, Duration: 3 * time.Second, OK: true})
	}

	start := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	rec := &run.Record{
		RunID:      "oct00001",
		Service:    "octavia",
		Scenario:   "small",
		Seed:       42,
		StartedAt:  start,
		FinishedAt: start.Add(2 * time.Second),
		Created:    statusRecordedOctaviaRun(),
		Error:      "octavia: provisioning failed: load balancer lb1 reached ERROR after a loadbalancer write",
		Metrics:    c.Aggregate(2 * time.Second),
	}
	path, err := run.Write(t.TempDir(), rec)
	if err != nil {
		t.Fatalf("seeding octavia record: %v", err)
	}
	return path
}

// octaviaReportKinds are the kinds an Octavia run records operations under.
var octaviaReportKinds = []string{"network", "subnet", "loadbalancer", "listener", "pool", "member", "healthmonitor"}

// TestOctaviaReportRendersRecordInEveryFormat confirms dizzy octavia report
// --run renders a record with service "octavia" as a table, JSON, CSV, and
// HTML, each carrying every Octavia kind and the provisioning_error kind.
func TestOctaviaReportRendersRecordInEveryFormat(t *testing.T) {
	path := writeOctaviaReportRecord(t)
	report := func(t *testing.T, format string) string {
		t.Helper()
		out, err := execRoot(t, "octavia", "report", "--run", path, "--format", format)
		if err != nil {
			t.Fatalf("octavia report --format %s: %v", format, err)
		}
		return out
	}

	t.Run("table", func(t *testing.T) {
		out := report(t, "table")
		if !strings.HasPrefix(out, "Run metrics (wall 2s)") {
			t.Errorf("report does not open with the metrics header:\n%s", out)
		}
		rows := map[string]int{}
		for _, line := range strings.Split(out, "\n") {
			if fields := strings.Fields(line); len(fields) > 0 {
				rows[fields[0]]++
			}
		}
		// A written kind appears in the operations table and in time to ready.
		for kind, n := range map[string]int{
			"overall": 1, "network": 1, "subnet": 1, "provisioning_error": 1,
			"loadbalancer": 2, "listener": 2, "pool": 2, "member": 2, "healthmonitor": 2,
		} {
			if rows[kind] != n {
				t.Errorf("table has %d rows for %q, want %d:\n%s", rows[kind], kind, n, out)
			}
		}
		if !strings.Contains(out, "Time to ready") {
			t.Errorf("table lacks the time-to-ready section:\n%s", out)
		}
	})
	t.Run("json", func(t *testing.T) {
		var agg metrics.Aggregate
		if err := json.Unmarshal([]byte(report(t, "json")), &agg); err != nil {
			t.Fatalf("report is not valid metrics JSON: %v", err)
		}
		var types, ready []string
		for _, s := range agg.ByType {
			types = append(types, s.Type)
		}
		for _, r := range agg.Readiness {
			ready = append(ready, r.Type)
		}
		for _, kind := range octaviaReportKinds {
			if !slices.Contains(types, kind) {
				t.Errorf("JSON byType %v lacks %q", types, kind)
			}
		}
		for _, kind := range octaviaReportKinds[2:] {
			if !slices.Contains(ready, kind) {
				t.Errorf("JSON readiness %v lacks %q", ready, kind)
			}
		}
		if len(agg.Errors) != 1 || agg.Errors[0].Kind != "provisioning_error" || agg.Errors[0].Count != 1 {
			t.Errorf("JSON errors = %+v, want one provisioning_error", agg.Errors)
		}
	})
	t.Run("csv", func(t *testing.T) {
		records, err := csv.NewReader(strings.NewReader(report(t, "csv"))).ReadAll()
		if err != nil {
			t.Fatalf("report is not valid CSV: %v", err)
		}
		var labels []string
		for _, r := range records[1:] {
			labels = append(labels, r[0])
		}
		for _, kind := range append([]string{"overall"}, octaviaReportKinds...) {
			if !slices.Contains(labels, kind) {
				t.Errorf("CSV rows %v lack %q", labels, kind)
			}
		}
	})
	t.Run("html", func(t *testing.T) {
		out := report(t, "html")
		if !strings.Contains(strings.ToLower(out), "<html") {
			t.Fatalf("report is not an HTML page:\n%.200s", out)
		}
		for _, want := range append([]string{"provisioning_error"}, octaviaReportKinds...) {
			if !strings.Contains(out, want) {
				t.Errorf("HTML report lacks %q", want)
			}
		}
	})
}
