// Package glancegraph builds the Glance churn graph the service-neutral chaos
// engine schedules. It maps a Glance plan onto engine nodes: one image node per
// planned image, created (with its synthetic payload uploaded and waited to
// active) and later deleted, and — when the image carries a planned lifecycle
// operation — mutable through that lifecycle as the engine's mutate action.
// Readiness is folded into each operation — a create completes only when the
// image reaches active, and a delete only when it is gone — and images carry no
// cross-image dependencies, so every node is parentless and gateless. Keeping the
// Glance coupling here leaves the chaos engine free of any service-specific
// import.
package glancegraph

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/glance"
	glanceexec "github.com/B42Labs/dizzy/internal/glance/executor"
	glanceplan "github.com/B42Labs/dizzy/internal/glance/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Glance is the create-drive-delete-and-wait surface the chaos engine drives
// through the nodes this package builds. It is the consumer-defined
// ports-and-adapters seam to the cloud — *glance.Client satisfies it in
// production and a fake satisfies it in tests. It mirrors the apply executor's
// Glance seam.
type Glance interface {
	CreateImage(ctx context.Context, img glanceplan.Image) (resource.Resource, error)
	UploadImageData(ctx context.Context, r resource.Resource, sizeMiB int, seed int64) error
	ImageOwner(ctx context.Context, r resource.Resource) (string, error)
	AddImageProperties(ctx context.Context, r resource.Resource) error
	ChurnImageProperties(ctx context.Context, r resource.Resource) error
	SetImageVisibility(ctx context.Context, r resource.Resource, v images.ImageVisibility) error
	AddImageMember(ctx context.Context, r resource.Resource, member string) error
	AcceptImageMember(ctx context.Context, r resource.Resource, member string) error
	RemoveImageMember(ctx context.Context, r resource.Resource, member string) error
	DeactivateImage(ctx context.Context, r resource.Resource) error
	ReactivateImage(ctx context.Context, r resource.Resource) error
	Delete(ctx context.Context, r resource.Resource) error
	WaitForReady(ctx context.Context, r resource.Resource) error
	WaitForGone(ctx context.Context, r resource.Resource) error
}

// The production *glance.Client must satisfy the seam.
var _ Glance = (*glance.Client)(nil)

// Build turns a Glance plan into the churn graph: one parentless, gateless image
// node per planned image, mutable when the image has a planned lifecycle
// operation. Every node's closures capture c and run through the glance
// executor's retry policy, bounded by opTimeout; a create uploads the same
// deterministic payload apply does (derived from seed and the image's logical
// name). The plan is validated first so a malformed image fails loudly instead of
// yielding a node that can never be created.
func Build(p *glanceplan.Plan, c Glance, seed int64, opTimeout time.Duration) ([]chaos.Node, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid plan: %w", err)
	}

	var nodes []chaos.Node
	for _, img := range p.Images {
		img := img
		node := chaos.Node{
			Key: img.Name, Kind: glance.KindImage,
			Create: func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
				return createImage(ctx, opTimeout, c, img, seed)
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteGone(ctx, opTimeout, c, res)
			},
		}
		if hasLifecycle(img) {
			node.Mutate = func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return mutateImage(ctx, opTimeout, c, img, res)
			}
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// hasLifecycle reports whether an image carries any lifecycle mutation the churn
// engine can apply: metadata churn, a visibility transition (shared, community,
// or public), or a deactivate/reactivate cycle.
func hasLifecycle(img glanceplan.Image) bool {
	return img.MetadataUpdate || img.Shared || img.Community || img.Public || img.Deactivate
}

// createImage creates an image, uploads its synthetic payload, and waits for it
// to become active, returning the image resource.
func createImage(ctx context.Context, opTimeout time.Duration, c Glance, img glanceplan.Image, seed int64) (resource.Resource, error) {
	var res resource.Resource
	if err := glanceexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		r, err := c.CreateImage(ctx, img)
		if err != nil {
			return err
		}
		res = r
		return nil
	}); err != nil {
		return resource.Resource{}, err
	}
	if err := glanceexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		return c.UploadImageData(ctx, res, img.SizeMiB, glance.PayloadSeed(seed, img.Name))
	}); err != nil {
		return res, err
	}
	if err := waitReady(ctx, opTimeout, c, res); err != nil {
		return res, err
	}
	return res, nil
}

// mutateImage applies an image's planned lifecycle operations in the same fixed
// precedence apply's driveImage uses — metadata churn, the shared flip with a
// self-share member add/accept/remove (kept together while shared), the
// deactivate/reactivate cycle, then the community and public flips — at most once
// per instance lifetime (the engine bounds the call). It is exactly the issue's
// "deactivate/reactivate cycles, visibility flips, member add/remove, and
// metadata churn".
func mutateImage(ctx context.Context, opTimeout time.Duration, c Glance, img glanceplan.Image, res resource.Resource) error {
	if img.MetadataUpdate {
		if err := glanceexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.AddImageProperties(ctx, res) }); err != nil {
			return err
		}
		if err := glanceexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.ChurnImageProperties(ctx, res) }); err != nil {
			return err
		}
	}

	if img.Shared {
		if err := glanceexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
			return c.SetImageVisibility(ctx, res, images.ImageVisibilityShared)
		}); err != nil {
			return err
		}
		owner, err := imageOwner(ctx, opTimeout, c, res)
		if err != nil {
			return err
		}
		if err := glanceexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.AddImageMember(ctx, res, owner) }); err != nil {
			return err
		}
		if img.MemberAccept {
			if err := glanceexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.AcceptImageMember(ctx, res, owner) }); err != nil {
				return err
			}
		}
		if img.MemberRemove {
			if err := glanceexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.RemoveImageMember(ctx, res, owner) }); err != nil {
				return err
			}
		}
	}

	if img.Deactivate {
		if err := glanceexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.DeactivateImage(ctx, res) }); err != nil {
			return err
		}
		if err := glanceexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.ReactivateImage(ctx, res) }); err != nil {
			return err
		}
	}

	if img.Community {
		if err := glanceexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
			return c.SetImageVisibility(ctx, res, images.ImageVisibilityCommunity)
		}); err != nil {
			return err
		}
	}

	if img.Public {
		if err := glanceexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
			return c.SetImageVisibility(ctx, res, images.ImageVisibilityPublic)
		}); err != nil {
			return err
		}
	}

	return nil
}

// imageOwner resolves the image's owning project id (the self-share member),
// bounded by opTimeout.
func imageOwner(ctx context.Context, opTimeout time.Duration, c Glance, res resource.Resource) (string, error) {
	ownerCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return c.ImageOwner(ownerCtx, res)
}

// waitReady polls res to active, bounded by opTimeout.
func waitReady(ctx context.Context, opTimeout time.Duration, c Glance, res resource.Resource) error {
	readyCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return c.WaitForReady(readyCtx, res)
}

// deleteGone deletes res through the retry policy, folds an already-gone (404)
// into success, and waits for the image to be fully gone (bounded by opTimeout)
// so a re-created logical name can never transiently double-count against the
// envelope.
func deleteGone(ctx context.Context, opTimeout time.Duration, c Glance, res resource.Resource) error {
	err := glanceexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		return c.Delete(ctx, res)
	})
	if err != nil {
		if glance.IsNotFound(err) {
			return nil
		}
		return err
	}
	goneCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return c.WaitForGone(goneCtx, res)
}

// Classify labels an operation error for the churn engine's per-bucket error
// breakdown, reusing the glance classification helpers so the labels match the
// kinds operators already see in the metrics report. It is wired into the engine
// via chaos.Config.Classify.
func Classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, glance.ErrQuota):
		return "quota"
	case glance.IsNotFound(err):
		return "not-found"
	case glance.IsRetryable(err):
		return "transient"
	default:
		return "other"
	}
}
