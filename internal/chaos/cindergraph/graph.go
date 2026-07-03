// Package cindergraph builds the Cinder churn graph the service-neutral chaos
// engine schedules. It maps a Cinder plan onto engine nodes: a volume node per
// planned volume (extendable to its planned target as the engine's mutate
// action) and a snapshot node per planned snapshot, parented on its source
// volume. Readiness is folded into each operation — a create or extend completes
// only when the resource reaches available, and a delete only when it is gone —
// and snapshot/extend operations on one volume are serialized behind a per-
// volume gate, since some backends reject concurrent operations on a volume.
// Keeping the Cinder coupling here leaves the chaos engine free of any
// service-specific import.
package cindergraph

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/B42Labs/openstack-tester/internal/chaos"
	"github.com/B42Labs/openstack-tester/internal/cinder"
	cinderexec "github.com/B42Labs/openstack-tester/internal/cinder/executor"
	cinderplan "github.com/B42Labs/openstack-tester/internal/cinder/plan"
	"github.com/B42Labs/openstack-tester/internal/resource"
)

// Cinder is the create/extend/delete-and-wait surface the chaos engine drives
// through the nodes this package builds. It is the consumer-defined
// ports-and-adapters seam to the cloud — *cinder.Client satisfies it in
// production and a fake satisfies it in tests. It mirrors the apply executor's
// Cinder plus the Delete and WaitForGone the churn deletes need.
type Cinder interface {
	CreateVolume(ctx context.Context, v cinderplan.Volume, volumeType string) (resource.Resource, error)
	ExtendVolume(ctx context.Context, r resource.Resource, newSizeGiB int) error
	CreateSnapshot(ctx context.Context, s cinderplan.Snapshot, volumeID string) (resource.Resource, error)
	Delete(ctx context.Context, r resource.Resource) error
	WaitForReady(ctx context.Context, r resource.Resource) error
	WaitForGone(ctx context.Context, r resource.Resource) error
}

// The production *cinder.Client must satisfy the seam.
var _ Cinder = (*cinder.Client)(nil)

// Build turns a Cinder plan into the churn graph: one volume node per volume
// (parentless, mutable when it has a planned resize target) and one snapshot
// node per snapshot, parented on its source volume so the engine never
// snapshots an absent volume nor deletes a volume with live snapshots. Every
// node's closures capture c and run through the cinder executor's retry policy,
// bounded by opTimeout; volumeType is applied to every volume create ("" is the
// cloud's default type). The plan is validated first so a dangling snapshot
// reference fails loudly instead of yielding a node that can never be created.
func Build(p *cinderplan.Plan, c Cinder, volumeType string, opTimeout time.Duration) ([]chaos.Node, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid plan: %w", err)
	}

	// One gate per volume serializes the snapshot and extend operations of that
	// volume's family, since some backends reject a snapshot or extend while the
	// volume is already snapshotting or extending. Volume create/delete take it
	// too so an extend never races the volume's own teardown. The gate is a
	// capacity-1 channel the engine acquires before granting a concurrency slot —
	// unlike a mutex held inside the op, an op parked behind a busy family never
	// occupies a slot, so other families keep churning at full concurrency.
	family := make(map[string]chan struct{}, len(p.Volumes))
	for _, v := range p.Volumes {
		family[v.Name] = make(chan struct{}, 1)
	}

	var nodes []chaos.Node

	for _, v := range p.Volumes {
		node := chaos.Node{
			Key: v.Name, Kind: cinder.KindVolume, Gate: family[v.Name],
			Create: func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
				return createReady(ctx, opTimeout, c, func(ctx context.Context) (resource.Resource, error) {
					return c.CreateVolume(ctx, v, volumeType)
				})
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteGone(ctx, opTimeout, c, res)
			},
		}
		// A volume is mutable exactly when it has a planned resize target: the
		// engine may extend it, at most once per lifetime, to that target.
		if v.ResizeToGiB > 0 {
			target := v.ResizeToGiB
			node.Mutate = func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				if err := cinderexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
					return c.ExtendVolume(ctx, res, target)
				}); err != nil {
					return err
				}
				return waitReady(ctx, opTimeout, c, res)
			}
		}
		nodes = append(nodes, node)
	}

	for _, s := range p.Snapshots {
		nodes = append(nodes, chaos.Node{
			Key: s.Name, Kind: cinder.KindSnapshot, Parents: []string{s.Volume}, Gate: family[s.Volume],
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createReady(ctx, opTimeout, c, func(ctx context.Context) (resource.Resource, error) {
					return c.CreateSnapshot(ctx, s, ids[s.Volume])
				})
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteGone(ctx, opTimeout, c, res)
			},
		})
	}

	return nodes, nil
}

// createReady runs a create through the retry policy and then folds readiness
// into the operation: it returns the created resource even when readiness fails,
// so the engine records a failed op but keeps the resource deletable and in the
// run record. A terminal error status (error, error_extending) or a readiness
// deadline is an operation failure — deliberately stricter than apply, which
// tolerates a deadline, because a not-yet-available volume can be neither
// snapshotted, extended, nor reliably deleted.
func createReady(ctx context.Context, opTimeout time.Duration, c Cinder, create func(context.Context) (resource.Resource, error)) (resource.Resource, error) {
	var res resource.Resource
	err := cinderexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		r, createErr := create(ctx)
		if createErr != nil {
			return createErr
		}
		res = r
		return nil
	})
	if err != nil {
		return resource.Resource{}, err
	}
	if err := waitReady(ctx, opTimeout, c, res); err != nil {
		return res, err
	}
	return res, nil
}

// waitReady polls res to available, bounded by opTimeout.
func waitReady(ctx context.Context, opTimeout time.Duration, c Cinder, res resource.Resource) error {
	readyCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return c.WaitForReady(readyCtx, res)
}

// deleteGone deletes res through the retry policy, folds an already-gone (404)
// into success, and waits for the resource to be fully gone (bounded by
// opTimeout) so a re-created logical name can never transiently double-count
// against the quota envelope.
func deleteGone(ctx context.Context, opTimeout time.Duration, c Cinder, res resource.Resource) error {
	err := cinderexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		return c.Delete(ctx, res)
	})
	if err != nil {
		if cinder.IsNotFound(err) {
			return nil // already gone is a successful, idempotent delete
		}
		return err
	}
	goneCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return c.WaitForGone(goneCtx, res)
}

// Classify labels an operation error for the churn engine's per-bucket error
// breakdown, reusing the cinder classification helpers so the labels match the
// kinds operators already see in the metrics report. It is wired into the engine
// via chaos.Config.Classify.
func Classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, cinder.ErrQuota):
		return "quota"
	case cinder.IsNotFound(err):
		return "not-found"
	case cinder.IsRetryable(err):
		return "transient"
	default:
		return "other"
	}
}
