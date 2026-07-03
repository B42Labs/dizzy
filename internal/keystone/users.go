package keystone

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/users"
	"github.com/gophercloud/gophercloud/v2/pagination"

	keystoneplan "github.com/B42Labs/openstack-tester/internal/keystone/plan"
	"github.com/B42Labs/openstack-tester/internal/resource"
)

// CreateUser creates an enabled user in domainID with the derived password. The
// password rides in the create request and is never logged. A 403 is wrapped
// with ErrForbidden so the executor fails fast.
func (c *Client) CreateUser(ctx context.Context, u keystoneplan.User, domainID, password string) (resource.Resource, error) {
	name := resourceName(c.runID, u.Name)
	var id string
	err := c.timed(ctx, string(KindUser), "create", func(ctx context.Context) error {
		created, err := users.Create(ctx, c.gc, users.CreateOpts{
			Name:     name,
			DomainID: domainID,
			Password: password,
			Enabled:  gophercloud.Enabled,
		}).Extract()
		if err != nil {
			return err
		}
		id = created.ID
		return nil
	})
	if err != nil {
		slog.Warn("create failed; a user with this name may be orphaned in the cloud", "name", name, "error", err)
		return resource.Resource{}, wrapCreate(KindUser, u.Name, err)
	}
	return resource.Resource{Kind: KindUser, Logical: u.Name, Name: name, ID: id}, nil
}

// ListUsersByPrefix returns the users whose name carries this run's
// ostester-<runID>- prefix, the discovery step cleanup deletes from. Like
// domains it fails open on a 403.
func (c *Client) ListUsersByPrefix(ctx context.Context, runID string) ([]resource.Resource, error) {
	return c.listUsers(ctx, runPrefix(runID))
}

// ListUsersByAnyRun returns every user carrying the ostester- name prefix,
// regardless of run id — the pre-flight sweep's handle.
func (c *Client) ListUsersByAnyRun(ctx context.Context) ([]resource.Resource, error) {
	return c.listUsers(ctx, namePrefix)
}

// listUsers streams the user list a page at a time and keeps only those whose
// name carries prefix, so cleanup never deletes a user the tool did not name.
func (c *Client) listUsers(ctx context.Context, prefix string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, string(KindUser), "list", func(ctx context.Context) error {
		found = nil
		return users.List(c.gc, users.ListOpts{}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
			items, err := users.ExtractUsers(page)
			if err != nil {
				return false, err
			}
			for _, u := range items {
				if strings.HasPrefix(u.Name, prefix) {
					found = append(found, resource.Resource{Kind: KindUser, Name: u.Name, ID: u.ID})
				}
			}
			return true, nil
		})
	})
	if err != nil {
		if IsForbidden(err) {
			slog.Warn("listing users denied; skipping user discovery", "error", err)
			return nil, nil
		}
		return nil, fmt.Errorf("listing users by prefix: %w", err)
	}
	return found, nil
}
