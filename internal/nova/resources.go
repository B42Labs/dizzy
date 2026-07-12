package nova

import (
	"context"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// status fetches a resource and returns its status without recording a sample.
// The readiness polls go through this so their repeated gets do not flood the
// per-call latency stats; the time-to-ready record stands in for them instead.
// Subnets report no status, so an existing subnet returns ("", nil).
func (c *Client) status(ctx context.Context, r resource.Resource) (string, error) {
	switch r.Kind {
	case KindServer:
		s, err := servers.Get(ctx, c.compute, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return s.Status, nil
	case KindVolume:
		v, err := volumes.Get(ctx, c.blockstorage, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return v.Status, nil
	case KindNetwork:
		n, err := networks.Get(ctx, c.network, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return n.Status, nil
	case KindPort:
		p, err := ports.Get(ctx, c.network, r.ID).Extract()
		if err != nil {
			return "", err
		}
		return p.Status, nil
	case KindSubnet:
		_, err := subnets.Get(ctx, c.network, r.ID).Extract()
		return "", err
	default:
		return "", fmt.Errorf("status not supported for kind %q", r.Kind)
	}
}

// Observe re-queries the live state of a created resource, recording the call.
// It returns the resource's status (empty for kinds that report none), whether
// the resource still exists, and any error other than a 404. A 404 is reported
// as ("", false, nil) so a resource deleted out of band reads as gone rather
// than as a failure. The status command drives this over a run's resources.
func (c *Client) Observe(ctx context.Context, r resource.Resource) (status string, exists bool, err error) {
	err = c.timed(ctx, string(r.Kind), "get", func(ctx context.Context) error {
		s, getErr := c.status(ctx, r)
		if getErr != nil {
			return getErr
		}
		status = s
		return nil
	})
	switch {
	case IsNotFound(err):
		return "", false, nil
	case err != nil:
		return "", false, err
	default:
		return status, true, nil
	}
}

// Delete removes a resource, recording the call. Servers, volumes, ports,
// subnets, and networks are deleted by id; a 404 surfaces to the caller, which
// treats an already-gone resource as success to keep cleanup idempotent.
func (c *Client) Delete(ctx context.Context, r resource.Resource) error {
	return c.timed(ctx, string(r.Kind), "delete", func(ctx context.Context) error {
		switch r.Kind {
		case KindServer:
			return servers.Delete(ctx, c.compute, r.ID).ExtractErr()
		case KindVolume:
			return volumes.Delete(ctx, c.blockstorage, r.ID, nil).ExtractErr()
		case KindPort:
			return ports.Delete(ctx, c.network, r.ID).ExtractErr()
		case KindSubnet:
			return subnets.Delete(ctx, c.network, r.ID).ExtractErr()
		case KindNetwork:
			return networks.Delete(ctx, c.network, r.ID).ExtractErr()
		default:
			return fmt.Errorf("delete not supported for kind %q", r.Kind)
		}
	})
}

// readyState reports whether status is the ready state for kind: a server or
// network is ready at ACTIVE, a volume at available, a port at ACTIVE or DOWN
// (an unbound port stays DOWN). Subnets have no status and are handled by
// WaitForReady directly.
func readyState(kind resource.Kind, status string) bool {
	switch kind {
	case KindServer, KindNetwork:
		return status == statusServerActive
	case KindVolume:
		return status == statusVolumeAvailable
	case KindPort:
		return status == statusServerActive || status == "DOWN"
	default:
		return false
	}
}

// terminalState reports whether status is a terminal failure for kind: a server
// at ERROR or a volume in any error_* state. Networks and ports have no terminal
// failure state a readiness poll must abort on.
func terminalState(kind resource.Kind, status string) bool {
	switch kind {
	case KindServer:
		return status == statusServerError
	case KindVolume:
		return isErrorStatus(status)
	default:
		return false
	}
}

// WaitForReady polls a created resource until it reaches its ready state,
// recording one Readiness sample. A subnet reports no status, so it returns nil
// immediately. It returns a terminal error when the resource reaches a terminal
// failure state (a server ERROR, a volume error_*), and ctx.Err() if ctx is
// cancelled or its deadline elapses first; the caller decides whether a
// readiness deadline is fatal.
func (c *Client) WaitForReady(ctx context.Context, r resource.Resource) error {
	if r.Kind == KindSubnet {
		return nil
	}
	return c.pollStatus(ctx, r,
		func(s string) bool { return readyState(r.Kind, s) },
		func(s string) bool { return terminalState(r.Kind, s) })
}

// WaitForServerStatus polls a server until it reaches want, recording one
// Readiness sample. It backs the stop/start (SHUTOFF) and resize (VERIFY_RESIZE)
// sequences, and treats an ERROR status as a terminal failure.
func (c *Client) WaitForServerStatus(ctx context.Context, r resource.Resource, want string) error {
	return c.pollStatus(ctx, r,
		func(s string) bool { return s == want },
		func(s string) bool { return want != statusServerError && s == statusServerError })
}

// WaitForVolumeStatus polls a data volume until it reaches want (in-use after an
// attach, available after a detach), recording one Readiness sample. An error_*
// status is a terminal failure.
func (c *Client) WaitForVolumeStatus(ctx context.Context, r resource.Resource, want string) error {
	return c.pollStatus(ctx, r,
		func(s string) bool { return s == want },
		func(s string) bool { return s != want && isErrorStatus(s) })
}

// pollStatus polls r until ready reports the current status is the target, or
// terminal reports it is an unrecoverable failure, recording one Readiness
// sample keyed on the resource kind. It returns a terminal error when the
// resource reaches a terminal state and ctx.Err() when the deadline elapses. A
// 404 aborts promptly: a resource this run created and is polling has gone
// (deleted out of band), which is definitive and must not masquerade as a slow
// readiness that spins to the deadline. Other Get errors are treated as transient
// and re-polled until the deadline.
func (c *Client) pollStatus(ctx context.Context, r resource.Resource, ready, terminal func(string) bool) error {
	start := time.Now()
	backoff := 200 * time.Millisecond
	for {
		status, err := c.status(ctx, r)
		switch {
		case err == nil && ready(status):
			c.recordReady(ctx, r, time.Since(start), true)
			return nil
		case err == nil && terminal(status):
			c.recordReady(ctx, r, time.Since(start), false)
			return fmt.Errorf("%s %s reached terminal status %q", r.Kind, r.ID, status)
		case IsNotFound(err):
			c.recordReady(ctx, r, time.Since(start), false)
			return fmt.Errorf("%s %s is gone while waiting for readiness: %w", r.Kind, r.ID, err)
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			c.recordReady(ctx, r, time.Since(start), false)
			return ctx.Err()
		}

		if backoff = time.Duration(float64(backoff) * 1.5); backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
	}
}

// WaitForGone polls a resource until it no longer exists (a 404). Cleanup uses
// it so a server's attachments release and the server is fully gone before its
// companion networks are deleted. It returns ctx.Err() if the deadline elapses
// first.
func (c *Client) WaitForGone(ctx context.Context, r resource.Resource) error {
	backoff := 200 * time.Millisecond
	for {
		if _, err := c.status(ctx, r); IsNotFound(err) {
			return nil
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}

		if backoff = time.Duration(float64(backoff) * 1.5); backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
	}
}

// recordReady records one time-to-ready sample into both the collector and
// telemetry.
func (c *Client) recordReady(ctx context.Context, r resource.Resource, d time.Duration, ok bool) {
	c.metrics.RecordReadiness(metrics.Readiness{Type: string(r.Kind), Duration: d, OK: ok})
	c.tel.RecordTimeToReady(ctx, string(r.Kind), d, ok)
}

// ListByTypeMetadata returns the resources of kind carrying the
// dizzy:type=<kind> metadata, matching every tester run rather than one run's
// dizzy:run metadata. It is the metadata discovery step for the monitor loop's
// pre-flight orphan sweep and covers the metadata-stamped kinds (servers,
// volumes); other kinds are discovered by tag through ListByTypeTag.
func (c *Client) ListByTypeMetadata(ctx context.Context, kind resource.Kind) ([]resource.Resource, error) {
	filter := map[string]string{metaType: string(kind)}
	switch kind {
	case KindServer:
		return c.listServersByMetadata(ctx, filter)
	case KindVolume:
		return c.listVolumesByMetadata(ctx, filter)
	default:
		return nil, fmt.Errorf("list by type metadata not supported for kind %q", kind)
	}
}
