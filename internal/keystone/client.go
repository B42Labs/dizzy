// Package keystone wraps the gophercloud v2 identity (Keystone v3) calls used to
// apply a Keystone plan against a real cloud. It is the ports-and-adapters seam
// to OpenStack: every created resource is given a deterministic
// ostester-<id>-<logical> name so a run can be identified and, later, cleaned up
// by that name prefix. Projects additionally carry ostester:run=<id> /
// ostester:type=project tags for a server-side filter, since projects are the
// only identity kind that supports tags. Each call is timed through a
// metrics.Collector and, when enabled, mirrored into OTEL. Unlike Neutron ports
// and Cinder volumes, Keystone creates and deletes are synchronous — the
// resource is usable on return and gone after delete — so there is no
// status-polling / time-to-ready stage.
package keystone

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// Keystone resource kinds. They double as the metrics "type" label; KindToken is
// also recorded as a time-to-ready kind (the token-issue latency). Assignments
// and tokens carry no cloud name or tag — they live only in the run record.
const (
	KindDomain     resource.Kind = "domain"
	KindProject    resource.Kind = "project"
	KindUser       resource.Kind = "user"
	KindRole       resource.Kind = "role"
	KindAssignment resource.Kind = "role_assignment"
	KindToken      resource.Kind = "token"
)

// namePrefix is the common prefix of every resource this tool creates. A run's
// resources are named namePrefix + runID + "-" + logical, so the run prefix
// uniquely identifies one run while namePrefix alone matches any tester run (the
// handle the monitor pre-flight sweep reclaims by).
const namePrefix = "ostester-"

// Project tag keys binding a created project to a tester run and its kind.
// Keystone project tags are plain strings, so the key=value shape mirrors
// Neutron's tags; Keystone forbids only "/" and "," in tags, and neither of
// these values contains either.
const (
	tagRun  = "ostester:run="
	tagType = "ostester:type="
)

// ErrForbidden is the sentinel a create/assign/issue wrapper wraps when Keystone
// rejects a write with HTTP 403 (a policy denial). The executor matches it with
// errors.Is to fail the run fast instead of retrying, the identity analog of
// Cinder's ErrQuota — the backstop behind the privilege pre-check.
var ErrForbidden = errors.New("keystone: forbidden")

// Client wraps an authenticated IdentityV3 service client, binding every created
// resource to a run id and recording timing into a Collector.
type Client struct {
	gc      *gophercloud.ServiceClient
	runID   string
	metrics *metrics.Collector
	tel     *telemetry.Telemetry
}

// New returns a Client that names resources for runID and records timing into m.
func New(gc *gophercloud.ServiceClient, runID string, m *metrics.Collector) *Client {
	return &Client{gc: gc, runID: runID, metrics: m}
}

// SetTelemetry attaches an OTEL export seam so each timed call is recorded live
// alongside the in-memory collector, which stays the source of truth for run
// records and reports. A nil t (the default after New) leaves export disabled:
// every telemetry call is a no-op.
func (c *Client) SetTelemetry(t *telemetry.Telemetry) {
	c.tel = t
}

// resourceName builds the deterministic cloud name for a logical plan name.
func resourceName(runID, logical string) string {
	return namePrefix + runID + "-" + logical
}

// runPrefix is the name prefix shared by every resource of one run: the reliable
// handle for client-side discovery of a run's domains, users, and roles, which
// carry no tags.
func runPrefix(runID string) string {
	return namePrefix + runID + "-"
}

// HasNamePrefix reports whether name carries the ostester- prefix every resource
// this tool creates. Cleanup uses it as a belt-and-suspenders guard so a reused
// domain or role (which carries no prefix) is never disabled or deleted in
// domain-manager mode.
func HasNamePrefix(name string) bool {
	return strings.HasPrefix(name, namePrefix)
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

// wrapCreate adds operation context to a create/assign/issue error and, when the
// error is a 403 policy denial, threads ErrForbidden into the chain so the
// executor can fail fast while preserving the underlying gophercloud error for
// classification.
func wrapCreate(kind resource.Kind, logical string, err error) error {
	if gophercloud.ResponseCodeIs(err, 403) {
		return fmt.Errorf("creating %s %q: %w: %w", kind, logical, ErrForbidden, err)
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
// metrics error breakdown. It returns the empty string for a nil error. Keystone
// has no quota, so there is no quota branch: a 403 stays http_403 (the privilege
// backstop reads it via ErrForbidden, not this label).
func errKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	if s := httpStatus(err); s != 0 {
		// The status is the peer's: an endpoint behind a proxy, LB, or WAF can
		// report any 3-digit code, so bound the label to the codes Keystone itself
		// returns and collapse the rest to http_other, keeping error.kind a bounded
		// enum like every other label.
		switch s {
		case 400, 401, 403, 404, 409, 429, 500, 502, 503, 504:
			return fmt.Sprintf("http_%d", s)
		default:
			return "http_other"
		}
	}
	return "other"
}

// IsRetryable reports whether err is a transient Keystone failure worth
// retrying: a 5xx or a 429 rate-limit. Unlike Cinder, a 409 conflict is NOT
// retryable — Keystone names are unique within their scope, so a conflict is a
// terminal duplicate rather than a transient in-use lock.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	for _, code := range []int{500, 502, 503, 504, 429} {
		if gophercloud.ResponseCodeIs(err, code) {
			return true
		}
	}
	return false
}

// IsForbidden reports whether err is (or wraps) a Keystone 403 policy denial.
// The executor uses it to fail fast; discovery listings use it to fail open.
func IsForbidden(err error) bool {
	return errors.Is(err, ErrForbidden) || gophercloud.ResponseCodeIs(err, 403)
}

// IsNotFound reports whether err is a Keystone 404. Status and cleanup use it to
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

// RandomPassword returns a fresh, cryptographically random password for a test
// user. A password is generated once per user at create time and held only in
// memory for the token-issue step — never written to the plan or run record. It
// is deliberately NOT derived from the run id, the user's logical name, or a
// scenario seed: all three are broadcast in the cloud-visible username, and the
// seed is low-entropy, so a derived password would be recomputable by anyone who
// can list a leftover user in a shared domain.
func RandomPassword() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
