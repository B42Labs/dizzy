package keystone

import (
	"context"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/trusts"

	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// CreateTrust creates the trust t as its trustor, the created user trustorID
// (with its password, in domain trustorDomainID), through a token of that user
// scoped to projectID: Keystone rejects a trust any other caller creates, in
// either privilege tier. The trust delegates the one role roleID on projectID
// to trusteeID with impersonation, as Heat creates its trusts, and without
// expiry or use limit. A trust has no name, so the returned resource carries
// the trust's id and an empty Name. A failed owner token and a failed create
// both return through wrapCreate, so a 403 carries ErrForbidden.
func (c *Client) CreateTrust(ctx context.Context, t keystoneplan.Trust, trustorID, trustorDomainID, trustorPassword, trusteeID, projectID, roleID string) (resource.Resource, error) {
	token, err := c.ownerToken(ctx, t.Trustor, trustorDomainID, trustorPassword, projectID)
	if err != nil {
		return resource.Resource{}, wrapCreate(KindTrust, t.Name, err)
	}
	var id string
	err = c.timed(ctx, string(KindTrust), "create", func(ctx context.Context) error {
		created, err := trusts.Create(ctx, withToken(c.gc, token), trusts.CreateOpts{
			Impersonation: true,
			TrustorUserID: trustorID,
			TrusteeUserID: trusteeID,
			ProjectID:     projectID,
			Roles:         []trusts.Role{{ID: roleID}},
		}).Extract()
		if err != nil {
			return err
		}
		id = created.ID
		return nil
	})
	if err != nil {
		return resource.Resource{}, wrapCreate(KindTrust, t.Name, err)
	}
	return resource.Resource{Kind: KindTrust, Logical: t.Name, ID: id}, nil
}

// AuthenticateTrust obtains a token scoped to the trust res as its trustee,
// the created user t.Trustee (with its password, in domain trusteeDomainID):
// the read-back that proves the trust works. The request goes through a copy
// of the admin's identity client that never re-authenticates (noReauth), so a
// 401 is the trustee's own failure. It is timed as the trust/authenticate
// operation and its duration is also recorded as a time-to-ready sample of
// kind trust.
func (c *Client) AuthenticateTrust(ctx context.Context, t keystoneplan.Trust, res resource.Resource, trusteeDomainID, trusteePassword string) error {
	var d time.Duration
	err := c.timed(ctx, string(KindTrust), "authenticate", func(ctx context.Context) error {
		start := time.Now()
		_, err := tokens.Create(ctx, noReauth(c.gc), &tokens.AuthOptions{
			Username:    resourceName(c.runID, t.Trustee),
			DomainID:    trusteeDomainID,
			Password:    trusteePassword,
			AllowReauth: false,
			Scope:       tokens.Scope{TrustID: res.ID},
		}).ExtractToken()
		d = time.Since(start)
		return err
	})
	c.metrics.RecordReadiness(metrics.Readiness{Type: string(KindTrust), Duration: d, OK: err == nil})
	c.tel.RecordTimeToReady(ctx, string(KindTrust), d, err == nil)
	if err != nil {
		return fmt.Errorf("authenticating trust %q as %q: %w", t.Name, t.Trustee, err)
	}
	return nil
}

// DeleteTrust deletes the trust res as its trustor, through a token of the
// trustor (with its password, in domain trustorDomainID) scoped to projectID:
// Keystone lets only an admin or the trustor delete a trust. It is timed as
// the trust/delete operation. A 404 stays reachable for IsNotFound, so a
// caller can treat a trust that is already gone as deleted.
func (c *Client) DeleteTrust(ctx context.Context, t keystoneplan.Trust, res resource.Resource, trustorDomainID, trustorPassword, projectID string) error {
	token, err := c.ownerToken(ctx, t.Trustor, trustorDomainID, trustorPassword, projectID)
	if err != nil {
		return fmt.Errorf("deleting trust %q: %w", t.Name, err)
	}
	err = c.timed(ctx, string(KindTrust), "delete", func(ctx context.Context) error {
		return trusts.Delete(ctx, withToken(c.gc, token), res.ID).ExtractErr()
	})
	if err != nil {
		return fmt.Errorf("deleting trust %q: %w", t.Name, err)
	}
	return nil
}
