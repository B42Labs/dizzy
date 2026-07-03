package main

import (
	"context"
	"time"

	"github.com/B42Labs/openstack-tester/internal/executor"
	"github.com/B42Labs/openstack-tester/internal/neutron"
)

// timeoutCleaner wraps a Cleaner so every cloud operation executor.Cleanup
// performs is bounded by opTimeout. Its callers — a monitor iteration's
// teardown, an interrupted apply's teardown, and a chaos run's teardown — all
// drive Cleanup on a context.WithoutCancel that strips any deadline, and the
// gophercloud client sets no HTTP timeout of its own (apply gets its
// per-operation bound from executor.WithRetry). Without this a wedged Neutron
// call would hang the teardown — and, in the monitor loop, the whole loop —
// indefinitely. Each call gets its own timeout, so a large teardown is bounded
// per operation, the same way apply bounds each create.
type timeoutCleaner struct {
	inner     executor.Cleaner
	opTimeout time.Duration
}

func (t timeoutCleaner) ListByTag(ctx context.Context, kind neutron.Kind, runID string) ([]neutron.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListByTag(ctx, kind, runID)
}

func (t timeoutCleaner) DetachRouterInterfaces(ctx context.Context, routerID string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.DetachRouterInterfaces(ctx, routerID)
}

func (t timeoutCleaner) DeleteNetworkPorts(ctx context.Context, networkID string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.DeleteNetworkPorts(ctx, networkID)
}

func (t timeoutCleaner) Delete(ctx context.Context, r neutron.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.Delete(ctx, r)
}
