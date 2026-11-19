package c5c3

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/B42Labs/dizzy/internal/resource"
)

// The data keys of a delivered Secret. Every Secret carries keyCloudsYAML; a
// user's also carries keyPassword, an application credential's keyAppCredID
// and keyAppCredSecret.
const (
	keyCloudsYAML    = "clouds.yaml"
	keyPassword      = "password"
	keyAppCredID     = "application_credential_id"
	keyAppCredSecret = "application_credential_secret"
)

// editedValue is what EditSecret writes into the key it edits, so the
// operator has a key it owns to rewrite.
const editedValue = "dizzy-edited"

// Credential is what a delivered Secret carries: the raw clouds.yaml
// document, the name of its one entry under clouds, the Secret's
// resourceVersion, and for a user the password, for an application credential
// order its id and secret, each read from the data key of the same name.
// Order is the order the Secret was delivered beside, which the Keystone
// checks read again to tell a rejected credential from a race with a change.
//
// Its String, Format and LogValue methods print only the resourceVersion and
// the application credential id, so neither fmt nor slog can leak the
// password, the secret or the document, which holds both. The secret-bearing
// fields are also left out of JSON.
type Credential struct {
	CloudsYAML                  []byte `json:"-"`
	Cloud                       string
	ResourceVersion             string
	Password                    string `json:"-"`
	ApplicationCredentialID     string
	ApplicationCredentialSecret string `json:"-"`
	Order                       resource.Resource
}

// String returns the Credential's resourceVersion and application credential
// id, and nothing it must keep secret.
func (c Credential) String() string {
	if c.ApplicationCredentialID == "" {
		return fmt.Sprintf("credential (resourceVersion %s)", c.ResourceVersion)
	}
	return fmt.Sprintf("credential (resourceVersion %s, application credential %s)", c.ResourceVersion, c.ApplicationCredentialID)
}

// Format writes String for every verb, %#v and %d included, which would
// otherwise print the fields.
func (c Credential) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.String())
}

// LogValue logs the Credential as its resourceVersion and application
// credential id.
func (c Credential) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("resourceVersion", c.ResourceVersion)}
	if c.ApplicationCredentialID != "" {
		attrs = append(attrs, slog.String("applicationCredentialID", c.ApplicationCredentialID))
	}
	return slog.GroupValue(attrs...)
}

// equal reports whether c carries the same document and owned keys as want.
// The resourceVersion is not compared: a rewritten Secret has a new one.
func (c Credential) equal(want Credential) bool {
	return bytes.Equal(c.CloudsYAML, want.CloudsYAML) &&
		c.Password == want.Password &&
		c.ApplicationCredentialID == want.ApplicationCredentialID &&
		c.ApplicationCredentialSecret == want.ApplicationCredentialSecret
}

// secretRef is the Secret delivered beside an order: the order it belongs to
// and the Secret's namespace and name.
type secretRef struct {
	order     target
	namespace string
	name      string
}

// id returns the Secret as <namespace>/<name>, for messages.
func (s secretRef) id() string {
	return orderID(s.namespace, s.name)
}

// ownedKeys returns the credential keys the operator writes into the Secret
// beside it, beside clouds.yaml.
func (s secretRef) ownedKeys() []string {
	if s.order.kind == KindUser {
		return []string{keyPassword}
	}
	return []string{keyAppCredID, keyAppCredSecret}
}

// resolveSecret resolves the Secret delivered beside the order r addresses.
// It returns an error for an order that is not a user or an application
// credential order, which deliver none, for a malformed id, and for an order
// whose name lacks the dizzyorder- prefix: dizzy touches no Secret that is not
// named after one of its own orders.
func resolveSecret(r resource.Resource) (secretRef, error) {
	t, err := parseTarget(r)
	if err != nil {
		return secretRef{}, err
	}
	if t.kind != KindUser && t.kind != KindApplicationCredential {
		return secretRef{}, fmt.Errorf("order %s is a %s order, which delivers no secret", r.ID, t.kind)
	}
	if !strings.HasPrefix(t.name, namePrefix) {
		return secretRef{}, fmt.Errorf("order %s is not named %s<runid>-<logical>; dizzy touches only the secrets of its own orders", r.ID, namePrefix)
	}
	return secretRef{order: t, namespace: t.namespace, name: secretName(t.name)}, nil
}

// resolveOwnSecret is resolveSecret for the calls that change a Secret. It
// also returns an error for an order whose name lacks this client's run
// prefix, dizzyorder-<runid>-: dizzy edits and deletes only the Secrets of its
// own run's orders. The calls that only read a Secret accept the orders of
// any run, because the pre-flight sweep reads the Secrets of other runs.
func (c *Client) resolveOwnSecret(r resource.Resource) (secretRef, error) {
	s, err := resolveSecret(r)
	if err != nil {
		return secretRef{}, err
	}
	if prefix := runPrefix(c.runID); !strings.HasPrefix(s.order.name, prefix) {
		return secretRef{}, fmt.Errorf("order %s is not named %s<logical>; dizzy edits and deletes only the secrets of its own run's orders",
			r.ID, prefix)
	}
	return s, nil
}

// secrets returns the dynamic client for Secrets in s's namespace.
func (c *Client) secrets(s secretRef) dynamic.ResourceInterface {
	return c.dyn.Resource(secretsGVR).Namespace(s.namespace)
}

// getSecret reads s once. It records nothing and returns the time the GET
// took, so the caller records the call with the error it decides on.
func (c *Client) getSecret(ctx context.Context, s secretRef) (*unstructured.Unstructured, time.Duration, error) {
	start := time.Now()
	u, err := c.secrets(s).Get(ctx, s.name, metav1.GetOptions{})
	return u, time.Since(start), err
}

// parseCredential reads the Credential of s from the Secret u. Every problem
// wraps ErrMismatch: a missing or empty clouds.yaml or owned key, a value
// that is not base64, and a document that does not hold exactly one entry
// under clouds. No error carries a value of the Secret.
func parseCredential(s secretRef, u *unstructured.Unstructured) (Credential, error) {
	data, ok := u.Object["data"].(map[string]any)
	if !ok {
		data = map[string]any{}
	}
	read := func(key string) ([]byte, error) {
		v, _ := data[key].(string)
		if v == "" {
			return nil, fmt.Errorf("%w: secret %s has no %s key, or an empty one", ErrMismatch, s.id(), key)
		}
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("%w: key %s of secret %s is not base64", ErrMismatch, key, s.id())
		}
		if len(b) == 0 {
			return nil, fmt.Errorf("%w: secret %s has no %s key, or an empty one", ErrMismatch, s.id(), key)
		}
		return b, nil
	}

	doc, err := read(keyCloudsYAML)
	if err != nil {
		return Credential{}, err
	}
	cloud, err := oneCloud(doc)
	if err != nil {
		return Credential{}, fmt.Errorf("%w: %s of secret %s %w", ErrMismatch, keyCloudsYAML, s.id(), err)
	}
	cred := Credential{CloudsYAML: doc, Cloud: cloud, ResourceVersion: u.GetResourceVersion()}

	owned := make(map[string]string, 2)
	for _, key := range s.ownedKeys() {
		v, err := read(key)
		if err != nil {
			return Credential{}, err
		}
		owned[key] = string(v)
	}
	cred.Password = owned[keyPassword]
	cred.ApplicationCredentialID = owned[keyAppCredID]
	cred.ApplicationCredentialSecret = owned[keyAppCredSecret]
	return cred, nil
}

// oneCloud returns the name of the one entry under clouds in doc. Its errors
// complete a sentence about the document and never quote it, because a YAML
// error can carry the text it failed on.
func oneCloud(doc []byte) (string, error) {
	var parsed struct {
		Clouds map[string]any `yaml:"clouds"`
	}
	if err := yaml.Unmarshal(doc, &parsed); err != nil {
		return "", errors.New("is not a clouds.yaml document")
	}
	if n := len(parsed.Clouds); n != 1 {
		return "", fmt.Errorf("holds %d entries under clouds, want 1", n)
	}
	for name := range parsed.Clouds {
		return name, nil
	}
	return "", nil
}

// ReadCredential reads the Secret delivered beside a ready user or application
// credential order, which must exist, as one get sample of kind secret. A
// 404, a missing or empty clouds.yaml or owned key, and a document that does
// not hold exactly one entry under clouds each return ErrMismatch, recorded on
// the sample with error kind mismatch. Any other failure is returned wrapped.
// The credential carries order as its Order.
func (c *Client) ReadCredential(ctx context.Context, order resource.Resource) (Credential, error) {
	s, err := resolveSecret(order)
	if err != nil {
		return Credential{}, err
	}
	u, d, err := c.getSecret(ctx, s)
	var cred Credential
	switch {
	case IsNotFound(err):
		err = fmt.Errorf("%w: secret %s of order %s is missing: %w", ErrMismatch, s.id(), order.ID, err)
	case err != nil:
		err = fmt.Errorf("reading secret %s: %w", s.id(), err)
	default:
		cred, err = parseCredential(s, u)
	}
	c.record(ctx, KindSecret, "get", d, err)
	if err != nil {
		return Credential{}, err
	}
	cred.Order = order
	return cred, nil
}

// LookupCredential is cleanup's ReadCredential, for an order that may have
// delivered nothing yet. It reads the Secret as one get sample of kind secret
// and reports false, with no error, when there is nothing to verify: a 404,
// which is recorded as a successful get, and a Secret it cannot parse, which
// it logs as a warning. Any other failure is returned wrapped. A credential it
// reads carries order as its Order.
func (c *Client) LookupCredential(ctx context.Context, order resource.Resource) (Credential, bool, error) {
	s, err := resolveSecret(order)
	if err != nil {
		return Credential{}, false, err
	}
	u, d, err := c.getSecret(ctx, s)
	switch {
	case IsNotFound(err):
		c.record(ctx, KindSecret, "get", d, nil)
		return Credential{}, false, nil
	case err != nil:
		c.record(ctx, KindSecret, "get", d, err)
		return Credential{}, false, fmt.Errorf("reading secret %s: %w", s.id(), err)
	}
	c.record(ctx, KindSecret, "get", d, nil)
	cred, err := parseCredential(s, u)
	if err != nil {
		slog.Warn("delivered secret cannot be parsed; its credential is not checked",
			"secret", s.id(), "order", order.ID, "error", err)
		return Credential{}, false, nil
	}
	cred.Order = order
	return cred, true, nil
}

// EditSecret overwrites one key the operator owns in the Secret beside a user
// or application credential order, password or application_credential_secret,
// with the base64 of dizzy-edited, as one merge patch and one update sample of
// kind secret. A 403 returns ErrForbidden naming checks.secret_repair, the
// key that turns the repair check off; a 409 is retryable. An order of another
// run is refused without a request.
func (c *Client) EditSecret(ctx context.Context, order resource.Resource) error {
	s, err := c.resolveOwnSecret(order)
	if err != nil {
		return err
	}
	key := keyPassword
	if s.order.kind == KindApplicationCredential {
		key = keyAppCredSecret
	}
	patch, err := json.Marshal(map[string]any{
		"data": map[string]string{key: base64.StdEncoding.EncodeToString([]byte(editedValue))},
	})
	if err != nil {
		return fmt.Errorf("encoding the edit of secret %s: %w", s.id(), err)
	}
	err = c.timed(ctx, KindSecret, "update", func(ctx context.Context) error {
		_, err := c.secrets(s).Patch(ctx, s.name, types.MergePatchType, patch, metav1.PatchOptions{})
		return wrapPatchError(err)
	})
	switch {
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%w: editing key %s of secret %s needs patch on secrets; "+
			"set checks.secret_repair: false for a deployment that grants only get on Secrets: %w",
			ErrForbidden, key, s.id(), err)
	case err != nil:
		return fmt.Errorf("editing key %s of secret %s: %w", key, s.id(), err)
	}
	return nil
}

// DeleteSecret deletes the Secret beside a user or application credential
// order as one delete sample of kind secret, for the operator to write it
// again. A 403 returns ErrForbidden naming checks.secret_repair. A 404 is
// returned wrapped: the repair check deletes a Secret it has just read. An
// order of another run is refused without a request.
func (c *Client) DeleteSecret(ctx context.Context, order resource.Resource) error {
	s, err := c.resolveOwnSecret(order)
	if err != nil {
		return err
	}
	err = c.timed(ctx, KindSecret, "delete", func(ctx context.Context) error {
		return c.secrets(s).Delete(ctx, s.name, metav1.DeleteOptions{})
	})
	switch {
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%w: deleting secret %s needs delete on secrets; "+
			"set checks.secret_repair: false for a deployment that grants only get on Secrets: %w",
			ErrForbidden, s.id(), err)
	case err != nil:
		return fmt.Errorf("deleting secret %s: %w", s.id(), err)
	}
	return nil
}

// WaitSecretRestored polls the Secret beside a user or application credential
// order until it exists and carries want's clouds.yaml and owned keys again,
// and records one repair sample of kind secret over the whole wait. Each poll
// is one get sample of kind secret. A 404 is a Secret the operator has not
// written again yet and is recorded as a successful get, like a Secret that
// still differs; any other failed GET is recorded as failed and polled
// through. It returns ctx.Err() when the context ends first.
func (c *Client) WaitSecretRestored(ctx context.Context, order resource.Resource, want Credential) error {
	s, err := resolveSecret(order)
	if err != nil {
		return err
	}
	start := time.Now()
	err = c.poll(ctx, func(ctx context.Context) (bool, error) {
		u, d, err := c.getSecret(ctx, s)
		switch {
		case IsNotFound(err):
			c.record(ctx, KindSecret, "get", d, nil)
			return false, nil
		case err != nil:
			c.record(ctx, KindSecret, "get", d, err)
			return false, nil
		}
		c.record(ctx, KindSecret, "get", d, nil)
		got, err := parseCredential(s, u)
		return err == nil && got.equal(want), nil
	})
	c.record(ctx, KindSecret, "repair", time.Since(start), err)
	return err
}

// WaitSecretGone polls the Secret beside a deleted user or application
// credential order until the garbage collector has removed it, each poll one
// get sample of kind secret and the final 404 a successful one, and records
// one finalize sample of kind secret over the whole wait. A Secret still
// present at the context's deadline returns ErrMismatch; a cancelled context
// returns ctx.Err().
func (c *Client) WaitSecretGone(ctx context.Context, order resource.Resource) error {
	s, err := resolveSecret(order)
	if err != nil {
		return err
	}
	start := time.Now()
	err = c.poll(ctx, func(ctx context.Context) (bool, error) {
		_, d, err := c.getSecret(ctx, s)
		if IsNotFound(err) {
			c.record(ctx, KindSecret, "get", d, nil)
			return true, nil
		}
		c.record(ctx, KindSecret, "get", d, err)
		return false, nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("%w: secret %s is still present after order %s is gone: %w", ErrMismatch, s.id(), order.ID, err)
	}
	c.record(ctx, KindSecret, "finalize", time.Since(start), err)
	return err
}
