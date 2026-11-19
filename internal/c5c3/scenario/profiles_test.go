package scenario

import (
	"bytes"
	"flag"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/B42Labs/dizzy/scenarios"
)

var update = flag.Bool("update", false, "update golden files")

// readProfile reads and parses a shipped c5c3 profile by its path in the
// embedded scenarios filesystem, so the test does not depend on the process
// working directory.
func readProfile(t *testing.T, file string) Scenario {
	t.Helper()
	data, err := scenarios.Files.ReadFile(file)
	if err != nil {
		t.Fatalf("reading profile %s: %v", file, err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing profile %s: %v", file, err)
	}
	return s
}

// TestProfileGeneratesValidPlan asserts every c5c3 profile embedded under
// c5c3/, small.yaml among them, parses, names itself after its file,
// validates, and generates a valid plan.
func TestProfileGeneratesValidPlan(t *testing.T) {
	files, err := fs.Glob(scenarios.Files, "c5c3/*.yaml")
	if err != nil {
		t.Fatalf("listing c5c3 profiles: %v", err)
	}
	if !slices.Contains(files, "c5c3/small.yaml") {
		t.Fatalf("scenarios.Files holds the c5c3 profiles %v, want c5c3/small.yaml among them", files)
	}
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			s := readProfile(t, file)
			if want := strings.TrimSuffix(path.Base(file), ".yaml"); s.Name != want {
				t.Errorf("profile %s has name %q, want %q", file, s.Name, want)
			}
			if err := s.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if _, err := s.Generate(); err != nil {
				t.Fatalf("Generate() = %v, want nil", err)
			}
		})
	}
}

// TestSmallProfileMatchesGoldenFixture asserts the shipped small profile
// parses to the smallScenario fixture the other tests use, and that its plan,
// encoded as the generate command encodes it, is byte-equal to the golden
// file. Run it with -update to rewrite the golden file after an intended
// change.
func TestSmallProfileMatchesGoldenFixture(t *testing.T) {
	s := readProfile(t, "c5c3/small.yaml")
	if want := smallScenario(); !reflect.DeepEqual(s, want) {
		t.Errorf("scenarios/c5c3/small.yaml = %+v, want %+v", s, want)
	}
	got := marshal(t, generate(t, s))

	golden := filepath.Join("testdata", "golden", "small.plan.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading golden file (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("plan of scenarios/c5c3/small.yaml differs from golden file %s; run with -update if the change is intended:\n%s", golden, got)
	}
}
