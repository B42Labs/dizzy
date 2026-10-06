package nova

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servergroups"
	"github.com/gophercloud/gophercloud/v2/pagination"

	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// CreateServerGroup creates a server group with the deterministic name and the
// planned policy. The compute client's microversion takes the policy as the
// one-element list policies. A server group carries neither metadata nor
// tags, so teardown finds it by name through ListServerGroupsByName. A quota
// rejection is wrapped with ErrQuota so the executor fails fast.
func (c *Client) CreateServerGroup(ctx context.Context, g novaplan.ServerGroup) (resource.Resource, error) {
	name := resourceName(c.runID, g.Name)
	opts := servergroups.CreateOpts{Name: name, Policies: []string{g.Policy}}

	var id string
	err := c.timed(ctx, string(KindServerGroup), "create", func(ctx context.Context) error {
		created, err := servergroups.Create(ctx, c.compute, opts).Extract()
		if err != nil {
			return err
		}
		id = created.ID
		return nil
	})
	if err != nil {
		return resource.Resource{}, wrapCreate(KindServerGroup, g.Name, err)
	}
	return resource.Resource{Kind: KindServerGroup, Logical: g.Name, Name: name, ID: id}, nil
}

// ListServerGroupsByName returns the project's server groups whose name
// carries the dizzy-<runID>- prefix, the discovery step teardown deletes from.
// The prefix ends in a dash, so the groups of run1 never include those of
// run10. Like Keystone's name discovery it fails open on a 403: it logs a
// warning and returns no group.
func (c *Client) ListServerGroupsByName(ctx context.Context, runID string) ([]resource.Resource, error) {
	prefix := resourceName(runID, "")
	var found []resource.Resource
	err := c.timed(ctx, string(KindServerGroup), "list", func(ctx context.Context) error {
		found = nil
		return servergroups.List(c.compute, servergroups.ListOpts{}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
			items, err := servergroups.ExtractServerGroups(page)
			if err != nil {
				return false, err
			}
			for _, g := range items {
				if strings.HasPrefix(g.Name, prefix) {
					found = append(found, resource.Resource{Kind: KindServerGroup, Name: g.Name, ID: g.ID})
				}
			}
			return true, nil
		})
	})
	if err != nil {
		if gophercloud.ResponseCodeIs(err, 403) {
			slog.Warn("listing server groups denied; skipping server group discovery", "error", err)
			return nil, nil
		}
		return nil, fmt.Errorf("listing server groups by name: %w", err)
	}
	return found, nil
}
