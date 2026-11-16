package barbican

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/secrets"
	"github.com/gophercloud/gophercloud/v2/pagination"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/payload"
	"github.com/B42Labs/dizzy/internal/resource"
)

// StoreSecret stores an opaque secret with the deterministic name and a
// synthetic payload of s.PayloadBytes bytes drawn from seed, sent base64-encoded
// in the create request so Barbican stores it in one step. The payload bytes
// follow from seed alone, so the same scenario and seed store byte-identical
// secrets every run and a read-back can compare against the same bytes. The
// secret carries no metadata yet; StampSecret adds the run identity next. A
// quota rejection is wrapped with ErrQuota so the executor fails fast.
func (c *Client) StoreSecret(ctx context.Context, s barbicanplan.Secret, seed int64) (resource.Resource, error) {
	name := resourceName(c.runID, s.Name)
	opts := secrets.CreateOpts{
		Name:                   name,
		Payload:                base64.StdEncoding.EncodeToString(payload.Bytes(seed, int64(s.PayloadBytes))),
		PayloadContentType:     contentTypeOctetStream,
		PayloadContentEncoding: "base64",
		SecretType:             secrets.OpaqueSecret,
	}

	var id string
	err := c.timed(ctx, KindSecret, "create", func(ctx context.Context) error {
		created, err := secrets.Create(ctx, c.kv, opts).Extract()
		if err != nil {
			return err
		}
		id, err = idFromRef(created.SecretRef)
		return err
	})
	if err != nil {
		// A store that fails after the request reached Barbican (a canceled or lost
		// response, or a 5xx past commit) can leave a secret behind that carries no
		// run metadata and no place in the run record. Discovery confirms it by its
		// creator (confirmSecrets), so cleanup and the monitor sweep reclaim it when
		// they run as this token's user. Log the deterministic name so an operator
		// can still locate the orphan when they do not.
		slog.Warn("create failed; a secret with this name may be orphaned in the cloud",
			"name", name, "error", err)
		return resource.Resource{}, wrapCreate(KindSecret, s.Name, err)
	}
	return resource.Resource{Kind: KindSecret, Logical: s.Name, Name: name, ID: id}, nil
}

// StampSecret writes the run identity onto a stored or generated secret as
// Barbican secret metadata: dizzy_run=<run id> and dizzy_type=secret. The PUT
// replaces the secret's whole metadata set, so a retried stamp lands the same
// set again and is safe to repeat.
func (c *Client) StampSecret(ctx context.Context, r resource.Resource) error {
	md := secrets.MetadataOpts{metaRun: c.runID, metaType: string(KindSecret)}
	err := c.timed(ctx, KindSecret, "update", func(ctx context.Context) error {
		return secrets.CreateMetadata(ctx, c.kv, r.ID, md).Err
	})
	if err != nil {
		return fmt.Errorf("stamping metadata on secret %q: %w", r.Logical, err)
	}
	return nil
}

// RetrievePayload reads a stored secret's payload back as raw bytes and compares
// it with the n bytes drawn from seed that the secret was stored with. The
// comparison runs inside the timed download call, so a mismatch marks that
// sample failed with error kind mismatch. A mismatch returns ErrPayloadMismatch
// naming both lengths and the first differing byte offset; the message never
// carries payload bytes.
func (c *Client) RetrievePayload(ctx context.Context, r resource.Resource, seed int64, n int) error {
	want := payload.Bytes(seed, int64(n))
	opts := secrets.GetPayloadOpts{PayloadContentType: contentTypeOctetStream}
	err := c.timed(ctx, KindSecret, "download", func(ctx context.Context) error {
		got, err := secrets.GetPayload(ctx, c.kv, r.ID, opts).Extract()
		if err != nil {
			return err
		}
		return comparePayload(r.Logical, got, want)
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrPayloadMismatch):
		return err
	default:
		return fmt.Errorf("retrieving payload of secret %q: %w", r.Logical, err)
	}
}

// comparePayload returns nil when got equals want, and otherwise an
// ErrPayloadMismatch naming both lengths and the first index at which they
// differ, which is the shorter length when one is a prefix of the other.
func comparePayload(logical string, got, want []byte) error {
	if bytes.Equal(got, want) {
		return nil
	}
	n := min(len(got), len(want))
	offset := n
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			offset = i
			break
		}
	}
	return fmt.Errorf("%w: secret %q returned %d bytes, want %d, first difference at byte %d",
		ErrPayloadMismatch, logical, len(got), len(want), offset)
}

// ListSecretsByRun returns the secrets carrying this run's dizzy-<runID>- name
// prefix whose metadata confirms dizzy_run=<runID>, or whose metadata set is
// empty and whose creator is the client's user (SetCreatorID), the discovery
// step cleanup deletes from. The prefix alone is not enough: a secret whose
// stamp never landed is taken only on its creator, so discovery never deletes a
// secret the run cannot prove it owns. A candidate whose metadata read returns
// 404 was deleted meanwhile and is dropped. With a request timeout set
// (SetRequestTimeout), the listing and each metadata read are bounded one by
// one, so a large run's discovery is not cut short by a bound over the whole
// call.
func (c *Client) ListSecretsByRun(ctx context.Context, runID string) ([]resource.Resource, error) {
	candidates, err := c.listSecrets(ctx, runPrefix(runID))
	if err != nil {
		return nil, fmt.Errorf("listing secrets by run: %w", err)
	}
	return c.confirmSecrets(ctx, candidates, func(md map[string]string) bool {
		return md[metaRun] == runID
	})
}

// ListSecretsByType returns the secrets carrying the dizzy- name prefix of any
// tester run whose metadata confirms dizzy_type=secret, regardless of their
// dizzy_run, or whose metadata set is empty and whose creator is the client's
// user: the pre-flight sweep's handle for reclaiming a crashed iteration's
// secrets once its run id is lost. As in ListSecretsByRun, the prefix alone is
// never enough, so a secret whose stamp never landed is discovered only on its
// creator, and a candidate whose metadata read returns 404 is dropped.
func (c *Client) ListSecretsByType(ctx context.Context) ([]resource.Resource, error) {
	candidates, err := c.listSecrets(ctx, namePrefix)
	if err != nil {
		return nil, fmt.Errorf("listing secrets by type: %w", err)
	}
	return c.confirmSecrets(ctx, candidates, func(md map[string]string) bool {
		return md[metaType] == string(KindSecret)
	})
}

// secretCandidate is one prefix-matched entry of the secret list with the
// creator Barbican recorded for it, the user whose token stored the secret or
// submitted the order that generated it.
type secretCandidate struct {
	res     resource.Resource
	creator string
}

// confirmSecrets reads the metadata of each prefix-matched candidate and keeps,
// in list order, those whose metadata stamped accepts, whoever created them,
// and those whose metadata set is empty when the client's user (SetCreatorID)
// is their creator. StampSecret writes the whole set at once, so a secret of
// dizzy's carries either both run keys or none; one with any other metadata is
// not dizzy's unstamped secret and is dropped silently. An unstamped secret of
// another user is dropped with a warning naming it, and when the client knows
// no user, every unstamped candidate is dropped and one warning counts them. A
// candidate whose metadata read returns 404 was deleted meanwhile and is
// dropped; any other read error stops the discovery and is returned with the
// secret's id.
func (c *Client) confirmSecrets(ctx context.Context, candidates []secretCandidate, stamped func(md map[string]string) bool) ([]resource.Resource, error) {
	var found []resource.Resource
	var unconfirmed int
	for _, cand := range candidates {
		r := cand.res
		md, err := c.secretMetadata(ctx, r)
		switch {
		case IsNotFound(err):
			continue
		case err != nil:
			return nil, fmt.Errorf("reading metadata of secret %s: %w", r.ID, err)
		case stamped(md):
			found = append(found, r)
		case len(md) != 0:
			continue
		case c.creatorID == "":
			unconfirmed++
		case cand.creator != c.creatorID:
			slog.Warn("leaving a secret without run metadata; another user created it",
				"name", r.Name, "id", r.ID, "creator", cand.creator)
		default:
			slog.Info("confirmed a secret without run metadata by its creator", "name", r.Name, "id", r.ID)
			found = append(found, r)
		}
	}
	if unconfirmed > 0 {
		slog.Warn("leaving secrets without run metadata; the token names no user to confirm them by",
			"count", unconfirmed)
	}
	return found, nil
}

// listSecrets streams the project's secret list a page at a time and keeps only
// those whose name carries prefix, with each one's creator_id, recording the
// whole listing under the list op. The prefix match runs client side, so a
// secret the tool did not name is never returned. The whole paged listing is
// one request under the request timeout.
func (c *Client) listSecrets(ctx context.Context, prefix string) ([]secretCandidate, error) {
	var found []secretCandidate
	err := c.timed(ctx, KindSecret, "list", func(ctx context.Context) error {
		ctx, cancel := c.requestContext(ctx)
		defer cancel()
		found = nil
		return secrets.List(c.kv, secrets.ListOpts{Limit: listLimit}).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
			items, err := secrets.ExtractSecrets(page)
			if err != nil {
				return false, err
			}
			for _, s := range items {
				if !strings.HasPrefix(s.Name, prefix) {
					continue
				}
				id, err := idFromRef(s.SecretRef)
				if err != nil {
					return false, fmt.Errorf("secret %q: %w", s.Name, err)
				}
				found = append(found, secretCandidate{
					res:     resource.Resource{Kind: KindSecret, Name: s.Name, ID: id},
					creator: s.CreatorID,
				})
			}
			return true, nil
		})
	})
	return found, err
}

// secretMetadata reads a secret's metadata under its own request timeout,
// recording the call under the get op.
func (c *Client) secretMetadata(ctx context.Context, r resource.Resource) (map[string]string, error) {
	var md map[string]string
	err := c.timed(ctx, KindSecret, "get", func(ctx context.Context) error {
		ctx, cancel := c.requestContext(ctx)
		defer cancel()
		var err error
		md, err = secrets.GetMetadata(ctx, c.kv, r.ID).Extract()
		return err
	})
	return md, err
}
