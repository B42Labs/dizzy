// Package executor turns a Barbican plan into real secrets, key orders, and
// generic containers and checks that what it stored comes back intact. It runs
// four strictly ordered stages: store each secret with its synthetic payload and
// stamp the run identity onto it, submit each key order and wait for Barbican to
// generate its key before stamping the generated secret, group the stored
// secrets into containers, then read back the payloads the plan selects and
// byte-compare each with the bytes it was stored with. Independent work within a
// stage runs concurrently up to a configurable limit; transient failures are
// retried with exponential backoff, quota errors fail fast, and every operation
// has its own timeout. A payload mismatch or an order that ends in ERROR is
// never retried, so either fails the run, as a failed token issue fails a
// Keystone run. The created resources and the timing the Barbican wrappers
// record are the hand-off surface a later run record and cleanup consume.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/B42Labs/dizzy/internal/barbican"
	"github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/payload"
	"github.com/B42Labs/dizzy/internal/resource"
)

// retryBaseDelay, retryMaxDelay, and maxAttempts bound the per-operation retry
// of transient errors. They duplicate the other executors' policy rather than
// sharing it: each package's WithRetry hardcodes its own service's classifiers,
// and this one uses the barbican classifiers, under which a payload mismatch and
// a failed order are never retried.
const (
	retryBaseDelay = 250 * time.Millisecond
	retryMaxDelay  = 5 * time.Second
	maxAttempts    = 5
)

// Barbican is the store, order, group, and read-back surface the executor
// drives. It is the single ports-and-adapters seam to the cloud:
// *barbican.Client satisfies it in production and a fake satisfies it in tests.
type Barbican interface {
	StoreSecret(ctx context.Context, s plan.Secret, seed int64) (resource.Resource, error)
	StampSecret(ctx context.Context, r resource.Resource) error
	CreateOrder(ctx context.Context, o plan.Order) (resource.Resource, error)
	WaitForOrder(ctx context.Context, r resource.Resource) (resource.Resource, error)
	CreateContainer(ctx context.Context, ct plan.Container, stored map[string]resource.Resource) (resource.Resource, error)
	RetrievePayload(ctx context.Context, r resource.Resource, seed int64, n int) error
}

// The production *barbican.Client must satisfy the seam.
var _ Barbican = (*barbican.Client)(nil)

// Result is the outcome of an apply: every resource that was created, in stage
// order. The stored secrets come first in plan order, then each order followed
// by the secret Barbican generated for it, then the containers. Read-backs
// create no resource and never appear here.
type Result struct {
	Created []resource.Resource
}

// Apply stores the plan's secrets, submits its key orders, creates its
// containers, and reads back the payloads it marks for retrieval, in four
// strictly ordered stages. Within a stage independent work runs concurrently up
// to concurrency, each operation retried on transient errors and bounded by
// opTimeout. seed threads the plan seed into the per-secret payload derivation,
// so a store and its read-back use the same bytes and two runs of the same plan
// store byte-identical payloads. The first quota error, or any non-retryable
// error, stops the run and is returned along with the resources created so far;
// ctx cancellation returns ctx.Err().
func Apply(ctx context.Context, c Barbican, p *plan.Plan, concurrency int, opTimeout time.Duration, seed int64) (*Result, error) {
	e := &applier{c: c, opTimeout: opTimeout, seed: seed}
	result := &Result{}

	// Stage 1: store each secret and stamp the run identity onto it. The stored
	// secrets are indexed by logical name for the containers and read-backs.
	storedRes, err := runStage(ctx, p.Secrets, concurrency, func(ctx context.Context, s plan.Secret) (resource.Resource, error) {
		return e.storeSecret(ctx, s)
	})
	result.Created = appendCreated(result.Created, storedRes)
	stored := make(map[string]resource.Resource, len(p.Secrets))
	for i, r := range storedRes {
		if r.ID != "" {
			stored[p.Secrets[i].Name] = r
		}
	}
	if err != nil {
		return result, err
	}

	// Stage 2: submit each key order, wait for its key, and stamp the generated
	// secret. One work item per order returns both the order and its secret.
	orderRes, err := runStage(ctx, p.Orders, concurrency, func(ctx context.Context, o plan.Order) (orderResult, error) {
		return e.provisionOrder(ctx, o)
	})
	result.Created = appendOrders(result.Created, orderRes)
	if err != nil {
		return result, err
	}

	// Stage 3: group the stored secrets into containers.
	ctrRes, err := runStage(ctx, p.Containers, concurrency, func(ctx context.Context, ct plan.Container) (resource.Resource, error) {
		return e.createContainer(ctx, ct, stored)
	})
	result.Created = appendCreated(result.Created, ctrRes)
	if err != nil {
		return result, err
	}

	// Stage 4: read back the payloads the plan selects and byte-compare each with
	// the bytes it was stored with.
	toRead := make([]plan.Secret, 0)
	for _, s := range p.Secrets {
		if s.Retrieve {
			toRead = append(toRead, s)
		}
	}
	_, err = runStage(ctx, toRead, concurrency, func(ctx context.Context, s plan.Secret) (resource.Resource, error) {
		r, ok := stored[s.Name]
		if !ok {
			return resource.Resource{}, nil // the store produced no secret; nothing to read back
		}
		return resource.Resource{}, e.readBack(ctx, s, r)
	})
	if err != nil {
		return result, err
	}

	return result, nil
}

// applier carries the apply-wide configuration shared by the stage helpers.
type applier struct {
	c         Barbican
	opTimeout time.Duration
	seed      int64
}

// orderResult is the outcome of one stage-2 work item: the order and the secret
// Barbican generated for it. Each stays the zero Resource until it exists, so a
// work item that fails partway still hands back what was created.
type orderResult struct {
	order  resource.Resource
	secret resource.Resource
}

// storeSecret stores a secret (retrying transient errors) with the payload
// derived from the plan seed and the secret's logical name, then stamps the run
// identity onto it. A stamp failure still returns the stored secret with the
// error, so the run record keeps it and cleanup reclaims it even though
// discovery cannot confirm a secret without its stamp.
func (e *applier) storeSecret(ctx context.Context, s plan.Secret) (resource.Resource, error) {
	var res resource.Resource
	if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return e.c.StoreSecret(ctx, s, payload.Seed(e.seed, s.Name))
	}, &res); err != nil {
		return resource.Resource{}, err
	}
	logCreated(res)

	if err := e.stamp(ctx, res); err != nil {
		return res, err
	}
	return res, nil
}

// provisionOrder submits a key order (retrying transient errors), waits for
// Barbican to generate its key, and stamps the run identity onto the generated
// secret. The wait is bounded by opTimeout and fatal on deadline, as Glance's
// active wait is: an order still pending at the deadline has generated no
// secret, so there is nothing to stamp, and tolerating it would report a run
// that never produced the key as a success. The result carries the order as
// soon as it exists and the generated secret as soon as the wait names it, so a
// failed wait or stamp still leaves both reclaimable.
func (e *applier) provisionOrder(ctx context.Context, o plan.Order) (orderResult, error) {
	var out orderResult
	if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return e.c.CreateOrder(ctx, o)
	}, &out.order); err != nil {
		return orderResult{}, err
	}
	logCreated(out.order)

	waitCtx, cancel := context.WithTimeout(ctx, e.opTimeout)
	defer cancel()
	generated, err := e.c.WaitForOrder(waitCtx, out.order)
	if err != nil {
		return out, fmt.Errorf("order %s did not become active: %w", out.order.ID, err)
	}
	out.secret = generated
	logCreated(generated)

	if err := e.stamp(ctx, generated); err != nil {
		return out, err
	}
	return out, nil
}

// createContainer creates a generic container (retrying transient errors) that
// references the stored secrets ct names, resolved through stored, the stage-1
// map from logical name to stored secret.
func (e *applier) createContainer(ctx context.Context, ct plan.Container, stored map[string]resource.Resource) (resource.Resource, error) {
	var res resource.Resource
	if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return e.c.CreateContainer(ctx, ct, stored)
	}, &res); err != nil {
		return resource.Resource{}, err
	}
	logCreated(res)
	return res, nil
}

// stamp writes the run identity onto a stored or generated secret, retrying
// transient errors. The stamp replaces the secret's whole metadata set, so a
// retry is safe.
func (e *applier) stamp(ctx context.Context, r resource.Resource) error {
	return WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.StampSecret(ctx, r) })
}

// readBack reads a stored secret's payload back and byte-compares it with the
// bytes derived from the same seed and size the store used. A transient error is
// retried; a mismatch is not, so it fails the stage and the run.
func (e *applier) readBack(ctx context.Context, s plan.Secret, r resource.Resource) error {
	return WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
		return e.c.RetrievePayload(ctx, r, payload.Seed(e.seed, s.Name), s.PayloadBytes)
	})
}

// logCreated logs one created resource, the per-resource progress line every
// stage emits.
func logCreated(r resource.Resource) {
	slog.Info("created resource", "kind", r.Kind, "logical", r.Logical, "id", r.ID)
}

// withCreate runs a create through the retry policy, storing the created
// resource into out on success.
func withCreate(ctx context.Context, opTimeout time.Duration, create func(context.Context) (resource.Resource, error), out *resource.Resource) error {
	return WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		r, err := create(ctx)
		if err != nil {
			return err
		}
		*out = r
		return nil
	})
}

// appendCreated appends the populated resources from a stage to dst, skipping
// the zero Resource{} slots a partially-failed stage leaves for items that
// failed or were never dispatched (identified by an empty ID). It keeps the run
// record honest about what actually exists when a stage fails partway.
func appendCreated(dst, stageRes []resource.Resource) []resource.Resource {
	for _, r := range stageRes {
		if r.ID != "" {
			dst = append(dst, r)
		}
	}
	return dst
}

// appendOrders appends each order followed by the secret it generated to dst,
// in plan order, skipping the slots appendCreated skips: an order whose create
// failed or was never dispatched, and a secret the wait never named.
func appendOrders(dst []resource.Resource, stageRes []orderResult) []resource.Resource {
	for _, o := range stageRes {
		dst = appendCreated(dst, []resource.Resource{o.order, o.secret})
	}
	return dst
}

// runStage runs work over items using a fixed pool of at most concurrency workers
// reading from a job channel: a bounded pool rather than one goroutine per item,
// so a large plan cannot exhaust resources. Results are returned in item order
// (populated even for a failing item, so a caller can record what already
// exists). The first error cancels the stage, stops dispatching, and is returned,
// with a quota error taking priority and a parent-context cancellation reported
// as ctx.Err().
func runStage[T, R any](ctx context.Context, items []T, concurrency int, work func(context.Context, T) (R, error)) ([]R, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stageCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := concurrency
	if workers < 1 {
		workers = 1
	}
	if workers > len(items) {
		workers = len(items)
	}

	results := make([]R, len(items))
	errs := make([]error, len(items))
	jobs := make(chan int)

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				res, err := work(stageCtx, items[i])
				results[i] = res
				if err != nil {
					errs[i] = err
					cancel()
				}
			}
		}()
	}

dispatch:
	for i := range items {
		select {
		case jobs <- i:
		case <-stageCtx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()

	for _, err := range errs {
		if errors.Is(err, barbican.ErrQuota) {
			return results, err
		}
	}
	if err := ctx.Err(); err != nil {
		return results, err
	}
	// Prefer a real failure over the context.Canceled that in-flight siblings
	// return once the first error triggers cancel(); otherwise the root cause is
	// masked by an arbitrary cancelled sibling.
	var canceled error
	for _, err := range errs {
		switch {
		case err == nil:
			continue
		case errors.Is(err, context.Canceled):
			canceled = err
		default:
			return results, err
		}
	}
	if canceled != nil {
		return results, canceled
	}
	return results, nil
}

// WithRetry runs fn, bounding each attempt with opTimeout, and retries transient
// errors with exponential backoff up to maxAttempts. It returns immediately on
// success, on a quota error (so the run fails fast), or on any non-retryable
// error, which includes a payload mismatch and a failed order. Backoff sleeps
// honor the parent context. It is exported, as the other executors' WithRetry
// is, so a caller outside the apply path can drive Barbican calls through the
// same transient/quota backoff policy.
func WithRetry(ctx context.Context, opTimeout time.Duration, fn func(context.Context) error) error {
	backoff := retryBaseDelay
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		opCtx, cancel := context.WithTimeout(ctx, opTimeout)
		err = fn(opCtx)
		cancel()

		switch {
		case err == nil:
			return nil
		case errors.Is(err, barbican.ErrQuota):
			return err
		case !barbican.IsRetryable(err):
			return err
		case attempt == maxAttempts:
			return err
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff *= 2; backoff > retryMaxDelay {
			backoff = retryMaxDelay
		}
	}
	return err
}
