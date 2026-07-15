package scenario

import (
	"encoding/json"
	"fmt"
	"testing"
)

// planJSON expands s and returns its plan encoded as indented JSON, the byte
// form the determinism assertions compare.
func planJSON(t *testing.T, s Scenario) string {
	t.Helper()
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("marshaling plan: %v", err)
	}
	return string(data)
}

func TestGenerateIsDeterministic(t *testing.T) {
	s := smallScenario()
	if first, second := planJSON(t, s), planJSON(t, s); first != second {
		t.Errorf("Generate() not deterministic for the same seed:\n%s\n---\n%s", first, second)
	}
}

func TestGenerateVariesWithSeed(t *testing.T) {
	a := smallScenario()
	b := smallScenario()
	b.Seed = a.Seed + 1
	if planJSON(t, a) == planJSON(t, b) {
		t.Error("Generate() produced identical plans for different seeds")
	}
}

// TestRatioZeroGuardsConsumeNoDraws pins the documented draw order: a scenario
// with every lifecycle ratio at 0 must produce a plan with no lifecycle decision
// on any image, and — because the ratio-0 guards skip the RNG entirely — the size
// draws (the only remaining draws) stay stable. It asserts the resulting plan
// carries only sizes.
func TestRatioZeroGuardsConsumeNoDraws(t *testing.T) {
	base := smallScenario()
	base.Distribution.MetadataUpdateRatio = 0
	base.Distribution.SharedRatio = 0
	base.Distribution.MemberAcceptRatio = 0
	base.Distribution.MemberRemoveRatio = 0
	base.Distribution.CommunityRatio = 0
	base.Distribution.PublicRatio = 0
	base.Distribution.DeactivateRatio = 0
	base.Distribution.DeletedRatio = 0

	p, err := base.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}
	for _, img := range p.Images {
		if img.MetadataUpdate || img.Shared || img.MemberAccept || img.MemberRemove || img.Community || img.Public || img.Deactivate || img.Delete {
			t.Errorf("image %q carries a lifecycle decision despite all ratios being 0: %+v", img.Name, img)
		}
		if img.SizeMiB < base.Distribution.ImageSizeMiB.Min || img.SizeMiB > base.Distribution.ImageSizeMiB.Max {
			t.Errorf("image %q size %d out of range [%d,%d]", img.Name, img.SizeMiB, base.Distribution.ImageSizeMiB.Min, base.Distribution.ImageSizeMiB.Max)
		}
	}
}

// TestMemberDrawsOnlyOnSharedImages asserts an accept or remove is never drawn
// for an image the run did not share, the plan invariant the executor depends on
// (Glance rejects members on a non-shared image).
func TestMemberDrawsOnlyOnSharedImages(t *testing.T) {
	s := smallScenario()
	s.Resources.Images = 200
	s.Distribution.MemberAcceptRatio = 1 // every shared image would accept
	s.Distribution.MemberRemoveRatio = 1 // and remove — if drawn on a non-shared one, this fails
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}
	for _, img := range p.Images {
		if (img.MemberAccept || img.MemberRemove) && !img.Shared {
			t.Errorf("image %q has a member op but is not shared: %+v", img.Name, img)
		}
	}
}

func TestGenerateContiguousCountersAndBounds(t *testing.T) {
	s := smallScenario()
	s.Resources.Images = 12
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}
	if len(p.Images) != 12 {
		t.Fatalf("images = %d, want 12", len(p.Images))
	}
	for i, img := range p.Images {
		if want := fmt.Sprintf("img-%04d", i+1); img.Name != want {
			t.Errorf("image %d name = %q, want %q", i, img.Name, want)
		}
		if img.SizeMiB < s.Distribution.ImageSizeMiB.Min || img.SizeMiB > s.Distribution.ImageSizeMiB.Max {
			t.Errorf("image %q size %d out of range [%d,%d]", img.Name, img.SizeMiB, s.Distribution.ImageSizeMiB.Min, s.Distribution.ImageSizeMiB.Max)
		}
	}
}
