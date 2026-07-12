package nova

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/attributestags"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	"github.com/gophercloud/gophercloud/v2/pagination"

	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// tagAttempts and tagRetryDelay bound how often a transient tag failure is
// retried in place. Tag retries live here, not in the executor's create retry,
// so a retryable tag error never re-enters create and duplicates the resource.
const (
	tagAttempts   = 3
	tagRetryDelay = 250 * time.Millisecond
)

// CreateNetwork creates a tagged tenant network.
func (c *Client) CreateNetwork(ctx context.Context, n novaplan.Network) (resource.Resource, error) {
	return c.createTagged(ctx, KindNetwork, n.Name, func(ctx context.Context, name string) (string, error) {
		created, err := networks.Create(ctx, c.network, networks.CreateOpts{Name: name}).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}

// CreateSubnet creates a tagged /24 subnet on networkID with the plan's CIDR.
func (c *Client) CreateSubnet(ctx context.Context, n novaplan.Network, networkID string) (resource.Resource, error) {
	return c.createTagged(ctx, KindSubnet, n.Subnet, func(ctx context.Context, name string) (string, error) {
		created, err := subnets.Create(ctx, c.network, subnets.CreateOpts{
			NetworkID: networkID,
			Name:      name,
			IPVersion: 4,
			CIDR:      n.CIDR,
		}).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}

// tagCollection maps a taggable kind to its Neutron tag-extension collection
// path.
func tagCollection(kind resource.Kind) string {
	switch kind {
	case KindNetwork:
		return "networks"
	case KindSubnet:
		return "subnets"
	case KindPort:
		return "ports"
	default:
		return ""
	}
}

// createTagged centralizes the create-then-tag flow shared by every taggable
// Neutron kind (network, subnet, port): it applies the deterministic name via
// create, records the create and tag timings, wraps quota errors with ErrQuota,
// and returns the resulting Resource. A tag failure (after a bounded in-place
// retry) rolls the created resource back and fails the create, so a resource is
// never left created-but-untagged where tag-based cleanup cannot reclaim it. It
// mirrors the neutron client's createTagged, minus the best-effort address-scope
// path (none of the nova companion kinds is best-effort).
func (c *Client) createTagged(ctx context.Context, kind resource.Kind, logical string, create func(ctx context.Context, name string) (id string, err error)) (resource.Resource, error) {
	name := resourceName(c.runID, logical)
	var id string
	err := c.timed(ctx, string(kind), "create", func(ctx context.Context) error {
		var createErr error
		id, createErr = create(ctx, name)
		return createErr
	})
	if err != nil {
		slog.Warn("create failed; a resource with this name may be orphaned in the cloud",
			"kind", kind, "name", name, "error", err)
		return resource.Resource{}, wrapCreate(kind, logical, err)
	}

	r := resource.Resource{Kind: kind, Logical: logical, Name: name, ID: id}
	if err := c.tagWithRetry(ctx, kind, id); err != nil {
		// The resource exists but is untagged, so tag-based cleanup can never
		// reclaim it. Roll it back so the resource is either fully tagged or gone;
		// if the rollback also fails, log the name as a last resort.
		if delErr := c.Delete(ctx, r); delErr != nil {
			slog.Warn("rolling back untagged resource failed; it may be orphaned in the cloud",
				"kind", kind, "name", name, "id", id, "error", delErr)
		}
		return resource.Resource{}, fmt.Errorf("tagging %s %q: %w", kind, logical, err)
	}
	return r, nil
}

// tagWithRetry replaces the tags on a created resource with the run tags for its
// kind, retrying a transient failure a bounded number of times. The resource id
// is fixed and ReplaceAll is idempotent, so unlike the executor's create retry
// this never re-creates the resource.
func (c *Client) tagWithRetry(ctx context.Context, kind resource.Kind, id string) error {
	do := func(ctx context.Context) error {
		opts := attributestags.ReplaceAllOpts{Tags: runTags(c.runID, kind)}
		_, err := attributestags.ReplaceAll(ctx, c.network, tagCollection(kind), id, opts).Extract()
		return err
	}
	var err error
	for attempt := 1; attempt <= tagAttempts; attempt++ {
		if err = c.timed(ctx, string(kind), "tag", do); err == nil || !IsRetryable(err) {
			return err
		}
		if attempt == tagAttempts {
			break
		}
		select {
		case <-time.After(tagRetryDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// ListByTag returns the taggable resources of kind carrying this run's
// dizzy:run=<runID> tag, the discovery step tag-based cleanup deletes from. It
// covers networks, subnets, and ports; other kinds return an error.
func (c *Client) ListByTag(ctx context.Context, kind resource.Kind, runID string) ([]resource.Resource, error) {
	return c.listByTagValue(ctx, kind, metaRun+"="+runID)
}

// ListByTypeTag returns the taggable resources of kind carrying the type tag
// dizzy:type=<kind>, matching every tester run rather than one run's dizzy:run
// tag. It is the discovery step for the monitor loop's pre-flight orphan sweep,
// which must reclaim leftovers from a previous crashed or interrupted iteration
// whose run id it no longer holds.
func (c *Client) ListByTypeTag(ctx context.Context, kind resource.Kind) ([]resource.Resource, error) {
	return c.listByTagValue(ctx, kind, metaType+"="+string(kind))
}

// listByTagValue is the shared timed body behind ListByTag and ListByTypeTag: it
// lists kind server-side filtered to the exact tag string and records the call
// under the list operation.
func (c *Client) listByTagValue(ctx context.Context, kind resource.Kind, tag string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, string(kind), "list", func(ctx context.Context) error {
		var listErr error
		found, listErr = c.listByTag(ctx, kind, tag)
		return listErr
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s by tag: %w", kind, err)
	}
	return found, nil
}

// listByTag performs the per-kind tagged list without recording a sample;
// listByTagValue wraps it through c.timed. Each kind uses its own typed ListOpts
// and extractor.
func (c *Client) listByTag(ctx context.Context, kind resource.Kind, tag string) ([]resource.Resource, error) {
	switch kind {
	case KindNetwork:
		return listTagged(ctx, kind, networks.List(c.network, networks.ListOpts{Tags: tag}),
			networks.ExtractNetworks, func(it networks.Network) (string, string) { return it.Name, it.ID })
	case KindSubnet:
		return listTagged(ctx, kind, subnets.List(c.network, subnets.ListOpts{Tags: tag}),
			subnets.ExtractSubnets, func(it subnets.Subnet) (string, string) { return it.Name, it.ID })
	case KindPort:
		return listTagged(ctx, kind, ports.List(c.network, ports.ListOpts{Tags: tag}),
			ports.ExtractPorts, func(it ports.Port) (string, string) { return it.Name, it.ID })
	default:
		return nil, fmt.Errorf("list by tag not supported for kind %q", kind)
	}
}

// listTagged runs a tagged list pager to completion and collects the results
// into Resources of kind. nameID pulls the cloud name and id from each item.
func listTagged[T any](
	ctx context.Context,
	kind resource.Kind,
	pager pagination.Pager,
	extract func(pagination.Page) ([]T, error),
	nameID func(T) (string, string),
) ([]resource.Resource, error) {
	pages, err := pager.AllPages(ctx)
	if err != nil {
		return nil, err
	}
	items, err := extract(pages)
	if err != nil {
		return nil, err
	}
	out := make([]resource.Resource, 0, len(items))
	for _, it := range items {
		name, id := nameID(it)
		out = append(out, resource.Resource{Kind: kind, Name: name, ID: id})
	}
	return out, nil
}

// DeleteNetworkPorts deletes the plain ports left on networkID — those with an
// empty device_owner — and returns how many it removed. These are the ports the
// run created on its own network that tag-based discovery can miss: a cancelled
// run can create a port and then lose the context before tagging (and before the
// rollback), leaving an untagged orphan that would otherwise block the network
// delete with NetworkInUse. Ports with a device owner are left alone: interface
// ports are detached separately, and Neutron's own service ports (DHCP/metadata)
// are removed by the network delete that follows. A port already gone (404) is
// skipped so repeated cleanup stays idempotent.
func (c *Client) DeleteNetworkPorts(ctx context.Context, networkID string) (int, error) {
	var deleted int
	err := c.timed(ctx, string(KindPort), "delete", func(ctx context.Context) error {
		pages, err := ports.List(c.network, ports.ListOpts{NetworkID: networkID}).AllPages(ctx)
		if err != nil {
			return err
		}
		items, err := ports.ExtractPorts(pages)
		if err != nil {
			return err
		}
		for _, p := range items {
			if p.DeviceOwner != "" {
				continue
			}
			if err := ports.Delete(ctx, c.network, p.ID).ExtractErr(); err != nil {
				if IsNotFound(err) {
					continue
				}
				return err
			}
			deleted++
		}
		return nil
	})
	if err != nil {
		return deleted, fmt.Errorf("deleting ports on network %s: %w", networkID, err)
	}
	return deleted, nil
}
