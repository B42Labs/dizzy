package c5c3

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stesting "k8s.io/client-go/testing"

	"github.com/B42Labs/dizzy/internal/resource"
)

// The secret values the tests deliver. No error, log line or formatted
// Credential may carry either.
const (
	testPassword      = "s3cr3t-password"
	testAppCredSecret = "s3cr3t-appcred"
)

// appcredDocument is a delivered application credential clouds.yaml.
func appcredDocument(id, secret string) string {
	return `clouds:
  admin:
    auth_type: "v3applicationcredential"
    auth:
      auth_url: "http://controlplane-keystone.openstack.svc:5000/v3"
      application_credential_id: "` + id + `"
      application_credential_secret: "` + secret + `"
    region_name: "RegionOne"
    identity_api_version: 3
`
}

// appcredSecret returns the Secret delivered beside the application
// credential order, at resourceVersion rv.
func appcredSecret(id, secret, rv string) *unstructured.Unstructured {
	return secretObject(secretName(resourceName(testRun, "appcred")), rv, map[string]string{
		keyCloudsYAML:    appcredDocument(id, secret),
		keyAppCredID:     id,
		keyAppCredSecret: secret,
	})
}

// userSecretName is the name of the Secret beside the user order.
var userSecretName = secretName(resourceName(testRun, "user"))

// TestReadCredential confirms ReadCredential reads the Secret named after the
// order in the order's namespace, returns the document, its one entry, the
// resourceVersion and the owned keys of a user and a credential order, and is
// one successful get sample of kind secret.
func TestReadCredential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		order  resource.Resource
		secret *unstructured.Unstructured
		want   Credential
	}{
		{"user", orderRef(KindUser, "user"), userSecret(testPassword, "12"),
			Credential{CloudsYAML: []byte(userDocument(testPassword)), Cloud: "admin", ResourceVersion: "12", Password: testPassword}},
		{"application credential", orderRef(KindApplicationCredential, "appcred"), appcredSecret("ac-1", testAppCredSecret, "13"),
			Credential{CloudsYAML: []byte(appcredDocument("ac-1", testAppCredSecret)), Cloud: "admin", ResourceVersion: "13",
				ApplicationCredentialID: "ac-1", ApplicationCredentialSecret: testAppCredSecret}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, dyn, m := testClient()
			seed(t, dyn, KindSecret, tc.secret)

			got, err := c.ReadCredential(context.Background(), tc.order)
			if err != nil {
				t.Fatalf("ReadCredential: %v", err)
			}
			if !got.equal(tc.want) || got.Cloud != tc.want.Cloud || got.ResourceVersion != tc.want.ResourceVersion {
				t.Errorf("ReadCredential = %v (cloud %q), want %v (cloud %q)", got, got.Cloud, tc.want, tc.want.Cloud)
			}
			get := dyn.Actions()[0].(k8stesting.GetActionImpl)
			if get.GetResource() != secretsGVR || get.GetNamespace() != testNamespace || get.GetName() != tc.secret.GetName() {
				t.Errorf("get of %v %s/%s, want secrets %s/%s", get.GetResource(), get.GetNamespace(), get.GetName(), testNamespace, tc.secret.GetName())
			}
			if st := statsOf(m.Aggregate(time.Second), KindSecret); st.Attempted != 1 || st.Succeeded != 1 {
				t.Errorf("secret samples = %+v, want 1 succeeded get", st)
			}
		})
	}
}

// TestReadCredentialMismatch confirms a missing Secret, a missing or empty
// clouds.yaml or owned key, a value that is not base64, and a document that
// does not hold exactly one entry under clouds each return ErrMismatch,
// recorded as a get sample of kind secret with error kind mismatch, and that
// no error quotes a secret value.
func TestReadCredentialMismatch(t *testing.T) {
	user := orderRef(KindUser, "user")
	appcred := orderRef(KindApplicationCredential, "appcred")
	twoClouds := userDocument(testPassword) + `  other:
    auth:
      auth_url: "http://other.invalid/v3"
`
	notBase64 := userSecret(testPassword, "1")
	notBase64.Object["data"].(map[string]any)[keyPassword] = "not base64: " + testPassword
	for _, tc := range []struct {
		name   string
		order  resource.Resource
		secret *unstructured.Unstructured
	}{
		{"missing secret", user, nil},
		{"no clouds.yaml key", user, secretObject(userSecretName, "1", map[string]string{keyPassword: testPassword})},
		{"empty clouds.yaml", user, secretObject(userSecretName, "1", map[string]string{keyCloudsYAML: "", keyPassword: testPassword})},
		{"two entries", user, secretObject(userSecretName, "1", map[string]string{keyCloudsYAML: twoClouds, keyPassword: testPassword})},
		{"no entry", user, secretObject(userSecretName, "1", map[string]string{keyCloudsYAML: "clouds: {}\n", keyPassword: testPassword})},
		{"no clouds key", user, secretObject(userSecretName, "1", map[string]string{keyCloudsYAML: "profiles: {}\n", keyPassword: testPassword})},
		{"not a clouds.yaml document", user, secretObject(userSecretName, "1", map[string]string{keyCloudsYAML: testPassword, keyPassword: testPassword})},
		{"no password key", user, secretObject(userSecretName, "1", map[string]string{keyCloudsYAML: userDocument(testPassword)})},
		{"empty password", user, secretObject(userSecretName, "1", map[string]string{keyCloudsYAML: userDocument(testPassword), keyPassword: ""})},
		{"password not base64", user, notBase64},
		{"no data", user, func() *unstructured.Unstructured {
			u := userSecret(testPassword, "1")
			delete(u.Object, "data")
			return u
		}()},
		{"no application_credential_secret", appcred, secretObject(secretName(appcred.Name), "1", map[string]string{
			keyCloudsYAML: appcredDocument("ac-1", testAppCredSecret), keyAppCredID: "ac-1"})},
		{"no application_credential_id", appcred, secretObject(secretName(appcred.Name), "1", map[string]string{
			keyCloudsYAML: appcredDocument("ac-1", testAppCredSecret), keyAppCredSecret: testAppCredSecret})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, dyn, m, reader := telemetryTestClient(t)
			if tc.secret != nil {
				seed(t, dyn, KindSecret, tc.secret)
			}

			_, err := c.ReadCredential(context.Background(), tc.order)
			if !errors.Is(err, ErrMismatch) {
				t.Fatalf("ReadCredential = %v, want ErrMismatch", err)
			}
			if errKind(err) != "mismatch" {
				t.Errorf("errKind = %q, want mismatch", errKind(err))
			}
			for _, secret := range []string{testPassword, testAppCredSecret} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q carries a secret value", err)
				}
			}
			agg := m.Aggregate(time.Second)
			if st := statsOf(agg, KindSecret); st.Attempted != 1 || st.Failed != 1 {
				t.Errorf("secret samples = %+v, want 1 failed get", st)
			}
			if got := countOf(metricPoints(t, reader, "dizzy.operation.errors"),
				map[string]string{"kind": "secret", "operation": "get", "error.kind": "mismatch"}); got != 1 {
				t.Errorf("secret/get mismatch errors = %d, want 1", got)
			}
		})
	}
}

// TestReadCredentialMissingWrapsNotFound confirms the mismatch of a missing
// Secret still wraps the 404.
func TestReadCredentialMissingWrapsNotFound(t *testing.T) {
	c, _, _ := testClient()
	_, err := c.ReadCredential(context.Background(), orderRef(KindUser, "user"))
	if !errors.Is(err, ErrMismatch) || !apierrors.IsNotFound(err) {
		t.Errorf("ReadCredential = %v, want ErrMismatch wrapping the 404", err)
	}
}

// TestReadCredentialServerError confirms a failed GET other than a 404 is
// returned wrapped, retryable, and not a mismatch.
func TestReadCredentialServerError(t *testing.T) {
	c, dyn, m := testClient()
	failVerb(dyn, "get", "secrets", internalError())
	_, err := c.ReadCredential(context.Background(), orderRef(KindUser, "user"))
	if !apierrors.IsInternalError(err) || errors.Is(err, ErrMismatch) || !IsRetryable(err) {
		t.Errorf("ReadCredential = %v, want a retryable 500 that is no mismatch", err)
	}
	if got := errorCount(m.Aggregate(time.Second), "http_500"); got != 1 {
		t.Errorf("http_500 samples = %d, want 1", got)
	}
}

// TestSecretCallsRefuseOtherSecrets confirms every Secret call refuses an
// order kind that delivers no Secret and an order not named like a dizzy
// order, without a request.
func TestSecretCallsRefuseOtherSecrets(t *testing.T) {
	ctx := context.Background()
	foreign := resource.Resource{Kind: KindUser, Logical: "user", Name: "workflow", ID: testNamespace + "/workflow"}
	for _, r := range []resource.Resource{
		orderRef(KindProject, "project"), orderRef(KindRoleAssignment, "role"), orderRef(KindCatalogEntry, "catalog"), foreign,
	} {
		t.Run(r.Name, func(t *testing.T) {
			c, dyn, _ := testClient()
			errs := []error{
				func() error { _, err := c.ReadCredential(ctx, r); return err }(),
				func() error { _, _, err := c.LookupCredential(ctx, r); return err }(),
				c.EditSecret(ctx, r),
				c.DeleteSecret(ctx, r),
				c.WaitSecretRestored(ctx, r, Credential{}),
				c.WaitSecretGone(ctx, r),
			}
			for i, err := range errs {
				if err == nil {
					t.Errorf("call %d on %s = nil, want an error", i, r.ID)
				}
			}
			if got := len(dyn.Actions()); got != 0 {
				t.Errorf("requests = %d, want 0", got)
			}
		})
	}
}

// TestCredentialHidesSecrets confirms no fmt verb, slog handler or JSON
// encoding of a Credential carries its password, its secret or its
// document, and that the resourceVersion and credential id stay readable.
func TestCredentialHidesSecrets(t *testing.T) {
	cred := Credential{
		CloudsYAML:                  []byte(appcredDocument("ac-1", testAppCredSecret) + "# " + testPassword),
		Cloud:                       "admin",
		ResourceVersion:             "12",
		Password:                    testPassword,
		ApplicationCredentialID:     "ac-1",
		ApplicationCredentialSecret: testAppCredSecret,
	}
	var outputs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"} {
		outputs = append(outputs, fmt.Sprintf(verb, cred), fmt.Sprintf(verb, &cred))
	}
	outputs = append(outputs, cred.String(), fmt.Sprint(cred), fmt.Sprint([]Credential{cred}))
	for _, h := range []func(*bytes.Buffer) slog.Handler{
		func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var b bytes.Buffer
		slog.New(h(&b)).Info("credential", "cred", cred, "ptr", &cred)
		outputs = append(outputs, b.String())
	}
	encoded, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	outputs = append(outputs, string(encoded))

	for _, out := range outputs {
		for _, secret := range []string{testPassword, testAppCredSecret, "auth_url"} {
			if strings.Contains(out, secret) {
				t.Errorf("output %q carries %q", out, secret)
			}
		}
	}
	if s := fmt.Sprint(cred); !strings.Contains(s, "12") || !strings.Contains(s, "ac-1") {
		t.Errorf("String = %q, want the resourceVersion and the credential id", s)
	}
}

// TestLookupCredential confirms cleanup's lookup reports a missing Secret as
// false, nil with a successful get, a Secret it cannot parse as false, nil
// with a warning that carries no secret value, a readable Secret as its
// credential, and a failed GET as an error.
func TestLookupCredential(t *testing.T) {
	user := orderRef(KindUser, "user")
	t.Run("missing", func(t *testing.T) {
		c, _, m := testClient()
		cred, ok, err := c.LookupCredential(context.Background(), user)
		if ok || err != nil || !cred.equal(Credential{}) {
			t.Fatalf("LookupCredential = (%v, %v, %v), want (zero, false, nil)", cred, ok, err)
		}
		agg := m.Aggregate(time.Second)
		if st := statsOf(agg, KindSecret); st.Attempted != 1 || st.Succeeded != 1 || len(agg.Errors) != 0 {
			t.Errorf("secret samples = %+v, errors %v, want 1 successful get", st, agg.Errors)
		}
	})
	t.Run("readable", func(t *testing.T) {
		c, dyn, _ := testClient()
		seed(t, dyn, KindSecret, userSecret(testPassword, "3"))
		cred, ok, err := c.LookupCredential(context.Background(), user)
		if !ok || err != nil || cred.Password != testPassword || cred.ResourceVersion != "3" {
			t.Fatalf("LookupCredential = (%v, %v, %v), want the user's credential", cred, ok, err)
		}
	})
	t.Run("unparseable", func(t *testing.T) {
		logs := captureLog(t)
		c, dyn, m := testClient()
		seed(t, dyn, KindSecret, secretObject(userSecretName, "1", map[string]string{keyCloudsYAML: testPassword, keyPassword: testPassword}))
		cred, ok, err := c.LookupCredential(context.Background(), user)
		if ok || err != nil || !cred.equal(Credential{}) {
			t.Fatalf("LookupCredential = (%v, %v, %v), want (zero, false, nil)", cred, ok, err)
		}
		if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), userSecretName) {
			t.Errorf("log %q has no warning naming the secret", logs.String())
		}
		if strings.Contains(logs.String(), testPassword) {
			t.Errorf("log %q carries the password", logs.String())
		}
		if st := statsOf(m.Aggregate(time.Second), KindSecret); st.Attempted != 1 || st.Succeeded != 1 {
			t.Errorf("secret samples = %+v, want 1 successful get", st)
		}
	})
	t.Run("500", func(t *testing.T) {
		c, dyn, _ := testClient()
		failVerb(dyn, "get", "secrets", internalError())
		if _, ok, err := c.LookupCredential(context.Background(), user); ok || !apierrors.IsInternalError(err) {
			t.Errorf("LookupCredential = (%v, %v), want (false, the 500)", ok, err)
		}
	})
}

// decodedData returns the decoded data of the stored Secret name.
func decodedData(t *testing.T, dyn interface {
	Tracker() k8stesting.ObjectTracker
}, name string) map[string]string {
	t.Helper()
	obj, err := dyn.Tracker().Get(secretsGVR, testNamespace, name)
	if err != nil {
		t.Fatalf("reading secret %s: %v", name, err)
	}
	out := map[string]string{}
	for k, v := range obj.(*unstructured.Unstructured).Object["data"].(map[string]any) {
		b, err := base64.StdEncoding.DecodeString(v.(string))
		if err != nil {
			t.Fatalf("key %s is not base64: %v", k, err)
		}
		out[k] = string(b)
	}
	return out
}

// TestEditSecret confirms EditSecret merge-patches the password of a user's
// Secret, and the application_credential_secret of a credential's, to
// dizzy-edited and leaves the other keys alone, as one update sample of kind
// secret.
func TestEditSecret(t *testing.T) {
	for _, tc := range []struct {
		name   string
		order  resource.Resource
		secret *unstructured.Unstructured
		want   map[string]string
		patch  string
	}{
		{"user", orderRef(KindUser, "user"), userSecret(testPassword, "1"),
			map[string]string{keyCloudsYAML: userDocument(testPassword), keyPassword: "dizzy-edited"},
			`{"data":{"password":"ZGl6enktZWRpdGVk"}}`},
		{"application credential", orderRef(KindApplicationCredential, "appcred"), appcredSecret("ac-1", testAppCredSecret, "1"),
			map[string]string{keyCloudsYAML: appcredDocument("ac-1", testAppCredSecret), keyAppCredID: "ac-1", keyAppCredSecret: "dizzy-edited"},
			`{"data":{"application_credential_secret":"ZGl6enktZWRpdGVk"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, dyn, m := testClient()
			seed(t, dyn, KindSecret, tc.secret)

			if err := c.EditSecret(context.Background(), tc.order); err != nil {
				t.Fatalf("EditSecret: %v", err)
			}
			if got := decodedData(t, dyn, tc.secret.GetName()); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("data = %v, want %v", got, tc.want)
			}
			patch := dyn.Actions()[0].(k8stesting.PatchActionImpl)
			if patch.GetResource() != secretsGVR || patch.GetName() != tc.secret.GetName() ||
				string(patch.GetPatchType()) != "application/merge-patch+json" || string(patch.GetPatch()) != tc.patch {
				t.Errorf("patch %s %s %s, want a merge patch %s of %s", patch.GetName(), patch.GetPatchType(), patch.GetPatch(), tc.patch, tc.secret.GetName())
			}
			if st := statsOf(m.Aggregate(time.Second), KindSecret); st.Attempted != 1 || st.Succeeded != 1 {
				t.Errorf("secret samples = %+v, want 1 update", st)
			}
		})
	}
}

// TestDeleteSecret confirms DeleteSecret deletes the Secret beside the order
// as one delete sample of kind secret and returns a 404 wrapped.
func TestDeleteSecret(t *testing.T) {
	c, dyn, m := testClient()
	user := orderRef(KindUser, "user")
	seed(t, dyn, KindSecret, userSecret(testPassword, "1"))

	if err := c.DeleteSecret(context.Background(), user); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if _, err := dyn.Tracker().Get(secretsGVR, testNamespace, userSecretName); !apierrors.IsNotFound(err) {
		t.Errorf("secret after DeleteSecret: %v, want gone", err)
	}
	if err := c.DeleteSecret(context.Background(), user); !apierrors.IsNotFound(err) {
		t.Errorf("DeleteSecret of a missing secret = %v, want the 404", err)
	}
	if st := statsOf(m.Aggregate(time.Second), KindSecret); st.Attempted != 2 || st.Succeeded != 1 {
		t.Errorf("secret samples = %+v, want 2 deletes with 1 succeeded", st)
	}
}

// TestSecretRepairForbidden confirms a 403 to EditSecret or DeleteSecret
// returns ErrForbidden wrapping the 403, with a message naming
// checks.secret_repair.
func TestSecretRepairForbidden(t *testing.T) {
	for _, tc := range []struct {
		verb string
		call func(*Client, resource.Resource) error
	}{
		{"patch", func(c *Client, r resource.Resource) error { return c.EditSecret(context.Background(), r) }},
		{"delete", func(c *Client, r resource.Resource) error { return c.DeleteSecret(context.Background(), r) }},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			c, dyn, m := testClient()
			seed(t, dyn, KindSecret, userSecret(testPassword, "1"))
			failVerb(dyn, tc.verb, "secrets", forbidden("secrets", userSecretName))

			err := tc.call(c, orderRef(KindUser, "user"))
			if !errors.Is(err, ErrForbidden) || !apierrors.IsForbidden(err) {
				t.Fatalf("%s = %v, want ErrForbidden wrapping the 403", tc.verb, err)
			}
			if !strings.Contains(err.Error(), "checks.secret_repair") {
				t.Errorf("error %q does not name checks.secret_repair", err)
			}
			if errKind(err) != "http_403" || IsRetryable(err) {
				t.Errorf("errKind = %q, retryable %v, want http_403 and not retryable", errKind(err), IsRetryable(err))
			}
			if got := errorCount(m.Aggregate(time.Second), "http_403"); got != 1 {
				t.Errorf("http_403 samples = %d, want 1", got)
			}
		})
	}
}

// TestWaitSecretRestored confirms the repair wait polls an edited and a
// deleted Secret until it carries the wanted document and keys again, counts
// a 404 poll as a successful get, records one repair sample of kind secret,
// and returns context.DeadlineExceeded when the Secret is not restored.
func TestWaitSecretRestored(t *testing.T) {
	user := orderRef(KindUser, "user")
	want := Credential{CloudsYAML: []byte(userDocument(testPassword)), Password: testPassword}
	edited := secretObject(userSecretName, "2", map[string]string{keyCloudsYAML: userDocument(testPassword), keyPassword: "dizzy-edited"})
	restored := userSecret(testPassword, "3")
	for _, tc := range []struct {
		name    string
		answers []any
	}{
		{"edited", []any{edited, edited, restored}},
		{"deleted", []any{notFound("secrets", userSecretName), notFound("secrets", userSecretName), restored}},
		{"rewritten with another document first", []any{
			secretObject(userSecretName, "2", map[string]string{keyCloudsYAML: userDocument("other"), keyPassword: testPassword}), restored}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, dyn, m, reader := telemetryTestClient(t)
			script(t, dyn, "secrets", tc.answers...)

			if err := c.WaitSecretRestored(shortWait(t, 5*time.Second), user, want); err != nil {
				t.Fatalf("WaitSecretRestored: %v", err)
			}
			gets := countActions(dyn, "get", "secrets")
			if gets != len(tc.answers) {
				t.Errorf("gets = %d, want %d", gets, len(tc.answers))
			}
			agg := m.Aggregate(time.Second)
			if st := statsOf(agg, KindSecret); st.Attempted != gets+1 || st.Succeeded != gets+1 {
				t.Errorf("secret samples = %+v, want %d successful gets and a repair", st, gets)
			}
			if got := countOf(metricPoints(t, reader, "dizzy.operation.duration"),
				map[string]string{"kind": "secret", "operation": "repair", "outcome": "success"}); got != 1 {
				t.Errorf("successful repairs = %d, want 1", got)
			}
		})
	}
	t.Run("deadline", func(t *testing.T) {
		c, dyn, _, reader := telemetryTestClient(t)
		seed(t, dyn, KindSecret, edited)
		if err := c.WaitSecretRestored(shortWait(t, 50*time.Millisecond), user, want); err != context.DeadlineExceeded {
			t.Fatalf("WaitSecretRestored = %v, want context.DeadlineExceeded", err)
		}
		if got := countOf(metricPoints(t, reader, "dizzy.operation.duration"),
			map[string]string{"kind": "secret", "operation": "repair", "outcome": "timeout"}); got != 1 {
			t.Errorf("repair timeouts = %d, want 1", got)
		}
	})
}

// TestWaitSecretGone confirms the wait polls a present Secret until its 404,
// the final one a successful get, returns ErrMismatch when the Secret is
// still present at the deadline, and context.Canceled when cancelled.
func TestWaitSecretGone(t *testing.T) {
	user := orderRef(KindUser, "user")
	t.Run("gone", func(t *testing.T) {
		c, dyn, m := testClient()
		present := userSecret(testPassword, "1")
		script(t, dyn, "secrets", present, internalError(), present, notFound("secrets", userSecretName))
		if err := c.WaitSecretGone(shortWait(t, 5*time.Second), user); err != nil {
			t.Fatalf("WaitSecretGone: %v", err)
		}
		agg := m.Aggregate(time.Second)
		// Four gets and one finalize; only the 500 failed.
		if st := statsOf(agg, KindSecret); st.Attempted != 5 || st.Failed != 1 {
			t.Errorf("secret samples = %+v, want 5 with 1 failed", st)
		}
		if got := errorCount(agg, "http_404"); got != 0 {
			t.Errorf("http_404 samples = %d, want 0", got)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		c, dyn, m := testClient()
		seed(t, dyn, KindSecret, userSecret(testPassword, "1"))
		err := c.WaitSecretGone(shortWait(t, 50*time.Millisecond), user)
		if !errors.Is(err, ErrMismatch) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitSecretGone = %v, want ErrMismatch at the deadline", err)
		}
		if strings.Contains(err.Error(), testPassword) {
			t.Errorf("error %q carries the password", err)
		}
		if got := errorCount(m.Aggregate(time.Second), "mismatch"); got != 1 {
			t.Errorf("mismatch samples = %d, want 1", got)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		c, dyn, _ := testClient()
		seed(t, dyn, KindSecret, userSecret(testPassword, "1"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.WaitSecretGone(ctx, user); err != context.Canceled {
			t.Fatalf("WaitSecretGone = %v, want context.Canceled", err)
		}
	})
}
