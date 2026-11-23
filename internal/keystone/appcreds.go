package keystone

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/applicationcredentials"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/gophercloud/gophercloud/v2/pagination"

	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// ownerToken obtains a token scoped to projectID as the created user user (its
// logical name), with its password in domain userDomainID, and returns the
// token id. Keystone lets only the owner create an application credential and
// only the trustor create a trust, so the calls that act as the created user
// send this token. It goes through IssueScopedToken, so every owner token
// counts as a token/create sample and a kind=token time-to-ready sample. The
// caller keeps the token in a local variable only.
func (c *Client) ownerToken(ctx context.Context, user, userDomainID, password, projectID string) (string, error) {
	tok, err := c.IssueScopedToken(ctx, keystoneplan.TokenIssue{User: user}, userDomainID, password, projectID)
	if err != nil {
		return "", err
	}
	return tok.ID, nil
}

// CreateApplicationCredential creates the application credential ac as its
// owner, the created user userID (with its password, in domain userDomainID),
// through a token of that user scoped to projectID: Keystone's default policy
// lets no one else create one, in either privilege tier. The credential
// carries the one role roleID and the secret the caller generated, so the
// secret never has to travel from the create result to an authentication. It
// is named dizzy-<run>-<logical>, unique per user. The returned resource's id
// is the synthetic <userID>:<credentialID>, because Keystone addresses a
// credential only under its user. A failed owner token and a failed create
// both return through wrapCreate, so a 403 carries ErrForbidden. A 409 whose
// lookup, sent with the same owner token, finds the user's credential of that
// name on projectID adopts it and records as a successful create: it is this
// run's credential, made by a create whose answer was lost or by an earlier
// instance of the node whose deletes failed, with the secret the caller still
// holds.
func (c *Client) CreateApplicationCredential(ctx context.Context, ac keystoneplan.ApplicationCredential, userID, userDomainID, password, projectID, roleID, secret string) (resource.Resource, error) {
	token, err := c.ownerToken(ctx, ac.User, userDomainID, password, projectID)
	if err != nil {
		return resource.Resource{}, wrapCreate(KindApplicationCredential, ac.Name, err)
	}
	name := resourceName(c.runID, ac.Name)
	sc := withToken(c.gc, token)
	var id string
	_, err = c.timedFold(ctx, string(KindApplicationCredential), "create", func(ctx context.Context) error {
		created, err := applicationcredentials.Create(ctx, sc, userID, applicationcredentials.CreateOpts{
			Name:   name,
			Secret: secret,
			Roles:  []applicationcredentials.Role{{ID: roleID}},
		}).Extract()
		if err != nil {
			return err
		}
		id = created.ID
		return nil
	}, adoptOnConflict(KindApplicationCredential, name, &id, func(ctx context.Context) (string, error) {
		found, err := c.findApplicationCredential(ctx, sc, userID, name, projectID)
		return found, redactToken(err, token)
	}))
	if err != nil {
		return resource.Resource{}, wrapCreate(KindApplicationCredential, ac.Name, redactToken(err, secret))
	}
	return resource.Resource{Kind: KindApplicationCredential, Logical: ac.Name, Name: name, ID: applicationCredentialID(userID, id)}, nil
}

// findApplicationCredential returns the id of userID's application
// credential named name on projectID, or "" when there is none: the lookup a
// create answered 409 adopts by. sc carries the owner's token. It lists the
// user's credentials filtered by name, outside timed, and rechecks name and
// project client side, so a server that ignores the filter never widens the
// match.
func (c *Client) findApplicationCredential(ctx context.Context, sc *gophercloud.ServiceClient, userID, name, projectID string) (string, error) {
	var found string
	err := applicationcredentials.List(sc, userID, applicationcredentials.ListOpts{Name: name}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
		items, err := applicationcredentials.ExtractApplicationCredentials(page)
		if err != nil {
			return false, err
		}
		for _, ac := range items {
			if ac.Name == name && ac.ProjectID == projectID {
				found = ac.ID
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return "", fmt.Errorf("listing application credentials named %q of user %s: %w", name, userID, err)
	}
	return found, nil
}

// AuthenticateApplicationCredential obtains a token with the application
// credential res and its secret, the read-back that proves the credential
// works. The request names no scope, since a credential carries its own
// project, and goes through a copy of the admin's identity client that never
// re-authenticates (noReauth), so a 401 is the credential's own failure. It is
// timed as the application_credential/authenticate operation and its duration
// is also recorded as a time-to-ready sample of kind application_credential.
// The error's text never carries the secret.
func (c *Client) AuthenticateApplicationCredential(ctx context.Context, ac keystoneplan.ApplicationCredential, res resource.Resource, secret string) error {
	_, credentialID, err := parseApplicationCredentialID(res.ID)
	if err != nil {
		return fmt.Errorf("authenticating application credential %q: %w", ac.Name, err)
	}
	var d time.Duration
	err = c.timed(ctx, string(KindApplicationCredential), "authenticate", func(ctx context.Context) error {
		start := time.Now()
		_, err := tokens.Create(ctx, noReauth(c.gc), &tokens.AuthOptions{
			ApplicationCredentialID:     credentialID,
			ApplicationCredentialSecret: secret,
			AllowReauth:                 false,
		}).ExtractToken()
		d = time.Since(start)
		return err
	})
	c.metrics.RecordReadiness(metrics.Readiness{Type: string(KindApplicationCredential), Duration: d, OK: err == nil})
	c.tel.RecordTimeToReady(ctx, string(KindApplicationCredential), d, err == nil)
	if err != nil {
		return fmt.Errorf("authenticating application credential %q: %w", ac.Name, redactToken(err, secret))
	}
	return nil
}

// DeleteApplicationCredential deletes the application credential res as its
// owner, through a token of the owning user (with its password, in domain
// userDomainID) scoped to projectID: Keystone lets only an admin or the owner
// delete a credential. It is timed as the application_credential/delete
// operation. A 404 stays reachable for IsNotFound, so a caller can treat a
// credential that is already gone as deleted.
func (c *Client) DeleteApplicationCredential(ctx context.Context, ac keystoneplan.ApplicationCredential, res resource.Resource, userDomainID, password, projectID string) error {
	userID, credentialID, err := parseApplicationCredentialID(res.ID)
	if err != nil {
		return fmt.Errorf("deleting application credential %q: %w", ac.Name, err)
	}
	token, err := c.ownerToken(ctx, ac.User, userDomainID, password, projectID)
	if err != nil {
		return fmt.Errorf("deleting application credential %q: %w", ac.Name, err)
	}
	err = c.timed(ctx, string(KindApplicationCredential), "delete", func(ctx context.Context) error {
		return applicationcredentials.Delete(ctx, withToken(c.gc, token), userID, credentialID).ExtractErr()
	})
	if err != nil {
		return fmt.Errorf("deleting application credential %q: %w", ac.Name, err)
	}
	return nil
}

// applicationCredentialID builds the synthetic application credential id
// "<userID>:<credentialID>" that parseApplicationCredentialID inverts. It is
// the one place the format is written.
func applicationCredentialID(userID, credentialID string) string {
	return userID + ":" + credentialID
}

// parseApplicationCredentialID inverts applicationCredentialID. Keystone ids
// are hex, so ":" is an unambiguous separator; both parts must be non-empty.
func parseApplicationCredentialID(id string) (userID, credentialID string, err error) {
	userID, credentialID, ok := strings.Cut(id, ":")
	if !ok || userID == "" || credentialID == "" || strings.Contains(credentialID, ":") {
		return "", "", fmt.Errorf("malformed application credential id %q", id)
	}
	return userID, credentialID, nil
}
