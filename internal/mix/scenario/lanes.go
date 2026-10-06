package scenario

import (
	"fmt"
	"strings"

	cinderscenario "github.com/B42Labs/dizzy/internal/cinder/scenario"
	glancescenario "github.com/B42Labs/dizzy/internal/glance/scenario"
	keystonescenario "github.com/B42Labs/dizzy/internal/keystone/scenario"
	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	neutronscenario "github.com/B42Labs/dizzy/internal/scenario"
	"github.com/B42Labs/dizzy/scenarios"
)

// laneBlock is one block of the lanes block: the lane's name, which is the
// service it churns, and the keys every lane has.
type laneBlock struct {
	name string
	lane *Lane
}

// blocks returns the lanes in canonical order, alphabetical by name. It is
// the one list of the lanes this build defines.
func (l *Lanes) blocks() []laneBlock {
	return []laneBlock{
		{"cinder", &l.Cinder.Lane},
		{"glance", &l.Glance},
		{"keystone", &l.Keystone.Lane},
		{"neutron", &l.Neutron.Lane},
	}
}

// NamedLane is one lane of the lanes block under its name, which is the
// service it churns.
type NamedLane struct {
	Name string
	Lane
}

// Enabled returns every enabled lane, in canonical order, which is the order
// of the plan's lanes. It reads no lane scenario.
func (l *Lanes) Enabled() []NamedLane {
	var out []NamedLane
	for _, b := range l.blocks() {
		if b.lane.Enabled {
			out = append(out, NamedLane{Name: b.name, Lane: *b.lane})
		}
	}
	return out
}

// validate checks every enabled lane, in canonical order, and returns the
// first error: the lane names its scenario in exactly one way, its profile is
// a bundled one, and the Keystone lane's privilege is a known tier. A disabled
// lane is not checked.
func (l *Lanes) validate() error {
	for _, b := range l.blocks() {
		if !b.lane.Enabled {
			continue
		}
		switch p := b.lane.Profile; {
		case p != "" && b.lane.Scenario != "":
			return fmt.Errorf("lanes.%s: set profile or scenario, not both", b.name)
		case p == "" && b.lane.Scenario == "":
			return fmt.Errorf("lanes.%s: an enabled lane needs a profile or a scenario", b.name)
		case p != "" && p != "small" && p != "medium" && p != "large":
			return fmt.Errorf("lanes.%s.profile must be small, medium or large, got %q", b.name, p)
		}
		if b.name == "keystone" {
			switch p := l.Keystone.Privilege; p {
			case "", "auto", "admin", "domain-manager":
			default:
				return fmt.Errorf("lanes.keystone.privilege must be auto, admin or domain-manager, got %q", p)
			}
		}
	}
	return nil
}

// set applies one lanes.* override: enabled, cloud, profile and scenario of
// every lane, and the service-specific keys of the Cinder, Keystone and
// Neutron lanes. Any other key is unknown.
func (l *Lanes) set(key, value string) error {
	switch key {
	case "lanes.cinder.volume_type":
		l.Cinder.VolumeType = value
		return nil
	case "lanes.keystone.privilege":
		l.Keystone.Privilege = value
		return nil
	case "lanes.keystone.domain":
		l.Keystone.Domain = value
		return nil
	case "lanes.keystone.roles":
		l.Keystone.Roles = value
		return nil
	case "lanes.neutron.external_network":
		l.Neutron.ExternalNetwork = value
		return nil
	}
	for _, b := range l.blocks() {
		field, ok := strings.CutPrefix(key, "lanes."+b.name+".")
		if !ok {
			continue
		}
		switch field {
		case "enabled":
			return setBool(&b.lane.Enabled, key, value)
		case "cloud":
			b.lane.Cloud = value
			return nil
		case "profile":
			b.lane.Profile = value
			return nil
		case "scenario":
			b.lane.Scenario = value
			return nil
		}
	}
	return fmt.Errorf("unknown override key %q", key)
}

// LaneScenarios holds the parsed scenario of every enabled lane; nil means the
// lane is off.
type LaneScenarios struct {
	Cinder   *cinderscenario.Scenario
	Glance   *glancescenario.Scenario
	Keystone *keystonescenario.Scenario
	Neutron  *neutronscenario.Scenario
}

// LoadLanes reads and parses the scenario of every enabled lane, in canonical
// order. It validates s first. A lane on a profile reads the service's
// embedded profile, and a lane on a scenario file reads it through readFile.
// Each parsed scenario is named <scenario name>/<lane> and seeded with
// PersonaSeed(s.Seed, <lane>), so the lane scenario's own name and seed have
// no effect. With no lane enabled it returns the zero value and reads nothing.
func (s Scenario) LoadLanes(readFile func(path string) ([]byte, error)) (LaneScenarios, error) {
	if err := s.Validate(); err != nil {
		return LaneScenarios{}, fmt.Errorf("invalid scenario: %w", err)
	}

	var ls LaneScenarios
	for _, b := range s.Lanes.blocks() {
		if !b.lane.Enabled {
			continue
		}
		data, err := b.read(readFile)
		if err != nil {
			return LaneScenarios{}, err
		}
		name, seed := s.Name+"/"+b.name, PersonaSeed(s.Seed, b.name)
		switch b.name {
		case "cinder":
			var sc cinderscenario.Scenario
			if sc, err = cinderscenario.Parse(data); err == nil {
				sc.Name, sc.Seed = name, seed
				ls.Cinder = &sc
			}
		case "glance":
			var sc glancescenario.Scenario
			if sc, err = glancescenario.Parse(data); err == nil {
				sc.Name, sc.Seed = name, seed
				ls.Glance = &sc
			}
		case "keystone":
			var sc keystonescenario.Scenario
			if sc, err = keystonescenario.Parse(data); err == nil {
				sc.Name, sc.Seed = name, seed
				ls.Keystone = &sc
			}
		case "neutron":
			var sc neutronscenario.Scenario
			if sc, err = neutronscenario.Parse(data); err == nil {
				sc.Name, sc.Seed = name, seed
				ls.Neutron = &sc
			}
		}
		if err != nil {
			return LaneScenarios{}, fmt.Errorf("lanes.%s: %w", b.name, err)
		}
	}
	return ls, nil
}

// read returns the scenario bytes of an enabled lane: the service's embedded
// profile when the lane names one, else the file at its scenario path, read
// through readFile.
func (b laneBlock) read(readFile func(path string) ([]byte, error)) ([]byte, error) {
	if p := b.lane.Profile; p != "" {
		data, err := scenarios.Files.ReadFile(b.name + "/" + p + ".yaml")
		if err != nil {
			return nil, fmt.Errorf("lanes.%s: reading profile %s: %w", b.name, p, err)
		}
		return data, nil
	}
	data, err := readFile(b.lane.Scenario)
	if err != nil {
		return nil, fmt.Errorf("lanes.%s: reading scenario %s: %w", b.name, b.lane.Scenario, err)
	}
	return data, nil
}

// generate expands the loaded scenario of the named lane into its plan lane,
// under the scenario's seed. It fails when the scenario was not loaded, when
// the service's generator fails, and when the plan is empty: no volumes
// (cinder), no images (glance), neither projects nor users (keystone), or no
// networks, routers and security groups (neutron).
func (ls LaneScenarios) generate(name string) (mixplan.Lane, error) {
	l := mixplan.Lane{Name: name}
	var (
		loaded, empty bool
		err           error
	)
	switch name {
	case "cinder":
		if loaded = ls.Cinder != nil; loaded {
			l.Seed = ls.Cinder.Seed
			if l.Cinder, err = ls.Cinder.Generate(); err == nil {
				empty = len(l.Cinder.Volumes) == 0
			}
		}
	case "glance":
		if loaded = ls.Glance != nil; loaded {
			l.Seed = ls.Glance.Seed
			if l.Glance, err = ls.Glance.Generate(); err == nil {
				empty = len(l.Glance.Images) == 0
			}
		}
	case "keystone":
		if loaded = ls.Keystone != nil; loaded {
			l.Seed = ls.Keystone.Seed
			if l.Keystone, err = ls.Keystone.Generate(); err == nil {
				empty = len(l.Keystone.Projects) == 0 && len(l.Keystone.Users) == 0
			}
		}
	case "neutron":
		if loaded = ls.Neutron != nil; loaded {
			l.Seed = ls.Neutron.Seed
			if l.Neutron, err = ls.Neutron.Generate(); err == nil {
				empty = len(l.Neutron.Networks) == 0 && len(l.Neutron.Routers) == 0 && len(l.Neutron.SecurityGroups) == 0
			}
		}
	}
	switch {
	case !loaded:
		return mixplan.Lane{}, fmt.Errorf("lanes.%s is enabled but its scenario was not loaded", name)
	case err != nil:
		return mixplan.Lane{}, fmt.Errorf("lanes.%s: %w", name, err)
	case empty:
		return mixplan.Lane{}, fmt.Errorf("lanes.%s: the scenario plans no resources to churn", name)
	}
	return l, nil
}
