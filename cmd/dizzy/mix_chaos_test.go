package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/mix"
	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	mixscenario "github.com/B42Labs/dizzy/internal/mix/scenario"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/scenarios"
)

// mixChaosScenarioYAML is sampleMixScenarioYAML with persona churn settings
// and a chaos block, used to exercise the mix chaos command's config merge.
const mixChaosScenarioYAML = sampleMixScenarioYAML + `    interval: { min: 50ms, max: 2s }
    churn_ratio: 0.4
    target_fill: 0.6
chaos:
  duration: 1m
  parallel: { max: 3 }
`

// noCloud points clouds.yaml discovery at nothing, so a command that reaches
// authentication fails there and never contacts a cloud.
func noCloud(t *testing.T) {
	t.Helper()
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")
}

func TestMixChaosRequiresScenario(t *testing.T) {
	_, err := execRoot(t, "mix", "chaos")
	if want := `required flag(s) "scenario" not set`; err == nil || err.Error() != want {
		t.Errorf("mix chaos without --scenario = %v, want %q", err, want)
	}
}

func TestMixChaosRequiresDuration(t *testing.T) {
	noCloud(t)
	path := writeScenario(t, sampleMixScenarioYAML)
	for name, sets := range map[string][]string{
		"ci alone":      nil,
		"ci and legacy": {"--set", "personas.legacy.share=1", "--set", "personas.legacy.networks=1"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := execRoot(t, append([]string{"mix", "chaos", "--scenario", path}, sets...)...)
			if want := "chaos duration must be set and positive, got 0s"; err == nil || err.Error() != want {
				t.Errorf("mix chaos without a duration = %v, want %q", err, want)
			}
		})
	}
}

// TestMixChaosRejectsBeforeCloud covers the errors mix chaos reports from the
// scenario and its overrides, before any cloud call.
func TestMixChaosRejectsBeforeCloud(t *testing.T) {
	noCloud(t)
	path := writeScenario(t, mixChaosScenarioYAML)
	tests := []struct {
		name   string
		args   []string
		want   string
		prefix bool
	}{
		{"unsupported service", []string{"--scenario", path, "--set", "services=octavia"},
			`opt-in service "octavia" is not supported by this build of dizzy (supported: none)`, false},
		{"missing file", []string{"--scenario", filepath.Join(t.TempDir(), "nope.yaml")}, "reading scenario:", true},
		{"set without value", []string{"--scenario", path, "--set", "nokey"}, `invalid --set "nokey": want key=value`, false},
		{"narrow bucket width", []string{"--scenario", path, "--duration", "0", "--bucket-width", "30s"},
			"chaos bucket-width must be at least 1m0s, got 30s", false},
		{"legacy interval with only a min", []string{"--scenario", path, "--set", "personas.legacy.share=1", "--set", "personas.legacy.networks=1",
			"--set", "personas.legacy.interval.min=1s"},
			"invalid scenario: personas.legacy.interval.min (1s) must not exceed personas.legacy.interval.max (0s)", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execRoot(t, append([]string{"mix", "chaos"}, tc.args...)...)
			switch {
			case err == nil:
				t.Fatalf("expected error %q, got nil", tc.want)
			case tc.prefix && !strings.HasPrefix(err.Error(), tc.want):
				t.Errorf("error %q does not start with %q", err, tc.want)
			case !tc.prefix && err.Error() != tc.want:
				t.Errorf("error = %q, want %q", err, tc.want)
			}
		})
	}
}

// TestMixChaosHelpListsFlags confirms mix chaos has its six flags and none of
// the per-persona knob flags, which --set covers.
func TestMixChaosHelpListsFlags(t *testing.T) {
	cmd := newMixChaosCmd(&globalOptions{})
	for _, name := range []string{"scenario", "set", "duration", "bucket-width", "max-parallel", "no-cleanup"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("mix chaos has no --%s flag", name)
		}
	}
	for _, name := range []string{"min-interval", "max-interval", "churn-ratio", "target-fill", "lifecycle-ratio"} {
		if cmd.Flags().Lookup(name) != nil {
			t.Errorf("mix chaos has a --%s flag, want none", name)
		}
	}
}

// The plan personas of the config merge tests, as the mix generator emits
// them: ci churns, and legacy is long-lived.
var (
	ciPersona     = mixplan.Persona{Name: "ci"}
	legacyPersona = mixplan.Persona{Name: "legacy", LongLived: true}
)

// mergeFor runs mergeMixChaosConfig for ps and fails the test on an error.
func mergeFor(t *testing.T, cmd *cobra.Command, opts *globalOptions, s mixscenario.Scenario, f chaosFlags, ps mixplan.Persona) chaos.Config {
	t.Helper()
	cfg, err := mergeMixChaosConfig(cmd, opts, s, f, &ps)
	if err != nil {
		t.Fatalf("mergeMixChaosConfig(%s): %v", ps.Name, err)
	}
	return cfg
}

// parseMix parses data as a mix scenario and applies sets, key=value each.
func parseMix(t *testing.T, data string, sets ...string) mixscenario.Scenario {
	t.Helper()
	s, err := mixscenario.Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, set := range sets {
		key, value, _ := strings.Cut(set, "=")
		if err := s.Set(key, value); err != nil {
			t.Fatalf("Set(%s): %v", set, err)
		}
	}
	return s
}

func TestMergeMixChaosConfig(t *testing.T) {
	t.Run("persona values", func(t *testing.T) {
		s, err := mixscenario.Parse([]byte(mixChaosScenarioYAML))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		opts := &globalOptions{concurrency: 8}
		cmd := newMixChaosCmd(opts)
		cfg := mergeFor(t, cmd, opts, s, chaosFlags{}, ciPersona)
		if cfg.MinInterval != 50*time.Millisecond || cfg.MaxInterval != 2*time.Second || cfg.ChurnRatio != 0.4 || cfg.TargetFill != 0.6 {
			t.Errorf("interval/churn/fill = %s-%s/%v/%v, want 50ms-2s/0.4/0.6", cfg.MinInterval, cfg.MaxInterval, cfg.ChurnRatio, cfg.TargetFill)
		}
		if cfg.Duration != time.Minute || cfg.MaxParallel != 3 || cfg.Concurrency != 8 || cfg.ResizeRatio != 0 || cfg.Classify == nil {
			t.Errorf("merged config = %+v, want duration 1m, max parallel 3, concurrency 8, resize ratio 0 and a classifier", cfg)
		}
	})

	t.Run("zero values fall back to the defaults", func(t *testing.T) {
		s, err := mixscenario.Parse([]byte(sampleMixScenarioYAML))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		opts := &globalOptions{concurrency: 5}
		cmd := newMixChaosCmd(opts)
		cfg := mergeFor(t, cmd, opts, s, chaosFlags{}, ciPersona)
		if cfg.MinInterval != defaultChaosMinInterval || cfg.MaxInterval != defaultChaosMaxInterval ||
			cfg.ChurnRatio != defaultChaosChurnRatio || cfg.TargetFill != defaultChaosTargetFill || cfg.MaxParallel != 5 {
			t.Errorf("merged config = %+v, want the defaults and max parallel from --concurrency", cfg)
		}
	})

	t.Run("--max-parallel overrides the block", func(t *testing.T) {
		s, err := mixscenario.Parse([]byte(mixChaosScenarioYAML))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		opts := &globalOptions{concurrency: 8}
		cmd := newMixChaosCmd(opts)
		if err := cmd.Flags().Set("max-parallel", "7"); err != nil {
			t.Fatalf("setting --max-parallel: %v", err)
		}
		if cfg := mergeFor(t, cmd, opts, s, chaosFlags{maxParallel: 7}, ciPersona); cfg.MaxParallel != 7 {
			t.Errorf("MaxParallel = %d, want 7", cfg.MaxParallel)
		}
	})

	modes := []struct {
		name          string
		block         string
		flags         map[string]string
		wantUnbounded bool
		wantDuration  time.Duration
		wantWidth     time.Duration
	}{
		{"--duration 0 over a block duration", "chaos:\n  duration: 1m\n", map[string]string{"duration": "0"}, true, 0, time.Hour},
		{"--duration 2h over a block duration", "chaos:\n  duration: 1m\n", map[string]string{"duration": "2h"}, false, 2 * time.Hour, time.Hour},
		{"block bucket width", "chaos:\n  duration: 1m\n  bucket_width: 30m\n", nil, false, time.Minute, 30 * time.Minute},
		{"--bucket-width over the block", "chaos:\n  duration: 1m\n  bucket_width: 30m\n", map[string]string{"bucket-width": "10m"}, false, time.Minute, 10 * time.Minute},
		{"no chaos block", "", nil, false, 0, time.Hour},
	}
	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			s, err := mixscenario.Parse([]byte(sampleMixScenarioYAML + tc.block))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			opts := &globalOptions{concurrency: 8}
			cmd := newMixChaosCmd(opts)
			cfg := mergeFor(t, cmd, opts, s, setChaosFlags(t, cmd, tc.flags), ciPersona)
			checkChaosMode(t, cfg, tc.wantUnbounded, tc.wantDuration, tc.wantWidth)
		})
	}

	t.Run("legacy values", func(t *testing.T) {
		s := parseMix(t, mixChaosScenarioYAML, "personas.legacy.interval.min=10s", "personas.legacy.interval.max=1m")
		opts := &globalOptions{concurrency: 8}
		cfg := mergeFor(t, newMixChaosCmd(opts), opts, s, chaosFlags{}, legacyPersona)
		if cfg.MinInterval != 10*time.Second || cfg.MaxInterval != time.Minute {
			t.Errorf("interval = %s-%s, want the legacy block's 10s-1m", cfg.MinInterval, cfg.MaxInterval)
		}
		if cfg.TargetFill != 1 || cfg.ResizeRatio != 1 || cfg.ChurnRatio != defaultChaosChurnRatio || cfg.Classify == nil {
			t.Errorf("merged config = %+v, want target fill 1, resize ratio 1, the default churn ratio and a classifier", cfg)
		}
	})

	t.Run("legacy zero bounds fall back to the defaults", func(t *testing.T) {
		for name, sets := range map[string][]string{
			"both zero":    nil,
			"only max set": {"personas.legacy.interval.max=2m"},
		} {
			t.Run(name, func(t *testing.T) {
				s := parseMix(t, sampleMixScenarioYAML, sets...)
				opts := &globalOptions{concurrency: 5}
				cfg := mergeFor(t, newMixChaosCmd(opts), opts, s, chaosFlags{}, legacyPersona)
				wantMax := defaultChaosMaxInterval
				if sets != nil {
					wantMax = 2 * time.Minute
				}
				if cfg.MinInterval != defaultChaosMinInterval || cfg.MaxInterval != wantMax {
					t.Errorf("interval = %s-%s, want %s-%s", cfg.MinInterval, cfg.MaxInterval, defaultChaosMinInterval, wantMax)
				}
			})
		}
	})

	t.Run("run-wide values are the same for both personas", func(t *testing.T) {
		s := parseMix(t, mixChaosScenarioYAML)
		opts := &globalOptions{concurrency: 8}
		cmd := newMixChaosCmd(opts)
		f := setChaosFlags(t, cmd, map[string]string{"duration": "0", "bucket-width": "10m", "max-parallel": "7"})
		f.maxParallel = 7
		type runWide struct {
			duration, bucketWidth    time.Duration
			unbounded                bool
			maxParallel, concurrency int
		}
		of := func(c chaos.Config) runWide {
			return runWide{c.Duration, c.BucketWidth, c.Unbounded, c.MaxParallel, c.Concurrency}
		}
		ci, legacy := mergeFor(t, cmd, opts, s, f, ciPersona), mergeFor(t, cmd, opts, s, f, legacyPersona)
		want := runWide{0, 10 * time.Minute, true, 7, 8}
		if of(ci) != want || of(legacy) != want {
			t.Errorf("run-wide ci = %+v, legacy = %+v, want both %+v", of(ci), of(legacy), want)
		}
		if ci.ResizeRatio != 0 {
			t.Errorf("ci resize ratio = %v, want 0", ci.ResizeRatio)
		}
	})

	t.Run("unknown persona", func(t *testing.T) {
		opts := &globalOptions{concurrency: 8}
		_, err := mergeMixChaosConfig(newMixChaosCmd(opts), opts, parseMix(t, sampleMixScenarioYAML), chaosFlags{}, &mixplan.Persona{Name: "nope"})
		if want := `persona "nope" is not defined by this build of dizzy`; err == nil || err.Error() != want {
			t.Errorf("mergeMixChaosConfig(nope) = %v, want %q", err, want)
		}
	})
}

// TestMixLaneConfigs confirms every lane runs under its own persona's config,
// in plan order: the ci lane never mutates, and the long-lived legacy lane
// keeps every planned resource and mutates on every step, also as the plan's
// only persona. Every lane checkpoints.
func TestMixLaneConfigs(t *testing.T) {
	type lane struct{ resizeRatio, targetFill float64 }
	ci, legacy := lane{0, 0.6}, lane{1, 1}
	legacySets := []string{"personas.legacy.share=1", "personas.legacy.networks=1"}
	tests := []struct {
		name string
		sets []string
		want []lane
	}{
		{"ci and legacy", legacySets, []lane{ci, legacy}},
		{"legacy alone", append([]string{"personas.ci.share=0"}, legacySets...), []lane{legacy}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := parseMix(t, mixChaosScenarioYAML, tc.sets...)
			p, err := s.Generate()
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			opts := &globalOptions{concurrency: 8}
			cfgs, err := mixLaneConfigs(newMixChaosCmd(opts), opts, s, chaosFlags{}, p)
			if err != nil {
				t.Fatalf("mixLaneConfigs: %v", err)
			}
			if len(cfgs) != len(tc.want) {
				t.Fatalf("got %d lane configs, want %d", len(cfgs), len(tc.want))
			}
			for i, want := range tc.want {
				if got := (lane{cfgs[i].ResizeRatio, cfgs[i].TargetFill}); got != want {
					t.Errorf("lane %d (%s) resize ratio and target fill = %v, want %v", i, p.Personas[i].Name, got, want)
				}
				if cfgs[i].CheckpointInterval != chaosCheckpointInterval {
					t.Errorf("lane %d checkpoint interval = %s, want %s", i, cfgs[i].CheckpointInterval, chaosCheckpointInterval)
				}
			}
		})
	}
}

// TestPlanLaneInputs confirms every persona's lane runs under the identity
// <runID>-<persona>, which mix chaos tags the resources with and mix cleanup
// --run-id finds them by, and under the cloud its scenario block names.
func TestPlanLaneInputs(t *testing.T) {
	s := parseMix(t, sampleMixScenarioYAML, "personas.ci.cloud=tenant-ci",
		"personas.legacy.share=1", "personas.legacy.networks=1", "personas.legacy.cloud=tenant-legacy")
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	overall := metrics.NewCollector()

	inputs, err := planLaneInputs(s, p, "run1234", overall)
	if err != nil {
		t.Fatalf("planLaneInputs: %v", err)
	}
	if len(inputs) != 2 {
		t.Fatalf("got %d lane inputs, want 2", len(inputs))
	}
	for i, want := range []struct{ name, runID, cloud string }{
		{"ci", "run1234-ci", "tenant-ci"},
		{"legacy", "run1234-legacy", "tenant-legacy"},
	} {
		if in := inputs[i]; in.name != want.name || in.runID != want.runID || in.cloud != want.cloud || in.persona != nil || in.overall != overall {
			t.Errorf("lane input %d = %+v, want %s under %s and %s, without a persona, recording into overall", i, in, want.name, want.runID, want.cloud)
		}
	}
}

func TestMixPersonaCloud(t *testing.T) {
	s := parseMix(t, sampleMixScenarioYAML, "personas.ci.cloud=tenant-ci", "personas.legacy.cloud=tenant-legacy")
	for name, want := range map[string]string{"ci": "tenant-ci", "legacy": "tenant-legacy"} {
		if got, err := mixPersonaCloud(s, name); err != nil || got != want {
			t.Errorf("mixPersonaCloud(%s) = %q, %v, want %q", name, got, err, want)
		}
	}
	if _, err := mixPersonaCloud(s, "nope"); err == nil || err.Error() != `persona "nope" is not defined by this build of dizzy` {
		t.Errorf("mixPersonaCloud(nope) = %v, want the not-defined error", err)
	}
}

// TestMixChaosUnknownCloud confirms a persona whose cloud clouds.yaml lacks
// fails at authentication, naming the persona, and leaves no run record.
func TestMixChaosUnknownCloud(t *testing.T) {
	cloudsFile := filepath.Join(t.TempDir(), "clouds.yaml")
	if err := os.WriteFile(cloudsFile, []byte("clouds:\n  other:\n    auth:\n      auth_url: http://127.0.0.1:1/v3\n"), 0o600); err != nil {
		t.Fatalf("writing clouds.yaml: %v", err)
	}
	for name, file := range map[string]string{"entry missing": cloudsFile, "file missing": "/nonexistent/clouds.yaml"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("OS_CLOUD", "")
			t.Setenv("OS_CLIENT_CONFIG_FILE", file)
			path := writeScenario(t, mixChaosScenarioYAML)
			t.Chdir(t.TempDir())

			_, err := execRoot(t, "mix", "chaos", "--scenario", path, "--os-cloud", "nope")
			if want := `creating compute clients for persona "ci":`; err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("mix chaos with an unknown cloud = %v, want an error starting with %q", err, want)
			}
			if records, _ := filepath.Glob("run-*.json"); len(records) != 0 {
				t.Errorf("mix chaos left run records %v although it created nothing", records)
			}
		})
	}
}

// TestMixChaosShippedProfilesRunWithoutDuration confirms each bundled mix
// profile supplies its own duration: mix chaos passes validation and fails
// only at authentication.
func TestMixChaosShippedProfilesRunWithoutDuration(t *testing.T) {
	noCloud(t)
	for _, name := range []string{"small", "medium", "large"} {
		t.Run(name, func(t *testing.T) {
			data, err := scenarios.Files.ReadFile("mix/" + name + ".yaml")
			if err != nil {
				t.Fatalf("reading shipped profile %s.yaml: %v", name, err)
			}
			_, err = execRoot(t, "mix", "chaos", "--scenario", writeScenario(t, string(data)))
			if err == nil || !strings.Contains(err.Error(), "compute clients") {
				t.Errorf("mix chaos %s = %v, want a compute-client failure", name, err)
			}
		})
	}
}

// mixTestPlan is a plan with the ci and legacy personas, for the record
// builder.
func mixTestPlan() *mixplan.Plan {
	return &mixplan.Plan{
		Scenario: "small",
		Seed:     42,
		Services: []string{},
		Personas: []mixplan.Persona{
			{Name: "ci", Share: 0.75, Servers: 3, Seed: 11},
			{Name: "legacy", Share: 0.25, Servers: 1, Seed: 12},
		},
	}
}

// mixTestLanes returns one fake lane per persona of p, recording into
// children of overall.
func mixTestLanes(p *mixplan.Plan, overall *metrics.Collector) []*mix.Lane {
	lanes := make([]*mix.Lane, 0, len(p.Personas))
	for i := range p.Personas {
		ps := &p.Personas[i]
		lanes = append(lanes, &mix.Lane{
			Name: ps.Name, RunID: "run1234-" + ps.Name, Cloud: "tenant-" + ps.Name, ProjectID: "proj-" + ps.Name,
			Persona: ps, Collector: overall.Child(),
		})
	}
	return lanes
}

func TestBuildMixRecord(t *testing.T) {
	start := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	finished := start.Add(2 * time.Minute)

	t.Run("two lanes with results", func(t *testing.T) {
		p := mixTestPlan()
		overall := metrics.NewCollector()
		lanes := mixTestLanes(p, overall)
		lanes[0].Collector.Record(metrics.Sample{Type: "server", Duration: time.Second, Success: true})
		lanes[1].Collector.Record(metrics.Sample{Type: "server", Duration: time.Second, Success: false, ErrKind: "quota"})
		results := map[string]*chaos.Result{
			"ci":     {Creates: 4, Created: []resource.Resource{{Kind: "server", Logical: "srv-0001", ID: "s1"}, {Kind: "network", Logical: "net-0001", ID: "n1"}}},
			"legacy": {Creates: 1, Created: []resource.Resource{{Kind: "server", Logical: "srv-0001", ID: "s2"}}},
		}

		rec := buildMixRecord(p, lanes, results, overall, "run1234", start, finished)
		if rec.Service != "mix" || rec.Chaos != nil || rec.RunID != "run1234" || rec.Scenario != "small" || rec.Seed != 42 {
			t.Errorf("record header = %s/%v/%s/%s/%d, want mix, no chaos, run1234, small, 42", rec.Service, rec.Chaos, rec.RunID, rec.Scenario, rec.Seed)
		}
		if rec.Metrics.Overall.Attempted != 2 || rec.Metrics.Wall != 2*time.Minute {
			t.Errorf("overall metrics = %+v, want the two samples of both lanes over 2m", rec.Metrics.Overall)
		}
		if len(rec.Personas) != 2 || rec.Personas[0].Name != "ci" || rec.Personas[1].Name != "legacy" {
			t.Fatalf("personas = %+v, want ci then legacy", rec.Personas)
		}
		ci := rec.Personas[0]
		if ci.RunID != "run1234-ci" || ci.Cloud != "tenant-ci" || ci.ProjectID != "proj-ci" || ci.Share != 0.75 || ci.Servers != 3 || ci.Seed != 11 {
			t.Errorf("ci entry = %+v, want its identity, cloud, project and plan values", ci)
		}
		if ci.Chaos == nil || ci.Chaos.Creates != 4 || ci.Metrics.Overall.Attempted != 1 {
			t.Errorf("ci chaos/metrics = %+v / %+v, want 4 creates and its own sample", ci.Chaos, ci.Metrics.Overall)
		}
		want := []string{"ci/s1", "ci/n1", "legacy/s2"}
		var got []string
		for _, r := range rec.Created {
			got = append(got, r.Persona+"/"+r.ID)
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("created = %v, want %v", got, want)
		}
		if results["ci"].Created[0].Persona != "" {
			t.Error("buildMixRecord marked the engine's own result")
		}
	})

	t.Run("a lane without a result and nothing live", func(t *testing.T) {
		p := mixTestPlan()
		overall := metrics.NewCollector()
		lanes := mixTestLanes(p, overall)
		lanes[1].Collector.Record(metrics.Sample{Type: "server", Duration: time.Second, Success: true})

		rec := buildMixRecord(p, lanes, map[string]*chaos.Result{"ci": {}}, overall, "run1234", start, finished)
		if legacy := rec.Personas[1]; legacy.Chaos != nil || legacy.Metrics.Overall.Attempted != 1 {
			t.Errorf("legacy entry = %+v, want its collector's metrics and no chaos", legacy)
		}
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !bytes.Contains(data, []byte(`"created":[]`)) {
			t.Errorf("record JSON lacks \"created\":[]: %s", data)
		}
		if bytes.Contains(data, []byte(`"services"`)) {
			t.Errorf("record JSON carries services although the plan has none: %s", data)
		}
	})
}

// TestMixCheckpointWritesIncompleteRecord confirms the checkpoint callback mix
// chaos hands mix.Run writes the record built from the snapshots, marked
// incomplete.
func TestMixCheckpointWritesIncompleteRecord(t *testing.T) {
	dir := t.TempDir()
	p := mixTestPlan()
	overall := metrics.NewCollector()
	lanes := mixTestLanes(p, overall)
	start := time.Now()
	build := func(results map[string]*chaos.Result, finished time.Time) *run.Record {
		return buildMixRecord(p, lanes, results, overall, "ckpt0003", start, finished)
	}

	chaosCheckpoint(dir, build)(map[string]*chaos.Result{"ci": {Creates: 2, Created: []resource.Resource{{Kind: "server", ID: "s1"}}}})

	rec, err := run.Load(filepath.Join(dir, "run-ckpt0003.json"))
	if err != nil {
		t.Fatalf("loading the checkpoint: %v", err)
	}
	if !rec.Incomplete || rec.Service != "mix" {
		t.Errorf("checkpoint = incomplete %v, service %q, want an incomplete mix record", rec.Incomplete, rec.Service)
	}
	if len(rec.Personas) != 2 || rec.Personas[0].Chaos == nil || rec.Personas[1].Chaos != nil {
		t.Errorf("checkpoint personas = %+v, want ci with chaos and legacy without", rec.Personas)
	}
	if len(rec.Created) != 1 || rec.Created[0].Persona != "ci" {
		t.Errorf("checkpoint created = %+v, want ci's server", rec.Created)
	}
}

// TestMixCheckpointSurvivesWriteError confirms a checkpoint that cannot be
// written logs one warning and returns, so the run goes on.
func TestMixCheckpointSurvivesWriteError(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p := mixTestPlan()
	overall := metrics.NewCollector()
	lanes := mixTestLanes(p, overall)
	build := func(results map[string]*chaos.Result, finished time.Time) *run.Record {
		return buildMixRecord(p, lanes, results, overall, "ckpt0004", finished, finished)
	}
	chaosCheckpoint(filepath.Join(t.TempDir(), "missing"), build)(map[string]*chaos.Result{})

	if got := strings.Count(logs.String(), "level=WARN"); got != 1 {
		t.Errorf("got %d warnings, want 1:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), `msg="writing run record checkpoint failed; the run continues"`) {
		t.Errorf("warning does not carry the expected message:\n%s", logs.String())
	}
}

// teardownLane is a fake lane for finishMixChurn: it logs its cleanup with the
// resources it was handed, reports leaked from its leak check, and fails with
// cleanupErr or leakErr when they are set.
type teardownLane struct {
	log        *[]string
	leaked     int
	cleanupErr error
	leakErr    error
	sawCancel  bool
}

func (f *teardownLane) lane(name string) *mix.Lane {
	return &mix.Lane{
		Name: name, RunID: "run1234-" + name,
		Cleanup: func(ctx context.Context, recorded []resource.Resource) (int, error) {
			f.sawCancel = f.sawCancel || ctx.Err() != nil
			ids := make([]string, 0, len(recorded))
			for _, r := range recorded {
				ids = append(ids, r.ID)
			}
			*f.log = append(*f.log, "cleanup "+name+" "+strings.Join(ids, ","))
			return len(recorded), f.cleanupErr
		},
		Leaked: func(ctx context.Context) (int, error) {
			f.sawCancel = f.sawCancel || ctx.Err() != nil
			*f.log = append(*f.log, "leaked "+name)
			return f.leaked, f.leakErr
		},
	}
}

// mixCreated is a mix record's created list with resources of ci and legacy.
var mixCreated = []resource.Resource{
	{Kind: "server", ID: "s1", Persona: "ci"},
	{Kind: "server", ID: "s2", Persona: "legacy"},
	{Kind: "network", ID: "n1", Persona: "ci"},
}

func TestFinishMixChurn(t *testing.T) {
	t.Run("tears every lane down then checks for leaks", func(t *testing.T) {
		var log []string
		ci, legacy := &teardownLane{log: &log}, &teardownLane{log: &log}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // a first signal cancelled the run
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)

		err := finishMixChurn(ctx, cmd, &globalOptions{}, []*mix.Lane{ci.lane("ci"), legacy.lane("legacy")}, mixCreated, "run1234", "run-run1234.json", "small.yaml", nil, true, false)
		if err != nil {
			t.Fatalf("finishMixChurn: %v", err)
		}
		if ci.sawCancel || legacy.sawCancel {
			t.Error("teardown ran on a cancelled context; it must run on context.WithoutCancel")
		}
		wantLog := []string{"cleanup ci s1,n1", "cleanup legacy s2", "leaked ci", "leaked legacy"}
		if strings.Join(log, "|") != strings.Join(wantLog, "|") {
			t.Errorf("calls = %q, want %q", log, wantLog)
		}
		wantOut := "deleted 2 resource(s) for run run1234-ci\n" +
			"deleted 1 resource(s) for run run1234-legacy\n" +
			"leak check: no run-tagged resources remain\n"
		if out.String() != wantOut {
			t.Errorf("output = %q, want %q", out.String(), wantOut)
		}
	})

	t.Run("sums the leaks of every lane", func(t *testing.T) {
		var log []string
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		lanes := []*mix.Lane{(&teardownLane{log: &log, leaked: 1}).lane("ci"), (&teardownLane{log: &log, leaked: 2}).lane("legacy")}

		if err := finishMixChurn(context.Background(), cmd, &globalOptions{}, lanes, nil, "run1234", "", "small.yaml", nil, false, false); err != nil {
			t.Fatalf("finishMixChurn: %v", err)
		}
		if !strings.HasSuffix(out.String(), "leak check: 3 run-tagged resource(s) still present after teardown\n") {
			t.Errorf("output %q lacks the summed leak line", out.String())
		}
	})

	t.Run("a failing lane does not stop the others", func(t *testing.T) {
		var log []string
		boom := errors.New("boom")
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		lanes := []*mix.Lane{(&teardownLane{log: &log, cleanupErr: boom}).lane("ci"), (&teardownLane{log: &log}).lane("legacy")}

		err := finishMixChurn(context.Background(), cmd, &globalOptions{}, lanes, mixCreated, "run1234", "", "small.yaml", nil, false, false)
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), `tearing down persona "ci" (run run1234-ci): boom`) {
			t.Fatalf("finishMixChurn = %v, want it to name the failing persona", err)
		}
		if len(log) != 2 || log[1] != "cleanup legacy s2" {
			t.Errorf("calls = %q, want the legacy lane cleaned up after ci failed", log)
		}
		if strings.Contains(out.String(), "leak check") {
			t.Errorf("output %q has a leak-check line although teardown failed", out.String())
		}
	})

	t.Run("a failing leak check names the persona", func(t *testing.T) {
		var log []string
		boom := errors.New("listing servers: 503")
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})
		lanes := []*mix.Lane{(&teardownLane{log: &log, leakErr: boom}).lane("ci")}

		err := finishMixChurn(context.Background(), cmd, &globalOptions{}, lanes, nil, "run1234", "", "small.yaml", nil, false, false)
		if !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), `leak check for persona "ci": `) {
			t.Errorf("finishMixChurn = %v, want the wrapped leak-check error", err)
		}
	})
}

func TestFinishMixChurnNoCleanup(t *testing.T) {
	tests := []struct {
		name         string
		recordPath   string
		scenarioPath string
		osCloud      string
		envCloud     string
		sets         []string
		interrupted  bool
		want         string
	}{
		{"by record", "run-run1234.json", "small.yaml", "", "", []string{"personas.ci.cloud=tenant-ci"}, false,
			"churn complete; resources left in place — reclaim with: mix cleanup --run run-run1234.json\n"},
		{"by record with the --os-cloud the run used", "run-run1234.json", "small.yaml", "soak", "other", nil, false,
			"churn complete; resources left in place — reclaim with: mix cleanup --run run-run1234.json --os-cloud 'soak'\n"},
		{"by record with the $OS_CLOUD the run fell back to", "run-run1234.json", "small.yaml", "", "soak", nil, false,
			"churn complete; resources left in place — reclaim with: mix cleanup --run run-run1234.json --os-cloud 'soak'\n"},
		{"by run id when no record was written", "", "small.yaml", "", "", nil, true,
			"churn interrupted; resources left in place — reclaim with: mix cleanup --run-id run1234 --scenario 'small.yaml'\n"},
		{"by run id with every --set and the --os-cloud the run used", "", "small.yaml", "tenant-ci", "", []string{"personas.ci.cloud=tenant-ci", "image=Ubuntu 24.04"}, false,
			"churn complete; resources left in place — reclaim with: mix cleanup --run-id run1234 --scenario 'small.yaml' " +
				"--set 'personas.ci.cloud=tenant-ci' --set 'image=Ubuntu 24.04' --os-cloud 'tenant-ci'\n"},
		{"by run id with the $OS_CLOUD the run fell back to", "", "small.yaml", "", "soak", nil, false,
			"churn complete; resources left in place — reclaim with: mix cleanup --run-id run1234 --scenario 'small.yaml' --os-cloud 'soak'\n"},
		{"by run id with shell metacharacters kept literal", "", "scenarios/$env/it's.yaml", "", "", []string{"image=$(id)`id`!"}, false,
			"churn complete; resources left in place — reclaim with: mix cleanup --run-id run1234 --scenario 'scenarios/$env/it'\\''s.yaml' " +
				"--set 'image=$(id)`id`!'\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OS_CLOUD", tc.envCloud)
			var log []string
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)
			lanes := []*mix.Lane{(&teardownLane{log: &log}).lane("ci")}

			if err := finishMixChurn(context.Background(), cmd, &globalOptions{osCloud: tc.osCloud}, lanes, mixCreated, "run1234", tc.recordPath, tc.scenarioPath, tc.sets, tc.interrupted, true); err != nil {
				t.Fatalf("finishMixChurn: %v", err)
			}
			if len(log) != 0 {
				t.Errorf("--no-cleanup called %q, want no lane touched", log)
			}
			if out.String() != tc.want {
				t.Errorf("output = %q, want %q", out.String(), tc.want)
			}
		})
	}
}

func TestWarnSharedProjects(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	warnSharedProjects([]*mix.Lane{
		{Name: "ci", ProjectID: "proj-1"},
		{Name: "legacy", ProjectID: "proj-1"},
		{Name: "gardener", ProjectID: "proj-2"},
		{Name: "a"},
		{Name: "b"},
	})

	if got := strings.Count(logs.String(), "level=WARN"); got != 1 {
		t.Fatalf("got %d warnings, want 1:\n%s", got, logs.String())
	}
	for _, want := range []string{`msg="personas share a project; each quota pre-check saw only its own plan"`, "project=proj-1", "personas=\"[ci legacy]\""} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("warning lacks %s:\n%s", want, logs.String())
		}
	}
}
