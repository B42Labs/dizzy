package keystone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// ownerTok is the token the fake Keystone of the delegation tests issues to a
// created user, so a test can tell it from the admin token in the headers.
// appcredSecret is the secret the tests hand an application credential.
const (
	ownerTok      = "owner-tok"
	appcredSecret = "ac-secret-Zq9w"
)

// testAppCred is the plan's application credential the tests create: the
// credential appcred-0001 of user-0001 with role-0001 on proj-0001.
var testAppCred = keystoneplan.ApplicationCredential{Name: "appcred-0001", User: "user-0001", Project: "proj-0001", Role: "role-0001"}

// keystoneReply is how the fake Keystone of the delegation tests answers one
// route: the status, the body and, for a token create, the X-Subject-Token
// header.
type keystoneReply struct {
	status       int
	subjectToken string
	body         string
}

// issuedReply answers a token create with tok, as Keystone does: 201, the
// token in X-Subject-Token and its expiry in the body.
func issuedReply(tok string) keystoneReply {
	return keystoneReply{status: http.StatusCreated, subjectToken: tok, body: `{"token":{"expires_at":"2030-01-01T00:00:00Z"}}`}
}

// errorReply answers with status and a Keystone error body carrying message.
func errorReply(status int, message string) keystoneReply {
	return keystoneReply{status: status, body: fmt.Sprintf(`{"error":{"code":%d,"title":%q,"message":%q}}`, status, http.StatusText(status), message)}
}

// seenRequest is one request the fake Keystone of the delegation tests
// received: its "<METHOD> <path>" route, its X-Auth-Token header and its
// decoded JSON body (nil for none).
type seenRequest struct {
	route     string
	authToken string
	body      map[string]any
}

// delegationKeystone is a fake Keystone that answers each request with the
// reply routes lists for its "<METHOD> <path>", and fails the test and
// answers 500 for a route it does not list. The returned function lists the
// requests it received, in order.
func delegationKeystone(t *testing.T, routes map[string]keystoneReply) (*httptest.Server, func() []seenRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []seenRequest
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := seenRequest{route: r.Method + " " + r.URL.Path, authToken: r.Header.Get("X-Auth-Token")}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			if err := json.Unmarshal(raw, &req.body); err != nil {
				t.Errorf("%s: body %q is not a JSON object: %v", req.route, raw, err)
			}
		}
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		reply, ok := routes[req.route]
		if !ok {
			t.Errorf("unexpected request %s", req.route)
			reply = keystoneReply{status: http.StatusInternalServerError}
		}
		if reply.subjectToken != "" {
			w.Header().Set("X-Subject-Token", reply.subjectToken)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	t.Cleanup(ts.Close)
	return ts, func() []seenRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// routesOf returns the "<METHOD> <path>" of each request, in order.
func routesOf(reqs []seenRequest) []string {
	routes := make([]string, len(reqs))
	for i, r := range reqs {
		routes[i] = r.route
	}
	return routes
}

// jsonAt returns the value at the path of object keys in a decoded JSON body,
// or nil when a step is missing or not an object.
func jsonAt(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// readinessOf returns the aggregate's time-to-ready stats for one kind, or
// zero stats when no record of that kind was made.
func readinessOf(agg metrics.Aggregate, typ string) metrics.ReadinessStats {
	for _, r := range agg.Readiness {
		if r.Type == typ {
			return r
		}
	}
	return metrics.ReadinessStats{}
}

// checkOwnerTokenRequest confirms req is a password token create as the
// created user's prefixed name, with password in domain domainID, scoped to
// project projectID: the owner token a delegation call takes.
func checkOwnerTokenRequest(t *testing.T, req seenRequest, name, domainID, password, projectID string) {
	t.Helper()
	if req.route != "POST /auth/tokens" {
		t.Errorf("owner token request = %s, want POST /auth/tokens", req.route)
	}
	if got := jsonAt(req.body, "auth", "identity", "methods"); !reflect.DeepEqual(got, []any{"password"}) {
		t.Errorf("owner token methods = %v, want [password]", got)
	}
	user := jsonAt(req.body, "auth", "identity", "password", "user")
	if got := jsonAt(user, "name"); got != name {
		t.Errorf("owner token user name = %v, want %q", got, name)
	}
	if got := jsonAt(user, "domain", "id"); got != domainID {
		t.Errorf("owner token user domain id = %v, want %q", got, domainID)
	}
	if got := jsonAt(user, "password"); got != password {
		t.Errorf("owner token password = %v, want %q", got, password)
	}
	if got := jsonAt(req.body, "auth", "scope", "project", "id"); got != projectID {
		t.Errorf("owner token scope project id = %v, want %q", got, projectID)
	}
}

// TestCreateApplicationCredential confirms a credential is created as its
// owner: one token request as dizzy-run0-user-0001 with its password, in its
// domain and scoped to the project, then one POST to the owner's
// application_credentials carrying that token, the cloud name
// dizzy-run0-appcred-0001, the secret and exactly one role. The returned
// resource carries the synthetic id <userID>:<credentialID>, and the admin
// client keeps its own token.
func TestCreateApplicationCredential(t *testing.T) {
	ts, seen := delegationKeystone(t, map[string]keystoneReply{
		"POST /auth/tokens": issuedReply(ownerTok),
		"POST /users/uid1/application_credentials": {
			status: http.StatusCreated,
			body:   `{"application_credential":{"id":"acid1","name":"dizzy-run0-appcred-0001","secret":"` + appcredSecret + `"}}`,
		},
	})
	c, m, reader := telemetryTestClient(t, ts)

	got, err := c.CreateApplicationCredential(context.Background(), testAppCred, "uid1", "did1", "user-pw", "pid1", "rid1", appcredSecret)
	if err != nil {
		t.Fatalf("CreateApplicationCredential: %v", err)
	}
	want := resource.Resource{Kind: KindApplicationCredential, Logical: "appcred-0001", Name: "dizzy-run0-appcred-0001", ID: "uid1:acid1"}
	if got != want {
		t.Errorf("CreateApplicationCredential = %+v, want %+v", got, want)
	}

	reqs := seen()
	wantRoutes := []string{"POST /auth/tokens", "POST /users/uid1/application_credentials"}
	if !slices.Equal(routesOf(reqs), wantRoutes) {
		t.Fatalf("requests = %v, want %v", routesOf(reqs), wantRoutes)
	}
	checkOwnerTokenRequest(t, reqs[0], "dizzy-run0-user-0001", "did1", "user-pw", "pid1")
	create := reqs[1]
	if create.authToken != ownerTok {
		t.Errorf("create X-Auth-Token = %q, want the owner token %q", create.authToken, ownerTok)
	}
	ac := jsonAt(create.body, "application_credential")
	if name := jsonAt(ac, "name"); name != "dizzy-run0-appcred-0001" {
		t.Errorf("create body name = %v, want dizzy-run0-appcred-0001", name)
	}
	if secret := jsonAt(ac, "secret"); secret != appcredSecret {
		t.Errorf("create body secret = %v, want %q", secret, appcredSecret)
	}
	if roles := jsonAt(ac, "roles"); !reflect.DeepEqual(roles, []any{map[string]any{"id": "rid1"}}) {
		t.Errorf("create body roles = %v, want exactly [{id: rid1}]", roles)
	}

	agg := m.Aggregate(time.Second)
	if st := statsOf(agg, string(KindToken)); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("token samples = %+v, want 1 attempted and 1 succeeded (the owner token)", st)
	}
	if st := statsOf(agg, string(KindApplicationCredential)); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("application_credential samples = %+v, want 1 attempted and 1 succeeded", st)
	}
	wantOp := map[string]string{"kind": "application_credential", "operation": "create", "outcome": "success"}
	if !hasOperationPoint(t, reader, wantOp) {
		t.Errorf("no dizzy.operation.duration point with %v", wantOp)
	}
	if tok := c.gc.Token(); tok != adminToken {
		t.Errorf("admin token after the create = %q, want %q unchanged", tok, adminToken)
	}
}

// TestCreateApplicationCredentialOwnerTokenRejected confirms a 401 on the
// owner token fails the create with an error that names the credential and
// wraps the 401, sends no request to the credentials endpoint, and counts a
// failed token sample.
func TestCreateApplicationCredentialOwnerTokenRejected(t *testing.T) {
	ts, seen := delegationKeystone(t, map[string]keystoneReply{
		"POST /auth/tokens": errorReply(http.StatusUnauthorized, "The request you have made requires authentication."),
	})
	c, m := testClient(ts)

	_, err := c.CreateApplicationCredential(context.Background(), testAppCred, "uid1", "did1", "user-pw", "pid1", "rid1", appcredSecret)
	const wantPrefix = `creating application_credential "appcred-0001": issuing token for user "user-0001" on project pid1: `
	if err == nil || !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Fatalf("CreateApplicationCredential error = %v, want it to start with %q", err, wantPrefix)
	}
	if !gophercloud.ResponseCodeIs(err, http.StatusUnauthorized) {
		t.Errorf("ResponseCodeIs(%v, 401) = false, want the 401 wrapped", err)
	}
	if errors.Is(err, ErrForbidden) {
		t.Errorf("errors.Is(%v, ErrForbidden) = true, want false for a 401", err)
	}
	if routes := routesOf(seen()); !slices.Equal(routes, []string{"POST /auth/tokens"}) {
		t.Errorf("requests = %v, want only the owner token request", routes)
	}

	agg := m.Aggregate(time.Second)
	if st := statsOf(agg, string(KindToken)); st.Attempted != 1 || st.Failed != 1 {
		t.Errorf("token samples = %+v, want 1 attempted and 1 failed", st)
	}
	if got := errorCount(agg, "http_401"); got != 1 {
		t.Errorf("http_401 samples = %d, want 1", got)
	}
	if st := statsOf(agg, string(KindApplicationCredential)); st.Attempted != 0 {
		t.Errorf("application_credential samples = %+v, want none", st)
	}
}

// TestCreateApplicationCredentialForbidden confirms a 403 on the credential
// create returns an error that names the credential and carries ErrForbidden,
// so the executor fails fast, and counts a failed application_credential
// sample.
func TestCreateApplicationCredentialForbidden(t *testing.T) {
	ts, seen := delegationKeystone(t, map[string]keystoneReply{
		"POST /auth/tokens":                        issuedReply(ownerTok),
		"POST /users/uid1/application_credentials": errorReply(http.StatusForbidden, "You are not authorized to perform the requested action."),
	})
	c, m := testClient(ts)

	_, err := c.CreateApplicationCredential(context.Background(), testAppCred, "uid1", "did1", "user-pw", "pid1", "rid1", appcredSecret)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("errors.Is(%v, ErrForbidden) = false, want true", err)
	}
	if want := `creating application_credential "appcred-0001": `; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error %q does not start with %q", err, want)
	}
	if routes := routesOf(seen()); len(routes) != 2 {
		t.Errorf("requests = %v, want the owner token and the create", routes)
	}
	if st := statsOf(m.Aggregate(time.Second), string(KindApplicationCredential)); st.Attempted != 1 || st.Failed != 1 {
		t.Errorf("application_credential samples = %+v, want 1 attempted and 1 failed", st)
	}
}

// TestAuthenticateApplicationCredential confirms the read-back is one token
// request whose only identity method is application_credential, carrying the
// credential id parsed from the resource id and the secret, and no scope, and
// that it records one application_credential/authenticate sample and one
// time-to-ready record of kind application_credential, and no token sample.
func TestAuthenticateApplicationCredential(t *testing.T) {
	ts, seen := delegationKeystone(t, map[string]keystoneReply{
		"POST /auth/tokens": issuedReply("ac-tok"),
	})
	c, m, reader := telemetryTestClient(t, ts)

	res := resource.Resource{Kind: KindApplicationCredential, Logical: "appcred-0001", Name: "dizzy-run0-appcred-0001", ID: "uid1:acid1"}
	if err := c.AuthenticateApplicationCredential(context.Background(), testAppCred, res, appcredSecret); err != nil {
		t.Fatalf("AuthenticateApplicationCredential: %v", err)
	}

	reqs := seen()
	if routes := routesOf(reqs); !slices.Equal(routes, []string{"POST /auth/tokens"}) {
		t.Fatalf("requests = %v, want one token request", routes)
	}
	body := reqs[0].body
	if got := jsonAt(body, "auth", "identity", "methods"); !reflect.DeepEqual(got, []any{"application_credential"}) {
		t.Errorf("identity methods = %v, want [application_credential]", got)
	}
	cred := jsonAt(body, "auth", "identity", "application_credential")
	if got := jsonAt(cred, "id"); got != "acid1" {
		t.Errorf("application_credential id = %v, want acid1", got)
	}
	if got := jsonAt(cred, "secret"); got != appcredSecret {
		t.Errorf("application_credential secret = %v, want %q", got, appcredSecret)
	}
	auth, _ := jsonAt(body, "auth").(map[string]any)
	if scope, ok := auth["scope"]; ok {
		t.Errorf("auth carries scope %v, want none", scope)
	}

	agg := m.Aggregate(time.Second)
	if st := statsOf(agg, string(KindApplicationCredential)); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("application_credential samples = %+v, want 1 attempted and 1 succeeded", st)
	}
	if r := readinessOf(agg, string(KindApplicationCredential)); r.Count != 1 || r.OK != 1 {
		t.Errorf("application_credential time-to-ready = %+v, want 1 record, 1 ok", r)
	}
	if st := statsOf(agg, string(KindToken)); st.Attempted != 0 {
		t.Errorf("token samples = %+v, want none", st)
	}
	wantOp := map[string]string{"kind": "application_credential", "operation": "authenticate", "outcome": "success"}
	if !hasOperationPoint(t, reader, wantOp) {
		t.Errorf("no dizzy.operation.duration point with %v", wantOp)
	}
}

// TestAuthenticateApplicationCredentialRedactsSecret confirms a 401 whose
// body quotes the secret fails the read-back after one request, with an error
// that names the credential, keeps the 401 reachable, and carries <token> in
// place of the secret, and that the failure is recorded as a failed
// application_credential sample and a failed time-to-ready record.
func TestAuthenticateApplicationCredentialRedactsSecret(t *testing.T) {
	ts, seen := delegationKeystone(t, map[string]keystoneReply{
		"POST /auth/tokens": errorReply(http.StatusUnauthorized, "Invalid application credential secret "+appcredSecret+"."),
	})
	c, m := testClient(ts)
	var reauths int
	c.gc.ReauthFunc = func(context.Context) error {
		reauths++
		return nil
	}

	res := resource.Resource{Kind: KindApplicationCredential, Logical: "appcred-0001", ID: "uid1:acid1"}
	err := c.AuthenticateApplicationCredential(context.Background(), testAppCred, res, appcredSecret)
	const wantPrefix = `authenticating application credential "appcred-0001"`
	if err == nil || !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Fatalf("AuthenticateApplicationCredential error = %v, want it to start with %q", err, wantPrefix)
	}
	if !strings.Contains(err.Error(), "<token>") {
		t.Errorf("error %q lacks the <token> placeholder", err)
	}
	if strings.Contains(err.Error(), appcredSecret) {
		t.Errorf("error %q carries the secret", err)
	}
	if !gophercloud.ResponseCodeIs(err, http.StatusUnauthorized) {
		t.Errorf("ResponseCodeIs(%v, 401) = false, want the 401 reachable", err)
	}
	if n := len(seen()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
	if reauths != 0 {
		t.Errorf("admin re-authentications = %d, want 0", reauths)
	}

	agg := m.Aggregate(time.Second)
	if st := statsOf(agg, string(KindApplicationCredential)); st.Attempted != 1 || st.Failed != 1 {
		t.Errorf("application_credential samples = %+v, want 1 attempted and 1 failed", st)
	}
	if r := readinessOf(agg, string(KindApplicationCredential)); r.Count != 1 || r.OK != 0 {
		t.Errorf("application_credential time-to-ready = %+v, want 1 record, 0 ok", r)
	}
}

// TestApplicationCredentialMalformedID confirms a resource id without the
// "<userID>:<credentialID>" form fails the read-back and the delete before
// any request and without a sample, with an error that names the credential
// and the malformed id.
func TestApplicationCredentialMalformedID(t *testing.T) {
	ts, seen := delegationKeystone(t, nil)
	c, m := testClient(ts)
	res := resource.Resource{Kind: KindApplicationCredential, Logical: "appcred-0001", ID: "acid1"}
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
		want string
	}{
		{
			name: "authenticate",
			call: func() error { return c.AuthenticateApplicationCredential(ctx, testAppCred, res, appcredSecret) },
			want: `authenticating application credential "appcred-0001": malformed application credential id "acid1"`,
		},
		{
			name: "delete",
			call: func() error { return c.DeleteApplicationCredential(ctx, testAppCred, res, "did1", "user-pw", "pid1") },
			want: `deleting application credential "appcred-0001": malformed application credential id "acid1"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil || err.Error() != tc.want {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
	if n := len(seen()); n != 0 {
		t.Errorf("requests = %d, want 0: a malformed id must fail before any request", n)
	}
	if agg := m.Aggregate(time.Second); agg.Overall.Attempted != 0 {
		t.Errorf("samples = %+v, want none", agg.Overall)
	}
}

// TestDeleteApplicationCredential confirms a delete takes an owner token as
// the credential's user and then sends DELETE
// /users/<user>/application_credentials/<id> with that token, and that a 404
// on the delete returns an error naming the credential for which IsNotFound
// holds, so a caller treats a credential already gone as deleted.
func TestDeleteApplicationCredential(t *testing.T) {
	cases := []struct {
		name     string
		reply    keystoneReply
		notFound bool
	}{
		{"deleted", keystoneReply{status: http.StatusNoContent}, false},
		{"already gone", errorReply(http.StatusNotFound, "Could not find application credential: acid1."), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, seen := delegationKeystone(t, map[string]keystoneReply{
				"POST /auth/tokens": issuedReply(ownerTok),
				"DELETE /users/uid1/application_credentials/acid1": tc.reply,
			})
			c, m, reader := telemetryTestClient(t, ts)

			res := resource.Resource{Kind: KindApplicationCredential, Logical: "appcred-0001", ID: "uid1:acid1"}
			err := c.DeleteApplicationCredential(context.Background(), testAppCred, res, "did1", "user-pw", "pid1")
			if tc.notFound {
				if !IsNotFound(err) {
					t.Errorf("IsNotFound(%v) = false, want true", err)
				}
				if want := `deleting application credential "appcred-0001": `; err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Errorf("error %v does not start with %q", err, want)
				}
			} else if err != nil {
				t.Fatalf("DeleteApplicationCredential: %v", err)
			}

			reqs := seen()
			wantRoutes := []string{"POST /auth/tokens", "DELETE /users/uid1/application_credentials/acid1"}
			if !slices.Equal(routesOf(reqs), wantRoutes) {
				t.Fatalf("requests = %v, want %v", routesOf(reqs), wantRoutes)
			}
			checkOwnerTokenRequest(t, reqs[0], "dizzy-run0-user-0001", "did1", "user-pw", "pid1")
			if reqs[1].authToken != ownerTok {
				t.Errorf("delete X-Auth-Token = %q, want the owner token %q", reqs[1].authToken, ownerTok)
			}

			if st := statsOf(m.Aggregate(time.Second), string(KindApplicationCredential)); st.Attempted != 1 || st.Failed != boolToInt(tc.notFound) {
				t.Errorf("application_credential samples = %+v, want 1 attempted and %d failed", st, boolToInt(tc.notFound))
			}
			outcome := "success"
			if tc.notFound {
				outcome = "error"
			}
			wantOp := map[string]string{"kind": "application_credential", "operation": "delete", "outcome": outcome}
			if !hasOperationPoint(t, reader, wantOp) {
				t.Errorf("no dizzy.operation.duration point with %v", wantOp)
			}
		})
	}
}

// TestApplicationCredentialIDRoundTrip confirms parseApplicationCredentialID
// inverts applicationCredentialID and rejects an id that is not two non-empty
// parts, with the malformed-id message.
func TestApplicationCredentialIDRoundTrip(t *testing.T) {
	id := applicationCredentialID("uid1", "acid1")
	if id != "uid1:acid1" {
		t.Errorf("applicationCredentialID(uid1, acid1) = %q, want uid1:acid1", id)
	}
	userID, credentialID, err := parseApplicationCredentialID(id)
	if err != nil || userID != "uid1" || credentialID != "acid1" {
		t.Errorf("parseApplicationCredentialID(%q) = (%q, %q, %v), want (uid1, acid1, nil)", id, userID, credentialID, err)
	}
	for _, bad := range []string{"", "acid1", "uid1:", ":acid1", "uid1:acid1:x"} {
		_, _, err := parseApplicationCredentialID(bad)
		if want := `malformed application credential id "` + bad + `"`; err == nil || err.Error() != want {
			t.Errorf("parseApplicationCredentialID(%q) error = %v, want %q", bad, err, want)
		}
	}
}

// checkObserveDelegation runs Observe on res against a Keystone that answers
// the GET of path with 200 and okBody, 404, 403 and 500 in turn, and confirms
// ("", true, nil), ("", false, nil), ("unknown", true, nil) and an error
// carrying the 500, each read with the run's own (admin) token.
func checkObserveDelegation(t *testing.T, res resource.Resource, path, okBody string) {
	t.Helper()
	cases := []struct {
		name       string
		reply      keystoneReply
		wantStatus string
		wantExists bool
		wantErr    bool
	}{
		{"present", keystoneReply{status: http.StatusOK, body: okBody}, "", true, false},
		{"gone", errorReply(http.StatusNotFound, "Could not find it."), "", false, false},
		{"forbidden", errorReply(http.StatusForbidden, "You are not authorized to perform the requested action."), "unknown", true, false},
		{"server error", errorReply(http.StatusInternalServerError, "An unexpected error prevented the server from fulfilling your request."), "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, seen := delegationKeystone(t, map[string]keystoneReply{"GET " + path: tc.reply})
			c, _ := testClient(ts)
			c.gc.SetToken(adminToken)

			status, exists, err := c.Observe(context.Background(), res)
			if tc.wantErr {
				if err == nil || !gophercloud.ResponseCodeIs(err, http.StatusInternalServerError) {
					t.Errorf("Observe error = %v, want the 500", err)
				}
			} else if err != nil {
				t.Errorf("Observe error = %v, want nil", err)
			}
			if status != tc.wantStatus || exists != tc.wantExists {
				t.Errorf("Observe = (%q, %v), want (%q, %v)", status, exists, tc.wantStatus, tc.wantExists)
			}
			reqs := seen()
			if routes := routesOf(reqs); !slices.Equal(routes, []string{"GET " + path}) {
				t.Fatalf("requests = %v, want one GET %s", routes, path)
			}
			if reqs[0].authToken != adminToken {
				t.Errorf("X-Auth-Token = %q, want the run's own token %q", reqs[0].authToken, adminToken)
			}
		})
	}
}

// TestObserveApplicationCredential confirms status reads an application
// credential under its user (GET /users/<user>/application_credentials/<id>)
// as present on 200, gone on 404, unknown but present on 403 (a domain manager
// may not read it), and as an error on 500, and that a malformed id is an
// error rather than gone.
func TestObserveApplicationCredential(t *testing.T) {
	res := resource.Resource{Kind: KindApplicationCredential, Logical: "appcred-0001", Name: "dizzy-run0-appcred-0001", ID: "uid1:acid1"}
	checkObserveDelegation(t, res, "/users/uid1/application_credentials/acid1",
		`{"application_credential":{"id":"acid1","name":"dizzy-run0-appcred-0001","project_id":"pid1","roles":[{"id":"rid1"}]}}`)

	ts, seen := delegationKeystone(t, nil)
	c, _ := testClient(ts)
	_, exists, err := c.Observe(context.Background(), resource.Resource{Kind: KindApplicationCredential, ID: "acid1"})
	if want := `malformed application credential id "acid1"`; err == nil || err.Error() != want || exists {
		t.Errorf("Observe with a malformed id = (exists %v, %v), want an error %q", exists, err, want)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}
