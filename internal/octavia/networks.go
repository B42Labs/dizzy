package octavia

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/attributestags"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	"github.com/gophercloud/gophercloud/v2/pagination"

	"github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// tagAttempts and tagRetryDelay bound how often a transient tag failure is
// retried in place. Tag retries live here, not in the executor's create retry,
// so a retryable tag error never re-enters create and duplicates the resource.
// tagRetryDelay is a variable so tests can shrink it.
const tagAttempts = 3

var tagRetryDelay = 250 * time.Millisecond

// Last octets of the addresses a VIP subnet hands out: the gateway at .1 and the
// allocation pool .2 to .127, the lower half of the /24. Neutron allocates the
// VIP ports and any amphora ports from that pool, so it never hands out an
// address in the upper half the plan gives the members.
const (
	gatewayOctet   = 1
	poolStartOctet = 2
	poolEndOctet   = 127
)

// CreateNetwork creates a tagged VIP network.
func (c *Client) CreateNetwork(ctx context.Context, n plan.Network) (resource.Resource, error) {
	return c.createTagged(ctx, KindNetwork, n.Name, func(ctx context.Context, name string) (string, error) {
		created, err := networks.Create(ctx, c.network, networks.CreateOpts{Name: name}).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}

// CreateSubnet creates the tagged /24 VIP subnet of n on networkID with the
// plan's CIDR, its gateway at .1, and one allocation pool from .2 to .127, so
// the addresses Neutron allocates stay out of the upper half the plan's members
// use. A CIDR that is not an IPv4 /24 is refused without a request.
func (c *Client) CreateSubnet(ctx context.Context, n plan.Network, networkID string) (resource.Resource, error) {
	gateway, start, end, err := subnetAddresses(n.CIDR)
	if err != nil {
		return resource.Resource{}, fmt.Errorf("creating %s %q: %w", KindSubnet, n.Subnet, err)
	}
	return c.createTagged(ctx, KindSubnet, n.Subnet, func(ctx context.Context, name string) (string, error) {
		created, err := subnets.Create(ctx, c.network, subnets.CreateOpts{
			NetworkID:       networkID,
			Name:            name,
			IPVersion:       4,
			CIDR:            n.CIDR,
			GatewayIP:       &gateway,
			AllocationPools: []subnets.AllocationPool{{Start: start, End: end}},
		}).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}

// subnetAddresses returns the gateway address and the first and last address
// of the allocation pool of the IPv4 /24 cidr.
func subnetAddresses(cidr string) (gateway, start, end string, err error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 24 {
		return "", "", "", fmt.Errorf("CIDR %q is not an IPv4 /24", cidr)
	}
	base := prefix.Masked().Addr().As4()
	at := func(octet byte) string {
		a := base
		a[3] = octet
		return netip.AddrFrom4(a).String()
	}
	return at(gatewayOctet), at(poolStartOctet), at(poolEndOctet), nil
}

// tagCollection maps a taggable kind to its Neutron tag-extension collection
// path.
func tagCollection(kind resource.Kind) string {
	switch kind {
	case KindNetwork:
		return "networks"
	case KindSubnet:
		return "subnets"
	default:
		return ""
	}
}

// createTagged centralizes the create-then-tag flow shared by the two Neutron
// kinds (network, subnet): it applies the deterministic name via create,
// records the create and tag timings, wraps quota errors with ErrQuota, and
// returns the resulting Resource. A tag failure (after a bounded in-place
// retry) rolls the created resource back and fails the create, so a resource is
// never left created-but-untagged where tag-based cleanup cannot reclaim it. It
// mirrors the nova client's createTagged.
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

// listTagged runs a tagged list pager to completion and collects into
// Resources of kind the items whose tags contain tag. describe pulls the cloud
// name, id, and tags from each item. The tag check is the client-side backstop
// the tagged list paths apply, so a server that ignores the tags query never
// widens the result beyond what the tool actually tagged.
func listTagged[T any](
	ctx context.Context,
	kind resource.Kind,
	pager pagination.Pager,
	extract func(pagination.Page) ([]T, error),
	describe func(T) (name, id string, tags []string),
	tag string,
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
		name, id, tags := describe(it)
		if !slices.Contains(tags, tag) {
			continue
		}
		out = append(out, resource.Resource{Kind: kind, Name: name, ID: id})
	}
	return out, nil
}

// WaitForReady polls a created VIP network until it is ACTIVE, recording one
// Readiness sample under network. A subnet reports no status, so it returns nil
// at once. A 404 aborts with a gone error; any other poll error is treated as
// transient and polled through. It returns ctx.Err() if ctx is cancelled or its
// deadline elapses first; the caller decides whether a readiness deadline is
// fatal. Any other kind is refused without a request: a load balancer waits
// through WaitForActive.
func (c *Client) WaitForReady(ctx context.Context, r resource.Resource) error {
	switch r.Kind {
	case KindSubnet:
		return nil
	case KindNetwork:
	default:
		return fmt.Errorf("wait for ready not supported for kind %q", r.Kind)
	}

	start := time.Now()
	backoff := pollInitial
	for {
		n, err := networks.Get(ctx, c.network, r.ID).Extract()
		switch {
		case err == nil && n.Status == statusNetworkActive:
			c.recordReady(ctx, KindNetwork, time.Since(start), true)
			return nil
		case IsNotFound(err):
			c.recordReady(ctx, KindNetwork, time.Since(start), false)
			return fmt.Errorf("%s %s is gone while waiting for readiness: %w", r.Kind, r.ID, err)
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			c.recordReady(ctx, KindNetwork, time.Since(start), false)
			return ctx.Err()
		}
		backoff = nextBackoff(backoff)
	}
}
