package keystone

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/roles"
	"github.com/gophercloud/gophercloud/v2/pagination"

	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// CreateRole creates a global role (no domain) with the deterministic name. A
// 403 is wrapped with ErrForbidden so the executor fails fast (only a cloud
// admin may create roles).
func (c *Client) CreateRole(ctx context.Context, r keystoneplan.Role) (resource.Resource, error) {
	name := resourceName(c.runID, r.Name)
	var id string
	err := c.timed(ctx, string(KindRole), "create", func(ctx context.Context) error {
		created, err := roles.Create(ctx, c.gc, roles.CreateOpts{Name: name}).Extract()
		if err != nil {
			return err
		}
		id = created.ID
		return nil
	})
	if err != nil {
		slog.Warn("create failed; a role with this name may be orphaned in the cloud", "name", name, "error", err)
		return resource.Resource{}, wrapCreate(KindRole, r.Name, err)
	}
	return resource.Resource{Kind: KindRole, Logical: r.Name, Name: name, ID: id}, nil
}

// CreateImpliedRole creates the implied-role rule r: a grant of the role
// priorID also gives its holder the role impliedID on the same target
// (PUT /v3/roles/<prior>/implies/<implied>). It returns a synthetic-id
// resource, since a rule has no cloud id or name, that status re-queries and
// cleanup deletes by parsing. A 403 is wrapped with ErrForbidden; a 409, which
// Keystone answers for a rule that already exists, stays terminal.
func (c *Client) CreateImpliedRole(ctx context.Context, r keystoneplan.ImpliedRole, priorID, impliedID string) (resource.Resource, error) {
	err := c.timed(ctx, string(KindImpliedRole), "create", func(ctx context.Context) error {
		return roles.CreateRoleInferenceRule(ctx, c.gc, priorID, impliedID).Err
	})
	if err != nil {
		return resource.Resource{}, wrapCreate(KindImpliedRole, impliedRoleLogical(r), err)
	}
	return resource.Resource{Kind: KindImpliedRole, Logical: impliedRoleLogical(r), ID: impliedRoleID(priorID, impliedID)}, nil
}

// impliedRoleID builds the synthetic implied-role rule id
// "<priorID>:<impliedID>" that parseImpliedRoleID inverts. It is the one place
// the format is written.
func impliedRoleID(priorID, impliedID string) string {
	return priorID + ":" + impliedID
}

// parseImpliedRoleID inverts impliedRoleID. Keystone ids are hex, so ":" is an
// unambiguous separator.
func parseImpliedRoleID(id string) (priorID, impliedID string, err error) {
	parts := strings.Split(id, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("malformed implied role id %q", id)
	}
	return parts[0], parts[1], nil
}

// impliedRoleLogical renders a human-readable label for an implied-role rule,
// for the status table's LOGICAL column (a rule has no cloud name).
func impliedRoleLogical(r keystoneplan.ImpliedRole) string {
	return r.Prior + "->" + r.Implied
}

// ErrRoleNotFound is what FindRoleByName's error wraps when no role has the
// name, so a caller can tell a missing role from a failed listing. Its text is
// the "not found" of that error's message.
var ErrRoleNotFound = errors.New("not found")

// FindRoleByName resolves an existing role by name, preferring a global role (no
// domain) over a domain-scoped one. It is the domain-manager binder's resolver
// for the reusable roles the run assigns; a plain API call, kept out of the run
// metrics because it is discovery, not part of the workload. It errors when no
// role matches, wrapping ErrRoleNotFound, so a typo in --roles fails clearly
// before any assignment.
func (c *Client) FindRoleByName(ctx context.Context, name string) (string, error) {
	var (
		globalID string
		anyID    string
	)
	err := roles.List(c.gc, roles.ListOpts{Name: name}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
		items, err := roles.ExtractRoles(page)
		if err != nil {
			return false, err
		}
		for _, r := range items {
			if r.Name != name {
				continue
			}
			anyID = r.ID
			if r.DomainID == "" {
				globalID = r.ID
			}
		}
		return true, nil
	})
	if err != nil {
		return "", fmt.Errorf("listing roles named %q: %w", name, err)
	}
	switch {
	case globalID != "":
		return globalID, nil
	case anyID != "":
		return anyID, nil
	default:
		return "", fmt.Errorf("role %q %w; grant it to the manager or pass an existing role with --roles", name, ErrRoleNotFound)
	}
}

// AssignRole grants a role to a user on a project (when projectID is set) or on
// the user's domain (domainID). It returns a synthetic-id assignment resource —
// assignments have no cloud id or name — that status re-queries and cleanup
// revokes by parsing. A 403 is wrapped with ErrForbidden (a manager may not
// grant the admin role).
func (c *Client) AssignRole(ctx context.Context, a keystoneplan.Assignment, userID, projectID, domainID, roleID string) (resource.Resource, error) {
	opts := roles.AssignOpts{UserID: userID}
	if projectID != "" {
		opts.ProjectID = projectID
	} else {
		opts.DomainID = domainID
	}
	err := c.timed(ctx, string(KindAssignment), "create", func(ctx context.Context) error {
		return roles.Assign(ctx, c.gc, roleID, opts).ExtractErr()
	})
	if err != nil {
		return resource.Resource{}, wrapCreate(KindAssignment, assignmentLogical(a), err)
	}
	return AssignmentResource(a, userID, projectID, domainID, roleID), nil
}

// AssignmentResource returns the synthetic-id resource AssignRole records for
// the grant of roleID to userID on projectID (when set) or on domainID. A
// caller that churns an existing grant builds the resource to pass to Delete
// with it, without a cloud call, so the id always has the shape Delete and
// cleanup parse.
func AssignmentResource(a keystoneplan.Assignment, userID, projectID, domainID, roleID string) resource.Resource {
	kind, targetID := "domain", domainID
	if projectID != "" {
		kind, targetID = "project", projectID
	}
	return resource.Resource{
		Kind:    KindAssignment,
		Logical: assignmentLogical(a),
		ID:      assignmentID(userID, kind, targetID, roleID),
	}
}

// assignmentID builds the synthetic assignment id
// "<userID>:project|domain:<targetID>:<roleID>" that parseAssignmentID inverts.
// It is the one place the format is written.
func assignmentID(userID, kind, targetID, roleID string) string {
	return userID + ":" + kind + ":" + targetID + ":" + roleID
}

// assignmentLogical renders a human-readable label for an assignment, for the
// status table's LOGICAL column (assignments have no cloud name).
func assignmentLogical(a keystoneplan.Assignment) string {
	target := a.Project
	if target == "" {
		target = "domain"
	}
	return a.User + "->" + target + "/" + a.Role
}

// assignmentTarget is the parsed (kind, userID, targetID, roleID) of a synthetic
// assignment id.
type assignmentTarget struct {
	userID   string
	kind     string // "project" or "domain"
	targetID string
	roleID   string
}

// parseAssignmentID inverts AssignRole's synthetic id
// "<userID>:project|domain:<targetID>:<roleID>". Keystone ids are hex, so ":" is
// an unambiguous separator.
func parseAssignmentID(id string) (assignmentTarget, error) {
	parts := strings.Split(id, ":")
	if len(parts) != 4 || (parts[1] != "project" && parts[1] != "domain") {
		return assignmentTarget{}, fmt.Errorf("malformed assignment id %q", id)
	}
	return assignmentTarget{userID: parts[0], kind: parts[1], targetID: parts[2], roleID: parts[3]}, nil
}

// ListAssignmentsForUser returns the role assignments held by a user as
// synthetic-id assignment resources — the discovery handle cleanup revokes from
// when reclaiming a run whose record is unavailable. Only project- and
// domain-scoped grants are represented (the kinds this tool creates).
func (c *Client) ListAssignmentsForUser(ctx context.Context, userID string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, string(KindAssignment), "list", func(ctx context.Context) error {
		found = nil
		return roles.ListAssignments(c.gc, roles.ListAssignmentsOpts{UserID: userID}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
			items, err := roles.ExtractRoleAssignments(page)
			if err != nil {
				return false, err
			}
			for _, ra := range items {
				switch {
				case ra.Scope.Project.ID != "":
					found = append(found, resource.Resource{
						Kind: KindAssignment,
						ID:   assignmentID(userID, "project", ra.Scope.Project.ID, ra.Role.ID),
					})
				case ra.Scope.Domain.ID != "":
					found = append(found, resource.Resource{
						Kind: KindAssignment,
						ID:   assignmentID(userID, "domain", ra.Scope.Domain.ID, ra.Role.ID),
					})
				}
			}
			return true, nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing assignments for user %s: %w", userID, err)
	}
	return found, nil
}

// ListEffectiveAssignments lists the effective role assignments of a user
// (GET /v3/role_assignments?user.id=<userID>&effective=true) over every page
// and returns how many there are. Keystone expands group memberships and
// inherited grants for an effective listing. It is recorded as the
// role_assignment/list operation.
func (c *Client) ListEffectiveAssignments(ctx context.Context, userID string) (int, error) {
	effective := true
	var n int
	err := c.timed(ctx, string(KindAssignment), "list", func(ctx context.Context) error {
		n = 0
		return roles.ListAssignments(c.gc, roles.ListAssignmentsOpts{UserID: userID, Effective: &effective}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
			items, err := roles.ExtractRoleAssignments(page)
			if err != nil {
				return false, err
			}
			n += len(items)
			return true, nil
		})
	})
	if err != nil {
		return 0, fmt.Errorf("listing effective assignments for user %s: %w", userID, err)
	}
	return n, nil
}

// ListRolesByPrefix returns the roles whose name carries this run's
// dizzy-<runID>- prefix. Like domains it fails open on a 403.
func (c *Client) ListRolesByPrefix(ctx context.Context, runID string) ([]resource.Resource, error) {
	return c.listRoles(ctx, runPrefix(runID))
}

// ListRolesByAnyRun returns every role carrying the dizzy- name prefix,
// regardless of run id — the pre-flight sweep's handle.
func (c *Client) ListRolesByAnyRun(ctx context.Context) ([]resource.Resource, error) {
	return c.listRoles(ctx, namePrefix)
}

// listRoles streams the role list a page at a time and keeps only those whose
// name carries prefix, so cleanup never deletes a role the tool did not name —
// critical in domain-manager mode where reused roles carry no prefix.
func (c *Client) listRoles(ctx context.Context, prefix string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, string(KindRole), "list", func(ctx context.Context) error {
		found = nil
		return roles.List(c.gc, roles.ListOpts{}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
			items, err := roles.ExtractRoles(page)
			if err != nil {
				return false, err
			}
			for _, r := range items {
				if strings.HasPrefix(r.Name, prefix) {
					found = append(found, resource.Resource{Kind: KindRole, Name: r.Name, ID: r.ID})
				}
			}
			return true, nil
		})
	})
	if err != nil {
		if IsForbidden(err) {
			slog.Warn("listing roles denied (domain manager?); skipping role discovery", "error", err)
			return nil, nil
		}
		return nil, fmt.Errorf("listing roles by prefix: %w", err)
	}
	return found, nil
}
