package keystone

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"

	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
)

// IssuedToken is a scoped token the client obtained for a created user: the
// token id Keystone returned in the X-Subject-Token header and the expiry it
// reported in the body. A caller that keeps it (the pressure token pool) holds
// it in process memory only; it never reaches a log, a plan or a run record.
type IssuedToken struct {
	ID        string
	ExpiresAt time.Time
}

// IssueScopedToken authenticates as the created user t.User (with its
// generated password, in domain userDomainID) and obtains a token scoped to
// projectID, recording the issue latency, and returns the token. It doubles as
// an end-to-end consistency check that the create→assign chain really took
// effect: a failed issue is a failed operation.
//
// The latency is recorded twice: once as the token/create operation sample and
// once as a time-to-ready sample (kind=token), the natural fit for the
// synchronous-create service's otherwise-unused readiness instrument. The
// request carries the admin token, which Keystone ignores for a token create,
// and goes through a copy of the admin's identity client that never
// re-authenticates (noReauth). It mutates no client state, so issuing as the
// created user through the admin's identity client is side-effect-free, and a
// 401 is the created user's own failure rather than a retry after the admin
// re-authenticated.
func (c *Client) IssueScopedToken(ctx context.Context, t keystoneplan.TokenIssue, userDomainID, password, projectID string) (IssuedToken, error) {
	name := resourceName(c.runID, t.User)
	var (
		d      time.Duration
		issued IssuedToken
	)
	err := c.timed(ctx, string(KindToken), "create", func(ctx context.Context) error {
		start := time.Now()
		tok, issueErr := tokens.Create(ctx, noReauth(c.gc), &tokens.AuthOptions{
			Username:    name,
			DomainID:    userDomainID,
			Password:    password,
			AllowReauth: false,
			Scope:       tokens.Scope{ProjectID: projectID},
		}).ExtractToken()
		d = time.Since(start)
		if issueErr != nil {
			return issueErr
		}
		issued = IssuedToken{ID: tok.ID, ExpiresAt: tok.ExpiresAt}
		return nil
	})
	// Record the issue latency on the time-to-ready instrument too (kind=token),
	// with the same measured duration.
	c.metrics.RecordReadiness(metrics.Readiness{Type: string(KindToken), Duration: d, OK: err == nil})
	c.tel.RecordTimeToReady(ctx, string(KindToken), d, err == nil)
	if err != nil {
		return IssuedToken{}, fmt.Errorf("issuing token for user %q on project %s: %w", t.User, projectID, err)
	}
	return issued, nil
}

// IssueToken issues a scoped token for the created user t.User exactly as
// IssueScopedToken does, recording the same token/create sample and readiness
// record, and drops the token: the apply executor and the churn graph only
// need the issue to succeed and its latency recorded.
func (c *Client) IssueToken(ctx context.Context, t keystoneplan.TokenIssue, userDomainID, password, projectID string) error {
	_, err := c.IssueScopedToken(ctx, t, userDomainID, password, projectID)
	return err
}

// ValidateToken validates token with GET /v3/auth/tokens, sending the admin
// token as X-Auth-Token and token as X-Subject-Token: the request
// keystonemiddleware makes on a cache miss. It is recorded as the token/validate
// operation. An invalid or revoked token is Keystone's 404, which classifies as
// http_404 in the samples and as not-found in ClassifyOperationError. The
// error's text never carries the token (redactToken).
func (c *Client) ValidateToken(ctx context.Context, token string) error {
	err := c.timed(ctx, string(KindToken), "validate", func(ctx context.Context) error {
		return tokens.Get(ctx, c.gc, token).Err
	})
	if err != nil {
		return fmt.Errorf("validating token: %w", redactToken(err, token))
	}
	return nil
}

// RevokeToken revokes token with DELETE /v3/auth/tokens, sending the admin
// token as X-Auth-Token and token as X-Subject-Token, so Keystone records a
// revocation event for it. It is recorded as the token/revoke operation. The
// error's text never carries the token (redactToken). tokens.RevokeResult has
// no ExtractErr in gophercloud v2, so the result's Err is the request's outcome.
func (c *Client) RevokeToken(ctx context.Context, token string) error {
	err := c.timed(ctx, string(KindToken), "revoke", func(ctx context.Context) error {
		return tokens.Revoke(ctx, c.gc, token).Err
	})
	if err != nil {
		return fmt.Errorf("revoking token: %w", redactToken(err, token))
	}
	return nil
}

// tokenRedacted is an error whose text has a token replaced, while Unwrap keeps
// the original error in the chain for errors.Is and errors.As.
type tokenRedacted struct {
	err   error
	token string
}

// Error returns the wrapped error's text with every occurrence of the token
// replaced by <token>.
func (e *tokenRedacted) Error() string {
	return strings.ReplaceAll(e.err.Error(), e.token, "<token>")
}

// Unwrap returns the original error, so the response status still classifies.
func (e *tokenRedacted) Unwrap() error {
	return e.err
}

// redactToken keeps token out of err's text. A gophercloud response error
// quotes the response body, and Keystone's 404 for a token it cannot find names
// that token, so without it a pool token could reach a log line. The status
// stays reachable through Unwrap, so errKind and ClassifyOperationError read
// the redacted error as they read the original.
func redactToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	return &tokenRedacted{err: err, token: token}
}

// noReauth returns a copy of gc whose provider client never re-authenticates,
// so a 401 on a request sent through it is the request's own failure. The copy
// carries the admin's HTTP client, user agent and token. The token is read under
// the admin's token lock, since another worker's 401 may re-authenticate the
// shared admin client concurrently.
func noReauth(gc *gophercloud.ServiceClient) *gophercloud.ServiceClient {
	sc := detached(gc)
	sc.CopyTokenFrom(gc.ProviderClient)
	return sc
}

// withToken returns a copy of gc that sends token as X-Auth-Token and never
// re-authenticates, so a call made as a pool user neither reads nor changes the
// admin client's token, and a 401 is the pool token's own failure.
func withToken(gc *gophercloud.ServiceClient, token string) *gophercloud.ServiceClient {
	sc := detached(gc)
	sc.SetToken(token)
	return sc
}

// detached returns a copy of gc with a fresh provider client that carries the
// admin's HTTP client and user agent but no token and no re-authentication
// function. noReauth and withToken fill in the token their callers need.
func detached(gc *gophercloud.ServiceClient) *gophercloud.ServiceClient {
	pc := &gophercloud.ProviderClient{HTTPClient: gc.HTTPClient, UserAgent: gc.UserAgent}
	sc := *gc
	sc.ProviderClient = pc
	return &sc
}
