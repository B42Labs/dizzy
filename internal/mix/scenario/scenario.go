// Package scenario defines the YAML scenario format of a combined (mix) run:
// the shared image and flavor, the server envelope, the opt-in services, one
// block per workload persona, one block per background lane, and the run-wide
// chaos settings. Generate expands it into a mix plan by dividing the server
// envelope among the personas and building each persona's compute scenario;
// it also expands the scenario of every enabled lane, which
// LoadLanes reads. The same scenario and seed always yield a byte-identical
// plan. Mix gets its own schema so yaml.UnmarshalStrict
// keeps failing loudly on a typo, and reuses the Nova package's range,
// interval, parallel and duration types.
package scenario

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"

	novascenario "github.com/B42Labs/dizzy/internal/nova/scenario"
)

// Scenario is the parametrized description of a combined run. Image and Flavor
// name resources that must already exist on the target cloud and are shared by
// every persona. Resources.Servers is the server envelope the personas divide
// by their shares. Services lists the opt-in services bound to every persona.
// Lanes switches on the single-service churns that run next to the personas.
type Scenario struct {
	Name      string    `yaml:"name"`
	Seed      int64     `yaml:"seed"`
	Image     string    `yaml:"image"`
	Flavor    string    `yaml:"flavor"`
	Services  []string  `yaml:"services"`
	Resources Resources `yaml:"resources"`
	Personas  Personas  `yaml:"personas"`
	Lanes     Lanes     `yaml:"lanes"`
	Chaos     *Chaos    `yaml:"chaos,omitempty"`
}

// Resources holds the server envelope of the run.
type Resources struct {
	Servers int `yaml:"servers"`
}

// Personas holds one block per workload persona.
type Personas struct {
	CI       CI       `yaml:"ci"`
	Gardener Gardener `yaml:"gardener"`
	Legacy   Legacy   `yaml:"legacy"`
}

// CI is the CI persona: short-lived servers on one network each, with zero or
// more data volumes and no lifecycle operation. Share is its share of the
// server envelope and Cloud the clouds.yaml entry it authenticates with, empty
// for the --os-cloud fallback. Networks is the number of networks its servers
// are spread over. Interval, ChurnRatio and TargetFill configure its churn
// engine; a zero value falls back to the command's default.
type CI struct {
	Share            float64               `yaml:"share"`
	Cloud            string                `yaml:"cloud"`
	Networks         int                   `yaml:"networks"`
	VolumesPerServer novascenario.Range    `yaml:"volumes_per_server"`
	VolumeGiB        novascenario.Range    `yaml:"volume_gib"`
	Interval         novascenario.Interval `yaml:"interval"`
	ChurnRatio       float64               `yaml:"churn_ratio"`
	TargetFill       float64               `yaml:"target_fill"`
}

// Gardener is the Gardener persona: Kubernetes clusters as Gardener keeps them
// on OpenStack. A cluster is one server group, one network, and the worker
// servers booted into that group on that network, each with one data volume.
// Once a cluster is complete, its workers are replaced one at a time: a worker
// is deleted with its volume and booted again under the same name. Share and
// Cloud mean what they mean for the CI persona. Clusters is the number of
// clusters the persona's servers are spread over, round-robin, and Policy the
// policy of every cluster's server group, "anti-affinity" or
// "soft-anti-affinity". VolumeGiB is the size range of the data volumes.
// Interval configures the persona's churn engine; a zero bound falls back to
// the command's default. The persona keeps its clusters whole, so it has no
// churn ratio or target fill.
type Gardener struct {
	Share     float64               `yaml:"share"`
	Cloud     string                `yaml:"cloud"`
	Clusters  int                   `yaml:"clusters"`
	Policy    string                `yaml:"policy"`
	VolumeGiB novascenario.Range    `yaml:"volume_gib"`
	Interval  novascenario.Interval `yaml:"interval"`
}

// Legacy is the Legacy persona: long-lived servers on one network each that
// stay until the run's teardown and are changed in place again and again, by a
// stop and start, a resize, a live or a cold migration, or the detach or
// re-attach of one of their data volumes or ports. Share, Cloud and Networks
// mean what they mean for the CI persona. ResizeFlavor is the second flavor
// the servers alternate with; empty, the servers are never resized.
// ColdMigration set to false turns cold migration off and leaves live
// migration to the migration pre-check; nil or true keeps it on.
// VolumesPerServer and VolumeGiB shape the data volumes and PortsPerServer the
// extra ports. Interval configures the persona's churn engine; a zero bound
// falls back to the command's default. The persona keeps every planned
// resource and mutates on every step, so it has no churn ratio or target fill.
type Legacy struct {
	Share            float64               `yaml:"share"`
	Cloud            string                `yaml:"cloud"`
	Networks         int                   `yaml:"networks"`
	ResizeFlavor     string                `yaml:"resize_flavor"`
	ColdMigration    *bool                 `yaml:"cold_migration"`
	VolumesPerServer novascenario.Range    `yaml:"volumes_per_server"`
	VolumeGiB        novascenario.Range    `yaml:"volume_gib"`
	PortsPerServer   novascenario.Range    `yaml:"ports_per_server"`
	Interval         novascenario.Interval `yaml:"interval"`
}

// ColdMigrates reports whether the persona's servers are cold-migrated: true
// unless the block sets cold_migration to false.
func (l Legacy) ColdMigrates() bool { return l.ColdMigration == nil || *l.ColdMigration }

// Lanes holds one block per background lane: the churn of a single service
// that the service's own chaos command runs, in a churn engine of its own next
// to the personas. Every lane is off unless its block enables it.
type Lanes struct {
	Cinder   CinderLane   `yaml:"cinder"`
	Glance   Lane         `yaml:"glance"`
	Keystone KeystoneLane `yaml:"keystone"`
	Neutron  NeutronLane  `yaml:"neutron"`
}

// Lane holds the keys every lane has. Enabled switches the lane on. Cloud is
// the clouds.yaml entry the lane authenticates with, empty for the --os-cloud
// fallback. An enabled lane names its scenario in exactly one way: Profile is
// a bundled profile of the service (small, medium or large), and Scenario the
// path of a scenario file of the service, relative to the working directory.
type Lane struct {
	Enabled  bool   `yaml:"enabled"`
	Cloud    string `yaml:"cloud"`
	Profile  string `yaml:"profile"`
	Scenario string `yaml:"scenario"`
}

// CinderLane is the Cinder lane, volume and snapshot churn. VolumeType names
// the volume type its volumes are created with, empty for the cloud's default
// type.
type CinderLane struct {
	Lane       `yaml:",inline"`
	VolumeType string `yaml:"volume_type"`
}

// KeystoneLane is the Keystone lane, project, user and role assignment churn.
// Privilege selects the privilege tier, auto, admin or domain-manager, and
// empty means auto. Domain and Roles bind the domain-manager tier: the
// in-scope domain, empty for the domain the token is scoped to, and the
// existing roles to reuse, comma-separated, empty for member,reader.
type KeystoneLane struct {
	Lane      `yaml:",inline"`
	Privilege string `yaml:"privilege"`
	Domain    string `yaml:"domain"`
	Roles     string `yaml:"roles"`
}

// NeutronLane is the Neutron lane, network topology churn. ExternalNetwork
// names the external network for gateways and floating IPs, empty to detect
// the first one.
type NeutronLane struct {
	Lane            `yaml:",inline"`
	ExternalNetwork string `yaml:"external_network"`
}

// Chaos holds the run-wide churn settings, applied to every persona's engine.
// A zero field falls back to the command's default, and a flag overrides it.
type Chaos struct {
	Duration    novascenario.Duration `yaml:"duration"`
	Parallel    novascenario.Parallel `yaml:"parallel"`
	BucketWidth novascenario.Duration `yaml:"bucket_width"`
}

// maxServers caps the server envelope, the same cap the Nova scenario puts on
// its server count.
const maxServers = 1_000_000

// Parse decodes a scenario from YAML. Unknown keys are rejected so that a typo
// in a scenario file fails loudly instead of being silently ignored. It does no
// semantic validation; call Validate for that.
func Parse(data []byte) (Scenario, error) {
	var s Scenario
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return Scenario{}, fmt.Errorf("parsing scenario: %w", err)
	}
	return s, nil
}

// Validate checks the scenario for semantic consistency, returning an
// actionable error that names the offending field. It checks every enabled
// lane, in canonical order. It then runs the persona-specific checks of every
// persona with a share above 0, builds its compute scenario and validates it
// too.
func (s Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name must not be empty")
	}
	if s.Resources.Servers < 0 {
		return fmt.Errorf("resources.servers must not be negative, got %d", s.Resources.Servers)
	}
	if s.Resources.Servers > maxServers {
		return fmt.Errorf("resources.servers exceeds the limit of %d, got %d", maxServers, s.Resources.Servers)
	}

	personas := s.personas()
	active := false
	for _, p := range personas {
		if math.IsNaN(p.share) || math.IsInf(p.share, 0) || p.share < 0 {
			return fmt.Errorf("personas.%s.share must be a finite number of at least 0, got %v", p.name, p.share)
		}
		active = active || p.share > 0
	}
	if !active {
		return fmt.Errorf("at least one persona must have a share above 0")
	}

	ci := s.Personas.CI
	if err := validateInterval("personas.ci.interval", ci.Interval); err != nil {
		return err
	}
	if math.IsNaN(ci.ChurnRatio) || ci.ChurnRatio < 0 || ci.ChurnRatio > 1 {
		return fmt.Errorf("personas.ci.churn_ratio must be between 0 and 1, got %v", ci.ChurnRatio)
	}
	if math.IsNaN(ci.TargetFill) || ci.TargetFill < 0 || ci.TargetFill > 1 {
		return fmt.Errorf("personas.ci.target_fill must be between 0 and 1, got %v", ci.TargetFill)
	}
	if err := validateInterval("personas.gardener.interval", s.Personas.Gardener.Interval); err != nil {
		return err
	}
	if err := validateInterval("personas.legacy.interval", s.Personas.Legacy.Interval); err != nil {
		return err
	}

	if c := s.Chaos; c != nil {
		if c.Duration < 0 {
			return fmt.Errorf("chaos.duration must not be negative, got %s", time.Duration(c.Duration))
		}
		if c.BucketWidth < 0 {
			return fmt.Errorf("chaos.bucket_width must not be negative, got %s", time.Duration(c.BucketWidth))
		}
		if c.Parallel.Max < 0 {
			return fmt.Errorf("chaos.parallel.max must not be negative, got %d", c.Parallel.Max)
		}
	}

	seen := make(map[string]bool, len(s.Services))
	for i, name := range s.Services {
		if name == "" {
			return fmt.Errorf("services[%d] must not be empty", i)
		}
		if seen[name] {
			return fmt.Errorf("services lists %q twice", name)
		}
		seen[name] = true
	}

	if err := s.Lanes.validate(); err != nil {
		return err
	}

	servers := Apportion(s.Resources.Servers, shares(personas))
	for i, p := range personas {
		if p.share == 0 {
			continue
		}
		if p.check != nil {
			if err := p.check(servers[i]); err != nil {
				return err
			}
		}
		if err := p.nova(servers[i]).Validate(); err != nil {
			return fmt.Errorf("personas.%s: %w", p.name, err)
		}
	}
	return nil
}

// validateInterval checks a persona's interval block, naming it by key: its
// lower bound must not be negative or exceed its upper bound.
func validateInterval(key string, iv novascenario.Interval) error {
	if iv.Min < 0 {
		return fmt.Errorf("%s.min must not be negative, got %s", key, time.Duration(iv.Min))
	}
	if iv.Min > iv.Max {
		return fmt.Errorf("%s.min (%s) must not exceed %s.max (%s)", key, time.Duration(iv.Min), key, time.Duration(iv.Max))
	}
	return nil
}

// Set applies a single dotted-key override of the form key=value, matching the
// documented scenario fields. services takes a comma-separated list, and the
// empty string clears it; the interval bounds take Go duration strings, and
// lanes.<name>.enabled a boolean. It returns an error for an unknown key or a
// value that does not parse to the field's type.
func (s *Scenario) Set(key, value string) error {
	ci, gardener, legacy := &s.Personas.CI, &s.Personas.Gardener, &s.Personas.Legacy
	switch key {
	case "seed":
		return setInt64(&s.Seed, key, value)
	case "image":
		s.Image = value
		return nil
	case "flavor":
		s.Flavor = value
		return nil
	case "resources.servers":
		return setInt(&s.Resources.Servers, key, value)
	case "services":
		s.Services = nil
		if strings.TrimSpace(value) == "" {
			return nil
		}
		for _, name := range strings.Split(value, ",") {
			s.Services = append(s.Services, strings.TrimSpace(name))
		}
		return nil
	case "personas.ci.share":
		return setFloat(&ci.Share, key, value)
	case "personas.ci.cloud":
		ci.Cloud = value
		return nil
	case "personas.ci.networks":
		return setInt(&ci.Networks, key, value)
	case "personas.ci.volumes_per_server.min":
		return setInt(&ci.VolumesPerServer.Min, key, value)
	case "personas.ci.volumes_per_server.max":
		return setInt(&ci.VolumesPerServer.Max, key, value)
	case "personas.ci.volume_gib.min":
		return setInt(&ci.VolumeGiB.Min, key, value)
	case "personas.ci.volume_gib.max":
		return setInt(&ci.VolumeGiB.Max, key, value)
	case "personas.ci.interval.min":
		return setDuration(&ci.Interval.Min, key, value)
	case "personas.ci.interval.max":
		return setDuration(&ci.Interval.Max, key, value)
	case "personas.ci.churn_ratio":
		return setFloat(&ci.ChurnRatio, key, value)
	case "personas.ci.target_fill":
		return setFloat(&ci.TargetFill, key, value)
	case "personas.gardener.share":
		return setFloat(&gardener.Share, key, value)
	case "personas.gardener.cloud":
		gardener.Cloud = value
		return nil
	case "personas.gardener.clusters":
		return setInt(&gardener.Clusters, key, value)
	case "personas.gardener.policy":
		gardener.Policy = value
		return nil
	case "personas.gardener.volume_gib.min":
		return setInt(&gardener.VolumeGiB.Min, key, value)
	case "personas.gardener.volume_gib.max":
		return setInt(&gardener.VolumeGiB.Max, key, value)
	case "personas.gardener.interval.min":
		return setDuration(&gardener.Interval.Min, key, value)
	case "personas.gardener.interval.max":
		return setDuration(&gardener.Interval.Max, key, value)
	case "personas.legacy.share":
		return setFloat(&legacy.Share, key, value)
	case "personas.legacy.cloud":
		legacy.Cloud = value
		return nil
	case "personas.legacy.networks":
		return setInt(&legacy.Networks, key, value)
	case "personas.legacy.resize_flavor":
		legacy.ResizeFlavor = value
		return nil
	case "personas.legacy.volumes_per_server.min":
		return setInt(&legacy.VolumesPerServer.Min, key, value)
	case "personas.legacy.volumes_per_server.max":
		return setInt(&legacy.VolumesPerServer.Max, key, value)
	case "personas.legacy.volume_gib.min":
		return setInt(&legacy.VolumeGiB.Min, key, value)
	case "personas.legacy.volume_gib.max":
		return setInt(&legacy.VolumeGiB.Max, key, value)
	case "personas.legacy.ports_per_server.min":
		return setInt(&legacy.PortsPerServer.Min, key, value)
	case "personas.legacy.ports_per_server.max":
		return setInt(&legacy.PortsPerServer.Max, key, value)
	case "personas.legacy.interval.min":
		return setDuration(&legacy.Interval.Min, key, value)
	case "personas.legacy.interval.max":
		return setDuration(&legacy.Interval.Max, key, value)
	default:
		return s.Lanes.set(key, value)
	}
}

// setInt parses value as an int into dst, wrapping a parse failure with the key.
func setInt(dst *int, key, value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("override %s: %q is not an integer", key, value)
	}
	*dst = n
	return nil
}

// setInt64 parses value as an int64 into dst, wrapping a parse failure with the
// key.
func setInt64(dst *int64, key, value string) error {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return fmt.Errorf("override %s: %q is not an integer", key, value)
	}
	*dst = n
	return nil
}

// setFloat parses value as a float64 into dst, wrapping a parse failure with the
// key.
func setFloat(dst *float64, key, value string) error {
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return fmt.Errorf("override %s: %q is not a number", key, value)
	}
	*dst = f
	return nil
}

// setBool parses value as a boolean into dst, wrapping a parse failure with the
// key.
func setBool(dst *bool, key, value string) error {
	b, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("override %s: %q is not a boolean", key, value)
	}
	*dst = b
	return nil
}

// setDuration parses value as a Go duration string into dst, wrapping a parse
// failure with the key.
func setDuration(dst *novascenario.Duration, key, value string) error {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("override %s: %q is not a duration", key, value)
	}
	*dst = novascenario.Duration(d)
	return nil
}
