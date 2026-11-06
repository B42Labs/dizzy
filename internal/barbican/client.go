// Package barbican wraps the gophercloud v2 key manager (Barbican) v1 calls used
// to apply a Barbican plan against a real cloud. It is the ports-and-adapters
// seam to OpenStack for secrets: dizzy stores opaque secrets whose synthetic
// payload is derived through internal/payload from the plan seed and the
// secret's logical name, submits AES-256 key orders and polls each until
// Barbican has generated its key, groups stored secrets into generic
// containers, and reads stored payloads back to byte-compare them with the
// bytes it stored. Every created resource gets a deterministic
// dizzy-<id>-<logical> name (an order carries it as meta.name, which Barbican
// also gives the secret it generates). Barbican has no tags and no server-side
// prefix filter, so a run's resources are discovered client side by that name
// prefix, and a secret is confirmed by its dizzy_run=<id> metadata or as the
// secret_ref of a discovered order. The monitor's pre-flight sweep discovers
// any tester run's resources by namePrefix alone instead, confirming a secret
// by its dizzy_type=secret metadata. StampSecret writes dizzy_run and
// dizzy_type=secret onto a stored secret after its store and onto a generated
// secret once its order turns ACTIVE. Each call is timed through a
// metrics.Collector and, when enabled, mirrored into OTEL. Barbican deletes are
// synchronous, so there is no wait for a resource to be gone.
package barbican

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/containers"
	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/orders"
	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/secrets"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// Barbican resource kinds. They double as the metrics "type" label; KindSecret
// is also the value written under dizzy_type, and KindOrder the time-to-ready
// kind of the order wait. A stored secret and an order's generated secret are
// both KindSecret.
const (
	KindSecret    resource.Kind = "secret"
	KindOrder     resource.Kind = "order"
	KindContainer resource.Kind = "container"
)

// Barbican order status strings the order wait branches on. An order is ready
// when ACTIVE; ERROR is a terminal generation failure.
const (
	statusActive = "ACTIVE"
	statusError  = "ERROR"
)

// contentTypeOctetStream is the payload content type of every stored secret and
// every order, and the Accept type of a read-back, so Barbican stores and
// returns the payload as raw bytes.
const contentTypeOctetStream = "application/octet-stream"

// listLimit is the page size of every listing. Barbican's default page is 10
// and its largest is 100, so a run's discovery takes a tenth of the requests.
const listLimit = 100

// namePrefix is the common prefix of every resource this tool creates. A run's
// resources are named namePrefix + runID + "-" + logical, so the run prefix
// uniquely identifies one run while namePrefix alone matches any tester run.
const namePrefix = "dizzy-"

// Secret metadata keys binding a stored or generated secret to a tester run and
// its kind. Barbican carries them as secret metadata, which orders and
// containers do not have. Its secret metadata validator rejects any key that
// does not match ^[A-Za-z0-9_-]*$ with HTTP 400 "Invalid Key. Key must be URL
// safe." and stores an accepted key lowercased, so the keys join their words
// with an underscore, not the colon the other services' identity keys use, and stay
// lowercase so the key discovery reads back is the key StampSecret wrote.
const (
	metaRun  = "dizzy_run"
	metaType = "dizzy_type"
)

// ErrQuota is the sentinel a create wrapper wraps when Barbican rejects the
// request because a project quota has been reached. The executor matches it
// with errors.Is to fail the run fast instead of retrying.
var ErrQuota = errors.New("barbican: quota exceeded")

// ErrPayloadMismatch is the sentinel a read-back wraps when Barbican returned
// bytes that differ from the payload the secret was stored with. It is never
// retryable: the stored data is wrong, not the call.
var ErrPayloadMismatch = errors.New("barbican: payload mismatch")

// ErrKindMismatch is the error-kind label an ErrPayloadMismatch carries in the
// metrics error breakdown and the churn engine's per-bucket errors. It is
// exported so the churn classifier and the chaos command's mismatch count name
// the one value errKind records, rather than each repeating the literal.
const ErrKindMismatch = "mismatch"

// ErrOrderFailed is the sentinel the order wait wraps when an order ended in
// ERROR, or ACTIVE without a usable secret reference. It is never retryable.
var ErrOrderFailed = errors.New("barbican: order failed")

// Client wraps the authenticated key manager (Barbican) service client a
// Barbican run drives, binding every created resource to a run id and recording
// timing into a Collector.
type Client struct {
	kv             *gophercloud.ServiceClient
	runID          string
	metrics        *metrics.Collector
	tel            *telemetry.Telemetry
	requestTimeout time.Duration
}

// New returns a Client that names and stamps resources for runID and records
// timing into m.
func New(kv *gophercloud.ServiceClient, runID string, m *metrics.Collector) *Client {
	return &Client{kv: kv, runID: runID, metrics: m}
}

// SetTelemetry attaches an OTEL export seam so each timed call and order wait is
// recorded live alongside the in-memory collector, which stays the source of
// truth for run records and reports. A nil t (the default after New) leaves
// export disabled: every telemetry call is a no-op.
func (c *Client) SetTelemetry(t *telemetry.Telemetry) {
	c.tel = t
}

// SetRequestTimeout bounds each discovery request on its own: the paged
// listing of one kind (secrets, orders, or containers) and each secret metadata
// read run under their own context.WithTimeout(d). Secret discovery issues one
// metadata read per prefix-matching secret, so its total time grows with the
// run; a bound per request keeps a wedged call from hanging cleanup without
// failing a large run's discovery as a whole. A d of zero (the default after
// New) adds no bound, leaving each request to the caller's context.
func (c *Client) SetRequestTimeout(d time.Duration) {
	c.requestTimeout = d
}

// requestContext returns ctx bounded by the request timeout SetRequestTimeout
// set, or ctx unchanged when none is set. The caller must call the returned
// cancel function once its request is done.
func (c *Client) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.requestTimeout > 0 {
		return context.WithTimeout(ctx, c.requestTimeout)
	}
	return ctx, func() {}
}

// resourceName builds the deterministic cloud name for a logical plan name.
func resourceName(runID, logical string) string {
	return namePrefix + runID + "-" + logical
}

// runPrefix is the name prefix shared by every resource of one run: the handle
// for client-side discovery of a run's secrets, orders, and containers, which
// carry no tags.
func runPrefix(runID string) string {
	return namePrefix + runID + "-"
}

// secretRef builds the href Barbican identifies a secret by, the form a
// container's secret_refs entry takes.
func (c *Client) secretRef(id string) string {
	return c.kv.ServiceURL("secrets", id)
}

// idFromRef returns the id a Barbican href ends in: the last segment of its
// path, ignoring a trailing slash. Barbican answers a create and lists its
// resources with such an href (secret_ref, order_ref, container_ref) rather than
// a bare id. It returns an error for an empty ref or one whose path has no
// segment.
func idFromRef(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("empty reference")
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("parsing reference %q: %w", ref, err)
	}
	p := strings.TrimRight(u.Path, "/")
	id := p[strings.LastIndex(p, "/")+1:]
	if id == "" {
		return "", fmt.Errorf("reference %q has no id segment", ref)
	}
	return id, nil
}

// timed runs fn, records a Sample of kind for the attempt (including the error
// classification extracted from any gophercloud error), mirrors the same
// measurement live into telemetry under the low-cardinality op label, and
// returns fn's error unchanged. The in-memory collector stays the source of
// truth for run records and reports; the telemetry record is additive and a
// no-op when export is disabled.
func (c *Client) timed(ctx context.Context, kind resource.Kind, op string, fn func(context.Context) error) error {
	start := time.Now()
	err := fn(ctx)
	c.record(ctx, kind, op, time.Since(start), err)
	return err
}

// record adds one sample of kind and its live operation record, classifying
// err. timed records through it, and so does the order wait for the one poll
// that found an order in ERROR.
func (c *Client) record(ctx context.Context, kind resource.Kind, op string, d time.Duration, err error) {
	ek := errKind(err)
	c.metrics.Record(metrics.Sample{
		Type:     string(kind),
		Duration: d,
		Success:  err == nil,
		ErrKind:  ek,
	})
	c.tel.RecordOperation(ctx, string(kind), op, d, ek)
}

// recordReady records one time-to-ready sample of kind into both the collector
// and telemetry.
func (c *Client) recordReady(ctx context.Context, kind resource.Kind, d time.Duration, ok bool) {
	c.metrics.RecordReadiness(metrics.Readiness{Type: string(kind), Duration: d, OK: ok})
	c.tel.RecordTimeToReady(ctx, string(kind), d, ok)
}

// wrapCreate adds operation context to a create error and, when the error is a
// quota rejection, threads ErrQuota into the chain so the executor can fail fast
// while preserving the underlying gophercloud error for classification.
func wrapCreate(kind resource.Kind, logical string, err error) error {
	if isQuota(err) {
		return fmt.Errorf("creating %s %q: %w: %w", kind, logical, ErrQuota, err)
	}
	return fmt.Errorf("creating %s %q: %w", kind, logical, err)
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
// metrics error breakdown. It returns the empty string for a nil error. A
// read-back that returned different bytes is mismatch and an order that ended
// in ERROR is order_error, so neither reads as other.
func errKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrPayloadMismatch):
		return ErrKindMismatch
	case errors.Is(err, ErrOrderFailed):
		return "order_error"
	case isQuota(err):
		return "quota"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	if s := httpStatus(err); s != 0 {
		// The status is the peer's: an endpoint behind a proxy, LB, or WAF can
		// report any 3-digit code, so bound the label to the codes the key manager
		// API itself returns and collapse the rest to http_other. This keeps
		// error.kind a bounded enum like every other metric label.
		switch s {
		case 400, 401, 403, 404, 409, 413, 415, 429, 500, 502, 503, 504:
			return fmt.Sprintf("http_%d", s)
		default:
			return "http_other"
		}
	}
	return "other"
}

// IsRetryable reports whether err is a transient failure worth retrying: a 5xx,
// a 429 rate-limit, or a 409 conflict that is not a quota rejection. A payload
// mismatch and a failed order are never retryable.
func IsRetryable(err error) bool {
	if err == nil || isQuota(err) || errors.Is(err, ErrPayloadMismatch) || errors.Is(err, ErrOrderFailed) {
		return false
	}
	for _, code := range []int{500, 502, 503, 504, 429, 409} {
		if gophercloud.ResponseCodeIs(err, code) {
			return true
		}
	}
	return false
}

// IsNotFound reports whether err is a 404. Status and cleanup use it to treat a
// resource that is already gone as success rather than an error, which is what
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

// isQuota reports whether err is a quota rejection. Barbican's QuotaReached
// answers HTTP 403 "Creation not allowed because a quota has been reached", so
// only a 403 whose body mentions a quota counts; a 403 without one is a policy
// denial. A 413 is not a quota either: it is Barbican's LimitExceeded for a
// payload above the deployment's secret size limit.
func isQuota(err error) bool {
	var code gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &code) || code.Actual != 403 {
		return false
	}
	return strings.Contains(strings.ToLower(string(code.Body)), "quota")
}

// Delete removes a secret, order, or container by id, switching on its kind and
// recording the call under that kind. A 404 surfaces to the caller, which treats
// an already-gone resource as success to keep cleanup idempotent. An unknown
// kind is refused without a request.
func (c *Client) Delete(ctx context.Context, r resource.Resource) error {
	var del func(context.Context) error
	switch r.Kind {
	case KindSecret:
		del = func(ctx context.Context) error { return secrets.Delete(ctx, c.kv, r.ID).ExtractErr() }
	case KindOrder:
		del = func(ctx context.Context) error { return orders.Delete(ctx, c.kv, r.ID).ExtractErr() }
	case KindContainer:
		del = func(ctx context.Context) error { return containers.Delete(ctx, c.kv, r.ID).ExtractErr() }
	default:
		return fmt.Errorf("unknown barbican resource kind %q", r.Kind)
	}
	return c.timed(ctx, r.Kind, "delete", del)
}

// Observe re-queries the live state of a created secret, order, or container,
// recording the call under its kind. It returns the resource's status (ACTIVE,
// PENDING, or ERROR), whether it still exists, and any error other than a 404.
// A 404 is reported as ("", false, nil) so a resource deleted out of band reads
// as gone rather than as a failure. The status command drives this over a run's
// resources.
func (c *Client) Observe(ctx context.Context, r resource.Resource) (status string, exists bool, err error) {
	var get func(context.Context) (string, error)
	switch r.Kind {
	case KindSecret:
		get = func(ctx context.Context) (string, error) {
			s, err := secrets.Get(ctx, c.kv, r.ID).Extract()
			if err != nil {
				return "", err
			}
			return s.Status, nil
		}
	case KindOrder:
		get = func(ctx context.Context) (string, error) {
			o, err := orders.Get(ctx, c.kv, r.ID).Extract()
			if err != nil {
				return "", err
			}
			return o.Status, nil
		}
	case KindContainer:
		get = func(ctx context.Context) (string, error) {
			ct, err := containers.Get(ctx, c.kv, r.ID).Extract()
			if err != nil {
				return "", err
			}
			return ct.Status, nil
		}
	default:
		return "", false, fmt.Errorf("unknown barbican resource kind %q", r.Kind)
	}

	err = c.timed(ctx, r.Kind, "get", func(ctx context.Context) error {
		s, getErr := get(ctx)
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
