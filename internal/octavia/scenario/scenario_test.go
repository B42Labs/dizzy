package scenario

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// floatPtr returns a pointer to f, for building the *float64 chaos field
// (UpdateRatio) in test fixtures.
func floatPtr(f float64) *float64 { return &f }

// smallScenario is the compact scenario backing the golden test; it mirrors the
// shipped scenarios/octavia/small.yaml without its chaos block, so the profile
// test can tie to it.
func smallScenario() Scenario {
	return Scenario{
		Name:     "small",
		Seed:     42,
		Provider: "",
		Resources: Resources{
			Networks:       1,
			LoadBalancers:  2,
			Listeners:      4,
			Members:        8,
			HealthMonitors: 2,
		},
		Distribution: Distribution{
			ListenerPort: Range{Min: 1024, Max: 65535},
			MemberPort:   Range{Min: 1024, Max: 65535},
		},
	}
}

// TestParseDecodesEveryKey confirms a scenario spelling out every key outside
// the chaos block decodes into the matching fields. TestParseChaosBlock covers
// the chaos block.
func TestParseDecodesEveryKey(t *testing.T) {
	yaml := `name: small
seed: 42
provider: ovn
resources:
  networks: 1
  loadbalancers: 2
  listeners: 4
  members: 8
  health_monitors: 2
distribution:
  listener_port: { min: 1024, max: 65535 }
  member_port: { min: 1024, max: 65535 }
`
	s, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() = %v, want nil", err)
	}
	want := smallScenario()
	want.Provider = "ovn"
	if s != want {
		t.Errorf("Parse() = %+v, want %+v", s, want)
	}
}

// TestParseChaosBlock confirms a scenario with a full chaos block decodes into
// Chaos with every field set, update_ratio included, leaves the rest of the
// scenario as it would be without the block, and validates. It compares with
// reflect.DeepEqual rather than == because Scenario carries a *Chaos pointer,
// which == would compare by identity.
func TestParseChaosBlock(t *testing.T) {
	yaml := `name: small
seed: 42
resources:
  networks: 1
  loadbalancers: 2
  listeners: 4
  members: 8
  health_monitors: 2
distribution:
  listener_port: { min: 1024, max: 65535 }
  member_port: { min: 1024, max: 65535 }
chaos:
  duration: 5m
  interval: { min: 200ms, max: 3s }
  parallel: { max: 4 }
  churn_ratio: 0.5
  target_fill: 0.8
  update_ratio: 0.3
  bucket_width: 30m
`
	s, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() = %v, want nil", err)
	}
	want := smallScenario()
	want.Chaos = &Chaos{
		Duration:    Duration(5 * time.Minute),
		Interval:    Interval{Min: Duration(200 * time.Millisecond), Max: Duration(3 * time.Second)},
		Parallel:    Parallel{Max: 4},
		ChurnRatio:  0.5,
		TargetFill:  0.8,
		UpdateRatio: floatPtr(0.3),
		BucketWidth: Duration(30 * time.Minute),
	}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("Parse() = %+v (chaos %+v), want %+v (chaos %+v)", s, s.Chaos, want, want.Chaos)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

// TestParseWithoutChaosBlock confirms a scenario with no chaos block parses
// with a nil Chaos and still validates, so apply and generate keep accepting
// the scenarios written before the block existed.
func TestParseWithoutChaosBlock(t *testing.T) {
	yaml := `name: small
seed: 42
resources:
  networks: 1
  loadbalancers: 2
  listeners: 4
  members: 8
  health_monitors: 2
distribution:
  listener_port: { min: 1024, max: 65535 }
  member_port: { min: 1024, max: 65535 }
`
	s, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() = %v, want nil", err)
	}
	if s.Chaos != nil {
		t.Errorf("Parse() Chaos = %+v, want nil", *s.Chaos)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

// TestParseChaosUpdateRatio confirms the chaos block's update_ratio keeps an
// omitted key apart from an explicit zero: omitted parses to nil, so the
// command falls back to its default, while update_ratio: 0 parses to a pointer
// to 0, which disables the admin-state toggle.
func TestParseChaosUpdateRatio(t *testing.T) {
	tests := []struct {
		name string
		line string
		want *float64
	}{
		{name: "omitted", line: ""},
		{name: "zero", line: "  update_ratio: 0\n", want: floatPtr(0)},
		{name: "one", line: "  update_ratio: 1\n", want: floatPtr(1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Parse([]byte("name: x\nchaos:\n  duration: 5m\n" + tc.line))
			if err != nil {
				t.Fatalf("Parse() = %v, want nil", err)
			}
			if s.Chaos == nil {
				t.Fatal("Parse() Chaos = nil, want the parsed chaos block")
			}
			if got := s.Chaos.UpdateRatio; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("chaos.update_ratio = %s, want %s", describeRatio(got), describeRatio(tc.want))
			}
		})
	}
}

// describeRatio renders an optional ratio for a test failure message, since
// %v on a *float64 prints the address rather than the value.
func describeRatio(r *float64) string {
	if r == nil {
		return "nil"
	}
	return fmt.Sprintf("&%v", *r)
}

// TestParseChaosMalformedDuration confirms every duration key of the chaos
// block fails Parse on a string time.ParseDuration rejects, naming the string.
func TestParseChaosMalformedDuration(t *testing.T) {
	for _, block := range []string{
		"  duration: soon\n",
		"  interval: { min: soon, max: 3s }\n",
		"  interval: { min: 200ms, max: soon }\n",
		"  bucket_width: soon\n",
	} {
		t.Run(strings.TrimSpace(block), func(t *testing.T) {
			_, err := Parse([]byte("name: x\nchaos:\n" + block))
			if want := `parsing duration "soon"`; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Parse() = %v, want an error containing %q", err, want)
			}
		})
	}
}

// TestParseRejectsUnknownKey confirms strict unmarshal fails loudly on a key
// the schema does not know: a pools count, which the listeners count already
// sets, a stray leaf borrowed from another service, and Barbican's
// retrieve_ratio inside the chaos block, where Octavia's knob is update_ratio.
func TestParseRejectsUnknownKey(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "pools count",
			yaml:    "name: x\nresources:\n  pools: 3\n",
			wantErr: "field pools not found",
		},
		{
			name:    "barbican resources leaf",
			yaml:    "name: x\nresources:\n  secrets: 1\n",
			wantErr: "field secrets not found",
		},
		{
			name:    "nova distribution leaf",
			yaml:    "name: x\ndistribution:\n  ports_per_server: { min: 0, max: 1 }\n",
			wantErr: "field ports_per_server not found",
		},
		{
			name:    "retrieve_ratio inside chaos",
			yaml:    "name: x\nchaos:\n  retrieve_ratio: 0.3\n",
			wantErr: "field retrieve_ratio not found",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Parse() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidate asserts Validate accepts the fixture and its edge cases and
// rejects each malformed variant with the exact message naming the offending
// key. Every case starts from smallScenario (1 network, 2 load balancers, 4
// listeners, 8 members, 2 health monitors), so a case whose input violates
// more than one rule also pins which rule Validate reports first.
func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Scenario)
		// wantErr is the exact expected error; empty means valid.
		wantErr string
	}{
		{name: "valid", mutate: func(*Scenario) {}},
		{name: "empty name", mutate: func(s *Scenario) { s.Name = "" }, wantErr: "name must not be empty"},
		{name: "empty name before a negative count", mutate: func(s *Scenario) {
			s.Name = ""
			s.Resources.Networks = -1
		}, wantErr: "name must not be empty"},
		{name: "empty provider", mutate: func(s *Scenario) { s.Provider = "" }},
		{name: "unknown provider is not validated locally", mutate: func(s *Scenario) { s.Provider = "no-such-driver" }},

		{name: "negative networks", mutate: func(s *Scenario) { s.Resources.Networks = -1 }, wantErr: "resources.networks must not be negative, got -1"},
		{name: "networks above count limit", mutate: func(s *Scenario) { s.Resources.Networks = 1_000_001 }, wantErr: "resources.networks exceeds the limit of 1000000, got 1000001"},
		{name: "negative loadbalancers", mutate: func(s *Scenario) { s.Resources.LoadBalancers = -1 }, wantErr: "resources.loadbalancers must not be negative, got -1"},
		{name: "loadbalancers above count limit", mutate: func(s *Scenario) { s.Resources.LoadBalancers = 1_000_001 }, wantErr: "resources.loadbalancers exceeds the limit of 1000000, got 1000001"},
		{name: "negative listeners", mutate: func(s *Scenario) { s.Resources.Listeners = -1 }, wantErr: "resources.listeners must not be negative, got -1"},
		{name: "listeners above count limit", mutate: func(s *Scenario) { s.Resources.Listeners = 1_000_001 }, wantErr: "resources.listeners exceeds the limit of 1000000, got 1000001"},
		{name: "negative members", mutate: func(s *Scenario) { s.Resources.Members = -1 }, wantErr: "resources.members must not be negative, got -1"},
		{name: "members above count limit", mutate: func(s *Scenario) { s.Resources.Members = 1_000_001 }, wantErr: "resources.members exceeds the limit of 1000000, got 1000001"},
		{name: "negative health monitors", mutate: func(s *Scenario) { s.Resources.HealthMonitors = -1 }, wantErr: "resources.health_monitors must not be negative, got -1"},
		{name: "health monitors above count limit", mutate: func(s *Scenario) { s.Resources.HealthMonitors = 1_000_001 }, wantErr: "resources.health_monitors exceeds the limit of 1000000, got 1000001"},
		{name: "count limit before the network limit", mutate: func(s *Scenario) {
			s.Resources.Networks = 256
			s.Resources.HealthMonitors = -1
		}, wantErr: "resources.health_monitors must not be negative, got -1"},

		{name: "networks above the VIP CIDR limit", mutate: func(s *Scenario) { s.Resources.Networks = 256 }, wantErr: "resources.networks exceeds the limit of 255, got 256"},
		{name: "networks at the VIP CIDR limit", mutate: func(s *Scenario) { s.Resources.Networks = 255 }},

		{name: "load balancers without networks", mutate: func(s *Scenario) { s.Resources.Networks = 0 }, wantErr: "resources.networks must be at least 1 when resources.loadbalancers > 0, got 0"},
		{name: "listeners without load balancers", mutate: func(s *Scenario) {
			s.Resources.LoadBalancers = 0
			s.Resources.Listeners = 2
		}, wantErr: "resources.loadbalancers must be at least 1 when resources.listeners > 0, got 0"},
		{name: "members without listeners", mutate: func(s *Scenario) {
			s.Resources.Listeners = 0
			s.Resources.Members = 3
		}, wantErr: "resources.listeners must be at least 1 when resources.members > 0, got 0"},
		{name: "networks without load balancers", mutate: func(s *Scenario) {
			s.Resources = Resources{Networks: 3}
		}},

		{name: "fewer listeners than load balancers", mutate: func(s *Scenario) { s.Resources.Listeners = 1 }, wantErr: "resources.listeners (1) must be at least resources.loadbalancers (2)"},
		{name: "fewer members than listeners", mutate: func(s *Scenario) { s.Resources.Members = 3 }, wantErr: "resources.members (3) must be at least resources.listeners (4)"},
		{name: "more health monitors than listeners", mutate: func(s *Scenario) { s.Resources.HealthMonitors = 5 }, wantErr: "resources.health_monitors (5) exceeds resources.listeners (4)"},
		{name: "a listener per load balancer and a member per pool", mutate: func(s *Scenario) {
			s.Resources = Resources{Networks: 1, LoadBalancers: 4, Listeners: 4, Members: 4, HealthMonitors: 4}
		}},
		{name: "relation rules before the port ranges", mutate: func(s *Scenario) {
			s.Resources.HealthMonitors = 5
			s.Distribution.ListenerPort.Min = 0
		}, wantErr: "resources.health_monitors (5) exceeds resources.listeners (4)"},

		{name: "listener port min zero", mutate: func(s *Scenario) { s.Distribution.ListenerPort.Min = 0 }, wantErr: "distribution.listener_port.min must be between 1 and 65535, got 0"},
		{name: "listener port min above 65535", mutate: func(s *Scenario) { s.Distribution.ListenerPort.Min = 65536 }, wantErr: "distribution.listener_port.min must be between 1 and 65535, got 65536"},
		{name: "listener port max zero", mutate: func(s *Scenario) { s.Distribution.ListenerPort.Max = 0 }, wantErr: "distribution.listener_port.max must be between 1 and 65535, got 0"},
		{name: "listener port max above 65535", mutate: func(s *Scenario) { s.Distribution.ListenerPort.Max = 65536 }, wantErr: "distribution.listener_port.max must be between 1 and 65535, got 65536"},
		{name: "listener port min above max", mutate: func(s *Scenario) {
			s.Distribution.ListenerPort = Range{Min: 2000, Max: 1000}
		}, wantErr: "distribution.listener_port.min (2000) must not exceed distribution.listener_port.max (1000)"},
		{name: "member port min zero", mutate: func(s *Scenario) { s.Distribution.MemberPort.Min = 0 }, wantErr: "distribution.member_port.min must be between 1 and 65535, got 0"},
		{name: "member port min above 65535", mutate: func(s *Scenario) { s.Distribution.MemberPort.Min = 65536 }, wantErr: "distribution.member_port.min must be between 1 and 65535, got 65536"},
		{name: "member port max zero", mutate: func(s *Scenario) { s.Distribution.MemberPort.Max = 0 }, wantErr: "distribution.member_port.max must be between 1 and 65535, got 0"},
		{name: "member port max above 65535", mutate: func(s *Scenario) { s.Distribution.MemberPort.Max = 65536 }, wantErr: "distribution.member_port.max must be between 1 and 65535, got 65536"},
		{name: "member port min above max", mutate: func(s *Scenario) {
			s.Distribution.MemberPort = Range{Min: 2000, Max: 1000}
		}, wantErr: "distribution.member_port.min (2000) must not exceed distribution.member_port.max (1000)"},
		{name: "listener port before member port", mutate: func(s *Scenario) {
			s.Distribution.ListenerPort.Max = 0
			s.Distribution.MemberPort.Min = 0
		}, wantErr: "distribution.listener_port.max must be between 1 and 65535, got 0"},
		{name: "min bounds before max bounds", mutate: func(s *Scenario) {
			s.Distribution.ListenerPort = Range{Min: 0, Max: 0}
		}, wantErr: "distribution.listener_port.min must be between 1 and 65535, got 0"},
		{name: "single-port ranges at the bounds", mutate: func(s *Scenario) {
			s.Resources = Resources{Networks: 1, LoadBalancers: 1, Listeners: 1, Members: 1}
			s.Distribution.ListenerPort = Range{Min: 65535, Max: 65535}
			s.Distribution.MemberPort = Range{Min: 1, Max: 1}
		}},
		{name: "port ranges are checked without any listener", mutate: func(s *Scenario) {
			s.Resources = Resources{}
			s.Distribution.MemberPort.Min = 0
		}, wantErr: "distribution.member_port.min must be between 1 and 65535, got 0"},

		{name: "listener port range narrower than one load balancer's listeners", mutate: func(s *Scenario) {
			s.Resources = Resources{Networks: 1, LoadBalancers: 1, Listeners: 3, Members: 3}
			s.Distribution.ListenerPort = Range{Min: 1024, Max: 1025}
		}, wantErr: "distribution.listener_port spans 2 ports, fewer than the 3 listeners one load balancer gets"},
		{name: "listener port span counts the busiest load balancer", mutate: func(s *Scenario) {
			s.Resources = Resources{Networks: 1, LoadBalancers: 2, Listeners: 5, Members: 5}
			s.Distribution.ListenerPort = Range{Min: 1024, Max: 1025}
		}, wantErr: "distribution.listener_port spans 2 ports, fewer than the 3 listeners one load balancer gets"},
		{name: "listener port range exactly wide enough", mutate: func(s *Scenario) {
			s.Resources = Resources{Networks: 1, LoadBalancers: 2, Listeners: 5, Members: 5}
			s.Distribution.ListenerPort = Range{Min: 1024, Max: 1026}
		}},
		{name: "two-port listener range for two listeners per load balancer", mutate: func(s *Scenario) {
			s.Distribution.ListenerPort = Range{Min: 80, Max: 81}
		}},

		{name: "128 members on one network", mutate: func(s *Scenario) {
			s.Resources = Resources{Networks: 1, LoadBalancers: 1, Listeners: 1, Members: 128}
		}, wantErr: "resources.members puts 128 members on VIP network net-0001, more than the 127 member addresses in the upper half of its /24"},
		{name: "127 members on one network", mutate: func(s *Scenario) {
			s.Resources = Resources{Networks: 1, LoadBalancers: 1, Listeners: 1, Members: 127}
		}},
		{name: "members of one load balancer stay on its network", mutate: func(s *Scenario) {
			// An even split over the two networks would put 64 on each, but the one
			// load balancer sits on net-0001 and so do all its members.
			s.Resources = Resources{Networks: 2, LoadBalancers: 1, Listeners: 2, Members: 128}
		}, wantErr: "resources.members puts 128 members on VIP network net-0001, more than the 127 member addresses in the upper half of its /24"},
		{name: "members follow the round-robin onto the busier network", mutate: func(s *Scenario) {
			// lb-0001 and lb-0003 sit on net-0001, so two of every three members
			// land there: 128 of 192, against 64 on net-0002.
			s.Resources = Resources{Networks: 2, LoadBalancers: 3, Listeners: 3, Members: 192}
		}, wantErr: "resources.members puts 128 members on VIP network net-0001, more than the 127 member addresses in the upper half of its /24"},
		{name: "members just fit the busier network", mutate: func(s *Scenario) {
			// 127 on net-0001 and 64 on net-0002.
			s.Resources = Resources{Networks: 2, LoadBalancers: 3, Listeners: 3, Members: 191}
		}},
		{name: "two networks of 127 members each", mutate: func(s *Scenario) {
			s.Resources = Resources{Networks: 2, LoadBalancers: 2, Listeners: 2, Members: 254}
		}},
		{name: "one member too many for two networks", mutate: func(s *Scenario) {
			// Two load balancers on two networks split 255 members 128 to 127.
			s.Resources = Resources{Networks: 2, LoadBalancers: 2, Listeners: 2, Members: 255}
		}, wantErr: "resources.members puts 128 members on VIP network net-0001, more than the 127 member addresses in the upper half of its /24"},

		{name: "all-zero resources", mutate: func(s *Scenario) { s.Resources = Resources{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			tc.mutate(&s)
			err := s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Validate() = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateZeroDivisorGuards confirms each of the three guards that keep the
// generator's round-robins off a zero divisor rejects its input with the exact
// message, from Validate and from Generate alike, without panicking.
func TestValidateZeroDivisorGuards(t *testing.T) {
	tests := []struct {
		name      string
		resources Resources
		wantErr   string
	}{
		{
			name:      "load balancers without networks",
			resources: Resources{Networks: 0, LoadBalancers: 2, Listeners: 2, Members: 2},
			wantErr:   "resources.networks must be at least 1 when resources.loadbalancers > 0, got 0",
		},
		{
			name:      "listeners without load balancers",
			resources: Resources{Networks: 1, LoadBalancers: 0, Listeners: 2, Members: 2},
			wantErr:   "resources.loadbalancers must be at least 1 when resources.listeners > 0, got 0",
		},
		{
			name:      "members without listeners",
			resources: Resources{Networks: 1, LoadBalancers: 0, Listeners: 0, Members: 3},
			wantErr:   "resources.listeners must be at least 1 when resources.members > 0, got 0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			s.Resources = tc.resources
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked on resources %+v: %v", tc.resources, r)
				}
			}()
			if err := s.Validate(); err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Validate() = %v, want %q", err, tc.wantErr)
			}
			if _, err := s.Generate(); err == nil || err.Error() != "invalid scenario: "+tc.wantErr {
				t.Fatalf("Generate() = %v, want %q", err, "invalid scenario: "+tc.wantErr)
			}
		})
	}
}

// TestSet asserts each of the eleven documented override keys parses into its
// field.
func TestSet(t *testing.T) {
	keys := []struct {
		key   string
		value string
		check func(Scenario) bool
	}{
		{"seed", "7", func(s Scenario) bool { return s.Seed == 7 }},
		{"provider", "ovn", func(s Scenario) bool { return s.Provider == "ovn" }},
		{"resources.networks", "3", func(s Scenario) bool { return s.Resources.Networks == 3 }},
		{"resources.loadbalancers", "5", func(s Scenario) bool { return s.Resources.LoadBalancers == 5 }},
		{"resources.listeners", "10", func(s Scenario) bool { return s.Resources.Listeners == 10 }},
		{"resources.members", "30", func(s Scenario) bool { return s.Resources.Members == 30 }},
		{"resources.health_monitors", "0", func(s Scenario) bool { return s.Resources.HealthMonitors == 0 }},
		{"distribution.listener_port.min", "80", func(s Scenario) bool { return s.Distribution.ListenerPort.Min == 80 }},
		{"distribution.listener_port.max", "90", func(s Scenario) bool { return s.Distribution.ListenerPort.Max == 90 }},
		{"distribution.member_port.min", "8080", func(s Scenario) bool { return s.Distribution.MemberPort.Min == 8080 }},
		{"distribution.member_port.max", "8090", func(s Scenario) bool { return s.Distribution.MemberPort.Max == 8090 }},
	}
	for _, k := range keys {
		t.Run(k.key, func(t *testing.T) {
			s := smallScenario()
			if err := s.Set(k.key, k.value); err != nil {
				t.Fatalf("Set(%q, %q) = %v, want nil", k.key, k.value, err)
			}
			if !k.check(s) {
				t.Errorf("Set(%q, %q) did not apply: %+v", k.key, k.value, s)
			}
		})
	}
}

// TestSetProviderEmpty confirms an explicit empty provider override clears a
// provider the scenario set, selecting the cloud's default again.
func TestSetProviderEmpty(t *testing.T) {
	s := smallScenario()
	s.Provider = "amphora"
	if err := s.Set("provider", ""); err != nil {
		t.Fatalf(`Set("provider", "") = %v, want nil`, err)
	}
	if s.Provider != "" {
		t.Errorf(`Set("provider", "") left provider %q, want ""`, s.Provider)
	}
}

// TestSetErrors asserts an unknown key and a value that does not parse to the
// field's type each fail with a message naming the key, leaving the scenario
// unchanged. pools is unknown because the listener count is also the pool
// count, and chaos keys are unknown because the chaos block has no --set keys;
// the chaos command's flags override it.
func TestSetErrors(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr string
	}{
		{"pools", "pools", "3", `unknown override key "pools"`},
		{"resources.pools", "resources.pools", "3", `unknown override key "resources.pools"`},
		{"chaos key", "chaos.duration", "5m", `unknown override key "chaos.duration"`},
		{"barbican key", "resources.secrets", "1", `unknown override key "resources.secrets"`},
		{"non-integer count", "resources.listeners", "x", `override resources.listeners: "x" is not an integer`},
		{"non-integer port", "distribution.member_port.max", "high", `override distribution.member_port.max: "high" is not an integer`},
		{"non-integer seed", "seed", "x", `override seed: "x" is not an integer`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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

// TestValidateChaosBlock asserts Validate accepts a chaos block at the edges
// of each bound and rejects each malformed chaos value, one violated rule at a
// time, with the exact message naming the offending key. Each block is parsed
// from YAML so the duration strings go through Duration.UnmarshalYAML as they
// would in a scenario file.
func TestValidateChaosBlock(t *testing.T) {
	tests := []struct {
		name  string
		block string
		// wantErr is the exact expected error; empty means valid.
		wantErr string
	}{
		{name: "zero duration", block: "  duration: 0s\n"},
		{name: "zero ratios and fan-out", block: "  parallel: { max: 0 }\n  churn_ratio: 0\n  target_fill: 0\n  update_ratio: 0\n"},
		{name: "ratios at one", block: "  churn_ratio: 1\n  target_fill: 1\n  update_ratio: 1\n"},
		{name: "interval min equals max", block: "  interval: { min: 1s, max: 1s }\n"},

		{name: "negative duration", block: "  duration: -1m\n", wantErr: "chaos.duration must not be negative, got -1m0s"},
		{name: "negative bucket width", block: "  bucket_width: -1m\n", wantErr: "chaos.bucket_width must not be negative, got -1m0s"},
		{name: "negative interval min", block: "  interval: { min: -1s, max: 1s }\n", wantErr: "chaos.interval.min must not be negative, got -1s"},
		{name: "interval min above max", block: "  interval: { min: 3s, max: 1s }\n", wantErr: "chaos.interval.min (3s) must not exceed chaos.interval.max (1s)"},
		{name: "negative parallel max", block: "  parallel: { max: -1 }\n", wantErr: "chaos.parallel.max must not be negative, got -1"},
		{name: "churn ratio below zero", block: "  churn_ratio: -0.1\n", wantErr: "chaos.churn_ratio must be between 0 and 1, got -0.1"},
		{name: "churn ratio above one", block: "  churn_ratio: 1.5\n", wantErr: "chaos.churn_ratio must be between 0 and 1, got 1.5"},
		{name: "target fill below zero", block: "  target_fill: -0.1\n", wantErr: "chaos.target_fill must be between 0 and 1, got -0.1"},
		{name: "target fill above one", block: "  target_fill: 1.5\n", wantErr: "chaos.target_fill must be between 0 and 1, got 1.5"},
		{name: "update ratio below zero", block: "  update_ratio: -0.1\n", wantErr: "chaos.update_ratio must be between 0 and 1, got -0.1"},
		{name: "update ratio above one", block: "  update_ratio: 1.5\n", wantErr: "chaos.update_ratio must be between 0 and 1, got 1.5"},
		{name: "duration before update ratio", block: "  duration: -1m\n  update_ratio: 1.5\n", wantErr: "chaos.duration must not be negative, got -1m0s"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := Parse([]byte("name: x\nchaos:\n" + tc.block))
			if err != nil {
				t.Fatalf("Parse() = %v, want nil", err)
			}
			s := smallScenario()
			s.Chaos = parsed.Chaos
			err = s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Validate() = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateChecksChaosBlockLast confirms Validate reports a violated
// resource rule before a malformed chaos block, so the chaos block is checked
// after every other rule.
func TestValidateChecksChaosBlockLast(t *testing.T) {
	s := smallScenario()
	s.Resources = Resources{Networks: 1, LoadBalancers: 1, Listeners: 1, Members: 128}
	s.Chaos = &Chaos{UpdateRatio: floatPtr(1.5)}
	want := "resources.members puts 128 members on VIP network net-0001, more than the 127 member addresses in the upper half of its /24"
	if err := s.Validate(); err == nil || err.Error() != want {
		t.Fatalf("Validate() = %v, want %q", err, want)
	}
}
