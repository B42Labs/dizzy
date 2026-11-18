package keystone

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// testTrust is the plan's trust the tests create: trust-0001, in which
// user-0002 delegates role-0001 on proj-0001 to user-0001.
var testTrust = keystoneplan.Trust{Name: "trust-0001", Trustor: "user-0002", Trustee: "user-0001", Project: "proj-0001", Role: "role-0001"}

// testTrustResource is the resource CreateTrust returns for testTrust.
var testTrustResource = resource.Resource{Kind: KindTrust, Logical: "trust-0001", ID: "trust-id1"}

// TestCreateTrust confirms a trust is created as its trustor: one token
// request as dizzy-run0-user-0002 with the trustor's password, in its domain
// and scoped to the project, then one POST /OS-TRUST/trusts carrying that
// token and a body with impersonation, the trustor, trustee and project ids
// and exactly one role, and nothing else. The returned resource is of kind
// trust with the trust's id and no name.
func TestCreateTrust(t *testing.T) {
	ts, seen := delegationKeystone(t, map[string]keystoneReply{
		"POST /auth/tokens": issuedReply(ownerTok),
		"POST /OS-TRUST/trusts": {
			status: http.StatusCreated,
			body:   `{"trust":{"id":"trust-id1","impersonation":true,"trustor_user_id":"uid2","trustee_user_id":"uid1","project_id":"pid1","roles":[{"id":"rid1"}]}}`,
		},
	})
	c, m, reader := telemetryTestClient(t, ts)

	got, err := c.CreateTrust(context.Background(), testTrust, "uid2", "did2", "trustor-pw", "uid1", "pid1", "rid1")
	if err != nil {
		t.Fatalf("CreateTrust: %v", err)
	}
	if got != testTrustResource {
		t.Errorf("CreateTrust = %+v, want %+v", got, testTrustResource)
	}
	if got.Name != "" {
		t.Errorf("trust Name = %q, want empty", got.Name)
	}

	reqs := seen()
	wantRoutes := []string{"POST /auth/tokens", "POST /OS-TRUST/trusts"}
	if !slices.Equal(routesOf(reqs), wantRoutes) {
		t.Fatalf("requests = %v, want %v", routesOf(reqs), wantRoutes)
	}
	checkOwnerTokenRequest(t, reqs[0], "dizzy-run0-user-0002", "did2", "trustor-pw", "pid1")
	create := reqs[1]
	if create.authToken != ownerTok {
		t.Errorf("create X-Auth-Token = %q, want the trustor's token %q", create.authToken, ownerTok)
	}
	wantBody := map[string]any{"trust": map[string]any{
		"impersonation":   true,
		"trustor_user_id": "uid2",
		"trustee_user_id": "uid1",
		"project_id":      "pid1",
		"roles":           []any{map[string]any{"id": "rid1"}},
	}}
	if !reflect.DeepEqual(create.body, wantBody) {
		t.Errorf("create body = %v, want %v", create.body, wantBody)
	}

	agg := m.Aggregate(time.Second)
	if st := statsOf(agg, string(KindToken)); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("token samples = %+v, want 1 attempted and 1 succeeded (the trustor's token)", st)
	}
	if st := statsOf(agg, string(KindTrust)); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("trust samples = %+v, want 1 attempted and 1 succeeded", st)
	}
	wantOp := map[string]string{"kind": "trust", "operation": "create", "outcome": "success"}
	if !hasOperationPoint(t, reader, wantOp) {
		t.Errorf("no dizzy.operation.duration point with %v", wantOp)
	}
	if tok := c.gc.Token(); tok != adminToken {
		t.Errorf("admin token after the create = %q, want %q unchanged", tok, adminToken)
	}
}

// TestCreateTrustErrors confirms a failed trustor token and a failed trust
// create both name the trust: a 401 on the token wraps the 401 and sends no
// create, and a 403 on the create carries ErrForbidden, so the executor fails
// fast.
func TestCreateTrustErrors(t *testing.T) {
	cases := []struct {
		name       string
		routes     map[string]keystoneReply
		wantPrefix string
		wantStatus int
		forbidden  bool
		wantRoutes []string
	}{
		{
			name: "trustor token rejected",
			routes: map[string]keystoneReply{
				"POST /auth/tokens": errorReply(http.StatusUnauthorized, "The request you have made requires authentication."),
			},
			wantPrefix: `creating trust "trust-0001": issuing token for user "user-0002" on project pid1: `,
			wantStatus: http.StatusUnauthorized,
			wantRoutes: []string{"POST /auth/tokens"},
		},
		{
			name: "create forbidden",
			routes: map[string]keystoneReply{
				"POST /auth/tokens":     issuedReply(ownerTok),
				"POST /OS-TRUST/trusts": errorReply(http.StatusForbidden, "The authenticated user should match the trustor."),
			},
			wantPrefix: `creating trust "trust-0001": `,
			wantStatus: http.StatusForbidden,
			forbidden:  true,
			wantRoutes: []string{"POST /auth/tokens", "POST /OS-TRUST/trusts"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, seen := delegationKeystone(t, tc.routes)
			c, _ := testClient(ts)

			_, err := c.CreateTrust(context.Background(), testTrust, "uid2", "did2", "trustor-pw", "uid1", "pid1", "rid1")
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantPrefix) {
				t.Fatalf("CreateTrust error = %v, want it to start with %q", err, tc.wantPrefix)
			}
			if !gophercloud.ResponseCodeIs(err, tc.wantStatus) {
				t.Errorf("ResponseCodeIs(%v, %d) = false, want the status wrapped", err, tc.wantStatus)
			}
			if got := errors.Is(err, ErrForbidden); got != tc.forbidden {
				t.Errorf("errors.Is(%v, ErrForbidden) = %v, want %v", err, got, tc.forbidden)
			}
			if routes := routesOf(seen()); !slices.Equal(routes, tc.wantRoutes) {
				t.Errorf("requests = %v, want %v", routes, tc.wantRoutes)
			}
		})
	}
}

// TestAuthenticateTrust confirms the read-back is one password token request
// as the trustee's prefixed name, with its domain id and password, scoped to
// the trust ({"OS-TRUST:trust":{"id":<trust id>}}), recorded as one
// trust/authenticate sample and one time-to-ready record of kind trust, and
// that a 401 fails it with an error that starts by naming the trust and the
// trustee and keeps the 401 reachable.
func TestAuthenticateTrust(t *testing.T) {
	cases := []struct {
		name    string
		reply   keystoneReply
		wantErr bool
	}{
		{"authenticated", issuedReply("trust-tok"), false},
		{"rejected", errorReply(http.StatusUnauthorized, "The request you have made requires authentication."), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, seen := delegationKeystone(t, map[string]keystoneReply{"POST /auth/tokens": tc.reply})
			c, m, reader := telemetryTestClient(t, ts)
			// A re-authenticating admin client would retry a 401 after
			// re-authenticating; the trustee's request must not reach it.
			var reauths int
			c.gc.ReauthFunc = func(context.Context) error {
				reauths++
				return nil
			}

			err := c.AuthenticateTrust(context.Background(), testTrust, testTrustResource, "did1", "trustee-pw")
			if reauths != 0 {
				t.Errorf("admin re-authentications = %d, want 0", reauths)
			}
			if tc.wantErr {
				const wantPrefix = `authenticating trust "trust-0001" as "user-0001"`
				if err == nil || !strings.HasPrefix(err.Error(), wantPrefix) {
					t.Errorf("AuthenticateTrust error = %v, want it to start with %q", err, wantPrefix)
				}
				if !gophercloud.ResponseCodeIs(err, http.StatusUnauthorized) {
					t.Errorf("ResponseCodeIs(%v, 401) = false, want the 401 reachable", err)
				}
			} else if err != nil {
				t.Fatalf("AuthenticateTrust: %v", err)
			}

			reqs := seen()
			if routes := routesOf(reqs); !slices.Equal(routes, []string{"POST /auth/tokens"}) {
				t.Fatalf("requests = %v, want one token request", routes)
			}
			body := reqs[0].body
			if got := jsonAt(body, "auth", "identity", "methods"); !reflect.DeepEqual(got, []any{"password"}) {
				t.Errorf("identity methods = %v, want [password]", got)
			}
			user := jsonAt(body, "auth", "identity", "password", "user")
			if got := jsonAt(user, "name"); got != "dizzy-run0-user-0001" {
				t.Errorf("trustee name = %v, want dizzy-run0-user-0001", got)
			}
			if got := jsonAt(user, "domain", "id"); got != "did1" {
				t.Errorf("trustee domain id = %v, want did1", got)
			}
			if got := jsonAt(user, "password"); got != "trustee-pw" {
				t.Errorf("trustee password = %v, want trustee-pw", got)
			}
			wantScope := map[string]any{"OS-TRUST:trust": map[string]any{"id": "trust-id1"}}
			if got := jsonAt(body, "auth", "scope"); !reflect.DeepEqual(got, wantScope) {
				t.Errorf("auth scope = %v, want %v", got, wantScope)
			}

			agg := m.Aggregate(time.Second)
			if st := statsOf(agg, string(KindTrust)); st.Attempted != 1 || st.Failed != boolToInt(tc.wantErr) {
				t.Errorf("trust samples = %+v, want 1 attempted and %d failed", st, boolToInt(tc.wantErr))
			}
			if r := readinessOf(agg, string(KindTrust)); r.Count != 1 || r.OK != boolToInt(!tc.wantErr) {
				t.Errorf("trust time-to-ready = %+v, want 1 record, %d ok", r, boolToInt(!tc.wantErr))
			}
			if st := statsOf(agg, string(KindToken)); st.Attempted != 0 {
				t.Errorf("token samples = %+v, want none", st)
			}
			outcome := "success"
			if tc.wantErr {
				outcome = "error"
			}
			wantOp := map[string]string{"kind": "trust", "operation": "authenticate", "outcome": outcome}
			if !hasOperationPoint(t, reader, wantOp) {
				t.Errorf("no dizzy.operation.duration point with %v", wantOp)
			}
		})
	}
}

// TestDeleteTrust confirms a delete takes a token as the trustor and then
// sends DELETE /OS-TRUST/trusts/<id> with that token, and that a 404 on the
// delete returns an error naming the trust for which IsNotFound holds.
func TestDeleteTrust(t *testing.T) {
	cases := []struct {
		name     string
		reply    keystoneReply
		notFound bool
	}{
		{"deleted", keystoneReply{status: http.StatusNoContent}, false},
		{"already gone", errorReply(http.StatusNotFound, "Could not find trust: trust-id1."), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, seen := delegationKeystone(t, map[string]keystoneReply{
				"POST /auth/tokens":                 issuedReply(ownerTok),
				"DELETE /OS-TRUST/trusts/trust-id1": tc.reply,
			})
			c, m, reader := telemetryTestClient(t, ts)

			err := c.DeleteTrust(context.Background(), testTrust, testTrustResource, "did2", "trustor-pw", "pid1")
			if tc.notFound {
				if !IsNotFound(err) {
					t.Errorf("IsNotFound(%v) = false, want true", err)
				}
				if want := `deleting trust "trust-0001": `; err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Errorf("error %v does not start with %q", err, want)
				}
			} else if err != nil {
				t.Fatalf("DeleteTrust: %v", err)
			}

			reqs := seen()
			wantRoutes := []string{"POST /auth/tokens", "DELETE /OS-TRUST/trusts/trust-id1"}
			if !slices.Equal(routesOf(reqs), wantRoutes) {
				t.Fatalf("requests = %v, want %v", routesOf(reqs), wantRoutes)
			}
			checkOwnerTokenRequest(t, reqs[0], "dizzy-run0-user-0002", "did2", "trustor-pw", "pid1")
			if reqs[1].authToken != ownerTok {
				t.Errorf("delete X-Auth-Token = %q, want the trustor's token %q", reqs[1].authToken, ownerTok)
			}

			if st := statsOf(m.Aggregate(time.Second), string(KindTrust)); st.Attempted != 1 || st.Failed != boolToInt(tc.notFound) {
				t.Errorf("trust samples = %+v, want 1 attempted and %d failed", st, boolToInt(tc.notFound))
			}
			outcome := "success"
			if tc.notFound {
				outcome = "error"
			}
			wantOp := map[string]string{"kind": "trust", "operation": "delete", "outcome": outcome}
			if !hasOperationPoint(t, reader, wantOp) {
				t.Errorf("no dizzy.operation.duration point with %v", wantOp)
			}
		})
	}
}

// TestObserveTrust confirms status reads a trust (GET /OS-TRUST/trusts/<id>)
// as present on 200, gone on 404, unknown but present on 403 (a domain
// manager may not read it), and as an error on 500.
func TestObserveTrust(t *testing.T) {
	checkObserveDelegation(t, testTrustResource, "/OS-TRUST/trusts/trust-id1",
		`{"trust":{"id":"trust-id1","impersonation":true,"trustor_user_id":"uid2","trustee_user_id":"uid1","project_id":"pid1"}}`)
}
