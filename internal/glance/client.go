// Package glance wraps the gophercloud v2 image (Glance) v2 calls used to apply a
// Glance plan against a real cloud. It is the ports-and-adapters seam to
// OpenStack for images: dizzy creates every image itself with a synthetic,
// generated data payload uploaded through the direct data path (no web-download
// import, no multi-store placement — every image lands in the cloud's default
// store), and stamps each with the run identity as first-class Glance image tags
// (dizzy:run=<id> and dizzy:type=image, set atomically at create via
// CreateOpts.Tags and filterable server-side through the list API's tag
// parameter). Every created image gets a deterministic dizzy-<id>-<logical> name
// so a run can be identified and, later, cleaned up by that identity. Each call
// is timed through a metrics.Collector, and images can be polled until active.
package glance

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

// KindImage is the Glance resource kind. It doubles as the metrics "type" label
// and the tag value written under dizzy:type.
const KindImage resource.Kind = "image"

// Glance image status strings the readiness polls branch on. An image is ready
// when active; killed is a terminal upload failure; deleted and pending_delete
// are gone states (the latter appears on deployments configured with
// delayed_delete).
const (
	statusImageActive        = "active"
	statusImageKilled        = "killed"
	statusImageDeleted       = "deleted"
	statusImagePendingDelete = "pending_delete"
)

// Tag keys binding a created image to a tester run and its kind. They mirror the
// dizzy:run / dizzy:type conventions the other services use; Glance carries them
// as first-class image tags rather than Neutron tags or resource metadata.
const (
	metaRun  = "dizzy:run"
	metaType = "dizzy:type"
)

// ErrQuota is the sentinel a create wrapper wraps when Glance rejects the
// request because a limit was exceeded. The executor matches it with errors.Is
// to fail the run fast instead of retrying.
var ErrQuota = errors.New("glance: quota exceeded")

// Client wraps the authenticated image (Glance) service client an image run
// drives, binding every created image to a run id and recording timing into a
// Collector.
type Client struct {
	image   *gophercloud.ServiceClient
	runID   string
	metrics *metrics.Collector
	tel     *telemetry.Telemetry
}

// New returns a Client that names and stamps images for runID and records timing
// into m.
func New(image *gophercloud.ServiceClient, runID string, m *metrics.Collector) *Client {
	return &Client{image: image, runID: runID, metrics: m}
}

// SetTelemetry attaches an OTEL export seam so each timed call and readiness poll
// is recorded live alongside the in-memory collector, which stays the source of
// truth for run records and reports. A nil t (the default after New) leaves
// export disabled: every telemetry call is a no-op.
func (c *Client) SetTelemetry(t *telemetry.Telemetry) {
	c.tel = t
}

// resourceName builds the deterministic cloud name for a logical plan name.
func resourceName(runID, logical string) string {
	return "dizzy-" + runID + "-" + logical
}

// runTags returns the Glance image tags applied to every created image: the run
// identifier and the image type. Discovery for cleanup filters on the run tag;
// the type tag backs the monitor loop's orphan sweep.
func runTags(runID string) []string {
	return []string{metaRun + "=" + runID, metaType + "=" + string(KindImage)}
}

// timed runs fn, records a Sample for the attempt (including the error
// classification extracted from any gophercloud error), mirrors the same
// measurement live into telemetry under the low-cardinality op label, and
// returns fn's error unchanged. The in-memory collector stays the source of
// truth for run records and reports; the telemetry record is additive and a
// no-op when export is disabled.
func (c *Client) timed(ctx context.Context, op string, fn func(context.Context) error) error {
	start := time.Now()
	err := fn(ctx)
	d := time.Since(start)
	ek := errKind(err)
	c.metrics.Record(metrics.Sample{
		Type:     string(KindImage),
		Duration: d,
		Success:  err == nil,
		ErrKind:  ek,
	})
	c.tel.RecordOperation(ctx, string(KindImage), op, d, ek)
	return err
}

// wrapCreate adds operation context to a create error and, when the error is a
// quota rejection, threads ErrQuota into the chain so the executor can fail fast
// while preserving the underlying gophercloud error for classification.
func wrapCreate(logical string, err error) error {
	if isQuota(err) {
		return fmt.Errorf("creating image %q: %w: %w", logical, ErrQuota, err)
	}
	return fmt.Errorf("creating image %q: %w", logical, err)
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
		// report any 3-digit code, so bound the label to the codes the image API
		// itself returns and collapse the rest to http_other. This keeps error.kind
		// a bounded enum like every other metric label.
		switch s {
		case 400, 401, 403, 404, 409, 413, 429, 500, 502, 503, 504:
			return fmt.Sprintf("http_%d", s)
		default:
			return "http_other"
		}
	}
	return "other"
}

// IsRetryable reports whether err is a transient failure worth retrying: a 5xx,
// a 429 rate-limit, or a 409 conflict that is not a quota rejection.
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

// IsNotFound reports whether err is a 404. Status and cleanup use it to treat an
// image that is already gone as success rather than an error, which is what
// makes cleanup idempotent.
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

// isQuota reports whether err is a limit rejection. Glance signals an
// over-the-limit request (image count, member, or tag limits) with HTTP 413
// (Request Entity Too Large); a 403 or 400 whose body mentions a quota or limit
// is also treated as one, since body phrasing varies across releases.
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
		strings.Contains(body, "limit") ||
		strings.Contains(body, "exceeds allowed")
}
