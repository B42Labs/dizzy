package scenario

import (
	"strings"
	"testing"
)

// smallScenario is the compact scenario backing the golden test; it mirrors the
// shipped scenarios/barbican/small.yaml so the profile test can tie to it.
func smallScenario() Scenario {
	return Scenario{
		Name: "small",
		Seed: 42,
		Resources: Resources{
			Secrets:    10,
			Orders:     5,
			Containers: 3,
		},
		Distribution: Distribution{
			PayloadBytes:        Range{Min: 64, Max: 8192},
			RetrieveRatio:       0.5,
			SecretsPerContainer: Range{Min: 1, Max: 3},
		},
	}
}

// TestParseDecodesEveryKey confirms a scenario spelling out every key decodes
// into the matching fields.
func TestParseDecodesEveryKey(t *testing.T) {
	yaml := `name: small
seed: 42
resources:
  secrets: 10
  orders: 5
  containers: 3
distribution:
  payload_bytes: { min: 64, max: 8192 }
  retrieve_ratio: 0.5
  secrets_per_container: { min: 1, max: 3 }
`
	s, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() = %v, want nil", err)
	}
	if want := smallScenario(); s != want {
		t.Errorf("Parse() = %+v, want %+v", s, want)
	}
}

// TestParseRejectsUnknownKey confirms strict unmarshal fails loudly on a key
// the schema does not know: a stray leaf borrowed from another service, and a
// chaos block, which the Barbican schema does not carry.
func TestParseRejectsUnknownKey(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "glance resources leaf",
			yaml:    "name: x\nresources:\n  images: 1\n",
			wantErr: "field images not found",
		},
		{
			name:    "chaos block",
			yaml:    "name: x\nresources:\n  secrets: 1\nchaos:\n  duration: 5m\n",
			wantErr: "field chaos not found",
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
// key.
func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Scenario)
		// wantErr is the exact expected error; empty means valid.
		wantErr string
	}{
		{name: "valid", mutate: func(*Scenario) {}},
		{name: "empty name", mutate: func(s *Scenario) { s.Name = "" }, wantErr: "name must not be empty"},

		{name: "negative secrets", mutate: func(s *Scenario) { s.Resources.Secrets = -1 }, wantErr: "resources.secrets must not be negative, got -1"},
		{name: "secrets above limit", mutate: func(s *Scenario) { s.Resources.Secrets = 1_000_001 }, wantErr: "resources.secrets exceeds the limit of 1000000, got 1000001"},
		{name: "negative orders", mutate: func(s *Scenario) { s.Resources.Orders = -1 }, wantErr: "resources.orders must not be negative, got -1"},
		{name: "orders above limit", mutate: func(s *Scenario) { s.Resources.Orders = 1_000_001 }, wantErr: "resources.orders exceeds the limit of 1000000, got 1000001"},
		{name: "negative containers", mutate: func(s *Scenario) { s.Resources.Containers = -1 }, wantErr: "resources.containers must not be negative, got -1"},
		{name: "containers above limit", mutate: func(s *Scenario) { s.Resources.Containers = 1_000_001 }, wantErr: "resources.containers exceeds the limit of 1000000, got 1000001"},
		{name: "counts at the limit", mutate: func(s *Scenario) {
			s.Resources = Resources{Secrets: maxCount, Orders: maxCount, Containers: maxCount}
		}},

		{
			name:    "payload min above max",
			mutate:  func(s *Scenario) { s.Distribution.PayloadBytes = Range{Min: 8192, Max: 64} },
			wantErr: "distribution.payload_bytes.min (8192) must not exceed distribution.payload_bytes.max (64)",
		},
		{
			name:    "negative payload min",
			mutate:  func(s *Scenario) { s.Distribution.PayloadBytes.Min = -1 },
			wantErr: "distribution.payload_bytes.min must not be negative, got -1",
		},
		{
			name:    "payload max above limit",
			mutate:  func(s *Scenario) { s.Distribution.PayloadBytes.Max = 1_000_001 },
			wantErr: "distribution.payload_bytes.max (1000001) exceeds the limit of 1000000",
		},
		{
			name:    "secrets per container min above max",
			mutate:  func(s *Scenario) { s.Distribution.SecretsPerContainer = Range{Min: 3, Max: 1} },
			wantErr: "distribution.secrets_per_container.min (3) must not exceed distribution.secrets_per_container.max (1)",
		},
		{
			name:    "negative secrets per container min",
			mutate:  func(s *Scenario) { s.Distribution.SecretsPerContainer.Min = -1 },
			wantErr: "distribution.secrets_per_container.min must not be negative, got -1",
		},

		{name: "retrieve ratio below zero", mutate: func(s *Scenario) { s.Distribution.RetrieveRatio = -0.1 }, wantErr: "distribution.retrieve_ratio must be between 0 and 1, got -0.1"},
		{name: "retrieve ratio above one", mutate: func(s *Scenario) { s.Distribution.RetrieveRatio = 1.1 }, wantErr: "distribution.retrieve_ratio must be between 0 and 1, got 1.1"},
		{name: "retrieve ratio zero", mutate: func(s *Scenario) { s.Distribution.RetrieveRatio = 0 }},
		{name: "retrieve ratio one", mutate: func(s *Scenario) { s.Distribution.RetrieveRatio = 1 }},

		{
			name:    "payload min zero with secrets",
			mutate:  func(s *Scenario) { s.Distribution.PayloadBytes.Min = 0 },
			wantErr: "distribution.payload_bytes.min must be at least 1 when resources.secrets > 0, got 0",
		},

		{
			name: "secrets per container max above secrets",
			mutate: func(s *Scenario) {
				s.Resources.Secrets = 2
				s.Distribution.SecretsPerContainer.Max = 3
			},
			wantErr: "distribution.secrets_per_container.max (3) exceeds resources.secrets (2)",
		},
		{
			name: "secrets per container max equal to secrets",
			mutate: func(s *Scenario) {
				s.Resources.Secrets = 3
				s.Distribution.SecretsPerContainer.Max = 3
			},
		},
		{
			name: "secrets per container max above secrets without containers",
			mutate: func(s *Scenario) {
				s.Resources.Secrets = 2
				s.Resources.Containers = 0
			},
		},

		{
			name: "zero counts with an empty payload range",
			mutate: func(s *Scenario) {
				s.Resources = Resources{}
				s.Distribution.PayloadBytes = Range{Min: 0, Max: 0}
			},
		},
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

// TestSet asserts each of the nine documented override keys parses into its
// field.
func TestSet(t *testing.T) {
	keys := []struct {
		key   string
		value string
		check func(Scenario) bool
	}{
		{"seed", "7", func(s Scenario) bool { return s.Seed == 7 }},
		{"resources.secrets", "20", func(s Scenario) bool { return s.Resources.Secrets == 20 }},
		{"resources.orders", "8", func(s Scenario) bool { return s.Resources.Orders == 8 }},
		{"resources.containers", "6", func(s Scenario) bool { return s.Resources.Containers == 6 }},
		{"distribution.payload_bytes.min", "128", func(s Scenario) bool { return s.Distribution.PayloadBytes.Min == 128 }},
		{"distribution.payload_bytes.max", "4096", func(s Scenario) bool { return s.Distribution.PayloadBytes.Max == 4096 }},
		{"distribution.retrieve_ratio", "0.25", func(s Scenario) bool { return s.Distribution.RetrieveRatio == 0.25 }},
		{"distribution.secrets_per_container.min", "2", func(s Scenario) bool { return s.Distribution.SecretsPerContainer.Min == 2 }},
		{"distribution.secrets_per_container.max", "5", func(s Scenario) bool { return s.Distribution.SecretsPerContainer.Max == 5 }},
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

// TestSetErrors asserts an unknown key and a value that does not parse to the
// field's type each fail with a message naming the key.
func TestSetErrors(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr string
	}{
		{"unknown key", "resources.images", "1", `unknown override key "resources.images"`},
		{"non-integer count", "resources.secrets", "x", `override resources.secrets: "x" is not an integer`},
		{"non-integer seed", "seed", "x", `override seed: "x" is not an integer`},
		{"non-number ratio", "distribution.retrieve_ratio", "half", `override distribution.retrieve_ratio: "half" is not a number`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScenario()
			err := s.Set(tc.key, tc.value)
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Set(%q, %q) = %v, want %q", tc.key, tc.value, err, tc.wantErr)
			}
			if s != smallScenario() {
				t.Errorf("Set(%q, %q) changed the scenario on error: %+v", tc.key, tc.value, s)
			}
		})
	}
}
