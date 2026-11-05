package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/barbican"
	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	barbicanscenario "github.com/B42Labs/dizzy/internal/barbican/scenario"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/scenarios"
)

// recordingBarbicanCleaner is a fake barbicanexec.Cleaner that records what it
// deletes and whether the contexts it is handed were cancelled. Its listings
// return nothing, so the only deletes are the recorded resources passed through
// the created list. A delete of a kind in failKinds returns deleteErr instead.
type recordingBarbicanCleaner struct {
	mu                 sync.Mutex
	recordWritten      *bool
	firstCallSawRecord bool
	calls              int
	sawCancelled       bool
	deleted            []resource.Resource
	failKinds          map[resource.Kind]bool
	deleteErr          error
}

func (c *recordingBarbicanCleaner) observe(ctx context.Context) {
	c.calls++
	if c.calls == 1 && c.recordWritten != nil {
		c.firstCallSawRecord = *c.recordWritten
	}
	if ctx.Err() != nil {
		c.sawCancelled = true
	}
}

func (c *recordingBarbicanCleaner) list(ctx context.Context) ([]resource.Resource, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observe(ctx)
	return nil, nil
}

func (c *recordingBarbicanCleaner) ListContainersByRun(ctx context.Context, _ string) ([]resource.Resource, error) {
	return c.list(ctx)
}
func (c *recordingBarbicanCleaner) ListOrdersByRun(ctx context.Context, _ string) (orders, secrets []resource.Resource, err error) {
	orders, err = c.list(ctx)
	return orders, nil, err
}
func (c *recordingBarbicanCleaner) ListSecretsByRun(ctx context.Context, _ string) ([]resource.Resource, error) {
	return c.list(ctx)
}
func (c *recordingBarbicanCleaner) Delete(ctx context.Context, r resource.Resource) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observe(ctx)
	if c.failKinds[r.Kind] {
		return c.deleteErr
	}
	c.deleted = append(c.deleted, r)
	return nil
}

// sampleBarbicanScenarioYAML is a small but complete Barbican scenario used by
// the barbican command tests. Its container fan-in stays at most 2, so the
// resources.secrets=2 override still validates.
const sampleBarbicanScenarioYAML = `
name: cli
seed: 5
resources:
  secrets:    4
  orders:     2
  containers: 2
distribution:
  payload_bytes:         { min: 16, max: 256 }
  retrieve_ratio:        0.5
  secrets_per_container: { min: 1, max: 2 }
`

// shippedBarbicanProfile copies the shipped scenarios/barbican/<name>.yaml
// profile into a temporary file and returns its path, so a test exercises the
// exact bytes the repository ships without depending on the working directory.
func shippedBarbicanProfile(t *testing.T, name string) string {
	t.Helper()
	data, err := scenarios.Files.ReadFile("barbican/" + name + ".yaml")
	if err != nil {
		t.Fatalf("reading shipped profile %s.yaml: %v", name, err)
	}
	return writeScenario(t, string(data))
}

// TestBarbicanHelpListsSubcommands confirms dizzy barbican --help lists the
// five subcommands the namespace offers.
func TestBarbicanHelpListsSubcommands(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"barbican", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("barbican --help: %v", err)
	}
	listed := map[string]bool{}
	for _, line := range strings.Split(out.String(), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && strings.HasPrefix(line, "  ") {
			listed[fields[0]] = true
		}
	}
	for _, name := range []string{"generate", "apply", "status", "report", "cleanup"} {
		if !listed[name] {
			t.Errorf("barbican --help does not list %q:\n%s", name, out.String())
		}
	}
}

// TestBarbicanGenerateSmallProfileIsDeterministic covers generating the
// shipped small profile twice: the JSON is byte-identical and enumerates 10
// secrets, 5 orders, and 3 containers.
func TestBarbicanGenerateSmallProfileIsDeterministic(t *testing.T) {
	path := shippedBarbicanProfile(t, "small")

	first, err := execRoot(t, "barbican", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate (first): %v", err)
	}
	second, err := execRoot(t, "barbican", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate (second): %v", err)
	}
	if first != second {
		t.Errorf("two generations of the small profile differ:\n%s\n---\n%s", first, second)
	}

	var p barbicanplan.Plan
	if err := json.Unmarshal([]byte(first), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if len(p.Secrets) != 10 || len(p.Orders) != 5 || len(p.Containers) != 3 {
		t.Errorf("plan has %d secrets, %d orders, %d containers; want 10, 5, 3",
			len(p.Secrets), len(p.Orders), len(p.Containers))
	}
}

func TestBarbicanGenerateStdout(t *testing.T) {
	path := writeScenario(t, sampleBarbicanScenarioYAML)

	out, err := execRoot(t, "barbican", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	var p barbicanplan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if len(p.Secrets) != 4 || len(p.Orders) != 2 || len(p.Containers) != 2 {
		t.Errorf("plan has %d secrets, %d orders, %d containers; want 4, 2, 2",
			len(p.Secrets), len(p.Orders), len(p.Containers))
	}
}

func TestBarbicanGenerateSetOverride(t *testing.T) {
	path := writeScenario(t, sampleBarbicanScenarioYAML)

	out, err := execRoot(t, "barbican", "generate", "--scenario", path, "--set", "resources.secrets=2")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var p barbicanplan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if len(p.Secrets) != 2 {
		t.Errorf("secrets = %d, want 2 after override", len(p.Secrets))
	}
}

func TestBarbicanGenerateSeedOverride(t *testing.T) {
	path := writeScenario(t, sampleBarbicanScenarioYAML)

	base, err := execRoot(t, "barbican", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate (file seed): %v", err)
	}
	overridden, err := execRoot(t, "barbican", "generate", "--scenario", path, "--seed", "999")
	if err != nil {
		t.Fatalf("generate (--seed): %v", err)
	}
	if base == overridden {
		t.Error("global --seed did not change the generated plan")
	}
	var p barbicanplan.Plan
	if err := json.Unmarshal([]byte(overridden), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if p.Seed != 999 {
		t.Errorf("plan seed = %d, want 999", p.Seed)
	}
}

func TestBarbicanGenerateErrors(t *testing.T) {
	valid := writeScenario(t, sampleBarbicanScenarioYAML)
	// A Glance scenario's keys are unknown to the Barbican schema, so strict
	// parsing must reject it.
	glanceScenario := writeScenario(t, sampleGlanceScenarioYAML)

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"missing scenario flag", []string{"barbican", "generate"}, `"scenario" not set`},
		{"nonexistent file", []string{"barbican", "generate", "--scenario", filepath.Join(t.TempDir(), "nope.yaml")}, "reading scenario"},
		{"glance scenario", []string{"barbican", "generate", "--scenario", glanceScenario}, "not found"},
		{"unknown set key", []string{"barbican", "generate", "--scenario", valid, "--set", "resources.nope=1"}, `unknown override key "resources.nope"`},
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

func TestBarbicanApplyDryRunSummaryNoAPICall(t *testing.T) {
	// Point cloud configuration at nothing: succeeding without a reachable cloud
	// proves the dry-run path makes zero auth/API calls.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := shippedBarbicanProfile(t, "small")

	out, err := execRoot(t, "barbican", "apply", "--scenario", path, "--dry-run")
	if err != nil {
		t.Fatalf("apply --dry-run: %v", err)
	}

	data, err := scenarios.Files.ReadFile("barbican/small.yaml")
	if err != nil {
		t.Fatalf("reading shipped profile: %v", err)
	}
	s, err := barbicanscenario.Parse(data)
	if err != nil {
		t.Fatalf("parsing shipped profile: %v", err)
	}
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("generating shipped profile: %v", err)
	}
	if out != p.Summary() {
		t.Errorf("dry-run output is not the plan summary:\ngot:\n%s\nwant:\n%s", out, p.Summary())
	}

	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	wantPrefixes := []string{
		`Plan for scenario "small" (seed 42)`,
		"  secrets:         10",
		"  payload total:   ",
		"  read-backs:      ",
		"  orders:          5",
		"  containers:      3 (",
	}
	if len(lines) != len(wantPrefixes) {
		t.Fatalf("summary has %d lines, want %d:\n%s", len(lines), len(wantPrefixes), out)
	}
	for i, want := range wantPrefixes {
		if !strings.HasPrefix(lines[i], want) {
			t.Errorf("summary line %d = %q, want prefix %q", i, lines[i], want)
		}
	}
}

func TestBarbicanApplyWithoutDryRunRequiresCloud(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := shippedBarbicanProfile(t, "small")
	// Run from a fresh directory so a run record written to "." would show up.
	t.Chdir(t.TempDir())

	_, err := execRoot(t, "barbican", "apply", "--scenario", path)
	if err == nil {
		t.Fatal("apply without --dry-run: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "key manager client") {
		t.Errorf("error %q does not mention key manager client creation", err.Error())
	}

	records, err := filepath.Glob("run-*")
	if err != nil {
		t.Fatalf("listing run records: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("apply that failed before any request wrote %v, want no run record", records)
	}
}

func TestBarbicanApplyKeepOnAbortFlagParses(t *testing.T) {
	// --dry-run returns before signal setup, so a successful parse proves the flag
	// is registered without needing a cloud.
	path := writeScenario(t, sampleBarbicanScenarioYAML)
	if _, err := execRoot(t, "barbican", "apply", "--scenario", path, "--dry-run", "--keep-on-abort"); err != nil {
		t.Fatalf("barbican apply --dry-run --keep-on-abort: %v", err)
	}
}

// abortedBarbicanCreated is the partial Created list of an interrupted run: a
// stored secret, an order and its generated secret, and a container, in the
// order apply records them.
func abortedBarbicanCreated() []resource.Resource {
	return []resource.Resource{
		{Kind: barbican.KindSecret, Logical: "sec-0001", ID: "s1"},
		{Kind: barbican.KindOrder, Logical: "ord-0001", ID: "o1"},
		{Kind: barbican.KindSecret, Logical: "ord-0001", ID: "g1"},
		{Kind: barbican.KindContainer, Logical: "ctr-0001", ID: "c1"},
	}
}

func TestFinishAbortedBarbicanApplyTearsDownOnLiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a first signal cancelled the run

	recordWritten := false
	c := &recordingBarbicanCleaner{recordWritten: &recordWritten}
	var out bytes.Buffer
	writeRecord := func() (string, error) {
		recordWritten = true
		return "run-abcd1234.json", nil
	}

	err := finishAbortedBarbicanApply(ctx, &out, c, "abcd1234", abortedBarbicanCreated(), false, errors.New("apply boom"), 1, time.Second, writeRecord)

	if err == nil {
		t.Fatal("finishAbortedBarbicanApply returned nil; an interrupted apply must exit non-zero")
	}
	if !strings.Contains(err.Error(), "abcd1234") {
		t.Errorf("error %q does not name the run id", err)
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("error %q does not signal the interruption", err)
	}
	if c.sawCancelled {
		t.Error("teardown ran with a cancelled context; it must run on context.WithoutCancel")
	}
	if !c.firstCallSawRecord {
		t.Error("the run record was not written before teardown began")
	}

	var gotIDs []string
	for _, r := range c.deleted {
		gotIDs = append(gotIDs, r.ID)
	}
	// Containers before orders before secrets, the stored and generated secret
	// in record order.
	if want := []string{"c1", "o1", "s1", "g1"}; strings.Join(gotIDs, ",") != strings.Join(want, ",") {
		t.Errorf("deleted ids = %v, want %v", gotIDs, want)
	}
	if want := "deleted 4 resource(s) for run abcd1234\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestFinishAbortedBarbicanApplyKeepOnAbortLeavesResources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name       string
		recordPath string
		wantHint   string
	}{
		{"with record path", "run-keep.json", "barbican cleanup --run run-keep.json"},
		{"empty record path falls back to run-id", "", "barbican cleanup --run-id keep1234"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingBarbicanCleaner{}
			var out bytes.Buffer
			writeRecord := func() (string, error) { return tc.recordPath, nil }

			err := finishAbortedBarbicanApply(ctx, &out, c, "keep1234", abortedBarbicanCreated(), true, errors.New("apply boom"), 1, time.Second, writeRecord)

			if err == nil || !strings.Contains(err.Error(), "keep1234") {
				t.Errorf("error = %v, want a non-nil error naming the run id", err)
			}
			if c.calls != 0 {
				t.Errorf("cleaner was called %d times; --keep-on-abort must not tear anything down", c.calls)
			}
			if !strings.Contains(out.String(), tc.wantHint) {
				t.Errorf("output %q missing hint %q", out.String(), tc.wantHint)
			}
		})
	}
}

// TestFinishAbortedBarbicanApplyReportsIncompleteTeardown confirms a teardown
// that fails partway returns an error carrying the count deleted so far and the
// reclaim hint, and stops before the secrets once an order delete fails.
func TestFinishAbortedBarbicanApplyReportsIncompleteTeardown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &recordingBarbicanCleaner{
		failKinds: map[resource.Kind]bool{barbican.KindOrder: true},
		deleteErr: errors.New("order delete boom"),
	}
	var out bytes.Buffer
	writeRecord := func() (string, error) { return "run-abcd1234.json", nil }

	err := finishAbortedBarbicanApply(ctx, &out, c, "abcd1234", abortedBarbicanCreated(), false, errors.New("apply boom"), 1, time.Second, writeRecord)

	if err == nil {
		t.Fatal("finishAbortedBarbicanApply returned nil for a failed teardown")
	}
	for _, want := range []string{"abcd1234", "teardown incomplete after deleting 1 resource(s)", "order delete boom", "barbican cleanup --run run-abcd1234.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
	for _, r := range c.deleted {
		if r.Kind == barbican.KindSecret {
			t.Errorf("secret %s deleted after an order delete failed", r.ID)
		}
	}
	if want := "deleted 1 resource(s) for run abcd1234\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// blockingBarbicanCleaner mimics a wedged key manager: every operation blocks
// until its context is done. The abort teardown drives it on a deadline-free
// context, so without a per-operation timeout it would block forever.
type blockingBarbicanCleaner struct{}

func (blockingBarbicanCleaner) ListContainersByRun(ctx context.Context, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingBarbicanCleaner) ListOrdersByRun(ctx context.Context, _ string) (orders, secrets []resource.Resource, err error) {
	<-ctx.Done()
	return nil, nil, ctx.Err()
}
func (blockingBarbicanCleaner) ListSecretsByRun(ctx context.Context, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingBarbicanCleaner) Delete(ctx context.Context, _ resource.Resource) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestBarbicanTimeoutCleanerBoundsWedgedOperation confirms every operation
// barbicanTimeoutCleaner bounds itself, invoked on a deadline-free parent
// context, returns promptly with the deadline error instead of blocking
// indefinitely. Secret discovery is bounded per request by the client instead;
// TestBarbicanTimeoutCleanerPassesSecretDiscoveryThrough covers it.
func TestBarbicanTimeoutCleanerBoundsWedgedOperation(t *testing.T) {
	tc := barbicanTimeoutCleaner{inner: blockingBarbicanCleaner{}, opTimeout: 10 * time.Millisecond}

	ops := map[string]func(context.Context) error{
		"ListContainersByRun": func(ctx context.Context) error { _, err := tc.ListContainersByRun(ctx, "r1"); return err },
		"ListOrdersByRun":     func(ctx context.Context) error { _, _, err := tc.ListOrdersByRun(ctx, "r1"); return err },
		"Delete": func(ctx context.Context) error {
			return tc.Delete(ctx, resource.Resource{Kind: barbican.KindSecret, ID: "s1"})
		},
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
				t.Fatalf("%s did not return; barbicanTimeoutCleaner failed to bound the wedged operation", name)
			}
		})
	}
}

// deadlineProbeBarbicanCleaner records whether the context its
// ListSecretsByRun is handed carries a deadline. Its other operations block like
// blockingBarbicanCleaner's.
type deadlineProbeBarbicanCleaner struct {
	blockingBarbicanCleaner
	sawDeadline *bool
}

func (p deadlineProbeBarbicanCleaner) ListSecretsByRun(ctx context.Context, _ string) ([]resource.Resource, error) {
	_, *p.sawDeadline = ctx.Deadline()
	return nil, nil
}

// TestBarbicanTimeoutCleanerPassesSecretDiscoveryThrough confirms
// barbicanTimeoutCleaner adds no timeout of its own to secret discovery, whose
// duration grows with the run: the client bounds its listing and each metadata
// read per request instead (barbican.Client.SetRequestTimeout).
func TestBarbicanTimeoutCleanerPassesSecretDiscoveryThrough(t *testing.T) {
	var sawDeadline bool
	tc := barbicanTimeoutCleaner{inner: deadlineProbeBarbicanCleaner{sawDeadline: &sawDeadline}, opTimeout: 10 * time.Millisecond}
	if _, err := tc.ListSecretsByRun(context.Background(), "r1"); err != nil {
		t.Fatalf("ListSecretsByRun: %v", err)
	}
	if sawDeadline {
		t.Error("ListSecretsByRun reached the inner cleaner with a deadline; secret discovery must not share one opTimeout")
	}
}

func TestBarbicanStatusRequiresRunFlag(t *testing.T) {
	if _, err := execRoot(t, "barbican", "status"); err == nil {
		t.Fatal("status without --run: expected error, got nil")
	}
}

func TestBarbicanCleanupRequiresRunOrRunID(t *testing.T) {
	if _, err := execRoot(t, "barbican", "cleanup"); err == nil {
		t.Fatal("cleanup with neither --run nor --run-id: expected error, got nil")
	}
}

// TestBarbicanRejectsGlanceRecord confirms the service guard stops a barbican
// command from operating on a glance run record, whose resource kinds the
// barbican client cannot handle.
func TestBarbicanRejectsGlanceRecord(t *testing.T) {
	dir := t.TempDir()
	glanceRec, err := run.Write(dir, &run.Record{RunID: "gla00001", Service: "glance"})
	if err != nil {
		t.Fatalf("seeding glance record: %v", err)
	}

	if _, err := execRoot(t, "barbican", "status", "--run", glanceRec); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("barbican status on a glance record: err = %v, want a service mismatch error", err)
	}
	if _, err := execRoot(t, "barbican", "cleanup", "--run", glanceRec); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("barbican cleanup on a glance record: err = %v, want a service mismatch error", err)
	}
}

// TestBarbicanReportRendersRecord confirms "barbican report --run" renders the
// metrics table of a barbican run record: a row per secret, order, and
// container kind, the mismatch error kind, and the order time-to-ready.
func TestBarbicanReportRendersRecord(t *testing.T) {
	c := metrics.NewCollector()
	for _, s := range []metrics.Sample{
		{Type: "secret", Duration: 20 * time.Millisecond, Success: true},
		{Type: "secret", Duration: 30 * time.Millisecond, Success: true},
		{Type: "secret", Duration: 10 * time.Millisecond, Success: false, ErrKind: "mismatch"},
		{Type: "order", Duration: 40 * time.Millisecond, Success: true},
		{Type: "container", Duration: 15 * time.Millisecond, Success: true},
	} {
		c.Record(s)
	}
	c.RecordReadiness(metrics.Readiness{Type: "order", Duration: 900 * time.Millisecond, OK: true})

	start := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	rec := &run.Record{
		RunID:      "bar00001",
		Service:    "barbican",
		Scenario:   "small",
		Seed:       42,
		StartedAt:  start,
		FinishedAt: start.Add(2 * time.Second),
		Created: []resource.Resource{
			{Kind: barbican.KindSecret, Logical: "sec-0001", Name: "dizzy-bar00001-sec-0001", ID: "s1"},
			{Kind: barbican.KindSecret, Logical: "sec-0002", Name: "dizzy-bar00001-sec-0002", ID: "s2"},
			{Kind: barbican.KindOrder, Logical: "ord-0001", Name: "dizzy-bar00001-ord-0001", ID: "o1"},
			{Kind: barbican.KindSecret, Logical: "ord-0001", Name: "dizzy-bar00001-ord-0001", ID: "g1"},
			{Kind: barbican.KindContainer, Logical: "ctr-0001", Name: "dizzy-bar00001-ctr-0001", ID: "c1"},
		},
		Error:   "barbican: payload mismatch",
		Metrics: c.Aggregate(2 * time.Second),
	}
	path, err := run.Write(t.TempDir(), rec)
	if err != nil {
		t.Fatalf("seeding barbican record: %v", err)
	}

	out, err := execRoot(t, "barbican", "report", "--run", path)
	if err != nil {
		t.Fatalf("barbican report: %v", err)
	}
	if !strings.HasPrefix(out, "Run metrics (wall 2s)") {
		t.Errorf("report does not open with the metrics header:\n%s", out)
	}
	rows := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			rows[fields[0]]++
		}
	}
	for _, kind := range []string{"overall", "secret", "container", "mismatch"} {
		if rows[kind] != 1 {
			t.Errorf("report has %d rows for %q, want 1:\n%s", rows[kind], kind, out)
		}
	}
	// order appears twice: once in the operations table, once in time to ready.
	if rows["order"] != 2 {
		t.Errorf("report has %d rows for \"order\", want 2 (operations and time to ready):\n%s", rows["order"], out)
	}
	if !strings.Contains(out, "Time to ready") {
		t.Errorf("report lacks the time-to-ready table:\n%s", out)
	}
}

// TestNewBarbicanClientBoundsSecretDiscovery confirms the client every barbican
// command builds bounds secret discovery per request: barbicanTimeoutCleaner
// passes ListSecretsByRun through without a timeout, so without this bound a
// wedged metadata read would hang cleanup and the abort teardown, which runs on
// a context that never expires.
func TestNewBarbicanClientBoundsSecretDiscovery(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/metadata") {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"secrets":[{"name":"dizzy-run0-sec-0001","secret_ref":"`+
			"http://"+r.Host+`/v1/secrets/s1"}],"total":1}`)
	}))
	defer ts.Close()
	defer close(release)
	kv := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: ts.URL + "/", ResourceBase: ts.URL + "/v1/"}

	client := newBarbicanClient(kv, "run0", metrics.NewCollector(), 200*time.Millisecond)
	done := make(chan error, 1)
	go func() {
		_, err := barbicanTimeoutCleaner{client, 200 * time.Millisecond}.ListSecretsByRun(context.Background(), "run0")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ListSecretsByRun = %v, want an error wrapping context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListSecretsByRun did not return within 5s; the wedged metadata read is unbounded")
	}
}
