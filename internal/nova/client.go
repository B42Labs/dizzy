// Package nova wraps the gophercloud v2 compute, networking, and block-storage
// calls used to apply a Nova plan against a real cloud. It is the
// ports-and-adapters seam to OpenStack for compute: dizzy itself creates the
// companion networks, subnets, and ports (tagged with dizzy:run=<id> /
// dizzy:type=<kind>, the Neutron convention) and the data volumes (stamped with
// the same keys as Cinder metadata), and boots servers carrying that identity as
// server metadata. Images and flavors are referenced by name only — dizzy
// uploads no image and creates no flavor, since both would need rights the tool
// does not assume. Every created resource gets a deterministic dizzy-<id>-
// <logical> name so a run can be identified and, later, cleaned up by that
// identity. Each call is timed through a metrics.Collector, and status-bearing
// resources can be polled until ready.
package nova

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

// Nova resource kinds. They double as the metrics "type" label and the
// tag/metadata value written under dizzy:type. Servers and volumes carry the
// identity as metadata; networks, subnets, and ports carry it as Neutron tags.
const (
	KindServer  resource.Kind = "server"
	KindNetwork resource.Kind = "network"
	KindSubnet  resource.Kind = "subnet"
	KindPort    resource.Kind = "port"
	KindVolume  resource.Kind = "volume"
)

// Compute server status strings the readiness polls branch on. A server is
// ready when ACTIVE; SHUTOFF is an intermediate state the stop/start sequence
// waits for; ERROR is a terminal failure.
const (
	statusServerActive  = "ACTIVE"
	statusServerShutoff = "SHUTOFF"
	statusServerError   = "ERROR"
)

// Block-storage status strings a data volume reaches. A volume is ready when
// available; a status with the error prefix is a terminal failure.
const (
	statusVolumeAvailable = "available"
	statusVolumeErrPrefix = "error"
)

// Metadata/tag keys binding a created resource to a tester run and its kind.
// They mirror the Neutron and Cinder conventions already in use.
const (
	metaRun  = "dizzy:run"
	metaType = "dizzy:type"
)

// ErrQuota is the sentinel a create wrapper wraps when Nova rejects the request
// because a quota was exceeded. The executor matches it with errors.Is to fail
// the run fast instead of retrying.
var ErrQuota = errors.New("nova: quota exceeded")

// Client wraps the three authenticated service clients a compute run drives —
// compute (Nova), network (Neutron), and block storage (Cinder) — binding every
// created resource to a run id and recording timing into a Collector.
type Client struct {
	compute      *gophercloud.ServiceClient
	network      *gophercloud.ServiceClient
	blockstorage *gophercloud.ServiceClient
	runID        string
	metrics      *metrics.Collector
	tel          *telemetry.Telemetry
}

// New returns a Client that names and stamps resources for runID and records
// timing into m. compute drives servers and their attachments, network the
// companion networks/subnets/ports, and blockstorage the data volumes.
func New(compute, network, blockstorage *gophercloud.ServiceClient, runID string, m *metrics.Collector) *Client {
	return &Client{
		compute:      compute,
		network:      network,
		blockstorage: blockstorage,
		runID:        runID,
		metrics:      m,
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

// runMetadata returns the metadata stamped on every created resource of the
// given kind: the run identifier and the resource type. Discovery for cleanup
// filters on the run key. Servers and volumes use it directly; networks,
// subnets, and ports carry the same pair as Neutron tags.
func runMetadata(runID string, kind resource.Kind) map[string]string {
	return map[string]string{
		metaRun:  runID,
		metaType: string(kind),
	}
}

// runTags returns the Neutron tags applied to every created network, subnet, or
// port of the given kind: the run identifier and the resource type.
func runTags(runID string, kind resource.Kind) []string {
	return []string{metaRun + "=" + runID, metaType + "=" + string(kind)}
}

// metadataMatches reports whether md carries every key/value pair in filter. It
// is the client-side backstop the metadata list paths apply, so a server that
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
		// report any 3-digit code, so bound the label to the codes the compute,
		// network, and block-storage APIs themselves return and collapse the rest
		// to http_other. This keeps error.kind a bounded enum like every other
		// metric label.
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

// isQuota reports whether err is a quota rejection. Nova signals over-quota with
// HTTP 403 Forbidden whose body mentions a quota, the block-storage companion
// uses 413 (OverLimit), and the Neutron companion that backs the created
// networks, subnets, and ports uses 409 Conflict; a 400 whose body mentions a
// quota is also treated as one, since body phrasing varies across releases. The
// 409 is disambiguated from an ordinary lifecycle conflict by the quota mention
// in the body, so a state-transition 409 stays retryable.
func isQuota(err error) bool {
	var code gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &code) {
		return false
	}
	if code.Actual == 413 {
		return true
	}
	if code.Actual != 403 && code.Actual != 400 && code.Actual != 409 {
		return false
	}
	body := strings.ToLower(string(code.Body))
	return strings.Contains(body, "overlimit") ||
		strings.Contains(body, "quota") ||
		strings.Contains(body, "exceeds allowed")
}

// isErrorStatus reports whether a block-storage status is a terminal error state
// (any status beginning with "error": error, error_attaching, error_detaching).
func isErrorStatus(status string) bool {
	return strings.HasPrefix(status, statusVolumeErrPrefix)
}
