package barbican

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/orders"
	"github.com/gophercloud/gophercloud/v2/pagination"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// CreateOrder submits an asynchronous key order for a 256-bit AES key in CBC
// mode. The deterministic name goes into meta.name, which Barbican gives the
// secret it generates and returns in the order list, so both are discoverable
// by the run prefix. The returned resource is the order; WaitForOrder resolves
// the generated secret once the order is ACTIVE. A quota rejection is wrapped
// with ErrQuota so the executor fails fast.
func (c *Client) CreateOrder(ctx context.Context, o barbicanplan.Order) (resource.Resource, error) {
	name := resourceName(c.runID, o.Name)
	opts := orders.CreateOpts{
		Type: orders.KeyOrder,
		Meta: orders.MetaOpts{
			Algorithm:          "aes",
			BitLength:          256,
			Mode:               "cbc",
			Name:               name,
			PayloadContentType: contentTypeOctetStream,
		},
	}

	var id string
	err := c.timed(ctx, KindOrder, "create", func(ctx context.Context) error {
		created, err := orders.Create(ctx, c.kv, opts).Extract()
		if err != nil {
			return err
		}
		id, err = idFromRef(created.OrderRef)
		return err
	})
	if err != nil {
		// A create that fails after the request reached Barbican can leave an order
		// behind. Log the deterministic name so an operator can locate any such
		// orphan; its meta.name still makes it discoverable at cleanup.
		slog.Warn("create failed; an order with this name may be orphaned in the cloud",
			"name", name, "error", err)
		return resource.Resource{}, wrapCreate(KindOrder, o.Name, err)
	}
	return resource.Resource{Kind: KindOrder, Logical: o.Name, Name: name, ID: id}, nil
}

// WaitForOrder polls an order until Barbican has generated its key, recording
// one Readiness sample of kind order. The polls themselves are not recorded as
// operation samples, so their repeated gets do not flood the per-call latency
// stats; the time-to-ready record stands in for them. On ACTIVE it returns the
// generated secret, which shares the order's logical and cloud name.
//
// An order in ERROR, or ACTIVE without a usable secret_ref, is a terminal
// failure: it returns ErrOrderFailed with Barbican's reason and status code and
// records the last poll as one failed order sample with error kind order_error.
// A 404 means the order is gone and aborts the wait. Any other poll error is
// treated as transient and polled through. It returns ctx.Err() if ctx is
// cancelled or its deadline elapses first; the caller decides whether that is
// fatal.
func (c *Client) WaitForOrder(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	start := time.Now()
	backoff := 200 * time.Millisecond
	for {
		pollStart := time.Now()
		o, err := orders.Get(ctx, c.kv, r.ID).Extract()
		poll := time.Since(pollStart)
		switch {
		case err == nil && o.Status == statusActive:
			id, refErr := idFromRef(o.SecretRef)
			if refErr != nil {
				reason := "active without secret_ref"
				if o.SecretRef != "" {
					reason = fmt.Sprintf("active with unusable secret_ref: %v", refErr)
				}
				return resource.Resource{}, c.orderFailed(ctx, r, time.Since(start), poll, reason, o.ErrorStatusCode)
			}
			c.recordReady(ctx, KindOrder, time.Since(start), true)
			return resource.Resource{Kind: KindSecret, Logical: r.Logical, Name: r.Name, ID: id}, nil
		case err == nil && o.Status == statusError:
			return resource.Resource{}, c.orderFailed(ctx, r, time.Since(start), poll, o.ErrorReason, o.ErrorStatusCode)
		case IsNotFound(err):
			c.recordReady(ctx, KindOrder, time.Since(start), false)
			return resource.Resource{}, fmt.Errorf("order %s is gone while waiting for readiness: %w", r.ID, err)
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			c.recordReady(ctx, KindOrder, time.Since(start), false)
			return resource.Resource{}, ctx.Err()
		}

		if backoff = time.Duration(float64(backoff) * 1.5); backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
	}
}

// orderFailed records a terminal order failure: a failed readiness sample over
// the whole wait, and the poll that found the failure as one failed order get
// sample classified order_error. It returns the ErrOrderFailed error the wait
// returns.
func (c *Client) orderFailed(ctx context.Context, r resource.Resource, waited, poll time.Duration, reason, status string) error {
	err := fmt.Errorf("%w: order %s: %s (status %s)", ErrOrderFailed, r.ID, reason, status)
	c.recordReady(ctx, KindOrder, waited, false)
	c.record(ctx, KindOrder, "get", poll, err)
	return err
}

// ListOrdersByRun returns the orders whose meta.name carries this run's
// dizzy-<runID>- prefix, the discovery step cleanup deletes from, and the
// secrets those orders generated. Barbican's order list returns each order's
// meta and secret_ref, so the run's orders and their generated secrets are found
// without the run record. Each order with a non-empty secret_ref contributes one
// secret, named after the order's meta.name, with the id from that secret_ref.
// This reaches a generated secret dizzy never stamped: one whose order turned
// ACTIVE after the order wait gave up, or one of a duplicate order a retried
// create left behind. An order without a secret_ref (PENDING or ERROR)
// contributes none.
func (c *Client) ListOrdersByRun(ctx context.Context, runID string) (found, generated []resource.Resource, err error) {
	found, generated, err = c.listOrders(ctx, runPrefix(runID))
	if err != nil {
		return nil, nil, fmt.Errorf("listing orders by run: %w", err)
	}
	return found, generated, nil
}

// ListOrdersByAnyRun returns every order whose meta.name carries the dizzy-
// name prefix, regardless of run id, and the secrets those orders generated:
// the pre-flight sweep's handle for reclaiming a crashed iteration's orders,
// and through them a generated secret that was never stamped, once the run id
// is lost. An order without a secret_ref contributes no secret.
func (c *Client) ListOrdersByAnyRun(ctx context.Context) (found, generated []resource.Resource, err error) {
	found, generated, err = c.listOrders(ctx, namePrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("listing orders by any run: %w", err)
	}
	return found, generated, nil
}

// listOrders streams the project's order list a page at a time and keeps only
// those whose meta.name carries prefix, along with the secret each kept order's
// secret_ref names, recording the whole listing under the list op. The whole
// paged listing is one request under the request timeout. An order ref or a
// non-empty secret ref without an id stops the listing with an error.
func (c *Client) listOrders(ctx context.Context, prefix string) (found, generated []resource.Resource, err error) {
	err = c.timed(ctx, KindOrder, "list", func(ctx context.Context) error {
		ctx, cancel := c.requestContext(ctx)
		defer cancel()
		found, generated = nil, nil
		return orders.List(c.kv, orders.ListOpts{Limit: listLimit}).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
			items, err := orders.ExtractOrders(page)
			if err != nil {
				return false, err
			}
			for _, o := range items {
				if !strings.HasPrefix(o.Meta.Name, prefix) {
					continue
				}
				id, err := idFromRef(o.OrderRef)
				if err != nil {
					return false, fmt.Errorf("order %q: %w", o.Meta.Name, err)
				}
				found = append(found, resource.Resource{Kind: KindOrder, Name: o.Meta.Name, ID: id})
				if o.SecretRef == "" {
					continue
				}
				secretID, err := idFromRef(o.SecretRef)
				if err != nil {
					return false, fmt.Errorf("order %q secret_ref: %w", o.Meta.Name, err)
				}
				generated = append(generated, resource.Resource{Kind: KindSecret, Name: o.Meta.Name, ID: secretID})
			}
			return true, nil
		})
	})
	return found, generated, err
}
