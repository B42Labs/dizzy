package scenario

import (
	"fmt"
	"math/rand"
	"net/netip"

	"github.com/B42Labs/dizzy/internal/nova/plan"
)

// ipv4Base is 10.0.0.0; each network takes the next /24 block above it
// (10.0.1.0/24, 10.0.2.0/24, …), so the small network counts these scenarios
// use never collide and never overflow an octet.
const ipv4Base = uint32(10) << 24

// Generate expands the scenario and its seed into a fully-enumerated plan. The
// same scenario and seed always produce a byte-identical plan: the generator
// draws from math/rand v1 (whose sequence is frozen for compatibility) in a
// fixed order and emits every collection in a fixed order. The returned plan is
// validated before it is handed back.
//
// Networks are emitted first (net-%04d, each with its subnet and /24 CIDR). Then
// per server srv-%04d, in order, each decision is drawn only when its governing
// ratio or range is non-degenerate — drawing a decision only when its ratio is
// set keeps a ratio-0 plan byte-identical to one generated before that decision
// existed, mirroring the neutron generator's external-gateway guard. The fixed
// per-server draw order is: boot-from-volume (then its root size), the network
// count and membership, user data, stop/start (then its hard-variant draw),
// resize, live-migrate, delete, then the volume count with each volume's size
// and detach flag, then the port count with each port's network pick and detach
// flag. Each drawn volume and port is appended straight into the plan with a
// global contiguous counter (vol-%04d / port-%04d) in draw order.
func (s Scenario) Generate() (*plan.Plan, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid scenario: %w", err)
	}

	rng := rand.New(rand.NewSource(s.Seed))
	p := &plan.Plan{
		Scenario:     s.Name,
		Seed:         s.Seed,
		Image:        s.Image,
		Flavor:       s.Flavor,
		ResizeFlavor: s.ResizeFlavor,
	}

	p.Networks = make([]plan.Network, 0, s.Resources.Networks)
	for i := 0; i < s.Resources.Networks; i++ {
		block := ipv4Base + uint32(i+1)*256
		cidr := netip.PrefixFrom(
			netip.AddrFrom4([4]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)}),
			24).String()
		p.Networks = append(p.Networks, plan.Network{
			Name:   fmt.Sprintf("net-%04d", i+1),
			Subnet: fmt.Sprintf("sub-%04d", i+1),
			CIDR:   cidr,
		})
	}

	p.Servers = make([]plan.Server, 0, s.Resources.Servers)
	p.Volumes = make([]plan.Volume, 0)
	p.Ports = make([]plan.Port, 0)
	for i := 0; i < s.Resources.Servers; i++ {
		srv := plan.Server{Name: fmt.Sprintf("srv-%04d", i+1)}

		if s.Distribution.BootFromVolumeRatio > 0 && rng.Float64() < s.Distribution.BootFromVolumeRatio {
			srv.BootFromVolume = true
			srv.RootVolumeGiB = randRange(rng, s.Distribution.RootVolumeGiB)
		}

		k := randRange(rng, s.Distribution.NetworksPerServer)
		srv.Networks = make([]string, 0, k)
		for _, idx := range rng.Perm(s.Resources.Networks)[:k] {
			srv.Networks = append(srv.Networks, p.Networks[idx].Name)
		}

		if s.Distribution.UserDataRatio > 0 && rng.Float64() < s.Distribution.UserDataRatio {
			srv.UserData = true
		}

		if s.Distribution.StopStartRatio > 0 && rng.Float64() < s.Distribution.StopStartRatio {
			srv.StopStart = plan.StopStartSoft
			if s.Distribution.StopStartHardRatio > 0 && rng.Float64() < s.Distribution.StopStartHardRatio {
				srv.StopStart = plan.StopStartHard
			}
		}

		if s.Distribution.ResizedRatio > 0 && rng.Float64() < s.Distribution.ResizedRatio {
			srv.Resize = true
		}

		if s.Distribution.LiveMigratedRatio > 0 && rng.Float64() < s.Distribution.LiveMigratedRatio {
			srv.LiveMigrate = true
		}

		if s.Distribution.DeletedRatio > 0 && rng.Float64() < s.Distribution.DeletedRatio {
			srv.Delete = true
		}

		for v := 0; v < randRange(rng, s.Distribution.VolumesPerServer); v++ {
			vol := plan.Volume{
				Name:    fmt.Sprintf("vol-%04d", len(p.Volumes)+1),
				SizeGiB: randRange(rng, s.Distribution.AttachedVolumeGiB),
				Server:  srv.Name,
			}
			if s.Distribution.VolumeDetachRatio > 0 && rng.Float64() < s.Distribution.VolumeDetachRatio {
				vol.Detach = true
			}
			p.Volumes = append(p.Volumes, vol)
		}

		for pt := 0; pt < randRange(rng, s.Distribution.PortsPerServer); pt++ {
			port := plan.Port{
				Name:    fmt.Sprintf("port-%04d", len(p.Ports)+1),
				Network: srv.Networks[rng.Intn(len(srv.Networks))],
				Server:  srv.Name,
			}
			if s.Distribution.PortDetachRatio > 0 && rng.Float64() < s.Distribution.PortDetachRatio {
				port.Detach = true
			}
			p.Ports = append(p.Ports, port)
		}

		p.Servers = append(p.Servers, srv)
	}

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("generated plan failed validation: %w", err)
	}
	return p, nil
}

// randRange returns a uniformly random integer in the inclusive interval
// [r.Min, r.Max]. The caller guarantees r.Min <= r.Max via Scenario.Validate.
func randRange(rng *rand.Rand, r Range) int {
	return r.Min + rng.Intn(r.Max-r.Min+1)
}
