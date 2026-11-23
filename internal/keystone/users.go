package keystone

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/users"
	"github.com/gophercloud/gophercloud/v2/pagination"

	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// CreateUser creates an enabled user in domainID with the derived password. The
// password rides in the create request and is never logged. A 403 is wrapped
// with ErrForbidden so the executor fails fast. A 409 whose lookup finds the
// user of that name in domainID adopts it and records as a successful create:
// it is this run's user, made by a create whose answer was lost or by an
// earlier instance of the node whose deletes failed, with the password the
// caller still holds.
func (c *Client) CreateUser(ctx context.Context, u keystoneplan.User, domainID, password string) (resource.Resource, error) {
	name := resourceName(c.runID, u.Name)
	var id string
	_, err := c.timedFold(ctx, string(KindUser), "create", func(ctx context.Context) error {
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
	}, adoptOnConflict(KindUser, name, &id, func(ctx context.Context) (string, error) {
		return c.findUser(ctx, name, domainID)
	}))
	if err != nil {
		slog.Warn("create failed; a user with this name may be orphaned in the cloud", "name", name, "error", err)
		return resource.Resource{}, wrapCreate(KindUser, u.Name, err)
	}
	return resource.Resource{Kind: KindUser, Logical: u.Name, Name: name, ID: id}, nil
}

// findUser returns the id of the user named name in domainID, or "" when
// there is none: the lookup a create answered 409 adopts by. It lists users
// filtered by both, outside timed, and rechecks both client side, so a server
// that ignores the filter never widens the match. An empty domainID matches
// nothing.
func (c *Client) findUser(ctx context.Context, name, domainID string) (string, error) {
	var found string
	err := users.List(c.gc, users.ListOpts{Name: name, DomainID: domainID}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
		items, err := users.ExtractUsers(page)
		if err != nil {
			return false, err
		}
		for _, u := range items {
			if u.Name == name && domainID != "" && u.DomainID == domainID {
				found = u.ID
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return "", fmt.Errorf("listing users named %q: %w", name, err)
	}
	return found, nil
}

// ListUsersByPrefix returns the users whose name carries this run's
// dizzy-<runID>- prefix, the discovery step cleanup deletes from. Like
// domains it fails open on a 403.
func (c *Client) ListUsersByPrefix(ctx context.Context, runID string) ([]resource.Resource, error) {
	return c.listUsers(ctx, runPrefix(runID))
}

// ListUsersByAnyRun returns every user carrying the dizzy- name prefix,
// regardless of run id — the pre-flight sweep's handle.
func (c *Client) ListUsersByAnyRun(ctx context.Context) ([]resource.Resource, error) {
	return c.listUsers(ctx, namePrefix)
}

// pagedUserListOpts asks for listPageSize users per page. gophercloud's
// users.ListOpts has no Limit field, so the query is built here; UserPage
// follows links.next, which carries Keystone's marker.
type pagedUserListOpts struct{}

// ToUserListQuery returns the limit query.
func (pagedUserListOpts) ToUserListQuery() (string, error) {
	return "?limit=" + strconv.Itoa(listPageSize), nil
}

// listUsers streams the user list a page at a time, each request carrying
// limit so Keystone links every page to the next, and keeps only those whose
// name carries prefix, so cleanup never deletes a user the tool did not name.
func (c *Client) listUsers(ctx context.Context, prefix string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, string(KindUser), "list", func(ctx context.Context) error {
		found = nil
		return users.List(c.gc, pagedUserListOpts{}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
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
