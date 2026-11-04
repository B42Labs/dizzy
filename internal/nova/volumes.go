package nova

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/volumeattach"
	"github.com/gophercloud/gophercloud/v2/pagination"

	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// CreateVolume creates a blank data volume with the deterministic name and run
// metadata. The metadata rides in the create request itself, the Cinder pattern.
// A quota rejection is wrapped with ErrQuota so the executor fails fast.
//
// Each request carries a fresh create token under dizzy:create, and a lost
// answer is handled as CreateServer handles it: the volume is looked up by name,
// run and token right after a failure that may have landed and again before the
// next create of the name, and a match is adopted instead of creating a second
// volume. A pending lookup that fails returns its error without posting.
func (c *Client) CreateVolume(ctx context.Context, v novaplan.Volume) (resource.Resource, error) {
	name := resourceName(c.runID, v.Name)

	// adopt returns the newest of the volumes a lookup found, which are this
	// name's creates whose answer was lost. Any others stay in the cloud, where
	// their metadata still makes them discoverable.
	adopt := func(found []volumes.Volume) resource.Resource {
		id := found[0].ID
		slog.Info("adopted a volume whose create answer was lost", "name", name, "id", id)
		if len(found) > 1 {
			rest := make([]string, 0, len(found)-1)
			for _, vol := range found[1:] {
				rest = append(rest, vol.ID)
			}
			slog.Warn("more volumes of an earlier create whose answer was lost; the teardown deletes them",
				"name", name, "ids", rest)
		}
		return resource.Resource{Kind: KindVolume, Logical: v.Name, Name: name, ID: id}
	}

	if tokens := c.takePending(KindVolume, name); len(tokens) > 0 {
		found, err := c.lostVolumes(ctx, name, tokens)
		if err != nil {
			// Posting now could create a second volume beside one Cinder committed,
			// so keep the tokens for the next attempt and fail instead; a 5xx from
			// the list stays retryable.
			for _, token := range tokens {
				c.addPending(KindVolume, name, token)
			}
			return resource.Resource{}, wrapCreate(KindVolume, v.Name,
				fmt.Errorf("looking up an earlier create whose answer was lost: %w", err))
		}
		if len(found) > 0 {
			return adopt(found), nil
		}
	}

	// The token rides on a copy of the run metadata, so runMetadata keeps the
	// two keys discovery and the Neutron tags share.
	token := c.createToken()
	metadata := maps.Clone(runMetadata(c.runID, KindVolume))
	metadata[metaCreate] = token

	var id string
	err := c.timed(ctx, string(KindVolume), "create", func(ctx context.Context) error {
		created, err := volumes.Create(ctx, c.blockstorage, volumes.CreateOpts{
			Name:     name,
			Size:     v.SizeGiB,
			Metadata: metadata,
		}, nil).Extract()
		if err != nil {
			return err
		}
		id = created.ID
		return nil
	})
	if err != nil {
		// A create that may have reached Cinder is looked up by this request's
		// token while the context allows and adopted when Cinder has it; otherwise
		// the token stays pending for the next create of the name.
		var lookupErr error
		if mayHaveLanded(err) {
			if ctx.Err() == nil {
				var found []volumes.Volume
				found, lookupErr = c.lostVolumes(ctx, name, []string{token})
				if lookupErr == nil && len(found) > 0 {
					return adopt(found), nil
				}
			}
			c.addPending(KindVolume, name, token)
		}
		attrs := []any{"name", name, "error", err}
		if lookupErr != nil {
			attrs = append(attrs, "lookup", lookupErr)
		}
		slog.Warn("create failed; a volume with this name may be orphaned in the cloud", attrs...)
		// The POST's error, not the lookup's, so errKind, IsRetryable and the
		// executor see the failure they always saw.
		return resource.Resource{}, wrapCreate(KindVolume, v.Name, err)
	}
	return resource.Resource{Kind: KindVolume, Logical: v.Name, Name: name, ID: id}, nil
}

// lostVolumes returns this run's data volumes named name whose dizzy:create
// value is one of tokens, newest first: the volumes creates whose answer was lost
// made. The name and run filters are requested server-side, but the metadata
// filter is not universally honored, so the name, run and token are checked
// again client-side; a volume without metadata matches nothing.
func (c *Client) lostVolumes(ctx context.Context, name string, tokens []string) ([]volumes.Volume, error) {
	var found []volumes.Volume
	err := c.timed(ctx, string(KindVolume), "list", func(ctx context.Context) error {
		found = nil
		opts := volumes.ListOpts{Name: name, Metadata: map[string]string{metaRun: c.runID}}
		return volumes.List(c.blockstorage, opts).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
			items, err := volumes.ExtractVolumes(page)
			if err != nil {
				return false, err
			}
			for _, vol := range items {
				if vol.Name == name && vol.Metadata[metaRun] == c.runID && slices.Contains(tokens, vol.Metadata[metaCreate]) {
					found = append(found, vol)
				}
			}
			return true, nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing volumes named %s: %w", name, err)
	}
	slices.SortStableFunc(found, func(a, b volumes.Volume) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return found, nil
}

// AttachVolume attaches a data volume to a server through the compute
// volume-attach API. It records under the volume type and the attach operation.
// A repeated attach can hit a 400 because the volume is already attached from
// an earlier request whose answer or in-use wait was lost. Nova answers the
// same 400 when the volume is attached to another server, so only when this
// server has the volume's attachment does that 400 confirm the earlier attach
// committed and is treated as success; a 404 surfaces the 400. When the lookup
// itself fails, its error surfaces instead, so a transient one stays retryable.
// The waitVolumeStatus(in-use) that follows stays the source of truth.
func (c *Client) AttachVolume(ctx context.Context, server, volume resource.Resource) error {
	err := c.timed(ctx, string(KindVolume), "attach", func(ctx context.Context) error {
		_, err := volumeattach.Create(ctx, c.compute, server.ID, volumeattach.CreateOpts{VolumeID: volume.ID}).Extract()
		return err
	})
	if err != nil && badRequestMentions(err, "already attached") {
		_, gerr := volumeattach.Get(ctx, c.compute, server.ID, volume.ID).Extract()
		switch {
		case gerr == nil:
			slog.Info("volume already attached; treating a repeated attach as success",
				"volume", volume.Logical, "server", server.Logical, "id", volume.ID)
			return nil
		case !IsNotFound(gerr):
			return fmt.Errorf("attaching volume %q to server %q: checking the attachment after %v: %w",
				volume.Logical, server.Logical, err, gerr)
		}
	}
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

// WaitForVolumeDetached waits until the server no longer has the volume's
// attachment. DetachVolume returns once Nova accepted the detach, not once it is
// done, and Cinder reports the volume available before Nova drops its block
// device mapping. While that mapping exists, Nova rejects an attach with a 400,
// so a re-attach must wait here first. It polls the attachment until Nova
// answers 404, which it also answers for a deleted server. Every other answer,
// an error other than a 404 included, is polled again with the backoff
// WaitForGone uses: the caller's context is the only bound, and it returns
// ctx.Err() when that ends first. It records no metrics sample.
func (c *Client) WaitForVolumeDetached(ctx context.Context, server, volume resource.Resource) error {
	backoff := 200 * time.Millisecond
	for {
		if _, err := volumeattach.Get(ctx, c.compute, server.ID, volume.ID).Extract(); IsNotFound(err) {
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
