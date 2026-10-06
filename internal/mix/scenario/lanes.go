package scenario

import (
	"fmt"
	"strings"
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
