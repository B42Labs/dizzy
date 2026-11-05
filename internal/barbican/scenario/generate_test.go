package scenario

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/B42Labs/dizzy/internal/barbican/plan"
)

var update = flag.Bool("update", false, "update golden files")

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

// generate expands s, failing the test on an error.
func generate(t *testing.T, s Scenario) *plan.Plan {
	t.Helper()
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}
	return p
}

func TestGenerateInvalidScenario(t *testing.T) {
	s := smallScenario()
	s.Name = "" // fails Scenario.Validate

	_, err := s.Generate()
	if want := "invalid scenario: name must not be empty"; err == nil || err.Error() != want {
		t.Fatalf("Generate() = %v, want %q", err, want)
	}
}

func TestGenerateDeterministic(t *testing.T) {
	s := smallScenario()
	if first, second := marshal(t, generate(t, s)), marshal(t, generate(t, s)); !bytes.Equal(first, second) {
		t.Errorf("two generations of the same scenario+seed differ:\n%s\n---\n%s", first, second)
	}
}

func TestGenerateSeedChangesPlan(t *testing.T) {
	s42 := smallScenario()
	s43 := smallScenario()
	s43.Seed = 43

	if bytes.Equal(marshal(t, generate(t, s42)), marshal(t, generate(t, s43))) {
		t.Error("seeds 42 and 43 produced identical plans")
	}
}

func TestGenerateGolden(t *testing.T) {
	got := marshal(t, generate(t, smallScenario()))

	path := filepath.Join("testdata", "golden", "small.plan.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("generated plan differs from golden file %s; run with -update if the change is intended", path)
	}
}

// TestGenerateZeroCountsEncodeEmptyArrays confirms a scenario with no secrets,
// orders, or containers yields a valid plan whose collections encode as [],
// never null.
func TestGenerateZeroCountsEncodeEmptyArrays(t *testing.T) {
	s := smallScenario()
	s.Resources = Resources{}
	s.Distribution.PayloadBytes = Range{Min: 0, Max: 0}

	got := string(marshal(t, generate(t, s)))
	for _, want := range []string{`"secrets": []`, `"orders": []`, `"containers": []`} {
		if !strings.Contains(got, want) {
			t.Errorf("plan JSON lacks %s:\n%s", want, got)
		}
	}
}

// TestGenerateRetrieveRatioZero confirms that with retrieve_ratio 0 no secret
// carries retrieve, and that the ratio-0 guard consumes no draw: the payload
// sizes equal the sequence a source drawing only sizes produces.
func TestGenerateRetrieveRatioZero(t *testing.T) {
	s := smallScenario()
	s.Distribution.RetrieveRatio = 0

	p := generate(t, s)
	if got := string(marshal(t, p)); strings.Contains(got, `"retrieve"`) {
		t.Errorf("plan JSON carries retrieve despite ratio 0:\n%s", got)
	}

	sizes := rand.New(rand.NewSource(s.Seed))
	r := s.Distribution.PayloadBytes
	for _, sec := range p.Secrets {
		if sec.Retrieve {
			t.Errorf("secret %q retrieved despite ratio 0", sec.Name)
		}
		if want := r.Min + sizes.Intn(r.Max-r.Min+1); sec.PayloadBytes != want {
			t.Errorf("secret %q payload = %d, want %d from a size-only draw sequence", sec.Name, sec.PayloadBytes, want)
		}
	}
}

// TestGenerateRetrieveRatioOne confirms that with retrieve_ratio 1 every stored
// secret is read back.
func TestGenerateRetrieveRatioOne(t *testing.T) {
	s := smallScenario()
	s.Distribution.RetrieveRatio = 1

	p := generate(t, s)
	if got, want := p.Retrieves(), len(p.Secrets); got != want {
		t.Errorf("read-backs = %d, want %d (every secret)", got, want)
	}
}

// TestGenerateNoContainerReferences confirms a {0, 0} secrets_per_container
// range still emits every container, each with its references encoded as [].
func TestGenerateNoContainerReferences(t *testing.T) {
	s := smallScenario()
	s.Distribution.SecretsPerContainer = Range{Min: 0, Max: 0}

	var decoded struct {
		Containers []struct {
			Name    string          `json:"name"`
			Secrets json.RawMessage `json:"secrets"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(marshal(t, generate(t, s)), &decoded); err != nil {
		t.Fatalf("decoding plan JSON: %v", err)
	}
	if got, want := len(decoded.Containers), s.Resources.Containers; got != want {
		t.Fatalf("containers = %d, want %d", got, want)
	}
	for _, ct := range decoded.Containers {
		if got := string(ct.Secrets); got != "[]" {
			t.Errorf("container %q secrets = %s, want []", ct.Name, got)
		}
	}
}

// TestGenerateContainerReferences checks every generated container against the
// generator's guarantees: its reference count lies in the drawn range, and its
// references name stored secrets, are distinct, and ascend in stored-secret
// order.
func TestGenerateContainerReferences(t *testing.T) {
	wide := smallScenario()
	wide.Resources = Resources{Secrets: 30, Orders: 4, Containers: 60}
	wide.Distribution.SecretsPerContainer = Range{Min: 0, Max: 30}

	full := smallScenario()
	full.Resources = Resources{Secrets: 4, Containers: 5}
	full.Distribution.SecretsPerContainer = Range{Min: 4, Max: 4}

	for name, s := range map[string]Scenario{"small": smallScenario(), "wide": wide, "every secret": full} {
		t.Run(name, func(t *testing.T) {
			p := generate(t, s)
			if got, want := len(p.Containers), s.Resources.Containers; got != want {
				t.Fatalf("containers = %d, want %d", got, want)
			}
			index := make(map[string]int, len(p.Secrets))
			for i, sec := range p.Secrets {
				index[sec.Name] = i
			}
			r := s.Distribution.SecretsPerContainer
			for _, ct := range p.Containers {
				if n := len(ct.Secrets); n < r.Min || n > r.Max {
					t.Errorf("container %q has %d references, want %d to %d", ct.Name, n, r.Min, r.Max)
				}
				prev := -1
				for _, ref := range ct.Secrets {
					i, ok := index[ref]
					if !ok {
						t.Errorf("container %q references %q, which is not a stored secret", ct.Name, ref)
						continue
					}
					if i <= prev {
						t.Errorf("container %q references %v, want distinct names ascending in stored-secret order", ct.Name, ct.Secrets)
						break
					}
					prev = i
				}
			}
		})
	}
}

// TestGenerateCountersAndBounds confirms each collection is emitted with
// contiguous logical names and the exact count, and that every payload size
// lies in the drawn range.
func TestGenerateCountersAndBounds(t *testing.T) {
	s := smallScenario()
	s.Resources = Resources{Secrets: 25, Orders: 12, Containers: 7}

	p := generate(t, s)
	if len(p.Secrets) != 25 || len(p.Orders) != 12 || len(p.Containers) != 7 {
		t.Fatalf("counts = %d/%d/%d, want 25/12/7", len(p.Secrets), len(p.Orders), len(p.Containers))
	}
	r := s.Distribution.PayloadBytes
	for i, sec := range p.Secrets {
		if want := fmt.Sprintf("sec-%04d", i+1); sec.Name != want {
			t.Errorf("secret %d name = %q, want %q", i, sec.Name, want)
		}
		if sec.PayloadBytes < r.Min || sec.PayloadBytes > r.Max {
			t.Errorf("secret %q payload %d out of range [%d,%d]", sec.Name, sec.PayloadBytes, r.Min, r.Max)
		}
	}
	for i, o := range p.Orders {
		if want := fmt.Sprintf("ord-%04d", i+1); o.Name != want {
			t.Errorf("order %d name = %q, want %q", i, o.Name, want)
		}
	}
	for i, ct := range p.Containers {
		if want := fmt.Sprintf("ctr-%04d", i+1); ct.Name != want {
			t.Errorf("container %d name = %q, want %q", i, ct.Name, want)
		}
	}
}
