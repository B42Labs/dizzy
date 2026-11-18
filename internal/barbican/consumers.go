package barbican

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/containers"
	"github.com/gophercloud/gophercloud/v2/pagination"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// consumerURLBase prefixes the URL of every consumer dizzy registers. Barbican
// requires a URL and never contacts it, and .invalid is a reserved top-level
// domain, so no consumer URL names a real service.
const consumerURLBase = "https://dizzy.invalid/consumers/"

// consumerURL returns the URL of the consumer with cloud name name.
func consumerURL(name string) string {
	return consumerURLBase + name
}

// consumerID builds the id a consumer is recorded under. Barbican addresses a
// consumer only through its container, by name and URL, so the id joins the
// container's id and the consumer's cloud name; the URL follows from the name.
func consumerID(containerID, name string) string {
	return containerID + ":" + name
}

// parseConsumerID splits an id consumerID built at its first colon into the
// container's id and the consumer's cloud name.
func parseConsumerID(id string) (containerID, name string, err error) {
	containerID, name, ok := strings.Cut(id, ":")
	if !ok || containerID == "" || name == "" {
		return "", "", fmt.Errorf("malformed consumer id %q", id)
	}
	return containerID, name, nil
}

// RegisterConsumer registers con on the container the plan names, with the
// deterministic name and the URL derived from it, as one POST
// /v1/containers/<id>/consumers timed as consumer/create. Barbican updates an
// existing consumer of the same container, name, and URL in place, so a retried
// registration is safe. The returned resource carries the cloud name and the id
// <containerID>:<name>, the handle Delete and Observe address the consumer by.
// A container without an id is refused without a request. A quota rejection is
// wrapped with ErrQuota so the executor fails fast.
func (c *Client) RegisterConsumer(ctx context.Context, con barbicanplan.Consumer, container resource.Resource) (resource.Resource, error) {
	if container.ID == "" {
		return resource.Resource{}, fmt.Errorf("consumer %q targets container %q that was not created", con.Name, con.Container)
	}
	name := resourceName(c.runID, con.Name)
	opts := containers.CreateConsumerOpts{Name: name, URL: consumerURL(name)}
	err := c.timed(ctx, KindConsumer, "create", func(ctx context.Context) error {
		return containers.CreateConsumer(ctx, c.kv, container.ID, opts).Err
	})
	if err != nil {
		return resource.Resource{}, wrapCreate(KindConsumer, con.Name, err)
	}
	return resource.Resource{Kind: KindConsumer, Logical: con.Name, Name: name, ID: consumerID(container.ID, name)}, nil
}

// consumerKey identifies a listed or wanted consumer by what Barbican keys it
// on within its container: its name and its URL.
type consumerKey struct{ name, url string }

// VerifyConsumers lists the consumers of container and checks that every
// consumer in want is listed with the name and URL dizzy registered it with,
// inside one call timed as consumer/list, so a difference fails that sample
// with error kind mismatch. With exact, a listed consumer outside want is a
// mismatch too. The first wanted consumer the list lacks, in want order, and
// then, with exact, the first unexpected one it lists, return
// ErrConsumerMismatch; any other error returns wrapped with the container's id.
// An empty want without exact checks nothing and sends no request.
func (c *Client) VerifyConsumers(ctx context.Context, container resource.Resource, want []resource.Resource, exact bool) error {
	if len(want) == 0 && !exact {
		return nil
	}
	err := c.timed(ctx, KindConsumer, "list", func(ctx context.Context) error {
		listed, err := c.listConsumers(ctx, container.ID)
		if err != nil {
			return err
		}
		have := make(map[consumerKey]bool, len(listed))
		for _, l := range listed {
			have[consumerKey{l.Name, l.URL}] = true
		}
		wanted := make(map[consumerKey]bool, len(want))
		for _, w := range want {
			k := consumerKey{w.Name, consumerURL(w.Name)}
			if !have[k] {
				return fmt.Errorf("%w: container %s lacks consumer %q", ErrConsumerMismatch, container.ID, w.Name)
			}
			wanted[k] = true
		}
		if exact {
			for _, l := range listed {
				if !wanted[consumerKey{l.Name, l.URL}] {
					return fmt.Errorf("%w: container %s lists unexpected consumer %q", ErrConsumerMismatch, container.ID, l.Name)
				}
			}
		}
		return nil
	})
	if err == nil || errors.Is(err, ErrConsumerMismatch) {
		return err
	}
	return fmt.Errorf("listing consumers of container %s: %w", container.ID, err)
}

// listConsumers pages through the consumers of the container with containerID,
// listLimit at a time. It records nothing; its callers time it.
func (c *Client) listConsumers(ctx context.Context, containerID string) ([]containers.Consumer, error) {
	var all []containers.Consumer
	err := containers.ListConsumers(c.kv, containerID, containers.ListOpts{Limit: listLimit}).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		items, err := containers.ExtractConsumers(page)
		if err != nil {
			return false, err
		}
		all = append(all, items...)
		return true, nil
	})
	return all, err
}
