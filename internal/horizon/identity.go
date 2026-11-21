package horizon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/projects"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/roles"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/users"
	"github.com/gophercloud/gophercloud/v2/pagination"

	"github.com/B42Labs/dizzy/internal/horizon/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// namePrefix is the common prefix of every user and project a Horizon run
// creates. A run's resources are named namePrefix + runID + "-" + logical.
// It differs from the keystone namespace's dizzy-, whose orphan sweep
// deletes every user named dizzy-*.
const namePrefix = "dizzyhorizon-"

// listPageSize is the limit the user and project listings send. Keystone 29
// answers a listing without limit with at most max_db_limit (1,000) entries
// and no next link; with limit, each page links to the next. It duplicates
// the keystone namespace's value rather than importing it.
const listPageSize = 500

// pagedUserListOpts asks for listPageSize users per page. gophercloud's
// users.ListOpts has no Limit field, so the query is built here.
type pagedUserListOpts struct{}

// ToUserListQuery returns the limit query.
func (pagedUserListOpts) ToUserListQuery() (string, error) {
	return "?limit=" + strconv.Itoa(listPageSize), nil
}

// The project tags binding a created project to a run and to its kind. They
// differ from the keystone namespace's dizzy:run= and dizzy:type=, so its
// sweep never reaches a Horizon run's projects.
const (
	tagRun  = "dizzyhorizon:run="
	tagType = "dizzyhorizon:type="
)

// forbiddenHint is what a 403 on a create tells the operator.
const forbiddenHint = "resources.users needs a clouds.yaml entry that may create projects and users in its domain"

// HasNamePrefix reports whether name carries the dizzyhorizon- prefix every
// user and project a Horizon run creates. Cleanup uses it as a guard, so a
// resource without it is never deleted.
func HasNamePrefix(name string) bool {
	return strings.HasPrefix(name, namePrefix)
}

// runPrefix is the name prefix shared by every user and project of one run.
func runPrefix(runID string) string {
	return namePrefix + runID + "-"
}

// resourceName builds the cloud name of a logical plan name.
func resourceName(runID, logical string) string {
	return runPrefix(runID) + logical
}

// wrapCreate adds context to a create error and threads ErrForbidden, with
// its hint, into a 403, so the executor stops instead of retrying.
func wrapCreate(kind resource.Kind, logical string, err error) error {
	if gophercloud.ResponseCodeIs(err, 403) {
		return fmt.Errorf("creating %s %q: %w: %w; %s", kind, logical, ErrForbidden, err, forbiddenHint)
	}
	return fmt.Errorf("creating %s %q: %w", kind, logical, err)
}

// CreateProject creates an enabled project in domainID, named for the run
// and tagged with the run and its kind.
func (c *Client) CreateProject(ctx context.Context, p plan.Project, domainID string) (resource.Resource, error) {
	name := resourceName(c.runID, p.Name)
	var id string
	err := c.timed(ctx, KindProject, "create", func(ctx context.Context) error {
		created, err := projects.Create(ctx, c.identity, projects.CreateOpts{
			Name:     name,
			DomainID: domainID,
			Enabled:  gophercloud.Enabled,
			Tags:     []string{tagRun + c.runID, tagType + "project"},
		}).Extract()
		if err != nil {
			return err
		}
		id = created.ID
		return nil
	})
	if err != nil {
		slog.Warn("create failed; a project with this name may be orphaned in the cloud", "name", name, "error", err)
		return resource.Resource{}, wrapCreate(KindProject, p.Name, err)
	}
	return resource.Resource{Kind: KindProject, Logical: p.Name, Name: name, ID: id}, nil
}

// CreateUser creates an enabled user in domainID with password, named for
// the run. The password rides in the request and is never logged.
func (c *Client) CreateUser(ctx context.Context, u plan.User, domainID, password string) (resource.Resource, error) {
	name := resourceName(c.runID, u.Name)
	var id string
	err := c.timed(ctx, KindUser, "create", func(ctx context.Context) error {
		created, err := users.Create(ctx, c.identity, users.CreateOpts{
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

// AssignRole grants roleID to userID on projectID. Keystone removes the
// assignment with the user, so it is not recorded as a resource.
func (c *Client) AssignRole(ctx context.Context, userID, projectID, roleID string) error {
	err := c.timed(ctx, KindRoleAssignment, "create", func(ctx context.Context) error {
		return roles.Assign(ctx, c.identity, roleID, roles.AssignOpts{UserID: userID, ProjectID: projectID}).ExtractErr()
	})
	if err != nil {
		return wrapCreate(KindRoleAssignment, userID+"@"+projectID, err)
	}
	return nil
}

// ListUsersByRun returns the users named for runID.
func (c *Client) ListUsersByRun(ctx context.Context, runID string) ([]resource.Resource, error) {
	return c.listUsers(ctx, runPrefix(runID))
}

// ListUsersByAnyRun returns every user named dizzyhorizon-, the handle of
// the monitor's opt-in sweep.
func (c *Client) ListUsersByAnyRun(ctx context.Context) ([]resource.Resource, error) {
	return c.listUsers(ctx, namePrefix)
}

// listUsers lists the users page by page, listPageSize per page, and keeps
// those whose name starts with prefix. A 403 logs a warning and returns no
// users and no error, as the keystone namespace's listing does.
func (c *Client) listUsers(ctx context.Context, prefix string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, KindUser, "list", func(ctx context.Context) error {
		found = nil
		return users.List(c.identity, pagedUserListOpts{}).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
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
		if gophercloud.ResponseCodeIs(err, 403) {
			slog.Warn("listing users denied; skipping user discovery", "error", err)
			return nil, nil
		}
		return nil, fmt.Errorf("listing users: %w", err)
	}
	return found, nil
}

// ListProjectsByRun returns the projects tagged with runID's run tag.
func (c *Client) ListProjectsByRun(ctx context.Context, runID string) ([]resource.Resource, error) {
	return c.listProjects(ctx, tagRun+runID, runPrefix(runID))
}

// ListProjectsByAnyRun returns every project tagged
// dizzyhorizon:type=project, the handle of the monitor's opt-in sweep.
func (c *Client) ListProjectsByAnyRun(ctx context.Context) ([]resource.Resource, error) {
	return c.listProjects(ctx, tagType+"project", namePrefix)
}

// listProjects lists the projects carrying tag, filtered by Keystone,
// listPageSize per page, and rechecks the tag and the name prefix itself, so
// a server that ignores the filter never widens the result. A 403 logs a
// warning and returns no projects and no error.
func (c *Client) listProjects(ctx context.Context, tag, prefix string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, KindProject, "list", func(ctx context.Context) error {
		found = nil
		return projects.List(c.identity, projects.ListOpts{Tags: tag, Limit: listPageSize}).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
			items, err := projects.ExtractProjects(page)
			if err != nil {
				return false, err
			}
			for _, pr := range items {
				if slices.Contains(pr.Tags, tag) && strings.HasPrefix(pr.Name, prefix) {
					found = append(found, resource.Resource{Kind: KindProject, Name: pr.Name, ID: pr.ID})
				}
			}
			return true, nil
		})
	})
	if err != nil {
		if gophercloud.ResponseCodeIs(err, 403) {
			slog.Warn("listing projects denied; skipping project discovery", "error", err)
			return nil, nil
		}
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	return found, nil
}

// Delete deletes a user or a project by id. Keystone removes a user's role
// assignments with the user. It returns Keystone's error, a 404 included,
// which cleanup counts as already gone. Another kind returns an error
// without a request.
func (c *Client) Delete(ctx context.Context, r resource.Resource) error {
	var del func(context.Context) error
	switch r.Kind {
	case KindUser:
		del = func(ctx context.Context) error { return users.Delete(ctx, c.identity, r.ID).ExtractErr() }
	case KindProject:
		del = func(ctx context.Context) error { return projects.Delete(ctx, c.identity, r.ID).ExtractErr() }
	default:
		return fmt.Errorf("unknown horizon resource kind %q", r.Kind)
	}
	return c.timed(ctx, r.Kind, "delete", del)
}

// Observe returns whether a user or a project is enabled or disabled, and
// exists false when Keystone answers 404.
func (c *Client) Observe(ctx context.Context, r resource.Resource) (string, bool, error) {
	var enabled bool
	err := c.timed(ctx, r.Kind, "get", func(ctx context.Context) error {
		switch r.Kind {
		case KindUser:
			u, err := users.Get(ctx, c.identity, r.ID).Extract()
			if err != nil {
				return err
			}
			enabled = u.Enabled
		case KindProject:
			p, err := projects.Get(ctx, c.identity, r.ID).Extract()
			if err != nil {
				return err
			}
			enabled = p.Enabled
		default:
			return fmt.Errorf("unknown horizon resource kind %q", r.Kind)
		}
		return nil
	})
	switch {
	case IsNotFound(err):
		return "", false, nil
	case err != nil:
		return "", false, err
	case enabled:
		return "enabled", true, nil
	default:
		return "disabled", true, nil
	}
}

// AvailableProjects returns the ids of the enabled projects the token's user
// may scope to (GET /v3/auth/projects), sorted. It is discovery, not
// workload, and records no sample.
func (c *Client) AvailableProjects(ctx context.Context) ([]string, error) {
	var ids []string
	err := projects.ListAvailable(c.identity).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		items, err := projects.ExtractProjects(page)
		if err != nil {
			return false, err
		}
		for _, p := range items {
			if p.Enabled {
				ids = append(ids, p.ID)
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing the user's projects: %w", err)
	}
	slices.Sort(ids)
	return ids, nil
}
