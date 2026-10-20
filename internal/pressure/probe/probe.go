// Package probe sends a pressure pool user's token to Nova, Neutron, Cinder or
// Glance for the cheapest authenticated read the service offers, so the
// keystonemiddleware inside the service has to validate it. It is the adapter
// behind the pressure operations' service seam: probe sends a live pool token,
// bad_token a token the service has to refuse, and the propagation probe polls
// with a revoked one until the service refuses it.
//
// Every read is one request through a service client built for the call,
// which sends the given token and never re-authenticates, so a 401 is the
// read's own outcome. Each call is timed into a metrics.Collector and, when
// enabled, mirrored into OTEL, as keystone.Client records its calls. A token
// never reaches an error text: every returned error has it redacted, since a
// gateway may quote a request header in its response body.
package probe

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	cinderlimits "github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/limits"
	novalimits "github.com/gophercloud/gophercloud/v2/openstack/compute/v2/limits"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/pagination"

	"github.com/B42Labs/dizzy/internal/keystone"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// The four services a probe can go to, as the record, the reports and the
// telemetry name them, and Services, their fixed order.
const (
	Nova    = "nova"
	Neutron = "neutron"
	Cinder  = "cinder"
	Glance  = "glance"
)

// Services lists the four services in their fixed order.
var Services = []string{Nova, Neutron, Cinder, Glance}

// The kinds the adapter records its calls under.
const (
	KindProbe       = "probe"
	KindBadToken    = "bad_token"
	KindPropagation = "propagation"
	KindRevocation  = "revocation" // the time-to-ready kind of a propagation delay
)

// ErrAccepted is returned by Reject when the service answered a request it
// should have refused with a 2xx. Classifiers match it with errors.Is.
var ErrAccepted = errors.New("probe: the service accepted a token it should have rejected")

// errKindAccepted is the error label, in the samples and as the telemetry
// error.kind, of a Reject the service answered with a 2xx.
const errKindAccepted = "accepted"

// read is the cheapest authenticated read of one service: a single GET whose
// cost in the service does not grow with what the cloud holds, and whose body
// is discarded.
type read struct {
	// resourceBase is what the service's gophercloud constructor appends to
	// the endpoint as the client's ResourceBase, empty for none.
	resourceBase string
	// send makes the request through sc and returns its outcome.
	send func(ctx context.Context, sc *gophercloud.ServiceClient) error
}

// reads holds the read of each of Services. The two listings stop after the
// first page, so a next link never costs a second request.
var reads = map[string]read{
	// GET <compute>/limits.
	Nova: {send: func(ctx context.Context, sc *gophercloud.ServiceClient) error {
		return novalimits.Get(ctx, sc, nil).Err
	}},
	// GET <network>/v2.0/networks?limit=1, the ResourceBase
	// openstack.NewNetworkV2 sets.
	Neutron: {resourceBase: "v2.0/", send: func(ctx context.Context, sc *gophercloud.ServiceClient) error {
		return networks.List(sc, networks.ListOpts{Limit: 1}).EachPage(ctx, firstPage)
	}},
	// GET <block-storage>/limits, the catalog URL already carrying
	// /v3/<project_id>.
	Cinder: {send: func(ctx context.Context, sc *gophercloud.ServiceClient) error {
		return cinderlimits.Get(ctx, sc).Err
	}},
	// GET <image>/v2/images?limit=1, the ResourceBase openstack.NewImageV2
	// sets.
	Glance: {resourceBase: "v2/", send: func(ctx context.Context, sc *gophercloud.ServiceClient) error {
		return images.List(sc, images.ListOpts{Limit: 1}).EachPage(ctx, firstPage)
	}},
}

// firstPage ends a listing after its first page.
func firstPage(context.Context, pagination.Page) (bool, error) {
	return false, nil
}

// Client sends reads to the four services with tokens it is given, recording
// timing into a Collector. It holds no token of its own.
type Client struct {
	gc      *gophercloud.ServiceClient
	metrics *metrics.Collector
	tel     *telemetry.Telemetry
}

// New returns a Client whose reads share gc's HTTP client and user agent, the
// identity client's, and record timing into m. gc's token and endpoint are
// never used.
func New(gc *gophercloud.ServiceClient, m *metrics.Collector) *Client {
	return &Client{gc: gc, metrics: m}
}

// SetTelemetry attaches an OTEL export seam so each read is recorded live
// alongside the in-memory collector, which stays the source of truth for run
// records and reports. A nil t (the default after New) leaves export disabled:
// every telemetry call is a no-op.
func (c *Client) SetTelemetry(t *telemetry.Telemetry) {
	c.tel = t
}

// Probe sends service's cheapest authenticated read at endpoint with token
// as X-Auth-Token and returns the request's error. It is recorded as kind
// "probe" with the service as the operation.
func (c *Client) Probe(ctx context.Context, service, endpoint, token string) error {
	r, err := readOf(service)
	if err != nil {
		return err
	}
	d, err := c.send(ctx, r, endpoint, token)
	c.record(ctx, KindProbe, service, d, err == nil, keystone.ErrorKind(err))
	return err
}

// Reject sends the same read with a token the service has to refuse. A 401
// is the expected answer and returns nil; a 2xx returns ErrAccepted; any
// other failure is returned as is. It is recorded as kind "bad_token" with
// the service as the operation, success for the 401.
func (c *Client) Reject(ctx context.Context, service, endpoint, token string) error {
	r, err := readOf(service)
	if err != nil {
		return err
	}
	d, err := c.send(ctx, r, endpoint, token)
	switch {
	case gophercloud.ResponseCodeIs(err, 401):
		c.record(ctx, KindBadToken, service, d, true, "")
		return nil
	case accepted(err):
		c.record(ctx, KindBadToken, service, d, false, errKindAccepted)
		return ErrAccepted
	default:
		c.record(ctx, KindBadToken, service, d, false, keystone.ErrorKind(err))
		return err
	}
}

// Poll sends the same read for the propagation probe. rejected is true for
// a 401 and err nil; a 2xx is (false, nil); any other failure is (false,
// err). It is recorded as kind "propagation" with the service as the
// operation; a 401 and a 2xx both record success, since the poll answered
// its question, and every other outcome records a failure.
func (c *Client) Poll(ctx context.Context, service, endpoint, token string) (rejected bool, err error) {
	r, err := readOf(service)
	if err != nil {
		return false, err
	}
	d, err := c.send(ctx, r, endpoint, token)
	switch {
	case gophercloud.ResponseCodeIs(err, 401):
		c.record(ctx, KindPropagation, service, d, true, "")
		return true, nil
	case accepted(err):
		c.record(ctx, KindPropagation, service, d, true, "")
		return false, nil
	default:
		c.record(ctx, KindPropagation, service, d, false, keystone.ErrorKind(err))
		return false, err
	}
}

// RecordRevocationDelay records d on the time-to-ready instrument under kind
// "revocation": metrics.Readiness{Type: "revocation", Duration: d, OK: ok}
// into the collector and telemetry.RecordTimeToReady with the same values.
// ok is false for a probe that reached its timeout.
func (c *Client) RecordRevocationDelay(ctx context.Context, d time.Duration, ok bool) {
	c.metrics.RecordReadiness(metrics.Readiness{Type: KindRevocation, Duration: d, OK: ok})
	c.tel.RecordTimeToReady(ctx, KindRevocation, d, ok)
}

// readOf returns the read of service, or an error naming a service outside
// Services, which the callers return before any request and any record.
func readOf(service string) (read, error) {
	r, ok := reads[service]
	if !ok {
		return read{}, fmt.Errorf("probe: unknown service %q", service)
	}
	return r, nil
}

// send makes r at endpoint with token and returns how long the request took
// and its error with the token redacted. The service client is built for the
// call, as keystone's detached copy is: it shares the identity client's HTTP
// client, whose transport the pressure command sized, and user agent, and has
// no ReauthFunc, so a 401 is one request and the read's own outcome. The
// endpoint is normalized with a trailing "/", which the catalog's endpoints
// already carry.
func (c *Client) send(ctx context.Context, r read, endpoint, token string) (time.Duration, error) {
	endpoint = gophercloud.NormalizeURL(endpoint)
	sc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{HTTPClient: c.gc.HTTPClient, UserAgent: c.gc.UserAgent},
		Endpoint:       endpoint,
	}
	if r.resourceBase != "" {
		sc.ResourceBase = endpoint + r.resourceBase
	}
	sc.SetToken(token)
	start := time.Now()
	err := r.send(ctx, sc)
	return time.Since(start), keystone.RedactToken(err, token)
}

// record adds one sample of kind to the collector and mirrors it into
// telemetry under kind and the service as the operation, as
// keystone.Client.timed records a call.
func (c *Client) record(ctx context.Context, kind, service string, d time.Duration, success bool, errKind string) {
	c.metrics.Record(metrics.Sample{Type: kind, Duration: d, Success: success, ErrKind: errKind})
	c.tel.RecordOperation(ctx, kind, service, d, errKind)
}

// accepted reports whether a read's outcome means the service let the token
// through: no error, or a 2xx status the read did not expect, which a proxy
// answering 203 produces.
func accepted(err error) bool {
	if err == nil {
		return true
	}
	var code gophercloud.ErrUnexpectedResponseCode
	return errors.As(err, &code) && code.Actual >= 200 && code.Actual < 300
}
