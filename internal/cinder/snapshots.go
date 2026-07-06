package cinder

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/v2/pagination"

	cinderplan "github.com/B42Labs/dizzy/internal/cinder/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// CreateSnapshot creates a snapshot of volumeID with the deterministic name and
// run metadata. A quota rejection is wrapped with ErrQuota so the executor fails
// fast.
func (c *Client) CreateSnapshot(ctx context.Context, s cinderplan.Snapshot, volumeID string) (resource.Resource, error) {
	name := resourceName(c.runID, s.Name)
	var id string
	err := c.timed(ctx, string(KindSnapshot), "create", func(ctx context.Context) error {
		created, err := snapshots.Create(ctx, c.gc, snapshots.CreateOpts{
			VolumeID: volumeID,
			Name:     name,
			Metadata: runMetadata(c.runID, KindSnapshot),
		}).Extract()
		if err != nil {
			return err
		}
		id = created.ID
		return nil
	})
	if err != nil {
		slog.Warn("create failed; a snapshot with this name may be orphaned in the cloud",
			"name", name, "error", err)
		return resource.Resource{}, wrapCreate(KindSnapshot, s.Name, err)
	}
	return resource.Resource{Kind: KindSnapshot, Logical: s.Name, Name: name, ID: id}, nil
}

// ListSnapshotsByMetadata returns the snapshots carrying this run's
// ostester:run=<runID> metadata. Selection is keyed exclusively on the run
// metadata, so it never includes snapshots the tool did not create.
func (c *Client) ListSnapshotsByMetadata(ctx context.Context, runID string) ([]resource.Resource, error) {
	return c.listSnapshotsByMetadata(ctx, map[string]string{metaRun: runID})
}

// listSnapshotsByMetadata is the shared streamed, client-side-filtered snapshot
// listing behind ListSnapshotsByMetadata (one run's metadata) and
// ListByTypeMetadata (any run of one kind). gophercloud v2.12.0's
// snapshots.ListOpts has no metadata field (unlike volumes), so the project's
// snapshots are listed and filtered client-side against every filter entry on
// the metadata the tool itself wrote.
func (c *Client) listSnapshotsByMetadata(ctx context.Context, filter map[string]string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, string(KindSnapshot), "list", func(ctx context.Context) error {
		found = nil
		// The listing is unfiltered (ListOpts has no metadata field), so stream a
		// page at a time and keep only the matching snapshots rather than letting
		// AllPages accumulate every snapshot in the project — at cleanup a project
		// created by this tool is at peak resource count, and one allocation of the
		// whole list is a memory spike that could OOM the very step that frees the
		// billable resources.
		return snapshots.ListDetail(c.gc, snapshots.ListOpts{}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
			items, err := snapshots.ExtractSnapshots(page)
			if err != nil {
				return false, err
			}
			for _, s := range items {
				if metadataMatches(s.Metadata, filter) {
					found = append(found, resource.Resource{Kind: KindSnapshot, Name: s.Name, ID: s.ID})
				}
			}
			return true, nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing snapshots by metadata: %w", err)
	}
	return found, nil
}
