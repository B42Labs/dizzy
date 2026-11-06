// Package octavia wraps the gophercloud v2 load-balancer (Octavia) and
// networking (Neutron) calls used to apply an Octavia plan against a real
// cloud. It is the ports-and-adapters seam to OpenStack for load balancing:
// dizzy creates the VIP networks and their subnets (tagged through Neutron's
// tag extension), then the load balancers, TCP listeners, default pools,
// synthetic members, and TCP health monitors, each carrying the identity
// dizzy:run=<id> / dizzy:type=<kind> as Octavia tags in its create request.
// Every created resource gets a deterministic dizzy-<id>-<logical> name, so a
// run can be identified and, later, cleaned up by that identity: cleanup
// deletes a load balancer with cascade, which removes its children with it.
// Octavia's children collide on a repeated create, so a create whose answer was
// lost is looked up by name and run tag and adopted rather than posted again.
// Each call is timed through a metrics.Collector, and the wait for a load
// balancer to return to ACTIVE after a write is recorded as the time-to-ready
// of the kind just written.
package octavia

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// Octavia resource kinds. They double as the metrics "type" label and the tag
// value written under dizzy:type. The VIP networks and subnets carry the
// identity as Neutron tags; the Octavia objects carry it as Octavia tags.
const (
	KindNetwork       resource.Kind = "network"
	KindSubnet        resource.Kind = "subnet"
	KindLoadBalancer  resource.Kind = "loadbalancer"
	KindListener      resource.Kind = "listener"
	KindPool          resource.Kind = "pool"
	KindMember        resource.Kind = "member"
	KindHealthMonitor resource.Kind = "healthmonitor"
)

// Provisioning status strings the waits and lookups branch on. A load balancer
// accepts the next write at ACTIVE; ERROR is a terminal provisioning failure.
// An object in PENDING_DELETE or DELETED is on its way out, so a lookup for a
// lost create never adopts it, and a deletion wait counts DELETED as gone.
// statusNetworkActive is the Neutron status of a network that is ready.
const (
	statusActive        = "ACTIVE"
	statusError         = "ERROR"
	statusPendingDelete = "PENDING_DELETE"
	statusDeleted       = "DELETED"
	statusNetworkActive = "ACTIVE"
)

// Tag keys binding a created resource to a tester run and its kind. They
// mirror the Neutron convention the other services use.
const (
	metaRun  = "dizzy:run"
	metaType = "dizzy:type"
)

// providerOVN is the Octavia provider name of the OVN driver, the one provider
// that does not support ROUND_ROBIN.
const providerOVN = "ovn"

// ErrQuota is the sentinel a create wrapper wraps when Octavia or Neutron
// rejects the request because a quota was exceeded. The executor matches it
// with errors.Is to fail the run fast instead of retrying.
var ErrQuota = errors.New("octavia: quota exceeded")

// ErrProvisioning is the sentinel WaitForActive wraps when a load balancer
// reached ERROR after a write. It is never retryable: the provider failed to
// apply the change, and the load balancer refuses every further write.
var ErrProvisioning = errors.New("octavia: provisioning failed")

// Client wraps the two authenticated service clients an Octavia run drives —
// load balancer (Octavia) and network (Neutron) — binding every created
// resource to a run id and recording timing into a Collector.
type Client struct {
	lb      *gophercloud.ServiceClient
	network *gophercloud.ServiceClient
	runID   string
	metrics *metrics.Collector
	tel     *telemetry.Telemetry

	// mu guards pending, because the executor creates from several goroutines
	// at once.
	mu sync.Mutex
	// pending holds, under string(kind) + "/" + the cloud name, the creates whose
	// answer was lost and whose object a lookup did not find yet. The next create
	// of the name takes the entry and looks the name up before it posts.
	pending map[string]struct{}
}

// New returns a Client that names and tags resources for runID and records
// timing into m. lb drives the Octavia objects, network the VIP networks and
// their subnets.
func New(lb, network *gophercloud.ServiceClient, runID string, m *metrics.Collector) *Client {
	return &Client{
		lb:      lb,
		network: network,
		runID:   runID,
		metrics: m,
		pending: map[string]struct{}{},
	}
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

// runTag returns the dizzy:run=<runID> tag every resource of the run carries,
// the tag discovery and the lost-create lookups filter on.
func runTag(runID string) string {
	return metaRun + "=" + runID
}

// runTags returns the tags applied to every created resource of the given
// kind: the run identifier and the resource type.
func runTags(runID string, kind resource.Kind) []string {
	return []string{runTag(runID), metaType + "=" + string(kind)}
}

// PoolAlgorithm returns the pool load-balancing algorithm a load balancer of
// provider supports: SOURCE_IP_PORT for the OVN provider, which supports no
// other, and ROUND_ROBIN for amphora, the cloud's default provider (an empty
// name), and any other provider.
func PoolAlgorithm(provider string) string {
	if provider == providerOVN {
		return string(pools.LBMethodSourceIpPort)
	}
	return string(pools.LBMethodRoundRobin)
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
	c.record(ctx, typ, op, time.Since(start), err)
	return err
}

// record adds one sample for a call of typ and op that took d and ended
// with err to the collector, and mirrors it into telemetry under op. timed
// records through it, and so does the load balancer wait for the one poll that
// found the load balancer in ERROR.
func (c *Client) record(ctx context.Context, typ, op string, d time.Duration, err error) {
	ek := errKind(err)
	c.metrics.Record(metrics.Sample{
		Type:     typ,
		Duration: d,
		Success:  err == nil,
		ErrKind:  ek,
	})
	c.tel.RecordOperation(ctx, typ, op, d, ek)
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

// mayHaveLanded reports whether a create that failed with err may still have
// created its resource. gophercloud's input errors fail before the request is
// sent, and a status below 500 is the service refusing the request, a quota
// rejection included, so none of these created anything. A 5xx can come after
// the service committed, and an error without a status (a lost connection, a
// deadline, a cancel) leaves unknown whether the request arrived, so both may
// have landed. IsRetryable stays apart from this: retrying a create without a
// lookup is what collides with the object the first request created.
func mayHaveLanded(err error) bool {
	if err == nil {
		return false
	}
	var missing gophercloud.ErrMissingInput
	var invalid gophercloud.ErrInvalidInput
	if errors.As(err, &missing) || errors.As(err, &invalid) {
		return false
	}
	if s := httpStatus(err); s != 0 {
		return s >= 500
	}
	return true
}

// errKind classifies an error into a stable, low-cardinality label for the
// metrics error breakdown. It returns the empty string for a nil error. A load
// balancer that reached ERROR is provisioning_error, so it does not read as
// other.
func errKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrProvisioning):
		return "provisioning_error"
	case isQuota(err):
		return "quota"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	if s := httpStatus(err); s != 0 {
		// The status is the peer's: an endpoint behind a proxy, LB, or WAF can
		// report any 3-digit code, so bound the label to the codes the
		// load-balancer and network APIs themselves return and collapse the rest
		// to http_other. This keeps error.kind a bounded enum like every other
		// metric label.
		switch s {
		case 400, 401, 403, 404, 409, 429, 500, 502, 503, 504:
			return fmt.Sprintf("http_%d", s)
		default:
			return "http_other"
		}
	}
	return "other"
}

// IsRetryable reports whether err is a transient failure worth retrying: a 5xx,
// a 429 rate-limit, or a 409 conflict that is not a quota rejection. A 409 is
// also what a load balancer that is not ACTIVE answers a write with. A load
// balancer that reached ERROR is never retryable.
func IsRetryable(err error) bool {
	if err == nil || isQuota(err) || errors.Is(err, ErrProvisioning) {
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

// isQuota reports whether err is a quota rejection. Octavia signals an exceeded
// quota with HTTP 403 Forbidden "Quota has been met", and the Neutron companion
// that backs the VIP networks and subnets uses 409 Conflict; a 400 whose body
// mentions a quota is also treated as one, since body phrasing varies across
// releases. Each is told apart from a policy denial or an ordinary lifecycle
// conflict by the quota mention in the body, so a 403 without one stays a
// denial and a state-transition 409 stays retryable.
func isQuota(err error) bool {
	var code gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &code) {
		return false
	}
	if code.Actual != 403 && code.Actual != 400 && code.Actual != 409 {
		return false
	}
	body := strings.ToLower(string(code.Body))
	return strings.Contains(body, "overlimit") ||
		strings.Contains(body, "quota") ||
		strings.Contains(body, "exceeds allowed")
}

// pollInitial and pollMax bound the wait between two polls of a readiness or
// deletion wait: it starts at pollInitial and grows by half after each poll up
// to pollMax, as Nova's readiness polls do. They are variables so tests can
// shrink them.
var (
	pollInitial = 200 * time.Millisecond
	pollMax     = 2 * time.Second
)

// nextBackoff returns the wait after one of backoff: half as long again,
// capped at pollMax.
func nextBackoff(backoff time.Duration) time.Duration {
	if backoff = time.Duration(float64(backoff) * 1.5); backoff > pollMax {
		return pollMax
	}
	return backoff
}
