// Package c5c3 orders Keystone resources from CobaltCore's c5c3-operator and
// checks what the operator made of them. An order is a c5c3.io/v1alpha1 custom
// resource of one of five kinds (KeystoneUser, KeystoneProject,
// KeystoneRoleAssignment, KeystoneCatalogEntry, KeystoneApplicationCredential)
// that dizzy creates, reads, patches, lists and deletes through a client-go
// dynamic client, without typed copies of the operator's API. Every order is
// named dizzyorder-<runid>-<logical> and carries the labels dizzy/run=<runid>
// and dizzy/type=<kind>, so a run's orders are found by a server-side label
// selector in the one namespace the client acts in. The prefix differs from the
// dizzy- of the other services so that the Keystone namespace's orphan sweep,
// which deletes Keystone users named dizzy-*, never reaches an ordered user.
//
// A user and an application credential order each deliver a Secret
// <order name>-credentials beside the order, with a clouds.yaml key and the
// credential's own keys. The client reads those Secrets through the same
// dynamic client and edits and deletes them for the repair check. It reads no
// Secret that is not named after a dizzy order, and edits and deletes only the
// Secrets of its own run's orders. The Keystone checks log in with the
// delivered credentials to verify the orders.
//
// Each call is timed through a metrics.Collector and, when enabled, mirrored
// into OTEL. A wait for the operator polls with a GET whose interval starts at
// 500 ms, grows by half on each poll and is capped at 5 s, and every poll is
// recorded as one get, so a wait of minutes keeps feeding the health check.
package c5c3

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/dynamic"

	"github.com/B42Labs/dizzy/internal/c5c3/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// The c5c3 resource kinds. They double as the metrics "kind" label, and the
// five order kinds are also the value of an order's dizzy/type label. Each
// order kind is the plan's snake case name for one c5c3.io/v1alpha1 kind.
// KindSecret is a Secret the operator delivers beside a user or application
// credential order.
const (
	KindUser                  resource.Kind = resource.Kind(plan.KindUser)
	KindProject               resource.Kind = resource.Kind(plan.KindProject)
	KindRoleAssignment        resource.Kind = resource.Kind(plan.KindRoleAssignment)
	KindCatalogEntry          resource.Kind = resource.Kind(plan.KindCatalogEntry)
	KindApplicationCredential resource.Kind = resource.Kind(plan.KindApplicationCredential)
	KindSecret                resource.Kind = "secret"
)

// The API group and version every order is created in.
const (
	apiGroup   = "c5c3.io"
	apiVersion = "v1alpha1"
)

// namePrefix is the common prefix of every order dizzy creates. A run's orders
// are named namePrefix + runID + "-" + logical.
const namePrefix = "dizzyorder-"

// secretSuffix is what the operator appends to an order's name to name the
// Secret it delivers beside the order.
const secretSuffix = "-credentials"

// The labels binding an order to a run and to its kind. ListByRun selects on
// labelRun, ListByAnyRun on the presence of labelType.
const (
	labelRun  = "dizzy/run"
	labelType = "dizzy/type"
)

// orderKind ties one order kind to the resource and the Kubernetes kind it is
// served under.
type orderKind struct {
	kind     resource.Kind
	resource string
	k8sKind  string
}

// gvr is the group, version and resource the dynamic client addresses the
// kind's orders through.
func (k orderKind) gvr() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: apiGroup, Version: apiVersion, Resource: k.resource}
}

// orderKinds lists the five order kinds in the order Precheck lists them.
var orderKinds = []orderKind{
	{KindUser, "keystoneusers", "KeystoneUser"},
	{KindProject, "keystoneprojects", "KeystoneProject"},
	{KindRoleAssignment, "keystoneroleassignments", "KeystoneRoleAssignment"},
	{KindCatalogEntry, "keystonecatalogentries", "KeystoneCatalogEntry"},
	{KindApplicationCredential, "keystoneapplicationcredentials", "KeystoneApplicationCredential"},
}

// secretsGVR addresses the core v1 Secrets the operator delivers.
var secretsGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// lookupKind returns the table entry of an order kind, and false for any other
// kind, the secret kind included.
func lookupKind(kind resource.Kind) (orderKind, bool) {
	for _, k := range orderKinds {
		if k.kind == kind {
			return k, true
		}
	}
	return orderKind{}, false
}

// The intervals of every wait's polls: the first interval, the factor each
// later one grows by, and the cap.
const (
	pollInitial = 500 * time.Millisecond
	pollFactor  = 1.5
	pollMax     = 5 * time.Second
)

// ErrRefused is the sentinel WaitReady wraps when the operator refuses an
// order with a reason that no retry lifts, such as RoleNotAllowed. It is never
// retryable.
var ErrRefused = errors.New("c5c3: order refused")

// ErrMismatch is the sentinel a check wraps when what the operator delivered or
// reported differs from what dizzy expects: an order that turns ready where a
// refusal was due, a missing or malformed Secret, a credential that did not
// rotate, or a credential Keystone rejects while its order is ready. It is
// never retryable.
var ErrMismatch = errors.New("c5c3: mismatch")

// ErrAccepted is the sentinel a login that must fail wraps when Keystone
// issues a token instead. It is never retryable.
var ErrAccepted = errors.New("c5c3: credential accepted")

// ErrKindNotServed is the sentinel Precheck wraps, together with the 404, when
// the cluster does not serve one of the order kinds, which means the
// c5c3-operator's CRDs are not installed.
var ErrKindNotServed = errors.New("c5c3: kind not served")

// ErrForbidden is the sentinel a call wraps, together with the 403, when the
// process lacks a right in the namespace: Precheck for a list of an order
// kind, EditSecret and DeleteSecret for the Secret repair check.
var ErrForbidden = errors.New("c5c3: forbidden")

// Client creates and verifies one run's orders in one Kubernetes namespace,
// binding every order to a run id and recording timing into a Collector.
type Client struct {
	dyn       dynamic.Interface
	namespace string
	runID     string
	metrics   *metrics.Collector
	tel       *telemetry.Telemetry
	plan      *plan.Plan

	// pollInitial and pollMax are the first and the largest interval between
	// two polls of a wait. New sets them to the package's pollInitial and
	// pollMax; tests shorten them.
	pollInitial time.Duration
	pollMax     time.Duration

	// keystoneURL and keystoneTLS replace the auth_url and the TLS
	// configuration of every delivered document a Keystone check logs in
	// with, each when SetKeystoneEndpoint set it. Both are empty after New,
	// which leaves the documents as delivered.
	keystoneURL string
	keystoneTLS *tls.Config

	// requestTimeout bounds each request of Precheck and Observe on its own,
	// when SetRequestTimeout set it above zero. It is zero after New.
	requestTimeout time.Duration

	// passwordRejections counts the 401 answers to password logins. Once it
	// reaches maxPasswordRejections, the client sends no further password
	// login, so a run cannot lock its user out of Keystone.
	passwordRejections atomic.Int64
}

// New returns a Client that creates, lists and prechecks orders in namespace
// through dyn, names and labels them for runID, and records timing into m.
func New(dyn dynamic.Interface, namespace, runID string, m *metrics.Collector) *Client {
	return &Client{
		dyn:         dyn,
		namespace:   namespace,
		runID:       runID,
		metrics:     m,
		pollInitial: pollInitial,
		pollMax:     pollMax,
	}
}

// SetTelemetry attaches an OTEL export seam so each timed call and wait is
// recorded live alongside the in-memory collector, which stays the source of
// truth for run records and reports. A nil t (the default after New) leaves
// export disabled: every telemetry call is a no-op.
func (c *Client) SetTelemetry(t *telemetry.Telemetry) {
	c.tel = t
}

// SetPlan gives CreateOrder the ControlPlane every order references and the
// grace period of the application credential order. Only apply needs it:
// status and cleanup address existing orders and never create one.
func (c *Client) SetPlan(p *plan.Plan) {
	c.plan = p
}

// SetRequestTimeout bounds each request of Precheck and Observe on its own:
// each of Precheck's five lists, and Observe's GET, runs under its own
// context.WithTimeout(d). These two are what the commands send outside the
// executor, which bounds the calls it makes itself. A bound per request,
// not per check, keeps five slow lists from failing a check whose every list
// answered in time, while a wedged API server still cannot hang the command.
// A d of zero (the default after New) adds no bound, leaving each request to
// the caller's context. Call it before the first request; it is not safe to
// call while one runs.
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

// resourceName builds the deterministic Kubernetes name for a logical plan
// name.
func resourceName(runID, logical string) string {
	return runPrefix(runID) + logical
}

// runPrefix is the name prefix shared by every order of one run.
func runPrefix(runID string) string {
	return namePrefix + runID + "-"
}

// secretName returns the name of the Secret the operator delivers beside the
// order named orderName.
func secretName(orderName string) string {
	return orderName + secretSuffix
}

// orderID builds the id a created or listed order is recorded under: its
// namespace and name, joined by a slash.
func orderID(namespace, name string) string {
	return namespace + "/" + name
}

// parseID splits an order id into its namespace and name. It returns an error
// for an id without exactly one slash or with an empty half.
func parseID(id string) (namespace, name string, err error) {
	namespace, name, ok := strings.Cut(id, "/")
	if !ok || namespace == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("malformed order id %q: want <namespace>/<name>", id)
	}
	return namespace, name, nil
}

// target is an order a method addresses, resolved from a resource.Resource
// before any request: its kind's table entry and the namespace and name of
// its id.
type target struct {
	orderKind
	id        string
	namespace string
	name      string
}

// parseTarget resolves r into the order it addresses. It returns an error for
// a kind that is not an order kind and for a malformed id.
func parseTarget(r resource.Resource) (target, error) {
	k, ok := lookupKind(r.Kind)
	if !ok {
		return target{}, fmt.Errorf("unknown c5c3 order kind %q", r.Kind)
	}
	namespace, name, err := parseID(r.ID)
	if err != nil {
		return target{}, err
	}
	return target{orderKind: k, id: r.ID, namespace: namespace, name: name}, nil
}

// orders returns the dynamic client for the target's kind in its namespace.
func (c *Client) orders(t target) dynamic.ResourceInterface {
	return c.dyn.Resource(t.gvr()).Namespace(t.namespace)
}

// timed runs fn, records a Sample of kind for the attempt (including the error
// classification), mirrors the same measurement live into telemetry under the
// low-cardinality op label, and returns fn's error unchanged. The in-memory
// collector stays the source of truth for run records and reports; the
// telemetry record is additive and a no-op when export is disabled.
func (c *Client) timed(ctx context.Context, kind resource.Kind, op string, fn func(context.Context) error) error {
	start := time.Now()
	err := fn(ctx)
	c.record(ctx, kind, op, time.Since(start), err)
	return err
}

// record adds one sample of kind and its live operation record, classifying
// err. timed records through it, and so does every wait: for each poll, whose
// error the wait decides once it has read the answer, and for the operation
// the whole wait measures.
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

// poll calls check at once and then after each interval, starting at
// c.pollInitial and growing to at most c.pollMax, until check reports done or
// returns an error, which poll returns. It returns ctx.Err() when ctx ends
// first.
func (c *Client) poll(ctx context.Context, check func(context.Context) (bool, error)) error {
	interval := c.pollInitial
	for {
		done, err := check(ctx)
		if done || err != nil {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
		interval = nextInterval(interval, c.pollMax)
	}
}

// nextInterval returns the poll interval that follows d: d grown by
// pollFactor, capped at limit.
func nextInterval(d, limit time.Duration) time.Duration {
	if next := time.Duration(float64(d) * pollFactor); next < limit {
		return next
	}
	return limit
}

// patchConflictError marks a 409 answer to a merge patch. A merge patch
// carries no resourceVersion, so a conflict there means the API server lost a
// race with another writer and the same patch can be sent again. A 409 to any
// other call, such as a create of an order that exists, is not retried.
type patchConflictError struct {
	err error
}

// Error returns the conflict's own text.
func (e *patchConflictError) Error() string { return e.err.Error() }

// Unwrap returns the Kubernetes 409 the patch received, so the error kind
// stays http_409.
func (e *patchConflictError) Unwrap() error { return e.err }

// wrapPatchError wraps a 409 answer to a patch so IsRetryable retries it and
// returns every other error unchanged.
func wrapPatchError(err error) error {
	if apierrors.IsConflict(err) {
		return &patchConflictError{err: err}
	}
	return err
}

// httpStatus returns the HTTP status code carried by a Kubernetes API error or
// a gophercloud error, or 0 when the error is nil or carries none.
func httpStatus(err error) int {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		if code := status.Status().Code; code != 0 {
			return int(code)
		}
	}
	var code gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &code) {
		return code.GetStatusCode()
	}
	return 0
}

// isSentinel reports whether err wraps one of the package's sentinels.
func isSentinel(err error) bool {
	for _, s := range []error{ErrRefused, ErrMismatch, ErrAccepted, ErrKindNotServed, ErrForbidden} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

// errKind classifies an error into a stable, low-cardinality label for the
// metrics error breakdown. It returns the empty string for a nil error. The
// three check sentinels come first, so a mismatch found on a 404 reads as
// mismatch, not as http_404; ErrKindNotServed and ErrForbidden carry their
// status and read as http_404 and http_403.
func errKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrRefused):
		return "refused"
	case errors.Is(err, ErrMismatch):
		return "mismatch"
	case errors.Is(err, ErrAccepted):
		return "accepted"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	if s := httpStatus(err); s != 0 {
		// The status is the peer's: an API server or Keystone behind a proxy
		// can report any 3-digit code, so bound the label to the codes the two
		// APIs return and collapse the rest to http_other.
		switch s {
		case 400, 401, 403, 404, 409, 422, 429, 500, 502, 503, 504:
			return fmt.Sprintf("http_%d", s)
		default:
			return "http_other"
		}
	}
	return "other"
}

// IsRetryable reports whether err is a transient failure worth retrying: a 429,
// a 5xx of 500, 502, 503 or 504 from Kubernetes or Keystone, a 409 to a merge
// patch, or a connection that was refused, reset or cut short. A sentinel is
// never retryable, whatever status it carries.
func IsRetryable(err error) bool {
	if err == nil || isSentinel(err) {
		return false
	}
	var conflict *patchConflictError
	if errors.As(err, &conflict) {
		return true
	}
	switch httpStatus(err) {
	case 429, 500, 502, 503, 504:
		return true
	}
	return isConnectionError(err)
}

// isConnectionError reports whether err is a connection that was refused,
// reset, or closed before the answer was complete.
func isConnectionError(err error) bool {
	return utilnet.IsConnectionRefused(err) ||
		utilnet.IsConnectionReset(err) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		utilnet.IsProbableEOF(err)
}

// IsNotFound reports whether err is a 404 from Kubernetes or Keystone. Status
// and cleanup use it to treat an object that is already gone as success
// rather than an error, which is what makes cleanup idempotent.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) || gophercloud.ResponseCodeIs(err, 404) {
		return true
	}
	var nf gophercloud.ErrResourceNotFound
	return errors.As(err, &nf)
}

// Precheck lists each of the five order kinds in the namespace with a limit of
// 1, each as one list sample of its kind, so a command stops before it creates
// anything when the cluster or its rights cannot carry a run. Each list runs
// under the request timeout of SetRequestTimeout on its own. A 404 returns
// ErrKindNotServed naming the resource, because the operator's CRDs are
// missing; a 403 returns ErrForbidden naming the resource and the namespace.
// Both wrap the original error. Any other error is returned wrapped, and the
// first error ends the check.
func (c *Client) Precheck(ctx context.Context) error {
	for _, k := range orderKinds {
		err := c.timed(ctx, k.kind, "list", func(ctx context.Context) error {
			ctx, cancel := c.requestContext(ctx)
			defer cancel()
			_, err := c.dyn.Resource(k.gvr()).Namespace(c.namespace).List(ctx, metav1.ListOptions{Limit: 1})
			return err
		})
		qualified := k.resource + "." + apiGroup
		switch {
		case err == nil:
		case IsNotFound(err):
			return fmt.Errorf("%w: the cluster does not serve %s; install the c5c3-operator's CRDs: %w", ErrKindNotServed, qualified, err)
		case apierrors.IsForbidden(err):
			return fmt.Errorf("%w: listing %s in namespace %q: %w", ErrForbidden, qualified, c.namespace, err)
		default:
			return fmt.Errorf("listing %s in namespace %q: %w", qualified, c.namespace, err)
		}
	}
	return nil
}
