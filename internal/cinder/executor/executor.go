// Package executor turns a Cinder plan into real volumes and snapshots. It runs
// three strictly ordered stages — create volumes, extend the volumes with a
// resize target, then snapshot them — with independent work within a stage run
// concurrently up to a configurable limit, transient failures retried with
// exponential backoff, quota errors failing fast, and per-operation timeouts.
// Snapshots of the same volume are serialized while snapshots of different
// volumes run concurrently. The created resources and the timing the Cinder
// wrappers record are the hand-off surface a later run record and cleanup
// consume.
package executor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/B42Labs/openstack-tester/internal/cinder"
	"github.com/B42Labs/openstack-tester/internal/cinder/plan"
	"github.com/B42Labs/openstack-tester/internal/resource"
)

// retryBaseDelay, retryMaxDelay, and maxAttempts bound the per-operation retry
// of transient errors. They duplicate the internal/executor policy rather than
// sharing it: executor.WithRetry hardcodes the neutron classifiers and is
// exported for the chaos engine, so a cross-service refactor would touch call
// sites this slice does not need to.
const (
	retryBaseDelay = 250 * time.Millisecond
	retryMaxDelay  = 5 * time.Second
	maxAttempts    = 5
)

// Cinder is the create-and-wait surface the executor drives. It is the single
// ports-and-adapters seam to the cloud: *cinder.Client satisfies it in
// production and a fake satisfies it in tests.
type Cinder interface {
	CreateVolume(ctx context.Context, v plan.Volume, volumeType string) (resource.Resource, error)
	ExtendVolume(ctx context.Context, r resource.Resource, newSizeGiB int) error
	CreateSnapshot(ctx context.Context, s plan.Snapshot, volumeID string) (resource.Resource, error)
	WaitForReady(ctx context.Context, r resource.Resource) error
}

// Result is the outcome of an apply: every resource that was created, in
// dependency order (volumes then snapshots).
type Result struct {
	Created []resource.Resource
}

// Apply creates every volume in p against c, extends the volumes with a resize
// target, and snapshots them, in three strictly ordered stages. Within a stage
// independent work runs concurrently up to concurrency, each operation retried
// on transient errors and bounded by opTimeout. volumeType is applied to every
// volume create ("" means the cloud's default type). The first quota error, or
// any non-retryable error, stops the run and is returned along with the
// resources created so far; ctx cancellation returns ctx.Err().
func Apply(ctx context.Context, c Cinder, p *plan.Plan, concurrency int, opTimeout time.Duration, volumeType string) (*Result, error) {
	e := &applier{c: c, opTimeout: opTimeout}
	result := &Result{}

	// Stage 1: create every volume and poll it to available. A volume that
	// reaches a terminal error status fails the stage; a readiness deadline while
	// the run is still live is warned and tolerated.
	volRes, err := runStage(ctx, p.Volumes, concurrency, func(ctx context.Context, v plan.Volume) (resource.Resource, error) {
		return e.provisionVolume(ctx, v, volumeType)
	})
	result.Created = appendCreated(result.Created, volRes)
	// resByLogical maps a volume's logical name to its created resource, built
	// between stages (single-threaded) and read concurrently within the next
	// stage, so no lock is needed.
	resByLogical := make(map[string]resource.Resource, len(p.Volumes))
	for i, v := range p.Volumes {
		if volRes[i].ID != "" {
			resByLogical[v.Name] = volRes[i]
		}
	}
	if err != nil {
		return result, err
	}

	// Stage 2: extend only the volumes with a resize target, then re-poll each to
	// available. Runs strictly after stage 1 so only available volumes are
	// extended. Extends create no Resource.
	toExtend := make([]plan.Volume, 0)
	for _, v := range p.Volumes {
		if v.ResizeToGiB > 0 {
			toExtend = append(toExtend, v)
		}
	}
	_, err = runStage(ctx, toExtend, concurrency, func(ctx context.Context, v plan.Volume) (resource.Resource, error) {
		return resource.Resource{}, e.extendVolume(ctx, resByLogical[v.Name], v.ResizeToGiB)
	})
	if err != nil {
		return result, err
	}

	// Stage 3: snapshot the volumes. Snapshots are grouped by source volume in
	// first-appearance order; each group is one work item whose snapshots are
	// created strictly sequentially, while groups run concurrently up to
	// concurrency. Runs after stage 2 so a snapshot reflects its volume's final
	// size.
	groups := groupSnapshots(p.Snapshots)
	groupRes, err := runStage(ctx, groups, concurrency, func(ctx context.Context, g snapshotGroup) ([]resource.Resource, error) {
		return e.provisionSnapshots(ctx, g, resByLogical[g.volume].ID)
	})
	for _, rs := range groupRes {
		result.Created = appendCreated(result.Created, rs)
	}
	if err != nil {
		return result, err
	}

	return result, nil
}

// applier carries the apply-wide configuration shared by the stage helpers.
type applier struct {
	c         Cinder
	opTimeout time.Duration
}

// provisionVolume creates a volume (retrying transient errors) and waits for it
// to become available. It returns the created resource even when the readiness
// wait fails, so a volume that exists but errored is still recorded for cleanup.
func (e *applier) provisionVolume(ctx context.Context, v plan.Volume, volumeType string) (resource.Resource, error) {
	var res resource.Resource
	err := withRetry(ctx, e.opTimeout, func(ctx context.Context) error {
		r, err := e.c.CreateVolume(ctx, v, volumeType)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return resource.Resource{}, err
	}
	slog.Info("created resource", "kind", res.Kind, "logical", res.Logical, "id", res.ID)

	if err := e.waitReady(ctx, res); err != nil {
		return res, err
	}
	return res, nil
}

// extendVolume grows a volume to its resize target (retrying transient errors)
// and re-waits for it to return to available.
func (e *applier) extendVolume(ctx context.Context, r resource.Resource, newSizeGiB int) error {
	if err := withRetry(ctx, e.opTimeout, func(ctx context.Context) error {
		return e.c.ExtendVolume(ctx, r, newSizeGiB)
	}); err != nil {
		return err
	}
	slog.Info("extended volume", "logical", r.Logical, "id", r.ID, "newSizeGiB", newSizeGiB)
	return e.waitReady(ctx, r)
}

// provisionSnapshots creates every snapshot of one volume strictly
// sequentially: each is created (retrying transient errors) and polled to
// available before the next begins, since some backends reject a snapshot while
// the source volume is still snapshotting. It returns the snapshots created so
// far even when one fails, so the run record does not under-report them.
func (e *applier) provisionSnapshots(ctx context.Context, g snapshotGroup, volumeID string) ([]resource.Resource, error) {
	created := make([]resource.Resource, 0, len(g.snaps))
	for _, s := range g.snaps {
		var res resource.Resource
		err := withRetry(ctx, e.opTimeout, func(ctx context.Context) error {
			r, err := e.c.CreateSnapshot(ctx, s, volumeID)
			if err != nil {
				return err
			}
			res = r
			return nil
		})
		if err != nil {
			return created, err
		}
		slog.Info("created resource", "kind", res.Kind, "logical", res.Logical, "id", res.ID)
		if err := e.waitReady(ctx, res); err != nil {
			return append(created, res), err
		}
		created = append(created, res)
	}
	return created, nil
}

// waitReady bounds a readiness poll with opTimeout. A readiness deadline that
// elapses while the run is still live is warned and tolerated (nil); a parent
// cancellation returns ctx.Err(); a terminal backend error status is returned so
// the stage fails.
func (e *applier) waitReady(ctx context.Context, res resource.Resource) error {
	readyCtx, cancel := context.WithTimeout(ctx, e.opTimeout)
	defer cancel()
	err := e.c.WaitForReady(readyCtx, res)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("resource did not reach ready state before deadline",
			"kind", res.Kind, "id", res.ID, "logical", res.Logical, "error", err)
		return nil
	}
	return err
}

// snapshotGroup is the snapshots of one source volume, kept together so they can
// be created strictly sequentially while other volumes' groups run in parallel.
type snapshotGroup struct {
	volume string
	snaps  []plan.Snapshot
}

// groupSnapshots buckets snapshots by their source volume, preserving the order
// volumes first appear so the work list is deterministic.
func groupSnapshots(snaps []plan.Snapshot) []snapshotGroup {
	order := make([]string, 0)
	byVolume := make(map[string][]plan.Snapshot)
	for _, s := range snaps {
		if _, ok := byVolume[s.Volume]; !ok {
			order = append(order, s.Volume)
		}
		byVolume[s.Volume] = append(byVolume[s.Volume], s)
	}
	groups := make([]snapshotGroup, 0, len(order))
	for _, v := range order {
		groups = append(groups, snapshotGroup{volume: v, snaps: byVolume[v]})
	}
	return groups
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

// runStage runs work over items using a fixed pool of at most concurrency
// workers reading from a job channel — a bounded pool rather than one goroutine
// per item, so a large plan cannot exhaust resources. Results are returned in
// item order (populated even for a failing item, so a caller can record what
// already exists). The first error cancels the stage, stops dispatching, and is
// returned, with a quota error taking priority and a parent-context
// cancellation reported as ctx.Err().
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
		if errors.Is(err, cinder.ErrQuota) {
			return results, err
		}
	}
	if err := ctx.Err(); err != nil {
		return results, err
	}
	for _, err := range errs {
		if err != nil {
			return results, err
		}
	}
	return results, nil
}

// withRetry runs fn, bounding each attempt with opTimeout, and retries transient
// errors with exponential backoff up to maxAttempts. It returns immediately on
// success, on a quota error (so the run fails fast), or on any non-retryable
// error. Backoff sleeps honor the parent context.
func withRetry(ctx context.Context, opTimeout time.Duration, fn func(context.Context) error) error {
	backoff := retryBaseDelay
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		opCtx, cancel := context.WithTimeout(ctx, opTimeout)
		err = fn(opCtx)
		cancel()

		switch {
		case err == nil:
			return nil
		case errors.Is(err, cinder.ErrQuota):
			return err
		case !cinder.IsRetryable(err):
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
