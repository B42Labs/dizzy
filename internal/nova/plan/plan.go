// Package plan defines the expanded, fully-enumerated set of Nova resources —
// the expected-state source of truth produced deterministically from a scenario
// plus a seed. Like the neutron and cinder plans it is pure data: every
// collection is a slice (never a map) so that encoding the plan to JSON yields
// byte-identical output for the same input. Images and flavors are referenced by
// name only (dizzy uploads no image and creates no flavor); the companion
// networks, ports, and volumes are created by dizzy itself.
package plan

import (
	"fmt"
	"strings"
)

// Plan is the fully-expanded expected state for one Nova run. Scenario and Seed
// record the provenance that produced it; Image, Flavor, and ResizeFlavor are
// the by-name references resolved against the target cloud at apply time. The
// slices enumerate every server and its companion networks, volumes, and ports.
// Cross-resource references are by logical name, resolved by Validate.
type Plan struct {
	Scenario     string    `json:"scenario"`
	Seed         int64     `json:"seed"`
	Image        string    `json:"image"`
	Flavor       string    `json:"flavor"`
	ResizeFlavor string    `json:"resizeFlavor,omitempty"`
	Networks     []Network `json:"networks"`
	Servers      []Server  `json:"servers"`
	Volumes      []Volume  `json:"volumes"`
	Ports        []Port    `json:"ports"`
}

// Network is a tenant network dizzy creates for its servers, together with the
// single subnet it carries. CIDR is the subnet's address range.
type Network struct {
	Name   string `json:"name"`
	Subnet string `json:"subnet"`
	CIDR   string `json:"cidr"`
}

// Server is one instance to boot and drive through its lifecycle. Networks lists
// the logical names of the networks it is wired into (at least one; more than
// one exercises multi-network booting). BootFromVolume boots it from a
// dizzy-created root volume of RootVolumeGiB instead of directly from the image.
// UserData injects a deterministic cloud-config at boot. StopStart is "", "soft"
// (os-stop then os-start), or "hard" (a hard reboot). Resize resizes it to the
// plan's ResizeFlavor and confirms the resize. LiveMigrate live-migrates it when
// the admin pre-check permits. Delete deletes it during the run.
type Server struct {
	Name           string   `json:"name"`
	BootFromVolume bool     `json:"bootFromVolume,omitempty"`
	RootVolumeGiB  int      `json:"rootVolumeGiB,omitempty"`
	Networks       []string `json:"networks"`
	UserData       bool     `json:"userData,omitempty"`
	StopStart      string   `json:"stopStart,omitempty"`
	Resize         bool     `json:"resize,omitempty"`
	LiveMigrate    bool     `json:"liveMigrate,omitempty"`
	Delete         bool     `json:"delete,omitempty"`
}

// Volume is a data volume dizzy creates and attaches to its server. SizeGiB is
// its size. Detach detaches it again during the run.
type Volume struct {
	Name    string `json:"name"`
	SizeGiB int    `json:"sizeGiB"`
	Server  string `json:"server"`
	Detach  bool   `json:"detach,omitempty"`
}

// Port is a port dizzy creates on Network and attaches to Server. Network must
// be one of the server's own networks. Detach detaches it again during the run.
type Port struct {
	Name    string `json:"name"`
	Network string `json:"network"`
	Server  string `json:"server"`
	Detach  bool   `json:"detach,omitempty"`
}

// StopStart variants.
const (
	StopStartSoft = "soft"
	StopStartHard = "hard"
)

// Validate checks the plan graph for well-formedness: every server references at
// least one known network, a boot-from-volume server has a positive root size, a
// server marked for resize has a resize flavor, every volume references a known
// server with a positive size, and every port references a known server and a
// known network that is one of that server's networks. It returns an error
// naming the first offending resource.
func (p *Plan) Validate() error {
	networks := make(map[string]bool, len(p.Networks))
	for _, n := range p.Networks {
		networks[n.Name] = true
	}

	servers := make(map[string]map[string]bool, len(p.Servers))
	for _, s := range p.Servers {
		if len(s.Networks) == 0 {
			return fmt.Errorf("server %q references no network, want at least 1", s.Name)
		}
		member := make(map[string]bool, len(s.Networks))
		for _, net := range s.Networks {
			if !networks[net] {
				return fmt.Errorf("server %q references unknown network %q", s.Name, net)
			}
			member[net] = true
		}
		if s.BootFromVolume && s.RootVolumeGiB < 1 {
			return fmt.Errorf("server %q boots from volume but has root size %d GiB, want at least 1", s.Name, s.RootVolumeGiB)
		}
		if s.Resize && p.ResizeFlavor == "" {
			return fmt.Errorf("server %q is marked for resize but the plan has no resize flavor", s.Name)
		}
		servers[s.Name] = member
	}

	for _, v := range p.Volumes {
		if v.SizeGiB < 1 {
			return fmt.Errorf("volume %q has size %d GiB, want at least 1", v.Name, v.SizeGiB)
		}
		if _, ok := servers[v.Server]; !ok {
			return fmt.Errorf("volume %q references unknown server %q", v.Name, v.Server)
		}
	}

	for _, pt := range p.Ports {
		member, ok := servers[pt.Server]
		if !ok {
			return fmt.Errorf("port %q references unknown server %q", pt.Name, pt.Server)
		}
		if !networks[pt.Network] {
			return fmt.Errorf("port %q references unknown network %q", pt.Name, pt.Network)
		}
		if !member[pt.Network] {
			return fmt.Errorf("port %q is on network %q, which is not one of server %q's networks", pt.Name, pt.Network, pt.Server)
		}
	}

	return nil
}

// Resizes counts the servers the plan resizes.
func (p *Plan) Resizes() int {
	var n int
	for _, s := range p.Servers {
		if s.Resize {
			n++
		}
	}
	return n
}

// LiveMigrations counts the servers the plan live-migrates.
func (p *Plan) LiveMigrations() int {
	var n int
	for _, s := range p.Servers {
		if s.LiveMigrate {
			n++
		}
	}
	return n
}

// Deletes counts the servers the plan deletes during the run.
func (p *Plan) Deletes() int {
	var n int
	for _, s := range p.Servers {
		if s.Delete {
			n++
		}
	}
	return n
}

// StopStarts counts the servers the plan stop/starts, split by soft and hard
// variant.
func (p *Plan) StopStarts() (soft, hard int) {
	for _, s := range p.Servers {
		switch s.StopStart {
		case StopStartSoft:
			soft++
		case StopStartHard:
			hard++
		}
	}
	return soft, hard
}

// BootsFromVolume counts the servers that boot from a dizzy-created root volume.
func (p *Plan) BootsFromVolume() int {
	var n int
	for _, s := range p.Servers {
		if s.BootFromVolume {
			n++
		}
	}
	return n
}

// DetachedVolumes counts the data volumes the plan detaches during the run.
func (p *Plan) DetachedVolumes() int {
	var n int
	for _, v := range p.Volumes {
		if v.Detach {
			n++
		}
	}
	return n
}

// DetachedPorts counts the ports the plan detaches during the run.
func (p *Plan) DetachedPorts() int {
	var n int
	for _, pt := range p.Ports {
		if pt.Detach {
			n++
		}
	}
	return n
}

// Summary returns a deterministic, human-readable count of the plan, used by
// "nova apply --dry-run" to preview a scenario without touching a cloud.
func (p *Plan) Summary() string {
	soft, hard := p.StopStarts()
	var b strings.Builder
	fmt.Fprintf(&b, "Plan for scenario %q (seed %d)\n", p.Scenario, p.Seed)
	fmt.Fprintf(&b, "  image:           %s\n", p.Image)
	fmt.Fprintf(&b, "  flavor:          %s\n", p.Flavor)
	if p.ResizeFlavor != "" {
		fmt.Fprintf(&b, "  resize flavor:   %s\n", p.ResizeFlavor)
	}
	fmt.Fprintf(&b, "  networks:        %d\n", len(p.Networks))
	fmt.Fprintf(&b, "  servers:         %d (%d boot-from-volume)\n", len(p.Servers), p.BootsFromVolume())
	fmt.Fprintf(&b, "  stop/start:      %d soft, %d hard\n", soft, hard)
	fmt.Fprintf(&b, "  resizes:         %d\n", p.Resizes())
	fmt.Fprintf(&b, "  live-migrations: %d\n", p.LiveMigrations())
	fmt.Fprintf(&b, "  deletes:         %d\n", p.Deletes())
	fmt.Fprintf(&b, "  volumes:         %d (%d detached)\n", len(p.Volumes), p.DetachedVolumes())
	fmt.Fprintf(&b, "  ports:           %d (%d detached)\n", len(p.Ports), p.DetachedPorts())
	return b.String()
}
