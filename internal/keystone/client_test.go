package keystone

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
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
	if got := resourceName("abcd1234", "proj-0001"); got != "ostester-abcd1234-proj-0001" {
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
		_, _ = io.WriteString(w, `{"project":{"id":"pid1","name":"ostester-run0-proj-0001"}}`)
	}))
	defer ts.Close()

	c, _ := testClient(ts)
	res, err := c.CreateProject(context.Background(), keystoneplan.Project{Name: "proj-0001", Domain: "dom-0001"}, "did1")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if res.ID != "pid1" || res.Name != "ostester-run0-proj-0001" {
		t.Errorf("resource = %+v, want id pid1 and the prefixed name", res)
	}

	project, _ := body["project"].(map[string]any)
	tags, _ := project["tags"].([]any)
	want := map[string]bool{"ostester:run=run0": false, "ostester:type=project": false}
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
			{"id":"u-mine","name":"ostester-run0-user-0001"},
			{"id":"u-theirs","name":"ostester-run9-user-0001"},
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
	if name, _ := user["name"].(string); name != "ostester-run0-user-0001" {
		t.Errorf("auth username = %q, want the prefixed user name", name)
	}
	scope, _ := auth["scope"].(map[string]any)
	project, _ := scope["project"].(map[string]any)
	if id, _ := project["id"].(string); id != "pid1" {
		t.Errorf("auth scope project id = %q, want pid1", id)
	}
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
		_, _ = io.WriteString(w, `{"domain":{"id":"did1","name":"ostester-run0-dom-0001","enabled":false}}`)
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
