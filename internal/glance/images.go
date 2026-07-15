package glance

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/imagedata"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/pagination"

	glanceplan "github.com/B42Labs/dizzy/internal/glance/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Custom property names the metadata churn uses. AddImageProperties adds both;
// ChurnImageProperties replaces the first and removes the second. They are fixed
// per image (an image carries at most one set), so the two-property shape stays
// deterministic across a run.
const (
	propNameA = "dizzy_prop_a"
	propNameB = "dizzy_prop_b"
)

// CreateImage creates an image with the deterministic name and run tags. It sets
// visibility explicitly to private — Glance's default has been shared since
// Ocata, so the later private→shared transition is only real when create pins
// private — and uploads no data (UploadImageData does that next). A quota
// rejection is wrapped with ErrQuota so the executor fails fast.
func (c *Client) CreateImage(ctx context.Context, img glanceplan.Image) (resource.Resource, error) {
	name := resourceName(c.runID, img.Name)
	visibility := images.ImageVisibilityPrivate
	opts := images.CreateOpts{
		Name:            name,
		Visibility:      &visibility,
		Tags:            runTags(c.runID),
		ContainerFormat: "bare",
		DiskFormat:      "raw",
	}

	var id string
	err := c.timed(ctx, "create", func(ctx context.Context) error {
		created, err := images.Create(ctx, c.image, opts).Extract()
		if err != nil {
			return err
		}
		id = created.ID
		return nil
	})
	if err != nil {
		// A create that fails after the request reached Glance (a lost response or a
		// 5xx past commit) can leave an image behind. Log the deterministic name so
		// an operator can locate any such orphan; its tags still make it discoverable
		// at cleanup.
		slog.Warn("create failed; an image with this name may be orphaned in the cloud",
			"name", name, "error", err)
		return resource.Resource{}, wrapCreate(img.Name, err)
	}
	return resource.Resource{Kind: KindImage, Logical: img.Name, Name: name, ID: id}, nil
}

// UploadImageData streams a synthetic payload of sizeMiB mebibytes into the
// image through the direct data-upload path. The payload bytes are drawn
// deterministically from seed, so the same scenario and seed push byte-identical
// data every run.
func (c *Client) UploadImageData(ctx context.Context, r resource.Resource, sizeMiB int, seed int64) error {
	err := c.timed(ctx, "upload", func(ctx context.Context) error {
		return imagedata.Upload(ctx, c.image, r.ID, payloadReader(seed, int64(sizeMiB)<<20)).ExtractErr()
	})
	if err != nil {
		return fmt.Errorf("uploading data to image %q: %w", r.Logical, err)
	}
	return nil
}

// AddImageProperties adds two deterministic custom properties to the image, the
// first pass of the metadata churn.
func (c *Client) AddImageProperties(ctx context.Context, r resource.Resource) error {
	patch := images.UpdateOpts{
		images.UpdateImageProperty{Op: images.AddOp, Name: propNameA, Value: r.Logical + "-a"},
		images.UpdateImageProperty{Op: images.AddOp, Name: propNameB, Value: r.Logical + "-b"},
	}
	return c.updateProperties(ctx, r, patch)
}

// ChurnImageProperties replaces one custom property and removes the other, the
// second pass of the metadata churn. It runs after AddImageProperties.
func (c *Client) ChurnImageProperties(ctx context.Context, r resource.Resource) error {
	patch := images.UpdateOpts{
		images.UpdateImageProperty{Op: images.ReplaceOp, Name: propNameA, Value: r.Logical + "-churned"},
		images.UpdateImageProperty{Op: images.RemoveOp, Name: propNameB},
	}
	return c.updateProperties(ctx, r, patch)
}

// updateProperties applies a property patch through the timed update op. It
// deliberately only ever touches the dizzy_* custom properties and never the
// tags, so an update can never clobber the run-identity tags cleanup relies on.
func (c *Client) updateProperties(ctx context.Context, r resource.Resource, patch images.UpdateOpts) error {
	err := c.timed(ctx, "update", func(ctx context.Context) error {
		_, err := images.Update(ctx, c.image, r.ID, patch).Extract()
		return err
	})
	if err != nil {
		return fmt.Errorf("updating properties of image %q: %w", r.Logical, err)
	}
	return nil
}

// SetImageVisibility transitions the image to v (shared, community, or public).
func (c *Client) SetImageVisibility(ctx context.Context, r resource.Resource, v images.ImageVisibility) error {
	err := c.timed(ctx, "visibility", func(ctx context.Context) error {
		_, err := images.Update(ctx, c.image, r.ID, images.UpdateOpts{images.UpdateVisibility{Visibility: v}}).Extract()
		return err
	})
	if err != nil {
		return fmt.Errorf("setting visibility of image %q to %s: %w", r.Logical, v, err)
	}
	return nil
}

// DeactivateImage deactivates the image via the actions/deactivate endpoint.
// gophercloud has no typed helper for it, so it is a raw POST expecting 204.
func (c *Client) DeactivateImage(ctx context.Context, r resource.Resource) error {
	return c.imageAction(ctx, r, "deactivate")
}

// ReactivateImage reactivates a deactivated image via the actions/reactivate
// endpoint, the raw-POST counterpart of DeactivateImage.
func (c *Client) ReactivateImage(ctx context.Context, r resource.Resource) error {
	return c.imageAction(ctx, r, "reactivate")
}

// imageAction issues the raw POST /v2/images/{id}/actions/{action} request (the
// deactivate/reactivate verbs gophercloud lacks a helper for), timed under the
// action's op label and expecting a 204.
func (c *Client) imageAction(ctx context.Context, r resource.Resource, action string) error {
	err := c.timed(ctx, action, func(ctx context.Context) error {
		url := c.image.ServiceURL("images", r.ID, "actions", action)
		_, err := c.image.Post(ctx, url, nil, nil, &gophercloud.RequestOpts{OkCodes: []int{http.StatusNoContent}})
		return err
	})
	if err != nil {
		return fmt.Errorf("%s image %q: %w", action, r.Logical, err)
	}
	return nil
}

// Delete removes an image by id, recording the call. A 404 surfaces to the
// caller, which treats an already-gone image as success to keep cleanup
// idempotent.
func (c *Client) Delete(ctx context.Context, r resource.Resource) error {
	return c.timed(ctx, "delete", func(ctx context.Context) error {
		return images.Delete(ctx, c.image, r.ID).ExtractErr()
	})
}

// status fetches an image and returns its status without recording a sample. The
// readiness polls go through this so their repeated gets do not flood the
// per-call latency stats; the time-to-ready record stands in for them instead.
func (c *Client) status(ctx context.Context, r resource.Resource) (string, error) {
	img, err := images.Get(ctx, c.image, r.ID).Extract()
	if err != nil {
		return "", err
	}
	return string(img.Status), nil
}

// Observe re-queries the live state of a created image, recording the call. It
// returns the image's status, whether it still exists, and any error other than
// a 404. A 404 is reported as ("", false, nil) so an image deleted out of band
// reads as gone rather than as a failure. The status command drives this over a
// run's images.
func (c *Client) Observe(ctx context.Context, r resource.Resource) (status string, exists bool, err error) {
	err = c.timed(ctx, "get", func(ctx context.Context) error {
		s, getErr := c.status(ctx, r)
		if getErr != nil {
			return getErr
		}
		status = s
		return nil
	})
	switch {
	case IsNotFound(err):
		return "", false, nil
	case err != nil:
		return "", false, err
	default:
		return status, true, nil
	}
}

// WaitForReady polls an image until it becomes active, recording one Readiness
// sample. A killed status (an upload that failed) is a terminal failure that
// aborts the poll instead of spinning to the deadline. It returns ctx.Err() if
// ctx is cancelled or its deadline elapses first; the caller decides whether a
// readiness deadline is fatal.
func (c *Client) WaitForReady(ctx context.Context, r resource.Resource) error {
	start := time.Now()
	backoff := 200 * time.Millisecond
	for {
		status, err := c.status(ctx, r)
		switch {
		case err == nil && status == statusImageActive:
			c.recordReady(ctx, r, time.Since(start), true)
			return nil
		case err == nil && status == statusImageKilled:
			c.recordReady(ctx, r, time.Since(start), false)
			return fmt.Errorf("image %s reached terminal status %q", r.ID, status)
		case IsNotFound(err):
			c.recordReady(ctx, r, time.Since(start), false)
			return fmt.Errorf("image %s is gone while waiting for readiness: %w", r.ID, err)
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			c.recordReady(ctx, r, time.Since(start), false)
			return ctx.Err()
		}

		if backoff = time.Duration(float64(backoff) * 1.5); backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
	}
}

// WaitForGone polls an image until it no longer exists. A 404, or a status of
// deleted or pending_delete (the state a delayed_delete deployment parks a
// deleted image in), counts as gone, so cleanup does not spin to the deadline on
// a deployment that defers the actual removal. It returns ctx.Err() if the
// deadline elapses first.
func (c *Client) WaitForGone(ctx context.Context, r resource.Resource) error {
	backoff := 200 * time.Millisecond
	for {
		status, err := c.status(ctx, r)
		switch {
		case IsNotFound(err):
			return nil
		case err == nil && (status == statusImageDeleted || status == statusImagePendingDelete):
			return nil
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}

		if backoff = time.Duration(float64(backoff) * 1.5); backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
	}
}

// recordReady records one time-to-ready sample into both the collector and
// telemetry.
func (c *Client) recordReady(ctx context.Context, r resource.Resource, d time.Duration, ok bool) {
	c.metrics.RecordReadiness(metrics.Readiness{Type: string(r.Kind), Duration: d, OK: ok})
	c.tel.RecordTimeToReady(ctx, string(r.Kind), d, ok)
}

// ListImagesByTag returns the images carrying this run's dizzy:run=<runID> tag,
// the discovery step tag-based cleanup deletes from. Glance filters by tag
// server-side, so only this run's images are returned.
func (c *Client) ListImagesByTag(ctx context.Context, runID string) ([]resource.Resource, error) {
	return c.listByTag(ctx, metaRun+"="+runID)
}

// ListImagesByTypeTag returns the images carrying the dizzy:type=image tag,
// matching every tester run rather than one run's dizzy:run tag. It is the
// discovery step for the monitor loop's pre-flight orphan sweep, which must
// reclaim leftovers from a previous crashed or interrupted iteration whose run
// id it no longer holds.
func (c *Client) ListImagesByTypeTag(ctx context.Context) ([]resource.Resource, error) {
	return c.listByTag(ctx, metaType+"="+string(KindImage))
}

// listByTag lists images server-side filtered to the exact tag string, recording
// the call under the list op.
func (c *Client) listByTag(ctx context.Context, tag string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, "list", func(ctx context.Context) error {
		found = nil
		return images.List(c.image, images.ListOpts{Tags: []string{tag}}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
			items, err := images.ExtractImages(page)
			if err != nil {
				return false, err
			}
			for _, img := range items {
				found = append(found, resource.Resource{Kind: KindImage, Name: img.Name, ID: img.ID})
			}
			return true, nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing images by tag: %w", err)
	}
	return found, nil
}
