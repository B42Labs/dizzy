package keystone

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/domains"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/projects"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/roles"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/users"
	"github.com/gophercloud/gophercloud/v2/pagination"

	"github.com/B42Labs/openstack-tester/internal/resource"
)

// Observe re-queries the live state of a created resource, recording the call.
// It returns the resource's status, whether the resource still exists, and any
// error other than a 404. Domains and projects render enabled/disabled; users
// and roles have no status and render "" (shown as "present" by the status
// table); an assignment exists when the grant is still present. A 404 (or an
// absent assignment) is reported as ("", false, nil) so a resource deleted out
// of band reads as gone rather than as a failure. The status command drives this
// over a run's resources.
func (c *Client) Observe(ctx context.Context, r resource.Resource) (status string, exists bool, err error) {
	var (
		st string
		ex bool
	)
	err = c.timed(ctx, string(r.Kind), "get", func(ctx context.Context) error {
		s, e, gerr := c.observeOne(ctx, r)
		st, ex = s, e
		return gerr
	})
	switch {
	case IsNotFound(err):
		return "", false, nil
	case err != nil:
		return "", false, err
	default:
		return st, ex, nil
	}
}

// observeOne fetches one resource's live state, switching on kind.
func (c *Client) observeOne(ctx context.Context, r resource.Resource) (string, bool, error) {
	switch r.Kind {
	case KindDomain:
		d, err := domains.Get(ctx, c.gc, r.ID).Extract()
		if err != nil {
			return "", false, err
		}
		return enabledStatus(d.Enabled), true, nil
	case KindProject:
		p, err := projects.Get(ctx, c.gc, r.ID).Extract()
		if err != nil {
			return "", false, err
		}
		return enabledStatus(p.Enabled), true, nil
	case KindUser:
		if _, err := users.Get(ctx, c.gc, r.ID).Extract(); err != nil {
			return "", false, err
		}
		return "", true, nil
	case KindRole:
		if _, err := roles.Get(ctx, c.gc, r.ID).Extract(); err != nil {
			return "", false, err
		}
		return "", true, nil
	case KindAssignment:
		ex, err := c.assignmentExists(ctx, r)
		if err != nil {
			return "", false, err
		}
		return "", ex, nil
	default:
		return "", false, fmt.Errorf("observe not supported for kind %q", r.Kind)
	}
}

// enabledStatus renders a domain/project's enabled flag as the status string the
// table shows.
func enabledStatus(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

// assignmentExists reports whether the grant a synthetic assignment id names is
// still present, by querying the user's assignments filtered to that role and
// scope.
func (c *Client) assignmentExists(ctx context.Context, r resource.Resource) (bool, error) {
	t, err := parseAssignmentID(r.ID)
	if err != nil {
		return false, err
	}
	opts := roles.ListAssignmentsOpts{UserID: t.userID, RoleID: t.roleID}
	if t.kind == "project" {
		opts.ScopeProjectID = t.targetID
	} else {
		opts.ScopeDomainID = t.targetID
	}
	found := false
	err = roles.ListAssignments(c.gc, opts).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
		items, err := roles.ExtractRoleAssignments(page)
		if err != nil {
			return false, err
		}
		if len(items) > 0 {
			found = true
			return false, nil // stop paging: presence is enough
		}
		return true, nil
	})
	return found, err
}

// Delete removes a resource, recording the call. Domains, projects, users, and
// roles are deleted by id; an assignment is revoked by parsing its synthetic id
// back into an unassign. A 404 surfaces to the caller, which treats an
// already-gone resource as success to keep cleanup idempotent. Deleting a domain
// requires it be disabled first (DisableDomain), which the cleanup path does.
func (c *Client) Delete(ctx context.Context, r resource.Resource) error {
	return c.timed(ctx, string(r.Kind), "delete", func(ctx context.Context) error {
		switch r.Kind {
		case KindDomain:
			return domains.Delete(ctx, c.gc, r.ID).ExtractErr()
		case KindProject:
			return projects.Delete(ctx, c.gc, r.ID).ExtractErr()
		case KindUser:
			return users.Delete(ctx, c.gc, r.ID).ExtractErr()
		case KindRole:
			return roles.Delete(ctx, c.gc, r.ID).ExtractErr()
		case KindAssignment:
			t, err := parseAssignmentID(r.ID)
			if err != nil {
				return err
			}
			opts := roles.UnassignOpts{UserID: t.userID}
			if t.kind == "project" {
				opts.ProjectID = t.targetID
			} else {
				opts.DomainID = t.targetID
			}
			return roles.Unassign(ctx, c.gc, t.roleID, opts).ExtractErr()
		default:
			return fmt.Errorf("delete not supported for kind %q", r.Kind)
		}
	})
}
