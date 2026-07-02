package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/openstack-tester/internal/neutron"
)

// fakeClock is a virtual clock for the loop tests: Sleep advances time instantly
// and records the requested delay so the pacing is deterministic and
// inspectable; advance simulates the wall-clock an iteration consumes.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func TestMonitorLoopPacesIterations(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	cfg := monitorConfig{interval: 10 * time.Minute, iterations: 2}
	runOnce := func(ctx context.Context, iter int) bool {
		clk.advance(2 * time.Minute) // each iteration is shorter than the interval
		return true
	}

	iterations, failures := runMonitorLoop(context.Background(), cfg, clk, runOnce)

	if iterations != 2 || failures != 0 {
		t.Fatalf("iterations=%d failures=%d, want 2/0", iterations, failures)
	}
	// One sleep, of interval minus the 2m the iteration took; no trailing sleep.
	if len(clk.sleeps) != 1 || clk.sleeps[0] != 8*time.Minute {
		t.Errorf("sleeps = %v, want [8m0s]", clk.sleeps)
	}
}

func TestMonitorLoopStartsImmediatelyAfterOverrun(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	cfg := monitorConfig{interval: 10 * time.Minute, iterations: 2}
	runOnce := func(ctx context.Context, iter int) bool {
		clk.advance(15 * time.Minute) // overruns the interval
		return true
	}

	runMonitorLoop(context.Background(), cfg, clk, runOnce)

	// No backlog: an overrunning iteration is followed by a zero delay, not a
	// negative one that would let the schedule run backwards.
	if len(clk.sleeps) != 1 || clk.sleeps[0] != 0 {
		t.Errorf("sleeps = %v, want [0s] (start the next immediately, no backlog)", clk.sleeps)
	}
}

func TestMonitorLoopContinuousRunsBackToBack(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	cfg := monitorConfig{interval: 0, iterations: 3} // continuous: no pacing gap
	runOnce := func(ctx context.Context, iter int) bool {
		clk.advance(2 * time.Minute) // however long an iteration takes,
		return true                  // the next starts the moment it finishes
	}

	iterations, failures := runMonitorLoop(context.Background(), cfg, clk, runOnce)

	if iterations != 3 || failures != 0 {
		t.Fatalf("iterations=%d failures=%d, want 3/0", iterations, failures)
	}
	// Two inter-iteration sleeps, both exactly zero: back-to-back, no wait.
	if len(clk.sleeps) != 2 || clk.sleeps[0] != 0 || clk.sleeps[1] != 0 {
		t.Errorf("sleeps = %v, want [0s 0s] (iterations run back-to-back)", clk.sleeps)
	}
}

func TestMonitorLoopAppliesErrorWait(t *testing.T) {
	tests := []struct {
		name      string
		interval  time.Duration
		errorWait time.Duration
		iterDur   time.Duration
		wantSleep time.Duration
	}{
		{"error wait exceeds remaining", 10 * time.Minute, 20 * time.Minute, 2 * time.Minute, 20 * time.Minute},
		{"remaining exceeds error wait", 30 * time.Minute, 5 * time.Minute, 2 * time.Minute, 28 * time.Minute},
		{"continuous mode still waits after a failure", 0, 5 * time.Minute, 2 * time.Minute, 5 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk := &fakeClock{now: time.Unix(0, 0)}
			cfg := monitorConfig{interval: tc.interval, errorWait: tc.errorWait, iterations: 2}
			runOnce := func(ctx context.Context, iter int) bool {
				clk.advance(tc.iterDur)
				return false // a failed iteration triggers the error wait
			}

			_, failures := runMonitorLoop(context.Background(), cfg, clk, runOnce)

			if failures != 2 {
				t.Errorf("failures = %d, want 2", failures)
			}
			// A failed iteration waits max(remaining, errorWait) before the next.
			if len(clk.sleeps) != 1 || clk.sleeps[0] != tc.wantSleep {
				t.Errorf("sleeps = %v, want [%s]", clk.sleeps, tc.wantSleep)
			}
		})
	}
}

func TestMonitorLoopFloorsFastFailureBackoff(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	cfg := monitorConfig{interval: 0, iterations: 2} // continuous, no error-wait
	runOnce := func(ctx context.Context, iter int) bool {
		clk.advance(time.Millisecond) // a dependency (e.g. Keystone) fails fast
		return false
	}

	_, failures := runMonitorLoop(context.Background(), cfg, clk, runOnce)

	if failures != 2 {
		t.Fatalf("failures = %d, want 2", failures)
	}
	// With interval and error-wait both 0, a fast-failing iteration would yield a
	// zero delay and hot-loop; the minimum failure backoff floors it at 5s so the
	// loop backs off instead of self-DoSing the unhealthy dependency.
	if len(clk.sleeps) != 1 || clk.sleeps[0] != 5*time.Second {
		t.Errorf("sleeps = %v, want [5s] (fast failure floored to the minimum backoff)", clk.sleeps)
	}
}

func TestMonitorLoopStopsAfterIterations(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	cfg := monitorConfig{interval: time.Minute, iterations: 3}
	var calls int
	runOnce := func(ctx context.Context, iter int) bool {
		calls++
		clk.advance(time.Second)
		return true
	}

	iterations, _ := runMonitorLoop(context.Background(), cfg, clk, runOnce)

	if iterations != 3 || calls != 3 {
		t.Fatalf("iterations=%d calls=%d, want 3/3", iterations, calls)
	}
	// Two sleeps between three iterations; no trailing sleep after the last.
	if len(clk.sleeps) != 2 {
		t.Errorf("sleeps = %v, want two inter-iteration sleeps and no trailing one", clk.sleeps)
	}
}

func TestMonitorLoopStopsOnCancel(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	cfg := monitorConfig{interval: time.Minute} // 0 iterations = run forever
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	runOnce := func(ctx context.Context, iter int) bool {
		calls++
		clk.advance(time.Second)
		if calls == 3 {
			cancel() // an operator interrupts after the third iteration
		}
		return false // even a run of failing iterations must still exit on cancel
	}

	iterations, failures := runMonitorLoop(ctx, cfg, clk, runOnce)

	if iterations != 3 {
		t.Errorf("iterations = %d, want 3 (loop survives failures but stops promptly on cancel)", iterations)
	}
	if failures != 3 {
		t.Errorf("failures = %d, want 3", failures)
	}
}

func TestMonitorLoopContinuousStopsOnCancel(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	cfg := monitorConfig{interval: 0} // continuous and unbounded (run forever)
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	runOnce := func(ctx context.Context, iter int) bool {
		calls++
		clk.advance(time.Second)
		if calls == 3 {
			cancel() // an operator interrupts the third back-to-back iteration
		}
		return true
	}

	iterations, _ := runMonitorLoop(ctx, cfg, clk, runOnce)

	// Back-to-back iterations still end on cancel rather than spinning: the
	// clock's Sleep observes the cancelled context even with a zero delay.
	if iterations != 3 {
		t.Errorf("iterations = %d, want 3 (continuous loop stops promptly on cancel)", iterations)
	}
}

func TestRunIterationSkipsApplyWhenPreflightFails(t *testing.T) {
	var applied, cleaned bool
	res := runIteration(context.Background(), iterationDeps{
		preflight: func(ctx context.Context) (int, error) {
			return 0, errors.New("sweep boom")
		},
		apply: func(ctx context.Context) ([]neutron.Resource, error) {
			applied = true
			return nil, nil
		},
		cleanup: func(ctx context.Context, created []neutron.Resource) (int, error) {
			cleaned = true
			return 0, nil
		},
	})

	if applied {
		t.Error("apply ran despite a pre-flight failure; a half-swept project must not be filled again")
	}
	if !cleaned {
		t.Error("cleanup did not run after a pre-flight failure")
	}
	if res.ok {
		t.Error("iteration reported ok despite a pre-flight failure")
	}
	if res.err == nil || !strings.Contains(res.err.Error(), "pre-flight") {
		t.Errorf("res.err = %v, want a pre-flight error", res.err)
	}
}

func TestRunIterationCleansUpAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a first signal cancelled the run before/during the iteration

	var cleanupCtxErr error
	var cleanupCreated []neutron.Resource
	created := []neutron.Resource{{Kind: neutron.KindNetwork, ID: "n1"}}
	res := runIteration(ctx, iterationDeps{
		preflight: func(ctx context.Context) (int, error) { return 1, nil },
		apply:     func(ctx context.Context) ([]neutron.Resource, error) { return created, nil },
		cleanup: func(ctx context.Context, c []neutron.Resource) (int, error) {
			cleanupCtxErr = ctx.Err()
			cleanupCreated = c
			return len(c), nil
		},
	})

	// The heart of the clean-exit criterion: teardown runs on a live context even
	// though the parent was cancelled, with whatever apply managed to create.
	if cleanupCtxErr != nil {
		t.Errorf("cleanup ran with a cancelled context (%v); it must survive a first-signal cancel", cleanupCtxErr)
	}
	if len(cleanupCreated) != 1 {
		t.Errorf("cleanup got %d created resources, want the 1 apply produced", len(cleanupCreated))
	}
	if res.ok {
		t.Error("iteration reported ok despite the context being cancelled")
	}
}

func TestRunIterationReportsFailureWhenCleanupFails(t *testing.T) {
	res := runIteration(context.Background(), iterationDeps{
		preflight: func(ctx context.Context) (int, error) { return 0, nil },
		apply:     func(ctx context.Context) ([]neutron.Resource, error) { return nil, nil },
		cleanup: func(ctx context.Context, created []neutron.Resource) (int, error) {
			return 0, errors.New("teardown boom")
		},
	})

	if res.ok {
		t.Error("iteration reported ok despite a cleanup failure")
	}
	if res.err == nil || !strings.Contains(res.err.Error(), "cleanup") {
		t.Errorf("res.err = %v, want a cleanup error", res.err)
	}
}

// blockingCleaner mimics a wedged Neutron: every operation blocks until its
// context is done. Cleanup drives it on the monitor's deadline-free context, so
// without a per-operation timeout it would block forever.
type blockingCleaner struct{}

func (blockingCleaner) ListByTag(ctx context.Context, _ neutron.Kind, _ string) ([]neutron.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingCleaner) DetachRouterInterfaces(ctx context.Context, _ string) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
func (blockingCleaner) DeleteNetworkPorts(ctx context.Context, _ string) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
func (blockingCleaner) Delete(ctx context.Context, _ neutron.Resource) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestTimeoutCleanerBoundsWedgedOperation is the regression guard for the
// monitor loop hanging forever on a wedged cleanup call: a blocking operation
// invoked through timeoutCleaner on a deadline-free parent context must return
// promptly with the deadline error instead of blocking indefinitely.
func TestTimeoutCleanerBoundsWedgedOperation(t *testing.T) {
	tc := timeoutCleaner{inner: blockingCleaner{}, opTimeout: 10 * time.Millisecond}

	done := make(chan error, 1)
	go func() {
		done <- tc.Delete(context.Background(), neutron.Resource{Kind: neutron.KindNetwork, ID: "n1"})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Delete err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Delete did not return; timeoutCleaner failed to bound the wedged operation")
	}
}

func TestMonitorConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     monitorConfig
		wantErr bool
	}{
		{"valid", monitorConfig{interval: time.Minute}, false},
		{"zero interval (continuous)", monitorConfig{interval: 0}, false},
		{"negative interval", monitorConfig{interval: -time.Minute}, true},
		{"negative iterations", monitorConfig{interval: time.Minute, iterations: -1}, true},
		{"negative error wait", monitorConfig{interval: time.Minute, errorWait: -time.Second}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.validate(); (err != nil) != tc.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestMonitorRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "neutron", "monitor", "--interval", "1m"); err == nil {
		t.Error("monitor without --scenario: expected an error, got nil")
	}
}

func TestMonitorRejectsNegativeInterval(t *testing.T) {
	path := writeScenario(t, sampleScenarioYAML)
	_, err := execRoot(t, "neutron", "monitor", "--scenario", path, "--interval=-1m")
	if err == nil {
		t.Fatal("monitor with a negative --interval: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "--interval") {
		t.Errorf("error = %q, want it to mention --interval", err.Error())
	}
}

func TestMonitorWithValidConfigRequiresCloud(t *testing.T) {
	// Point clouds.yaml resolution at a nonexistent file so auth fails
	// deterministically, proving config validation and telemetry setup precede
	// authentication — a missing, zero, or positive interval all validate, yet
	// the command still fails only at client creation.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleScenarioYAML)
	tests := []struct {
		name string
		args []string
	}{
		{"paced", []string{"neutron", "monitor", "--scenario", path, "--iterations", "1", "--interval", "1m"}},
		{"continuous by default", []string{"neutron", "monitor", "--scenario", path, "--iterations", "1"}},
		{"explicit zero interval", []string{"neutron", "monitor", "--scenario", path, "--iterations", "1", "--interval", "0"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execRoot(t, tc.args...)
			if err == nil {
				t.Fatal("monitor with a valid config but no reachable cloud: expected an error, got nil")
			}
			if !strings.Contains(err.Error(), "network client") {
				t.Errorf("error = %q, want it to mention the network client (auth) step", err.Error())
			}
		})
	}
}
