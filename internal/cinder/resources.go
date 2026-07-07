package cinder

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"

	"github.com/B42Labs/dizzy/internal/resource"
)

// status fetches a resource and returns its Cinder status without recording a
// sample. WaitForReady and WaitForGone poll through this so their repeated gets
// do not flood the per-call latency stats; the time-to-ready record stands in
// for them instead.
func (c *Client) status(ctx context.Context, r resource.Resource) (string, error) {
	switch r.Kind {
	case KindVolume:
		v, err := volumes.Get(ctx, c.gc, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return v.Status, nil
	case KindSnapshot:
		s, err := snapshots.Get(ctx, c.gc, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return s.Status, nil
	default:
		return "", fmt.Errorf("status not supported for kind %q", r.Kind)
	}
}

// ListByTypeMetadata returns the resources of kind carrying the
// dizzy:type=<kind> metadata, matching every tester run rather than one run's
// dizzy:run metadata. It is the discovery step for the monitor loop's
// pre-flight orphan sweep, which must reclaim leftovers from a previous crashed
// or interrupted iteration whose run id it no longer holds. The type value is a
// fixed non-empty constant, so the empty-metadata hazard the run-id guard in
// executor.Cleanup protects against cannot arise here. It is the metadata analog
// of Neutron's ListByTypeTag and covers the kinds Cinder creates (volumes,
// snapshots); other kinds return an error.
func (c *Client) ListByTypeMetadata(ctx context.Context, kind resource.Kind) ([]resource.Resource, error) {
	filter := map[string]string{metaType: string(kind)}
	switch kind {
	case KindVolume:
		return c.listVolumesByMetadata(ctx, filter)
	case KindSnapshot:
		return c.listSnapshotsByMetadata(ctx, filter)
	default:
		return nil, fmt.Errorf("list by type metadata not supported for kind %q", kind)
	}
}

// Observe re-queries the live state of a created resource, recording the call.
// It returns the resource's status, whether the resource still exists, and any
// error other than a 404. A 404 is reported as ("", false, nil) so a resource
// deleted out of band reads as gone rather than as a failure. The status
// command drives this over a run's resources.
func (c *Client) Observe(ctx context.Context, r resource.Resource) (status string, exists bool, err error) {
	err = c.timed(ctx, string(r.Kind), "get", func(ctx context.Context) error {
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

// Delete removes a resource, recording the call. Volumes and snapshots are
// deleted by id; a 404 surfaces to the caller, which treats an already-gone
// resource as success to keep cleanup idempotent.
func (c *Client) Delete(ctx context.Context, r resource.Resource) error {
	return c.timed(ctx, string(r.Kind), "delete", func(ctx context.Context) error {
		switch r.Kind {
		case KindVolume:
			return volumes.Delete(ctx, c.gc, r.ID, nil).ExtractErr()
		case KindSnapshot:
			return snapshots.Delete(ctx, c.gc, r.ID).ExtractErr()
		default:
			return fmt.Errorf("delete not supported for kind %q", r.Kind)
		}
	})
}
