package octavia

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/gophercloud/gophercloud/v2/pagination"

	"github.com/B42Labs/dizzy/internal/resource"
)

// addPending records that a create of kind named name lost its answer and a
// lookup did not find its object, so the next create of that name looks for
// it before it posts again.
func (c *Client) addPending(kind resource.Kind, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending[string(kind)+"/"+name] = struct{}{}
}

// takePending reports whether a create of kind named name is pending and
// deletes the entry, so the name is looked up at most once more unless the
// caller puts it back with addPending.
func (c *Client) takePending(kind resource.Kind, name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := string(kind) + "/" + name
	_, ok := c.pending[key]
	delete(c.pending, key)
	return ok
}

// createAdopting creates the object of kind with the logical name logical and
// returns its Resource and the object create or lookup returned. Octavia
// refuses a second listener on a port, a second default pool on a listener, a
// second member at an address and port, and a second monitor on a pool with
// 409, and a second load balancer create boots a second amphora, so a create
// whose answer was lost is adopted rather than posted again. In one apply a
// logical name is created once, so the cloud name and the run tag identify the
// object; lookup finds it by both and reports whether it did.
//
//  1. When an earlier create of the name is pending, lookup runs first. A
//     lookup error puts the name back pending and fails without posting, since
//     posting now could collide with an object Octavia committed; a 5xx from
//     the list stays retryable. A match is adopted.
//  2. Otherwise create posts the object.
//  3. When the post fails in a way that may have landed (see mayHaveLanded),
//     lookup runs once while ctx still allows, and a match is adopted. Without
//     a match, after a lookup error, or when ctx is already done, the name goes
//     pending for the next create.
//
// Every lookup is timed under <kind>/list and every post under <kind>/create. A
// failed post returns the post's error, not the lookup's, wrapped by
// wrapCreate.
func createAdopting[T any](ctx context.Context, c *Client, kind resource.Kind, logical string,
	lookup func(ctx context.Context, name string) (T, bool, error),
	create func(ctx context.Context, name string) (T, error),
	id func(T) string) (resource.Resource, T, error) {
	name := resourceName(c.runID, logical)
	var zero T

	// find runs lookup timed under <kind>/list.
	find := func(ctx context.Context) (T, bool, error) {
		var (
			found T
			ok    bool
		)
		err := c.timed(ctx, string(kind), "list", func(ctx context.Context) error {
			var lookupErr error
			found, ok, lookupErr = lookup(ctx, name)
			return lookupErr
		})
		return found, ok, err
	}
	// adopt returns the Resource of found, an object of an earlier create whose
	// answer was lost.
	adopt := func(found T) resource.Resource {
		r := resource.Resource{Kind: kind, Logical: logical, Name: name, ID: id(found)}
		slog.Info("adopted an octavia resource whose create answer was lost", "kind", kind, "name", name, "id", r.ID)
		return r
	}

	if c.takePending(kind, name) {
		found, ok, err := find(ctx)
		if err != nil {
			c.addPending(kind, name)
			return resource.Resource{}, zero, wrapCreate(kind, logical,
				fmt.Errorf("looking up an earlier create whose answer was lost: %w", err))
		}
		if ok {
			return adopt(found), found, nil
		}
	}

	var created T
	err := c.timed(ctx, string(kind), "create", func(ctx context.Context) error {
		var createErr error
		created, createErr = create(ctx, name)
		return createErr
	})
	if err != nil {
		// A create that fails after the request reached Octavia (a lost response
		// or a 5xx past commit) can leave the object behind. Look it up by name
		// and run tag while the context allows, and adopt it when Octavia has it.
		// Otherwise keep the name pending for the next create, and log the
		// deterministic name so an operator can locate any such orphan; its run
		// tag still makes it reachable through its load balancer's cascade.
		var lookupErr error
		if mayHaveLanded(err) {
			if ctx.Err() == nil {
				var (
					found T
					ok    bool
				)
				found, ok, lookupErr = find(ctx)
				if lookupErr == nil && ok {
					return adopt(found), found, nil
				}
			}
			c.addPending(kind, name)
		}
		attrs := []any{"kind", kind, "name", name, "error", err}
		if lookupErr != nil {
			attrs = append(attrs, "lookup", lookupErr)
		}
		slog.Warn("create failed; an octavia resource with this name may be orphaned in the cloud", attrs...)
		// The POST's error, not the lookup's, so errKind, IsRetryable and the
		// executor see the failure they always saw.
		return resource.Resource{}, zero, wrapCreate(kind, logical, err)
	}
	return resource.Resource{Kind: kind, Logical: logical, Name: name, ID: id(created)}, created, nil
}

// lookupLost runs the lookup of an earlier create of kind named name whose
// answer was lost: it runs pager, which lists by name, parent, and (where the
// API takes it) the run tag tag, to completion and returns the first item
// pickLost keeps, and whether there was one.
func lookupLost[T any](ctx context.Context, kind resource.Kind, name, tag string,
	pager pagination.Pager,
	extract func(pagination.Page) ([]T, error),
	describe func(T) (name, id string, tags []string, status string)) (T, bool, error) {
	var zero T
	pages, err := pager.AllPages(ctx)
	if err != nil {
		return zero, false, err
	}
	items, err := extract(pages)
	if err != nil {
		return zero, false, err
	}
	found, ok := pickLost(kind, name, tag, items, describe)
	return found, ok, nil
}

// pickLost returns the first of items that is the object of an earlier create
// of kind named name: one with that name, carrying the run tag tag, and
// neither PENDING_DELETE nor DELETED. describe pulls an item's name, id, tags,
// and provisioning status. The run-tag check also stands in for a server-side
// tag filter, which the member list does not have. More than one match is
// logged with the ids of the others, which stay in the cloud until cleanup's
// cascade deletes them with their load balancer.
func pickLost[T any](kind resource.Kind, name, tag string, items []T,
	describe func(T) (name, id string, tags []string, status string)) (T, bool) {
	var matches []T
	var ids []string
	for _, it := range items {
		itName, itID, tags, status := describe(it)
		if itName != name || !slices.Contains(tags, tag) || status == statusPendingDelete || status == statusDeleted {
			continue
		}
		matches = append(matches, it)
		ids = append(ids, itID)
	}
	if len(matches) == 0 {
		var zero T
		return zero, false
	}
	if len(matches) > 1 {
		slog.Warn("more octavia resources of an earlier create whose answer was lost; cleanup's cascade deletes them",
			"kind", kind, "name", name, "ids", ids[1:])
	}
	return matches[0], true
}
