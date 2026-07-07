// Package cinder wraps the gophercloud v2 block-storage calls used to apply a
// Cinder plan against a real cloud. It is the ports-and-adapters seam to
// OpenStack: every created resource is given a deterministic dizzy-<id>-
// <logical> name and metadata dizzy:run=<id> / dizzy:type=<kind> so a run
// can be identified and, later, cleaned up by that metadata. Each call is timed
// through a metrics.Collector, and status-bearing resources can be polled until
// ready. Unlike Neutron there is no separate tag step: the metadata rides in the
// create request itself.
package cinder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// Cinder resource kinds. They double as the metrics "type" label and the
// metadata value written under dizzy:type.
const (
	KindVolume   resource.Kind = "volume"
	KindSnapshot resource.Kind = "snapshot"
)

// Cinder status strings the readiness poll branches on. A volume or snapshot is
// ready when available; a status with the error prefix (error, error_extending,
// error_deleting) is a terminal failure.
const (
	statusAvailable = "available"
	statusErrPrefix = "error"
)

// Metadata keys binding a created resource to a tester run and its kind. They
// mirror Neutron's dizzy:run / dizzy:type tags.
const (
	metaRun  = "dizzy:run"
	metaType = "dizzy:type"
)

// ErrQuota is the sentinel a create/extend wrapper wraps when Cinder rejects the
// request because a quota was exceeded. The executor matches it with errors.Is
// to fail the run fast instead of retrying.
var ErrQuota = errors.New("cinder: quota exceeded")

// Client wraps an authenticated BlockStorageV3 service client, binding every
// created resource to a run id and recording timing into a Collector.
type Client struct {
	gc      *gophercloud.ServiceClient
	runID   string
	metrics *metrics.Collector
	tel     *telemetry.Telemetry
}

// New returns a Client that names and stamps resources for runID and records
// timing into m.
func New(gc *gophercloud.ServiceClient, runID string, m *metrics.Collector) *Client {
	return &Client{gc: gc, runID: runID, metrics: m}
}

// SetTelemetry attaches an OTEL export seam so each timed call and readiness
// poll is recorded live alongside the in-memory collector, which stays the
// source of truth for run records and reports. A nil t (the default after New)
// leaves export disabled: every telemetry call is a no-op.
func (c *Client) SetTelemetry(t *telemetry.Telemetry) {
	c.tel = t
}

// resourceName builds the deterministic cloud name for a logical plan name.
func resourceName(runID, logical string) string {
	return "dizzy-" + runID + "-" + logical
}

// runMetadata returns the metadata stamped on every created resource of the
// given kind: the run identifier and the resource type. Discovery for cleanup
// filters on the run key.
func runMetadata(runID string, kind resource.Kind) map[string]string {
	return map[string]string{
		metaRun:  runID,
		metaType: string(kind),
	}
}

// metadataMatches reports whether md carries every key/value pair in filter. It
// is the client-side backstop both metadata list paths apply, so a server that
// ignores the metadata query never widens the result beyond what the tool
// actually tagged. An empty filter matches everything, which is why the run-id
// and type callers always pass a non-empty filter.
func metadataMatches(md, filter map[string]string) bool {
	for k, v := range filter {
		if md[k] != v {
			return false
		}
	}
	return true
}

// timed runs fn, records a Sample for the attempt (including the error
// classification extracted from any gophercloud error), mirrors the same
// measurement live into telemetry under the low-cardinality op label, and
// returns fn's error unchanged. The in-memory collector stays the source of
// truth for run records and reports; the telemetry record is additive and a
// no-op when export is disabled.
func (c *Client) timed(ctx context.Context, typ, op string, fn func(context.Context) error) error {
	start := time.Now()
	err := fn(ctx)
	d := time.Since(start)
	ek := errKind(err)
	c.metrics.Record(metrics.Sample{
		Type:     typ,
		Duration: d,
		Success:  err == nil,
		ErrKind:  ek,
	})
	c.tel.RecordOperation(ctx, typ, op, d, ek)
	return err
}

// wrapCreate adds operation context to a create/extend error and, when the
// error is a quota rejection, threads ErrQuota into the chain so the executor
// can fail fast while preserving the underlying gophercloud error for
// classification.
func wrapCreate(kind resource.Kind, logical string, err error) error {
	if isQuota(err) {
		return fmt.Errorf("creating %s %q: %w: %w", kind, logical, ErrQuota, err)
	}
	return fmt.Errorf("creating %s %q: %w", kind, logical, err)
}

// WaitForReady polls a created resource until it becomes available, recording
// one Readiness sample. It returns a terminal error when the status has the
// error prefix (error, error_extending) — the create or extend failed on the
// backend — and ctx.Err() if ctx is cancelled or its deadline elapses first;
// the caller decides whether a readiness deadline is fatal.
func (c *Client) WaitForReady(ctx context.Context, r resource.Resource) error {
	start := time.Now()
	backoff := 200 * time.Millisecond
	for {
		status, err := c.status(ctx, r)
		switch {
		case err == nil && status == statusAvailable:
			c.recordReady(ctx, r, time.Since(start), true)
			return nil
		case err == nil && isErrorStatus(status):
			// A terminal backend failure: record it as a failed readiness and
			// surface it so the executor fails the item rather than spinning until
			// the deadline.
			c.recordReady(ctx, r, time.Since(start), false)
			return fmt.Errorf("%s %s reached terminal status %q", r.Kind, r.ID, status)
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
// it so a volume's snapshots are fully gone before the volume is deleted, since
// Cinder rejects deleting a volume whose snapshots still exist and snapshot
// deletion is asynchronous. It returns ctx.Err() if the deadline elapses first.
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

// isErrorStatus reports whether a Cinder status is a terminal error state (any
// status beginning with "error": error, error_extending, error_deleting).
func isErrorStatus(status string) bool {
	return strings.HasPrefix(status, statusErrPrefix)
}

// httpStatus returns the HTTP status code carried by a gophercloud error, or 0
// when the error is nil or carries none.
func httpStatus(err error) int {
	var code gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &code) {
		return code.GetStatusCode()
	}
	return 0
}

// errKind classifies an error into a stable, low-cardinality label for the
// metrics error breakdown. It returns the empty string for a nil error.
func errKind(err error) string {
	switch {
	case err == nil:
		return ""
	case isQuota(err):
		return "quota"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	if s := httpStatus(err); s != 0 {
		// The status is the peer's: an endpoint behind a proxy, LB, or WAF can
		// report any 3-digit code, so bound the label to the codes Cinder itself
		// returns (413 is its over-limit quota rejection) and collapse the rest to
		// http_other. This keeps error.kind a bounded enum like every other label.
		switch s {
		case 400, 401, 403, 404, 409, 413, 429, 500, 502, 503, 504:
			return fmt.Sprintf("http_%d", s)
		default:
			return "http_other"
		}
	}
	return "other"
}

// IsRetryable reports whether err is a transient Cinder failure worth retrying:
// a 5xx, a 429 rate-limit, or a 409 conflict that is not a quota rejection.
func IsRetryable(err error) bool {
	if err == nil || isQuota(err) {
		return false
	}
	for _, code := range []int{500, 502, 503, 504, 429, 409} {
		if gophercloud.ResponseCodeIs(err, code) {
			return true
		}
	}
	return false
}

// IsNotFound reports whether err is a Cinder 404. Status and cleanup use it to
// treat a resource that is already gone as success rather than an error, which
// is what makes cleanup idempotent.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if gophercloud.ResponseCodeIs(err, 404) {
		return true
	}
	var nf gophercloud.ErrResourceNotFound
	return errors.As(err, &nf)
}

// isExtendAlreadyApplied reports whether err is Cinder's rejection of an extend
// whose new size is not larger than the volume's current size. Cinder raises it
// (HTTP 400, "New size for extend must be greater than current size") only when
// the volume already meets or exceeds the requested size, so on a retried extend
// it confirms the first request already committed and the volume is at the
// target. Body phrasing varies across releases, so match on the stable fragment,
// mirroring isQuota.
func isExtendAlreadyApplied(err error) bool {
	var code gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &code) || code.Actual != 400 {
		return false
	}
	return strings.Contains(strings.ToLower(string(code.Body)), "greater than current size")
}

// isQuota reports whether err is a Cinder quota rejection. Cinder signals an
// over-limit with HTTP 413 (OverLimit), so any 413 counts; a 403 or 400 whose
// body mentions a quota is also treated as one, since body phrasing varies
// across releases.
func isQuota(err error) bool {
	var code gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &code) {
		return false
	}
	if code.Actual == 413 {
		return true
	}
	if code.Actual != 403 && code.Actual != 400 {
		return false
	}
	body := strings.ToLower(string(code.Body))
	return strings.Contains(body, "overlimit") ||
		strings.Contains(body, "quota") ||
		strings.Contains(body, "exceeds allowed")
}
