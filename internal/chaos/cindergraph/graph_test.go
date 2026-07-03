package cindergraph

import (
	"testing"
	"time"

	"github.com/B42Labs/openstack-tester/internal/chaos"
	"github.com/B42Labs/openstack-tester/internal/cinder"
	cinderplan "github.com/B42Labs/openstack-tester/internal/cinder/plan"
)

func nodeByKey(t *testing.T, nodes []chaos.Node, key string) chaos.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Key == key {
			return n
		}
	}
	t.Fatalf("no node with key %q in %d nodes", key, len(nodes))
	return chaos.Node{}
}

// TestBuildShape confirms the graph is one node per volume and snapshot, that a
// snapshot is parented on its source volume, and that Mutate is set exactly for
// the volumes with a resize target.
func TestBuildShape(t *testing.T) {
	p := churnPlan()
	nodes, err := Build(p, newFakeCinder(), "", time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(nodes) != len(p.Volumes)+len(p.Snapshots) {
		t.Fatalf("built %d nodes, want %d (volumes + snapshots)", len(nodes), len(p.Volumes)+len(p.Snapshots))
	}

	for _, v := range p.Volumes {
		n := nodeByKey(t, nodes, v.Name)
		if n.Kind != cinder.KindVolume {
			t.Errorf("volume node %q has kind %q, want %q", v.Name, n.Kind, cinder.KindVolume)
		}
		if len(n.Parents) != 0 {
			t.Errorf("volume node %q has parents %v, want none", v.Name, n.Parents)
		}
		// Mutate is set exactly when the volume has a resize target.
		if (n.Mutate != nil) != (v.ResizeToGiB > 0) {
			t.Errorf("volume node %q mutable=%v, want %v (resizeToGiB=%d)", v.Name, n.Mutate != nil, v.ResizeToGiB > 0, v.ResizeToGiB)
		}
	}

	for _, s := range p.Snapshots {
		n := nodeByKey(t, nodes, s.Name)
		if n.Kind != cinder.KindSnapshot {
			t.Errorf("snapshot node %q has kind %q, want %q", s.Name, n.Kind, cinder.KindSnapshot)
		}
		if len(n.Parents) != 1 || n.Parents[0] != s.Volume {
			t.Errorf("snapshot node %q parents = %v, want [%q]", s.Name, n.Parents, s.Volume)
		}
		if n.Mutate != nil {
			t.Errorf("snapshot node %q is mutable, want not", s.Name)
		}
	}
}

// TestBuildRejectsInvalidPlan confirms Build surfaces a plan validation error
// (here a snapshot referencing an unknown volume) rather than emitting a node
// whose parent can never become present.
func TestBuildRejectsInvalidPlan(t *testing.T) {
	p := &cinderplan.Plan{
		Volumes:   []cinderplan.Volume{{Name: "vol-1", SizeGiB: 1}},
		Snapshots: []cinderplan.Snapshot{{Name: "snap-1", Volume: "ghost"}},
	}
	if _, err := Build(p, newFakeCinder(), "", time.Minute); err == nil {
		t.Fatal("Build of a plan with a dangling snapshot reference: expected an error, got nil")
	}
}
