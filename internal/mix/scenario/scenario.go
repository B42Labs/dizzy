// Package scenario defines the YAML scenario format of a combined (mix) run:
// the shared image and flavor, the server envelope, the opt-in services, one
// block per workload persona, and the run-wide chaos settings. Generate expands
// it into a mix plan by dividing the server envelope among the personas and
// building each persona's compute scenario. The same scenario and seed always
// yield a byte-identical plan. Mix gets its own schema so yaml.UnmarshalStrict
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
type Scenario struct {
	Name      string    `yaml:"name"`
	Seed      int64     `yaml:"seed"`
	Image     string    `yaml:"image"`
	Flavor    string    `yaml:"flavor"`
	Services  []string  `yaml:"services"`
	Resources Resources `yaml:"resources"`
	Personas  Personas  `yaml:"personas"`
	Chaos     *Chaos    `yaml:"chaos,omitempty"`
}

// Resources holds the server envelope of the run.
type Resources struct {
	Servers int `yaml:"servers"`
}

// Personas holds one block per workload persona.
type Personas struct {
	CI CI `yaml:"ci"`
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
// actionable error that names the offending field. It then builds the compute
// scenario of every persona with a share above 0 and validates it too.
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

	ci := s.Personas.CI
	if math.IsNaN(ci.Share) || math.IsInf(ci.Share, 0) || ci.Share < 0 {
		return fmt.Errorf("personas.ci.share must be a finite number of at least 0, got %v", ci.Share)
	}
	if ci.Share == 0 {
		return fmt.Errorf("at least one persona must have a share above 0")
	}
	if ci.Interval.Min < 0 {
		return fmt.Errorf("personas.ci.interval.min must not be negative, got %s", time.Duration(ci.Interval.Min))
	}
	if ci.Interval.Min > ci.Interval.Max {
		return fmt.Errorf("personas.ci.interval.min (%s) must not exceed personas.ci.interval.max (%s)",
			time.Duration(ci.Interval.Min), time.Duration(ci.Interval.Max))
	}
	if math.IsNaN(ci.ChurnRatio) || ci.ChurnRatio < 0 || ci.ChurnRatio > 1 {
		return fmt.Errorf("personas.ci.churn_ratio must be between 0 and 1, got %v", ci.ChurnRatio)
	}
	if math.IsNaN(ci.TargetFill) || ci.TargetFill < 0 || ci.TargetFill > 1 {
		return fmt.Errorf("personas.ci.target_fill must be between 0 and 1, got %v", ci.TargetFill)
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

	personas := s.personas()
	servers := Apportion(s.Resources.Servers, shares(personas))
	for i, p := range personas {
		if p.share == 0 {
			continue
		}
		if err := p.nova(servers[i]).Validate(); err != nil {
			return fmt.Errorf("personas.%s: %w", p.name, err)
		}
	}
	return nil
}

// Set applies a single dotted-key override of the form key=value, matching the
// documented scenario fields. services takes a comma-separated list, and the
// empty string clears it; the interval bounds take Go duration strings. It
// returns an error for an unknown key or a value that does not parse to the
// field's type.
func (s *Scenario) Set(key, value string) error {
	ci := &s.Personas.CI
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
	default:
		return fmt.Errorf("unknown override key %q", key)
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
