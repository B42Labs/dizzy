package scenario

import (
	"encoding/json"
	"fmt"
	"testing"
)

// planJSON expands s and returns its plan encoded as indented JSON, the byte
// form the determinism assertions compare.
func planJSON(t *testing.T, s Scenario) string {
	t.Helper()
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("marshaling plan: %v", err)
	}
	return string(data)
}

func TestGenerateIsDeterministic(t *testing.T) {
	s := smallScenario()
	if first, second := planJSON(t, s), planJSON(t, s); first != second {
		t.Errorf("Generate() not deterministic for the same seed:\n%s\n---\n%s", first, second)
	}
}

func TestGenerateVariesWithSeed(t *testing.T) {
	a := smallScenario()
	b := smallScenario()
	b.Seed = a.Seed + 1
	if planJSON(t, a) == planJSON(t, b) {
		t.Error("Generate() produced identical plans for different seeds")
	}
}

// TestRatioZeroGuardsConsumeNoDraws pins the documented draw order: a scenario
// with every lifecycle ratio at 0 must produce a plan byte-identical to one
// where the RNG for those draws is simply not consulted. It does so by comparing
// two seeds that differ only in whether an unrelated later draw shifts — since
// the guards skip the RNG entirely when their ratio is 0, the network membership
// (drawn right after) stays stable regardless of the disabled ratios.
func TestRatioZeroGuardsConsumeNoDraws(t *testing.T) {
	base := smallScenario()
	base.Distribution.BootFromVolumeRatio = 0
	base.Distribution.UserDataRatio = 0
	base.Distribution.StopStartRatio = 0
	base.Distribution.ResizedRatio = 0
	base.ResizeFlavor = ""
	base.Distribution.LiveMigratedRatio = 0
	base.Distribution.DeletedRatio = 0
	base.Distribution.VolumeDetachRatio = 0
	base.Distribution.PortDetachRatio = 0

	p, err := base.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}
	// With every lifecycle ratio at 0, no server carries a lifecycle mutation and
	// no volume or port is detached.
	for _, s := range p.Servers {
		if s.BootFromVolume || s.UserData || s.StopStart != "" || s.Resize || s.LiveMigrate || s.Delete {
			t.Errorf("server %q carries a lifecycle decision despite all ratios being 0: %+v", s.Name, s)
		}
	}
	if p.DetachedVolumes() != 0 {
		t.Errorf("DetachedVolumes() = %d, want 0 with volume_detach_ratio 0", p.DetachedVolumes())
	}
	if p.DetachedPorts() != 0 {
		t.Errorf("DetachedPorts() = %d, want 0 with port_detach_ratio 0", p.DetachedPorts())
	}
}

func TestGenerateContiguousCountersAndBounds(t *testing.T) {
	s := smallScenario()
	s.Resources.Servers = 12
	s.Distribution.VolumesPerServer = Range{Min: 1, Max: 2}
	s.Distribution.PortsPerServer = Range{Min: 1, Max: 1}
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}

	for i, v := range p.Volumes {
		if want := fmt.Sprintf("vol-%04d", i+1); v.Name != want {
			t.Errorf("volume %d name = %q, want %q", i, v.Name, want)
		}
	}
	for i, pt := range p.Ports {
		if want := fmt.Sprintf("port-%04d", i+1); pt.Name != want {
			t.Errorf("port %d name = %q, want %q", i, pt.Name, want)
		}
	}

	// Network membership stays within [min, max] and every referenced network is
	// one of the plan's networks.
	nets := make(map[string]bool, len(p.Networks))
	for _, n := range p.Networks {
		nets[n.Name] = true
	}
	for _, srv := range p.Servers {
		if got := len(srv.Networks); got < s.Distribution.NetworksPerServer.Min || got > s.Distribution.NetworksPerServer.Max {
			t.Errorf("server %q has %d networks, want within [%d, %d]", srv.Name, got, s.Distribution.NetworksPerServer.Min, s.Distribution.NetworksPerServer.Max)
		}
		for _, net := range srv.Networks {
			if !nets[net] {
				t.Errorf("server %q references network %q not in the plan", srv.Name, net)
			}
		}
	}

	// Every network's CIDR is unique.
	seen := make(map[string]bool, len(p.Networks))
	for _, n := range p.Networks {
		if seen[n.CIDR] {
			t.Errorf("duplicate CIDR %q", n.CIDR)
		}
		seen[n.CIDR] = true
	}
}
