package glance

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/members"

	"github.com/B42Labs/dizzy/internal/resource"
)

// Member status the accept step sets. Glance authorizes a member-status update
// for the member project itself, so a run that shares an image with its own
// project can accept the membership in-project without admin rights.
const memberStatusAccepted = "accepted"

// ImageOwner resolves the owning project id of an image, the value the run
// shares the image with (a self-share, so the accept step stays authorized). It
// is discovery, not workload — like FindImage in the Nova client — so it is kept
// out of the run metrics.
func (c *Client) ImageOwner(ctx context.Context, r resource.Resource) (string, error) {
	img, err := images.Get(ctx, c.image, r.ID).Extract()
	if err != nil {
		return "", fmt.Errorf("resolving owner of image %q: %w", r.Logical, err)
	}
	return img.Owner, nil
}

// AddImageMember adds member (a project id) to the image. The image must already
// be shared; the caller transitions its visibility first.
func (c *Client) AddImageMember(ctx context.Context, r resource.Resource, member string) error {
	err := c.timed(ctx, "member-add", func(ctx context.Context) error {
		_, err := members.Create(ctx, c.image, r.ID, member).Extract()
		return err
	})
	if err != nil {
		return fmt.Errorf("adding member to image %q: %w", r.Logical, err)
	}
	return nil
}

// AcceptImageMember accepts member's shared membership of the image. It is
// authorized because member is the run's own project (a self-share).
func (c *Client) AcceptImageMember(ctx context.Context, r resource.Resource, member string) error {
	err := c.timed(ctx, "member-accept", func(ctx context.Context) error {
		_, err := members.Update(ctx, c.image, r.ID, member, members.UpdateOpts{Status: memberStatusAccepted}).Extract()
		return err
	})
	if err != nil {
		return fmt.Errorf("accepting membership of image %q: %w", r.Logical, err)
	}
	return nil
}

// RemoveImageMember removes member from the image's shared members.
func (c *Client) RemoveImageMember(ctx context.Context, r resource.Resource, member string) error {
	err := c.timed(ctx, "member-remove", func(ctx context.Context) error {
		return members.Delete(ctx, c.image, r.ID, member).ExtractErr()
	})
	if err != nil {
		return fmt.Errorf("removing member from image %q: %w", r.Logical, err)
	}
	return nil
}
