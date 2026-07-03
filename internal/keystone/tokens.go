package keystone

import (
	"context"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"

	"github.com/B42Labs/openstack-tester/internal/metrics"
	keystoneplan "github.com/B42Labs/openstack-tester/internal/keystone/plan"
)

// IssueToken authenticates as the created user t.User (with its derived
// password, in domain userDomainID) and obtains a token scoped to projectID,
// recording the issue latency. It is the tool's first authenticate-as-a-created-
// principal operation and doubles as an end-to-end consistency check that the
// create→assign chain really took effect: a failed issue is a failed operation.
//
// The latency is recorded twice: once as the token/create operation sample and
// once as a time-to-ready sample (kind=token), the natural fit for the
// synchronous-create service's otherwise-unused readiness instrument. The
// underlying tokens.Create omits the X-Auth-Token header and mutates no client
// state, so issuing as the created user through the admin's identity client is
// side-effect-free.
func (c *Client) IssueToken(ctx context.Context, t keystoneplan.TokenIssue, userDomainID, password, projectID string) error {
	name := resourceName(c.runID, t.User)
	var d time.Duration
	err := c.timed(ctx, string(KindToken), "create", func(ctx context.Context) error {
		start := time.Now()
		_, issueErr := tokens.Create(ctx, c.gc, &tokens.AuthOptions{
			Username:    name,
			DomainID:    userDomainID,
			Password:    password,
			AllowReauth: false,
			Scope:       tokens.Scope{ProjectID: projectID},
		}).ExtractTokenID()
		d = time.Since(start)
		return issueErr
	})
	// Record the issue latency on the time-to-ready instrument too (kind=token),
	// with the same measured duration.
	c.metrics.RecordReadiness(metrics.Readiness{Type: string(KindToken), Duration: d, OK: err == nil})
	c.tel.RecordTimeToReady(ctx, string(KindToken), d, err == nil)
	if err != nil {
		return fmt.Errorf("issuing token for user %q on project %s: %w", t.User, projectID, err)
	}
	return nil
}
