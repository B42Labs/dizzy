package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	horizonscenario "github.com/B42Labs/dizzy/internal/horizon/scenario"
	"github.com/B42Labs/dizzy/internal/metrics"
)

// horizonChaosArgs runs horizon chaos of the small profile against f for a
// second at short intervals, with every live session browsing, plus extra.
func horizonChaosArgs(f *fakeHorizonCloud, scenario string, extra ...string) []string {
	return append([]string{
		"horizon", "chaos", "--os-cloud", "fake", "--scenario", scenario, "--horizon-url", f.horizon.URL,
		"--duration", "1s", "--min-interval", "10ms", "--max-interval", "40ms", "--browse-ratio", "1",
	}, extra...)
}

func TestHorizonChaosFlags(t *testing.T) {
	chaosCmd := findSubcommand(findSubcommand(newRootCmd(), "horizon"), "chaos")
	for _, name := range []string{"scenario", "set", "duration", "bucket-width", "min-interval", "max-interval",
		"max-parallel", "churn-ratio", "target-fill", "browse-ratio", "no-cleanup", "horizon-url"} {
		if chaosCmd.Flags().Lookup(name) == nil {
			t.Errorf("horizon chaos lacks --%s", name)
		}
	}
}

// TestMergeHorizonChaosConfigBrowseRatio asserts the browse ratio defaults,
// comes from the scenario block, including an explicit 0, and yields to the
// flag.
func TestMergeHorizonChaosConfigBrowseRatio(t *testing.T) {
	zero, half := 0.0, 0.5
	for _, tc := range []struct {
		name  string
		block *horizonscenario.Chaos
		flag  string
		want  float64
	}{
		{"default", nil, "", defaultChaosBrowseRatio},
		{"block", &horizonscenario.Chaos{BrowseRatio: &half}, "", 0.5},
		{"block zero", &horizonscenario.Chaos{BrowseRatio: &zero}, "", 0},
		{"flag", &horizonscenario.Chaos{BrowseRatio: &half}, "0.9", 0.9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newHorizonChaosCmd(&globalOptions{concurrency: 4})
			var browse float64
			if tc.flag != "" {
				if err := cmd.Flags().Set("browse-ratio", tc.flag); err != nil {
					t.Fatal(err)
				}
				browse = 0.9
			}
			cfg := mergeHorizonChaosConfig(cmd, &globalOptions{concurrency: 4}, horizonscenario.Scenario{Chaos: tc.block}, chaosFlags{}, browse)
			if cfg.ResizeRatio != tc.want {
				t.Errorf("ResizeRatio = %v, want %v", cfg.ResizeRatio, tc.want)
			}
		})
	}
}

func TestHorizonCheckError(t *testing.T) {
	if err := horizonCheckError("r1", metrics.Aggregate{Errors: []metrics.ErrorCount{{Kind: "http_500", Count: 3}}}); err != nil {
		t.Errorf("horizonCheckError without failed checks = %v, want nil", err)
	}
	err := horizonCheckError("r1", metrics.Aggregate{Errors: []metrics.ErrorCount{{Kind: "rejected", Count: 2}, {Kind: "mismatch", Count: 1}}})
	if want := "horizon checks failed (run r1): 2 login(s) rejected, 1 response(s) mismatched"; err == nil || err.Error() != want {
		t.Errorf("horizonCheckError = %v, want %q", err, want)
	}
}

// TestHorizonChaosAsCloudUser asserts a churn of the clouds.yaml user
// records a chaos block with creates, deletes and mutates, an empty created
// list, and leaves no session logged in.
func TestHorizonChaosAsCloudUser(t *testing.T) {
	scenario := horizonProfile(t, "small")
	t.Chdir(t.TempDir())
	f := newFakeHorizonCloud(t)
	f.clouds("")
	out, err := execRootWithin(t, 30*time.Second, horizonChaosArgs(f, scenario)...)
	if err != nil {
		t.Fatalf("chaos: %v\n%s", err, out)
	}
	rec := loadOnlyRecord(t)
	if rec.Service != "horizon" || len(rec.Created) != 0 || rec.Chaos == nil {
		t.Fatalf("record service %q, created %v, chaos %v", rec.Service, rec.Created, rec.Chaos)
	}
	if rec.Chaos.Creates == 0 || rec.Chaos.Deletes == 0 || rec.Chaos.Mutates == 0 {
		t.Errorf("chaos block = %+v, want creates, deletes and mutates", rec.Chaos)
	}
	logins, logouts := f.sessionCounts()
	if logins == 0 || logins != logouts {
		t.Errorf("Horizon accepted %d login(s) and %d logout(s), want every session logged out; output:\n%s", logins, logouts, out)
	}
	if strings.Contains(out, "leak check") {
		t.Errorf("a run that created nothing ran a leak check:\n%s", out)
	}
}

// TestHorizonChaosFailedChecksExitNonZero asserts a churn whose logins are
// rejected, or whose switches land in another project, exits non-zero after
// its teardown, naming the counts.
func TestHorizonChaosFailedChecksExitNonZero(t *testing.T) {
	scenario := horizonProfile(t, "small")
	for _, tc := range []struct {
		name  string
		setup func(*fakeHorizonCloud)
		want  string
	}{
		{"rejected", func(f *fakeHorizonCloud) { f.rejectLogin = true }, " login(s) rejected, 0 response(s) mismatched"},
		{"mismatch", func(f *fakeHorizonCloud) { f.wrongProject = true }, "0 login(s) rejected, "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			f := newFakeHorizonCloud(t)
			f.clouds("")
			tc.setup(f)
			out, err := execRootWithin(t, 30*time.Second, horizonChaosArgs(f, scenario)...)
			if err == nil || !strings.HasPrefix(err.Error(), "horizon checks failed (run ") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("chaos = %v, want the failed-check error containing %q", err, tc.want)
			}
			if !strings.Contains(out, "logged out ") {
				t.Errorf("the teardown did not run before the error:\n%s", out)
			}
			if rec := loadOnlyRecord(t); !strings.HasPrefix(rec.Error, "horizon checks failed") {
				t.Errorf("record error = %q, want the failed-check error", rec.Error)
			}
		})
	}
}

// TestHorizonChaosProvisionFailureRollsBack asserts a failed provisioning
// deletes the projects it created and returns the 403.
func TestHorizonChaosProvisionFailureRollsBack(t *testing.T) {
	scenario := horizonProfile(t, "small")
	t.Chdir(t.TempDir())
	f := newFakeHorizonCloud(t)
	f.clouds("")
	f.failUserCreate = http.StatusForbidden
	out, err := execRootWithin(t, 30*time.Second, horizonChaosArgs(f, scenario, "--set", "resources.users=2")...)
	if err == nil || !strings.Contains(err.Error(), "may create projects and users") {
		t.Fatalf("chaos = %v, want the forbidden hint", err)
	}
	if !strings.Contains(out, "deleted 2 resource(s) for run ") {
		t.Errorf("output lacks the rollback count:\n%s", out)
	}
	if names := f.liveNames(); len(names) != 0 {
		t.Errorf("Keystone still holds %v", names)
	}
	if n := f.horizonRequests(); n != 0 {
		t.Errorf("Horizon received %d request(s) after a failed provisioning, want none", n)
	}
}

// TestHorizonChaosWithCreatedUsers asserts chaos with resources.users=2
// provisions once, records the users and projects as created, reports no
// leak after its teardown, and with --no-cleanup prints the cleanup hint and
// leaves them.
func TestHorizonChaosWithCreatedUsers(t *testing.T) {
	scenario := horizonProfile(t, "small")
	t.Run("cleanup", func(t *testing.T) {
		t.Chdir(t.TempDir())
		f := newFakeHorizonCloud(t)
		f.clouds("")
		out, err := execRootWithin(t, 30*time.Second, horizonChaosArgs(f, scenario, "--set", "resources.users=2")...)
		if err != nil {
			t.Fatalf("chaos: %v\n%s", err, out)
		}
		if p, u := f.calls("POST /v3/projects"), f.calls("POST /v3/users"); p != 2 || u != 2 {
			t.Errorf("created %d project(s) and %d user(s), want 2 and 2: provisioned once", p, u)
		}
		rec := loadOnlyRecord(t)
		var kinds []string
		for _, r := range rec.Created {
			kinds = append(kinds, string(r.Kind))
		}
		if strings.Join(kinds, ",") != "project,project,user,user" {
			t.Errorf("created kinds = %v, want two projects and two users and no session", kinds)
		}
		if !strings.Contains(out, "deleted 4 resource(s) for run "+rec.RunID) || !strings.Contains(out, "leak check: no run-named users or projects remain") {
			t.Errorf("teardown output:\n%s", out)
		}
		if names := f.liveNames(); len(names) != 0 {
			t.Errorf("Keystone still holds %v", names)
		}
	})
	t.Run("no cleanup", func(t *testing.T) {
		t.Chdir(t.TempDir())
		f := newFakeHorizonCloud(t)
		f.clouds("")
		out, err := execRootWithin(t, 30*time.Second, horizonChaosArgs(f, scenario, "--set", "resources.users=2", "--no-cleanup")...)
		if err != nil {
			t.Fatalf("chaos: %v\n%s", err, out)
		}
		rec := loadOnlyRecord(t)
		if !strings.Contains(out, "reclaim with: horizon cleanup --run run-"+rec.RunID+".json") {
			t.Errorf("output lacks the cleanup hint:\n%s", out)
		}
		if names := f.liveNames(); len(names) != 4 {
			t.Errorf("Keystone holds %v, want the four created resources", names)
		}
	})
}
