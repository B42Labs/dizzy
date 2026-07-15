package glancegraph

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/glance"
	glanceplan "github.com/B42Labs/dizzy/internal/glance/plan"
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

// TestBuildShape confirms the graph is one node per image, that every node is
// parentless and gateless (images carry no cross-image dependencies), and that
// Mutate is set exactly for the images with a planned lifecycle operation.
func TestBuildShape(t *testing.T) {
	p := churnPlan()
	nodes, err := Build(p, newFakeGlance(), p.Seed, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(nodes) != len(p.Images) {
		t.Fatalf("built %d nodes, want %d", len(nodes), len(p.Images))
	}

	for _, img := range p.Images {
		node := nodeByKey(t, nodes, img.Name)
		if node.Kind != glance.KindImage {
			t.Errorf("image node %q kind = %q, want image", img.Name, node.Kind)
		}
		if len(node.Parents) != 0 {
			t.Errorf("image node %q parents = %v, want none", img.Name, node.Parents)
		}
		if node.Gate != nil {
			t.Errorf("image node %q has a gate, want none", img.Name)
		}
		wantMutable := img.MetadataUpdate || img.Shared || img.Community || img.Public || img.Deactivate
		if (node.Mutate != nil) != wantMutable {
			t.Errorf("image node %q mutable=%v, want %v", img.Name, node.Mutate != nil, wantMutable)
		}
	}
}

// TestBuildRejectsInvalidPlan confirms Build surfaces a plan validation error
// (here an image whose member op is not backed by a shared flag) rather than
// emitting a node that would fail every mutation.
func TestBuildRejectsInvalidPlan(t *testing.T) {
	p := &glanceplan.Plan{
		Images: []glanceplan.Image{{Name: "img-0001", SizeMiB: 1, MemberAccept: true}},
	}
	if _, err := Build(p, newFakeGlance(), 1, time.Minute); err == nil {
		t.Fatal("Build of an invalid plan: expected an error, got nil")
	}
}

// TestImmutableImageHasNoMutate confirms an image with no lifecycle operation is
// created and deleted but never mutated.
func TestImmutableImageHasNoMutate(t *testing.T) {
	p := &glanceplan.Plan{Images: []glanceplan.Image{{Name: "img-0001", SizeMiB: 1}}}
	nodes, err := Build(p, newFakeGlance(), 1, time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if node := nodeByKey(t, nodes, "img-0001"); node.Mutate != nil {
		t.Error("image with no lifecycle operation should not be mutable")
	}
}

func TestClassify(t *testing.T) {
	respErr := func(status int, body string) error {
		return gophercloud.ErrUnexpectedResponseCode{Actual: status, Body: []byte(body)}
	}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"canceled", context.Canceled, "canceled"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"quota", glance.ErrQuota, "quota"},
		{"not found", respErr(404, "gone"), "not-found"},
		{"transient", respErr(503, "unavailable"), "transient"},
		{"other", errors.New("boom"), "other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
