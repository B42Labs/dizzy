package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
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
		{"cold migration not a boolean", []string{"mix", "generate", "--scenario", valid, "--set", "personas.legacy.cold_migration=maybe"},
			`override personas.legacy.cold_migration: "maybe" is not a boolean`, false},
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
// and that --set personas.gardener.share=0 divides the servers between the CI
// and the Legacy persona.
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
		name               string
		servers            int
		share              float64
		longLived, rolling bool
	}
	personasOf := func(out string) []persona {
		var got []persona
		for _, ps := range decodeMixPlan(t, out).Personas {
			if len(ps.Nova.Servers) != ps.Servers {
				t.Errorf("persona %s plans %d servers, want its share %d", ps.Name, len(ps.Nova.Servers), ps.Servers)
			}
			got = append(got, persona{ps.Name, ps.Servers, ps.Share, ps.LongLived, ps.Rolling})
		}
		return got
	}
	want := []persona{{"ci", 3, 0.5, false, false}, {"gardener", 2, 0.3, false, true}, {"legacy", 1, 0.2, true, false}}
	if got := personasOf(first); !reflect.DeepEqual(got, want) {
		t.Errorf("personas = %+v, want %+v", got, want)
	}

	noGardener, err := execRoot(t, "mix", "generate", "--scenario", path, "--set", "personas.gardener.share=0")
	if err != nil {
		t.Fatalf("generate --set personas.gardener.share=0: %v", err)
	}
	var split []string
	for _, ps := range personasOf(noGardener) {
		split = append(split, fmt.Sprintf("%s %d", ps.name, ps.servers))
	}
	if want := []string{"ci 4", "legacy 2"}; !reflect.DeepEqual(split, want) {
		t.Errorf("personas with the gardener share 0 = %v, want %v", split, want)
	}
}

// TestMixGenerateColdMigrationOff confirms --set
// personas.legacy.cold_migration=false on the shipped small profile plans no
// cold migration, keeps the live migration of every Legacy server, and leaves
// the personas and their server counts as they are without the override.
func TestMixGenerateColdMigrationOff(t *testing.T) {
	data, err := scenarios.Files.ReadFile("mix/small.yaml")
	if err != nil {
		t.Fatalf("reading shipped profile: %v", err)
	}
	path := writeScenario(t, string(data))

	def, err := execRoot(t, "mix", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	off, err := execRoot(t, "mix", "generate", "--scenario", path, "--set", "personas.legacy.cold_migration=false")
	if err != nil {
		t.Fatalf("generate --set personas.legacy.cold_migration=false: %v", err)
	}
	if !strings.Contains(def, `"coldMigrate"`) {
		t.Fatal(`the small profile plans no "coldMigrate" without the override`)
	}
	if strings.Contains(off, `"coldMigrate"`) {
		t.Error(`plan with the override contains "coldMigrate"`)
	}

	personasOf := func(out string) []string {
		var got []string
		for _, ps := range decodeMixPlan(t, out).Personas {
			got = append(got, fmt.Sprintf("%s %d", ps.Name, ps.Servers))
			if ps.Name != "legacy" {
				continue
			}
			if len(ps.Nova.Servers) == 0 {
				t.Error("the legacy persona plans no server")
			}
			for _, srv := range ps.Nova.Servers {
				if !srv.LiveMigrate {
					t.Errorf("legacy server %s does not live-migrate", srv.Name)
				}
			}
		}
		return got
	}
	if got, want := personasOf(off), personasOf(def); !reflect.DeepEqual(got, want) {
		t.Errorf("personas with the override = %v, want %v", got, want)
	}
}

// TestMixGenerateLane confirms switching on a lane of the shipped small
// profile with --set adds it to the plan, under the lane's scenario name.
func TestMixGenerateLane(t *testing.T) {
	data, err := scenarios.Files.ReadFile("mix/small.yaml")
	if err != nil {
		t.Fatalf("reading shipped profile: %v", err)
	}
	out, err := execRoot(t, "mix", "generate", "--scenario", writeScenario(t, string(data)), "--set", "lanes.neutron.enabled=true")
	if err != nil {
		t.Fatalf("generate --set lanes.neutron.enabled=true: %v", err)
	}
	p := decodeMixPlan(t, out)
	if len(p.Lanes) != 1 || p.Lanes[0].Name != "neutron" || p.Lanes[0].Neutron == nil || p.Lanes[0].Neutron.Scenario != "small/neutron" {
		t.Errorf("plan lanes = %+v, want neutron alone with the scenario small/neutron", p.Lanes)
	}

	without, err := execRoot(t, "mix", "generate", "--scenario", writeScenario(t, string(data)))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if strings.Contains(without, `"lanes"`) {
		t.Errorf("plan of the small profile as shipped has a lanes key:\n%s", without)
	}
}

// TestMixGenerateRejectsGardenerPolicy confirms a Gardener policy other than
// the two anti-affinity policies fails the small profile before any API call.
func TestMixGenerateRejectsGardenerPolicy(t *testing.T) {
	noCloud(t)
	data, err := scenarios.Files.ReadFile("mix/small.yaml")
	if err != nil {
		t.Fatalf("reading shipped profile: %v", err)
	}
	_, err = execRoot(t, "mix", "generate", "--scenario", writeScenario(t, string(data)), "--set", "personas.gardener.policy=affinity")
	if want := `invalid scenario: personas.gardener.policy must be "anti-affinity" or "soft-anti-affinity", got "affinity"`; err == nil || err.Error() != want {
		t.Errorf("mix generate = %v, want %q", err, want)
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

// TestWriteMixStatusServiceLane confirms a background lane gets the lane
// heading and only the resources its lane created, and that a failing lane
// counts among the personas and lanes.
func TestWriteMixStatusServiceLane(t *testing.T) {
	rec := &run.Record{Created: []resource.Resource{
		{Kind: "server", Logical: "srv-0001", ID: "s1", Persona: "ci"},
		{Kind: "project", Logical: "proj-0001", ID: "p1", Lane: "keystone"},
	}}
	serviceStatusLane := func(name string, err error) *mix.Lane {
		l := statusLane(name, err)
		l.Service = name
		return l
	}

	t.Run("one section per lane", func(t *testing.T) {
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		lanes := []*mix.Lane{statusLane("ci", nil), serviceStatusLane("keystone", nil), serviceStatusLane("glance", nil)}
		if err := writeMixStatus(context.Background(), cmd, rec, lanes); err != nil {
			t.Fatalf("writeMixStatus: %v", err)
		}
		sections := strings.Split(out.String(), "\n\n")
		if len(sections) != 3 {
			t.Fatalf("output has %d sections, want 3:\n%s", len(sections), out.String())
		}
		keystone := strings.Split(strings.TrimSpace(sections[1]), "\n")
		if len(keystone) != 3 || keystone[0] != "lane keystone (run mix00001-keystone)" || !strings.Contains(keystone[2], "p1") {
			t.Errorf("keystone section = %q, want its lane heading, the header and its one resource", keystone)
		}
		glance := strings.Split(strings.TrimSpace(sections[2]), "\n")
		if len(glance) != 2 || glance[0] != "lane glance (run mix00001-glance)" || !strings.HasPrefix(glance[1], "LOGICAL") {
			t.Errorf("glance section = %q, want its lane heading and an empty table", glance)
		}
	})

	t.Run("a failing lane", func(t *testing.T) {
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})
		lanes := []*mix.Lane{statusLane("ci", nil), serviceStatusLane("keystone", errors.New("503"))}
		err := writeMixStatus(context.Background(), cmd, rec, lanes)
		if want := "re-querying 1 of 2 personas and lanes failed"; err == nil || err.Error() != want {
			t.Errorf("writeMixStatus = %v, want %q", err, want)
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
// service of the compute stack and the identity service at the server itself,
// and returns its auth URL. The endpoints carry their major version, so the
// client skips discovery. The token carries no role.
func fakeKeystone(t *testing.T, projectID string) string {
	t.Helper()
	url, _ := fakeKeystoneLog(t, projectID, nil)
	return url
}

// fakeKeystoneLog is fakeKeystone that also returns a function listing the
// method and path of every request the server received, in order. A request
// other than the token request is answered with the JSON body bodies holds for
// its "<method> <path>", or else with 404.
func fakeKeystoneLog(t *testing.T, projectID string, bodies map[string]string) (string, func() []string) {
	t.Helper()
	var (
		mu       sync.Mutex
		requests []string
	)
	mux := http.NewServeMux()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	mux.HandleFunc("POST /v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
		var catalog []string
		for _, svc := range [][2]string{{"compute", "v2.1"}, {"network", "v2.0"}, {"block-storage", "v3"}, {"image", "v2"}, {"identity", "v3"}} {
			catalog = append(catalog, fmt.Sprintf(`{"type":%q,"endpoints":[{"interface":"public","region":"RegionOne","url":%q}]}`,
				svc[0], ts.URL+"/"+svc[0]+"/"+svc[1]+"/"))
		}
		w.Header().Set("X-Subject-Token", "tok")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":{"expires_at":"2030-01-01T00:00:00Z","project":{"id":%q},"catalog":[%s]}}`, projectID, strings.Join(catalog, ","))
	})
	for pattern, body := range bodies {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		})
	}
	return ts.URL + "/v3", func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}
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

// TestMixLaneCloudFromRecord confirms mix status and mix cleanup build a
// record's background lanes, each with the cloud the record names for it,
// also for a record without a persona.
func TestMixLaneCloudFromRecord(t *testing.T) {
	tenantClouds(t, "http://127.0.0.1:1/v3")
	path, err := run.Write(t.TempDir(), &run.Record{
		RunID: "mix00001", Service: "mix", Created: []resource.Resource{{Kind: "project", ID: "p1", Lane: "keystone"}},
		Lanes: []run.LaneStats{{Name: "keystone", RunID: "mix00001-keystone", Cloud: "nope", Scenario: "small/keystone"}},
	})
	if err != nil {
		t.Fatalf("writing mix record: %v", err)
	}
	for _, sub := range []string{"status", "cleanup"} {
		t.Run(sub, func(t *testing.T) {
			_, err := execRoot(t, "mix", sub, "--run", path)
			if want := `creating identity client for lane "keystone":`; err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("mix %s = %v, want an error containing %q", sub, err, want)
			}
		})
	}
}

// TestMixCleanupByRunIDBuildsLanes confirms mix cleanup --run-id builds the
// background lanes the scenario enables after its personas, each with the
// cloud its block names, also a lane whose scenario file is gone since the
// run: cleanup needs only the lane's name and cloud.
func TestMixCleanupByRunIDBuildsLanes(t *testing.T) {
	tenantClouds(t, fakeKeystone(t, "proj-ci"))
	scenario := writeScenario(t, sampleMixScenarioYAML)
	for name, lane := range map[string]string{
		"on a profile":                    "lanes.glance.profile=small",
		"on a scenario file that is gone": "lanes.glance.scenario=" + filepath.Join(t.TempDir(), "gone.yaml"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := execRoot(t, "mix", "cleanup", "--run-id", "x", "--scenario", scenario, "--set", "personas.ci.cloud=tenant-ci",
				"--set", "lanes.glance.enabled=true", "--set", lane, "--set", "lanes.glance.cloud=nope")
			if want := `creating image client for lane "glance": parsing clouds.yaml:`; err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("mix cleanup --run-id = %v, want an error starting with %q", err, want)
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

// TestDeleteLaneResourcesServiceLane confirms a background lane gets only the
// entries its lane created and a persona only the entries its persona created,
// that a failing lane is named as a lane and does not stop the lanes after it,
// and that every lane is cleaned with no entries without a created list.
func TestDeleteLaneResourcesServiceLane(t *testing.T) {
	// A persona and a lane of the same name tell the two filters apart.
	created := []resource.Resource{
		{Kind: "server", ID: "s1", Persona: "ci"},
		{Kind: "project", ID: "p1", Lane: "keystone"},
		{Kind: "server", ID: "s2", Persona: "keystone"},
		{Kind: "image", ID: "i1", Lane: "glance"},
	}

	t.Run("filters by lane and by persona", func(t *testing.T) {
		var log []string
		boom := errors.New("boom")
		lanes := []*mix.Lane{
			(&teardownLane{log: &log}).lane("ci"),
			(&teardownLane{log: &log, cleanupErr: boom}).serviceLane("keystone"),
			(&teardownLane{log: &log}).serviceLane("glance"),
		}
		err := deleteLaneResources(context.Background(), &bytes.Buffer{}, lanes, created, "cleaning up")
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), `cleaning up lane "keystone" (run run1234-keystone): boom`) {
			t.Fatalf("deleteLaneResources = %v, want it to name the failing lane", err)
		}
		if want := []string{"cleanup ci s1", "cleanup keystone p1", "cleanup glance i1"}; strings.Join(log, "|") != strings.Join(want, "|") {
			t.Errorf("calls = %q, want %q", log, want)
		}
	})

	t.Run("no created list", func(t *testing.T) {
		var log []string
		lanes := []*mix.Lane{(&teardownLane{log: &log}).lane("ci"), (&teardownLane{log: &log}).serviceLane("keystone")}
		if err := deleteLaneResources(context.Background(), &bytes.Buffer{}, lanes, nil, "cleaning up"); err != nil {
			t.Fatalf("deleteLaneResources: %v", err)
		}
		if want := []string{"cleanup ci ", "cleanup keystone "}; strings.Join(log, "|") != strings.Join(want, "|") {
			t.Errorf("calls = %q, want %q", log, want)
		}
	})
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
				if n.Pinned != longLived || n.Roll != "" {
					t.Errorf("node %q pinned = %v, roll = %q, want %v and none", n.Key, n.Pinned, n.Roll, longLived)
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

// TestBuildPersonaNodesRolling confirms a rolling persona's lane gets the
// rolling graph, whose workers and their volumes carry their group as the Roll
// value, and an invalid compute plan the builder's error.
func TestBuildPersonaNodesRolling(t *testing.T) {
	plan := func() *novaplan.Plan {
		return &novaplan.Plan{
			Networks:     []novaplan.Network{{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"}},
			ServerGroups: []novaplan.ServerGroup{{Name: "grp-0001", Policy: novaplan.PolicySoftAntiAffinity}},
			Servers:      []novaplan.Server{{Name: "srv-0001", Networks: []string{"net-0001"}, Group: "grp-0001"}},
			Volumes:      []novaplan.Volume{{Name: "vol-0001", SizeGiB: 1, Server: "srv-0001"}},
		}
	}
	var c *nova.Client // the builder only captures the client in its closures
	nodes, err := buildPersonaNodes(&mixplan.Persona{Name: "gardener", Rolling: true, Nova: plan()}, c, novaexec.Resolved{}, time.Minute)
	if err != nil {
		t.Fatalf("buildPersonaNodes: %v", err)
	}
	roll := map[string]string{}
	for _, n := range nodes {
		roll[n.Key] = n.Roll
	}
	if want := map[string]string{"grp-0001": "", "net-0001": "", "srv-0001": "grp-0001", "vol-0001": "grp-0001"}; !reflect.DeepEqual(roll, want) {
		t.Errorf("roll values = %v, want %v", roll, want)
	}

	p := plan()
	p.Servers[0].Networks = []string{"ghost"}
	_, err = buildPersonaNodes(&mixplan.Persona{Name: "gardener", Rolling: true, Nova: p}, c, novaexec.Resolved{}, time.Minute)
	if err == nil || !strings.HasPrefix(err.Error(), "invalid plan:") {
		t.Errorf("buildPersonaNodes = %v, want an error starting with %q", err, "invalid plan:")
	}
}

// laneCleaner is an in-memory novaexec.Cleaner and novaexec.ServerGroupCleaner
// for the lane teardown tests: it lists the servers and server groups still
// live, deletes from that live set, and logs every call in order. The other
// kinds list empty.
type laneCleaner struct {
	servers, groups             []resource.Resource
	serverListErr, groupListErr error
	live                        map[string]bool
	calls                       []string
}

func newLaneCleaner(servers, groups []resource.Resource) *laneCleaner {
	c := &laneCleaner{servers: servers, groups: groups, live: map[string]bool{}}
	for _, r := range append(append([]resource.Resource(nil), servers...), groups...) {
		c.live[r.ID] = true
	}
	return c
}

func (c *laneCleaner) stillLive(list []resource.Resource) []resource.Resource {
	var out []resource.Resource
	for _, r := range list {
		if c.live[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

func (c *laneCleaner) ListServersByMetadata(context.Context, string) ([]resource.Resource, error) {
	c.calls = append(c.calls, "list servers")
	if c.serverListErr != nil {
		return nil, c.serverListErr
	}
	return c.stillLive(c.servers), nil
}
func (c *laneCleaner) ListVolumesByMetadata(context.Context, string) ([]resource.Resource, error) {
	c.calls = append(c.calls, "list volumes")
	return nil, nil
}
func (c *laneCleaner) ListByTag(_ context.Context, kind resource.Kind, _ string) ([]resource.Resource, error) {
	c.calls = append(c.calls, "list "+string(kind))
	return nil, nil
}
func (c *laneCleaner) DeleteNetworkPorts(context.Context, string) (int, error) { return 0, nil }
func (c *laneCleaner) Delete(_ context.Context, r resource.Resource) error {
	c.calls = append(c.calls, "delete "+r.ID)
	c.live[r.ID] = false
	return nil
}
func (c *laneCleaner) WaitForGone(context.Context, resource.Resource) error { return nil }
func (c *laneCleaner) ListServerGroupsByName(context.Context, string) ([]resource.Resource, error) {
	c.calls = append(c.calls, "list server groups")
	if c.groupListErr != nil {
		return nil, c.groupListErr
	}
	return c.stillLive(c.groups), nil
}

// TestLaneCleanup confirms a lane's teardown deletes its server groups after
// the last call of novaexec.Cleanup and counts both, that a failing cleanup
// still deletes the server groups, and that a failing server group sweep comes
// back with the count of both and its error.
func TestLaneCleanup(t *testing.T) {
	servers := []resource.Resource{{Kind: nova.KindServer, ID: "s1"}}
	groups := []resource.Resource{{Kind: nova.KindServerGroup, ID: "g1"}}

	t.Run("servers then groups", func(t *testing.T) {
		c := newLaneCleaner(servers, groups)
		n, err := laneCleanup(context.Background(), c, c, "run1-gardener", nil, time.Second)
		if n != 2 || err != nil {
			t.Fatalf("laneCleanup = %d, %v, want 2, nil", n, err)
		}
		tail := len(c.calls) - 2
		if tail < 1 || !reflect.DeepEqual(c.calls[tail:], []string{"list server groups", "delete g1"}) ||
			slices.Contains(c.calls[:tail], "list server groups") || !slices.Contains(c.calls[:tail], "list subnet") {
			t.Errorf("calls = %v, want every cleanup call before the server group calls", c.calls)
		}
	})

	t.Run("cleanup fails", func(t *testing.T) {
		c := newLaneCleaner(servers, groups)
		c.serverListErr = errors.New("listing servers by metadata: boom")
		n, err := laneCleanup(context.Background(), c, c, "run1-gardener", nil, time.Second)
		if n != 1 || !errors.Is(err, c.serverListErr) {
			t.Errorf("laneCleanup = %d, %v, want 1 (the group) and the cleanup error", n, err)
		}
		if !slices.Contains(c.calls, "delete g1") {
			t.Errorf("calls = %v, want the server group deleted", c.calls)
		}
	})

	t.Run("server groups fail", func(t *testing.T) {
		c := newLaneCleaner(servers, groups)
		c.groupListErr = errors.New("listing server groups by name: boom")
		n, err := laneCleanup(context.Background(), c, c, "run1-gardener", nil, time.Second)
		if n != 1 || !errors.Is(err, c.groupListErr) {
			t.Errorf("laneCleanup = %d, %v, want 1 (the server) and the group error", n, err)
		}
	})
}

// TestLaneLeaked confirms a lane's leak check adds the server groups left to
// the resources novaLeakCheck counts, wraps a failing group listing, and
// returns a failing novaLeakCheck unchanged without listing server groups.
func TestLaneLeaked(t *testing.T) {
	servers := []resource.Resource{{Kind: nova.KindServer, ID: "s1"}}
	groups := []resource.Resource{{Kind: nova.KindServerGroup, ID: "g1"}, {Kind: nova.KindServerGroup, ID: "g2"}}

	t.Run("servers and groups left", func(t *testing.T) {
		c := newLaneCleaner(servers, groups)
		if n, err := laneLeaked(context.Background(), c, c, "run1-gardener"); n != 3 || err != nil {
			t.Errorf("laneLeaked = %d, %v, want 3, nil", n, err)
		}
	})

	t.Run("group listing fails", func(t *testing.T) {
		c := newLaneCleaner(servers, groups)
		c.groupListErr = errors.New("boom")
		n, err := laneLeaked(context.Background(), c, c, "run1-gardener")
		if want := "leak check listing server groups: boom"; n != 1 || err == nil || err.Error() != want {
			t.Errorf("laneLeaked = %d, %v, want 1 and %q", n, err, want)
		}
	})

	t.Run("server listing fails", func(t *testing.T) {
		c := newLaneCleaner(servers, groups)
		c.serverListErr = errors.New("boom")
		_, want := novaLeakCheck(context.Background(), c, "run1-gardener")
		c.calls = nil
		n, err := laneLeaked(context.Background(), c, c, "run1-gardener")
		if n != 0 || err == nil || err.Error() != want.Error() || !errors.Is(err, c.serverListErr) {
			t.Errorf("laneLeaked = %d, %v, want 0 and %v", n, err, want)
		}
		if slices.Contains(c.calls, "list server groups") {
			t.Errorf("calls = %v, want no server group listing", c.calls)
		}
	})
}

// blockingGroupCleaner mimics a wedged compute API: both calls block until
// their context is done.
type blockingGroupCleaner struct{}

func (blockingGroupCleaner) ListServerGroupsByName(ctx context.Context, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingGroupCleaner) Delete(ctx context.Context, _ resource.Resource) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestServerGroupTimeoutCleanerBoundsCalls confirms both calls of the server
// group cleaner return the deadline error on a context without a deadline,
// rather than blocking on a wedged call.
func TestServerGroupTimeoutCleanerBoundsCalls(t *testing.T) {
	tc := serverGroupTimeoutCleaner{inner: blockingGroupCleaner{}, opTimeout: 10 * time.Millisecond}
	for name, call := range map[string]func() error{
		"list": func() error {
			_, err := tc.ListServerGroupsByName(context.Background(), "run1-gardener")
			return err
		},
		"delete": func() error {
			return tc.Delete(context.Background(), resource.Resource{Kind: nova.KindServerGroup, ID: "g1"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- call() }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("err = %v, want context.DeadlineExceeded", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the call did not return; serverGroupTimeoutCleaner failed to bound it")
			}
		})
	}
}
