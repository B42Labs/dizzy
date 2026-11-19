package scenario

import (
	"bytes"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/c5c3/plan"
)

// boolPtr returns a pointer to b, for building the *bool Secret repair switch
// in test fixtures.
func boolPtr(b bool) *bool { return &b }

// smallScenario is the scenario the shipped scenarios/c5c3/small.yaml parses
// to; TestSmallProfileMatchesGoldenFixture ties the two together.
func smallScenario() Scenario {
	return Scenario{
		Name:         "small",
		Namespace:    "dizzy-orders",
		ControlPlane: ControlPlane{Name: "controlplane", Namespace: "openstack"},
		Role:         "member",
		RefusedRole:  "dizzy-refused",
		Catalog:      Catalog{ServiceType: "dizzy-probe", EndpointURL: "https://dizzy-probe.invalid/v1"},
		Rotation:     Rotation{GracePeriod: Duration(2 * time.Minute)},
		Timeouts:     Timeouts{Ready: Duration(15 * time.Minute), Gone: Duration(15 * time.Minute)},
		Checks:       Checks{SecretRepair: boolPtr(true)},
	}
}

// minimalYAML names only the keys that have no default.
const minimalYAML = `name: minimal
namespace: dizzy-orders
control_plane:
  name: controlplane
role: member
catalog:
  service_type: dizzy-probe
  endpoint_url: https://dizzy-probe.invalid/v1
`

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

// generate turns s into its plan, failing the test on an error.
func generate(t *testing.T, s Scenario) *plan.Plan {
	t.Helper()
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}
	return p
}

// TestParseRejectsUnknownKey confirms strict unmarshal fails loudly on a key
// the schema does not know, at the top level and inside each block, among
// them a Barbican resources block and a count, which a c5c3 scenario does not
// have.
func TestParseRejectsUnknownKey(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "barbican resources block", yaml: minimalYAML + "resources:\n  secrets: 1\n", wantErr: "field resources not found"},
		{name: "count", yaml: minimalYAML + "count: 2\n", wantErr: "field count not found"},
		{name: "inside control_plane", yaml: "name: x\ncontrol_plane:\n  cluster: a\n", wantErr: "field cluster not found"},
		{name: "inside catalog", yaml: "name: x\ncatalog:\n  service_name: a\n", wantErr: "field service_name not found"},
		{name: "inside rotation", yaml: "name: x\nrotation:\n  interval: 720h\n", wantErr: "field interval not found"},
		{name: "inside timeouts", yaml: "name: x\ntimeouts:\n  readyy: 1m\n", wantErr: "field readyy not found"},
		{name: "inside checks", yaml: "name: x\nchecks:\n  secret-repair: false\n", wantErr: "field secret-repair not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Parse() = %v, want an error containing %q", err, tc.wantErr)
			}
			if !strings.HasPrefix(err.Error(), "parsing scenario: ") {
				t.Errorf("Parse() = %q, want it wrapped as %q", err, "parsing scenario: ...")
			}
		})
	}
}

// TestParseRejectsSeed confirms a seed key fails the strict parse: a c5c3 plan
// draws nothing, so the schema has no seed.
func TestParseRejectsSeed(t *testing.T) {
	_, err := Parse([]byte(minimalYAML + "seed: 42\n"))
	if want := "field seed not found"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Parse() = %v, want an error containing %q", err, want)
	}
}

// TestParseRejectsMalformedDuration confirms a duration that
// time.ParseDuration does not accept fails the parse and names the value.
func TestParseRejectsMalformedDuration(t *testing.T) {
	for _, block := range []string{
		"rotation:\n  grace_period: soon\n",
		"timeouts:\n  ready: soon\n",
		"timeouts:\n  gone: soon\n",
	} {
		_, err := Parse([]byte(minimalYAML + block))
		if want := `parsing duration "soon"`; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v, want an error containing %q", block, err, want)
		}
	}
}

// TestParseDefaults confirms a scenario without refused_role, rotation,
// timeouts and checks takes their defaults and yields a plan whose
// refused-role refusal asks for dizzy-refused, with a 2m grace period, 15m
// for both timeouts and the Secret repair check on. It also confirms the
// defaults never hide a value written explicitly: a block keeps the defaults
// of the keys it omits, a block header without children keeps all of them,
// an explicit 0s stays 0s and fails Validate, and an explicit false turns the
// repair check off.
func TestParseDefaults(t *testing.T) {
	t.Run("absent keys", func(t *testing.T) {
		s, err := Parse([]byte(minimalYAML))
		if err != nil {
			t.Fatalf("Parse() = %v, want nil", err)
		}
		if s.ControlPlane.Namespace != "" {
			t.Errorf("control_plane.namespace = %q, want empty", s.ControlPlane.Namespace)
		}
		if s.Checks.SecretRepair != nil {
			t.Errorf("checks.secret_repair = %v, want nil (absent)", *s.Checks.SecretRepair)
		}

		p := generate(t, s)
		if got := p.Refusals[1]; got.Order.Name != "refused-role" || got.Order.Role != "dizzy-refused" {
			t.Errorf("Refusals[1] = %+v, want refused-role with role dizzy-refused", got)
		}
		if p.GracePeriod != 2*time.Minute {
			t.Errorf("GracePeriod = %s, want 2m0s", p.GracePeriod)
		}
		if p.ReadyTimeout != 15*time.Minute {
			t.Errorf("ReadyTimeout = %s, want 15m0s", p.ReadyTimeout)
		}
		if p.GoneTimeout != 15*time.Minute {
			t.Errorf("GoneTimeout = %s, want 15m0s", p.GoneTimeout)
		}
		if !p.SecretRepair {
			t.Error("SecretRepair = false, want true")
		}
		if p.ControlPlane.Namespace != "" {
			t.Errorf("ControlPlane.Namespace = %q, want empty", p.ControlPlane.Namespace)
		}
	})

	t.Run("a block keeps the defaults of the keys it omits", func(t *testing.T) {
		s, err := Parse([]byte(minimalYAML + "timeouts:\n  ready: 5m\n"))
		if err != nil {
			t.Fatalf("Parse() = %v, want nil", err)
		}
		if got := time.Duration(s.Timeouts.Ready); got != 5*time.Minute {
			t.Errorf("timeouts.ready = %s, want 5m0s", got)
		}
		if got := time.Duration(s.Timeouts.Gone); got != 15*time.Minute {
			t.Errorf("timeouts.gone = %s, want the default 15m0s", got)
		}
	})

	// yaml.v2 decodes a block header without children as null, and null
	// into a struct as its zero value. Such a block names none of its keys,
	// so its plan must be the plan of a file that leaves the block out.
	t.Run("a block without children keeps its defaults", func(t *testing.T) {
		absent, err := Parse([]byte(minimalYAML))
		if err != nil {
			t.Fatalf("Parse() = %v, want nil", err)
		}
		want := marshal(t, generate(t, absent))

		for _, block := range []string{
			"rotation:\n",
			"rotation:\n  # grace_period: 2m\n",
			"rotation: {}\n",
			"timeouts:\n",
			"timeouts:\n  # ready: 15m\n  # gone: 15m\n",
			"timeouts: {}\n",
			"checks:\n",
			"checks:\n  # secret_repair: true\n",
		} {
			t.Run(block, func(t *testing.T) {
				s, err := Parse([]byte(minimalYAML + block))
				if err != nil {
					t.Fatalf("Parse() = %v, want nil", err)
				}
				if !reflect.DeepEqual(s, absent) {
					t.Errorf("Parse() = %+v, want the defaults of an absent block %+v", s, absent)
				}
				if got := marshal(t, generate(t, s)); !bytes.Equal(got, want) {
					t.Errorf("plan = %s, want the plan of an absent block %s", got, want)
				}
			})
		}
	})

	t.Run("an explicit 0s is kept and fails Validate", func(t *testing.T) {
		for _, tc := range []struct {
			block   string
			wantErr string
		}{
			{"timeouts:\n  ready: 0s\n", "timeouts.ready must be positive, got 0s"},
			{"timeouts:\n  gone: 0s\n", "timeouts.gone must be positive, got 0s"},
			{"rotation:\n  grace_period: 0s\n", "rotation.grace_period must be at least 10s and below 720h, got 0s"},
		} {
			s, err := Parse([]byte(minimalYAML + tc.block))
			if err != nil {
				t.Fatalf("Parse(%q) = %v, want nil", tc.block, err)
			}
			if err := s.Validate(); err == nil || err.Error() != tc.wantErr {
				t.Errorf("Parse(%q).Validate() = %v, want %q", tc.block, err, tc.wantErr)
			}
		}
	})

	t.Run("an explicit false turns the repair check off", func(t *testing.T) {
		s, err := Parse([]byte(minimalYAML + "checks:\n  secret_repair: false\n"))
		if err != nil {
			t.Fatalf("Parse() = %v, want nil", err)
		}
		if p := generate(t, s); p.SecretRepair {
			t.Error("SecretRepair = true, want false")
		}
	})

	t.Run("an explicit empty refused_role is kept and fails Validate", func(t *testing.T) {
		s, err := Parse([]byte(minimalYAML + "refused_role: \"\"\n"))
		if err != nil {
			t.Fatalf("Parse() = %v, want nil", err)
		}
		if want := "refused_role must not be empty"; s.Validate() == nil || s.Validate().Error() != want {
			t.Errorf("Validate() = %v, want %q", s.Validate(), want)
		}
	})
}

// urlParseError returns the error url.Parse reports for raw, so a test can
// expect the wrapped text without copying the standard library's wording.
func urlParseError(t *testing.T, raw string) string {
	t.Helper()
	_, err := url.Parse(raw)
	if err == nil {
		t.Fatalf("url.Parse(%q) = nil, want an error", raw)
	}
	return err.Error()
}

// TestValidate asserts Validate accepts the fixture and the edge values each
// rule still admits, and rejects a failing input per rule of the scenario
// schema, the label pattern one part at a time, with the exact message, which
// starts with the key it names.
func TestValidate(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	label64 := strings.Repeat("a", 64)
	role255 := strings.Repeat("r", 255)
	role256 := strings.Repeat("r", 256)
	badURL := "https://dizzy probe.invalid/v1"
	// The CRD caps the endpoint URL at 1024 characters, counted as the API
	// server counts them, in runes, so a URL of 1024 multibyte characters is
	// still admitted although it is longer in bytes.
	urlPrefix := "https://dizzy-probe.invalid/"
	url1024 := urlPrefix + strings.Repeat("a", 1024-len(urlPrefix))
	url1024Multibyte := urlPrefix + strings.Repeat("ä", 1024-len(urlPrefix))
	url1025 := url1024 + "a"

	type validateCase struct {
		name   string
		mutate func(*Scenario)
		// key is the scenario key the error must name; empty means valid.
		key string
		// wantErr is the exact expected error.
		wantErr string
	}
	tests := []validateCase{
		{name: "valid", mutate: func(*Scenario) {}},
		{name: "control plane in the orders' namespace", mutate: func(s *Scenario) { s.ControlPlane.Namespace = "" }},
		{name: "namespace of 63 characters", mutate: func(s *Scenario) { s.Namespace = label63 }},
		{name: "control plane namespace of 63 characters", mutate: func(s *Scenario) { s.ControlPlane.Namespace = label63 }},
		{name: "role of 255 characters", mutate: func(s *Scenario) { s.Role = role255 }},
		{name: "role of 255 multibyte characters", mutate: func(s *Scenario) { s.Role = strings.Repeat("ä", 255) }},
		{name: "role with spaces and capitals", mutate: func(s *Scenario) { s.Role = "Load Balancer Member" }},
		{name: "refused role of 255 characters", mutate: func(s *Scenario) { s.RefusedRole = role255 }},
		{name: "service type of 63 characters", mutate: func(s *Scenario) { s.Catalog.ServiceType = label63 }},
		{name: "http endpoint", mutate: func(s *Scenario) { s.Catalog.EndpointURL = "http://dizzy-probe.invalid:8080/v1" }},
		{name: "endpoint url of 1024 characters", mutate: func(s *Scenario) { s.Catalog.EndpointURL = url1024 }},
		{name: "endpoint url of 1024 multibyte characters", mutate: func(s *Scenario) { s.Catalog.EndpointURL = url1024Multibyte }},
		{name: "grace period of 10s", mutate: func(s *Scenario) { s.Rotation.GracePeriod = Duration(10 * time.Second) }},
		{name: "grace period just below 720h", mutate: func(s *Scenario) { s.Rotation.GracePeriod = Duration(720*time.Hour - time.Second) }},

		{name: "empty name", mutate: func(s *Scenario) { s.Name = "" }, key: "name", wantErr: "name must not be empty"},
		{name: "empty namespace", mutate: func(s *Scenario) { s.Namespace = "" }, key: "namespace", wantErr: "namespace must not be empty"},
		{name: "empty control plane name", mutate: func(s *Scenario) { s.ControlPlane.Name = "" }, key: "control_plane.name", wantErr: "control_plane.name must not be empty"},
		{name: "empty role", mutate: func(s *Scenario) { s.Role = "" }, key: "role", wantErr: "role must not be empty"},
		{name: "empty refused role", mutate: func(s *Scenario) { s.RefusedRole = "" }, key: "refused_role", wantErr: "refused_role must not be empty"},
		{name: "empty service type", mutate: func(s *Scenario) { s.Catalog.ServiceType = "" }, key: "catalog.service_type", wantErr: "catalog.service_type must not be empty"},
		{name: "empty endpoint url", mutate: func(s *Scenario) { s.Catalog.EndpointURL = "" }, key: "catalog.endpoint_url", wantErr: "catalog.endpoint_url must not be empty"},

		{
			name:    "namespace not a label",
			mutate:  func(s *Scenario) { s.Namespace = "Dizzy_Orders" },
			key:     "namespace",
			wantErr: `namespace must be a DNS-1123 label (lowercase letters, digits and '-', starting and ending with a letter or digit), got "Dizzy_Orders"`,
		},
		{
			name:    "namespace too long",
			mutate:  func(s *Scenario) { s.Namespace = label64 },
			key:     "namespace",
			wantErr: "namespace must be at most 63 characters, got 64",
		},
		{
			name:    "control plane namespace not a label",
			mutate:  func(s *Scenario) { s.ControlPlane.Namespace = "openstack-" },
			key:     "control_plane.namespace",
			wantErr: `control_plane.namespace must be a DNS-1123 label (lowercase letters, digits and '-', starting and ending with a letter or digit), got "openstack-"`,
		},
		{
			name:    "control plane namespace too long",
			mutate:  func(s *Scenario) { s.ControlPlane.Namespace = label64 },
			key:     "control_plane.namespace",
			wantErr: "control_plane.namespace must be at most 63 characters, got 64",
		},

		{name: "role with a comma", mutate: func(s *Scenario) { s.Role = "member,admin" }, key: "role", wantErr: `role must not contain a comma, got "member,admin"`},
		{name: "role too long", mutate: func(s *Scenario) { s.Role = role256 }, key: "role", wantErr: "role must be at most 255 characters, got 256"},
		{name: "refused role with a comma", mutate: func(s *Scenario) { s.RefusedRole = "a,b" }, key: "refused_role", wantErr: `refused_role must not contain a comma, got "a,b"`},
		{name: "refused role too long", mutate: func(s *Scenario) { s.RefusedRole = role256 }, key: "refused_role", wantErr: "refused_role must be at most 255 characters, got 256"},
		{name: "refused role equal to role", mutate: func(s *Scenario) { s.RefusedRole = "member" }, key: "refused_role", wantErr: `refused_role must differ from role, both are "member"`},

		{
			name:    "service type not a label",
			mutate:  func(s *Scenario) { s.Catalog.ServiceType = "Dizzy_Probe" },
			key:     "catalog.service_type",
			wantErr: `catalog.service_type must be a DNS-1123 label (lowercase letters, digits and '-', starting and ending with a letter or digit), got "Dizzy_Probe"`,
		},
		{
			name:    "service type too long",
			mutate:  func(s *Scenario) { s.Catalog.ServiceType = label64 },
			key:     "catalog.service_type",
			wantErr: "catalog.service_type must be at most 63 characters, got 64",
		},
		{
			name:    "service type identity",
			mutate:  func(s *Scenario) { s.Catalog.ServiceType = "identity" },
			key:     "catalog.service_type",
			wantErr: "catalog.service_type must not be identity, whose catalog entry the ControlPlane owns",
		},

		{
			name:    "endpoint url that does not parse",
			mutate:  func(s *Scenario) { s.Catalog.EndpointURL = badURL },
			key:     "catalog.endpoint_url",
			wantErr: "catalog.endpoint_url is not a URL: " + urlParseError(t, badURL),
		},
		{
			name:    "endpoint url with another scheme",
			mutate:  func(s *Scenario) { s.Catalog.EndpointURL = "ftp://dizzy-probe.invalid/v1" },
			key:     "catalog.endpoint_url",
			wantErr: `catalog.endpoint_url must use the http or https scheme, in lowercase, got "ftp"`,
		},
		{
			name:    "endpoint url without a scheme",
			mutate:  func(s *Scenario) { s.Catalog.EndpointURL = "dizzy-probe.invalid/v1" },
			key:     "catalog.endpoint_url",
			wantErr: `catalog.endpoint_url must use the http or https scheme, in lowercase, got ""`,
		},
		{
			name:    "endpoint url too long",
			mutate:  func(s *Scenario) { s.Catalog.EndpointURL = url1025 },
			key:     "catalog.endpoint_url",
			wantErr: "catalog.endpoint_url must be at most 1024 characters, got 1025",
		},
		{
			name:    "endpoint url with an uppercase scheme",
			mutate:  func(s *Scenario) { s.Catalog.EndpointURL = "HTTPS://dizzy-probe.invalid/v1" },
			key:     "catalog.endpoint_url",
			wantErr: `catalog.endpoint_url must use the http or https scheme, in lowercase, got "HTTPS"`,
		},
		{
			name:    "endpoint url with a mixed-case scheme",
			mutate:  func(s *Scenario) { s.Catalog.EndpointURL = "Http://dizzy-probe.invalid/v1" },
			key:     "catalog.endpoint_url",
			wantErr: `catalog.endpoint_url must use the http or https scheme, in lowercase, got "Http"`,
		},
		{
			name:    "endpoint url without a host",
			mutate:  func(s *Scenario) { s.Catalog.EndpointURL = "https:///v1" },
			key:     "catalog.endpoint_url",
			wantErr: `catalog.endpoint_url must name a host, got "https:///v1"`,
		},
		{
			name:    "endpoint url without the slashes after the scheme",
			mutate:  func(s *Scenario) { s.Catalog.EndpointURL = "http:dizzy-probe.invalid/v1" },
			key:     "catalog.endpoint_url",
			wantErr: `catalog.endpoint_url must name a host, got "http:dizzy-probe.invalid/v1"`,
		},
		{
			name:    "endpoint url with one slash after the scheme",
			mutate:  func(s *Scenario) { s.Catalog.EndpointURL = "https:/dizzy-probe.invalid/v1" },
			key:     "catalog.endpoint_url",
			wantErr: `catalog.endpoint_url must name a host, got "https:/dizzy-probe.invalid/v1"`,
		},
		{
			name:    "endpoint url with a port but no host, password redacted",
			mutate:  func(s *Scenario) { s.Catalog.EndpointURL = "https://user:hunter2@:8443/v1" },
			key:     "catalog.endpoint_url",
			wantErr: `catalog.endpoint_url must name a host, got "https://user:xxxxx@:8443/v1"`,
		},

		{
			name:    "grace period below 10s",
			mutate:  func(s *Scenario) { s.Rotation.GracePeriod = Duration(9 * time.Second) },
			key:     "rotation.grace_period",
			wantErr: "rotation.grace_period must be at least 10s and below 720h, got 9s",
		},
		{
			name:    "grace period of 720h",
			mutate:  func(s *Scenario) { s.Rotation.GracePeriod = Duration(720 * time.Hour) },
			key:     "rotation.grace_period",
			wantErr: "rotation.grace_period must be at least 10s and below 720h, got 720h0m0s",
		},
		{name: "zero ready timeout", mutate: func(s *Scenario) { s.Timeouts.Ready = 0 }, key: "timeouts.ready", wantErr: "timeouts.ready must be positive, got 0s"},
		{name: "negative ready timeout", mutate: func(s *Scenario) { s.Timeouts.Ready = Duration(-time.Minute) }, key: "timeouts.ready", wantErr: "timeouts.ready must be positive, got -1m0s"},
		{name: "zero gone timeout", mutate: func(s *Scenario) { s.Timeouts.Gone = 0 }, key: "timeouts.gone", wantErr: "timeouts.gone must be positive, got 0s"},
		{name: "negative gone timeout", mutate: func(s *Scenario) { s.Timeouts.Gone = Duration(-time.Minute) }, key: "timeouts.gone", wantErr: "timeouts.gone must be positive, got -1m0s"},
	}
	// Each label key gets edge values the pattern
	// ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ admits, and inputs that each break one
	// part of it: an underscore, an uppercase letter or a dot at the start,
	// in the middle or at the end, and a dash at either end. A pattern that
	// admits '_', an uppercase letter or '.' in any position, or a dash at
	// either end, fails one of them.
	labelKeys := []struct {
		key string
		set func(*Scenario, string)
	}{
		{"namespace", func(s *Scenario, v string) { s.Namespace = v }},
		{"control_plane.namespace", func(s *Scenario, v string) { s.ControlPlane.Namespace = v }},
		{"catalog.service_type", func(s *Scenario, v string) { s.Catalog.ServiceType = v }},
	}
	labels := []string{"a", "7", "0-probe", "a--b", "probe-0"}
	notLabels := []string{
		"_probe", "pro_be", "probe_",
		"Probe", "proBe", "probE",
		".probe", "pro.be", "probe.",
		"-probe", "probe-", "-",
	}
	for _, lk := range labelKeys {
		for _, value := range labels {
			tests = append(tests, validateCase{
				name:   lk.key + " " + value,
				mutate: func(s *Scenario) { lk.set(s, value) },
			})
		}
		for _, value := range notLabels {
			tests = append(tests, validateCase{
				name:    lk.key + " " + value,
				mutate:  func(s *Scenario) { lk.set(s, value) },
				key:     lk.key,
				wantErr: lk.key + ` must be a DNS-1123 label (lowercase letters, digits and '-', starting and ending with a letter or digit), got "` + value + `"`,
			})
		}
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			tc.mutate(&s)
			err := s.Validate()
			if tc.key == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Validate() = %v, want %q", err, tc.wantErr)
			}
			if !strings.HasPrefix(err.Error(), tc.key+" ") {
				t.Errorf("Validate() = %q, want it to start with the key %s", err, tc.key)
			}
		})
	}
}

// TestSet asserts each of the eleven override keys parses into its field and
// reaches the generated plan, the Secret repair switch's false included, and
// that a value that does not parse fails with a message naming the key and
// the value and leaves the scenario unchanged.
func TestSet(t *testing.T) {
	keys := []struct {
		key   string
		value string
		check func(*plan.Plan) bool
	}{
		{"namespace", "other-orders", func(p *plan.Plan) bool { return p.Namespace == "other-orders" }},
		{"control_plane.name", "cp2", func(p *plan.Plan) bool { return p.ControlPlane.Name == "cp2" }},
		{"control_plane.namespace", "", func(p *plan.Plan) bool { return p.ControlPlane.Namespace == "" }},
		{"role", "reader", func(p *plan.Plan) bool { return p.Orders[2].Name == "role" && p.Orders[2].Role == "reader" }},
		{"refused_role", "admin", func(p *plan.Plan) bool {
			return p.Refusals[1].Order.Name == "refused-role" && p.Refusals[1].Order.Role == "admin"
		}},
		{"catalog.service_type", "other-probe", func(p *plan.Plan) bool { return p.Orders[3].ServiceType == "other-probe" }},
		{"catalog.endpoint_url", "http://other.invalid:8080", func(p *plan.Plan) bool { return p.Orders[3].EndpointURL == "http://other.invalid:8080" }},
		{"rotation.grace_period", "30s", func(p *plan.Plan) bool { return p.GracePeriod == 30*time.Second }},
		{"timeouts.ready", "20m", func(p *plan.Plan) bool { return p.ReadyTimeout == 20*time.Minute }},
		{"timeouts.gone", "5m", func(p *plan.Plan) bool { return p.GoneTimeout == 5*time.Minute }},
		{"checks.secret_repair", "false", func(p *plan.Plan) bool { return !p.SecretRepair }},
	}
	for _, k := range keys {
		t.Run(k.key, func(t *testing.T) {
			s := smallScenario()
			if err := s.Set(k.key, k.value); err != nil {
				t.Fatalf("Set(%q, %q) = %v, want nil", k.key, k.value, err)
			}
			if p := generate(t, s); !k.check(p) {
				t.Errorf("Set(%q, %q) did not reach the plan: %+v", k.key, k.value, p)
			}
		})
	}

	t.Run("checks.secret_repair true after false", func(t *testing.T) {
		s := smallScenario()
		s.Checks.SecretRepair = boolPtr(false)
		if err := s.Set("checks.secret_repair", "true"); err != nil {
			t.Fatalf("Set() = %v, want nil", err)
		}
		if p := generate(t, s); !p.SecretRepair {
			t.Error("SecretRepair = false, want true")
		}
	})

	bad := []struct {
		key     string
		value   string
		wantErr string
	}{
		{"rotation.grace_period", "soon", `override rotation.grace_period: "soon" is not a duration`},
		{"timeouts.ready", "15", `override timeouts.ready: "15" is not a duration`},
		{"timeouts.gone", "", `override timeouts.gone: "" is not a duration`},
		{"checks.secret_repair", "maybe", `override checks.secret_repair: "maybe" is not a boolean`},
	}
	for _, tc := range bad {
		t.Run("bad value for "+tc.key, func(t *testing.T) {
			s := smallScenario()
			err := s.Set(tc.key, tc.value)
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Set(%q, %q) = %v, want %q", tc.key, tc.value, err, tc.wantErr)
			}
			if !reflect.DeepEqual(s, smallScenario()) {
				t.Errorf("Set(%q, %q) changed the scenario on error: %+v", tc.key, tc.value, s)
			}
		})
	}
}

// TestSetUnknownKey asserts every key Set does not accept, name and seed
// among them, fails with an error naming the key and leaves the scenario
// unchanged.
func TestSetUnknownKey(t *testing.T) {
	for _, key := range []string{"nope", "name", "seed", "count", "control_plane", "Role", "checks.secret-repair", "rotation.interval"} {
		t.Run(key, func(t *testing.T) {
			s := smallScenario()
			err := s.Set(key, "1")
			if want := `unknown override key "` + key + `"`; err == nil || err.Error() != want {
				t.Fatalf("Set(%q, %q) = %v, want %q", key, "1", err, want)
			}
			if !reflect.DeepEqual(s, smallScenario()) {
				t.Errorf("Set(%q, %q) changed the scenario on error: %+v", key, "1", s)
			}
		})
	}
}

// TestGenerateShape asserts Generate emits the five orders and two refusals
// of the plan's fixed shape, in that order, filled from the scenario: for the
// small fixture, and for a scenario whose ControlPlane lives in the orders'
// own namespace and that turns the repair check off.
func TestGenerateShape(t *testing.T) {
	want := func(s Scenario, secretRepair bool) *plan.Plan {
		return &plan.Plan{
			Scenario:     s.Name,
			Namespace:    s.Namespace,
			ControlPlane: plan.ControlPlane{Name: s.ControlPlane.Name, Namespace: s.ControlPlane.Namespace},
			Orders: []plan.Order{
				{Kind: plan.KindUser, Name: "user"},
				{Kind: plan.KindProject, Name: "project"},
				{Kind: plan.KindRoleAssignment, Name: "role", User: "user", Project: "project", Role: s.Role},
				{Kind: plan.KindCatalogEntry, Name: "catalog", ServiceType: s.Catalog.ServiceType, EndpointURL: s.Catalog.EndpointURL},
				{Kind: plan.KindApplicationCredential, Name: "appcred", User: "user", Project: "project"},
			},
			Refusals: []plan.Refusal{
				{Order: plan.Order{Kind: plan.KindApplicationCredential, Name: "refused-appcred", User: "user", Project: "project"}, Reason: "NoRoleOnProject"},
				{Order: plan.Order{Kind: plan.KindRoleAssignment, Name: "refused-role", User: "user", Project: "project", Role: s.RefusedRole}, Reason: "RoleNotAllowed"},
			},
			GracePeriod:  time.Duration(s.Rotation.GracePeriod),
			ReadyTimeout: time.Duration(s.Timeouts.Ready),
			GoneTimeout:  time.Duration(s.Timeouts.Gone),
			SecretRepair: secretRepair,
		}
	}

	t.Run("small", func(t *testing.T) {
		s := smallScenario()
		got := generate(t, s)
		if w := want(s, true); !reflect.DeepEqual(got, w) {
			t.Errorf("Generate() = %+v\nwant         %+v", got, w)
		}
		// The fixture's literal values, so the shape is not only checked
		// against itself.
		if got.Orders[2].Role != "member" || got.Refusals[1].Order.Role != "dizzy-refused" || got.Orders[3].ServiceType != "dizzy-probe" {
			t.Errorf("Generate() did not carry the fixture's roles and service type: %+v", got)
		}
	})

	t.Run("control plane in the orders' namespace, repair off", func(t *testing.T) {
		s := smallScenario()
		s.Name = "local"
		s.Namespace = "team-a"
		s.ControlPlane = ControlPlane{Name: "cp"}
		s.Role = "reader"
		s.RefusedRole = "admin"
		s.Catalog = Catalog{ServiceType: "probe", EndpointURL: "http://probe.invalid"}
		s.Rotation.GracePeriod = Duration(time.Minute)
		s.Timeouts = Timeouts{Ready: Duration(time.Minute), Gone: Duration(2 * time.Minute)}
		s.Checks.SecretRepair = boolPtr(false)

		got := generate(t, s)
		if w := want(s, false); !reflect.DeepEqual(got, w) {
			t.Errorf("Generate() = %+v\nwant         %+v", got, w)
		}
		var decoded struct {
			ControlPlane map[string]string `json:"controlPlane"`
		}
		data := marshal(t, got)
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("decoding plan JSON: %v", err)
		}
		if ns, ok := decoded.ControlPlane["namespace"]; ok {
			t.Errorf("plan JSON carries control plane namespace %q, want the key omitted:\n%s", ns, data)
		}
	})
}

// TestGenerateInvalidScenario confirms Generate validates the scenario first
// and wraps what Validate reports.
func TestGenerateInvalidScenario(t *testing.T) {
	s := smallScenario()
	s.Catalog.ServiceType = "identity"

	_, err := s.Generate()
	if want := "invalid scenario: catalog.service_type must not be identity, whose catalog entry the ControlPlane owns"; err == nil || err.Error() != want {
		t.Fatalf("Generate() = %v, want %q", err, want)
	}
}

// TestGenerateIsDeterministic confirms Generate is a pure function of the
// scenario: two generations encode byte-identically, as do the plans of two
// separate parses of the same file, and generating leaves the scenario
// unchanged.
func TestGenerateIsDeterministic(t *testing.T) {
	s := smallScenario()
	if first, second := marshal(t, generate(t, s)), marshal(t, generate(t, s)); !bytes.Equal(first, second) {
		t.Errorf("two generations of the same scenario differ:\n%s\n---\n%s", first, second)
	}
	if !reflect.DeepEqual(s, smallScenario()) {
		t.Errorf("Generate() changed the scenario: %+v", s)
	}

	parse := func() Scenario {
		t.Helper()
		parsed, err := Parse([]byte(minimalYAML))
		if err != nil {
			t.Fatalf("Parse() = %v, want nil", err)
		}
		return parsed
	}
	if first, second := marshal(t, generate(t, parse())), marshal(t, generate(t, parse())); !bytes.Equal(first, second) {
		t.Errorf("the plans of two parses of the same file differ:\n%s\n---\n%s", first, second)
	}
}
