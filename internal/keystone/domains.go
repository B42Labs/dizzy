package keystone

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/domains"
	"github.com/gophercloud/gophercloud/v2/pagination"

	keystoneplan "github.com/B42Labs/openstack-tester/internal/keystone/plan"
	"github.com/B42Labs/openstack-tester/internal/resource"
)

// CreateDomain creates an enabled domain with the deterministic name. Domains
// carry no tags — their name prefix is their only run identity. A 403 is wrapped
// with ErrForbidden so the executor fails fast (only a cloud admin may create
// domains).
func (c *Client) CreateDomain(ctx context.Context, d keystoneplan.Domain) (resource.Resource, error) {
	name := resourceName(c.runID, d.Name)
	var id string
	err := c.timed(ctx, string(KindDomain), "create", func(ctx context.Context) error {
		created, err := domains.Create(ctx, c.gc, domains.CreateOpts{
			Name:    name,
			Enabled: gophercloud.Enabled,
		}).Extract()
		if err != nil {
			return err
		}
		id = created.ID
		return nil
	})
	if err != nil {
		slog.Warn("create failed; a domain with this name may be orphaned in the cloud", "name", name, "error", err)
		return resource.Resource{}, wrapCreate(KindDomain, d.Name, err)
	}
	return resource.Resource{Kind: KindDomain, Logical: d.Name, Name: name, ID: id}, nil
}

// DisableDomain disables a domain so it can be deleted: Keystone refuses to
// delete an enabled domain. It records under the "update" operation label, the
// one new operation Keystone adds.
func (c *Client) DisableDomain(ctx context.Context, r resource.Resource) error {
	err := c.timed(ctx, string(KindDomain), "update", func(ctx context.Context) error {
		_, err := domains.Update(ctx, c.gc, r.ID, domains.UpdateOpts{Enabled: gophercloud.Disabled}).Extract()
		return err
	})
	if err != nil {
		return fmt.Errorf("disabling domain %s: %w", r.ID, err)
	}
	return nil
}

// ListDomainsByPrefix returns the domains whose name carries this run's
// ostester-<runID>- prefix, the discovery step cleanup deletes from. A 403
// returns (nil, nil) with a warning: a domain manager may not list domains, so
// discovery fails open, mirroring the quota pre-check's 403 policy — the run's
// created-list is the fallback handle, and a domain manager creates no domains
// anyway.
func (c *Client) ListDomainsByPrefix(ctx context.Context, runID string) ([]resource.Resource, error) {
	return c.listDomains(ctx, runPrefix(runID))
}

// ListDomainsByAnyRun returns every domain carrying the ostester- name prefix,
// regardless of run id — the pre-flight sweep's handle for reclaiming a previous
// crashed iteration's domains.
func (c *Client) ListDomainsByAnyRun(ctx context.Context) ([]resource.Resource, error) {
	return c.listDomains(ctx, namePrefix)
}

// listDomains streams the domain list a page at a time and keeps only those
// whose name carries prefix, so cleanup never touches a domain the tool did not
// name — critical in domain-manager mode where the run shares a pre-existing
// domain that carries no prefix.
func (c *Client) listDomains(ctx context.Context, prefix string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, string(KindDomain), "list", func(ctx context.Context) error {
		found = nil
		return domains.List(c.gc, domains.ListOpts{}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
			items, err := domains.ExtractDomains(page)
			if err != nil {
				return false, err
			}
			for _, d := range items {
				if strings.HasPrefix(d.Name, prefix) {
					found = append(found, resource.Resource{Kind: KindDomain, Name: d.Name, ID: d.ID})
				}
			}
			return true, nil
		})
	})
	if err != nil {
		if IsForbidden(err) {
			slog.Warn("listing domains denied (domain manager?); skipping domain discovery", "error", err)
			return nil, nil
		}
		return nil, fmt.Errorf("listing domains by prefix: %w", err)
	}
	return found, nil
}
