package c5c3

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic/fake"

	"github.com/B42Labs/dizzy/internal/resource"
)

// deliveredAuthURL is the auth_url of the documents userDocument and
// appcredDocument build. The Keystone tests point it at a test server.
const deliveredAuthURL = "http://controlplane-keystone.openstack.svc:5000/v3"

// secretMark starts both secret values the tests deliver. No error or log
// line may carry it: not the whole secret, and not the prefix a YAML error
// quotes either.
const secretMark = "s3cr3t"

// The Keystone names the tokens and checks of the tests use.
const (
	testProjectName = "dizzyorder-run0-project"
	testDomainName  = "Default"
	testRole        = "member"
	testServiceType = "dizzy-probe"
	testServiceName = "dizzyorder-run0-catalog"
	testEndpointURL = "https://dizzy-probe.invalid/v1"
	testAppCredID   = "ac-1"
)

// unauthorizedBody is what Keystone answers a rejected login with.
const unauthorizedBody = `{"error": {"code": 401, "message": "The request you have made requires authentication.", "title": "Unauthorized"}}`

// tokenAnswer is one answer of a fakeKeystone: a 201 whose body's token object
// is token, or status with body.
type tokenAnswer struct {
	status int
	token  map[string]any
	body   string
}

// issued answers a login with a token whose object is token.
func issued(token map[string]any) tokenAnswer {
	return tokenAnswer{status: http.StatusCreated, token: token}
}

// answered answers a login with status and a Keystone error body for it.
func answered(status int) tokenAnswer {
	if status == http.StatusUnauthorized {
		return tokenAnswer{status: status, body: unauthorizedBody}
	}
	return tokenAnswer{status: status, body: fmt.Sprintf(`{"error": {"code": %d, "message": "status %d", "title": "Error"}}`, status, status)}
}

// fakeKeystone is an httptest Keystone that serves POST /v3/auth/tokens,
// records the decoded body of every request, and answers with its answers in
// turn, repeating the last.
type fakeKeystone struct {
	t       *testing.T
	srv     *httptest.Server
	answers []tokenAnswer

	mu     sync.Mutex
	bodies []map[string]any
}

// newFakeKeystone starts a plain HTTP fakeKeystone that is closed when the
// test ends.
func newFakeKeystone(t *testing.T, answers ...tokenAnswer) *fakeKeystone {
	t.Helper()
	return startFakeKeystone(t, false, answers)
}

// newTLSFakeKeystone starts a fakeKeystone that serves HTTPS with a
// certificate of its own.
func newTLSFakeKeystone(t *testing.T, answers ...tokenAnswer) *fakeKeystone {
	t.Helper()
	return startFakeKeystone(t, true, answers)
}

func startFakeKeystone(t *testing.T, useTLS bool, answers []tokenAnswer) *fakeKeystone {
	t.Helper()
	if len(answers) == 0 {
		t.Fatal("a fake Keystone needs at least one answer")
	}
	k := &fakeKeystone{t: t, answers: answers}
	k.srv = httptest.NewUnstartedServer(http.HandlerFunc(k.serve))
	// A refused TLS handshake is part of a test, not news for its output.
	k.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	if useTLS {
		k.srv.StartTLS()
	} else {
		k.srv.Start()
	}
	t.Cleanup(k.srv.Close)
	return k
}

// serve records a token request and answers it.
func (k *fakeKeystone) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v3/auth/tokens" {
		k.t.Errorf("fake Keystone got %s %s, want POST /v3/auth/tokens", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		k.t.Errorf("decoding a token request: %v", err)
	}
	k.mu.Lock()
	n := len(k.bodies)
	k.bodies = append(k.bodies, body)
	k.mu.Unlock()

	a := k.answers[min(n, len(k.answers)-1)]
	w.Header().Set("Content-Type", "application/json")
	if a.status == http.StatusCreated {
		w.Header().Set("X-Subject-Token", "token-"+strconv.Itoa(n))
		w.WriteHeader(a.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": a.token})
		return
	}
	w.WriteHeader(a.status)
	_, _ = io.WriteString(w, a.body)
}

// authURL is the versioned identity endpoint of the fake.
func (k *fakeKeystone) authURL() string {
	return k.srv.URL + "/v3"
}

// requests returns the decoded bodies of the token requests received so far.
func (k *fakeKeystone) requests() []map[string]any {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.bodies)
}

// count returns how many token requests the fake received.
func (k *fakeKeystone) count() int {
	return len(k.requests())
}

// keystoneToken returns the token object of a 201 for the ordered user,
// scoped to project when it is not empty, with roles on it, and listing
// services in its catalog.
func keystoneToken(project string, roles []string, services ...map[string]any) map[string]any {
	domain := map[string]any{"id": "default", "name": testDomainName}
	catalog := make([]any, 0, len(services))
	for _, s := range services {
		catalog = append(catalog, s)
	}
	token := map[string]any{
		"methods":    []string{"password"},
		"user":       map[string]any{"id": "u-1", "name": resourceName(testRun, "user"), "domain": domain},
		"issued_at":  "2026-11-19T11:00:00.000000Z",
		"expires_at": "2026-11-19T12:00:00.000000Z",
		"catalog":    catalog,
	}
	if project != "" {
		token["project"] = map[string]any{"id": "p-1", "name": project, "domain": domain}
		granted := make([]any, 0, len(roles))
		for i, r := range roles {
			granted = append(granted, map[string]any{"id": "r-" + strconv.Itoa(i), "name": r})
		}
		token["roles"] = granted
	}
	return token
}

// catalogService returns a catalog entry of type typ named name with
// endpoints.
func catalogService(typ, name string, endpoints ...map[string]any) map[string]any {
	eps := make([]any, 0, len(endpoints))
	for _, ep := range endpoints {
		eps = append(eps, ep)
	}
	return map[string]any{"id": "s-" + name, "type": typ, "name": name, "endpoints": eps}
}

// catalogEndpoint returns a catalog endpoint of interface iface at url.
func catalogEndpoint(iface, url string) map[string]any {
	return map[string]any{"id": "e-" + iface, "interface": iface, "region": "RegionOne", "region_id": "RegionOne", "url": url}
}

// orderedEntry is the catalog entry the plan orders.
func orderedEntry() map[string]any {
	return catalogService(testServiceType, testServiceName, catalogEndpoint("public", testEndpointURL))
}

// bodyField returns the value at path in a decoded request body, nil when a
// step of the path is missing.
func bodyField(body map[string]any, path ...string) any {
	var v any = body
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// atAuthURL returns doc with its auth_url replaced by authURL.
func atAuthURL(t *testing.T, doc, authURL string) string {
	t.Helper()
	if strings.Count(doc, deliveredAuthURL) != 1 {
		t.Fatalf("the document does not name %s exactly once", deliveredAuthURL)
	}
	return strings.Replace(doc, deliveredAuthURL, authURL, 1)
}

// deliveredUserSecret returns the Secret beside the user order at
// resourceVersion rv, whose document logs in at authURL with testPassword.
func deliveredUserSecret(t *testing.T, authURL, rv string) *unstructured.Unstructured {
	t.Helper()
	return secretObject(userSecretName, rv, map[string]string{
		keyCloudsYAML: atAuthURL(t, userDocument(testPassword), authURL),
		keyPassword:   testPassword,
	})
}

// deliveredAppcredSecret returns the Secret beside the application credential
// order at resourceVersion rv, whose document logs in at authURL with
// testAppCredID and testAppCredSecret.
func deliveredAppcredSecret(t *testing.T, authURL, rv string) *unstructured.Unstructured {
	t.Helper()
	return secretObject(secretName(resourceName(testRun, "appcred")), rv, map[string]string{
		keyCloudsYAML:    atAuthURL(t, appcredDocument(testAppCredID, testAppCredSecret), authURL),
		keyAppCredID:     testAppCredID,
		keyAppCredSecret: testAppCredSecret,
	})
}

// deliverUser seeds the user order, ready, and the Secret beside it at
// resourceVersion 5 with a document that logs in at authURL, and returns the
// credential ReadCredential reads from that Secret.
func deliverUser(t *testing.T, c *Client, dyn *fake.FakeDynamicClient, authURL string) Credential {
	t.Helper()
	user := orderRef(KindUser, "user")
	seed(t, dyn, KindUser, readyObject(user, map[string]any{"passwordGeneration": int64(1)}))
	seed(t, dyn, KindSecret, deliveredUserSecret(t, authURL, "5"))
	return readDelivered(t, c, user)
}

// deliverAppcred seeds the application credential order, ready with the
// extra status fields, and the Secret beside it at resourceVersion 7 with a
// document that logs in at authURL, and returns the credential ReadCredential
// reads from that Secret.
func deliverAppcred(t *testing.T, c *Client, dyn *fake.FakeDynamicClient, authURL string, status map[string]any) Credential {
	t.Helper()
	appcred := orderRef(KindApplicationCredential, "appcred")
	seed(t, dyn, KindApplicationCredential, readyObject(appcred, status))
	seed(t, dyn, KindSecret, deliveredAppcredSecret(t, authURL, "7"))
	return readDelivered(t, c, appcred)
}

// readDelivered reads the credential delivered beside order.
func readDelivered(t *testing.T, c *Client, order resource.Resource) Credential {
	t.Helper()
	cred, err := c.ReadCredential(context.Background(), order)
	if err != nil {
		t.Fatalf("ReadCredential(%s): %v", order.Logical, err)
	}
	return cred
}

// operationCount returns how many operations op of kind the reader recorded,
// whatever their outcome.
func operationCount(t *testing.T, reader *sdkmetric.ManualReader, kind resource.Kind, op string) int64 {
	t.Helper()
	var n int64
	for _, p := range metricPoints(t, reader, "dizzy.operation.duration") {
		if p.attrs["kind"] == string(kind) && p.attrs["operation"] == op {
			n += p.count
		}
	}
	return n
}

// operationErrors returns how many operations op of kind the reader recorded
// as failed with error kind ek.
func operationErrors(t *testing.T, reader *sdkmetric.ManualReader, kind resource.Kind, op, ek string) int64 {
	t.Helper()
	return countOf(metricPoints(t, reader, "dizzy.operation.errors"),
		map[string]string{"kind": string(kind), "operation": op, "error.kind": ek})
}

// assertNoSecret fails the test when text carries a secret value the tests
// deliver, or the start of one.
func assertNoSecret(t *testing.T, what, text string) {
	t.Helper()
	if strings.Contains(text, secretMark) {
		t.Errorf("%s %q carries a secret value", what, text)
	}
}

// TestAuthenticateUserDocument confirms a delivered user document logs in as
// delivered, at its own auth_url: the request carries the document's user
// name, user domain and password, the password method, and no scope, and the
// call is one successful authenticate sample of kind keystone_user.
func TestAuthenticateUserDocument(t *testing.T) {
	ks := newFakeKeystone(t, issued(keystoneToken("", nil)))
	c, dyn, _, reader := telemetryTestClient(t)
	cred := deliverUser(t, c, dyn, ks.authURL())

	if err := c.Authenticate(context.Background(), KindUser, cred, ""); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	reqs := ks.requests()
	if len(reqs) != 1 {
		t.Fatalf("token requests = %d, want 1", len(reqs))
	}
	body := reqs[0]
	if got := fmt.Sprint(bodyField(body, "auth", "identity", "methods")); got != "[password]" {
		t.Errorf("methods = %s, want [password]", got)
	}
	for _, f := range []struct {
		path []string
		want string
	}{
		{[]string{"name"}, resourceName(testRun, "user")},
		{[]string{"domain", "name"}, testDomainName},
		{[]string{"password"}, testPassword},
	} {
		path := append([]string{"auth", "identity", "password", "user"}, f.path...)
		if got := bodyField(body, path...); got != f.want {
			t.Errorf("%s = %v, want %q", strings.Join(path, "."), got, f.want)
		}
	}
	if scope := bodyField(body, "auth", "scope"); scope != nil {
		t.Errorf("request carries scope %v, want none", scope)
	}
	if got := countOf(metricPoints(t, reader, "dizzy.operation.duration"),
		map[string]string{"kind": "keystone_user", "operation": "authenticate", "outcome": "success"}); got != 1 {
		t.Errorf("successful keystone_user authenticates = %d, want 1", got)
	}
}

// TestAuthenticateApplicationCredential confirms a delivered application
// credential document logs in with its id and secret and no scope, and that
// the token's project is checked against wantProject: a match passes, another
// project or none wraps ErrMismatch with error kind mismatch.
func TestAuthenticateApplicationCredential(t *testing.T) {
	for _, tc := range []struct {
		name     string
		project  string
		mismatch bool
	}{
		{"project matches", testProjectName, false},
		{"another project", "dizzyorder-run0-other", true},
		{"no project", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ks := newFakeKeystone(t, issued(keystoneToken(tc.project, []string{testRole})))
			c, dyn, _, reader := telemetryTestClient(t)
			cred := deliverAppcred(t, c, dyn, ks.authURL(), map[string]any{"credentialID": testAppCredID})

			err := c.Authenticate(context.Background(), KindApplicationCredential, cred, testProjectName)
			if tc.mismatch {
				if !errors.Is(err, ErrMismatch) || errKind(err) != "mismatch" {
					t.Fatalf("Authenticate = %v (kind %q), want ErrMismatch", err, errKind(err))
				}
				if got := operationErrors(t, reader, KindApplicationCredential, "authenticate", "mismatch"); got != 1 {
					t.Errorf("authenticate mismatches = %d, want 1", got)
				}
			} else if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}

			reqs := ks.requests()
			if len(reqs) != 1 {
				t.Fatalf("token requests = %d, want 1", len(reqs))
			}
			body := reqs[0]
			if got := fmt.Sprint(bodyField(body, "auth", "identity", "methods")); got != "[application_credential]" {
				t.Errorf("methods = %s, want [application_credential]", got)
			}
			if got := bodyField(body, "auth", "identity", "application_credential", "id"); got != testAppCredID {
				t.Errorf("application_credential.id = %v, want %s", got, testAppCredID)
			}
			if got := bodyField(body, "auth", "identity", "application_credential", "secret"); got != testAppCredSecret {
				t.Errorf("application_credential.secret = %v, want the delivered secret", got)
			}
			if scope := bodyField(body, "auth", "scope"); scope != nil {
				t.Errorf("request carries scope %v, want none", scope)
			}
			if got := operationCount(t, reader, KindApplicationCredential, "authenticate"); got != 1 {
				t.Errorf("authenticate samples = %d, want 1", got)
			}
		})
	}
}

// TestAuthenticateEndpoint confirms where a login goes: after
// SetKeystoneEndpoint to the override URL, trusted through the override's TLS
// configuration, and never to the document's auth_url; without an override,
// or with an empty one, to the document's auth_url.
func TestAuthenticateEndpoint(t *testing.T) {
	t.Run("override", func(t *testing.T) {
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the delivered auth_url got %s %s, want no request", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(trap.Close)
		ks := newTLSFakeKeystone(t, issued(keystoneToken("", nil)))
		c, dyn, _ := testClient()
		cred := deliverUser(t, c, dyn, trap.URL+"/v3")

		// The delivered document carries no CA bundle, so the override's
		// certificate is trusted only through the override's TLS configuration.
		c.SetKeystoneEndpoint(ks.authURL(), nil)
		if err := c.Authenticate(context.Background(), KindUser, cred, ""); err == nil || errors.Is(err, ErrMismatch) {
			t.Fatalf("Authenticate without the override's TLS configuration = %v, want a TLS failure", err)
		}

		roots := x509.NewCertPool()
		roots.AddCert(ks.srv.Certificate())
		c.SetKeystoneEndpoint(ks.authURL(), &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
		if err := c.Authenticate(context.Background(), KindUser, cred, ""); err != nil {
			t.Fatalf("Authenticate with the override: %v", err)
		}
		if got := ks.count(); got != 1 {
			t.Errorf("token requests at the override = %d, want 1", got)
		}
	})
	t.Run("delivered", func(t *testing.T) {
		for _, override := range []bool{false, true} {
			ks := newFakeKeystone(t, issued(keystoneToken("", nil)))
			c, dyn, _ := testClient()
			cred := deliverUser(t, c, dyn, ks.authURL())
			if override {
				c.SetKeystoneEndpoint("", nil)
			}
			if err := c.Authenticate(context.Background(), KindUser, cred, ""); err != nil {
				t.Fatalf("Authenticate (empty override %v): %v", override, err)
			}
			if got := ks.count(); got != 1 {
				t.Errorf("token requests at the delivered auth_url (empty override %v) = %d, want 1", override, got)
			}
		}
	})
}

// TestVerifyRole confirms VerifyRole logs in with the user's document scoped
// to the project and its domain, passes when the token carries the role,
// wraps ErrMismatch when it does not, records one validate sample of kind
// keystone_role_assignment, and refuses an empty project without a request.
func TestVerifyRole(t *testing.T) {
	for _, tc := range []struct {
		name     string
		roles    []string
		mismatch bool
	}{
		{"holds the role", []string{"reader", testRole}, false},
		{"lacks the role", []string{"reader"}, true},
		{"no role", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ks := newFakeKeystone(t, issued(keystoneToken(testProjectName, tc.roles)))
			c, dyn, _, reader := telemetryTestClient(t)
			cred := deliverUser(t, c, dyn, ks.authURL())

			err := c.VerifyRole(context.Background(), cred, testProjectName, testDomainName, testRole)
			if tc.mismatch {
				if !errors.Is(err, ErrMismatch) || errKind(err) != "mismatch" {
					t.Fatalf("VerifyRole = %v, want ErrMismatch", err)
				}
				if !strings.Contains(err.Error(), testRole) {
					t.Errorf("error %q does not name the role", err)
				}
				if got := operationErrors(t, reader, KindRoleAssignment, "validate", "mismatch"); got != 1 {
					t.Errorf("validate mismatches = %d, want 1", got)
				}
			} else if err != nil {
				t.Fatalf("VerifyRole: %v", err)
			}

			reqs := ks.requests()
			if len(reqs) != 1 {
				t.Fatalf("token requests = %d, want 1", len(reqs))
			}
			body := reqs[0]
			if got := bodyField(body, "auth", "scope", "project", "name"); got != testProjectName {
				t.Errorf("scope.project.name = %v, want %s", got, testProjectName)
			}
			if got := bodyField(body, "auth", "scope", "project", "domain", "name"); got != testDomainName {
				t.Errorf("scope.project.domain.name = %v, want %s", got, testDomainName)
			}
			if got := bodyField(body, "auth", "identity", "password", "user", "name"); got != resourceName(testRun, "user") {
				t.Errorf("password.user.name = %v, want the delivered user", got)
			}
			if got := operationCount(t, reader, KindRoleAssignment, "validate"); got != 1 {
				t.Errorf("validate samples = %d, want 1", got)
			}
		})
	}
	t.Run("empty project", func(t *testing.T) {
		ks := newFakeKeystone(t, issued(keystoneToken(testProjectName, []string{testRole})))
		c, dyn, _ := testClient()
		cred := deliverUser(t, c, dyn, ks.authURL())
		if err := c.VerifyRole(context.Background(), cred, "", testDomainName, testRole); err == nil {
			t.Error("VerifyRole without a project = nil, want an error")
		}
		if err := c.VerifyCatalog(context.Background(), cred, testProjectName, "", testServiceType, testServiceName, testEndpointURL); err == nil {
			t.Error("VerifyCatalog without a domain = nil, want an error")
		}
		if got := ks.count(); got != 0 {
			t.Errorf("token requests = %d, want 0", got)
		}
	})
}

// TestVerifyCatalog confirms VerifyCatalog logs in scoped to the project
// until a token lists the ordered service with its public endpoint, records
// one validate sample of kind keystone_catalog_entry over the whole poll,
// returns ErrMismatch at the deadline when the entry never appears, and an
// outage at the deadline as the outage.
func TestVerifyCatalog(t *testing.T) {
	t.Run("found on a later attempt", func(t *testing.T) {
		ks := newFakeKeystone(t,
			issued(keystoneToken(testProjectName, []string{testRole})),
			issued(keystoneToken(testProjectName, []string{testRole},
				catalogService(testServiceType, testServiceName, catalogEndpoint("internal", testEndpointURL)))),
			issued(keystoneToken(testProjectName, []string{testRole},
				catalogService(testServiceType, testServiceName, catalogEndpoint("public", "https://other.invalid/v1")))),
			issued(keystoneToken(testProjectName, []string{testRole},
				catalogService(testServiceType, "another-service", catalogEndpoint("public", testEndpointURL)))),
			answered(http.StatusServiceUnavailable),
			issued(keystoneToken(testProjectName, []string{testRole}, catalogService("identity", "keystone"), orderedEntry())),
		)
		c, dyn, _, reader := telemetryTestClient(t)
		cred := deliverUser(t, c, dyn, ks.authURL())

		err := c.VerifyCatalog(shortWait(t, 5*time.Second), cred, testProjectName, testDomainName, testServiceType, testServiceName, testEndpointURL)
		if err != nil {
			t.Fatalf("VerifyCatalog: %v", err)
		}
		reqs := ks.requests()
		if len(reqs) != 6 {
			t.Errorf("token requests = %d, want 6", len(reqs))
		}
		for i, body := range reqs {
			if got := bodyField(body, "auth", "scope", "project", "name"); got != testProjectName {
				t.Errorf("request %d: scope.project.name = %v, want %s", i, got, testProjectName)
			}
		}
		if got := countOf(metricPoints(t, reader, "dizzy.operation.duration"),
			map[string]string{"kind": "keystone_catalog_entry", "operation": "validate", "outcome": "success"}); got != 1 {
			t.Errorf("successful validates = %d, want 1", got)
		}
		if got := operationCount(t, reader, KindCatalogEntry, "validate"); got != 1 {
			t.Errorf("validate samples = %d, want 1", got)
		}
	})
	t.Run("never", func(t *testing.T) {
		ks := newFakeKeystone(t, issued(keystoneToken(testProjectName, []string{testRole})))
		c, dyn, _, reader := telemetryTestClient(t)
		cred := deliverUser(t, c, dyn, ks.authURL())

		err := c.VerifyCatalog(shortWait(t, 100*time.Millisecond), cred, testProjectName, testDomainName, testServiceType, testServiceName, testEndpointURL)
		if !errors.Is(err, ErrMismatch) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("VerifyCatalog = %v, want ErrMismatch at the deadline", err)
		}
		if errKind(err) != "mismatch" || IsRetryable(err) {
			t.Errorf("errKind = %q, retryable %v, want mismatch and not retryable", errKind(err), IsRetryable(err))
		}
		if got := ks.count(); got < 2 {
			t.Errorf("token requests = %d, want the poll to log in more than once", got)
		}
		if got := operationErrors(t, reader, KindCatalogEntry, "validate", "mismatch"); got != 1 {
			t.Errorf("validate mismatches = %d, want 1", got)
		}
		if got := operationCount(t, reader, KindCatalogEntry, "validate"); got != 1 {
			t.Errorf("validate samples = %d, want 1", got)
		}
	})
	t.Run("outage at the deadline", func(t *testing.T) {
		ks := newFakeKeystone(t, issued(keystoneToken(testProjectName, []string{testRole})), answered(http.StatusServiceUnavailable))
		c, dyn, _, reader := telemetryTestClient(t)
		cred := deliverUser(t, c, dyn, ks.authURL())

		err := c.VerifyCatalog(shortWait(t, 100*time.Millisecond), cred, testProjectName, testDomainName, testServiceType, testServiceName, testEndpointURL)
		if err == nil || errors.Is(err, ErrMismatch) {
			t.Fatalf("VerifyCatalog = %v, want the outage, not a mismatch", err)
		}
		if errKind(err) != "http_503" {
			t.Errorf("errKind = %q, want http_503", errKind(err))
		}
		if got := operationErrors(t, reader, KindCatalogEntry, "validate", "http_503"); got != 1 {
			t.Errorf("validate http_503 errors = %d, want 1", got)
		}
	})
}

// TestLogin401Classification confirms how a 401 to a login with a delivered
// credential is classified after the order and its Secret are read again: a
// mismatch when the order vouched for the credential before and after the
// login, the 401 with error kind http_401 when the login raced a change or a
// read failed.
func TestLogin401Classification(t *testing.T) {
	user := orderRef(KindUser, "user")
	appcred := orderRef(KindApplicationCredential, "appcred")
	readyUser := readyObject(user, map[string]any{"passwordGeneration": int64(1)})
	pendingUser := pendingObject(user, "WaitingForServiceAccounts")
	named := func(current, previous string) *unstructured.Unstructured {
		return readyObject(appcred, map[string]any{"credentialID": current, "previousCredentialID": previous})
	}
	for _, tc := range []struct {
		name     string
		order    resource.Resource
		status   map[string]any
		setup    func(t *testing.T, dyn *fake.FakeDynamicClient, authURL string)
		mismatch bool
	}{
		{name: "user ready and its secret unchanged", order: user, mismatch: true},
		{name: "user not ready after the login", order: user, setup: func(t *testing.T, dyn *fake.FakeDynamicClient, _ string) {
			script(t, dyn, "keystoneusers", readyUser, pendingUser)
		}},
		{name: "user not ready before the login", order: user, setup: func(t *testing.T, dyn *fake.FakeDynamicClient, _ string) {
			script(t, dyn, "keystoneusers", pendingUser, readyUser)
		}},
		{name: "user secret rewritten during the login", order: user, setup: func(t *testing.T, dyn *fake.FakeDynamicClient, authURL string) {
			script(t, dyn, "secrets", deliveredUserSecret(t, authURL, "5"), deliveredUserSecret(t, authURL, "6"))
		}},
		{name: "user order unreadable", order: user, setup: func(t *testing.T, dyn *fake.FakeDynamicClient, _ string) {
			failVerb(dyn, "get", "keystoneusers", internalError())
		}},
		{name: "user secret unreadable", order: user, setup: func(t *testing.T, dyn *fake.FakeDynamicClient, _ string) {
			failVerb(dyn, "get", "secrets", internalError())
		}},
		{name: "credential named by credentialID", order: appcred,
			status: map[string]any{"credentialID": testAppCredID}, mismatch: true},
		{name: "credential named by previousCredentialID", order: appcred,
			status: map[string]any{"credentialID": "ac-2", "previousCredentialID": testAppCredID}, mismatch: true},
		{name: "credential named by neither", order: appcred,
			status: map[string]any{"credentialID": "ac-2", "previousCredentialID": "ac-0"}},
		{name: "credential no longer named after the login", order: appcred,
			status: map[string]any{"credentialID": "ac-2", "previousCredentialID": testAppCredID},
			setup: func(t *testing.T, dyn *fake.FakeDynamicClient, _ string) {
				script(t, dyn, "keystoneapplicationcredentials", named("ac-2", testAppCredID), named("ac-2", ""))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ks := newFakeKeystone(t, answered(http.StatusUnauthorized))
			c, dyn, _, reader := telemetryTestClient(t)
			var cred Credential
			wantProject := ""
			if tc.order.Kind == KindUser {
				cred = deliverUser(t, c, dyn, ks.authURL())
			} else {
				cred = deliverAppcred(t, c, dyn, ks.authURL(), tc.status)
				wantProject = testProjectName
			}
			if tc.setup != nil {
				tc.setup(t, dyn, ks.authURL())
			}
			k, _ := lookupKind(tc.order.Kind)
			readsBefore := countActions(dyn, "get", k.resource)

			err := c.Authenticate(context.Background(), tc.order.Kind, cred, wantProject)
			if err == nil {
				t.Fatal("Authenticate = nil, want the 401 classified")
			}
			if IsRetryable(err) {
				t.Errorf("error %v is retryable, want not", err)
			}
			if tc.mismatch {
				if !errors.Is(err, ErrMismatch) || errKind(err) != "mismatch" {
					t.Errorf("Authenticate = %v (kind %q), want ErrMismatch with error kind mismatch", err, errKind(err))
				}
				if got := operationErrors(t, reader, tc.order.Kind, "authenticate", "mismatch"); got != 1 {
					t.Errorf("authenticate mismatches = %d, want 1", got)
				}
			} else {
				if errors.Is(err, ErrMismatch) || errKind(err) != "http_401" {
					t.Errorf("Authenticate = %v (kind %q), want the 401 with error kind http_401", err, errKind(err))
				}
				if got := operationErrors(t, reader, tc.order.Kind, "authenticate", "http_401"); got != 1 {
					t.Errorf("authenticate http_401 errors = %d, want 1", got)
				}
			}
			if got := countActions(dyn, "get", k.resource) - readsBefore; got != 2 {
				t.Errorf("order reads around the login = %d, want 2", got)
			}
			if got := ks.count(); got != 1 {
				t.Errorf("token requests = %d, want 1", got)
			}
			assertNoSecret(t, "error", err.Error())
		})
	}
}

// TestVerifyChecksClassify401 confirms VerifyRole and VerifyCatalog classify a
// 401 the way Authenticate does, and that VerifyCatalog ends its poll at the
// first 401.
func TestVerifyChecksClassify401(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind resource.Kind
		call func(context.Context, *Client, Credential) error
	}{
		{"VerifyRole", KindRoleAssignment, func(ctx context.Context, c *Client, cred Credential) error {
			return c.VerifyRole(ctx, cred, testProjectName, testDomainName, testRole)
		}},
		{"VerifyCatalog", KindCatalogEntry, func(ctx context.Context, c *Client, cred Credential) error {
			return c.VerifyCatalog(ctx, cred, testProjectName, testDomainName, testServiceType, testServiceName, testEndpointURL)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("ready and unchanged", func(t *testing.T) {
				ks := newFakeKeystone(t, answered(http.StatusUnauthorized))
				c, dyn, _, reader := telemetryTestClient(t)
				cred := deliverUser(t, c, dyn, ks.authURL())

				err := tc.call(shortWait(t, 5*time.Second), c, cred)
				if !errors.Is(err, ErrMismatch) || errKind(err) != "mismatch" {
					t.Fatalf("%s = %v, want ErrMismatch", tc.name, err)
				}
				if got := ks.count(); got != 1 {
					t.Errorf("token requests = %d, want 1", got)
				}
				if got := operationErrors(t, reader, tc.kind, "validate", "mismatch"); got != 1 {
					t.Errorf("validate mismatches = %d, want 1", got)
				}
			})
			t.Run("raced a change", func(t *testing.T) {
				ks := newFakeKeystone(t, answered(http.StatusUnauthorized))
				c, dyn, _ := testClient()
				cred := deliverUser(t, c, dyn, ks.authURL())
				user := orderRef(KindUser, "user")
				script(t, dyn, "keystoneusers", readyObject(user, nil), pendingObject(user, "WaitingForServiceAccounts"))

				err := tc.call(shortWait(t, 5*time.Second), c, cred)
				if errors.Is(err, ErrMismatch) || errKind(err) != "http_401" {
					t.Fatalf("%s = %v, want the 401 with error kind http_401", tc.name, err)
				}
				if got := ks.count(); got != 1 {
					t.Errorf("token requests = %d, want 1", got)
				}
			})
		})
	}
}

// echoing answers a login with a 503 whose body quotes secret, as a broken
// proxy might.
func echoing(secret string) tokenAnswer {
	return tokenAnswer{status: http.StatusServiceUnavailable,
		body: `{"error": {"code": 503, "message": "upstream saw ` + secret + `"}}`}
}

// TestLogin503IsNotMismatch confirms a 503 to a login is returned as the
// outage it is: error kind http_503, retryable, never a mismatch, with no
// read of the order after it, and with the login's password, which Keystone's
// answer echoes, cut from the error text.
func TestLogin503IsNotMismatch(t *testing.T) {
	ks := newFakeKeystone(t, echoing(testPassword))
	c, dyn, _, reader := telemetryTestClient(t)
	cred := deliverUser(t, c, dyn, ks.authURL())

	err := c.Authenticate(context.Background(), KindUser, cred, "")
	if err == nil || errors.Is(err, ErrMismatch) {
		t.Fatalf("Authenticate = %v, want the 503, not a mismatch", err)
	}
	if errKind(err) != "http_503" || !IsRetryable(err) {
		t.Errorf("errKind = %q, retryable %v, want http_503 and retryable", errKind(err), IsRetryable(err))
	}
	if got := countActions(dyn, "get", "keystoneusers"); got != 1 {
		t.Errorf("order reads = %d, want only the one before the login", got)
	}
	if got := operationErrors(t, reader, KindUser, "authenticate", "http_503"); got != 1 {
		t.Errorf("authenticate http_503 errors = %d, want 1", got)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("error %q does not show the echoed secrets as cut", err)
	}
	assertNoSecret(t, "error", err.Error())
}

// TestReject confirms Reject sends exactly one login and takes a 401, and a
// 404 for an application credential, as the rejection it must see, a token as
// ErrAccepted, and anything else as it is, each with one reject sample.
func TestReject(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   resource.Kind
		answer tokenAnswer
		want   string // the error kind; empty for nil
	}{
		{"superseded password rejected", KindUser, answered(http.StatusUnauthorized), ""},
		{"deleted credential not found", KindApplicationCredential, answered(http.StatusNotFound), ""},
		{"deleted credential rejected", KindApplicationCredential, answered(http.StatusUnauthorized), ""},
		{"superseded password accepted", KindUser, issued(keystoneToken("", nil)), "accepted"},
		{"deleted credential accepted", KindApplicationCredential, issued(keystoneToken(testProjectName, []string{testRole})), "accepted"},
		{"outage", KindUser, answered(http.StatusServiceUnavailable), "http_503"},
		{"password login not found", KindUser, answered(http.StatusNotFound), "http_404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ks := newFakeKeystone(t, tc.answer)
			c, dyn, _, reader := telemetryTestClient(t)
			var cred Credential
			if tc.kind == KindUser {
				cred = deliverUser(t, c, dyn, ks.authURL())
			} else {
				cred = deliverAppcred(t, c, dyn, ks.authURL(), map[string]any{"credentialID": "ac-2"})
			}
			kubeRequests := len(dyn.Actions())

			err := c.Reject(context.Background(), tc.kind, cred)
			if got := errKind(err); got != tc.want {
				t.Fatalf("Reject = %v (kind %q), want error kind %q", err, got, tc.want)
			}
			if tc.want == "accepted" && !errors.Is(err, ErrAccepted) {
				t.Errorf("Reject = %v, want ErrAccepted", err)
			}
			if got := ks.count(); got != 1 {
				t.Errorf("token requests = %d, want exactly 1", got)
			}
			if got := len(dyn.Actions()) - kubeRequests; got != 0 {
				t.Errorf("Kubernetes requests = %d, want 0", got)
			}
			if got := operationCount(t, reader, tc.kind, "reject"); got != 1 {
				t.Errorf("reject samples = %d, want 1", got)
			}
			if tc.want != "" {
				if got := operationErrors(t, reader, tc.kind, "reject", tc.want); got != 1 {
					t.Errorf("reject %s errors = %d, want 1", tc.want, got)
				}
				assertNoSecret(t, "error", err.Error())
			}
		})
	}
}

// TestPasswordLoginGuard confirms that after five 401s to password logins,
// sent concurrently, the client sends no further password login from any
// check and says why, while application credential logins go on, and that
// 401s to application credential logins do not count.
func TestPasswordLoginGuard(t *testing.T) {
	t.Run("password logins", func(t *testing.T) {
		ks := newFakeKeystone(t, answered(http.StatusUnauthorized))
		c, dyn, m := testClient()
		user := deliverUser(t, c, dyn, ks.authURL())
		appcred := deliverAppcred(t, c, dyn, ks.authURL(), map[string]any{"credentialID": testAppCredID})

		var wg sync.WaitGroup
		errs := make([]error, maxPasswordRejections)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = c.Reject(context.Background(), KindUser, user)
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("Reject %d = %v, want nil", i, err)
			}
		}
		if got := ks.count(); got != maxPasswordRejections {
			t.Fatalf("token requests = %d, want %d", got, maxPasswordRejections)
		}

		ctx := shortWait(t, 5*time.Second)
		for name, call := range map[string]func() error{
			"Authenticate": func() error { return c.Authenticate(ctx, KindUser, user, "") },
			"VerifyRole":   func() error { return c.VerifyRole(ctx, user, testProjectName, testDomainName, testRole) },
			"VerifyCatalog": func() error {
				return c.VerifyCatalog(ctx, user, testProjectName, testDomainName, testServiceType, testServiceName, testEndpointURL)
			},
			"Reject": func() error { return c.Reject(ctx, KindUser, user) },
		} {
			err := call()
			if !errors.Is(err, errPasswordLoginLimit) {
				t.Errorf("%s after five 401s = %v, want the login refused", name, err)
				continue
			}
			if errKind(err) != "other" || IsRetryable(err) {
				t.Errorf("%s: errKind = %q, retryable %v, want other and not retryable", name, errKind(err), IsRetryable(err))
			}
			if !strings.Contains(err.Error(), "lock") || !strings.Contains(err.Error(), user.Order.ID) {
				t.Errorf("%s error %q does not name the lockout risk and the order", name, err)
			}
		}
		if got := ks.count(); got != maxPasswordRejections {
			t.Errorf("token requests = %d, want still %d: no further password login is sent", got, maxPasswordRejections)
		}
		if got := errorCount(m.Aggregate(time.Second), "other"); got != 4 {
			t.Errorf("samples of error kind other = %d, want 4", got)
		}

		if err := c.Reject(ctx, KindApplicationCredential, appcred); err != nil {
			t.Errorf("Reject of the application credential = %v, want nil", err)
		}
		if got := ks.count(); got != maxPasswordRejections+1 {
			t.Errorf("token requests = %d, want %d: an application credential login is still sent", got, maxPasswordRejections+1)
		}
	})
	t.Run("application credential logins do not count", func(t *testing.T) {
		ks := newFakeKeystone(t, answered(http.StatusUnauthorized))
		c, dyn, _ := testClient()
		user := deliverUser(t, c, dyn, ks.authURL())
		appcred := deliverAppcred(t, c, dyn, ks.authURL(), map[string]any{"credentialID": "ac-2"})

		for i := 0; i <= maxPasswordRejections; i++ {
			if err := c.Reject(context.Background(), KindApplicationCredential, appcred); err != nil {
				t.Fatalf("Reject %d of the application credential = %v, want nil", i, err)
			}
		}
		if err := c.Reject(context.Background(), KindUser, user); err != nil {
			t.Errorf("Reject of the password = %v, want nil: the password login is still sent", err)
		}
		if got := ks.count(); got != maxPasswordRejections+2 {
			t.Errorf("token requests = %d, want %d", got, maxPasswordRejections+2)
		}
	})
}

// TestKeystoneChecksKeepSecretsOut drives every failure of the Keystone
// checks and confirms that no error text and no log line carries the
// password or the application credential secret, not even the start of one
// that a YAML error would quote.
func TestKeystoneChecksKeepSecretsOut(t *testing.T) {
	logs := captureLog(t)
	ctx := shortWait(t, 5*time.Second)
	var errs []error
	collect := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s = nil, want an error", what)
			return
		}
		errs = append(errs, err)
	}

	unauthorized := newFakeKeystone(t, answered(http.StatusUnauthorized))
	c, dyn, _ := testClient()
	user := deliverUser(t, c, dyn, unauthorized.authURL())
	appcred := deliverAppcred(t, c, dyn, unauthorized.authURL(), map[string]any{"credentialID": testAppCredID})
	collect("password mismatch", c.Authenticate(ctx, KindUser, user, ""))
	collect("credential mismatch", c.Authenticate(ctx, KindApplicationCredential, appcred, testProjectName))
	collect("role mismatch on a 401", c.VerifyRole(ctx, user, testProjectName, testDomainName, testRole))
	for range maxPasswordRejections {
		_ = c.Reject(ctx, KindUser, user)
	}
	collect("refused password login", c.Authenticate(ctx, KindUser, user, ""))

	raced, dyn, _ := testClient()
	user = deliverUser(t, raced, dyn, unauthorized.authURL())
	script(t, dyn, "keystoneusers", readyObject(user.Order, nil), pendingObject(user.Order, "WaitingForServiceAccounts"))
	collect("raced 401", raced.Authenticate(ctx, KindUser, user, ""))

	echoPassword := newFakeKeystone(t, echoing(testPassword))
	echoSecret := newFakeKeystone(t, echoing(testAppCredSecret))
	c, dyn, _ = testClient()
	user = deliverUser(t, c, dyn, echoPassword.authURL())
	appcred = deliverAppcred(t, c, dyn, echoSecret.authURL(), map[string]any{"credentialID": testAppCredID})
	collect("503 echoing the password", c.Authenticate(ctx, KindUser, user, ""))
	collect("503 echoing the credential secret", c.Authenticate(ctx, KindApplicationCredential, appcred, testProjectName))
	collect("rejection answered with a 503 echoing the password", c.Reject(ctx, KindUser, user))

	granting := newFakeKeystone(t, issued(keystoneToken("dizzyorder-run0-other", []string{"reader"})))
	c, dyn, _ = testClient()
	user = deliverUser(t, c, dyn, granting.authURL())
	appcred = deliverAppcred(t, c, dyn, granting.authURL(), map[string]any{"credentialID": testAppCredID})
	collect("project mismatch", c.Authenticate(ctx, KindApplicationCredential, appcred, testProjectName))
	collect("missing role", c.VerifyRole(ctx, user, testProjectName, testDomainName, testRole))
	collect("missing catalog entry", c.VerifyCatalog(shortWait(t, 50*time.Millisecond), user,
		testProjectName, testDomainName, testServiceType, testServiceName, testEndpointURL))
	collect("accepted password", c.Reject(ctx, KindUser, user))
	collect("accepted credential", c.Reject(ctx, KindApplicationCredential, appcred))

	// A document gophercloud cannot decode: the YAML error would quote the
	// start of the scalar it fails on, which is the password here.
	undecodable := "clouds:\n  admin: \"" + testPassword + "\"\n"
	var probe clouds.Clouds
	if err := yaml.Unmarshal([]byte(undecodable), &probe); err == nil || !strings.Contains(err.Error(), secretMark) {
		t.Fatalf("decoding the undecodable document = %v, want a YAML error quoting the secret's start", err)
	}
	bad := Credential{CloudsYAML: []byte(undecodable), Cloud: "admin", Password: testPassword, Order: orderRef(KindUser, "user")}
	kubeRequests := len(dyn.Actions())
	err := c.Authenticate(ctx, KindUser, bad, "")
	if !errors.Is(err, ErrMismatch) {
		t.Errorf("Authenticate with an undecodable document = %v, want ErrMismatch", err)
	}
	collect("undecodable document", err)
	if got := len(dyn.Actions()) - kubeRequests; got != 0 {
		t.Errorf("Kubernetes requests for an undecodable document = %d, want 0: it is refused before any read", got)
	}

	for _, err := range errs {
		assertNoSecret(t, "error", err.Error())
		assertNoSecret(t, "formatted error", fmt.Sprintf("%+v", err))
	}
	assertNoSecret(t, "log", logs.String())
}
