// Package executor turns a Nova plan into a real server fleet and drives it
// through its lifecycle. It runs six strictly ordered stages — create the
// companion networks (and their subnets), create the data volumes, create the
// ports, boot the servers, run each server's post-boot operation sequence, then
// delete the servers the plan marks for deletion — with independent work within
// a create stage running concurrently up to a configurable limit, transient
// failures retried with exponential backoff, quota errors failing fast, and
// per-operation timeouts. Each server's operation sequence (attach volumes and
// ports, stop/start, resize+confirm, live-migrate, detach) runs sequentially
// within its work item, since Nova rejects concurrent state transitions on one
// server, while different servers' sequences run concurrently. The created
// resources and the timing the Nova wrappers record are the hand-off surface a
// later run record and cleanup consume.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/B42Labs/dizzy/internal/nova"
	"github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// retryBaseDelay, retryMaxDelay, and maxAttempts bound the per-operation retry
// of transient errors. They duplicate the neutron/cinder executor policy rather
// than sharing it: each package's WithRetry hardcodes its own service's
// classifiers, and this one uses the nova classifiers and backs both the apply
// path and the nova chaos graph.
const (
	retryBaseDelay = 250 * time.Millisecond
	retryMaxDelay  = 5 * time.Second
	maxAttempts    = 5
)

// Nova is the create-drive-and-wait surface the executor drives. It is the
// single ports-and-adapters seam to the cloud: *nova.Client satisfies it in
// production and a fake satisfies it in tests. It is deliberately broad because
// a compute run exercises the full server verb set through one client.
type Nova interface {
	CreateNetwork(ctx context.Context, n plan.Network) (resource.Resource, error)
	CreateSubnet(ctx context.Context, n plan.Network, networkID string) (resource.Resource, error)
	CreateVolume(ctx context.Context, v plan.Volume) (resource.Resource, error)
	CreatePort(ctx context.Context, pt plan.Port, networkID string) (resource.Resource, error)
	CreateServer(ctx context.Context, s plan.Server, boot nova.BootSpec) (resource.Resource, error)
	StopServer(ctx context.Context, r resource.Resource) error
	StartServer(ctx context.Context, r resource.Resource) error
	RebootServerHard(ctx context.Context, r resource.Resource) error
	ResizeServer(ctx context.Context, r resource.Resource, flavorID string) error
	ConfirmResizeServer(ctx context.Context, r resource.Resource) error
	LiveMigrateServer(ctx context.Context, r resource.Resource) error
	AttachVolume(ctx context.Context, server, volume resource.Resource) error
	DetachVolume(ctx context.Context, server, volume resource.Resource) error
	AttachPort(ctx context.Context, server, port resource.Resource) error
	DetachPort(ctx context.Context, server, port resource.Resource) error
	Delete(ctx context.Context, r resource.Resource) error
	WaitForReady(ctx context.Context, r resource.Resource) error
	WaitForServerStatus(ctx context.Context, r resource.Resource, want string) error
	WaitForVolumeStatus(ctx context.Context, r resource.Resource, want string) error
	WaitForGone(ctx context.Context, r resource.Resource) error
}

// The production *nova.Client must satisfy the seam.
var _ Nova = (*nova.Client)(nil)

// Nova server and volume statuses the executor's sequences wait for. They are
// the strings the WaitFor* helpers compare against, duplicated from the client
// so the executor reads without importing them by unexported name.
const (
	statusServerActive       = "ACTIVE"
	statusServerShutoff      = "SHUTOFF"
	statusServerVerifyResize = "VERIFY_RESIZE"
	statusVolumeAvailable    = "available"
	statusVolumeInUse        = "in-use"
)

// Resolved carries the by-name references resolved to cloud ids at apply time,
// plus whether live migration is enabled for this run (the admin pre-check's
// verdict). The executor threads these into the boot and resize operations.
type Resolved struct {
	ImageID        string
	FlavorID       string
	ResizeFlavorID string
	LiveMigration  bool
}

// Result is the outcome of an apply: every resource that was created, in
// dependency order (networks, subnets, volumes, ports, servers).
type Result struct {
	Created []resource.Resource
}

// Apply creates the plan's companion networks, volumes, and ports, boots its
// servers, drives each server through its operation sequence, and deletes the
// servers marked for deletion, in six strictly ordered stages. Within a create
// stage independent work runs concurrently up to concurrency, each operation
// retried on transient errors and bounded by opTimeout. r supplies the resolved
// image/flavor ids and whether live migration is enabled. The first quota error,
// or any non-retryable error, stops the run and is returned along with the
// resources created so far; ctx cancellation returns ctx.Err().
func Apply(ctx context.Context, c Nova, p *plan.Plan, concurrency int, opTimeout time.Duration, r Resolved) (*Result, error) {
	e := &applier{c: c, opTimeout: opTimeout, resolved: r}
	result := &Result{}

	// Stage 1: networks and their subnets.
	netByLogical := make(map[string]resource.Resource, len(p.Networks))
	netRes, err := runStage(ctx, p.Networks, concurrency, func(ctx context.Context, n plan.Network) ([]resource.Resource, error) {
		return e.provisionNetwork(ctx, n)
	})
	for _, rs := range netRes {
		result.Created = appendCreated(result.Created, rs)
	}
	for i, n := range p.Networks {
		if len(netRes[i]) > 0 && netRes[i][0].ID != "" {
			netByLogical[n.Name] = netRes[i][0] // the network resource is first, its subnet second
		}
	}
	if err != nil {
		return result, err
	}

	// Stage 2: data volumes.
	volByLogical := make(map[string]resource.Resource, len(p.Volumes))
	volRes, err := runStage(ctx, p.Volumes, concurrency, func(ctx context.Context, v plan.Volume) (resource.Resource, error) {
		return e.provisionVolume(ctx, v)
	})
	result.Created = appendCreated(result.Created, volRes)
	for i, v := range p.Volumes {
		if volRes[i].ID != "" {
			volByLogical[v.Name] = volRes[i]
		}
	}
	if err != nil {
		return result, err
	}

	// Stage 3: ports (created, not yet attached).
	portByLogical := make(map[string]resource.Resource, len(p.Ports))
	portRes, err := runStage(ctx, p.Ports, concurrency, func(ctx context.Context, pt plan.Port) (resource.Resource, error) {
		return e.provisionPort(ctx, pt, netByLogical[pt.Network].ID)
	})
	result.Created = appendCreated(result.Created, portRes)
	for i, pt := range p.Ports {
		if portRes[i].ID != "" {
			portByLogical[pt.Name] = portRes[i]
		}
	}
	if err != nil {
		return result, err
	}

	// Stage 4: boot the servers.
	serverByLogical := make(map[string]resource.Resource, len(p.Servers))
	srvRes, err := runStage(ctx, p.Servers, concurrency, func(ctx context.Context, s plan.Server) (resource.Resource, error) {
		return e.provisionServer(ctx, s, netByLogical)
	})
	result.Created = appendCreated(result.Created, srvRes)
	for i, s := range p.Servers {
		if srvRes[i].ID != "" {
			serverByLogical[s.Name] = srvRes[i]
		}
	}
	if err != nil {
		return result, err
	}

	// Stage 5: each server's post-boot operation sequence. One work item per
	// server; the sequence runs sequentially within the item while different
	// servers run concurrently.
	volsByServer := groupVolumesByServer(p.Volumes)
	portsByServer := groupPortsByServer(p.Ports)
	_, err = runStage(ctx, p.Servers, concurrency, func(ctx context.Context, s plan.Server) (resource.Resource, error) {
		srv, ok := serverByLogical[s.Name]
		if !ok {
			return resource.Resource{}, nil // the boot produced no server; nothing to drive
		}
		return resource.Resource{}, e.driveServer(ctx, s, srv, volByLogical, portByLogical, volsByServer[s.Name], portsByServer[s.Name])
	})
	if err != nil {
		return result, err
	}

	// Stage 6: delete the servers the plan marks for deletion.
	toDelete := make([]resource.Resource, 0)
	for _, s := range p.Servers {
		if s.Delete {
			if srv, ok := serverByLogical[s.Name]; ok {
				toDelete = append(toDelete, srv)
			}
		}
	}
	_, err = runStage(ctx, toDelete, concurrency, func(ctx context.Context, srv resource.Resource) (resource.Resource, error) {
		return resource.Resource{}, e.deleteServer(ctx, srv)
	})
	if err != nil {
		return result, err
	}

	return result, nil
}

// applier carries the apply-wide configuration shared by the stage helpers.
type applier struct {
	c         Nova
	opTimeout time.Duration
	resolved  Resolved
}

// provisionNetwork creates a network and its subnet (retrying transient errors),
// waiting each to become ready. It returns both created resources — the network
// first, the subnet second — even when a readiness wait fails, so a resource
// that exists but errored is still recorded for cleanup.
func (e *applier) provisionNetwork(ctx context.Context, n plan.Network) ([]resource.Resource, error) {
	var net resource.Resource
	if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return e.c.CreateNetwork(ctx, n)
	}, &net); err != nil {
		return nil, err
	}
	slog.Info("created resource", "kind", net.Kind, "logical", net.Logical, "id", net.ID)
	created := []resource.Resource{net}
	if err := e.waitReady(ctx, net); err != nil {
		return created, err
	}

	var sub resource.Resource
	if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return e.c.CreateSubnet(ctx, n, net.ID)
	}, &sub); err != nil {
		return created, err
	}
	slog.Info("created resource", "kind", sub.Kind, "logical", sub.Logical, "id", sub.ID)
	created = append(created, sub)
	if err := e.waitReady(ctx, sub); err != nil {
		return created, err
	}
	return created, nil
}

// provisionVolume creates a data volume (retrying transient errors) and waits
// for it to become available.
func (e *applier) provisionVolume(ctx context.Context, v plan.Volume) (resource.Resource, error) {
	var res resource.Resource
	if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return e.c.CreateVolume(ctx, v)
	}, &res); err != nil {
		return resource.Resource{}, err
	}
	slog.Info("created resource", "kind", res.Kind, "logical", res.Logical, "id", res.ID)
	if err := e.waitReady(ctx, res); err != nil {
		return res, err
	}
	return res, nil
}

// provisionPort creates a port on networkID (retrying transient errors). Ports
// are attached to their server later, in the server's operation sequence.
func (e *applier) provisionPort(ctx context.Context, pt plan.Port, networkID string) (resource.Resource, error) {
	var res resource.Resource
	if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return e.c.CreatePort(ctx, pt, networkID)
	}, &res); err != nil {
		return resource.Resource{}, err
	}
	slog.Info("created resource", "kind", res.Kind, "logical", res.Logical, "id", res.ID)
	return res, nil
}

// provisionServer boots a server (retrying transient errors) and waits for it to
// become ACTIVE, resolving its network ids from netByLogical.
func (e *applier) provisionServer(ctx context.Context, s plan.Server, netByLogical map[string]resource.Resource) (resource.Resource, error) {
	networkIDs := make([]string, 0, len(s.Networks))
	for _, name := range s.Networks {
		networkIDs = append(networkIDs, netByLogical[name].ID)
	}
	boot := nova.BootSpec{ImageID: e.resolved.ImageID, FlavorID: e.resolved.FlavorID, NetworkIDs: networkIDs}

	var res resource.Resource
	if err := withCreate(ctx, e.opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return e.c.CreateServer(ctx, s, boot)
	}, &res); err != nil {
		return resource.Resource{}, err
	}
	slog.Info("created resource", "kind", res.Kind, "logical", res.Logical, "id", res.ID)
	if err := e.waitServerActive(ctx, res); err != nil {
		return res, err
	}
	return res, nil
}

// driveServer runs one server's post-boot operation sequence, in order: attach
// its data volumes (wait in-use), attach its ports, run its stop/start cycle,
// resize and confirm, live-migrate (only when enabled for the run), then detach
// the flagged ports and volumes. Any step's error stops the sequence.
func (e *applier) driveServer(ctx context.Context, s plan.Server, srv resource.Resource, volByLogical, portByLogical map[string]resource.Resource, vols []plan.Volume, ports []plan.Port) error {
	for _, v := range vols {
		vol, ok := volByLogical[v.Name]
		if !ok {
			continue
		}
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.AttachVolume(ctx, srv, vol) }); err != nil {
			return err
		}
		if err := e.waitVolumeStatus(ctx, vol, statusVolumeInUse); err != nil {
			return err
		}
	}

	for _, pt := range ports {
		port, ok := portByLogical[pt.Name]
		if !ok {
			continue
		}
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.AttachPort(ctx, srv, port) }); err != nil {
			return err
		}
	}

	switch s.StopStart {
	case plan.StopStartSoft:
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.StopServer(ctx, srv) }); err != nil {
			return err
		}
		if err := e.waitServerStatus(ctx, srv, statusServerShutoff); err != nil {
			return err
		}
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.StartServer(ctx, srv) }); err != nil {
			return err
		}
		if err := e.waitServerStatus(ctx, srv, statusServerActive); err != nil {
			return err
		}
	case plan.StopStartHard:
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.RebootServerHard(ctx, srv) }); err != nil {
			return err
		}
		if err := e.waitServerStatus(ctx, srv, statusServerActive); err != nil {
			return err
		}
	}

	if s.Resize {
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error {
			return e.c.ResizeServer(ctx, srv, e.resolved.ResizeFlavorID)
		}); err != nil {
			return err
		}
		if err := e.waitServerStatus(ctx, srv, statusServerVerifyResize); err != nil {
			return err
		}
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.ConfirmResizeServer(ctx, srv) }); err != nil {
			return err
		}
		if err := e.waitServerStatus(ctx, srv, statusServerActive); err != nil {
			return err
		}
	}

	// Live migration runs only when the admin pre-check enabled it for the run;
	// otherwise it is skipped without an attempt, never aborting the run.
	if s.LiveMigrate && e.resolved.LiveMigration {
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.LiveMigrateServer(ctx, srv) }); err != nil {
			return err
		}
		if err := e.waitServerStatus(ctx, srv, statusServerActive); err != nil {
			return err
		}
	}

	for _, pt := range ports {
		if !pt.Detach {
			continue
		}
		port, ok := portByLogical[pt.Name]
		if !ok {
			continue
		}
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.DetachPort(ctx, srv, port) }); err != nil {
			return err
		}
	}

	for _, v := range vols {
		if !v.Detach {
			continue
		}
		vol, ok := volByLogical[v.Name]
		if !ok {
			continue
		}
		if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.DetachVolume(ctx, srv, vol) }); err != nil {
			return err
		}
		if err := e.waitVolumeStatus(ctx, vol, statusVolumeAvailable); err != nil {
			return err
		}
	}

	return nil
}

// deleteServer deletes a server and waits for it to be gone, so its attachments
// release before any later teardown.
func (e *applier) deleteServer(ctx context.Context, srv resource.Resource) error {
	if err := WithRetry(ctx, e.opTimeout, func(ctx context.Context) error { return e.c.Delete(ctx, srv) }); err != nil {
		if nova.IsNotFound(err) {
			return nil
		}
		return err
	}
	slog.Info("deleted resource", "kind", srv.Kind, "logical", srv.Logical, "id", srv.ID)
	goneCtx, cancel := context.WithTimeout(ctx, e.opTimeout)
	defer cancel()
	return e.c.WaitForGone(goneCtx, srv)
}

// waitReady bounds a readiness poll with opTimeout, tolerating a deadline while
// the run is still live (warn and continue). A parent cancellation returns
// ctx.Err(); a terminal backend error status is returned so the stage fails.
func (e *applier) waitReady(ctx context.Context, res resource.Resource) error {
	return e.tolerateDeadline(ctx, res, func(ctx context.Context) error { return e.c.WaitForReady(ctx, res) })
}

// waitServerStatus bounds a server-status poll with opTimeout, with the same
// deadline tolerance as waitReady.
func (e *applier) waitServerStatus(ctx context.Context, res resource.Resource, want string) error {
	return e.tolerateDeadline(ctx, res, func(ctx context.Context) error { return e.c.WaitForServerStatus(ctx, res, want) })
}

// waitServerActive bounds the boot ACTIVE poll with opTimeout and, unlike the
// tolerated waits, treats a missed deadline as fatal: a server that never
// reaches ACTIVE cannot be driven, since Stage 5 would issue attach/stop/resize
// against a still-BUILDING instance and Nova rejects each with a 409. Tolerating
// the deadline would only defer the failure to a later op with a misleading
// cause, so the boot wait surfaces the accurate error instead.
func (e *applier) waitServerActive(ctx context.Context, res resource.Resource) error {
	waitCtx, cancel := context.WithTimeout(ctx, e.opTimeout)
	defer cancel()
	if err := e.c.WaitForServerStatus(waitCtx, res, statusServerActive); err != nil {
		return fmt.Errorf("server %s did not reach %s: %w", res.ID, statusServerActive, err)
	}
	return nil
}

// waitVolumeStatus bounds a volume-status poll with opTimeout, with the same
// deadline tolerance as waitReady.
func (e *applier) waitVolumeStatus(ctx context.Context, res resource.Resource, want string) error {
	return e.tolerateDeadline(ctx, res, func(ctx context.Context) error { return e.c.WaitForVolumeStatus(ctx, res, want) })
}

// tolerateDeadline bounds wait with opTimeout. A readiness deadline that elapses
// while the run is still live is warned and tolerated (nil); a parent
// cancellation returns ctx.Err(); a terminal backend error is returned so the
// stage fails.
func (e *applier) tolerateDeadline(ctx context.Context, res resource.Resource, wait func(context.Context) error) error {
	waitCtx, cancel := context.WithTimeout(ctx, e.opTimeout)
	defer cancel()
	err := wait(waitCtx)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("resource did not reach the expected state before deadline",
			"kind", res.Kind, "id", res.ID, "logical", res.Logical, "error", err)
		return nil
	}
	return err
}

// groupVolumesByServer buckets a plan's volumes by their server, preserving plan
// order so each server's sequence is deterministic.
func groupVolumesByServer(vols []plan.Volume) map[string][]plan.Volume {
	byServer := make(map[string][]plan.Volume)
	for _, v := range vols {
		byServer[v.Server] = append(byServer[v.Server], v)
	}
	return byServer
}

// groupPortsByServer buckets a plan's ports by their server, preserving plan
// order.
func groupPortsByServer(ports []plan.Port) map[string][]plan.Port {
	byServer := make(map[string][]plan.Port)
	for _, pt := range ports {
		byServer[pt.Server] = append(byServer[pt.Server], pt)
	}
	return byServer
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

// runStage runs work over items using a fixed pool of at most concurrency
// workers reading from a job channel — a bounded pool rather than one goroutine
// per item, so a large plan cannot exhaust resources. Results are returned in
// item order (populated even for a failing item, so a caller can record what
// already exists). The first error cancels the stage, stops dispatching, and is
// returned, with a quota error taking priority and a parent-context cancellation
// reported as ctx.Err().
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
		if errors.Is(err, nova.ErrQuota) {
			return results, err
		}
	}
	if err := ctx.Err(); err != nil {
		return results, err
	}
	// Prefer a real failure over the context.Canceled that in-flight siblings
	// return once the first error triggers cancel(); otherwise the root cause
	// is masked by an arbitrary cancelled sibling.
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
// error. Backoff sleeps honor the parent context. It is exported so the nova
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
		case errors.Is(err, nova.ErrQuota):
			return err
		case !nova.IsRetryable(err):
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
