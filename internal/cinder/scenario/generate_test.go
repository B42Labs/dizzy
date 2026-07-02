package scenario

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/B42Labs/openstack-tester/internal/cinder/plan"
)

var update = flag.Bool("update", false, "update golden files")

// marshal renders a plan exactly as the generate command does: indented JSON
// with a trailing newline.
func marshal(t *testing.T, p *plan.Plan) []byte {
	t.Helper()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("marshalling plan: %v", err)
	}
	return append(data, '\n')
}

func TestGenerateInvalidScenario(t *testing.T) {
	s := smallScenario()
	s.Name = "" // fails Scenario.Validate

	if _, err := s.Generate(); err == nil {
		t.Fatal("Generate() = nil error, want error for invalid scenario")
	}
}

func TestGenerateDeterministic(t *testing.T) {
	s := smallScenario()

	p1, err := s.Generate()
	if err != nil {
		t.Fatalf("first Generate(): %v", err)
	}
	p2, err := s.Generate()
	if err != nil {
		t.Fatalf("second Generate(): %v", err)
	}

	if got, want := marshal(t, p1), marshal(t, p2); !bytes.Equal(got, want) {
		t.Error("two generations of the same scenario+seed differ")
	}
}

func TestGenerateSeedChangesPlan(t *testing.T) {
	s1 := smallScenario()
	s2 := smallScenario()
	s2.Seed = s1.Seed + 1

	p1, err := s1.Generate()
	if err != nil {
		t.Fatalf("Generate(seed=%d): %v", s1.Seed, err)
	}
	p2, err := s2.Generate()
	if err != nil {
		t.Fatalf("Generate(seed=%d): %v", s2.Seed, err)
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

// TestGenerateInvariants exercises the generator's structural guarantees over a
// scenario that mixes resized and unresized volumes and multiple snapshots: the
// volume count is exact, every resize target strictly exceeds its size, and
// every snapshot references a real volume.
func TestGenerateInvariants(t *testing.T) {
	s := smallScenario()
	s.Resources.Volumes = 40

	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}

	if got := len(p.Volumes); got != 40 {
		t.Fatalf("volumes = %d, want 40", got)
	}
	volumes := make(map[string]bool, len(p.Volumes))
	var resized int
	for _, v := range p.Volumes {
		if v.SizeGiB < 1 {
			t.Errorf("volume %q has non-positive size %d", v.Name, v.SizeGiB)
		}
		if v.ResizeToGiB != 0 {
			resized++
			if v.ResizeToGiB <= v.SizeGiB {
				t.Errorf("volume %q resize target %d is not larger than size %d", v.Name, v.ResizeToGiB, v.SizeGiB)
			}
		}
		volumes[v.Name] = true
	}
	if resized == 0 {
		t.Error("no volumes resized despite volume_resized_ratio > 0")
	}
	for _, snap := range p.Snapshots {
		if !volumes[snap.Volume] {
			t.Errorf("snapshot %q references unknown volume %q", snap.Name, snap.Volume)
		}
	}
}

// TestGenerateRatioZeroNeverResizes confirms that with volume_resized_ratio 0 no
// volume carries a resize target — the guard that keeps the RNG sequence
// undisturbed.
func TestGenerateRatioZeroNeverResizes(t *testing.T) {
	s := smallScenario()
	s.Distribution.VolumeResizedRatio = 0

	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	for _, v := range p.Volumes {
		if v.ResizeToGiB != 0 {
			t.Errorf("volume %q resized despite ratio 0", v.Name)
		}
	}
}

// TestGenerateNoSnapshots confirms a {0,0} snapshot range yields no snapshots.
func TestGenerateNoSnapshots(t *testing.T) {
	s := smallScenario()
	s.Distribution.SnapshotsPerVolume = Range{Min: 0, Max: 0}

	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	if len(p.Snapshots) != 0 {
		t.Errorf("snapshots = %d, want 0 for a {0,0} range", len(p.Snapshots))
	}
}
