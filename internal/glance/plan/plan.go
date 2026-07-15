// Package plan defines the expanded, fully-enumerated set of Glance images — the
// expected-state source of truth produced deterministically from a scenario plus
// a seed. Like the neutron, cinder, and nova plans it is pure data: every
// collection is a slice (never a map) so that encoding the plan to JSON yields
// byte-identical output for the same input. Each image is created by dizzy
// itself with a synthetic, generated data payload of a configurable size, so a
// run depends on no pre-existing image and the byte volume it pushes through the
// service is a scenario parameter rather than an accident of the cloud.
package plan

import (
	"fmt"
	"strings"
)

// Plan is the fully-expanded expected state for one Glance run. Scenario and
// Seed record the provenance that produced it; the slice enumerates every image
// and the lifecycle operations it is driven through.
type Plan struct {
	Scenario string  `json:"scenario"`
	Seed     int64   `json:"seed"`
	Images   []Image `json:"images"`
}

// Image is one image dizzy creates, uploads a synthetic payload to, and drives
// through its lifecycle. SizeMiB is the size of the generated upload payload in
// mebibytes. MetadataUpdate churns the image's custom properties (an add pass
// then a replace/remove pass). Shared transitions the image's visibility to
// shared and adds the run's own project as a member; MemberAccept then accepts
// that membership and MemberRemove later removes it — both are only valid when
// Shared, since Glance requires a shared image to have members. Community and
// Public transition visibility to community and public respectively (public
// needs the admin-only publicize_image policy). Deactivate runs a
// deactivate/reactivate cycle. Delete deletes the image during the run.
type Image struct {
	Name           string `json:"name"`
	SizeMiB        int    `json:"sizeMiB"`
	MetadataUpdate bool   `json:"metadataUpdate,omitempty"`
	Shared         bool   `json:"shared,omitempty"`
	MemberAccept   bool   `json:"memberAccept,omitempty"`
	MemberRemove   bool   `json:"memberRemove,omitempty"`
	Community      bool   `json:"community,omitempty"`
	Public         bool   `json:"public,omitempty"`
	Deactivate     bool   `json:"deactivate,omitempty"`
	Delete         bool   `json:"delete,omitempty"`
}

// Validate checks the plan for well-formedness: every image has a unique name
// and a positive payload size, and a member accept or remove is only scheduled
// on a shared image (Glance rejects members on a non-shared image). It returns
// an error naming the first offending image.
func (p *Plan) Validate() error {
	seen := make(map[string]bool, len(p.Images))
	for _, img := range p.Images {
		if img.Name == "" {
			return fmt.Errorf("image has an empty name")
		}
		if seen[img.Name] {
			return fmt.Errorf("duplicate image name %q", img.Name)
		}
		seen[img.Name] = true
		if img.SizeMiB < 1 {
			return fmt.Errorf("image %q has payload size %d MiB, want at least 1", img.Name, img.SizeMiB)
		}
		if (img.MemberAccept || img.MemberRemove) && !img.Shared {
			return fmt.Errorf("image %q has a member operation but is not shared", img.Name)
		}
	}
	return nil
}

// MetadataUpdates counts the images the plan churns properties on.
func (p *Plan) MetadataUpdates() int {
	return p.count(func(img Image) bool { return img.MetadataUpdate })
}

// SharedCount counts the images the plan transitions to shared.
func (p *Plan) SharedCount() int {
	return p.count(func(img Image) bool { return img.Shared })
}

// MemberAccepts counts the images whose shared membership the plan accepts.
func (p *Plan) MemberAccepts() int {
	return p.count(func(img Image) bool { return img.MemberAccept })
}

// MemberRemoves counts the images whose shared membership the plan removes.
func (p *Plan) MemberRemoves() int {
	return p.count(func(img Image) bool { return img.MemberRemove })
}

// CommunityFlips counts the images the plan transitions to community.
func (p *Plan) CommunityFlips() int {
	return p.count(func(img Image) bool { return img.Community })
}

// PublicFlips counts the images the plan transitions to public.
func (p *Plan) PublicFlips() int {
	return p.count(func(img Image) bool { return img.Public })
}

// Deactivates counts the images the plan deactivates and reactivates.
func (p *Plan) Deactivates() int {
	return p.count(func(img Image) bool { return img.Deactivate })
}

// Deletes counts the images the plan deletes during the run.
func (p *Plan) Deletes() int {
	return p.count(func(img Image) bool { return img.Delete })
}

// TotalUploadMiB sums the payload size across every image, the byte volume the
// run pushes through the service (in mebibytes).
func (p *Plan) TotalUploadMiB() int {
	var total int
	for _, img := range p.Images {
		total += img.SizeMiB
	}
	return total
}

// count returns the number of images satisfying pred.
func (p *Plan) count(pred func(Image) bool) int {
	var n int
	for _, img := range p.Images {
		if pred(img) {
			n++
		}
	}
	return n
}

// Summary returns a deterministic, human-readable count of the plan, used by
// "glance apply --dry-run" to preview a scenario without touching a cloud.
func (p *Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan for scenario %q (seed %d)\n", p.Scenario, p.Seed)
	fmt.Fprintf(&b, "  images:          %d\n", len(p.Images))
	fmt.Fprintf(&b, "  upload total:    %d MiB\n", p.TotalUploadMiB())
	fmt.Fprintf(&b, "  metadata churn:  %d\n", p.MetadataUpdates())
	fmt.Fprintf(&b, "  shared:          %d (%d accepted, %d removed)\n", p.SharedCount(), p.MemberAccepts(), p.MemberRemoves())
	fmt.Fprintf(&b, "  community:       %d\n", p.CommunityFlips())
	fmt.Fprintf(&b, "  public:          %d\n", p.PublicFlips())
	fmt.Fprintf(&b, "  deactivations:   %d\n", p.Deactivates())
	fmt.Fprintf(&b, "  deletes:         %d\n", p.Deletes())
	return b.String()
}
