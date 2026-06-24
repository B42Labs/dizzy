// Package neutron wraps the gophercloud v2 networking calls used to apply a
// plan against a real cloud. It is the ports-and-adapters seam to OpenStack:
// every created resource is given a deterministic ostester-<id>-<logical> name
// and tagged with ostester:run=<id> so a run can be identified and, later,
// cleaned up by tag. Each call is timed through a metrics.Collector, and
// status-bearing resources can be polled until ready.
package neutron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/attributestags"

	"github.com/B42Labs/openstack-tester/internal/metrics"
)

// Neutron status strings shared across resource kinds. Networks, routers, and
// ports all report readiness with these values.
const (
	statusActive = "ACTIVE"
	statusDown   = "DOWN"
)

// ErrQuota is the sentinel a create wrapper wraps when Neutron rejects the
// request because a quota was exceeded. The executor matches it with errors.Is
// to fail the run fast instead of retrying.
var ErrQuota = errors.New("neutron: quota exceeded")

// Kind names a Phase 1 Neutron resource type. It doubles as the metrics
// "type" label and the tag value written under ostester:type.
type Kind string

// The Phase 1 resource kinds, in dependency order.
const (
	KindAddressScope      Kind = "address-scope"
	KindSubnetPool        Kind = "subnet-pool"
	KindNetwork           Kind = "network"
	KindSubnet            Kind = "subnet"
	KindRouter            Kind = "router"
	KindRouterInterface   Kind = "router-interface"
	KindSecurityGroup     Kind = "security-group"
	KindSecurityGroupRule Kind = "security-group-rule"
	KindPort              Kind = "port"
)

// Resource is the cloud identity of a created resource. Logical is the plan's
// reference name (e.g. "net-0001"); Name is the applied cloud name; ID is the
// Neutron UUID. The executor collects these and a later cleanup consumes them.
type Resource struct {
	Kind    Kind   `json:"kind"`
	Logical string `json:"logical"`
	Name    string `json:"name"`
	ID      string `json:"id"`
}

// Client wraps an authenticated NetworkV2 service client, binding every created
// resource to a run id and recording timing into a Collector.
type Client struct {
	gc      *gophercloud.ServiceClient
	runID   string
	metrics *metrics.Collector
}

// New returns a Client that names and tags resources for runID and records
// timing into m.
func New(gc *gophercloud.ServiceClient, runID string, m *metrics.Collector) *Client {
	return &Client{gc: gc, runID: runID, metrics: m}
}

// resourceName builds the deterministic cloud name for a logical plan name.
func resourceName(runID, logical string) string {
	return "ostester-" + runID + "-" + logical
}

// runTags returns the tags applied to every created resource of the given kind:
// the run identifier and the resource type.
func runTags(runID string, kind Kind) []string {
	return []string{"ostester:run=" + runID, "ostester:type=" + string(kind)}
}

// tagCollection maps a Kind to the Neutron tag-extension resource collection
// path used to tag it.
func tagCollection(kind Kind) string {
	switch kind {
	case KindNetwork:
		return "networks"
	case KindSubnet:
		return "subnets"
	case KindSubnetPool:
		return "subnetpools"
	case KindRouter:
		return "routers"
	case KindPort:
		return "ports"
	case KindSecurityGroup:
		return "security-groups"
	case KindAddressScope:
		return "address-scopes"
	default:
		return ""
	}
}

// timed runs fn, records a Sample for the attempt (including the error
// classification extracted from any gophercloud error), and returns fn's error
// unchanged.
func (c *Client) timed(ctx context.Context, typ string, fn func(context.Context) error) error {
	start := time.Now()
	err := fn(ctx)
	c.metrics.Record(metrics.Sample{
		Type:     typ,
		Duration: time.Since(start),
		Success:  err == nil,
		ErrKind:  errKind(err),
	})
	return err
}

// tagAttempts and tagRetryDelay bound how often a transient tag failure is
// retried in place. Tag retries live here, not in the executor's create retry,
// so a retryable tag error never re-enters create and duplicates the resource.
const (
	tagAttempts   = 3
	tagRetryDelay = 250 * time.Millisecond
)

// tag replaces the tags on a created resource with the run tags for its kind.
// ReplaceAll is idempotent, so it is safe to retry.
func (c *Client) tag(ctx context.Context, kind Kind, id string) error {
	return c.timed(ctx, string(kind), func(ctx context.Context) error {
		opts := attributestags.ReplaceAllOpts{Tags: runTags(c.runID, kind)}
		_, err := attributestags.ReplaceAll(ctx, c.gc, tagCollection(kind), id, opts).Extract()
		return err
	})
}

// tagWithRetry tags an already-created resource, retrying a transient failure a
// bounded number of times. The resource id is fixed and ReplaceAll is
// idempotent, so unlike the executor's create retry this never re-creates the
// resource — preventing a retryable tag failure from orphaning a duplicate.
func (c *Client) tagWithRetry(ctx context.Context, kind Kind, id string) error {
	var err error
	for attempt := 1; attempt <= tagAttempts; attempt++ {
		if err = c.tag(ctx, kind, id); err == nil || !IsRetryable(err) {
			return err
		}
		if attempt == tagAttempts {
			break
		}
		select {
		case <-time.After(tagRetryDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// createTagged centralizes the create-then-tag flow shared by every taggable
// resource kind: it applies the deterministic name via create, records the
// create and tag timings, wraps quota errors with ErrQuota, and returns the
// resulting Resource. Tagging address scopes is best-effort because not every
// Neutron release supports it; a tag failure there is logged and the run
// continues. For all other kinds a tag failure (after a bounded in-place retry)
// rolls the created resource back and fails the create, so a resource is never
// left created-but-untagged where tag-based cleanup cannot reclaim it.
func (c *Client) createTagged(ctx context.Context, kind Kind, logical string, create func(ctx context.Context, name string) (id string, err error)) (Resource, error) {
	name := resourceName(c.runID, logical)
	var id string
	err := c.timed(ctx, string(kind), func(ctx context.Context) error {
		var createErr error
		id, createErr = create(ctx, name)
		return createErr
	})
	if err != nil {
		// A create that fails after the request reached Neutron (a lost response
		// or a 5xx past commit) can leave an untagged resource that tag-based
		// cleanup cannot find. Log the deterministic name so an operator can
		// locate any such orphan by name.
		slog.Warn("create failed; a resource with this name may be orphaned in the cloud",
			"kind", kind, "name", name, "error", err)
		return Resource{}, wrapCreate(kind, logical, err)
	}

	r := Resource{Kind: kind, Logical: logical, Name: name, ID: id}
	if err := c.tagWithRetry(ctx, kind, id); err != nil {
		if kind == KindAddressScope {
			slog.Warn("tagging address scope failed; continuing", "id", id, "error", err)
			return r, nil
		}
		// The resource exists but is untagged, so tag-based cleanup can never
		// reclaim it. Roll it back so the resource is either fully tagged or
		// gone; if the rollback also fails, log the name as a last resort.
		if delErr := c.Delete(ctx, r); delErr != nil {
			slog.Warn("rolling back untagged resource failed; it may be orphaned in the cloud",
				"kind", kind, "name", name, "id", id, "error", delErr)
		}
		return Resource{}, fmt.Errorf("tagging %s %q: %w", kind, logical, err)
	}
	return r, nil
}

// wrapCreate adds operation context to a create error and, when the error is a
// quota rejection, threads ErrQuota into the chain so the executor can fail
// fast while preserving the underlying gophercloud error for classification.
func wrapCreate(kind Kind, logical string, err error) error {
	if isQuota(err) {
		return fmt.Errorf("creating %s %q: %w: %w", kind, logical, ErrQuota, err)
	}
	return fmt.Errorf("creating %s %q: %w", kind, logical, err)
}

// WaitForReady polls a status-bearing resource until it reaches its expected
// status, recording one Readiness sample. For kinds without a meaningful status
// it returns nil immediately. It returns ctx.Err() if ctx is cancelled or its
// deadline elapses before the resource is ready; the caller decides whether a
// readiness deadline is fatal.
func (c *Client) WaitForReady(ctx context.Context, r Resource) error {
	if !isStatusKind(r.Kind) {
		return nil
	}

	start := time.Now()
	backoff := 200 * time.Millisecond
	for {
		if status, err := c.status(ctx, r); err == nil {
			if expectedReady(r.Kind, status) {
				c.metrics.RecordReadiness(metrics.Readiness{
					Type:     string(r.Kind),
					Duration: time.Since(start), OK: true,
				})
				return nil
			}
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			c.metrics.RecordReadiness(metrics.Readiness{
				Type:     string(r.Kind),
				Duration: time.Since(start), OK: false,
			})
			return ctx.Err()
		}

		if backoff = time.Duration(float64(backoff) * 1.5); backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
	}
}

// isStatusKind reports whether a kind exposes a status worth polling.
func isStatusKind(kind Kind) bool {
	switch kind {
	case KindNetwork, KindRouter, KindPort:
		return true
	default:
		return false
	}
}

// expectedReady reports whether status is a ready state for kind. Ports are
// ready when ACTIVE or DOWN (an unbound port stays DOWN); networks and routers
// are ready when ACTIVE.
func expectedReady(kind Kind, status string) bool {
	switch kind {
	case KindPort:
		return status == statusActive || status == statusDown
	default:
		return status == statusActive
	}
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
		return fmt.Sprintf("http_%d", s)
	}
	return "other"
}

// IsRetryable reports whether err is a transient Neutron failure worth retrying:
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

// IsConflict reports whether err is a non-quota 409 conflict. Most Neutron 409s
// are permanent (overlapping CIDR, address-scope conflict, duplicate prefix), so
// the executor caps their retries rather than spending the full backoff budget
// on a conflict that will never clear.
func IsConflict(err error) bool {
	return !isQuota(err) && gophercloud.ResponseCodeIs(err, 409)
}

// isQuota reports whether err is a Neutron quota rejection: a 409 or 403 whose
// body mentions a quota.
func isQuota(err error) bool {
	var code gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &code) {
		return false
	}
	if code.Actual != 409 && code.Actual != 403 {
		return false
	}
	body := strings.ToLower(string(code.Body))
	return strings.Contains(body, "overquota") ||
		strings.Contains(body, "quota exceeded") ||
		strings.Contains(body, "quotaexceeded")
}
