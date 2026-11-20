// Package scenario defines the human-authored YAML scenario format for
// Horizon (the session count, the identity the sessions log in as, the pages
// they load, how many pages a session loads and how many sessions switch
// project, the seed, and the optional chaos block of the churn mode) and the
// deterministic generator that expands a scenario plus its seed into a
// fully-enumerated plan. The same scenario and seed always yield a
// byte-identical plan. Horizon gets its own scenario schema rather than
// sharing another service's, so yaml.UnmarshalStrict keeps failing loudly on
// a typo in any of them.
package scenario

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

// Scenario is the parametrized description of a desired Horizon workload. It
// is parsed from YAML, validated, optionally overridden via Set, and expanded
// into a plan by Generate. With resources.users 0 every session logs in as
// the user of the clouds.yaml entry and a run creates nothing; with a
// positive count a run creates that many users and resources.projects
// projects in Keystone and grants each user role on each project.
type Scenario struct {
	Name         string       `yaml:"name"`
	Seed         int64        `yaml:"seed"`
	Resources    Resources    `yaml:"resources"`
	Role         string       `yaml:"role"`
	Pages        []Page       `yaml:"pages"`
	Distribution Distribution `yaml:"distribution"`
	// Chaos, when present, configures the random churn/soak mode (the horizon
	// chaos subcommand). It is a pointer so an absent block stays nil and
	// apply and generate ignore it entirely.
	Chaos *Chaos `yaml:"chaos,omitempty"`
}

// Resources holds the fixed counts: the sessions of one apply, which are
// also the envelope of chaos, the users a run creates, and the projects it
// creates when it creates users.
type Resources struct {
	Sessions int `yaml:"sessions"`
	Users    int `yaml:"users"`
	Projects int `yaml:"projects"`
}

// Page is one dashboard page a session may load after its login. Each must
// answer 200.
type Page struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// Distribution holds the per-session range of pages loaded and the fraction
// of sessions that switch project.
type Distribution struct {
	PagesPerSession Range   `yaml:"pages_per_session"`
	SwitchRatio     float64 `yaml:"switch_ratio"`
}

// Range is an inclusive integer interval drawn from during generation.
type Range struct {
	Min int `yaml:"min"`
	Max int `yaml:"max"`
}

// Chaos holds the churn-mode knobs read from a scenario's chaos block. Every
// field has a chaos CLI flag that overrides it; an unset field falls back to
// the command's default. Duration is not required here, since a flag may
// supply it. BrowseRatio is the per-step probability that a live session
// browses: loads its pages and switches project as its plan says. It is a
// pointer so an omitted key falls back to the command's default while an
// explicit browse_ratio: 0 turns browsing off. BucketWidth is the width of
// one time bucket of a run without an end, which only --duration 0 selects.
type Chaos struct {
	Duration    Duration `yaml:"duration"`
	Interval    Interval `yaml:"interval"`
	Parallel    Parallel `yaml:"parallel"`
	ChurnRatio  float64  `yaml:"churn_ratio"`
	TargetFill  float64  `yaml:"target_fill"`
	BrowseRatio *float64 `yaml:"browse_ratio"`
	BucketWidth Duration `yaml:"bucket_width"`
}

// Interval is the random delay range between scheduled churn actions.
type Interval struct {
	Min Duration `yaml:"min"`
	Max Duration `yaml:"max"`
}

// Parallel bounds the fan-out of a churn tick.
type Parallel struct {
	Max int `yaml:"max"`
}

// Duration is a time.Duration that decodes from a Go duration string (e.g.
// "5m", "200ms") under strict YAML, since yaml.v2 has no native duration
// decoding.
type Duration time.Duration

// UnmarshalYAML decodes a Go duration string into a Duration.
func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parsing duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Parse decodes a scenario from YAML. Unknown keys are rejected so that a
// typo in a scenario file fails loudly instead of being silently ignored. It
// does no semantic validation; call Validate for that.
func Parse(data []byte) (Scenario, error) {
	var s Scenario
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return Scenario{}, fmt.Errorf("parsing scenario: %w", err)
	}
	return s, nil
}

// maxCount caps every count, bounding the plan's allocations as the other
// services' scenarios do.
const maxCount = 1_000_000

// pageName is what a page name must match: it becomes the metric kind
// page_<name>, so it stays short and lowercase.
var pageName = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// Validate checks the scenario for semantic consistency, returning an
// actionable error that names the offending key. With resources.users 0,
// resources.projects and role are ignored and not checked.
func (s Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name must not be empty")
	}

	r := s.Resources
	if r.Sessions < 1 {
		return fmt.Errorf("resources.sessions must be at least 1, got %d", r.Sessions)
	}
	for _, c := range []struct {
		key   string
		count int
	}{
		{"resources.sessions", r.Sessions},
		{"resources.users", r.Users},
		{"resources.projects", r.Projects},
	} {
		if c.count < 0 {
			return fmt.Errorf("%s must not be negative, got %d", c.key, c.count)
		}
		if c.count > maxCount {
			return fmt.Errorf("%s exceeds the limit of %d, got %d", c.key, maxCount, c.count)
		}
	}
	if r.Users > r.Sessions {
		return fmt.Errorf("resources.users (%d) exceeds resources.sessions (%d)", r.Users, r.Sessions)
	}
	if r.Users > 0 {
		if r.Projects < 1 {
			return fmt.Errorf("resources.projects must be at least 1 when resources.users > 0, got %d", r.Projects)
		}
		if s.Role == "" {
			return fmt.Errorf("role must not be empty when resources.users > 0")
		}
	}

	if len(s.Pages) == 0 {
		return fmt.Errorf("pages must list at least one page")
	}
	seen := make(map[string]bool, len(s.Pages))
	for i, pg := range s.Pages {
		if !pageName.MatchString(pg.Name) {
			return fmt.Errorf("pages[%d].name %q must match %s", i, pg.Name, pageName)
		}
		if seen[pg.Name] {
			return fmt.Errorf("pages[%d].name %q repeats an earlier page", i, pg.Name)
		}
		seen[pg.Name] = true
		if err := validatePath(pg.Path); err != nil {
			return fmt.Errorf("pages[%d].path %q %w", i, pg.Path, err)
		}
	}

	pps := s.Distribution.PagesPerSession
	if pps.Min < 0 {
		return fmt.Errorf("distribution.pages_per_session.min must not be negative, got %d", pps.Min)
	}
	if pps.Min > pps.Max {
		return fmt.Errorf("distribution.pages_per_session.min (%d) must not exceed distribution.pages_per_session.max (%d)", pps.Min, pps.Max)
	}
	if pps.Max > len(s.Pages) {
		return fmt.Errorf("distribution.pages_per_session.max (%d) exceeds the number of pages (%d)", pps.Max, len(s.Pages))
	}
	if err := checkRatio("distribution.switch_ratio", s.Distribution.SwitchRatio); err != nil {
		return err
	}

	return s.Chaos.validate()
}

// validatePath checks a page path: it starts with one slash, lies outside
// /auth/, whose views the session drives itself, and carries no query,
// fragment or whitespace. The returned error completes "pages[i].path ...".
func validatePath(p string) error {
	switch {
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("must start with /")
	case strings.HasPrefix(p, "//"):
		return fmt.Errorf("must not start with //")
	case strings.HasPrefix(p, "/auth/"):
		return fmt.Errorf("must not lie under /auth/")
	case strings.ContainsAny(p, "?#"):
		return fmt.Errorf("must not contain ? or #")
	case strings.IndexFunc(p, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' }) >= 0:
		return fmt.Errorf("must not contain whitespace")
	}
	return nil
}

// checkRatio rejects a ratio outside 0 to 1, NaN included.
func checkRatio(key string, v float64) error {
	if math.IsNaN(v) || v < 0 || v > 1 {
		return fmt.Errorf("%s must be between 0 and 1, got %v", key, v)
	}
	return nil
}

// validate checks the chaos block. A nil receiver (no chaos block) is valid.
func (c *Chaos) validate() error {
	if c == nil {
		return nil
	}
	if c.Duration < 0 {
		return fmt.Errorf("chaos.duration must not be negative, got %s", time.Duration(c.Duration))
	}
	if c.BucketWidth < 0 {
		return fmt.Errorf("chaos.bucket_width must not be negative, got %s", time.Duration(c.BucketWidth))
	}
	if c.Interval.Min < 0 {
		return fmt.Errorf("chaos.interval.min must not be negative, got %s", time.Duration(c.Interval.Min))
	}
	if c.Interval.Min > c.Interval.Max {
		return fmt.Errorf("chaos.interval.min (%s) must not exceed chaos.interval.max (%s)", time.Duration(c.Interval.Min), time.Duration(c.Interval.Max))
	}
	if c.Parallel.Max < 0 {
		return fmt.Errorf("chaos.parallel.max must not be negative, got %d", c.Parallel.Max)
	}
	if err := checkRatio("chaos.churn_ratio", c.ChurnRatio); err != nil {
		return err
	}
	if err := checkRatio("chaos.target_fill", c.TargetFill); err != nil {
		return err
	}
	if c.BrowseRatio != nil {
		if err := checkRatio("chaos.browse_ratio", *c.BrowseRatio); err != nil {
			return err
		}
	}
	return nil
}

// Set applies a single dotted-key override of the form key=value. It returns
// an error for an unknown key or a value that does not parse to the field's
// type. pages is a list and has no override key.
func (s *Scenario) Set(key, value string) error {
	switch key {
	case "seed":
		return setInt64(&s.Seed, key, value)
	case "resources.sessions":
		return setInt(&s.Resources.Sessions, key, value)
	case "resources.users":
		return setInt(&s.Resources.Users, key, value)
	case "resources.projects":
		return setInt(&s.Resources.Projects, key, value)
	case "role":
		s.Role = strings.TrimSpace(value)
		return nil
	case "distribution.pages_per_session.min":
		return setInt(&s.Distribution.PagesPerSession.Min, key, value)
	case "distribution.pages_per_session.max":
		return setInt(&s.Distribution.PagesPerSession.Max, key, value)
	case "distribution.switch_ratio":
		return setFloat(&s.Distribution.SwitchRatio, key, value)
	default:
		return fmt.Errorf("unknown override key %q", key)
	}
}

// setInt parses value as an int into dst, wrapping a parse failure with the
// key.
func setInt(dst *int, key, value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("override %s: %q is not an integer", key, value)
	}
	*dst = n
	return nil
}

// setInt64 parses value as an int64 into dst, wrapping a parse failure with
// the key.
func setInt64(dst *int64, key, value string) error {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return fmt.Errorf("override %s: %q is not an integer", key, value)
	}
	*dst = n
	return nil
}

// setFloat parses value as a float64 into dst, wrapping a parse failure with
// the key.
func setFloat(dst *float64, key, value string) error {
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return fmt.Errorf("override %s: %q is not a number", key, value)
	}
	*dst = f
	return nil
}
