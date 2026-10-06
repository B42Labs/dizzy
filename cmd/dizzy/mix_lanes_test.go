package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/chaos/cindergraph"
	"github.com/B42Labs/dizzy/internal/chaos/glancegraph"
	"github.com/B42Labs/dizzy/internal/chaos/keystonegraph"
	"github.com/B42Labs/dizzy/internal/chaos/neutrongraph"
	keystoneexec "github.com/B42Labs/dizzy/internal/keystone/executor"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/mix"
	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	mixscenario "github.com/B42Labs/dizzy/internal/mix/scenario"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
)

// laneConfigsFor parses the mix scenario data with sets applied, loads its
// lanes and generates its plan, then builds the configs of its background
// lanes as mix chaos does: under flags set on the mix chaos command and with
// the first persona's config as the run's.
func laneConfigsFor(t *testing.T, opts *globalOptions, data string, flags map[string]string, sets ...string) ([]chaos.Config, error) {
	t.Helper()
	s := parseMix(t, data, sets...)
	ls, err := s.LoadLanes(os.ReadFile)
	if err != nil {
		t.Fatalf("LoadLanes: %v", err)
	}
	p, err := s.Generate(ls)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cmd := newMixChaosCmd(opts)
	f := setChaosFlags(t, cmd, flags)
	if f.maxParallel, err = cmd.Flags().GetInt("max-parallel"); err != nil {
		t.Fatalf("reading --max-parallel: %v", err)
	}
	cfgs, err := mixLaneConfigs(cmd, opts, s, f, p)
	if err != nil {
		t.Fatalf("mixLaneConfigs: %v", err)
	}
	return mixServiceLaneConfigs(cmd, opts, ls, f, p, cfgs[0])
}

// writeLaneScenario writes a cinder scenario of one volume and no chaos block
// and returns its path.
func writeLaneScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cinder.yaml")
	if err := os.WriteFile(path, []byte("name: lane\nresources: { volumes: 1 }\ndistribution: { volume_size_gib: { min: 1, max: 1 } }\n"), 0o600); err != nil {
		t.Fatalf("writing lane scenario: %v", err)
	}
	return path
}

func TestMixServiceLaneConfigs(t *testing.T) {
	t.Run("keystone lane on the small profile", func(t *testing.T) {
		opts := &globalOptions{concurrency: 8}
		cfgs, err := laneConfigsFor(t, opts, mixChaosScenarioYAML, map[string]string{"duration": "30m"},
			"lanes.keystone.enabled=true", "lanes.keystone.profile=small")
		if err != nil {
			t.Fatalf("mixServiceLaneConfigs: %v", err)
		}
		if len(cfgs) != 1 {
			t.Fatalf("got %d lane configs, want 1", len(cfgs))
		}
		cfg := cfgs[0]
		if cfg.MinInterval != 200*time.Millisecond || cfg.MaxInterval != 3*time.Second || cfg.MaxParallel != 4 ||
			cfg.ChurnRatio != 0.5 || cfg.TargetFill != 0.8 || cfg.ResizeRatio != 0.3 {
			t.Errorf("lane config = %+v, want the profile's 200ms-3s, max parallel 4, churn 0.5, fill 0.8 and token ratio 0.3", cfg)
		}
		if cfg.Duration != 30*time.Minute || cfg.Unbounded || cfg.Concurrency != 8 || cfg.Classify == nil || cfg.CheckpointInterval != chaosCheckpointInterval {
			t.Errorf("lane config = %+v, want the run's 30m, concurrency 8, a classifier and the checkpoint interval", cfg)
		}
	})

	t.Run("run-wide values and --max-parallel", func(t *testing.T) {
		opts := &globalOptions{concurrency: 8}
		data := sampleMixScenarioYAML + "chaos:\n  duration: 1m\n  bucket_width: 30m\n"
		cfgs, err := laneConfigsFor(t, opts, data, map[string]string{"duration": "0", "max-parallel": "2"},
			"lanes.glance.enabled=true", "lanes.glance.profile=small", "lanes.keystone.enabled=true", "lanes.keystone.profile=small")
		if err != nil {
			t.Fatalf("mixServiceLaneConfigs: %v", err)
		}
		if len(cfgs) != 2 {
			t.Fatalf("got %d lane configs, want 2", len(cfgs))
		}
		for i, cfg := range cfgs {
			if !cfg.Unbounded || cfg.Duration != 0 || cfg.BucketWidth != 30*time.Minute || cfg.MaxParallel != 2 {
				t.Errorf("lane %d config = %+v, want unbounded, the run's 30m bucket width and max parallel 2", i, cfg)
			}
		}
	})

	t.Run("a lane scenario without a chaos block gets the defaults", func(t *testing.T) {
		opts := &globalOptions{concurrency: 5}
		cfgs, err := laneConfigsFor(t, opts, mixChaosScenarioYAML, nil,
			"lanes.cinder.enabled=true", "lanes.cinder.scenario="+writeLaneScenario(t))
		if err != nil {
			t.Fatalf("mixServiceLaneConfigs: %v", err)
		}
		cfg := cfgs[0]
		if cfg.MinInterval != defaultChaosMinInterval || cfg.MaxInterval != defaultChaosMaxInterval || cfg.MaxParallel != 5 ||
			cfg.ChurnRatio != defaultChaosChurnRatio || cfg.TargetFill != defaultChaosTargetFill || cfg.Duration != time.Minute {
			t.Errorf("lane config = %+v, want the defaults, max parallel from --concurrency and the run's 1m", cfg)
		}
	})

	t.Run("every lane on its own service's merge and classifier", func(t *testing.T) {
		opts := &globalOptions{concurrency: 8}
		cfgs, err := laneConfigsFor(t, opts, mixChaosScenarioYAML, nil,
			"lanes.cinder.enabled=true", "lanes.cinder.profile=small", "lanes.glance.enabled=true", "lanes.glance.profile=small",
			"lanes.keystone.enabled=true", "lanes.keystone.profile=small", "lanes.neutron.enabled=true", "lanes.neutron.profile=small")
		if err != nil {
			t.Fatalf("mixServiceLaneConfigs: %v", err)
		}
		classifiers := []any{cindergraph.Classify, glancegraph.Classify, keystonegraph.Classify, neutrongraph.Classify}
		if len(cfgs) != len(classifiers) {
			t.Fatalf("got %d lane configs, want %d", len(cfgs), len(classifiers))
		}
		for i, cfg := range cfgs {
			if reflect.ValueOf(cfg.Classify).Pointer() != reflect.ValueOf(classifiers[i]).Pointer() {
				t.Errorf("lane %d has another service's classifier", i)
			}
		}
		// The neutron profile's chaos block, not the mix scenario's, sets the
		// neutron lane's interval and max parallel.
		if cfg := cfgs[3]; cfg.MinInterval != 200*time.Millisecond || cfg.MaxInterval != 3*time.Second || cfg.MaxParallel != 4 || cfg.Duration != time.Minute {
			t.Errorf("neutron lane config = %+v, want the profile's 200ms-3s and max parallel 4 and the run's 1m", cfg)
		}
	})

	t.Run("errors", func(t *testing.T) {
		s := parseMix(t, sampleMixScenarioYAML, "lanes.cinder.enabled=true", "lanes.cinder.scenario="+writeLaneScenario(t))
		ls, err := s.LoadLanes(os.ReadFile)
		if err != nil {
			t.Fatalf("LoadLanes: %v", err)
		}
		runCfg := chaos.Config{Duration: time.Minute, BucketWidth: time.Hour}
		cinderLane := []mixplan.Lane{{Name: "cinder"}}
		tests := []struct {
			name        string
			concurrency int
			ls          mixscenario.LaneScenarios
			lanes       []mixplan.Lane
			want        string
		}{
			{"no scenario", 8, mixscenario.LaneScenarios{}, cinderLane, `lane "cinder" has no scenario`},
			{"unknown lane", 8, ls, []mixplan.Lane{{Name: "swift"}}, `lane "swift" is not defined by this build of dizzy`},
			{"no concurrency", 0, ls, cinderLane, `lane "cinder": chaos max-parallel must be between 1 and`},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				opts := &globalOptions{concurrency: tc.concurrency}
				_, err := mixServiceLaneConfigs(newMixChaosCmd(opts), opts, tc.ls, chaosFlags{}, &mixplan.Plan{Lanes: tc.lanes}, runCfg)
				if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
					t.Errorf("mixServiceLaneConfigs = %v, want an error starting with %q", err, tc.want)
				}
			})
		}
	})

	t.Run("no lanes", func(t *testing.T) {
		opts := &globalOptions{concurrency: 8}
		cfgs, err := mixServiceLaneConfigs(newMixChaosCmd(opts), opts, mixscenario.LaneScenarios{}, chaosFlags{}, &mixplan.Plan{}, chaos.Config{})
		if err != nil || cfgs == nil || len(cfgs) != 0 {
			t.Errorf("mixServiceLaneConfigs = %v, %v, want an empty non-nil slice", cfgs, err)
		}
	})
}

func TestBuildServiceLaneUnknown(t *testing.T) {
	noCloud(t)
	l, err := buildServiceLane(context.Background(), &globalOptions{}, serviceLaneInput{name: "swift", runID: "run1234-swift", overall: metrics.NewCollector()})
	if want := `lane "swift" is not defined by this build of dizzy`; l != nil || err == nil || err.Error() != want {
		t.Errorf("buildServiceLane = %v, %v, want no lane and %q", l, err, want)
	}
}

func TestRefuseOtherProject(t *testing.T) {
	for _, tc := range []struct {
		noun, got, recorded, want string
	}{
		{"persona", "p1", "p2", `persona "x" authenticated against project p1, but the run record says it ran in project p2; authenticate with the cloud the run used`},
		{"lane", "p1", "p2", `lane "x" authenticated against project p1, but the run record says it ran in project p2; authenticate with the cloud the run used`},
		{"lane", "p1", "p1", ""},
		{"lane", "", "p2", ""},
		{"lane", "p1", "", ""},
	} {
		err := refuseOtherProject(tc.noun, "x", tc.got, tc.recorded)
		if (tc.want == "" && err != nil) || (tc.want != "" && (err == nil || err.Error() != tc.want)) {
			t.Errorf("refuseOtherProject(%s, %q, %q) = %v, want %q", tc.noun, tc.got, tc.recorded, err, tc.want)
		}
	}
}

// TestBuildServiceLaneRefusesAnotherProject confirms a cleanup-only lane stops
// when its cloud authenticates against another project than the record names.
func TestBuildServiceLaneRefusesAnotherProject(t *testing.T) {
	tenantClouds(t, fakeKeystone(t, "p1"))
	in := serviceLaneInput{name: "glance", cloud: "tenant-ci", runID: "mix00001-glance", project: "p2", overall: metrics.NewCollector()}
	l, err := buildServiceLane(context.Background(), &globalOptions{timeout: time.Second}, in)
	want := `lane "glance" authenticated against project p1, but the run record says it ran in project p2; authenticate with the cloud the run used`
	if l != nil || err == nil || err.Error() != want {
		t.Errorf("buildServiceLane = %v, %v, want no lane and %q", l, err, want)
	}
}

// TestBuildServiceLaneBindsHandles confirms every background lane binds as a
// lane of its service, under its identity, its cloud and the project that cloud
// authenticates against, with its cleanup, leak-check and observe handles, and
// sends nothing past authentication. A plan lane also carries its seed, config,
// scenario and churn graph.
func TestBuildServiceLaneBindsHandles(t *testing.T) {
	opts := &globalOptions{timeout: time.Second, concurrency: 2}
	for _, name := range []string{"cinder", "glance", "keystone", "neutron"} {
		t.Run(name, func(t *testing.T) {
			url, requests := fakeKeystoneLog(t, "p1", nil)
			tenantClouds(t, url)
			in := serviceLaneInput{name: name, cloud: "tenant-ci", runID: "mix00001-" + name, project: "p1", overall: metrics.NewCollector()}
			l, err := buildServiceLane(context.Background(), opts, in)
			if err != nil {
				t.Fatalf("buildServiceLane: %v", err)
			}
			if !l.Background() || l.Service != name || l.Name != name || l.RunID != in.runID || l.Cloud != "tenant-ci" || l.ProjectID != "p1" || l.Persona != nil {
				t.Errorf("lane = %+v, want the %s background lane under %s in project p1", l, name, in.runID)
			}
			if l.Cleanup == nil || l.Leaked == nil || l.Observe == nil {
				t.Errorf("lane handles bound: cleanup %v, leaked %v, observe %v, want all three", l.Cleanup != nil, l.Leaked != nil, l.Observe != nil)
			}
			if l.Nodes != nil || l.Provision != nil {
				t.Errorf("cleanup-only lane has nodes %v or a provision step %v, want neither", l.Nodes, l.Provision != nil)
			}
			if got := requests(); !reflect.DeepEqual(got, []string{"POST /v3/auth/tokens"}) {
				t.Errorf("requests = %q, want only authentication", got)
			}
		})
	}

	t.Run("glance plan lane", func(t *testing.T) {
		s := parseMix(t, sampleMixScenarioYAML, "lanes.glance.enabled=true", "lanes.glance.profile=small")
		ls, err := s.LoadLanes(os.ReadFile)
		if err != nil {
			t.Fatalf("LoadLanes: %v", err)
		}
		p, err := s.Generate(ls)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		url, requests := fakeKeystoneLog(t, "p1", nil)
		tenantClouds(t, url)
		in := serviceLaneInput{name: "glance", cloud: "tenant-ci", runID: "run1234-glance", overall: metrics.NewCollector(),
			lane: &p.Lanes[0], block: s.Lanes, cfg: chaos.Config{MaxParallel: 3}}
		l, err := buildServiceLane(context.Background(), opts, in)
		if err != nil {
			t.Fatalf("buildServiceLane: %v", err)
		}
		if l.Seed != p.Lanes[0].Seed || l.Config.MaxParallel != 3 || l.Scenario != "cli/glance" || len(l.Nodes) == 0 || l.Provision != nil {
			t.Errorf("lane seed %d, max parallel %d, scenario %q, %d nodes, provision %v, want the plan lane's seed %d, the config, cli/glance, a churn graph and no provision step",
				l.Seed, l.Config.MaxParallel, l.Scenario, len(l.Nodes), l.Provision != nil, p.Lanes[0].Seed)
		}
		if got := requests(); !reflect.DeepEqual(got, []string{"POST /v3/auth/tokens"}) {
			t.Errorf("requests = %q, want only authentication", got)
		}
	})
}

// TestBuildServiceLaneBlockOptions confirms the service keys of the lanes
// block reach a plan lane's pre-checks: the Cinder volume type and the Neutron
// external network are looked up by name, and the Keystone privilege
// overrides the classification of a token that carries no role. A Keystone
// lane that passes defers its scaffold and graph to its Provision step. No
// lane sends anything but reads.
func TestBuildServiceLaneBlockOptions(t *testing.T) {
	plans := parseMix(t, sampleMixScenarioYAML, "lanes.cinder.enabled=true", "lanes.cinder.profile=small",
		"lanes.neutron.enabled=true", "lanes.neutron.profile=small")
	ls, err := plans.LoadLanes(os.ReadFile)
	if err != nil {
		t.Fatalf("LoadLanes: %v", err)
	}
	p, err := plans.Generate(ls)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cinderLane, neutronLane := &p.Lanes[0], &p.Lanes[1]
	keystoneLane := &mixplan.Lane{Name: "keystone", Seed: 1, Keystone: &keystoneplan.Plan{Scenario: "small/keystone"}}
	networks := map[string]string{"GET /network/v2.0/v2.0/networks": `{"networks":[{"id":"n1","name":"public","router:external":true}]}`}

	for _, tc := range []struct {
		name   string
		lane   *mixplan.Lane
		block  mixscenario.Lanes
		bodies map[string]string
		want   string
	}{
		{"cinder default volume type", cinderLane, mixscenario.Lanes{}, nil, `lane "cinder": reading project quotas for pre-check:`},
		{"cinder named volume type", cinderLane, mixscenario.Lanes{Cinder: mixscenario.CinderLane{VolumeType: "fast"}}, nil,
			`lane "cinder": listing volume types:`},
		{"neutron first external network", neutronLane, mixscenario.Lanes{}, networks, `lane "neutron": reading project quotas for pre-check:`},
		{"neutron named external network", neutronLane, mixscenario.Lanes{Neutron: mixscenario.NeutronLane{ExternalNetwork: "ext"}}, networks,
			`lane "neutron": external network "ext" not found (or not external)`},
		{"keystone auto refuses a roleless token", keystoneLane, mixscenario.Lanes{}, nil,
			`lane "keystone": caller is neither cloud admin nor domain manager`},
		{"keystone admin override", keystoneLane, mixscenario.Lanes{Keystone: mixscenario.KeystoneLane{Privilege: "admin"}}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, requests := fakeKeystoneLog(t, "p1", tc.bodies)
			tenantClouds(t, url)
			in := serviceLaneInput{name: tc.lane.Name, cloud: "tenant-ci", runID: "run1234-" + tc.lane.Name, overall: metrics.NewCollector(),
				lane: tc.lane, block: tc.block}
			l, err := buildServiceLane(context.Background(), &globalOptions{timeout: time.Second, concurrency: 4}, in)
			switch {
			case tc.want != "" && (l != nil || err == nil || !strings.HasPrefix(err.Error(), tc.want)):
				t.Errorf("buildServiceLane = %v, %v, want no lane and an error starting with %q", l, err, tc.want)
			case tc.want == "" && err != nil:
				t.Fatalf("buildServiceLane: %v", err)
			case tc.want == "" && (l.Provision == nil || l.Nodes != nil || l.Roots != nil || l.Scenario != "small/keystone" || l.Seed != 1):
				t.Errorf("lane = %+v, want a provision step, no nodes or roots yet, the scenario small/keystone and seed 1", l)
			}
			for _, r := range requests() {
				if r != "POST /v3/auth/tokens" && !strings.HasPrefix(r, "GET ") {
					t.Errorf("request %q, want authentication and reads only", r)
				}
			}
		})
	}
}

func TestKeystoneProvision(t *testing.T) {
	roots := []resource.Resource{{Kind: "domain", ID: "d1"}, {Kind: "role", ID: "r1"}}
	nodes := []chaos.Node{{Key: "proj-0001"}}
	boom := errors.New("boom")
	bindWith := func(rs []resource.Resource, err error) func(context.Context) (keystoneexec.Bindings, []resource.Resource, error) {
		return func(context.Context) (keystoneexec.Bindings, []resource.Resource, error) {
			return keystoneexec.Bindings{}, rs, err
		}
	}

	t.Run("binds and builds", func(t *testing.T) {
		l := &mix.Lane{Name: "keystone"}
		provision := keystoneProvision(l, bindWith(roots, nil), func(keystoneexec.Bindings) ([]chaos.Node, error) { return nodes, nil })
		if err := provision(context.Background()); err != nil {
			t.Fatalf("provision: %v", err)
		}
		if !reflect.DeepEqual(l.Roots, roots) || len(l.Nodes) != 1 || l.Nodes[0].Key != "proj-0001" {
			t.Errorf("lane roots/nodes = %+v / %+v, want the bound roots and the built nodes", l.Roots, l.Nodes)
		}
	})

	t.Run("binding fails", func(t *testing.T) {
		l := &mix.Lane{Name: "keystone"}
		built := false
		provision := keystoneProvision(l, bindWith(roots[:1], boom), func(keystoneexec.Bindings) ([]chaos.Node, error) {
			built = true
			return nodes, nil
		})
		err := provision(context.Background())
		if !errors.Is(err, boom) || err.Error() != "binding scaffold: boom" {
			t.Errorf("provision = %v, want %q", err, "binding scaffold: boom")
		}
		if !reflect.DeepEqual(l.Roots, roots[:1]) || built || l.Nodes != nil {
			t.Errorf("roots = %+v, built = %v, nodes = %+v, want the partial roots kept and nothing built", l.Roots, built, l.Nodes)
		}
	})

	t.Run("building fails", func(t *testing.T) {
		l := &mix.Lane{Name: "keystone"}
		provision := keystoneProvision(l, bindWith(roots, nil), func(keystoneexec.Bindings) ([]chaos.Node, error) { return nil, boom })
		err := provision(context.Background())
		if !errors.Is(err, boom) || err.Error() != "building churn graph: boom" {
			t.Errorf("provision = %v, want %q", err, "building churn graph: boom")
		}
		if !reflect.DeepEqual(l.Roots, roots) {
			t.Errorf("roots = %+v, want the bound roots kept for teardown", l.Roots)
		}
	})
}

// provisionLane is a fake lane for provisionLanes: its Provision logs, creates
// one root and fails with provisionErr when set; its Cleanup logs the roots it
// was handed and whether its context was cancelled, and fails with cleanupErr
// when set.
type provisionLane struct {
	log                      *[]string
	provisionErr, cleanupErr error
}

func (f *provisionLane) lane(name string) *mix.Lane {
	l := &mix.Lane{Name: name, Service: name, RunID: "run1234-" + name}
	l.Provision = func(context.Context) error {
		*f.log = append(*f.log, "provision "+name)
		l.Roots = []resource.Resource{{Kind: "domain", ID: name + "-root"}}
		return f.provisionErr
	}
	l.Cleanup = func(ctx context.Context, recorded []resource.Resource) (int, error) {
		ids := make([]string, 0, len(recorded))
		for _, r := range recorded {
			ids = append(ids, r.ID)
		}
		*f.log = append(*f.log, fmt.Sprintf("cleanup %s %s cancelled=%v", name, strings.Join(ids, ","), ctx.Err() != nil))
		return len(recorded), f.cleanupErr
	}
	return l
}

func TestProvisionLanes(t *testing.T) {
	t.Run("no lane provisions", func(t *testing.T) {
		var log []string
		for name, lanes := range map[string][]*mix.Lane{
			"empty":        nil,
			"no provision": {(&teardownLane{log: &log}).lane("ci"), (&teardownLane{log: &log}).serviceLane("glance")},
		} {
			if err := provisionLanes(context.Background(), lanes); err != nil {
				t.Errorf("provisionLanes(%s) = %v, want nil", name, err)
			}
		}
		if len(log) != 0 {
			t.Errorf("calls = %q, want no cleanup", log)
		}
	})

	t.Run("the second of three lanes fails", func(t *testing.T) {
		var log []string
		boom := errors.New("boom")
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // a first signal cancelled the run
		lanes := []*mix.Lane{
			(&teardownLane{log: &log}).lane("ci"),
			(&provisionLane{log: &log}).lane("glance"),
			(&provisionLane{log: &log, provisionErr: boom}).lane("keystone"),
			(&provisionLane{log: &log}).lane("neutron"),
		}

		err := provisionLanes(ctx, lanes)
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), `provisioning lane "keystone" (run run1234-keystone): boom`) {
			t.Fatalf("provisionLanes = %v, want it to name the failing lane", err)
		}
		want := []string{"provision glance", "provision keystone",
			"cleanup glance glance-root cancelled=false", "cleanup keystone keystone-root cancelled=false"}
		if !reflect.DeepEqual(log, want) {
			t.Errorf("calls = %q, want %q", log, want)
		}
	})

	t.Run("a failing teardown is joined", func(t *testing.T) {
		var log []string
		boom, gone := errors.New("boom"), errors.New("503")
		lanes := []*mix.Lane{(&provisionLane{log: &log, provisionErr: boom, cleanupErr: gone}).lane("keystone")}

		err := provisionLanes(context.Background(), lanes)
		if !errors.Is(err, boom) || !errors.Is(err, gone) ||
			!strings.Contains(err.Error(), `tearing down lane "keystone" (run run1234-keystone): 503`) {
			t.Errorf("provisionLanes = %v, want the provisioning and the teardown failure", err)
		}
	})
}

// TestPlanServiceLaneInputs confirms every enabled background lane runs, in
// canonical order, under the identity <runID>-<lane> and the cloud its block
// names, with the scenario's lanes block, and that no lane scenario is read.
func TestPlanServiceLaneInputs(t *testing.T) {
	s := parseMix(t, sampleMixScenarioYAML,
		"lanes.cinder.enabled=true", "lanes.cinder.scenario="+filepath.Join(t.TempDir(), "gone.yaml"), "lanes.cinder.cloud=c",
		"lanes.glance.enabled=true", "lanes.glance.profile=small", "lanes.glance.cloud=g",
		"lanes.keystone.profile=small", "lanes.keystone.cloud=k",
		"lanes.neutron.enabled=true", "lanes.neutron.profile=small")
	overall := metrics.NewCollector()

	inputs := planServiceLaneInputs(s, "run1234", overall)
	want := []struct{ name, runID, cloud string }{
		{"cinder", "run1234-cinder", "c"},
		{"glance", "run1234-glance", "g"},
		{"neutron", "run1234-neutron", ""},
	}
	if len(inputs) != len(want) {
		t.Fatalf("got %d lane inputs, want %d", len(inputs), len(want))
	}
	for i, w := range want {
		in := inputs[i]
		if in.name != w.name || in.runID != w.runID || in.cloud != w.cloud || in.lane != nil || in.overall != overall || in.block != s.Lanes {
			t.Errorf("lane input %d = %+v, want %s under %s and cloud %q with the lanes block, without a plan lane", i, in, w.name, w.runID, w.cloud)
		}
	}

	none := planServiceLaneInputs(parseMix(t, sampleMixScenarioYAML), "run1234", overall)
	if none == nil || len(none) != 0 {
		t.Errorf("planServiceLaneInputs without lanes = %v, want an empty non-nil slice", none)
	}
}

func TestRecordServiceLaneInputs(t *testing.T) {
	overall := metrics.NewCollector()
	rec := &run.Record{Lanes: []run.LaneStats{{Name: "keystone", RunID: "mix00001-keystone", Cloud: "admin", ProjectID: "proj-admin"}}}

	inputs := recordServiceLaneInputs(rec, overall)
	if len(inputs) != 1 {
		t.Fatalf("got %d lane inputs, want 1", len(inputs))
	}
	if in := inputs[0]; in.name != "keystone" || in.runID != "mix00001-keystone" || in.cloud != "admin" || in.project != "proj-admin" || in.lane != nil || in.overall != overall {
		t.Errorf("lane input = %+v, want keystone under the record's identity, cloud and project, without a plan lane", in)
	}
	if got := recordServiceLaneInputs(&run.Record{}, overall); len(got) != 0 {
		t.Errorf("recordServiceLaneInputs without lanes = %+v, want none", got)
	}
}

func TestWarnLaneUnreclaimable(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	lanes := []*mix.Lane{
		{Name: "ci", RunID: "x-ci"},
		{Name: "cinder", Service: "cinder", RunID: "x-cinder"},
		{Name: "glance", Service: "glance", RunID: "x-glance"},
		{Name: "keystone", Service: "keystone", RunID: "x-keystone"},
		{Name: "neutron", Service: "neutron", RunID: "x-neutron"},
	}

	warnLaneUnreclaimable(lanes, nil)
	out := logs.String()
	if got := strings.Count(out, "level=WARN"); got != 2 {
		t.Fatalf("got %d warnings without a record, want 2:\n%s", got, out)
	}
	for _, want := range []string{
		"cleaning up by id without a run record; resources that cannot be discovered by tag", "run=x-neutron",
		"role assignments are best reclaimed with a record", "run=x-keystone",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("warnings lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "x-glance") || strings.Contains(out, "x-cinder") || strings.Contains(out, "x-ci ") {
		t.Errorf("warnings name a lane that reclaims everything by identity:\n%s", out)
	}

	logs.Reset()
	warnLaneUnreclaimable(lanes, &run.Record{})
	if logs.Len() != 0 {
		t.Errorf("warnings with a complete record:\n%s", logs.String())
	}
}
