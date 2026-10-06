package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/mix"
	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	"github.com/B42Labs/dizzy/internal/nova"
	novaexec "github.com/B42Labs/dizzy/internal/nova/executor"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/scenarios"
)

// sampleMixScenarioYAML is a small but complete mix scenario used by the mix
// command tests.
const sampleMixScenarioYAML = `
name: cli
seed: 5
image: cirros
flavor: m1.tiny
services: []
resources:
  servers: 4
personas:
  ci:
    share: 1
    networks: 2
    volumes_per_server: { min: 0, max: 1 }
    volume_gib: { min: 1, max: 2 }
`

// decodeMixPlan decodes the mix plan JSON generate wrote.
func decodeMixPlan(t *testing.T, data string) mixplan.Plan {
	t.Helper()
	var p mixplan.Plan
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		t.Fatalf("output is not valid mix plan JSON: %v", err)
	}
	return p
}

func TestMixGenerateStdout(t *testing.T) {
	path := writeScenario(t, sampleMixScenarioYAML)

	out, err := execRoot(t, "mix", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	p := decodeMixPlan(t, out)
	if len(p.Personas) != 1 || p.Personas[0].Name != "ci" || len(p.Personas[0].Nova.Servers) != 4 {
		t.Errorf("plan personas = %+v, want ci with 4 servers", p.Personas)
	}
}

func TestMixGenerateOut(t *testing.T) {
	path := writeScenario(t, sampleMixScenarioYAML)
	dest := filepath.Join(t.TempDir(), "plan.json")

	out, err := execRoot(t, "mix", "generate", "--scenario", path, "--out", dest)
	if err != nil {
		t.Fatalf("generate --out: %v", err)
	}
	if out != "" {
		t.Errorf("generate --out wrote %q to stdout, want nothing", out)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading the plan file: %v", err)
	}
	decodeMixPlan(t, string(data))
}

func TestMixGenerateOverrides(t *testing.T) {
	path := writeScenario(t, sampleMixScenarioYAML)

	base, err := execRoot(t, "mix", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	seeded, err := execRoot(t, "mix", "generate", "--scenario", path, "--seed", "7")
	if err != nil {
		t.Fatalf("generate --seed 7: %v", err)
	}
	if base == seeded {
		t.Error("--seed 7 did not change the generated plan")
	}

	out, err := execRoot(t, "mix", "generate", "--scenario", path, "--set", "resources.servers=3")
	if err != nil {
		t.Fatalf("generate --set resources.servers=3: %v", err)
	}
	if p := decodeMixPlan(t, out); len(p.Personas) != 1 || p.Personas[0].Servers != 3 || len(p.Personas[0].Nova.Servers) != 3 {
		t.Errorf("plan personas = %+v, want ci with 3 servers", p.Personas)
	}
}

func TestMixGenerateErrors(t *testing.T) {
	valid := writeScenario(t, sampleMixScenarioYAML)

	tests := []struct {
		name   string
		args   []string
		want   string
		prefix bool
	}{
		{"missing scenario flag", []string{"mix", "generate"}, `required flag(s) "scenario" not set`, false},
		{"missing file", []string{"mix", "generate", "--scenario", filepath.Join(t.TempDir(), "nope.yaml")}, "reading scenario:", true},
		{"set without value", []string{"mix", "generate", "--scenario", valid, "--set", "nokey"}, `invalid --set "nokey": want key=value`, false},
		{"unknown set key", []string{"mix", "generate", "--scenario", valid, "--set", "personas.ci.nope=1"}, `unknown override key "personas.ci.nope"`, false},
		{"unsupported service", []string{"mix", "generate", "--scenario", valid, "--set", "services=octavia"},
			`opt-in service "octavia" is not supported by this build of dizzy (supported: none)`, false},
		{"no server to divide", []string{"mix", "generate", "--scenario", valid, "--set", "resources.servers=0"},
			"generated plan failed validation: plan has no persona with at least one server", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execRoot(t, tc.args...)
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

// TestMixGenerateSmallProfile runs mix generate on the shipped small profile
// twice and checks the output is byte-identical and has the documented shape,
// and that --set personas.legacy.share=0 gives the CI persona every server.
func TestMixGenerateSmallProfile(t *testing.T) {
	data, err := scenarios.Files.ReadFile("mix/small.yaml")
	if err != nil {
		t.Fatalf("reading shipped profile: %v", err)
	}
	path := writeScenario(t, string(data))

	first, err := execRoot(t, "mix", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	second, err := execRoot(t, "mix", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if first != second {
		t.Error("two runs of mix generate on the small profile differ")
	}

	type persona struct {
		name      string
		servers   int
		share     float64
		longLived bool
	}
	personasOf := func(out string) []persona {
		var got []persona
		for _, ps := range decodeMixPlan(t, out).Personas {
			if len(ps.Nova.Servers) != ps.Servers {
				t.Errorf("persona %s plans %d servers, want its share %d", ps.Name, len(ps.Nova.Servers), ps.Servers)
			}
			got = append(got, persona{ps.Name, ps.Servers, ps.Share, ps.LongLived})
		}
		return got
	}
	if got, want := personasOf(first), []persona{{"ci", 5, 0.8, false}, {"legacy", 1, 0.2, true}}; !reflect.DeepEqual(got, want) {
		t.Errorf("personas = %+v, want %+v", got, want)
	}

	ciOnly, err := execRoot(t, "mix", "generate", "--scenario", path, "--set", "personas.legacy.share=0")
	if err != nil {
		t.Fatalf("generate --set personas.legacy.share=0: %v", err)
	}
	if got, want := personasOf(ciOnly), []persona{{"ci", 6, 1, false}}; !reflect.DeepEqual(got, want) {
		t.Errorf("personas with the legacy share 0 = %+v, want %+v", got, want)
	}
}

// writeMixRecord writes a mix record with the ci and legacy personas, and
// services when given, and returns its path.
func writeMixRecord(t *testing.T, services ...string) string {
	t.Helper()
	path, err := run.Write(t.TempDir(), &run.Record{
		RunID: "mix00001", Service: "mix", Services: services, Created: mixCreated,
		Personas: []run.PersonaStats{{Name: "ci", RunID: "mix00001-ci"}, {Name: "legacy", RunID: "mix00001-legacy"}},
	})
	if err != nil {
		t.Fatalf("writing mix record: %v", err)
	}
	return path
}

func TestMixStatusErrors(t *testing.T) {
	noCloud(t)
	novaRec := writeNovaRecord(t)
	tests := []struct {
		name   string
		args   []string
		want   string
		prefix bool
	}{
		{"missing --run", nil, `required flag(s) "run" not set`, false},
		{"missing record", []string{"--run", filepath.Join(t.TempDir(), "missing.json")}, "reading run record:", true},
		{"nova record", []string{"--run", novaRec}, `run record is for service "nova", not "mix"`, false},
		{"unsupported service", []string{"--run", writeMixRecord(t, "octavia")},
			`opt-in service "octavia" is not supported by this build of dizzy (supported: none)`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execRoot(t, append([]string{"mix", "status"}, tc.args...)...)
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

// statusLane is a fake lane for writeMixStatus whose Observe reports every
// resource ACTIVE, or fails with err when it is set.
func statusLane(name string, err error) *mix.Lane {
	return &mix.Lane{
		Name: name, RunID: "mix00001-" + name,
		Observe: func(context.Context, resource.Resource) (string, bool, error) {
			return "ACTIVE", true, err
		},
	}
}

func TestWriteMixStatus(t *testing.T) {
	rec := &run.Record{Created: []resource.Resource{
		{Kind: "server", Logical: "srv-0001", ID: "s1", Persona: "ci"},
		{Kind: "network", Logical: "net-0001", ID: "n1", Persona: "ci"},
	}}

	t.Run("one section per persona", func(t *testing.T) {
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := writeMixStatus(context.Background(), cmd, rec, []*mix.Lane{statusLane("ci", nil), statusLane("legacy", nil)}); err != nil {
			t.Fatalf("writeMixStatus: %v", err)
		}
		sections := strings.Split(out.String(), "\n\n")
		if len(sections) != 2 {
			t.Fatalf("output has %d sections, want 2:\n%s", len(sections), out.String())
		}
		ci := strings.Split(strings.TrimSpace(sections[0]), "\n")
		if len(ci) != 4 || ci[0] != "persona ci (run mix00001-ci)" || !strings.Contains(ci[2], "s1") || !strings.Contains(ci[3], "n1") {
			t.Errorf("ci section = %q, want its heading, the header and its two resources", ci)
		}
		legacy := strings.Split(strings.TrimSpace(sections[1]), "\n")
		if len(legacy) != 2 || legacy[0] != "persona legacy (run mix00001-legacy)" || !strings.HasPrefix(legacy[1], "LOGICAL") {
			t.Errorf("legacy section = %q, want its heading and an empty table", legacy)
		}
	})

	t.Run("a failing persona does not stop the others", func(t *testing.T) {
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		lanes := []*mix.Lane{statusLane("ci", errors.New("503")), statusLane("legacy", nil)}
		err := writeMixStatus(context.Background(), cmd, rec, lanes)
		if want := "re-querying 1 of 2 personas failed"; err == nil || err.Error() != want {
			t.Errorf("writeMixStatus = %v, want %q", err, want)
		}
		if !strings.Contains(out.String(), "persona legacy (run mix00001-legacy)") {
			t.Errorf("output %q lacks the persona after the failing one", out.String())
		}
	})
}

func TestMixCleanupFlagErrors(t *testing.T) {
	noCloud(t)
	scenario := writeScenario(t, sampleMixScenarioYAML)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"neither --run nor --run-id", nil, "exactly one of --run or --run-id is required"},
		{"both --run and --run-id", []string{"--run", "run-x.json", "--run-id", "x"}, "exactly one of --run or --run-id is required"},
		{"--run-id without --scenario", []string{"--run-id", "x"}, "--run-id needs --scenario to name the personas and their clouds"},
		{"--run with --scenario", []string{"--run", "run-x.json", "--scenario", scenario}, "--scenario is only used with --run-id"},
		{"nova record", []string{"--run", writeNovaRecord(t)}, `run record is for service "nova", not "mix"`},
		{"unsupported service", []string{"--run", writeMixRecord(t, "octavia")},
			`opt-in service "octavia" is not supported by this build of dizzy (supported: none)`},
		{"unsupported service by run id", []string{"--run-id", "x", "--scenario", scenario, "--set", "services=octavia"},
			`opt-in service "octavia" is not supported by this build of dizzy (supported: none)`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execRoot(t, append([]string{"mix", "cleanup"}, tc.args...)...)
			if err == nil || err.Error() != tc.want {
				t.Errorf("mix cleanup %v = %v, want %q", tc.args, err, tc.want)
			}
		})
	}
}

// TestMixCleanupReachesCloudPerPersona confirms a valid --run and a valid
// --run-id with its scenario both proceed to authenticate the first persona.
func TestMixCleanupReachesCloudPerPersona(t *testing.T) {
	noCloud(t)
	scenario := writeScenario(t, sampleMixScenarioYAML)
	for name, args := range map[string][]string{
		"by record": {"--run", writeMixRecord(t)},
		"by run id": {"--run-id", "x", "--scenario", scenario},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := execRoot(t, append([]string{"mix", "cleanup"}, args...)...)
			if want := `creating compute clients for persona "ci":`; err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("mix cleanup %v = %v, want an error starting with %q", args, err, want)
			}
		})
	}
}

// tenantClouds points clouds.yaml discovery at a file whose only entry,
// tenant-ci, authenticates at authURL, so a lane that falls back to --os-cloud
// fails parsing clouds.yaml instead.
func tenantClouds(t *testing.T, authURL string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clouds.yaml")
	data := "clouds:\n  tenant-ci:\n    auth:\n      auth_url: " + authURL + "\n" +
		"      username: u\n      password: p\n      project_name: p\n      user_domain_name: Default\n      project_domain_name: Default\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("writing clouds.yaml: %v", err)
	}
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", path)
}

// fakeKeystone serves a v3 token scoped to projectID whose catalog points every
// service of the compute stack at the server itself, and returns its auth URL.
// The endpoints carry their major version, so the client skips discovery.
func fakeKeystone(t *testing.T, projectID string) string {
	t.Helper()
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	mux.HandleFunc("POST /v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
		var catalog []string
		for _, svc := range [][2]string{{"compute", "v2.1"}, {"network", "v2.0"}, {"block-storage", "v3"}, {"image", "v2"}} {
			catalog = append(catalog, fmt.Sprintf(`{"type":%q,"endpoints":[{"interface":"public","region":"RegionOne","url":%q}]}`,
				svc[0], ts.URL+"/"+svc[0]+"/"+svc[1]+"/"))
		}
		w.Header().Set("X-Subject-Token", "tok")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":{"expires_at":"2030-01-01T00:00:00Z","project":{"id":%q},"catalog":[%s]}}`, projectID, strings.Join(catalog, ","))
	})
	return ts.URL + "/v3"
}

// writeTenantMixRecord writes a mix record whose ci persona ran under the
// tenant-ci cloud in project, and returns its path.
func writeTenantMixRecord(t *testing.T, project string) string {
	t.Helper()
	path, err := run.Write(t.TempDir(), &run.Record{
		RunID: "mix00001", Service: "mix", Created: mixCreated[:1],
		Personas: []run.PersonaStats{{Name: "ci", RunID: "mix00001-ci", Cloud: "tenant-ci", ProjectID: project}},
	})
	if err != nil {
		t.Fatalf("writing mix record: %v", err)
	}
	return path
}

// TestMixPersonaCloudOverridesOSCloud confirms every mix command authenticates
// a persona with the cloud its scenario block or record names, not with
// --os-cloud: tenant-ci fails connecting, where nope would fail parsing.
func TestMixPersonaCloudOverridesOSCloud(t *testing.T) {
	tenantClouds(t, "http://127.0.0.1:1/v3")
	scenario := writeScenario(t, mixChaosScenarioYAML)
	record := writeTenantMixRecord(t, "proj-ci")
	for name, args := range map[string][]string{
		"chaos":             {"chaos", "--scenario", scenario, "--set", "personas.ci.cloud=tenant-ci"},
		"cleanup by run id": {"cleanup", "--run-id", "x", "--scenario", scenario, "--set", "personas.ci.cloud=tenant-ci"},
		"cleanup by record": {"cleanup", "--run", record},
		"status":            {"status", "--run", record},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := execRoot(t, append(append([]string{"mix"}, args...), "--os-cloud", "nope")...)
			if want := `creating compute clients for persona "ci": creating provider client:`; err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("mix %s = %v, want an error starting with %q from the tenant-ci cloud", name, err, want)
			}
		})
	}
}

// TestMixRecordRefusesAnotherProject confirms mix status and mix cleanup stop
// before touching a persona whose cloud now authenticates against another
// project than the record names, where discovery would find nothing.
func TestMixRecordRefusesAnotherProject(t *testing.T) {
	tenantClouds(t, fakeKeystone(t, "proj-other"))
	record := writeTenantMixRecord(t, "proj-ci")
	for _, sub := range []string{"status", "cleanup"} {
		t.Run(sub, func(t *testing.T) {
			_, err := execRoot(t, "mix", sub, "--run", record)
			want := `persona "ci" authenticated against project proj-other, but the run record says it ran in project proj-ci; authenticate with the cloud the run used`
			if err == nil || err.Error() != want {
				t.Errorf("mix %s = %v, want %q", sub, err, want)
			}
		})
	}
}

func TestRecordLaneInputs(t *testing.T) {
	overall := metrics.NewCollector()
	rec := &run.Record{Personas: []run.PersonaStats{{Name: "ci", RunID: "mix00001-ci", Cloud: "tenant-ci", ProjectID: "proj-ci"}}}

	inputs := recordLaneInputs(rec, overall)
	if len(inputs) != 1 {
		t.Fatalf("got %d lane inputs, want 1", len(inputs))
	}
	if in := inputs[0]; in.name != "ci" || in.runID != "mix00001-ci" || in.cloud != "tenant-ci" || in.project != "proj-ci" || in.persona != nil || in.overall != overall {
		t.Errorf("lane input = %+v, want ci under the record's identity, cloud and project, without a persona", in)
	}
}

// writeNovaRecord writes a nova run record and returns its path.
func writeNovaRecord(t *testing.T) string {
	t.Helper()
	path, err := run.Write(t.TempDir(), &run.Record{RunID: "nova0001", Service: "nova"})
	if err != nil {
		t.Fatalf("writing nova record: %v", err)
	}
	return path
}

func TestDeleteLaneResources(t *testing.T) {
	var log []string
	boom := errors.New("boom")
	lanes := []*mix.Lane{(&teardownLane{log: &log, cleanupErr: boom}).lane("ci"), (&teardownLane{log: &log}).lane("legacy")}
	var out bytes.Buffer

	err := deleteLaneResources(context.Background(), &out, lanes, mixCreated, "cleaning up")
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), `cleaning up persona "ci" (run run1234-ci): boom`) {
		t.Fatalf("deleteLaneResources = %v, want it to name the failing persona", err)
	}
	if want := []string{"cleanup ci s1,n1", "cleanup legacy s2"}; strings.Join(log, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q, want %q", log, want)
	}
	wantOut := "deleted 2 resource(s) for run run1234-ci\ndeleted 1 resource(s) for run run1234-legacy\n"
	if out.String() != wantOut {
		t.Errorf("output = %q, want %q", out.String(), wantOut)
	}
}

// TestNovaCleanupRejectsMixRecord confirms the service guard keeps nova cleanup
// off a mix record.
func TestNovaCleanupRejectsMixRecord(t *testing.T) {
	_, err := execRoot(t, "nova", "cleanup", "--run", writeMixRecord(t))
	if want := `run record is for service "mix", not "nova"`; err == nil || err.Error() != want {
		t.Errorf("nova cleanup on a mix record = %v, want %q", err, want)
	}
}

// TestBuildPersonaNodes confirms a long-lived persona's lane gets the pinned
// long-lived graph, any other persona the graph nova chaos churns, and an
// invalid compute plan the builder's error.
func TestBuildPersonaNodes(t *testing.T) {
	plan := func() *novaplan.Plan {
		return &novaplan.Plan{
			Networks: []novaplan.Network{{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"}},
			Servers:  []novaplan.Server{{Name: "srv-0001", Networks: []string{"net-0001"}, StopStart: novaplan.StopStartSoft, ColdMigrate: true}},
			Volumes:  []novaplan.Volume{{Name: "vol-0001", SizeGiB: 1, Server: "srv-0001", Detach: true}},
		}
	}
	var c *nova.Client // the builders only capture the client in their closures
	for _, longLived := range []bool{false, true} {
		t.Run(fmt.Sprintf("long-lived %v", longLived), func(t *testing.T) {
			ps := &mixplan.Persona{Name: "x", LongLived: longLived, Nova: plan()}
			nodes, err := buildPersonaNodes(ps, c, novaexec.Resolved{}, time.Minute)
			if err != nil {
				t.Fatalf("buildPersonaNodes: %v", err)
			}
			if len(nodes) != 3 {
				t.Fatalf("built %d nodes, want 3", len(nodes))
			}
			for _, n := range nodes {
				if n.Pinned != longLived {
					t.Errorf("node %q pinned = %v, want %v", n.Key, n.Pinned, longLived)
				}
			}
		})

		t.Run(fmt.Sprintf("long-lived %v, invalid plan", longLived), func(t *testing.T) {
			p := plan()
			p.Servers[0].Networks = []string{"ghost"}
			_, err := buildPersonaNodes(&mixplan.Persona{Name: "x", LongLived: longLived, Nova: p}, c, novaexec.Resolved{}, time.Minute)
			if err == nil || !strings.HasPrefix(err.Error(), "invalid plan:") {
				t.Errorf("buildPersonaNodes = %v, want an error starting with %q", err, "invalid plan:")
			}
		})
	}
}
