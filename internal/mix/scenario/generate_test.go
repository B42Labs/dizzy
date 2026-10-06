package scenario

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	novascenario "github.com/B42Labs/dizzy/internal/nova/scenario"
)

var update = flag.Bool("update", false, "update golden files")

// smallScenario equals the shipped small profile: six servers, all for the CI
// persona, on two networks. It backs the golden test.
func smallScenario() Scenario {
	return Scenario{
		Name:      "small",
		Seed:      42,
		Image:     "cirros",
		Flavor:    "m1.tiny",
		Services:  []string{},
		Resources: Resources{Servers: 6},
		Personas: Personas{CI: CI{
			Share:            1,
			Networks:         2,
			VolumesPerServer: novascenario.Range{Min: 0, Max: 1},
			VolumeGiB:        novascenario.Range{Min: 1, Max: 2},
			Interval: novascenario.Interval{
				Min: novascenario.Duration(100 * time.Millisecond),
				Max: novascenario.Duration(time.Second),
			},
			ChurnRatio: 0.5,
			TargetFill: 0.6,
		}},
		Chaos: &Chaos{
			Duration: novascenario.Duration(5 * time.Minute),
			Parallel: novascenario.Parallel{Max: 4},
		},
	}
}

// marshal encodes v as the indented JSON mix generate writes.
func marshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(data, '\n')
}

func TestGenerateDeterministic(t *testing.T) {
	p1, err := smallScenario().Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	p2, err := smallScenario().Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	if !bytes.Equal(marshal(t, p1), marshal(t, p2)) {
		t.Error("the same scenario and seed produced different plans")
	}
}

func TestGenerateSeedChangesPlan(t *testing.T) {
	s := smallScenario()
	p1, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	s.Seed = 7
	p2, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	if bytes.Equal(marshal(t, p1), marshal(t, p2)) {
		t.Error("different seeds produced identical plans")
	}
}

func TestGenerateGolden(t *testing.T) {
	p, err := smallScenario().Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	got := marshal(t, p)

	path := filepath.Join("testdata", "golden", "small.plan.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("generated plan differs from golden file %s; run with -update if the change is intended", path)
	}
}

// TestGenerateSmallPlanShape confirms the small scenario gives the CI persona
// every server and the whole share, under its derived seed.
func TestGenerateSmallPlanShape(t *testing.T) {
	p, err := smallScenario().Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	if p.Scenario != "small" || p.Seed != 42 || len(p.Personas) != 1 {
		t.Fatalf("plan = %s/%d with %d personas, want small/42 with 1", p.Scenario, p.Seed, len(p.Personas))
	}
	ci := p.Personas[0]
	if ci.Name != "ci" || ci.Servers != 6 || ci.Share != 1 || ci.Seed != PersonaSeed(42, "ci") {
		t.Errorf("persona = %+v, want ci with 6 servers, share 1 and seed PersonaSeed(42, ci)", ci)
	}
	if ci.Nova.Scenario != "small/ci" || ci.Nova.Seed != ci.Seed || ci.Nova.Image != "cirros" || ci.Nova.Flavor != "m1.tiny" {
		t.Errorf("compute plan provenance = %s/%d %s/%s, want small/ci with the persona seed, cirros and m1.tiny",
			ci.Nova.Scenario, ci.Nova.Seed, ci.Nova.Image, ci.Nova.Flavor)
	}
	for i, srv := range ci.Nova.Servers {
		if want := fmt.Sprintf("srv-%04d", i+1); srv.Name != want {
			t.Errorf("server %d = %q, want %q", i, srv.Name, want)
		}
	}
	if len(ci.Nova.Servers) != 6 || len(ci.Nova.Networks) != 2 {
		t.Errorf("compute plan has %d servers and %d networks, want 6 and 2", len(ci.Nova.Servers), len(ci.Nova.Networks))
	}
}

// TestCIPersonaShape confirms a CI server has exactly one network, no extra
// port and no lifecycle operation, so its churn node is never mutated.
func TestCIPersonaShape(t *testing.T) {
	s := smallScenario()
	s.Resources.Servers = 40
	s.Personas.CI.VolumesPerServer.Max = 3
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	np := p.Personas[0].Nova
	if len(np.Ports) != 0 || np.ResizeFlavor != "" {
		t.Errorf("compute plan has %d ports and resize flavor %q, want none", len(np.Ports), np.ResizeFlavor)
	}
	for _, srv := range np.Servers {
		if len(srv.Networks) != 1 || srv.StopStart != "" || srv.Resize || srv.LiveMigrate || srv.Delete || srv.BootFromVolume || srv.UserData {
			t.Errorf("server %+v is not a plain CI server", srv)
		}
	}
	if len(np.Volumes) == 0 {
		t.Error("forty servers with up to three volumes each drew no volume")
	}
}

func TestGenerateZeroServers(t *testing.T) {
	s := smallScenario()
	s.Resources.Servers = 0
	_, err := s.Generate()
	if want := "generated plan failed validation: plan has no persona with at least one server"; err == nil || err.Error() != want {
		t.Errorf("Generate() = %v, want %q", err, want)
	}
}

func TestGenerateInvalidScenario(t *testing.T) {
	s := smallScenario()
	s.Name = ""
	_, err := s.Generate()
	if err == nil || !strings.HasPrefix(err.Error(), "invalid scenario: ") {
		t.Errorf("Generate() = %v, want an error starting with %q", err, "invalid scenario: ")
	}
}

// TestGenerateServicesNonNil confirms a scenario without services still emits
// "services": [] rather than null.
func TestGenerateServicesNonNil(t *testing.T) {
	s := smallScenario()
	s.Services = nil
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	if !bytes.Contains(marshal(t, p), []byte(`"services": []`)) {
		t.Errorf("plan JSON lacks \"services\": []:\n%s", marshal(t, p))
	}
}

func TestPersonaSeed(t *testing.T) {
	h := fnv.New64a()
	_, _ = h.Write([]byte("ci"))
	if got, want := PersonaSeed(42, "ci"), 42^int64(h.Sum64()); got != want {
		t.Errorf("PersonaSeed(42, ci) = %d, want %d", got, want)
	}
	if PersonaSeed(42, "ci") == PersonaSeed(42, "legacy") {
		t.Error("PersonaSeed gives ci and legacy the same seed")
	}
}
