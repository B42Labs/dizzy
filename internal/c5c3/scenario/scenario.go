// Package scenario defines the human-authored YAML scenario format for c5c3
// (the Kubernetes namespace the orders are created in, the ControlPlane they
// reference, the role to assign and the role the operator must refuse, the
// catalog entry to register, the credential grace period, the bounds of the
// convergence waits, and the Secret repair switch) and the generator that
// turns a scenario into a plan.
//
// A c5c3 scenario has no seed and no counts: every plan holds the same five
// orders and two refusals, and only the scenario's values fill them, so the
// same scenario always yields a byte-identical plan. c5c3 gets its own
// scenario schema rather than sharing another service's, so that
// yaml.UnmarshalStrict fails loudly on a key borrowed from another service,
// seed among them.
package scenario

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v2"

	"github.com/B42Labs/dizzy/internal/c5c3/plan"
)

// Scenario is the description of one c5c3 run. It is parsed from YAML,
// validated, optionally overridden via Set, and turned into a plan by
// Generate. Parse gives every key a file omits its default; a Scenario built
// in Go starts from the zero value and gets no defaults.
type Scenario struct {
	// Name names the scenario and becomes the plan's scenario field.
	Name string `yaml:"name"`
	// Namespace is the Kubernetes namespace the orders are created in. A
	// ControlPlane must assign it.
	Namespace string `yaml:"namespace"`
	// ControlPlane names the ControlPlane every order references.
	ControlPlane ControlPlane `yaml:"control_plane"`
	// Role is the role the role assignment order grants on the project. It
	// must be on the allowedRoles of the namespace's assignment entry.
	Role string `yaml:"role"`
	// RefusedRole is the role of the assignment the operator must refuse with
	// RoleNotAllowed, so it must not be on allowedRoles. The default,
	// dizzy-refused, is a role Keystone does not have, so a ControlPlane that
	// wrongly admits it grants nothing.
	RefusedRole string `yaml:"refused_role"`
	// Catalog is the catalog entry the catalog entry order registers.
	Catalog Catalog `yaml:"catalog"`
	// Rotation configures the application credential order's rotation.
	Rotation Rotation `yaml:"rotation"`
	// Timeouts bound the waits for the operator.
	Timeouts Timeouts `yaml:"timeouts"`
	// Checks switches optional checks on and off.
	Checks Checks `yaml:"checks"`
}

// ControlPlane names the ControlPlane the orders reference. An empty
// Namespace means the orders' own namespace.
type ControlPlane struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// Catalog is the catalog entry dizzy registers: an OpenStack service type and
// the URL of its public endpoint. The bundled profile uses a type no
// OpenStack client resolves and a host under .invalid, so the entry is never
// used.
type Catalog struct {
	ServiceType string `yaml:"service_type"`
	EndpointURL string `yaml:"endpoint_url"`
}

// Rotation holds the rotation settings of the application credential order.
// GracePeriod becomes its spec.rotation.gracePeriod: how long the superseded
// credential stays in Keystone after the delivered Secret switched to its
// successor.
type Rotation struct {
	GracePeriod Duration `yaml:"grace_period"`
}

// Timeouts bound the waits for the operator. Ready bounds the create,
// rotation, repair and refusal waits; Gone bounds the wait for a deleted
// order to disappear. Neither bounds a single request, which --timeout does.
type Timeouts struct {
	Ready Duration `yaml:"ready"`
	Gone  Duration `yaml:"gone"`
}

// Checks switches optional checks on and off. SecretRepair is a pointer so
// that an absent key (nil, which means on) differs from an explicit false. The
// Secret repair check edits and deletes the delivered Secrets and needs patch
// and delete on Secrets; false turns it off for a deployment that grants only
// get.
type Checks struct {
	SecretRepair *bool `yaml:"secret_repair"`
}

// secretRepair resolves the switch: nil means on.
func (c Checks) secretRepair() bool {
	return c.SecretRepair == nil || *c.SecretRepair
}

// Duration is a time.Duration that decodes from a Go duration string (e.g.
// "15m", "10s") under strict YAML, since yaml.v2 has no native duration
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

// The defaults of the keys a scenario file may omit. control_plane.namespace
// defaults to empty and checks.secret_repair to on, which their zero values
// already express.
const (
	defaultRefusedRole  = "dizzy-refused"
	defaultGracePeriod  = 2 * time.Minute
	defaultReadyTimeout = 15 * time.Minute
	defaultGoneTimeout  = 15 * time.Minute
)

// defaults returns the scenario Parse decodes a file into, holding the
// default of every key that has one. yaml.v2 decodes a block into the struct
// already in place and sets only the keys the block names, so a block that
// names some of its keys keeps the defaults of the others, and a key written
// explicitly, 0s included, replaces its default. A block header without
// children is the exception Parse handles: yaml.v2 decodes it as null, which
// zeroes the struct.
func defaults() Scenario {
	return Scenario{
		RefusedRole: defaultRefusedRole,
		Rotation:    Rotation{GracePeriod: Duration(defaultGracePeriod)},
		Timeouts: Timeouts{
			Ready: Duration(defaultReadyTimeout),
			Gone:  Duration(defaultGoneTimeout),
		},
	}
}

// Parse decodes a scenario from YAML, starting from the defaults. Unknown
// keys are rejected so that a typo in a scenario file fails loudly instead of
// being silently ignored; seed is one of them, since a c5c3 plan draws
// nothing. A block header written without children, such as "rotation:"
// with every key commented out, names none of its keys and keeps their
// defaults, as an absent block does. It does no semantic validation; call
// Validate for that.
func Parse(data []byte) (Scenario, error) {
	d := defaults()
	s := d
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return Scenario{}, fmt.Errorf("parsing scenario: %w", err)
	}

	// yaml.v2 decodes a block header without children as null and null into
	// a struct as its zero value, so the strict decode above cannot tell such
	// a block from one whose keys are all written as zero. A second decode
	// into untyped fields can: a header without children, like an absent
	// block, leaves its field nil, and a block with keys, or {}, does not.
	// checks needs no such step, since its zero value is its default.
	var blocks struct {
		Rotation interface{} `yaml:"rotation"`
		Timeouts interface{} `yaml:"timeouts"`
	}
	if err := yaml.Unmarshal(data, &blocks); err != nil {
		return Scenario{}, fmt.Errorf("parsing scenario: %w", err)
	}
	if blocks.Rotation == nil {
		s.Rotation = d.Rotation
	}
	if blocks.Timeouts == nil {
		s.Timeouts = d.Timeouts
	}
	return s, nil
}

// dnsLabel is the shape of a DNS-1123 label, which Kubernetes requires of a
// namespace and the c5c3-operator's KeystoneCatalogEntry of a service type.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const (
	// maxLabelLength is the longest DNS-1123 label.
	maxLabelLength = 63
	// maxRoleLength is the longest role name the c5c3-operator accepts, the
	// cap of K-ORC's KeystoneName.
	maxRoleLength = 255
	// maxEndpointURLLength is the longest catalog endpoint URL the
	// c5c3-operator's KeystoneCatalogEntry accepts, the cap of K-ORC's
	// EndpointResourceSpec.URL.
	maxEndpointURLLength = 1024
	// minGracePeriod is the shortest grace period dizzy accepts. The
	// application credential stage logs in with the superseded credential
	// only while its deletion is more than 5 seconds away, so a shorter grace
	// period would leave that login no room.
	minGracePeriod = 10 * time.Second
	// maxGracePeriod is the default rotation interval of an application
	// credential order. The CRD requires the grace period to be shorter than
	// the interval, so the grace period must stay below it.
	maxGracePeriod = 720 * time.Hour
)

// Validate checks the scenario for semantic consistency, returning an
// actionable error that names the offending key. It returns the first problem
// it finds.
func (s Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name must not be empty")
	}
	if err := validateLabel("namespace", s.Namespace); err != nil {
		return err
	}
	if s.ControlPlane.Name == "" {
		return fmt.Errorf("control_plane.name must not be empty")
	}
	if s.ControlPlane.Namespace != "" {
		if err := validateLabel("control_plane.namespace", s.ControlPlane.Namespace); err != nil {
			return err
		}
	}

	if err := validateRole("role", s.Role); err != nil {
		return err
	}
	if err := validateRole("refused_role", s.RefusedRole); err != nil {
		return err
	}
	// The refusal must fail on the allowlist, so it cannot ask for the role
	// the run is granted.
	if s.Role == s.RefusedRole {
		return fmt.Errorf("refused_role must differ from role, both are %q", s.Role)
	}

	if err := validateLabel("catalog.service_type", s.Catalog.ServiceType); err != nil {
		return err
	}
	if s.Catalog.ServiceType == "identity" {
		return fmt.Errorf("catalog.service_type must not be identity, whose catalog entry the ControlPlane owns")
	}
	if err := validateEndpointURL("catalog.endpoint_url", s.Catalog.EndpointURL); err != nil {
		return err
	}

	if g := time.Duration(s.Rotation.GracePeriod); g < minGracePeriod || g >= maxGracePeriod {
		return fmt.Errorf("rotation.grace_period must be at least 10s and below 720h, got %s", g)
	}
	if d := time.Duration(s.Timeouts.Ready); d <= 0 {
		return fmt.Errorf("timeouts.ready must be positive, got %s", d)
	}
	if d := time.Duration(s.Timeouts.Gone); d <= 0 {
		return fmt.Errorf("timeouts.gone must be positive, got %s", d)
	}
	return nil
}

// validateLabel checks that value, the value of key, is a non-empty DNS-1123
// label.
func validateLabel(key, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", key)
	}
	if len(value) > maxLabelLength {
		return fmt.Errorf("%s must be at most %d characters, got %d", key, maxLabelLength, len(value))
	}
	if !dnsLabel.MatchString(value) {
		return fmt.Errorf("%s must be a DNS-1123 label (lowercase letters, digits and '-', starting and ending with a letter or digit), got %q", key, value)
	}
	return nil
}

// validateRole checks that value, the value of key, is a role name the
// c5c3-operator accepts: not empty, without a comma, and at most
// maxRoleLength characters.
func validateRole(key, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", key)
	}
	if strings.Contains(value, ",") {
		return fmt.Errorf("%s must not contain a comma, got %q", key, value)
	}
	if n := utf8.RuneCountInString(value); n > maxRoleLength {
		return fmt.Errorf("%s must be at most %d characters, got %d", key, maxRoleLength, n)
	}
	return nil
}

// validateEndpointURL checks that value, the value of key, is an endpoint URL
// the c5c3-operator's KeystoneCatalogEntry admits, whose CRD gives it the
// pattern ^https?://[^\s/]+ and a cap of maxEndpointURLLength characters: at
// most that long, with the http or https scheme written in lowercase, and
// naming a host. An error quotes the URL with any password in it redacted.
func validateEndpointURL(key, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", key)
	}
	// The API server counts the characters of a string in runes.
	if n := utf8.RuneCountInString(value); n > maxEndpointURLLength {
		return fmt.Errorf("%s must be at most %d characters, got %d", key, maxEndpointURLLength, n)
	}
	u, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%s is not a URL: %w", key, err)
	}
	// url.Parse lowercases the scheme, and the CRD's pattern admits only a
	// lowercase one, so the scheme is read as written: url.Parse takes it
	// from the start of value and keeps its length. It holds no password.
	if scheme := value[:len(u.Scheme)]; scheme != "http" && scheme != "https" {
		return fmt.Errorf("%s must use the http or https scheme, in lowercase, got %q", key, scheme)
	}
	// url.Parse reads a host only after "//", so a URL that names one
	// starts with "http://" or "https://", as the pattern requires.
	if u.Hostname() == "" {
		return fmt.Errorf("%s must name a host, got %q", key, u.Redacted())
	}
	return nil
}

// Set applies a single dotted-key override of the form key=value, matching
// the documented scenario keys. name is not among them: it names the profile
// the plan came from. Durations parse with time.ParseDuration and the Secret
// repair switch with strconv.ParseBool; every other key takes the value as
// written, and Validate checks it. It returns an error for an unknown key or
// a value that does not parse, and leaves the scenario unchanged on an error.
func (s *Scenario) Set(key, value string) error {
	switch key {
	case "namespace":
		s.Namespace = value
	case "control_plane.name":
		s.ControlPlane.Name = value
	case "control_plane.namespace":
		s.ControlPlane.Namespace = value
	case "role":
		s.Role = value
	case "refused_role":
		s.RefusedRole = value
	case "catalog.service_type":
		s.Catalog.ServiceType = value
	case "catalog.endpoint_url":
		s.Catalog.EndpointURL = value
	case "rotation.grace_period":
		return setDuration(&s.Rotation.GracePeriod, key, value)
	case "timeouts.ready":
		return setDuration(&s.Timeouts.Ready, key, value)
	case "timeouts.gone":
		return setDuration(&s.Timeouts.Gone, key, value)
	case "checks.secret_repair":
		return setBool(&s.Checks.SecretRepair, key, value)
	default:
		return fmt.Errorf("unknown override key %q", key)
	}
	return nil
}

// setDuration parses value as a Go duration into dst, wrapping a parse
// failure with the key.
func setDuration(dst *Duration, key, value string) error {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("override %s: %q is not a duration", key, value)
	}
	*dst = Duration(d)
	return nil
}

// setBool parses value as a boolean into dst, wrapping a parse failure with
// the key.
func setBool(dst **bool, key, value string) error {
	b, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("override %s: %q is not a boolean", key, value)
	}
	*dst = &b
	return nil
}

// Generate turns the scenario into its plan. The plan always has the same
// shape, and only the scenario's values fill it, so the same scenario always
// yields a byte-identical plan: no seed, no randomness. The returned plan is
// validated before it is handed back.
//
// The orders, in creation order, are the user (user), the project (project),
// the role assignment of role on the project (role), the catalog entry
// (catalog), and the application credential of the user on the project
// (appcred). The refusals are an application credential created before the
// role is assigned, which the operator must refuse with NoRoleOnProject
// (refused-appcred), and a role assignment of refused_role, which it must
// refuse with RoleNotAllowed (refused-role).
func (s Scenario) Generate() (*plan.Plan, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid scenario: %w", err)
	}

	p := &plan.Plan{
		Scenario:  s.Name,
		Namespace: s.Namespace,
		ControlPlane: plan.ControlPlane{
			Name:      s.ControlPlane.Name,
			Namespace: s.ControlPlane.Namespace,
		},
		Orders: []plan.Order{
			{Kind: plan.KindUser, Name: "user"},
			{Kind: plan.KindProject, Name: "project"},
			{Kind: plan.KindRoleAssignment, Name: "role", User: "user", Project: "project", Role: s.Role},
			{Kind: plan.KindCatalogEntry, Name: "catalog", ServiceType: s.Catalog.ServiceType, EndpointURL: s.Catalog.EndpointURL},
			{Kind: plan.KindApplicationCredential, Name: "appcred", User: "user", Project: "project"},
		},
		Refusals: []plan.Refusal{
			{
				Order:  plan.Order{Kind: plan.KindApplicationCredential, Name: "refused-appcred", User: "user", Project: "project"},
				Reason: "NoRoleOnProject",
			},
			{
				Order:  plan.Order{Kind: plan.KindRoleAssignment, Name: "refused-role", User: "user", Project: "project", Role: s.RefusedRole},
				Reason: "RoleNotAllowed",
			},
		},
		GracePeriod:  time.Duration(s.Rotation.GracePeriod),
		ReadyTimeout: time.Duration(s.Timeouts.Ready),
		GoneTimeout:  time.Duration(s.Timeouts.Gone),
		SecretRepair: s.Checks.secretRepair(),
	}

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("generated plan failed validation: %w", err)
	}
	return p, nil
}
