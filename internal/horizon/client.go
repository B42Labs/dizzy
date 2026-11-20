// Package horizon drives the Horizon dashboard the way a browser does and
// creates the Keystone users and projects a run logs in as. A Session holds
// its own cookie jar and its own connections, loads the login form, posts it
// with the form's CSRF token, loads pages, switches project and logs out,
// and checks every response against what Horizon sends on success. No
// request follows a redirect, so each check sees the status Horizon sent.
// The Keystone side (identity.go) names every created user and project
// dizzyhorizon-<runid>-<logical> and tags projects dizzyhorizon:run=<runid>
// and dizzyhorizon:type=project. The prefix and the tags differ from the
// dizzy- of the keystone namespace, so its orphan sweep never reaches a
// Horizon run's users or projects.
//
// Each request and each Keystone call is timed through a metrics.Collector
// and, when enabled, mirrored into OTEL, as one sample of its kind. No
// cookie, CSRF token or password ever appears in an error text or a log
// line.
package horizon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// The Horizon resource kinds. They double as the metrics "kind" label. The
// session kinds name the steps of a journey; KindSession is also the
// time-to-ready kind of a whole login and the kind of a churned session.
// The Keystone kinds name what a run creates.
const (
	KindLoginPage      resource.Kind = "login_page"
	KindLogin          resource.Kind = "login"
	KindSwitch         resource.Kind = "switch"
	KindSwitchLanding  resource.Kind = "switch_landing"
	KindLogout         resource.Kind = "logout"
	KindSession        resource.Kind = "session"
	KindProject        resource.Kind = "project"
	KindUser           resource.Kind = "user"
	KindRoleAssignment resource.Kind = "role_assignment"
)

// PageKind is the metric kind of the page named name: page_<name>.
func PageKind(name string) resource.Kind {
	return resource.Kind("page_" + name)
}

// ErrRejected is the sentinel a login wraps when Horizon renders the login
// form again instead of redirecting, which is how it answers wrong
// credentials. It is never retryable.
var ErrRejected = errors.New("horizon: login rejected")

// ErrMismatch is the sentinel a check wraps when Horizon answered with the
// expected status but not with what that status promises: a login form
// without its CSRF token or region, a login that redirects into /auth/ or
// sets no session cookie, a switch that lands elsewhere or in another
// project, a logout without a CSRF cookie or one that does not lead back to
// the login page. It is never retryable.
var ErrMismatch = errors.New("horizon: mismatch")

// ErrForbidden is the sentinel a Keystone create wraps, together with the
// 403, when the clouds.yaml entry may not create projects, users or role
// assignments. It is never retryable.
var ErrForbidden = errors.New("horizon: forbidden")

// maxBody bounds every response body the client reads, parsed or drained.
const maxBody = 4 << 20

// Client checks one Horizon dashboard for one run and, with an identity
// client, creates and deletes the run's Keystone users and projects. It
// records every request into a Collector.
type Client struct {
	base      *url.URL
	tlsConfig *tls.Config
	runID     string
	metrics   *metrics.Collector
	tel       *telemetry.Telemetry
	identity  *gophercloud.ServiceClient
	userAgent string
}

// New returns a Client for the dashboard at base, whose path (Horizon's
// WEBROOT, empty or e.g. /dashboard, without a trailing slash) prefixes
// every request. tlsConfig is the TLS configuration of every session's
// connections; nil means Go's defaults. Created Keystone resources are named
// for runID, and every request is recorded into m.
func New(base *url.URL, tlsConfig *tls.Config, runID string, m *metrics.Collector) *Client {
	return &Client{base: base, tlsConfig: tlsConfig, runID: runID, metrics: m, userAgent: "dizzy"}
}

// SetTelemetry attaches an OTEL export seam so each request is recorded live
// alongside the in-memory collector. A nil t (the default after New) leaves
// export disabled.
func (c *Client) SetTelemetry(t *telemetry.Telemetry) {
	c.tel = t
}

// SetIdentity sets the IdentityV3 client the Keystone side creates, lists
// and deletes users and projects through.
func (c *Client) SetIdentity(gc *gophercloud.ServiceClient) {
	c.identity = gc
}

// SetUserAgent sets the User-Agent every Horizon request sends; the default
// after New is "dizzy".
func (c *Client) SetUserAgent(ua string) {
	c.userAgent = ua
}

// basePath returns the path every request is prefixed with.
func (c *Client) basePath() string {
	return c.base.Path
}

// url returns the dashboard URL of path, a path below the base that starts
// with a slash. escapedPath is path as it goes on the wire.
func (c *Client) url(path, escapedPath string) *url.URL {
	u := *c.base
	u.Path = c.base.Path + path
	u.RawPath = c.base.EscapedPath() + escapedPath
	u.RawQuery = ""
	u.Fragment = ""
	return &u
}

// timed runs fn, records one sample of kind for it, classifying its error,
// mirrors it into telemetry under op, and returns fn's error unchanged.
func (c *Client) timed(ctx context.Context, kind resource.Kind, op string, fn func(context.Context) error) error {
	start := time.Now()
	err := fn(ctx)
	c.record(ctx, kind, op, time.Since(start), err)
	return err
}

// record adds one sample of kind and its live operation record.
func (c *Client) record(ctx context.Context, kind resource.Kind, op string, d time.Duration, err error) {
	ek := errKind(err)
	c.metrics.Record(metrics.Sample{Type: string(kind), Duration: d, Success: err == nil, ErrKind: ek})
	c.tel.RecordOperation(ctx, string(kind), op, d, ek)
}

// recordReady records one time-to-ready sample of kind into the collector
// and telemetry.
func (c *Client) recordReady(ctx context.Context, kind resource.Kind, d time.Duration, ok bool) {
	c.metrics.RecordReadiness(metrics.Readiness{Type: string(kind), Duration: d, OK: ok})
	c.tel.RecordTimeToReady(ctx, string(kind), d, ok)
}

// StatusError is a Horizon response whose status is not the one the step
// expects. Step names the step and Location the path a redirect pointed at.
type StatusError struct {
	Step     string
	Status   int
	Location string
}

// Error names the step, the status and, for a redirect, where it pointed.
func (e *StatusError) Error() string {
	if e.Location != "" {
		return fmt.Sprintf("%s: unexpected status %d, redirect to %s", e.Step, e.Status, e.Location)
	}
	return fmt.Sprintf("%s: unexpected status %d", e.Step, e.Status)
}

// httpStatus returns the HTTP status a Horizon or Keystone error carries, or
// 0 when it carries none.
func httpStatus(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	var code gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &code) {
		return code.GetStatusCode()
	}
	return 0
}

// isSentinel reports whether err wraps one of the package's sentinels.
func isSentinel(err error) bool {
	return errors.Is(err, ErrRejected) || errors.Is(err, ErrMismatch) || errors.Is(err, ErrForbidden)
}

// errKind classifies an error into a stable, low-cardinality label for the
// metrics error breakdown: "" for nil, rejected, mismatch, http_403 for a
// forbidden Keystone create, timeout, canceled, http_<status> for the
// statuses Horizon and Keystone answer with, http_other for any other
// status, and other for everything else, a refused connection included.
func errKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrRejected):
		return "rejected"
	case errors.Is(err, ErrMismatch):
		return "mismatch"
	case errors.Is(err, ErrForbidden):
		return "http_403"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	if s := httpStatus(err); s != 0 {
		switch s {
		case 302, 400, 401, 403, 404, 405, 409, 429, 500, 502, 503, 504:
			return fmt.Sprintf("http_%d", s)
		default:
			return "http_other"
		}
	}
	return "other"
}

// ErrorKind is the error label the client's samples carry, for a caller
// outside the package that labels the same errors.
func ErrorKind(err error) string { return errKind(err) }

// IsRetryable reports whether err is a transient failure worth retrying: a
// 429 or a 500, 502, 503 or 504 from Horizon or Keystone, or a transport
// error that is no context error, such as a refused or reset connection. A
// sentinel is never retryable.
func IsRetryable(err error) bool {
	if err == nil || isSentinel(err) {
		return false
	}
	switch httpStatus(err) {
	case 429, 500, 502, 503, 504:
		return true
	case 0:
	default:
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ue *url.Error
	var ne net.Error
	return errors.As(err, &ue) || errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF)
}

// IsNotFound reports whether err is a Keystone 404, which delete and observe
// treat as gone.
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
