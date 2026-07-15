// Package executor turns a Glance plan into a real image fleet and drives it
// through its lifecycle. It runs three strictly ordered stages — create each
// image and upload its synthetic payload, drive each image's post-upload
// operation sequence, then delete the images the plan marks for deletion — with
// independent work within a stage running concurrently up to a configurable
// limit, transient failures retried with exponential backoff, quota errors
// failing fast, and per-operation timeouts. Each image's operation sequence
// (metadata churn, sharing with member add/accept/remove, deactivate/reactivate,
// and visibility flips) runs sequentially within its work item, while different
// images' sequences run concurrently. The created resources and the timing the
// Glance wrappers record are the hand-off surface a later run record and cleanup
// consume.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"

	"github.com/B42Labs/dizzy/internal/glance"
	"github.com/B42Labs/dizzy/internal/glance/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// retryBaseDelay, retryMaxDelay, and maxAttempts bound the per-operation retry
// of transient errors. They duplicate the other executors' policy rather than
// sharing it: each package's WithRetry hardcodes its own service's classifiers,
// and this one uses the glance classifiers and backs both the apply path and the
// glance chaos graph.
const (
	retryBaseDelay = 250 * time.Millisecond
	retryMaxDelay  = 5 * time.Second
	maxAttempts    = 5
)

// Glance is the create-drive-and-wait surface the executor drives. It is the
// single ports-and-adapters seam to the cloud: *glance.Client satisfies it in
// production and a fake satisfies it in tests. It is deliberately broad because
// an image run exercises the full image verb set through one client.
type Glance interface {
	CreateImage(ctx context.Context, img plan.Image) (resource.Resource, error)
	UploadImageData(ctx context.Context, r resource.Resource, sizeMiB int, seed int64) error
	ImageOwner(ctx context.Context, r resource.Resource) (string, error)
	AddImageProperties(ctx context.Context, r resource.Resource) error
	ChurnImageProperties(ctx context.Context, r resource.Resource) error
	SetImageVisibility(ctx context.Context, r resource.Resource, v images.ImageVisibility) error
	AddImageMember(ctx context.Context, r resource.Resource, member string) error
	AcceptImageMember(ctx context.Context, r resource.Resource, member string) error
	RemoveImageMember(ctx context.Context, r resource.Resource, member string) error
	DeactivateImage(ctx context.Context, r resource.Resource) error
	ReactivateImage(ctx context.Context, r resource.Resource) error
	Delete(ctx context.Context, r resource.Resource) error
	WaitForReady(ctx context.Context, r resource.Resource) error
	WaitForGone(ctx context.Context, r resource.Resource) error
}

// The production *glance.Client must satisfy the seam.
var _ Glance = (*glance.Client)(nil)

// Result is the outcome of an apply: every image that was created.
type Result struct {
	Created []resource.Resource
}

// Apply creates the plan's images, uploads each image's synthetic payload, drives
// each image through its operation sequence, and deletes the images marked for
// deletion, in three strictly ordered stages. Within a stage independent work
// runs concurrently up to concurrency, each operation retried on transient errors
// and bounded by opTimeout. seed threads the plan seed into the per-image payload
// derivation so uploads are deterministic. The first quota error, or any
// non-retryable error, stops the run and is returned along with the resources
// created so far; ctx cancellation returns ctx.Err().
func Apply(ctx context.Context, c Glance, p *plan.Plan, concurrency int, opTimeout time.Duration, seed int64) (*Result, error) {
	e := &applier{c: c, opTimeout: opTimeout, seed: seed}
	result := &Result{}

	// Stage 1: create each image and upload its payload, waiting for active.
	imgByLogical := make(map[string]resource.Resource, len(p.Images))
	imgRes, err := runStage(ctx, p.Images, concurrency, func(ctx context.Context, img plan.Image) (resource.Resource, error) {
		return e.provisionImage(ctx, img)
	})
	result.Created = appendCreated(result.Created, imgRes)
	for i, img := range p.Images {
		if imgRes[i].ID != "" {
			imgByLogical[img.Name] = imgRes[i]
		}
	}
	if err != nil {
		return result, err
	}

	// Stage 2: each image's post-upload operation sequence. One work item per
	// image; the sequence runs sequentially within the item while different images
	// run concurrently.
	_, err = runStage(ctx, p.Images, concurrency, func(ctx context.Context, img plan.Image) (resource.Resource, error) {
		r, ok := imgByLogical[img.Name]
		if !ok {
			return resource.Resource{}, nil // the create produced no image; nothing to drive
		}
		return resource.Resource{}, e.driveImage(ctx, img, r)
	})
	if err != nil {
		return result, err
	}

	// Stage 3: delete the images the plan marks for deletion.
	toDelete := make([]resource.Resource, 0)
	for _, img := range p.Images {
		if img.Delete {
			if r, ok := imgByLogical[img.Name]; ok {
				toDelete = append(toDelete, r)
			}
		}
	}
	_, err = runStage(ctx, toDelete, concurrency, func(ctx context.Context, r resource.Resource) (resource.Resource, error) {
		return resource.Resource{}, e.deleteImage(ctx, r)
	})
	if err != nil {
		return result, err
	}

	return result, nil
}

// applier carries the apply-wide configuration shared by the stage helpers.
type applier struct {
	c         Glance
	opTimeout time.Duration
	seed      int64
}

// provisionImage creates an image (retrying transient errors), uploads its
// synthetic payload, and waits for it to become active. The active wait is fatal
// on deadline: an image that never reaches active cannot be driven, since Stage 2
// would issue property, visibility, and member operations against an image still
// uploading or killed. Tolerating the deadline would only defer the failure to a
// later op with a misleading cause.
func (e *applier) provisionImage(ctx context.Context, img plan.Image) (resource.Resource, error) {
	var res resource.Resource
	if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return e.c.CreateImage(ctx, img)
	}, &res); err != nil {
		return resource.Resource{}, err
	}
	slog.Info("created resource", "kind", res.Kind, "logical", res.Logical, "id", res.ID)

	if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
		return e.c.UploadImageData(ctx, res, img.SizeMiB, glance.PayloadSeed(e.seed, img.Name))
	}); err != nil {
		return res, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, e.opTimeout)
	defer cancel()
	if err := e.c.WaitForReady(waitCtx, res); err != nil {
		return res, fmt.Errorf("image %s did not become active: %w", res.ID, err)
	}
	return res, nil
}

// driveImage runs one image's post-upload operation sequence, in a fixed order:
// metadata churn (an add pass then a replace/remove pass), then — when shared —
// the visibility flip to shared, a self-share member add and, when planned, its
// accept and remove (kept together while the image is still shared, since a
// member operation requires shared visibility), then the deactivate/reactivate
// cycle, then the community and public visibility flips. Any step's error stops
// the sequence.
func (e *applier) driveImage(ctx context.Context, img plan.Image, r resource.Resource) error {
	if img.MetadataUpdate {
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.AddImageProperties(ctx, r) }); err != nil {
			return err
		}
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.ChurnImageProperties(ctx, r) }); err != nil {
			return err
		}
	}

	if img.Shared {
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
			return e.c.SetImageVisibility(ctx, r, images.ImageVisibilityShared)
		}); err != nil {
			return err
		}
		owner, err := e.imageOwner(ctx, r)
		if err != nil {
			return err
		}
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.AddImageMember(ctx, r, owner) }); err != nil {
			return err
		}
		if img.MemberAccept {
			if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.AcceptImageMember(ctx, r, owner) }); err != nil {
				return err
			}
		}
		if img.MemberRemove {
			if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.RemoveImageMember(ctx, r, owner) }); err != nil {
				return err
			}
		}
	}

	if img.Deactivate {
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.DeactivateImage(ctx, r) }); err != nil {
			return err
		}
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.ReactivateImage(ctx, r) }); err != nil {
			return err
		}
	}

	if img.Community {
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
			return e.c.SetImageVisibility(ctx, r, images.ImageVisibilityCommunity)
		}); err != nil {
			return err
		}
	}

	if img.Public {
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
			return e.c.SetImageVisibility(ctx, r, images.ImageVisibilityPublic)
		}); err != nil {
			return err
		}
	}

	return nil
}

// imageOwner resolves the image's owning project id (the self-share member),
// bounded by opTimeout so a wedged discovery call cannot hang the sequence.
func (e *applier) imageOwner(ctx context.Context, r resource.Resource) (string, error) {
	ownerCtx, cancel := context.WithTimeout(ctx, e.opTimeout)
	defer cancel()
	owner, err := e.c.ImageOwner(ownerCtx, r)
	if err != nil {
		return "", err
	}
	return owner, nil
}

// deleteImage deletes an image and waits for it to be gone, folding an
// already-gone (404) delete into success.
func (e *applier) deleteImage(ctx context.Context, r resource.Resource) error {
	if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.Delete(ctx, r) }); err != nil {
		if glance.IsNotFound(err) {
			return nil
		}
		return err
	}
	slog.Info("deleted resource", "kind", r.Kind, "logical", r.Logical, "id", r.ID)
	goneCtx, cancel := context.WithTimeout(ctx, e.opTimeout)
	defer cancel()
	return e.c.WaitForGone(goneCtx, r)
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

// runStage runs work over items using a fixed pool of at most concurrency workers
// reading from a job channel — a bounded pool rather than one goroutine per item,
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
		if errors.Is(err, glance.ErrQuota) {
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
// error. Backoff sleeps honor the parent context. It is exported so the glance
// chaos graph drives its create/delete/mutate operations through the same
// transient/quota backoff policy the apply path uses.
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
		case errors.Is(err, glance.ErrQuota):
			return err
		case !glance.IsRetryable(err):
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
