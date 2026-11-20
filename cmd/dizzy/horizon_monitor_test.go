package main

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	horizonplan "github.com/B42Labs/dizzy/internal/horizon/plan"
	horizonscenario "github.com/B42Labs/dizzy/internal/horizon/scenario"
	"github.com/B42Labs/dizzy/internal/metrics"
)

func TestHorizonMonitorFlags(t *testing.T) {
	monitorCmd := findSubcommand(findSubcommand(newRootCmd(), "horizon"), "monitor")
	for _, name := range []string{"scenario", "set", "interval", "iterations", "error-wait", "keep-run-records", "reclaim-orphans", "horizon-url"} {
		if monitorCmd.Flags().Lookup(name) == nil {
			t.Errorf("horizon monitor lacks --%s", name)
		}
	}
}

// horizonIterationAgainst connects to f for plan p and returns the phases of
// one iteration of run iter1.
func horizonIterationAgainst(t *testing.T, f *fakeHorizonCloud, p *horizonplan.Plan, reclaim bool) iterationDeps {
	t.Helper()
	base, err := parseHorizonURL(f.horizon.URL)
	if err != nil {
		t.Fatal(err)
	}
	opts := &globalOptions{osCloud: "fake", concurrency: 4, timeout: 5 * time.Second}
	target, err := connectHorizon(t.Context(), opts, base, p)
	if err != nil {
		t.Fatalf("connectHorizon: %v", err)
	}
	client := target.client("iter1", metrics.NewCollector(), nil)
	return horizonIterationDeps(target, client, p, "iter1", opts.concurrency, opts.timeout, reclaim)
}

// smallPlan returns the small profile's plan, with that many created users
// when users is not empty.
func smallPlan(t *testing.T, users string) *horizonplan.Plan {
	t.Helper()
	data, err := os.ReadFile(horizonProfile(t, "small"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := horizonscenario.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if users != "" {
		if err := s.Set("resources.users", users); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestHorizonIterationSweepApplyCleanup asserts an iteration with
// --reclaim-orphans sweeps another Horizon run's users and projects but not
// a keystone run's, applies, and deletes its own users and projects.
func TestHorizonIterationSweepApplyCleanup(t *testing.T) {
	f := newFakeHorizonCloud(t)
	f.clouds("")
	f.seedOrphans()
	res := runIteration(t.Context(), horizonIterationAgainst(t, f, smallPlan(t, "2"), true))
	if !res.ok || res.err != nil {
		t.Fatalf("iteration = %+v", res)
	}
	if res.swept != 2 || len(res.created) != 4 || res.deleted != 4 {
		t.Errorf("swept %d, created %d, deleted %d, want 2, 4, 4", res.swept, len(res.created), res.deleted)
	}
	if got, want := f.liveNames(), []string{"dizzy-old-project-0001", "dizzy-old-user-0001"}; !slices.Equal(got, want) {
		t.Errorf("Keystone holds %v, want only the keystone run's %v", got, want)
	}
}

// TestHorizonSweepIsOptIn asserts the sweep sends nothing without
// --reclaim-orphans, and nothing for a plan without users even with it.
func TestHorizonSweepIsOptIn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		users   string
		reclaim bool
	}{
		{"without the flag", "2", false},
		{"without users", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHorizonCloud(t)
			f.clouds("")
			f.seedOrphans()
			deps := horizonIterationAgainst(t, f, smallPlan(t, tc.users), tc.reclaim)
			swept, err := deps.preflight(t.Context())
			if swept != 0 || err != nil {
				t.Errorf("preflight = %d, %v, want 0, nil", swept, err)
			}
			if n := f.calls("GET /v3/users"); n != 0 {
				t.Errorf("preflight listed users %d time(s), want none", n)
			}
			if len(f.liveNames()) != 4 {
				t.Errorf("Keystone holds %v, want the four seeded resources", f.liveNames())
			}
		})
	}
}

// TestHorizonFailedConnectFailsIterationNotLoop asserts a connect that fails
// fails its iteration, and the loop goes on to the next.
func TestHorizonFailedConnectFailsIterationNotLoop(t *testing.T) {
	var connects int
	connect := func(context.Context) (horizonTarget, error) {
		connects++
		return horizonTarget{}, errors.New("keystone down")
	}
	opts := &globalOptions{concurrency: 1, timeout: time.Second}
	runOnce := horizonMonitorRunOnce(opts, smallPlan(t, ""), nil, connect, false, false)
	iterations, failures := runMonitorLoop(t.Context(), monitorConfig{iterations: 3}, &fakeClock{now: time.Unix(0, 0)}, runOnce)
	if iterations != 3 || failures != 3 || connects != 3 {
		t.Errorf("loop ran %d iteration(s), %d failed, %d connect(s), want 3, 3, 3", iterations, failures, connects)
	}
}
