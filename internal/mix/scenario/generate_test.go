package scenario

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	novascenario "github.com/B42Labs/dizzy/internal/nova/scenario"
)

var update = flag.Bool("update", false, "update golden files")

// smallScenario equals the shipped small profile: six servers, five for the CI
// persona on two networks and one for the Legacy persona on one. It backs the
// golden test.
func smallScenario() Scenario {
	return Scenario{
		Name:      "small",
		Seed:      42,
		Image:     "cirros",
		Flavor:    "m1.tiny",
		Services:  []string{},
		Resources: Resources{Servers: 6},
		Personas: Personas{
			CI: CI{
				Share:            0.8,
				Networks:         2,
				VolumesPerServer: novascenario.Range{Min: 0, Max: 1},
				VolumeGiB:        novascenario.Range{Min: 1, Max: 2},
				Interval: novascenario.Interval{
					Min: novascenario.Duration(100 * time.Millisecond),
					Max: novascenario.Duration(time.Second),
				},
				ChurnRatio: 0.5,
				TargetFill: 0.6,
			},
			Legacy: legacyBlock(),
		},
		Chaos: &Chaos{
			Duration: novascenario.Duration(5 * time.Minute),
			Parallel: novascenario.Parallel{Max: 4},
		},
	}
}

// legacyBlock is the legacy block of the shipped small profile.
func legacyBlock() Legacy {
	return Legacy{
		Share:            0.2,
		Networks:         1,
		ResizeFlavor:     "m1.small",
		VolumesPerServer: novascenario.Range{Min: 1, Max: 2},
		VolumeGiB:        novascenario.Range{Min: 1, Max: 2},
		PortsPerServer:   novascenario.Range{Min: 0, Max: 1},
		Interval: novascenario.Interval{
			Min: novascenario.Duration(10 * time.Second),
			Max: novascenario.Duration(time.Minute),
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

// TestGenerateSmallPlanShape confirms the small scenario divides its servers 5
// to 1 between the CI and the Legacy persona, in canonical order, each under
// its derived seed, and that only the Legacy persona is long-lived.
func TestGenerateSmallPlanShape(t *testing.T) {
	p, err := smallScenario().Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	if p.Scenario != "small" || p.Seed != 42 {
		t.Fatalf("plan = %s/%d, want small/42", p.Scenario, p.Seed)
	}
	type entry struct {
		name      string
		servers   int
		planned   int
		share     float64
		longLived bool
	}
	var got []entry
	for _, ps := range p.Personas {
		got = append(got, entry{ps.Name, ps.Servers, len(ps.Nova.Servers), ps.Share, ps.LongLived})
	}
	want := []entry{{"ci", 5, 5, 0.8, false}, {"legacy", 1, 1, 0.2, true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("personas = %+v, want %+v", got, want)
	}
	for _, ps := range p.Personas {
		if ps.Seed != PersonaSeed(42, ps.Name) || ps.Nova.Scenario != "small/"+ps.Name || ps.Nova.Seed != ps.Seed ||
			ps.Nova.Image != "cirros" || ps.Nova.Flavor != "m1.tiny" {
			t.Errorf("%s compute plan provenance = %s/%d %s/%s, want small/%s with the persona seed, cirros and m1.tiny",
				ps.Name, ps.Nova.Scenario, ps.Nova.Seed, ps.Nova.Image, ps.Nova.Flavor, ps.Name)
		}
		for i, srv := range ps.Nova.Servers {
			if want := fmt.Sprintf("srv-%04d", i+1); srv.Name != want {
				t.Errorf("%s server %d = %q, want %q", ps.Name, i, srv.Name, want)
			}
		}
	}
	if ci, legacy := p.Personas[0].Nova, p.Personas[1].Nova; len(ci.Networks) != 2 || len(legacy.Networks) != 1 || legacy.ResizeFlavor != "m1.small" {
		t.Errorf("networks ci/legacy = %d/%d, legacy resize flavor %q, want 2/1 and m1.small", len(ci.Networks), len(legacy.Networks), legacy.ResizeFlavor)
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

// TestLegacyPersonaShape confirms a Legacy server carries every operation the
// persona exercises, on one network, and that every Legacy volume and port is
// detached and re-attached.
func TestLegacyPersonaShape(t *testing.T) {
	s := smallScenario()
	s.Resources.Servers = 40
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	np := p.Personas[1].Nova
	for _, srv := range np.Servers {
		if srv.StopStart != "soft" || !srv.Resize || !srv.LiveMigrate || !srv.ColdMigrate || len(srv.Networks) != 1 {
			t.Errorf("server %+v, want soft stop/start, resize, live and cold migration and one network", srv)
		}
		if srv.Delete || srv.BootFromVolume || srv.UserData {
			t.Errorf("server %+v carries an operation the Legacy persona does not use", srv)
		}
	}
	if len(np.Volumes) == 0 || len(np.Ports) == 0 {
		t.Fatalf("eight servers drew %d volumes and %d ports, want some of each", len(np.Volumes), len(np.Ports))
	}
	for _, v := range np.Volumes {
		if !v.Detach {
			t.Errorf("volume %+v is not detached", v)
		}
	}
	for _, pt := range np.Ports {
		if !pt.Detach {
			t.Errorf("port %+v is not detached", pt)
		}
	}
}

// TestLegacyWithoutResizeFlavor confirms an empty resize flavor turns resize
// off, and that zero volume and port ranges give empty lists.
func TestLegacyWithoutResizeFlavor(t *testing.T) {
	s := smallScenario()
	s.Personas.Legacy.ResizeFlavor = ""
	s.Personas.Legacy.VolumesPerServer = novascenario.Range{}
	s.Personas.Legacy.PortsPerServer = novascenario.Range{}
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	np := p.Personas[1].Nova
	for _, srv := range np.Servers {
		if srv.Resize || !srv.ColdMigrate {
			t.Errorf("server %+v, want no resize and a cold migration", srv)
		}
	}
	data := string(marshal(t, np))
	if strings.Contains(data, `"resizeFlavor"`) || !strings.Contains(data, `"volumes": []`) || !strings.Contains(data, `"ports": []`) {
		t.Errorf("legacy compute plan = %s, want no resizeFlavor and empty volumes and ports", data)
	}
}

// TestGenerateLegacyOnly confirms a scenario whose CI share is 0 gives the
// Legacy persona every server.
func TestGenerateLegacyOnly(t *testing.T) {
	s := smallScenario()
	s.Personas.CI.Share = 0
	s.Personas.Legacy.Share = 1
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	if len(p.Personas) != 1 || p.Personas[0].Name != "legacy" || p.Personas[0].Servers != 6 || p.Personas[0].Share != 1 {
		t.Errorf("personas = %+v, want legacy alone with 6 servers and share 1", p.Personas)
	}
}

// TestCIPlanJSONUnchangedKeys confirms the CI persona's entry carries neither
// longLived nor coldMigrate, so its plan JSON keeps its bytes, while the
// Legacy entry carries both.
func TestCIPlanJSONUnchangedKeys(t *testing.T) {
	p, err := smallScenario().Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	for _, ps := range p.Personas {
		data := string(marshal(t, ps))
		want := ps.Name == "legacy"
		if strings.Contains(data, `"longLived"`) != want || strings.Contains(data, `"coldMigrate"`) != want {
			t.Errorf("persona %s JSON has longLived=%v coldMigrate=%v, want both %v",
				ps.Name, strings.Contains(data, `"longLived"`), strings.Contains(data, `"coldMigrate"`), want)
		}
	}
}
