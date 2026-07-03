package cinder

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/pagination"

	cinderplan "github.com/B42Labs/openstack-tester/internal/cinder/plan"
	"github.com/B42Labs/openstack-tester/internal/resource"
)

// CreateVolume creates a blank volume with the deterministic name and run
// metadata, in the given volume type (empty means the cloud's default type).
// The metadata rides in the create request itself, so unlike Neutron there is
// no separate tag step that could orphan an untagged resource. A quota
// rejection is wrapped with ErrQuota so the executor fails fast.
func (c *Client) CreateVolume(ctx context.Context, v cinderplan.Volume, volumeType string) (resource.Resource, error) {
	name := resourceName(c.runID, v.Name)
	var id string
	err := c.timed(ctx, string(KindVolume), "create", func(ctx context.Context) error {
		created, err := volumes.Create(ctx, c.gc, volumes.CreateOpts{
			Name:       name,
			Size:       v.SizeGiB,
			VolumeType: volumeType,
			Metadata:   runMetadata(c.runID, KindVolume),
		}, nil).Extract()
		if err != nil {
			return err
		}
		id = created.ID
		return nil
	})
	if err != nil {
		// A create that fails after the request reached Cinder (a lost response or
		// a 5xx past commit) can leave a volume behind. Log the deterministic name
		// so an operator can locate any such orphan; its metadata still makes it
		// discoverable at cleanup.
		slog.Warn("create failed; a volume with this name may be orphaned in the cloud",
			"name", name, "error", err)
		return resource.Resource{}, wrapCreate(KindVolume, v.Name, err)
	}
	return resource.Resource{Kind: KindVolume, Logical: v.Name, Name: name, ID: id}, nil
}

// ExtendVolume grows an already-available volume to newSizeGiB. The extend is an
// offline resize on an unattached volume, so it needs no microversion. It
// records under the new "extend" operation label and wraps a quota rejection
// (the extra gigabytes count against the shared quota) with ErrQuota.
func (c *Client) ExtendVolume(ctx context.Context, r resource.Resource, newSizeGiB int) error {
	err := c.timed(ctx, string(KindVolume), "extend", func(ctx context.Context) error {
		return volumes.ExtendSize(ctx, c.gc, r.ID, volumes.ExtendSizeOpts{NewSize: newSizeGiB}).ExtractErr()
	})
	if err != nil {
		// Extend is not idempotent: if the first request commits on the backend but
		// its response is lost (or returns a 5xx), the executor retries and Cinder
		// rejects the re-issue with a 400 because the volume is already at the
		// target. That 400 is itself confirmation the volume reached newSizeGiB, so
		// treat it as success rather than failing an otherwise-correct run.
		if isExtendAlreadyApplied(err) {
			slog.Info("extend already applied; treating a retried extend as success",
				"logical", r.Logical, "id", r.ID, "newSizeGiB", newSizeGiB)
			return nil
		}
		return wrapCreate(KindVolume, r.Logical, err)
	}
	return nil
}

// ListVolumesByMetadata returns the volumes carrying this run's
// ostester:run=<runID> metadata, the discovery step metadata-based cleanup
// deletes from.
func (c *Client) ListVolumesByMetadata(ctx context.Context, runID string) ([]resource.Resource, error) {
	return c.listVolumesByMetadata(ctx, map[string]string{metaRun: runID})
}

// listVolumesByMetadata is the shared streamed, client-side-rechecked volume
// listing behind ListVolumesByMetadata (one run's metadata) and
// ListByTypeMetadata (any run of one kind). The metadata filter is requested
// server-side, but that filter is not universally honored (older releases,
// backends that ignore unknown query params, a proxy that strips it, a
// microversion mismatch can all return the full volume list), so
// /volumes/detail's metadata is re-checked client-side against every filter
// entry — mirroring the snapshot path — so the result never includes a volume
// the tool did not tag with all of filter, whatever the server returns.
func (c *Client) listVolumesByMetadata(ctx context.Context, filter map[string]string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, string(KindVolume), "list", func(ctx context.Context) error {
		found = nil
		// The server-side metadata filter is requested but not universally honored, so
		// stream a page at a time and keep only the matching volumes rather than letting
		// AllPages accumulate every volume in the project — at cleanup a project created
		// by this tool is at peak resource count, and one allocation of the whole list is
		// a memory spike that could OOM the very step that frees the billable resources.
		return volumes.List(c.gc, volumes.ListOpts{
			Metadata: filter,
		}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
			items, err := volumes.ExtractVolumes(page)
			if err != nil {
				return false, err
			}
			for _, v := range items {
				if !metadataMatches(v.Metadata, filter) {
					continue // server-side filter not honored; never delete a volume this run did not tag
				}
				found = append(found, resource.Resource{Kind: KindVolume, Name: v.Name, ID: v.ID})
			}
			return true, nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing volumes by metadata: %w", err)
	}
	return found, nil
}
