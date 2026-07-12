package nova

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/volumeattach"
	"github.com/gophercloud/gophercloud/v2/pagination"

	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// CreateVolume creates a blank data volume with the deterministic name and run
// metadata. The metadata rides in the create request itself, the Cinder pattern.
// A quota rejection is wrapped with ErrQuota so the executor fails fast.
func (c *Client) CreateVolume(ctx context.Context, v novaplan.Volume) (resource.Resource, error) {
	name := resourceName(c.runID, v.Name)
	var id string
	err := c.timed(ctx, string(KindVolume), "create", func(ctx context.Context) error {
		created, err := volumes.Create(ctx, c.blockstorage, volumes.CreateOpts{
			Name:     name,
			Size:     v.SizeGiB,
			Metadata: runMetadata(c.runID, KindVolume),
		}, nil).Extract()
		if err != nil {
			return err
		}
		id = created.ID
		return nil
	})
	if err != nil {
		slog.Warn("create failed; a volume with this name may be orphaned in the cloud",
			"name", name, "error", err)
		return resource.Resource{}, wrapCreate(KindVolume, v.Name, err)
	}
	return resource.Resource{Kind: KindVolume, Logical: v.Name, Name: name, ID: id}, nil
}

// AttachVolume attaches a data volume to a server through the compute
// volume-attach API. It records under the volume type and the attach operation.
func (c *Client) AttachVolume(ctx context.Context, server, volume resource.Resource) error {
	err := c.timed(ctx, string(KindVolume), "attach", func(ctx context.Context) error {
		_, err := volumeattach.Create(ctx, c.compute, server.ID, volumeattach.CreateOpts{VolumeID: volume.ID}).Extract()
		return err
	})
	if err != nil {
		return fmt.Errorf("attaching volume %q to server %q: %w", volume.Logical, server.Logical, err)
	}
	return nil
}

// DetachVolume detaches a data volume from a server. A 404 (already detached)
// surfaces to the caller, which treats it as success to keep the operation
// idempotent.
func (c *Client) DetachVolume(ctx context.Context, server, volume resource.Resource) error {
	err := c.timed(ctx, string(KindVolume), "detach", func(ctx context.Context) error {
		return volumeattach.Delete(ctx, c.compute, server.ID, volume.ID).ExtractErr()
	})
	if err != nil {
		return fmt.Errorf("detaching volume %q from server %q: %w", volume.Logical, server.Logical, err)
	}
	return nil
}

// ListVolumesByMetadata returns the data volumes carrying this run's
// dizzy:run=<runID> metadata, the discovery step metadata-based cleanup deletes
// from.
func (c *Client) ListVolumesByMetadata(ctx context.Context, runID string) ([]resource.Resource, error) {
	return c.listVolumesByMetadata(ctx, map[string]string{metaRun: runID})
}

// listVolumesByMetadata is the shared streamed, client-side-rechecked volume
// listing behind ListVolumesByMetadata (one run's metadata) and
// ListByTypeMetadata (any run of one kind). The metadata filter is requested
// server-side, but that filter is not universally honored, so /volumes/detail's
// metadata is re-checked client-side against every filter entry — streaming a
// page at a time rather than accumulating the whole list, since at cleanup a
// project created by this tool is at peak resource count.
func (c *Client) listVolumesByMetadata(ctx context.Context, filter map[string]string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, string(KindVolume), "list", func(ctx context.Context) error {
		found = nil
		return volumes.List(c.blockstorage, volumes.ListOpts{Metadata: filter}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
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
