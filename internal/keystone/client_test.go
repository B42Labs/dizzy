package keystone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// respErr builds a gophercloud unexpected-response error for the given status
// and body, the shape the classifiers branch on.
func respErr(status int, body string) error {
	return gophercloud.ErrUnexpectedResponseCode{Actual: status, Body: []byte(body)}
}

// testClient builds a Client (with a fresh collector) whose gophercloud identity
// calls hit ts.
func testClient(ts *httptest.Server) (*Client, *metrics.Collector) {
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	m := metrics.NewCollector()
	return New(gc, "run0", m), m
}

func TestResourceNameAndProjectTags(t *testing.T) {
	if got := resourceName("abcd1234", "proj-0001"); got != "dizzy-abcd1234-proj-0001" {
		t.Errorf("resourceName = %q", got)
	}

	var body map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/projects") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"project":{"id":"pid1","name":"dizzy-run0-proj-0001"}}`)
	}))
	defer ts.Close()

	c, _ := testClient(ts)
	res, err := c.CreateProject(context.Background(), keystoneplan.Project{Name: "proj-0001", Domain: "dom-0001"}, "did1")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if res.ID != "pid1" || res.Name != "dizzy-run0-proj-0001" {
		t.Errorf("resource = %+v, want id pid1 and the prefixed name", res)
	}

	project, _ := body["project"].(map[string]any)
	tags, _ := project["tags"].([]any)
	want := map[string]bool{"dizzy:run=run0": false, "dizzy:type=project": false}
	for _, tg := range tags {
		if s, ok := tg.(string); ok {
			if _, exists := want[s]; exists {
				want[s] = true
			}
		}
	}
	for tag, seen := range want {
		if !seen {
			t.Errorf("create body missing tag %q; body=%v", tag, body)
		}
	}
}

// TestClassification pins the retry, forbidden, and error-kind classification
// that drives the executor's retry/fail-fast decisions. Keystone has no quota,
// so a 403 stays http_403 (read as forbidden via the wrap, not this label), and
// a 409 conflict is terminal (names are unique per scope).
func TestClassification(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		retryable   bool
		forbidden   bool
		wantErrKind string
	}{
		{"nil", nil, false, false, ""},
		{"500", respErr(500, ""), true, false, "http_500"},
		{"503", respErr(503, ""), true, false, "http_503"},
		{"429", respErr(429, ""), true, false, "http_429"},
		{"409 conflict is terminal", respErr(409, "duplicate"), false, false, "http_409"},
		{"403 forbidden", respErr(403, "policy"), false, true, "http_403"},
		{"404", respErr(404, ""), false, false, "http_404"},
		{"418 collapses to http_other", respErr(418, ""), false, false, "http_other"},
		{"timeout", context.DeadlineExceeded, false, false, "timeout"},
		{"canceled", context.Canceled, false, false, "canceled"},
		{"transport", errors.New("connection refused"), false, false, "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.retryable {
				t.Errorf("IsRetryable = %v, want %v", got, tc.retryable)
			}
			if got := IsForbidden(tc.err); got != tc.forbidden {
				t.Errorf("IsForbidden = %v, want %v", got, tc.forbidden)
			}
			if got := errKind(tc.err); got != tc.wantErrKind {
				t.Errorf("errKind = %q, want %q", got, tc.wantErrKind)
			}
		})
	}
}

// TestWrapForbidden verifies a 403 is wrapped so errors.Is finds ErrForbidden
// (for the executor's fail-fast) while the underlying response error stays in
// the chain (so classification still sees the 403), and that a non-403 error is
// neither.
func TestWrapForbidden(t *testing.T) {
	wrapped := wrapCreate(KindDomain, "dom-0001", respErr(403, "policy denied"))
	if !errors.Is(wrapped, ErrForbidden) {
		t.Errorf("wrapped 403 does not match ErrForbidden: %v", wrapped)
	}
	if IsRetryable(wrapped) {
		t.Error("wrapped 403 must not be retryable")
	}

	plain := wrapCreate(KindDomain, "dom-0002", respErr(503, ""))
	if errors.Is(plain, ErrForbidden) {
		t.Error("non-403 error must not match ErrForbidden")
	}
	if !IsRetryable(plain) {
		t.Error("wrapped 503 error should remain retryable")
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(respErr(404, "")) {
		t.Error("IsNotFound(404) = false, want true")
	}
	if IsNotFound(respErr(500, "")) {
		t.Error("IsNotFound(500) = true, want false")
	}
	if IsNotFound(nil) {
		t.Error("IsNotFound(nil) = true, want false")
	}
}

// TestListUsersByPrefixClientSideFilter simulates a backend that returns every
// user in the domain; the client must keep only those carrying this run's name
// prefix, so cleanup never deletes a user the tool did not name.
func TestListUsersByPrefixClientSideFilter(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"users":[
			{"id":"u-mine","name":"dizzy-run0-user-0001"},
			{"id":"u-theirs","name":"dizzy-run9-user-0001"},
			{"id":"u-foreign","name":"admin"}
		]}`)
	}))
	defer ts.Close()

	c, _ := testClient(ts)
	got, err := c.ListUsersByPrefix(context.Background(), "run0")
	if err != nil {
		t.Fatalf("ListUsersByPrefix: %v", err)
	}
	if len(got) != 1 || got[0].ID != "u-mine" {
		t.Fatalf("ListUsersByPrefix = %+v, want only this run's user", got)
	}
}

// TestListDomainsByPrefixToleratesForbidden confirms a 403 on the domain list
// (a domain manager may not list domains) fails open: empty result, no error.
func TestListDomainsByPrefixToleratesForbidden(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"forbidden":{"code":403,"message":"You are not authorized"}}`)
	}))
	defer ts.Close()

	c, _ := testClient(ts)
	got, err := c.ListDomainsByPrefix(context.Background(), "run0")
	if err != nil {
		t.Fatalf("ListDomainsByPrefix must fail open on a 403, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListDomainsByPrefix returned %d domains on a 403, want 0", len(got))
	}
}

// TestFindRoleByName confirms the lookup prefers the global role over a
// domain-scoped one of the same name, and that a name no role has fails
// with the binder's message wrapping ErrRoleNotFound, while a failed listing
// does not wrap it.
func TestFindRoleByName(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/roles") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		if r.URL.Query().Get("name") == "reader" {
			_, _ = io.WriteString(w, `{"roles":[
				{"id":"r-domain","name":"reader","domain_id":"d1"},
				{"id":"r-global","name":"reader","domain_id":""}
			]}`)
			return
		}
		_, _ = io.WriteString(w, `{"roles":[]}`)
	}))
	defer ts.Close()
	c, _ := testClient(ts)
	ctx := context.Background()

	if id, err := c.FindRoleByName(ctx, "reader"); err != nil || id != "r-global" {
		t.Errorf("FindRoleByName(reader) = %q, %v; want the global role r-global", id, err)
	}
	_, err := c.FindRoleByName(ctx, "auditor")
	const want = `role "auditor" not found; grant it to the manager or pass an existing role with --roles`
	if err == nil || err.Error() != want || !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("FindRoleByName(auditor) error = %v, want %q wrapping ErrRoleNotFound", err, want)
	}
	status.Store(http.StatusServiceUnavailable)
	if _, err := c.FindRoleByName(ctx, "reader"); err == nil || errors.Is(err, ErrRoleNotFound) || !IsRetryable(err) {
		t.Errorf("FindRoleByName on a 503 = %v, want a retryable error that is not ErrRoleNotFound", err)
	}
}

// TestAssignmentIDRoundTrip covers both scopes: the synthetic id built by
// AssignRole parses back to the same fields, and a malformed id is rejected.
func TestAssignmentIDRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want assignmentTarget
	}{
		{"project scope", "uid1:project:pid1:rid1", assignmentTarget{userID: "uid1", kind: "project", targetID: "pid1", roleID: "rid1"}},
		{"domain scope", "uid2:domain:did2:rid2", assignmentTarget{userID: "uid2", kind: "domain", targetID: "did2", roleID: "rid2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAssignmentID(tc.id)
			if err != nil {
				t.Fatalf("parseAssignmentID(%q): %v", tc.id, err)
			}
			if got != tc.want {
				t.Errorf("parseAssignmentID(%q) = %+v, want %+v", tc.id, got, tc.want)
			}
		})
	}
	if _, err := parseAssignmentID("not-an-id"); err == nil {
		t.Error("parseAssignmentID of a malformed id: expected an error, got nil")
	}

	if got := assignmentLogical(keystoneplan.Assignment{User: "user-0001", Role: "role-0001", Project: "proj-0001"}); got != "user-0001->proj-0001/role-0001" {
		t.Errorf("assignmentLogical (project) = %q", got)
	}
	if got := assignmentLogical(keystoneplan.Assignment{User: "user-0001", Role: "role-0002"}); got != "user-0001->domain/role-0002" {
		t.Errorf("assignmentLogical (domain) = %q", got)
	}
}

// TestDeleteAssignmentUnassigns confirms deleting an assignment resource issues
// a DELETE to the unassign URL for the parsed user/project/role.
func TestDeleteAssignmentUnassigns(t *testing.T) {
	var gotPath, gotMethod string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	c, _ := testClient(ts)
	err := c.Delete(context.Background(), resource.Resource{
		Kind: KindAssignment,
		ID:   "uid1:project:pid1:rid1",
	})
	if err != nil {
		t.Fatalf("Delete(assignment): %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if want := "/projects/pid1/users/uid1/roles/rid1"; !strings.HasSuffix(gotPath, want) {
		t.Errorf("unassign path = %q, want it to end with %q", gotPath, want)
	}
}

// TestIssueTokenRecordsOperationAndReadiness confirms a token issue records both
// a token/create operation sample and a token time-to-ready sample, and that the
// request authenticates as the created user scoped to the project.
func TestIssueTokenRecordsOperationAndReadiness(t *testing.T) {
	var body map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/auth/tokens") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("X-Subject-Token", "tok-123")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":{"expires_at":"2030-01-01T00:00:00Z"}}`)
	}))
	defer ts.Close()

	c, m := testClient(ts)
	err := c.IssueToken(context.Background(), keystoneplan.TokenIssue{User: "user-0001", Project: "proj-0001"}, "did1", "s3cret", "pid1")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	agg := m.Aggregate(time.Second)
	var tokenOps int
	for _, s := range agg.ByType {
		if s.Type == string(KindToken) {
			tokenOps = s.Attempted
		}
	}
	if tokenOps != 1 {
		t.Errorf("token operation samples = %d, want 1", tokenOps)
	}
	var readyCount int
	for _, r := range agg.Readiness {
		if r.Type == string(KindToken) {
			readyCount = r.Count
		}
	}
	if readyCount != 1 {
		t.Errorf("token time-to-ready samples = %d, want 1", readyCount)
	}

	// The request authenticates as the created (prefixed) user scoped to the
	// project — never the plan's bare logical name and never the plaintext
	// password anywhere but the request body.
	auth, _ := body["auth"].(map[string]any)
	ident, _ := auth["identity"].(map[string]any)
	pw, _ := ident["password"].(map[string]any)
	user, _ := pw["user"].(map[string]any)
	if name, _ := user["name"].(string); name != "dizzy-run0-user-0001" {
		t.Errorf("auth username = %q, want the prefixed user name", name)
	}
	scope, _ := auth["scope"].(map[string]any)
	project, _ := scope["project"].(map[string]any)
	if id, _ := project["id"].(string); id != "pid1" {
		t.Errorf("auth scope project id = %q, want pid1", id)
	}
}

// TestIssueTokenNeverReauthenticates confirms a 401 on the created user's token
// request is that request's own failure: the admin client does not
// re-authenticate and repeat it, which would hide a password that has not
// replicated yet.
func TestIssueTokenNeverReauthenticates(t *testing.T) {
	var posts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/auth/tokens") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		posts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":401,"title":"Unauthorized","message":"The request you have made requires authentication."}}`)
	}))
	defer ts.Close()

	c, _ := testClient(ts)
	var reauths int
	c.gc.ReauthFunc = func(context.Context) error {
		reauths++
		return nil
	}
	err := c.IssueToken(context.Background(), keystoneplan.TokenIssue{User: "user-0001", Project: "proj-0001"}, "did1", "s3cret", "pid1")
	if got := errKind(err); got != "http_401" {
		t.Errorf("errKind(%v) = %q, want http_401", err, got)
	}
	if posts != 1 {
		t.Errorf("token requests = %d, want 1", posts)
	}
	if reauths != 0 {
		t.Errorf("admin re-authentications = %d, want 0", reauths)
	}
	if c.gc.ReauthFunc == nil {
		t.Error("IssueToken cleared the admin client's ReauthFunc")
	}
}

// TestIssueTokenDuringAdminReauth confirms token issues are safe while another
// worker's 401 re-authenticates the shared admin client, since the chaos graph
// issues tokens concurrently with other admin requests. The race detector
// reports an unguarded read of the admin token.
func TestIssueTokenDuringAdminReauth(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Subject-Token", "tok-123")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":{"expires_at":"2030-01-01T00:00:00Z"}}`)
	}))
	defer ts.Close()

	c, _ := testClient(ts)
	c.gc.UseTokenLock()
	c.gc.ReauthFunc = func(context.Context) error {
		c.gc.SetToken("tok-2")
		return nil
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = c.gc.Reauthenticate(context.Background(), "")
			}
		}
	}()
	for range 20 {
		if err := c.IssueToken(context.Background(), keystoneplan.TokenIssue{User: "user-0001", Project: "proj-0001"}, "did1", "s3cret", "pid1"); err != nil {
			t.Errorf("IssueToken: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestDisableDomainRecordsUpdate confirms disabling a domain issues a PATCH with
// enabled=false and records the new "update" operation label.
func TestDisableDomainRecordsUpdate(t *testing.T) {
	var body map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"domain":{"id":"did1","name":"dizzy-run0-dom-0001","enabled":false}}`)
	}))
	defer ts.Close()

	c, m := testClient(ts)
	if err := c.DisableDomain(context.Background(), resource.Resource{Kind: KindDomain, ID: "did1"}); err != nil {
		t.Fatalf("DisableDomain: %v", err)
	}

	domain, _ := body["domain"].(map[string]any)
	if enabled, ok := domain["enabled"].(bool); !ok || enabled {
		t.Errorf("update body enabled = %v, want false; body=%v", domain["enabled"], body)
	}

	agg := m.Aggregate(time.Second)
	var updates int
	for _, s := range agg.ByType {
		if s.Type == string(KindDomain) {
			updates += s.Attempted
		}
	}
	if updates != 1 {
		t.Errorf("domain operation samples = %d, want 1 (the update)", updates)
	}
}

// TestRandomPasswordUnique confirms each call returns a fresh, non-empty password
// so a user's credential cannot be recomputed from its cloud-visible name or the
// run's seed.
func TestRandomPasswordUnique(t *testing.T) {
	a, err := RandomPassword()
	if err != nil {
		t.Fatalf("RandomPassword: %v", err)
	}
	if a == "" {
		t.Error("RandomPassword returned an empty password")
	}
	b, err := RandomPassword()
	if err != nil {
		t.Fatalf("RandomPassword: %v", err)
	}
	if a == b {
		t.Error("two RandomPassword calls returned the same value; it must be random")
	}
}

// adminToken is the token the admin client of telemetryTestClient carries, and
// poolToken the created user's token the pressure calls send, so a test can
// tell the two apart in the request headers.
const (
	adminToken = "admin-tok"
	poolToken  = "pool-tok"
)

// telemetryTestClient builds a testClient that carries adminToken and mirrors
// each timed call into a ManualReader-backed telemetry, so a test can assert
// the request headers, the in-memory sample and the live operation series of
// one call.
func telemetryTestClient(t *testing.T, ts *httptest.Server) (*Client, *metrics.Collector, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	tel, err := telemetry.NewWithProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatalf("NewWithProvider: %v", err)
	}
	c, m := testClient(ts)
	c.gc.SetToken(adminToken)
	c.SetTelemetry(tel)
	return c, m, reader
}

// hasOperationPoint reports whether dizzy.operation.duration has a data point
// whose attributes are exactly want.
func hasOperationPoint(t *testing.T, reader *sdkmetric.ManualReader, want map[string]string) bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "dizzy.operation.duration" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is not a float64 histogram: %T", m.Name, m.Data)
			}
			for _, dp := range h.DataPoints {
				if attrsEqual(dp.Attributes, want) {
					return true
				}
			}
		}
	}
	return false
}

// attrsEqual reports whether set has exactly the wanted string attributes.
func attrsEqual(set attribute.Set, want map[string]string) bool {
	if set.Len() != len(want) {
		return false
	}
	for k, v := range want {
		got, ok := set.Value(attribute.Key(k))
		if !ok || got.AsString() != v {
			return false
		}
	}
	return true
}

// statsOf returns the aggregate's stats for one sample type, or zero stats when
// no sample of that type was recorded.
func statsOf(agg metrics.Aggregate, typ string) metrics.Stats {
	for _, s := range agg.ByType {
		if s.Type == typ {
			return s
		}
	}
	return metrics.Stats{}
}

// errorCount returns how many samples of the aggregate carry error kind.
func errorCount(agg metrics.Aggregate, kind string) int {
	for _, e := range agg.Errors {
		if e.Kind == kind {
			return e.Count
		}
	}
	return 0
}

// TestValidateToken confirms ValidateToken sends the request keystonemiddleware
// makes on a cache miss (GET /auth/tokens with the admin token as X-Auth-Token
// and the pool token as X-Subject-Token), records it as a token sample and as
// the token/validate operation, and that a 404 for an invalid token classifies
// as http_404 in the samples and as not-found for the bucket breakdown, with
// the token kept out of the error text although the 404 body names it.
func TestValidateToken(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		wantErrKind  string
		wantClassify string
		wantOutcome  string
	}{
		{"valid token", http.StatusOK, "", "", "success"},
		{"invalid token is a 404", http.StatusNotFound, "http_404", "not-found", "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests int
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/auth/tokens") {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("X-Subject-Token"); got != poolToken {
					t.Errorf("X-Subject-Token = %q, want the pool token %q", got, poolToken)
				}
				if got := r.Header.Get("X-Auth-Token"); got != adminToken {
					t.Errorf("X-Auth-Token = %q, want the admin token %q", got, adminToken)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					_, _ = io.WriteString(w, `{"token":{"expires_at":"2030-01-01T00:00:00.000000Z"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"error":{"code":404,"title":"Not Found","message":"Could not find token: `+poolToken+`."}}`)
			}))
			defer ts.Close()

			c, m, reader := telemetryTestClient(t, ts)
			err := c.ValidateToken(context.Background(), poolToken)
			if got := errKind(err); got != tc.wantErrKind {
				t.Errorf("errKind(%v) = %q, want %q", err, got, tc.wantErrKind)
			}
			if got := ClassifyOperationError(err); got != tc.wantClassify {
				t.Errorf("ClassifyOperationError(%v) = %q, want %q", err, got, tc.wantClassify)
			}
			if err != nil && strings.Contains(err.Error(), poolToken) {
				t.Errorf("error %q carries the token", err)
			}
			if requests != 1 {
				t.Errorf("requests = %d, want 1", requests)
			}

			agg := m.Aggregate(time.Second)
			st := statsOf(agg, string(KindToken))
			if st.Attempted != 1 || st.Failed != boolToInt(tc.wantErrKind != "") {
				t.Errorf("token samples = %+v, want 1 attempted and %d failed", st, boolToInt(tc.wantErrKind != ""))
			}
			if tc.wantErrKind != "" && errorCount(agg, tc.wantErrKind) != 1 {
				t.Errorf("errors = %+v, want one %s", agg.Errors, tc.wantErrKind)
			}
			want := map[string]string{"kind": "token", "operation": "validate", "outcome": tc.wantOutcome}
			if !hasOperationPoint(t, reader, want) {
				t.Errorf("no dizzy.operation.duration point with %v", want)
			}
		})
	}
}

// boolToInt returns 1 for true and 0 for false, for expected sample counts.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestRevokeToken confirms RevokeToken sends DELETE /auth/tokens with the pool
// token as X-Subject-Token under the admin token, and that a 204 records a
// successful token sample under the token/revoke operation.
func TestRevokeToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || !strings.HasSuffix(r.URL.Path, "/auth/tokens") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-Subject-Token"); got != poolToken {
			t.Errorf("X-Subject-Token = %q, want the pool token %q", got, poolToken)
		}
		if got := r.Header.Get("X-Auth-Token"); got != adminToken {
			t.Errorf("X-Auth-Token = %q, want the admin token %q", got, adminToken)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	c, m, reader := telemetryTestClient(t, ts)
	if err := c.RevokeToken(context.Background(), poolToken); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	st := statsOf(m.Aggregate(time.Second), string(KindToken))
	if st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("token samples = %+v, want 1 attempted and 1 succeeded", st)
	}
	want := map[string]string{"kind": "token", "operation": "revoke", "outcome": "success"}
	if !hasOperationPoint(t, reader, want) {
		t.Errorf("no dizzy.operation.duration point with %v", want)
	}
}

// TestRevokeTokenFailure confirms a failed revocation returns an error that
// keeps the response status for classification and whose text does not carry
// the token, although Keystone's 404 body names it.
func TestRevokeTokenFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":404,"title":"Not Found","message":"Could not find token: `+poolToken+`."}}`)
	}))
	defer ts.Close()

	c, m, _ := telemetryTestClient(t, ts)
	err := c.RevokeToken(context.Background(), poolToken)
	if got := ClassifyOperationError(err); got != "not-found" {
		t.Errorf("ClassifyOperationError(%v) = %q, want not-found", err, got)
	}
	if err == nil || strings.Contains(err.Error(), poolToken) {
		t.Errorf("error %v is nil or carries the token", err)
	}
	if got := errorCount(m.Aggregate(time.Second), "http_404"); got != 1 {
		t.Errorf("http_404 samples = %d, want 1", got)
	}
}

// assignmentsPage renders one page of a role assignment listing with n
// project-scoped assignments for userID and next as the links.next value.
func assignmentsPage(userID string, n int, next string) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"role":{"id":"rid%d"},"scope":{"project":{"id":"pid%d"}},"user":{"id":%q}}`, i, i, userID)
	}
	nextJSON := "null"
	if next != "" {
		nextJSON = fmt.Sprintf("%q", next)
	}
	return fmt.Sprintf(`{"role_assignments":[%s],"links":{"next":%s}}`, strings.Join(items, ","), nextJSON)
}

// TestListEffectiveAssignments confirms the listing asks Keystone for the
// user's effective assignments (effective=true and user.id in the query), counts
// them across every page, returns 0 and no error for an empty listing, and
// returns a wrapped error for a 500 that is recorded as a role_assignment sample
// with error kind http_500 under the role_assignment/list operation.
func TestListEffectiveAssignments(t *testing.T) {
	cases := []struct {
		name        string
		pages       []int // assignments per page; nil for a 500
		want        int
		wantErrKind string
	}{
		{"two pages", []int{2, 1}, 3, ""},
		{"empty listing", []int{0}, 0, ""},
		{"server error", nil, 0, "http_500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var queries []url.Values
			var ts *httptest.Server
			ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/role_assignments") {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				queries = append(queries, r.URL.Query())
				w.Header().Set("Content-Type", "application/json")
				if tc.pages == nil {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"error":{"code":500,"title":"Internal Server Error","message":"boom"}}`)
					return
				}
				page := 0
				if r.URL.Query().Get("page") == "2" {
					page = 1
				}
				next := ""
				if page+1 < len(tc.pages) {
					next = ts.URL + "/role_assignments?effective=true&user.id=uid1&page=2"
				}
				_, _ = io.WriteString(w, assignmentsPage("uid1", tc.pages[page], next))
			}))
			defer ts.Close()

			c, m, reader := telemetryTestClient(t, ts)
			got, err := c.ListEffectiveAssignments(context.Background(), "uid1")
			if gotKind := errKind(err); gotKind != tc.wantErrKind {
				t.Fatalf("errKind(%v) = %q, want %q", err, gotKind, tc.wantErrKind)
			}
			if got != tc.want {
				t.Errorf("ListEffectiveAssignments = %d, want %d", got, tc.want)
			}
			if len(queries) == 0 {
				t.Fatal("no request reached the server")
			}
			if q := queries[0]; q.Get("effective") != "true" || q.Get("user.id") != "uid1" {
				t.Errorf("first query = %v, want effective=true and user.id=uid1", q)
			}
			if wantRequests := max(len(tc.pages), 1); len(queries) != wantRequests {
				t.Errorf("requests = %d, want %d", len(queries), wantRequests)
			}

			agg := m.Aggregate(time.Second)
			st := statsOf(agg, string(KindAssignment))
			if st.Attempted != 1 {
				t.Errorf("role_assignment samples = %d, want 1 for the whole listing", st.Attempted)
			}
			outcome := "success"
			if tc.wantErrKind != "" {
				outcome = "error"
				if st.Failed != 1 || errorCount(agg, tc.wantErrKind) != 1 {
					t.Errorf("role_assignment samples = %+v, errors = %+v, want one failed %s", st, agg.Errors, tc.wantErrKind)
				}
				if !strings.Contains(err.Error(), "uid1") {
					t.Errorf("error %q does not name the user", err)
				}
			}
			want := map[string]string{"kind": "role_assignment", "operation": "list", "outcome": outcome}
			if !hasOperationPoint(t, reader, want) {
				t.Errorf("no dizzy.operation.duration point with %v", want)
			}
		})
	}
}

// projectsBody renders a project listing with n projects and no next page.
func projectsBody(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"id":"pid%d","name":"proj-%d"}`, i, i)
	}
	return fmt.Sprintf(`{"projects":[%s],"links":{"next":null}}`, strings.Join(items, ","))
}

// TestListUserProjects confirms the listing asks for the user's projects as the
// admin and returns their count, recorded as one project/list sample.
func TestListUserProjects(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/users/uid1/projects") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-Auth-Token"); got != adminToken {
			t.Errorf("X-Auth-Token = %q, want the admin token %q", got, adminToken)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, projectsBody(2))
	}))
	defer ts.Close()

	c, m, reader := telemetryTestClient(t, ts)
	got, err := c.ListUserProjects(context.Background(), "uid1")
	if err != nil {
		t.Fatalf("ListUserProjects: %v", err)
	}
	if got != 2 {
		t.Errorf("ListUserProjects = %d, want 2", got)
	}
	if st := statsOf(m.Aggregate(time.Second), string(KindProject)); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("project samples = %+v, want 1 attempted and 1 succeeded", st)
	}
	want := map[string]string{"kind": "project", "operation": "list", "outcome": "success"}
	if !hasOperationPoint(t, reader, want) {
		t.Errorf("no dizzy.operation.duration point with %v", want)
	}
}

// TestListAvailableProjects confirms the listing is sent with the pool token as
// X-Auth-Token, returns the project count, and leaves the admin client's token
// as it was; a 401 for the pool token is the request's own failure and never
// re-authenticates the admin client.
func TestListAvailableProjects(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		want        int
		wantErrKind string
	}{
		{"pool token accepted", http.StatusOK, 3, ""},
		{"pool token rejected", http.StatusUnauthorized, 0, "http_401"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests int
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/auth/projects") {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("X-Auth-Token"); got != poolToken {
					t.Errorf("X-Auth-Token = %q, want the pool token %q", got, poolToken)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.status != http.StatusOK {
					_, _ = io.WriteString(w, `{"error":{"code":401,"title":"Unauthorized","message":"The token `+poolToken+` is not valid."}}`)
					return
				}
				_, _ = io.WriteString(w, projectsBody(tc.want))
			}))
			defer ts.Close()

			c, m, _ := telemetryTestClient(t, ts)
			var reauths int
			c.gc.ReauthFunc = func(context.Context) error {
				reauths++
				return nil
			}
			got, err := c.ListAvailableProjects(context.Background(), poolToken)
			if gotKind := errKind(err); gotKind != tc.wantErrKind {
				t.Fatalf("errKind(%v) = %q, want %q", err, gotKind, tc.wantErrKind)
			}
			if got != tc.want {
				t.Errorf("ListAvailableProjects = %d, want %d", got, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), poolToken) {
				t.Errorf("error %q carries the token", err)
			}
			if tok := c.gc.Token(); tok != adminToken {
				t.Errorf("admin token after the call = %q, want %q unchanged", tok, adminToken)
			}
			if requests != 1 || reauths != 0 {
				t.Errorf("requests = %d and admin re-authentications = %d, want 1 and 0", requests, reauths)
			}
			if st := statsOf(m.Aggregate(time.Second), string(KindProject)); st.Attempted != 1 {
				t.Errorf("project samples = %d, want 1", st.Attempted)
			}
		})
	}
}

// TestIssueScopedTokenReturnsToken confirms the issued token carries the id
// from the X-Subject-Token response header and the expiry from the body, and
// that the issue still records one token/create sample and one token readiness
// record.
func TestIssueScopedTokenReturnsToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/auth/tokens") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("X-Subject-Token", "tok-123")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":{"expires_at":"2030-01-01T12:30:00.000000Z"}}`)
	}))
	defer ts.Close()

	c, m, reader := telemetryTestClient(t, ts)
	got, err := c.IssueScopedToken(context.Background(), keystoneplan.TokenIssue{User: "user-0001", Project: "proj-0001"}, "did1", "s3cret", "pid1")
	if err != nil {
		t.Fatalf("IssueScopedToken: %v", err)
	}
	if got.ID != "tok-123" {
		t.Errorf("token id = %q, want tok-123 from X-Subject-Token", got.ID)
	}
	if want := time.Date(2030, 1, 1, 12, 30, 0, 0, time.UTC); !got.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, want)
	}
	if got.Endpoints != nil {
		t.Errorf("Endpoints = %v, want nil for a client without SetEndpointOpts", got.Endpoints)
	}

	agg := m.Aggregate(time.Second)
	if st := statsOf(agg, string(KindToken)); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("token samples = %+v, want 1 attempted and 1 succeeded", st)
	}
	var ready int
	for _, r := range agg.Readiness {
		if r.Type == string(KindToken) {
			ready = r.Count
		}
	}
	if ready != 1 {
		t.Errorf("token readiness records = %d, want 1", ready)
	}
	want := map[string]string{"kind": "token", "operation": "create", "outcome": "success"}
	if !hasOperationPoint(t, reader, want) {
		t.Errorf("no dizzy.operation.duration point with %v", want)
	}
	if tok := c.gc.Token(); tok != adminToken {
		t.Errorf("admin token after the issue = %q, want %q unchanged", tok, adminToken)
	}
}

// catalogEntry renders one service catalog entry of type typ with an internal
// and a public endpoint in region, the internal one first so a lookup that
// ignores the interface picks the wrong one, and the public one at publicURL.
func catalogEntry(typ, region, publicURL string) string {
	return fmt.Sprintf(`{"type":%q,"endpoints":[`+
		`{"interface":"internal","region":%q,"region_id":%q,"url":"http://%s.internal:8080"},`+
		`{"interface":"public","region":%q,"region_id":%q,"url":%q}]}`,
		typ, region, region, typ, region, region, publicURL)
}

// catalogToken is a token-create response body whose token lists the catalog
// entries given, or carries no catalog key at all when none is given.
func catalogToken(entries ...string) string {
	if len(entries) == 0 {
		return `{"token":{"expires_at":"2030-01-01T00:00:00Z"}}`
	}
	return `{"token":{"expires_at":"2030-01-01T00:00:00Z","catalog":[` + strings.Join(entries, ",") + `]}}`
}

// servicesCatalog is a catalog listing identity, compute, network, volumev3
// (the alias Cinder's own catalog entry carries) and image, with Cinder's
// endpoints in cinderRegion and all others in RegionOne. The public URLs carry
// no trailing "/" and Cinder's names the token's project, as a real catalog
// does.
func servicesCatalog(cinderRegion string) string {
	return catalogToken(
		catalogEntry("identity", "RegionOne", "https://keystone.example/v3"),
		catalogEntry("compute", "RegionOne", "https://nova.example/v2.1"),
		catalogEntry("network", "RegionOne", "https://neutron.example:9696"),
		catalogEntry("volumev3", cinderRegion, "https://cinder.example/v3/pid1"),
		catalogEntry("image", "RegionOne", "https://glance.example"),
	)
}

// tokenServer is a Keystone that answers every token create with tok-123 and
// body, and counts the requests it receives.
func tokenServer(t *testing.T, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/auth/tokens") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("X-Subject-Token", "tok-123")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts, &requests
}

// regionOnePublic selects the public endpoints of RegionOne, as a clouds.yaml
// entry naming that region and interface does.
var regionOnePublic = gophercloud.EndpointOpts{Region: "RegionOne", Availability: gophercloud.AvailabilityPublic}

// issueAsUser0001 issues the token of user-0001 on pid1 through c.
func issueAsUser0001(c *Client) (IssuedToken, error) {
	return c.IssueScopedToken(context.Background(), keystoneplan.TokenIssue{User: "user-0001", Project: "proj-0001"}, "did1", "s3cret", "pid1")
}

// TestIssueScopedTokenResolvesEndpoints confirms a client with endpoint options
// maps the four services to the public RegionOne URLs of the token's own
// catalog, Cinder found under its volumev3 alias and its URL keeping the
// token's project, every URL normalized with a trailing "/", and that reading
// the catalog costs no request beyond the token create.
func TestIssueScopedTokenResolvesEndpoints(t *testing.T) {
	ts, requests := tokenServer(t, servicesCatalog("RegionOne"))
	c, m := testClient(ts)
	c.SetEndpointOpts(regionOnePublic)

	got, err := issueAsUser0001(c)
	if err != nil {
		t.Fatalf("IssueScopedToken: %v", err)
	}
	want := map[string]string{
		"nova":    "https://nova.example/v2.1/",
		"neutron": "https://neutron.example:9696/",
		"cinder":  "https://cinder.example/v3/pid1/",
		"glance":  "https://glance.example/",
	}
	if !maps.Equal(got.Endpoints, want) {
		t.Errorf("Endpoints = %v, want %v", got.Endpoints, want)
	}
	if got.ID != "tok-123" {
		t.Errorf("token id = %q, want tok-123", got.ID)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
	if st := statsOf(m.Aggregate(time.Second), string(KindToken)); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("token samples = %+v, want 1 attempted and 1 succeeded", st)
	}
}

// TestIssueScopedTokenWithoutEndpointOpts confirms a client without
// SetEndpointOpts leaves Endpoints nil although the response carries a
// catalog, so apply, chaos, monitor and the churn graph issue as before.
func TestIssueScopedTokenWithoutEndpointOpts(t *testing.T) {
	ts, _ := tokenServer(t, servicesCatalog("RegionOne"))
	c, _ := testClient(ts)

	got, err := issueAsUser0001(c)
	if err != nil {
		t.Fatalf("IssueScopedToken: %v", err)
	}
	if got.Endpoints != nil {
		t.Errorf("Endpoints = %v, want nil", got.Endpoints)
	}
}

// TestIssueScopedTokenCinderSkipsVolumeV2 confirms Cinder resolves to the
// first catalog entry of type block-storage, volumev3, volume or block-store
// and never to a volumev2 entry, whose type names an API Cinder no longer
// serves, also when the catalog lists it first; a catalog whose only Cinder
// entry is volumev2 lists no cinder. Each issue stays one request.
func TestIssueScopedTokenCinderSkipsVolumeV2(t *testing.T) {
	const v2, v3 = "https://cinder.example/v2/pid1", "https://cinder.example/v3/pid1"
	cases := []struct {
		name    string
		entries []string
		want    string // the cinder endpoint, empty for none
	}{
		{"volumev2 before volumev3", []string{catalogEntry("volumev2", "RegionOne", v2), catalogEntry("volumev3", "RegionOne", v3)}, v3 + "/"},
		{"volumev2 before block-storage", []string{catalogEntry("volumev2", "RegionOne", v2), catalogEntry("block-storage", "RegionOne", v3)}, v3 + "/"},
		{"volumev2 before volume", []string{catalogEntry("volumev2", "RegionOne", v2), catalogEntry("volume", "RegionOne", v3)}, v3 + "/"},
		{"volumev2 before block-store", []string{catalogEntry("volumev2", "RegionOne", v2), catalogEntry("block-store", "RegionOne", v3)}, v3 + "/"},
		{"volumev2 only", []string{catalogEntry("volumev2", "RegionOne", v2)}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := append([]string{catalogEntry("compute", "RegionOne", "https://nova.example/v2.1")}, tc.entries...)
			ts, requests := tokenServer(t, catalogToken(entries...))
			c, _ := testClient(ts)
			c.SetEndpointOpts(regionOnePublic)

			got, err := issueAsUser0001(c)
			if err != nil {
				t.Fatalf("IssueScopedToken: %v", err)
			}
			url, ok := got.Endpoints["cinder"]
			if tc.want == "" && ok {
				t.Errorf("Endpoints[cinder] = %q, want no cinder", url)
			}
			if tc.want != "" && url != tc.want {
				t.Errorf("Endpoints[cinder] = %q, want %q", url, tc.want)
			}
			if got.Endpoints["nova"] != "https://nova.example/v2.1/" {
				t.Errorf("Endpoints[nova] = %q, want the compute entry's URL", got.Endpoints["nova"])
			}
			if n := requests.Load(); n != 1 {
				t.Errorf("requests = %d, want 1", n)
			}
		})
	}
}

// TestIssueScopedTokenEndpointRegion confirms the region of the endpoint
// options selects the endpoints: a service listed only in another region is
// left out rather than failing the issue.
func TestIssueScopedTokenEndpointRegion(t *testing.T) {
	ts, _ := tokenServer(t, servicesCatalog("RegionTwo"))
	c, _ := testClient(ts)
	c.SetEndpointOpts(regionOnePublic)

	got, err := issueAsUser0001(c)
	if err != nil {
		t.Fatalf("IssueScopedToken: %v", err)
	}
	if len(got.Endpoints) != 3 {
		t.Errorf("Endpoints = %v, want three services", got.Endpoints)
	}
	if url, ok := got.Endpoints["cinder"]; ok {
		t.Errorf("Endpoints[cinder] = %q, want no cinder outside RegionOne", url)
	}
}

// TestIssueScopedTokenCatalogWithoutServices confirms a token whose catalog
// lists none of the four services, or that carries no catalog at all, issues
// successfully with an empty, non-nil Endpoints map.
func TestIssueScopedTokenCatalogWithoutServices(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no catalog", catalogToken()},
		{"identity only", catalogToken(catalogEntry("identity", "RegionOne", "https://keystone.example/v3"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := tokenServer(t, tc.body)
			c, m := testClient(ts)
			c.SetEndpointOpts(regionOnePublic)

			got, err := issueAsUser0001(c)
			if err != nil {
				t.Fatalf("IssueScopedToken: %v", err)
			}
			if got.Endpoints == nil || len(got.Endpoints) != 0 {
				t.Errorf("Endpoints = %#v, want an empty, non-nil map", got.Endpoints)
			}
			if st := statsOf(m.Aggregate(time.Second), string(KindToken)); st.Succeeded != 1 {
				t.Errorf("token samples = %+v, want 1 succeeded", st)
			}
		})
	}
}

// TestIssueScopedTokenEndpointResolutionFails confirms a resolution error
// other than a missing endpoint, here an interface gophercloud does not know,
// fails the issue with the service named and counts the token/create sample
// as failed, since the resolution runs inside it.
func TestIssueScopedTokenEndpointResolutionFails(t *testing.T) {
	ts, _ := tokenServer(t, servicesCatalog("RegionOne"))
	c, m := testClient(ts)
	c.SetEndpointOpts(gophercloud.EndpointOpts{Region: "RegionOne", Availability: "bogus"})

	got, err := issueAsUser0001(c)
	const wantPrefix = `issuing token for user "user-0001" on project pid1: resolving the nova endpoint from the token's catalog: `
	if err == nil || !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Fatalf("IssueScopedToken error = %v, want it to start with %q", err, wantPrefix)
	}
	if !errors.As(err, new(*openstack.ErrInvalidAvailabilityProvided)) {
		t.Errorf("IssueScopedToken error = %v, want it to wrap the invalid availability", err)
	}
	if got.ID != "" || got.Endpoints != nil {
		t.Errorf("IssueScopedToken = %+v, want the zero token on error", got)
	}
	if st := statsOf(m.Aggregate(time.Second), string(KindToken)); st.Attempted != 1 || st.Succeeded != 0 {
		t.Errorf("token samples = %+v, want 1 attempted and 0 succeeded", st)
	}
}

// TestErrorKind confirms the exported label is the client's own: empty for
// nil, the status for a response error, and the same for a redacted one.
func TestErrorKind(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"404", respErr(404, ""), "http_404"},
		{"401", respErr(401, ""), "http_401"},
		{"unlisted status", respErr(418, ""), "http_other"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"redacted 503", RedactToken(respErr(503, "tok"), "tok"), "http_503"},
		{"transport", errors.New("connection refused"), "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorKind(tc.err); got != tc.want {
				t.Errorf("ErrorKind(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestRedactTokenExported confirms the exported redaction replaces the token
// in the error text and keeps the response status reachable through
// gophercloud.ResponseCodeIs, which the probe adapter's callers classify by.
func TestRedactTokenExported(t *testing.T) {
	err := RedactToken(respErr(401, `{"error":{"message":"tok is not valid"}}`), "tok")
	// The placeholder itself contains "tok", so every occurrence left must be
	// one of a placeholder.
	text := err.Error()
	if !strings.Contains(text, "<token>") || strings.Count(text, "tok") != strings.Count(text, "<token>") {
		t.Errorf("redacted error %q carries the token", text)
	}
	if !gophercloud.ResponseCodeIs(err, 401) {
		t.Errorf("ResponseCodeIs(%v, 401) = false, want the status reachable", err)
	}
	if RedactToken(nil, "tok") != nil {
		t.Error("RedactToken(nil) is not nil")
	}
}

// TestClassifyOperationError pins the labels of the shared per-bucket
// classifier and the order it checks them in: a context error wins over the
// response status it wraps.
func TestClassifyOperationError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"canceled", context.Canceled, "canceled"},
		{"wrapped canceled", fmt.Errorf("op: %w", context.Canceled), "canceled"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"canceled before a transient status", fmt.Errorf("%w: %w", context.Canceled, respErr(503, "")), "canceled"},
		{"timeout before a transient status", fmt.Errorf("%w: %w", context.DeadlineExceeded, respErr(500, "")), "timeout"},
		{"403", respErr(403, "policy"), "forbidden"},
		{"wrapped forbidden", wrapCreate(KindProject, "proj-0001", respErr(403, "policy")), "forbidden"},
		{"404", respErr(404, ""), "not-found"},
		{"resource not found", gophercloud.ErrResourceNotFound{}, "not-found"},
		{"500", respErr(500, ""), "transient"},
		{"503", respErr(503, ""), "transient"},
		{"429", respErr(429, ""), "transient"},
		{"409", respErr(409, "duplicate"), "other"},
		{"401", respErr(401, ""), "other"},
		{"transport", errors.New("connection refused"), "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyOperationError(tc.err); got != tc.want {
				t.Errorf("ClassifyOperationError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestAssignmentResource confirms the synthetic assignment id for a project-
// and a domain-scoped grant, that it parses back to the same fields, and that
// AssignRole returns exactly the resource AssignmentResource builds.
func TestAssignmentResource(t *testing.T) {
	cases := []struct {
		name        string
		a           keystoneplan.Assignment
		projectID   string
		domainID    string
		wantID      string
		wantLogical string
		wantTarget  assignmentTarget
	}{
		{
			name:        "project scope",
			a:           keystoneplan.Assignment{User: "user-0001", Role: "role-0001", Project: "proj-0001"},
			projectID:   "pid1",
			domainID:    "did1",
			wantID:      "uid1:project:pid1:rid1",
			wantLogical: "user-0001->proj-0001/role-0001",
			wantTarget:  assignmentTarget{userID: "uid1", kind: "project", targetID: "pid1", roleID: "rid1"},
		},
		{
			name:        "domain scope",
			a:           keystoneplan.Assignment{User: "user-0001", Role: "role-0001"},
			domainID:    "did1",
			wantID:      "uid1:domain:did1:rid1",
			wantLogical: "user-0001->domain/role-0001",
			wantTarget:  assignmentTarget{userID: "uid1", kind: "domain", targetID: "did1", roleID: "rid1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AssignmentResource(tc.a, "uid1", tc.projectID, tc.domainID, "rid1")
			want := resource.Resource{Kind: KindAssignment, Logical: tc.wantLogical, ID: tc.wantID}
			if got != want {
				t.Errorf("AssignmentResource = %+v, want %+v", got, want)
			}
			parsed, err := parseAssignmentID(got.ID)
			if err != nil {
				t.Fatalf("parseAssignmentID(%q): %v", got.ID, err)
			}
			if parsed != tc.wantTarget {
				t.Errorf("parseAssignmentID(%q) = %+v, want %+v", got.ID, parsed, tc.wantTarget)
			}

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut {
					t.Errorf("method = %s, want PUT", r.Method)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer ts.Close()
			c, _ := testClient(ts)
			assigned, err := c.AssignRole(context.Background(), tc.a, "uid1", tc.projectID, tc.domainID, "rid1")
			if err != nil {
				t.Fatalf("AssignRole: %v", err)
			}
			if assigned != want {
				t.Errorf("AssignRole = %+v, want the AssignmentResource %+v", assigned, want)
			}
		})
	}
}

// TestRedactToken confirms the redacted error drops every occurrence of the
// token from its text and keeps the response error reachable for errors.As,
// and that a nil error or an empty token passes through unchanged.
func TestRedactToken(t *testing.T) {
	orig := respErr(404, `{"message":"Could not find token: tok-1, tok-1."}`)
	err := redactToken(orig, "tok-1")
	if strings.Contains(err.Error(), "tok-1") {
		t.Errorf("redacted error %q carries the token", err)
	}
	if !strings.Contains(err.Error(), "<token>") {
		t.Errorf("redacted error %q lacks the <token> placeholder", err)
	}
	if got := errKind(fmt.Errorf("ctx: %w", err)); got != "http_404" {
		t.Errorf("errKind(redacted) = %q, want http_404", got)
	}
	if redactToken(nil, "tok-1") != nil {
		t.Error("redactToken(nil) is not nil")
	}
	if got := redactToken(orig, ""); got.Error() != orig.Error() {
		t.Errorf("redactToken with an empty token = %v, want the original error", got)
	} else if _, wrapped := got.(*tokenRedacted); wrapped {
		t.Error("redactToken with an empty token wrapped the error, want it returned as is")
	}
}

// TestCreateImpliedRole confirms creating an implied-role rule issues one PUT
// to /roles/<prior>/implies/<implied>, records one implied_role sample, and
// returns a rule resource with the logical label and the synthetic id.
func TestCreateImpliedRole(t *testing.T) {
	var gotPath, gotMethod string
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"role_inference":{"prior_role":{"id":"rid1"},"implies":{"id":"rid2"}}}`)
	}))
	defer ts.Close()

	c, m := testClient(ts)
	got, err := c.CreateImpliedRole(context.Background(), keystoneplan.ImpliedRole{Prior: "role-0001", Implied: "role-0002"}, "rid1", "rid2")
	if err != nil {
		t.Fatalf("CreateImpliedRole: %v", err)
	}
	if calls != 1 || gotMethod != http.MethodPut {
		t.Errorf("%d call(s), method %s; want 1 PUT", calls, gotMethod)
	}
	if want := "/roles/rid1/implies/rid2"; !strings.HasSuffix(gotPath, want) {
		t.Errorf("path = %q, want it to end with %q", gotPath, want)
	}
	if want := (resource.Resource{Kind: KindImpliedRole, Logical: "role-0001->role-0002", ID: "rid1:rid2"}); got != want {
		t.Errorf("CreateImpliedRole = %+v, want %+v", got, want)
	}

	var samples int
	for _, s := range m.Aggregate(time.Second).ByType {
		if s.Type == string(KindImpliedRole) {
			samples += s.Attempted
		}
	}
	if samples != 1 {
		t.Errorf("implied_role operation samples = %d, want 1 (the create)", samples)
	}
}

// TestCreateImpliedRoleErrors confirms a 403 carries ErrForbidden, so the
// executor fails fast, and a 409 (Keystone's answer to an existing rule) names
// the rule and is not retryable.
func TestCreateImpliedRoleErrors(t *testing.T) {
	for _, tc := range []struct {
		status    int
		forbidden bool
	}{
		{http.StatusForbidden, true},
		{http.StatusConflict, false},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer ts.Close()

			c, _ := testClient(ts)
			_, err := c.CreateImpliedRole(context.Background(), keystoneplan.ImpliedRole{Prior: "role-0001", Implied: "role-0002"}, "rid1", "rid2")
			if err == nil {
				t.Fatalf("CreateImpliedRole against a %d: expected an error, got nil", tc.status)
			}
			if got := errors.Is(err, ErrForbidden); got != tc.forbidden {
				t.Errorf("errors.Is(err, ErrForbidden) = %v, want %v; err = %v", got, tc.forbidden, err)
			}
			if want := `creating implied_role "role-0001->role-0002"`; !strings.HasPrefix(err.Error(), want) {
				t.Errorf("error %q does not start with %q", err, want)
			}
			if IsRetryable(err) {
				t.Errorf("IsRetryable(%v) = true, want false", err)
			}
		})
	}
}

// TestDeleteImpliedRole confirms deleting an implied-role rule resource issues
// a DELETE to /roles/<prior>/implies/<implied> parsed from its id, and that a
// malformed id fails before any request.
func TestDeleteImpliedRole(t *testing.T) {
	var gotPath, gotMethod string
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	c, _ := testClient(ts)
	if err := c.Delete(context.Background(), resource.Resource{Kind: KindImpliedRole, ID: "rid1:rid2"}); err != nil {
		t.Fatalf("Delete(implied role): %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if want := "/roles/rid1/implies/rid2"; !strings.HasSuffix(gotPath, want) {
		t.Errorf("path = %q, want it to end with %q", gotPath, want)
	}

	err := c.Delete(context.Background(), resource.Resource{Kind: KindImpliedRole, ID: "not-an-id"})
	if err == nil || err.Error() != `malformed implied role id "not-an-id"` {
		t.Errorf("Delete with a malformed id = %v, want %q", err, `malformed implied role id "not-an-id"`)
	}
	if calls != 1 {
		t.Errorf("%d request(s), want 1: a malformed id must fail before any request", calls)
	}
}

// TestObserveImpliedRole confirms status reads an implied-role rule as
// present while Keystone has it and as gone once Keystone answers 404, and
// that a malformed id is an error, which status shows as such, not as gone.
func TestObserveImpliedRole(t *testing.T) {
	for _, tc := range []struct {
		status int
		exists bool
	}{
		{http.StatusOK, true},
		{http.StatusNotFound, false},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			var gotPath string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					_, _ = io.WriteString(w, `{"role_inference":{"prior_role":{"id":"rid1"},"implies":{"id":"rid2"}}}`)
				}
			}))
			defer ts.Close()

			c, _ := testClient(ts)
			status, exists, err := c.Observe(context.Background(), resource.Resource{Kind: KindImpliedRole, ID: "rid1:rid2"})
			if err != nil || status != "" || exists != tc.exists {
				t.Errorf("Observe = (%q, %v, %v), want (\"\", %v, nil)", status, exists, err, tc.exists)
			}
			if want := "/roles/rid1/implies/rid2"; !strings.HasSuffix(gotPath, want) {
				t.Errorf("path = %q, want it to end with %q", gotPath, want)
			}
		})
	}

	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	c, _ := testClient(ts)
	_, exists, err := c.Observe(context.Background(), resource.Resource{Kind: KindImpliedRole, ID: "not-an-id"})
	if err == nil || err.Error() != `malformed implied role id "not-an-id"` || exists {
		t.Errorf("Observe with a malformed id = (exists %v, %v), want an error %q", exists, err, `malformed implied role id "not-an-id"`)
	}
}

// TestImpliedRoleIDRoundTrip confirms parseImpliedRoleID inverts impliedRoleID
// and rejects an id that is not two non-empty parts.
func TestImpliedRoleIDRoundTrip(t *testing.T) {
	prior, implied, err := parseImpliedRoleID(impliedRoleID("rid1", "rid2"))
	if err != nil || prior != "rid1" || implied != "rid2" {
		t.Errorf("parseImpliedRoleID(impliedRoleID(rid1, rid2)) = (%q, %q, %v), want (rid1, rid2, nil)", prior, implied, err)
	}
	for _, id := range []string{"", "rid1", "rid1:", ":rid2", "rid1:rid2:rid3", "u:project:p:r"} {
		if _, _, err := parseImpliedRoleID(id); err == nil {
			t.Errorf("parseImpliedRoleID(%q): expected an error, got nil", id)
		}
	}
	if got := impliedRoleLogical(keystoneplan.ImpliedRole{Prior: "role-0001", Implied: "role-0002"}); got != "role-0001->role-0002" {
		t.Errorf("impliedRoleLogical = %q, want role-0001->role-0002", got)
	}
}
