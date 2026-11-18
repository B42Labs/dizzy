package barbican

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/acls"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// aclRead is the one ACL type Barbican defines for a secret: who may read it.
const aclRead = "read"

// SetACL sets the read ACL of the stored secret a names to the users a lists,
// with one PUT /v1/secrets/<id>/acl timed as acl/create. The PUT replaces the
// secret's read ACL and carries no project-access key, which Barbican then sets
// to true, so the secret stays readable and deletable by its project and a
// retried PUT lands the same ACL again. An ACL has no name and no id of its
// own, so the returned resource carries the secret's id and no name. A secret
// without an id is refused without a request, and any other failure returns as
// a create error.
func (c *Client) SetACL(ctx context.Context, a barbicanplan.ACL, secret resource.Resource) (resource.Resource, error) {
	if secret.ID == "" {
		return resource.Resource{}, fmt.Errorf("acl %q targets secret %q that was not stored", a.Name, a.Secret)
	}
	users := slices.Clone(a.Users)
	opts := acls.SetOpts{{Type: aclRead, Users: &users}}
	err := c.timed(ctx, KindACL, "create", func(ctx context.Context) error {
		return acls.SetSecretACL(ctx, c.kv, secret.ID, opts).Err
	})
	if err != nil {
		return resource.Resource{}, wrapCreate(KindACL, a.Name, err)
	}
	return resource.Resource{Kind: KindACL, Logical: a.Name, ID: secret.ID}, nil
}

// VerifyACL reads back the read ACL of the secret r carries the id of and
// compares the users it lists with the users a lists, as sorted copies of both,
// inside one call timed as acl/get, so a difference fails that sample with
// error kind mismatch. A difference returns ErrACLMismatch naming both sorted
// lists; any other error returns wrapped with the ACL's name.
func (c *Client) VerifyACL(ctx context.Context, a barbicanplan.ACL, r resource.Resource) error {
	want := slices.Sorted(slices.Values(a.Users))
	err := c.timed(ctx, KindACL, "get", func(ctx context.Context) error {
		listed, err := c.aclUsers(ctx, r.ID)
		if err != nil {
			return err
		}
		if got := slices.Sorted(slices.Values(listed)); !slices.Equal(got, want) {
			return fmt.Errorf("%w: acl %q reads back users %v, want %v", ErrACLMismatch, a.Name, got, want)
		}
		return nil
	})
	if err == nil || errors.Is(err, ErrACLMismatch) {
		return err
	}
	return fmt.Errorf("reading acl %q: %w", a.Name, err)
}

// aclUsers reads the ACL of the secret with secretID and returns the users its
// read entry lists. A secret without an ACL answers a read entry with project
// access and no users, so a missing read entry or users key yields no users.
// It records nothing; its callers time it.
func (c *Client) aclUsers(ctx context.Context, secretID string) ([]string, error) {
	acl, err := acls.GetSecretACL(ctx, c.kv, secretID).Extract()
	if err != nil || acl == nil {
		return nil, err
	}
	return (*acl)[aclRead].Users, nil
}
