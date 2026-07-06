package keystone

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/projects"
	"github.com/gophercloud/gophercloud/v2/pagination"

	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// CreateProject creates an enabled project in domainID, tagged with the run and
// type so it can be discovered server-side. A 403 is wrapped with ErrForbidden
// so the executor fails fast.
func (c *Client) CreateProject(ctx context.Context, p keystoneplan.Project, domainID string) (resource.Resource, error) {
	name := resourceName(c.runID, p.Name)
	var id string
	err := c.timed(ctx, string(KindProject), "create", func(ctx context.Context) error {
		created, err := projects.Create(ctx, c.gc, projects.CreateOpts{
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

// ListProjectsByTag returns the projects carrying this run's ostester:run=<id>
// tag, the discovery step cleanup deletes from. Projects are the only identity
// kind that supports tags, so this is a server-side filter (rechecked
// client-side).
func (c *Client) ListProjectsByTag(ctx context.Context, runID string) ([]resource.Resource, error) {
	return c.listProjects(ctx, tagRun+runID)
}

// ListProjectsByType returns the projects carrying the ostester:type=project
// tag, matching any tester run — the pre-flight sweep's handle for reclaiming a
// previous crashed iteration's projects whose run id is no longer known.
func (c *Client) ListProjectsByType(ctx context.Context) ([]resource.Resource, error) {
	return c.listProjects(ctx, tagType+"project")
}

// listProjects streams the project list filtered server-side by tag a page at a
// time, rechecking the tag client-side so a server that ignores the filter never
// widens the result beyond what the tool actually tagged.
func (c *Client) listProjects(ctx context.Context, tag string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, string(KindProject), "list", func(ctx context.Context) error {
		found = nil
		return projects.List(c.gc, projects.ListOpts{Tags: tag}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
			items, err := projects.ExtractProjects(page)
			if err != nil {
				return false, err
			}
			for _, pr := range items {
				if !hasTag(pr.Tags, tag) {
					continue // server-side filter not honored; never delete a project this tool did not tag
				}
				found = append(found, resource.Resource{Kind: KindProject, Name: pr.Name, ID: pr.ID})
			}
			return true, nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing projects by tag: %w", err)
	}
	return found, nil
}

// hasTag reports whether tags contains want. It is the client-side backstop the
// tag list path applies, so a server that ignores the tag query never widens the
// result beyond what the tool tagged.
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
