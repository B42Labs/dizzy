package scenario

import (
	"bytes"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	cinderscenario "github.com/B42Labs/dizzy/internal/cinder/scenario"
	glancescenario "github.com/B42Labs/dizzy/internal/glance/scenario"
	keystonescenario "github.com/B42Labs/dizzy/internal/keystone/scenario"
	neutronscenario "github.com/B42Labs/dizzy/internal/scenario"
	"github.com/B42Labs/dizzy/scenarios"
)

// noRead is a readFile that fails the test when LoadLanes calls it.
func noRead(t *testing.T) func(string) ([]byte, error) {
	t.Helper()
	return func(path string) ([]byte, error) {
		t.Errorf("readFile(%q) called, want no read", path)
		return nil, errors.New("unexpected read")
	}
}

// fileRead is a readFile that serves data for every path and records each
// path it is called with in paths.
func fileRead(data string, paths *[]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		*paths = append(*paths, path)
		return []byte(data), nil
	}
}

// enableAllLanes switches every lane of s on, on the bundled small profile.
func enableAllLanes(s *Scenario) {
	for _, b := range s.Lanes.blocks() {
		*b.lane = Lane{Enabled: true, Profile: "small"}
	}
}

func TestLoadLanesNoneEnabled(t *testing.T) {
	ls, err := smallScenario().LoadLanes(noRead(t))
	if err != nil {
		t.Fatalf("LoadLanes: %v", err)
	}
	if ls != (LaneScenarios{}) {
		t.Errorf("LoadLanes = %+v, want four nil scenarios", ls)
	}
}

// TestLoadLanesProfiles confirms a lane on a profile gets the service's
// embedded profile, named and seeded after the mix scenario and the lane.
func TestLoadLanesProfiles(t *testing.T) {
	s := smallScenario()
	enableAllLanes(&s)
	ls, err := s.LoadLanes(noRead(t))
	if err != nil {
		t.Fatalf("LoadLanes: %v", err)
	}
	if ls.Cinder == nil || ls.Glance == nil || ls.Keystone == nil || ls.Neutron == nil {
		t.Fatalf("LoadLanes = %+v, want all four scenarios", ls)
	}

	profile := func(lane string) []byte {
		data, err := scenarios.Files.ReadFile(lane + "/small.yaml")
		if err != nil {
			t.Fatalf("reading profile %s/small.yaml: %v", lane, err)
		}
		return data
	}
	cinder, _ := cinderscenario.Parse(profile("cinder"))
	cinder.Name, cinder.Seed = "small/cinder", PersonaSeed(42, "cinder")
	glance, _ := glancescenario.Parse(profile("glance"))
	glance.Name, glance.Seed = "small/glance", PersonaSeed(42, "glance")
	keystone, _ := keystonescenario.Parse(profile("keystone"))
	keystone.Name, keystone.Seed = "small/keystone", PersonaSeed(42, "keystone")
	neutron, _ := neutronscenario.Parse(profile("neutron"))
	neutron.Name, neutron.Seed = "small/neutron", PersonaSeed(42, "neutron")

	for _, c := range []struct {
		lane      string
		got, want any
	}{
		{"cinder", *ls.Cinder, cinder},
		{"glance", *ls.Glance, glance},
		{"keystone", *ls.Keystone, keystone},
		{"neutron", *ls.Neutron, neutron},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s scenario = %+v, want %+v", c.lane, c.got, c.want)
		}
	}
}

// TestLoadLanesScenarioFile confirms a lane on a scenario file reads exactly
// its path, and that the file's own seed and chaos duration do not reach the
// lane's plan.
func TestLoadLanesScenarioFile(t *testing.T) {
	s := smallScenario()
	s.Lanes.Cinder.Lane = Lane{Enabled: true, Scenario: "lanes/cinder.yaml"}
	var plans [][]byte
	for _, file := range []string{
		"name: lane\nseed: 1\nresources: { volumes: 2 }\ndistribution: { volume_size_gib: { min: 1, max: 9 } }\nchaos: { duration: 5m }\n",
		"name: lane\nseed: 2\nresources: { volumes: 2 }\ndistribution: { volume_size_gib: { min: 1, max: 9 } }\nchaos: { duration: 1h }\n",
	} {
		var paths []string
		ls, err := s.LoadLanes(fileRead(file, &paths))
		if err != nil {
			t.Fatalf("LoadLanes: %v", err)
		}
		if want := []string{"lanes/cinder.yaml"}; !reflect.DeepEqual(paths, want) {
			t.Errorf("readFile paths = %q, want %q", paths, want)
		}
		p, err := s.Generate(ls)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if len(p.Lanes) != 1 || len(p.Lanes[0].Cinder.Volumes) != 2 {
			t.Fatalf("lanes = %+v, want one cinder lane with the file's two volumes", p.Lanes)
		}
		plans = append(plans, marshal(t, p.Lanes))
	}
	if !bytes.Equal(plans[0], plans[1]) {
		t.Errorf("files differing only in seed and chaos.duration yield different lane plans:\n%s\n%s", plans[0], plans[1])
	}
}

func TestLoadLanesErrors(t *testing.T) {
	t.Run("missing scenario file", func(t *testing.T) {
		s := smallScenario()
		s.Lanes.Cinder.Lane = Lane{Enabled: true, Scenario: "lanes/cinder.yaml"}
		_, err := s.LoadLanes(func(string) ([]byte, error) { return nil, fs.ErrNotExist })
		if want := "lanes.cinder: reading scenario lanes/cinder.yaml: "; err == nil || !strings.HasPrefix(err.Error(), want) || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("LoadLanes = %v, want an error starting with %q that wraps fs.ErrNotExist", err, want)
		}
	})

	t.Run("unknown key in the scenario file", func(t *testing.T) {
		s := smallScenario()
		s.Lanes.Cinder.Lane = Lane{Enabled: true, Scenario: "lanes/cinder.yaml"}
		var paths []string
		_, err := s.LoadLanes(fileRead("name: lane\nresources: { volumez: 2 }\n", &paths))
		if want := "lanes.cinder: parsing scenario:"; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("LoadLanes = %v, want an error starting with %q", err, want)
		}
	})

	t.Run("invalid mix scenario", func(t *testing.T) {
		s := smallScenario()
		s.Lanes.Glance = Lane{Enabled: true, Profile: "huge"}
		_, err := s.LoadLanes(noRead(t))
		if want := "invalid scenario: "; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("LoadLanes = %v, want an error starting with %q", err, want)
		}
	})
}
