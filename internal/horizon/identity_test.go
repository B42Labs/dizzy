package horizon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/horizon/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeKeystone answers the identity calls a Horizon run makes. status, when
// set for "METHOD /path", replaces the answer of that call with a bare
// status. users and projects are what the listings return, as JSON objects;
// the project listing ignores its tags filter, as a server that does not
// honor it would.
type fakeKeystone struct {
	srv *httptest.Server

	mu       sync.Mutex
	status   map[string]int
	users    []string
	projects []string
	bodies   map[string]string
	calls    []string
}

func newFakeKeystone(t *testing.T) *fakeKeystone {
	t.Helper()
	k := &fakeKeystone{status: map[string]int{}, bodies: map[string]string{}}
	k.srv = httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(k.srv.Close)
	return k
}

func (k *fakeKeystone) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.Path
	k.mu.Lock()
	k.calls = append(k.calls, key+"?"+r.URL.RawQuery)
	k.bodies[key] = string(body)
	st, override := k.status[key]
	users, projects := k.users, k.projects
	k.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if override {
		w.WriteHeader(st)
		return
	}
	switch {
	case key == "POST /v3/projects":
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"project":{"id":"pid","name":"x","enabled":true}}`)
	case key == "POST /v3/users":
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"user":{"id":"uid","name":"x","enabled":true}}`)
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/roles/"):
		w.WriteHeader(http.StatusNoContent)
	case key == "GET /v3/users":
		_, _ = fmt.Fprintf(w, `{"users":[%s],"links":{}}`, strings.Join(users, ","))
	case key == "GET /v3/projects":
		_, _ = fmt.Fprintf(w, `{"projects":[%s],"links":{}}`, strings.Join(projects, ","))
	case key == "GET /v3/auth/projects":
		_, _ = fmt.Fprint(w, `{"projects":[{"id":"p9","enabled":true},{"id":"p1","enabled":true},{"id":"p5","enabled":false}],"links":{}}`)
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(key, "GET /v3/users/"):
		_, _ = fmt.Fprint(w, `{"user":{"id":"uid","enabled":true}}`)
	case strings.HasPrefix(key, "GET /v3/projects/"):
		_, _ = fmt.Fprint(w, `{"project":{"id":"pid","enabled":false}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// set makes "METHOD /path" answer status.
func (k *fakeKeystone) set(key string, status int) {
	k.mu.Lock()
	k.status[key] = status
	k.mu.Unlock()
}

// body returns the last body "METHOD /path" received.
func (k *fakeKeystone) body(key string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.bodies[key]
}

// count returns how many requests matched "METHOD /path".
func (k *fakeKeystone) count(key string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	var n int
	for _, c := range k.calls {
		if strings.HasPrefix(c, key+"?") {
			n++
		}
	}
	return n
}

// identityClient returns a Horizon client for run run1 whose identity calls
// hit k.
func (k *fakeKeystone) identityClient(t *testing.T) (*Client, *metrics.Collector) {
	t.Helper()
	base, _ := url.Parse("http://horizon.invalid")
	m := metrics.NewCollector()
	c := New(base, nil, "run1", m)
	c.SetIdentity(&gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: k.srv.URL + "/v3/"})
	return c, m
}

// TestCreateNamesAndTags asserts created users and projects are named
// dizzyhorizon-<runid>-<logical> and projects carry both tags.
func TestCreateNamesAndTags(t *testing.T) {
	k := newFakeKeystone(t)
	c, m := k.identityClient(t)
	ctx := t.Context()

	pr, err := c.CreateProject(ctx, plan.Project{Name: "project-0001"}, "did")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if want := (resource.Resource{Kind: KindProject, Logical: "project-0001", Name: "dizzyhorizon-run1-project-0001", ID: "pid"}); pr != want {
		t.Errorf("project = %+v, want %+v", pr, want)
	}
	var sent struct {
		Project struct {
			Name     string   `json:"name"`
			DomainID string   `json:"domain_id"`
			Enabled  bool     `json:"enabled"`
			Tags     []string `json:"tags"`
		} `json:"project"`
	}
	if err := json.Unmarshal([]byte(k.body("POST /v3/projects")), &sent); err != nil {
		t.Fatalf("decoding the project create: %v", err)
	}
	if p := sent.Project; p.Name != pr.Name || p.DomainID != "did" || !p.Enabled ||
		strings.Join(p.Tags, ",") != "dizzyhorizon:run=run1,dizzyhorizon:type=project" {
		t.Errorf("project create sent %+v", p)
	}

	u, err := c.CreateUser(ctx, plan.User{Name: "user-0001"}, "did", "pw")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.Name != "dizzyhorizon-run1-user-0001" || u.Kind != KindUser || u.ID != "uid" {
		t.Errorf("user = %+v", u)
	}
	if err := c.AssignRole(ctx, "uid", "pid", "rid"); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if k.count("PUT /v3/projects/pid/users/uid/roles/rid") != 1 {
		t.Error("the role assignment was not sent to the project, user and role")
	}
	for _, kind := range []string{"project", "user", "role_assignment"} {
		if st, ok := stats(m, kind); !ok || st.Succeeded != 1 {
			t.Errorf("kind %s: %+v, want one successful sample", kind, st)
		}
	}
	if !HasNamePrefix(u.Name) || HasNamePrefix("dizzy-run1-user-0001") {
		t.Error("HasNamePrefix does not tell dizzyhorizon- from dizzy-")
	}
}

// TestCreateForbidden asserts a 403 on each create wraps ErrForbidden with
// the hint and is not retryable.
func TestCreateForbidden(t *testing.T) {
	k := newFakeKeystone(t)
	for _, key := range []string{"POST /v3/projects", "POST /v3/users", "PUT /v3/projects/pid/users/uid/roles/rid"} {
		k.set(key, http.StatusForbidden)
	}
	c, _ := k.identityClient(t)
	ctx := t.Context()
	_, perr := c.CreateProject(ctx, plan.Project{Name: "project-0001"}, "did")
	_, uerr := c.CreateUser(ctx, plan.User{Name: "user-0001"}, "did", "pw")
	aerr := c.AssignRole(ctx, "uid", "pid", "rid")
	for _, err := range []error{perr, uerr, aerr} {
		if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), forbiddenHint) {
			t.Errorf("error = %v, want ErrForbidden with the hint", err)
		}
		if IsRetryable(err) {
			t.Errorf("IsRetryable(%v) = true, want false", err)
		}
		if ErrorKind(err) != "http_403" {
			t.Errorf("ErrorKind(%v) = %q, want http_403", err, ErrorKind(err))
		}
	}
}

// TestListingsKeepOnlyHorizonResources asserts the listings ignore a user
// named dizzy-<runid>-x and a project tagged dizzy:type=project, keep the
// run's own, and the any-run listings keep every dizzyhorizon- one.
func TestListingsKeepOnlyHorizonResources(t *testing.T) {
	k := newFakeKeystone(t)
	k.users = []string{
		`{"id":"u1","name":"dizzyhorizon-run1-user-0001"}`,
		`{"id":"u2","name":"dizzyhorizon-run2-user-0001"}`,
		`{"id":"u3","name":"dizzy-run1-x"}`,
		`{"id":"u4","name":"alice"}`,
	}
	k.projects = []string{
		`{"id":"p1","name":"dizzyhorizon-run1-project-0001","tags":["dizzyhorizon:run=run1","dizzyhorizon:type=project"]}`,
		`{"id":"p2","name":"dizzyhorizon-run2-project-0001","tags":["dizzyhorizon:run=run2","dizzyhorizon:type=project"]}`,
		`{"id":"p3","name":"dizzy-run1-project-0001","tags":["dizzy:run=run1","dizzy:type=project"]}`,
		`{"id":"p4","name":"other","tags":["dizzyhorizon:run=run1","dizzyhorizon:type=project"]}`,
	}
	c, _ := k.identityClient(t)
	ctx := t.Context()

	ids := func(rs []resource.Resource, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("listing: %v", err)
		}
		var out []string
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(c.ListUsersByRun(ctx, "run1")); got != "u1" {
		t.Errorf("ListUsersByRun = %s, want u1", got)
	}
	if got := ids(c.ListUsersByAnyRun(ctx)); got != "u1,u2" {
		t.Errorf("ListUsersByAnyRun = %s, want u1,u2", got)
	}
	if got := ids(c.ListProjectsByRun(ctx, "run1")); got != "p1" {
		t.Errorf("ListProjectsByRun = %s, want p1", got)
	}
	if got := ids(c.ListProjectsByAnyRun(ctx)); got != "p1,p2" {
		t.Errorf("ListProjectsByAnyRun = %s, want p1,p2", got)
	}
}

// TestListingsEmptyAndForbidden asserts an empty listing returns nothing and
// a 403 returns nothing without an error.
func TestListingsEmptyAndForbidden(t *testing.T) {
	k := newFakeKeystone(t)
	c, _ := k.identityClient(t)
	ctx := t.Context()
	for name, list := range map[string]func() ([]resource.Resource, error){
		"users":    func() ([]resource.Resource, error) { return c.ListUsersByRun(ctx, "run1") },
		"projects": func() ([]resource.Resource, error) { return c.ListProjectsByRun(ctx, "run1") },
	} {
		if got, err := list(); err != nil || len(got) != 0 {
			t.Errorf("empty %s listing = %v, %v, want nothing", name, got, err)
		}
	}
	k.set("GET /v3/users", http.StatusForbidden)
	k.set("GET /v3/projects", http.StatusForbidden)
	if got, err := c.ListUsersByAnyRun(ctx); err != nil || got != nil {
		t.Errorf("forbidden user listing = %v, %v, want nil, nil", got, err)
	}
	if got, err := c.ListProjectsByAnyRun(ctx); err != nil || got != nil {
		t.Errorf("forbidden project listing = %v, %v, want nil, nil", got, err)
	}
	k.set("GET /v3/users", http.StatusInternalServerError)
	if _, err := c.ListUsersByRun(ctx, "run1"); err == nil {
		t.Error("a 500 on the user listing returned no error")
	}
}

// TestProjectListingFiltersByTag asserts the listings ask Keystone for the
// run tag or the type tag.
func TestProjectListingFiltersByTag(t *testing.T) {
	k := newFakeKeystone(t)
	c, _ := k.identityClient(t)
	_, _ = c.ListProjectsByRun(t.Context(), "run1")
	_, _ = c.ListProjectsByAnyRun(t.Context())
	k.mu.Lock()
	defer k.mu.Unlock()
	want := []string{
		"GET /v3/projects?limit=500&tags=dizzyhorizon%3Arun%3Drun1",
		"GET /v3/projects?limit=500&tags=dizzyhorizon%3Atype%3Dproject",
	}
	if strings.Join(k.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls = %v, want %v", k.calls, want)
	}
}

// TestListingsPageWithLimit asserts the user and project listings ask for
// listPageSize entries per page, follow links.next to a second page, and
// return the run's resources from both pages.
func TestListingsPageWithLimit(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		second := r.URL.Query().Has("marker")
		switch {
		case r.URL.Path == "/v3/users" && !second:
			_, _ = fmt.Fprintf(w, `{"users":[{"id":"u1","name":"dizzyhorizon-run1-user-0001"},{"id":"u-foreign","name":"alice"}],"links":{"next":%q}}`,
				srv.URL+"/v3/users?limit=500&marker=u-foreign")
		case r.URL.Path == "/v3/users":
			_, _ = fmt.Fprint(w, `{"users":[{"id":"u2","name":"dizzyhorizon-run1-user-0002"}],"links":{"next":null}}`)
		case r.URL.Path == "/v3/projects" && !second:
			_, _ = fmt.Fprintf(w, `{"projects":[{"id":"p1","name":"dizzyhorizon-run1-project-0001","tags":["dizzyhorizon:run=run1","dizzyhorizon:type=project"]}],"links":{"next":%q}}`,
				srv.URL+"/v3/projects?limit=500&marker=p1&tags=dizzyhorizon%3Arun%3Drun1")
		case r.URL.Path == "/v3/projects":
			_, _ = fmt.Fprint(w, `{"projects":[{"id":"p2","name":"dizzyhorizon-run1-project-0002","tags":["dizzyhorizon:run=run1","dizzyhorizon:type=project"]}],"links":{"next":null}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	base, _ := url.Parse("http://horizon.invalid")
	c := New(base, nil, "run1", metrics.NewCollector())
	c.SetIdentity(&gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: srv.URL + "/v3/"})
	ctx := t.Context()

	for _, tc := range []struct {
		name, path, want string
		list             func() ([]resource.Resource, error)
	}{
		{"users", "/v3/users", "u1,u2", func() ([]resource.Resource, error) { return c.ListUsersByRun(ctx, "run1") }},
		{"projects", "/v3/projects", "p1,p2", func() ([]resource.Resource, error) { return c.ListProjectsByRun(ctx, "run1") }},
	} {
		got, err := tc.list()
		if err != nil {
			t.Fatalf("%s listing: %v", tc.name, err)
		}
		var ids []string
		for _, r := range got {
			ids = append(ids, r.ID)
		}
		if strings.Join(ids, ",") != tc.want {
			t.Errorf("%s listing = %v, want %s", tc.name, ids, tc.want)
		}
		mu.Lock()
		var requests []string
		for _, call := range calls {
			if strings.HasPrefix(call, tc.path+"?") {
				requests = append(requests, call)
			}
		}
		mu.Unlock()
		if len(requests) != 2 {
			t.Fatalf("%s requests = %v, want 2", tc.name, requests)
		}
		q, _ := url.ParseQuery(strings.TrimPrefix(requests[0], tc.path+"?"))
		if q.Get("limit") != "500" || q.Has("marker") {
			t.Errorf("first %s request = %s, want limit=500 and no marker", tc.name, requests[0])
		}
	}
}

// TestDeleteAndObserve asserts Delete addresses users and projects by id,
// passes a 404 through, refuses another kind without a request, and
// Observe reports enabled, disabled and gone.
func TestDeleteAndObserve(t *testing.T) {
	k := newFakeKeystone(t)
	c, _ := k.identityClient(t)
	ctx := t.Context()
	user := resource.Resource{Kind: KindUser, ID: "uid"}
	project := resource.Resource{Kind: KindProject, ID: "pid"}

	if err := c.Delete(ctx, user); err != nil {
		t.Errorf("Delete(user) = %v", err)
	}
	if err := c.Delete(ctx, project); err != nil {
		t.Errorf("Delete(project) = %v", err)
	}
	if k.count("DELETE /v3/users/uid") != 1 || k.count("DELETE /v3/projects/pid") != 1 {
		t.Error("Delete did not address the user and the project by id")
	}
	k.set("DELETE /v3/users/uid", http.StatusNotFound)
	if err := c.Delete(ctx, user); !IsNotFound(err) {
		t.Errorf("Delete of a gone user = %v, want a 404", err)
	}
	before := len(k.calls)
	if err := c.Delete(ctx, resource.Resource{Kind: "session", ID: "x"}); err == nil || err.Error() != `unknown horizon resource kind "session"` {
		t.Errorf("Delete(session) = %v, want the unknown-kind error", err)
	}
	if len(k.calls) != before {
		t.Error("Delete of an unknown kind sent a request")
	}

	if st, ok, err := c.Observe(ctx, user); st != "enabled" || !ok || err != nil {
		t.Errorf("Observe(user) = %q, %v, %v, want enabled", st, ok, err)
	}
	if st, ok, err := c.Observe(ctx, project); st != "disabled" || !ok || err != nil {
		t.Errorf("Observe(project) = %q, %v, %v, want disabled", st, ok, err)
	}
	k.set("GET /v3/users/uid", http.StatusNotFound)
	if st, ok, err := c.Observe(ctx, user); st != "" || ok || err != nil {
		t.Errorf("Observe(gone user) = %q, %v, %v, want gone", st, ok, err)
	}
}

// TestAvailableProjects asserts the enabled projects come back sorted and
// the listing records no sample.
func TestAvailableProjects(t *testing.T) {
	k := newFakeKeystone(t)
	c, m := k.identityClient(t)
	ids, err := c.AvailableProjects(t.Context())
	if err != nil || strings.Join(ids, ",") != "p1,p9" {
		t.Errorf("AvailableProjects = %v, %v, want p1,p9", ids, err)
	}
	if n := m.Aggregate(0).Overall.Attempted; n != 0 {
		t.Errorf("recorded %d sample(s), want none", n)
	}
}
