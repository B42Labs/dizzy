package barbican

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/containers"
	"github.com/gophercloud/gophercloud/v2/pagination"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// CreateContainer creates a generic container with the deterministic name that
// references the stored secrets ct names, in ct.Secrets order. Each reference
// carries the secret's logical name and its href, resolved through stored, the
// map from logical name to stored secret. A logical name missing from stored is
// refused without a request. A quota rejection is wrapped with ErrQuota so the
// executor fails fast.
func (c *Client) CreateContainer(ctx context.Context, ct barbicanplan.Container, stored map[string]resource.Resource) (resource.Resource, error) {
	refs := make([]containers.SecretRef, 0, len(ct.Secrets))
	for _, logical := range ct.Secrets {
		s, ok := stored[logical]
		if !ok || s.ID == "" {
			return resource.Resource{}, fmt.Errorf("container %q references secret %q that was not stored", ct.Name, logical)
		}
		refs = append(refs, containers.SecretRef{Name: logical, SecretRef: c.secretRef(s.ID)})
	}

	name := resourceName(c.runID, ct.Name)
	opts := containers.CreateOpts{
		Type:       containers.GenericContainer,
		Name:       name,
		SecretRefs: refs,
	}

	var id string
	err := c.timed(ctx, KindContainer, "create", func(ctx context.Context) error {
		created, err := containers.Create(ctx, c.kv, opts).Extract()
		if err != nil {
			return err
		}
		id, err = idFromRef(created.ContainerRef)
		return err
	})
	if err != nil {
		// A create that fails after the request reached Barbican can leave a
		// container behind. Log the deterministic name so an operator can locate any
		// such orphan; its name prefix still makes it discoverable at cleanup.
		slog.Warn("create failed; a container with this name may be orphaned in the cloud",
			"name", name, "error", err)
		return resource.Resource{}, wrapCreate(KindContainer, ct.Name, err)
	}
	return resource.Resource{Kind: KindContainer, Logical: ct.Name, Name: name, ID: id}, nil
}

// ListContainersByRun returns the containers whose name carries this run's
// dizzy-<runID>- prefix, the discovery step cleanup deletes from.
func (c *Client) ListContainersByRun(ctx context.Context, runID string) ([]resource.Resource, error) {
	found, err := c.listContainers(ctx, runPrefix(runID))
	if err != nil {
		return nil, fmt.Errorf("listing containers by run: %w", err)
	}
	return found, nil
}

// listContainers streams the project's container list a page at a time and
// keeps only those whose name carries prefix, recording the whole listing under
// the list op. The whole paged listing is one request under the request
// timeout.
func (c *Client) listContainers(ctx context.Context, prefix string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, KindContainer, "list", func(ctx context.Context) error {
		ctx, cancel := c.requestContext(ctx)
		defer cancel()
		found = nil
		return containers.List(c.kv, containers.ListOpts{Limit: listLimit}).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
			items, err := containers.ExtractContainers(page)
			if err != nil {
				return false, err
			}
			for _, ct := range items {
				if !strings.HasPrefix(ct.Name, prefix) {
					continue
				}
				id, err := idFromRef(ct.ContainerRef)
				if err != nil {
					return false, fmt.Errorf("container %q: %w", ct.Name, err)
				}
				found = append(found, resource.Resource{Kind: KindContainer, Name: ct.Name, ID: id})
			}
			return true, nil
		})
	})
	return found, err
}
