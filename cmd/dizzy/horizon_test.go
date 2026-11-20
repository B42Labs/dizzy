package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	horizonplan "github.com/B42Labs/dizzy/internal/horizon/plan"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
)

// horizonCloudPassword is the password of the clouds.yaml user of
// fakeHorizonCloud.
const horizonCloudPassword = "cloud-pass"

// fakeHorizonCloud is a Keystone and a Horizon that share their users: the
// Keystone issues tokens to the clouds.yaml user "operator" in domain
// "Users", creates, lists and deletes users and projects, and the Horizon
// logs in every user Keystone knows with its password. Both count their
// requests.
type fakeHorizonCloud struct {
	t        *testing.T
	keystone *httptest.Server
	horizon  *httptest.Server

	mu sync.Mutex
	// roles are the role names the role listing finds.
	roles []string
	// available are the project ids GET /v3/auth/projects returns.
	available []string
	users     map[string]fakeUser    // by id
	projects  map[string]fakeProject // by id
	nextID    int
	horizonN  int
	// failPage makes every page load answer this status.
	failPage int
}

type fakeUser struct {
	name, password string
}

type fakeProject struct {
	name string
	tags []string
}

func newFakeHorizonCloud(t *testing.T) *fakeHorizonCloud {
	t.Helper()
	f := &fakeHorizonCloud{
		t:         t,
		roles:     []string{"member"},
		available: []string{"p-b", "p-a"},
		users:     map[string]fakeUser{"op-id": {name: "operator", password: horizonCloudPassword}},
		projects:  map[string]fakeProject{},
	}
	f.keystone = httptest.NewServer(http.HandlerFunc(f.serveKeystone))
	t.Cleanup(f.keystone.Close)
	f.horizon = httptest.NewServer(http.HandlerFunc(f.serveHorizon))
	t.Cleanup(f.horizon.Close)
	return f
}

// clouds points clouds.yaml discovery at an entry "fake" for the fake's
// Keystone, with auth given as unindented "key: value" lines, or the
// operator's password entry when auth is empty.
func (f *fakeHorizonCloud) clouds(auth string) {
	f.t.Helper()
	if auth == "" {
		auth = "auth_url: " + f.keystone.URL + "/v3\nusername: operator\npassword: " + horizonCloudPassword +
			"\nproject_name: p\nuser_domain_name: Users\nproject_domain_name: Users\n"
	}
	var b strings.Builder
	b.WriteString("clouds:\n  fake:\n")
	if strings.Contains(auth, "application_credential_id") {
		b.WriteString("    auth_type: v3applicationcredential\n")
	}
	b.WriteString("    auth:\n")
	for line := range strings.Lines(auth) {
		b.WriteString("      " + strings.TrimSuffix(line, "\n") + "\n")
	}
	path := filepath.Join(f.t.TempDir(), "clouds.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		f.t.Fatalf("writing clouds.yaml: %v", err)
	}
	f.t.Setenv("OS_CLOUD", "")
	f.t.Setenv("OS_CLIENT_CONFIG_FILE", path)
}

// horizonRequests returns how many requests the Horizon received.
func (f *fakeHorizonCloud) horizonRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.horizonN
}

// liveNames returns the names of the users and projects Keystone holds
// besides the operator, sorted.
func (f *fakeHorizonCloud) liveNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id, u := range f.users {
		if id != "op-id" {
			out = append(out, u.name)
		}
	}
	for _, p := range f.projects {
		out = append(out, p.name)
	}
	sort.Strings(out)
	return out
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeHorizonCloud) serveKeystone(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/v3/auth/tokens":
		catalog := fmt.Sprintf(`[{"type":"identity","endpoints":[{"interface":"public","region":"RegionOne","url":%q}]}]`, f.keystone.URL+"/v3/")
		w.Header().Set("X-Subject-Token", "tok")
		writeJSON(w, http.StatusCreated, `{"token":{"expires_at":"2030-01-01T00:00:00Z","project":{"id":"p-a"},`+
			`"user":{"id":"op-id","name":"operator","domain":{"id":"dom-id","name":"Users"}},"catalog":`+catalog+`}}`)
	case r.Method == http.MethodGet && path == "/v3/auth/projects":
		var items []string
		for _, id := range f.available {
			items = append(items, fmt.Sprintf(`{"id":%q,"enabled":true}`, id))
		}
		writeJSON(w, http.StatusOK, `{"projects":[`+strings.Join(items, ",")+`],"links":{}}`)
	case r.Method == http.MethodGet && path == "/v3/roles":
		var items []string
		for _, name := range f.roles {
			if name == r.URL.Query().Get("name") {
				items = append(items, fmt.Sprintf(`{"id":"role-%s","name":%q}`, name, name))
			}
		}
		writeJSON(w, http.StatusOK, `{"roles":[`+strings.Join(items, ",")+`],"links":{}}`)
	case r.Method == http.MethodPost && path == "/v3/projects":
		var req struct {
			Project struct {
				Name string   `json:"name"`
				Tags []string `json:"tags"`
			} `json:"project"`
		}
		_ = json.Unmarshal(body, &req)
		f.nextID++
		id := fmt.Sprintf("proj-%d", f.nextID)
		f.projects[id] = fakeProject{name: req.Project.Name, tags: req.Project.Tags}
		writeJSON(w, http.StatusCreated, fmt.Sprintf(`{"project":{"id":%q,"name":%q,"enabled":true}}`, id, req.Project.Name))
	case r.Method == http.MethodPost && path == "/v3/users":
		var req struct {
			User struct {
				Name     string `json:"name"`
				Password string `json:"password"`
			} `json:"user"`
		}
		_ = json.Unmarshal(body, &req)
		f.nextID++
		id := fmt.Sprintf("user-%d", f.nextID)
		f.users[id] = fakeUser{name: req.User.Name, password: req.User.Password}
		writeJSON(w, http.StatusCreated, fmt.Sprintf(`{"user":{"id":%q,"name":%q,"enabled":true}}`, id, req.User.Name))
	case r.Method == http.MethodPut && strings.Contains(path, "/roles/"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == "/v3/users":
		var items []string
		for id, u := range f.users {
			items = append(items, fmt.Sprintf(`{"id":%q,"name":%q}`, id, u.name))
		}
		writeJSON(w, http.StatusOK, `{"users":[`+strings.Join(items, ",")+`],"links":{}}`)
	case r.Method == http.MethodGet && path == "/v3/projects":
		var items []string
		for id, p := range f.projects {
			tags, _ := json.Marshal(p.tags)
			items = append(items, fmt.Sprintf(`{"id":%q,"name":%q,"tags":%s}`, id, p.name, tags))
		}
		writeJSON(w, http.StatusOK, `{"projects":[`+strings.Join(items, ",")+`],"links":{}}`)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v3/users/"):
		id := strings.TrimPrefix(path, "/v3/users/")
		if _, ok := f.users[id]; !ok {
			writeJSON(w, http.StatusNotFound, `{}`)
			return
		}
		delete(f.users, id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v3/projects/"):
		id := strings.TrimPrefix(path, "/v3/projects/")
		if _, ok := f.projects[id]; !ok {
			writeJSON(w, http.StatusNotFound, `{}`)
			return
		}
		delete(f.projects, id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v3/users/"):
		id := strings.TrimPrefix(path, "/v3/users/")
		if _, ok := f.users[id]; !ok {
			writeJSON(w, http.StatusNotFound, `{}`)
			return
		}
		writeJSON(w, http.StatusOK, fmt.Sprintf(`{"user":{"id":%q,"enabled":true}}`, id))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v3/projects/"):
		id := strings.TrimPrefix(path, "/v3/projects/")
		if _, ok := f.projects[id]; !ok {
			writeJSON(w, http.StatusNotFound, `{}`)
			return
		}
		writeJSON(w, http.StatusOK, fmt.Sprintf(`{"project":{"id":%q,"enabled":true}}`, id))
	default:
		writeJSON(w, http.StatusNotFound, `{}`)
	}
}

// horizonLoginForm is the login form the fake Horizon renders.
const horizonLoginForm = `<form><input type="hidden" name="csrfmiddlewaretoken" value="form-token">` +
	`<input type="hidden" name="region" value="default"></form>`

func (f *fakeHorizonCloud) serveHorizon(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.horizonN++
	failPage := f.failPage
	f.mu.Unlock()
	switch path := r.URL.Path; {
	case path == "/auth/login/" && r.Method == http.MethodGet:
		http.SetCookie(w, &http.Cookie{Name: "csrftoken", Value: "cookie-token", Path: "/"})
		_, _ = io.WriteString(w, horizonLoginForm)
	case path == "/auth/login/":
		f.mu.Lock()
		ok := false
		for _, u := range f.users {
			if u.name == r.PostForm.Get("username") && u.password == r.PostForm.Get("password") {
				ok = r.PostForm.Get("domain") == "Users"
			}
		}
		f.mu.Unlock()
		if !ok {
			_, _ = io.WriteString(w, horizonLoginForm)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "s", Path: "/"})
		http.Redirect(w, r, "/project/", http.StatusFound)
	case strings.HasPrefix(path, "/auth/switch/"):
		id := strings.Trim(strings.TrimPrefix(path, "/auth/switch/"), "/")
		http.SetCookie(w, &http.Cookie{Name: "recent_project", Value: id, Path: "/"})
		http.Redirect(w, r, r.URL.Query().Get("next"), http.StatusFound)
	case path == "/auth/logout/" && r.Method == http.MethodPost:
		http.Redirect(w, r, "/auth/login/", http.StatusFound)
	default:
		if failPage != 0 {
			w.WriteHeader(failPage)
			return
		}
		_, _ = io.WriteString(w, "<html>ok</html>")
	}
}

// repoRoot is the repository's root, resolved when the package's tests
// start in cmd/dizzy, before any test changes its working directory.
var repoRoot, _ = filepath.Abs(filepath.Join("..", ".."))

// horizonProfile returns the absolute path of a shipped Horizon profile, so a
// test can change its working directory first.
func horizonProfile(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(repoRoot, "scenarios", "horizon", name+".yaml")
}

// loadOnlyRecord loads the one run record in the working directory.
func loadOnlyRecord(t *testing.T) *run.Record {
	t.Helper()
	paths, err := filepath.Glob("run-*.json")
	if err != nil || len(paths) != 1 {
		t.Fatalf("run records = %v (%v), want exactly one", paths, err)
	}
	rec, err := run.Load(paths[0])
	if err != nil {
		t.Fatalf("loading %s: %v", paths[0], err)
	}
	return rec
}

func TestHorizonSubcommandsRegistered(t *testing.T) {
	h := findSubcommand(newRootCmd(), "horizon")
	if h == nil {
		t.Fatal("horizon command not registered on root")
	}
	for _, name := range []string{"generate", "apply", "status", "report", "cleanup"} {
		if findSubcommand(h, name) == nil {
			t.Errorf("horizon subcommand %q not registered", name)
		}
	}
}

// TestHorizonGenerateMatchesGolden asserts two generations of the small
// profile are byte-identical and equal the golden plan.
func TestHorizonGenerateMatchesGolden(t *testing.T) {
	scenario := horizonProfile(t, "small")
	first, err := execRoot(t, "horizon", "generate", "--scenario", scenario)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	second, err := execRoot(t, "horizon", "generate", "--scenario", scenario)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	golden, err := os.ReadFile(filepath.Join(repoRoot, "internal", "horizon", "scenario", "testdata", "golden", "small.plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !bytes.Equal([]byte(first), golden) {
		t.Error("generate output differs between runs or from the golden plan")
	}
}

// TestHorizonGenerateOverrides asserts --seed changes the draws and --set
// resources.users=2 assigns two users in turn on two projects.
func TestHorizonGenerateOverrides(t *testing.T) {
	scenario := horizonProfile(t, "small")
	base, _ := execRoot(t, "horizon", "generate", "--scenario", scenario)
	seeded, err := execRoot(t, "horizon", "generate", "--scenario", scenario, "--seed", "7")
	if err != nil || seeded == base {
		t.Errorf("--seed 7: %v, output unchanged %v", err, seeded == base)
	}
	out, err := execRoot(t, "horizon", "generate", "--scenario", scenario, "--set", "resources.users=2")
	if err != nil {
		t.Fatalf("generate --set resources.users=2: %v", err)
	}
	var p horizonplan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Projects) != 2 || len(p.Users) != 2 {
		t.Errorf("plan has %d projects and %d users, want 2 and 2", len(p.Projects), len(p.Users))
	}
	for i, s := range p.Sessions {
		if want := fmt.Sprintf("user-%04d", i%2+1); s.User != want {
			t.Errorf("session %s user = %q, want %q", s.Name, s.User, want)
		}
	}
}

func TestParseHorizonURL(t *testing.T) {
	for _, tc := range []struct {
		raw, want, err string
	}{
		{"https://horizon.example.com", "https://horizon.example.com", ""},
		{"https://horizon.example.com//", "https://horizon.example.com", ""},
		{"https://cloud.example.com/dashboard/", "https://cloud.example.com/dashboard", ""},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", ""},
		{"", "", "--horizon-url is required: the base URL of the Horizon dashboard, e.g. https://horizon.example.com"},
		{"ftp://x", "", "the scheme must be http or https"},
		{"horizon.example.com", "", "the scheme must be http or https"},
		{"https://", "", "names no host"},
		{"https://h/?a=b", "", "query or a fragment"},
		{"https://h/#x", "", "query or a fragment"},
	} {
		u, err := parseHorizonURL(tc.raw)
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("parseHorizonURL(%q) = %v, %v, want an error containing %q", tc.raw, u, err, tc.err)
		case tc.err == "" && (err != nil || u.String() != tc.want):
			t.Errorf("parseHorizonURL(%q) = %v, %v, want %s", tc.raw, u, err, tc.want)
		}
	}
}

// TestHorizonApplyDryRunNeedsNoCloud asserts the dry run prints the summary
// without --horizon-url, $OS_CLOUD or a clouds.yaml.
func TestHorizonApplyDryRunNeedsNoCloud(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", filepath.Join(t.TempDir(), "missing.yaml"))
	out, err := execRoot(t, "horizon", "apply", "--scenario", horizonProfile(t, "small"), "--dry-run")
	if err != nil {
		t.Fatalf("apply --dry-run: %v", err)
	}
	for _, want := range []string{`Plan for scenario "small" (seed 42)`, "identity:        the clouds.yaml user", "sessions:        5"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}

// TestHorizonApplyURLErrors asserts apply without --horizon-url and with an
// ftp URL fails with the URL's message before it reads a clouds.yaml.
func TestHorizonApplyURLErrors(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", filepath.Join(t.TempDir(), "missing.yaml"))
	scenario := horizonProfile(t, "small")
	_, err := execRoot(t, "horizon", "apply", "--scenario", scenario)
	if want := "--horizon-url is required: the base URL of the Horizon dashboard, e.g. https://horizon.example.com"; err == nil || err.Error() != want {
		t.Errorf("apply without --horizon-url = %v, want %q", err, want)
	}
	_, err = execRoot(t, "horizon", "apply", "--scenario", scenario, "--horizon-url", "ftp://x")
	if err == nil || !strings.Contains(err.Error(), "the scheme must be http or https") {
		t.Errorf("apply --horizon-url ftp://x = %v, want the scheme error", err)
	}
}

// TestConnectHorizonErrors asserts each identity problem fails before any
// Horizon request: an entry without a password, a clouds.yaml user without
// an enabled project to switch to, and a role Keystone does not have.
func TestConnectHorizonErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fakeHorizonCloud)
		args  []string
		want  string
	}{
		{"no password", func(f *fakeHorizonCloud) {
			f.clouds("auth_url: " + f.keystone.URL + "/v3\napplication_credential_id: ac\napplication_credential_secret: s\n")
		}, nil, "horizon logs in as the clouds.yaml user and the entry has no password; use a username and password entry, or set resources.users to log in as created users"},
		{"no enabled project", func(f *fakeHorizonCloud) {
			f.clouds("")
			f.available = nil
		}, nil, "the clouds.yaml user has no enabled project to switch to"},
		{"unknown role", func(f *fakeHorizonCloud) {
			f.clouds("")
			f.roles = nil
		}, []string{"--set", "resources.users=2"}, `role "member" not found in Keystone; set role in the scenario`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			f := newFakeHorizonCloud(t)
			tc.setup(f)
			args := append([]string{"horizon", "apply", "--os-cloud", "fake", "--scenario", horizonProfile(t, "small"),
				"--horizon-url", f.horizon.URL}, tc.args...)
			_, err := execRoot(t, args...)
			if err == nil || err.Error() != tc.want {
				t.Errorf("apply = %v, want %q", err, tc.want)
			}
			if n := f.horizonRequests(); n != 0 {
				t.Errorf("Horizon received %d request(s), want none", n)
			}
			if f.liveNames() != nil {
				t.Errorf("Keystone holds %v, want nothing created", f.liveNames())
			}
		})
	}
}

// TestHorizonApplyAsCloudUser asserts apply of the small profile against
// the fake exits 0, prints the metrics, and writes a record of service
// horizon with an empty created list and a byType entry per step.
func TestHorizonApplyAsCloudUser(t *testing.T) {
	t.Chdir(t.TempDir())
	f := newFakeHorizonCloud(t)
	f.clouds("")
	out, err := execRoot(t, "horizon", "apply", "--os-cloud", "fake", "--scenario", horizonProfile(t, "small"), "--horizon-url", f.horizon.URL)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if !strings.Contains(out, "run record written to") {
		t.Errorf("output lacks the record line:\n%s", out)
	}
	rec := loadOnlyRecord(t)
	if rec.Service != "horizon" || rec.Created == nil || len(rec.Created) != 0 || rec.Error != "" {
		t.Errorf("record service %q, created %v, error %q", rec.Service, rec.Created, rec.Error)
	}
	types := map[string]int{}
	for _, s := range rec.Metrics.ByType {
		types[s.Type] = s.Succeeded
	}
	for _, kind := range []string{"login_page", "login", "page_api_access", "page_identity", "switch", "switch_landing", "logout"} {
		if types[kind] == 0 {
			t.Errorf("record has no successful %s samples: %v", kind, types)
		}
	}
	data, _ := os.ReadFile(filepath.Join(".", "run-"+rec.RunID+".json"))
	if strings.Contains(string(data), horizonCloudPassword) || strings.Contains(string(data), "cookie-token") {
		t.Error("the run record carries the password or a cookie")
	}
}

// TestHorizonApplyFailedPageExitsNonZero asserts a failed check does not
// stop the run but makes the command fail, with the record written.
func TestHorizonApplyFailedPageExitsNonZero(t *testing.T) {
	t.Chdir(t.TempDir())
	f := newFakeHorizonCloud(t)
	f.clouds("")
	f.failPage = http.StatusNotFound
	_, err := execRoot(t, "horizon", "apply", "--os-cloud", "fake", "--scenario", horizonProfile(t, "small"), "--horizon-url", f.horizon.URL)
	if err == nil || !strings.Contains(err.Error(), "5 of 5 journeys failed") {
		t.Fatalf("apply = %v, want 5 of 5 journeys failed", err)
	}
	rec := loadOnlyRecord(t)
	if st := findStats(rec, "logout"); st.Succeeded != 5 {
		t.Errorf("logout samples = %+v, want every session logged out", st)
	}
}

func findStats(rec *run.Record, kind string) (out struct{ Attempted, Succeeded, Failed int }) {
	for _, s := range rec.Metrics.ByType {
		if s.Type == kind {
			out.Attempted, out.Succeeded, out.Failed = s.Attempted, s.Succeeded, s.Failed
		}
	}
	return out
}

// TestHorizonCreatedUsersStatusAndCleanup asserts apply with created users
// records them, status shows them enabled, cleanup deletes exactly them, and
// status then shows them gone.
func TestHorizonCreatedUsersStatusAndCleanup(t *testing.T) {
	t.Chdir(t.TempDir())
	f := newFakeHorizonCloud(t)
	f.clouds("")
	if out, err := execRoot(t, "horizon", "apply", "--os-cloud", "fake", "--scenario", horizonProfile(t, "small"),
		"--horizon-url", f.horizon.URL, "--set", "resources.users=2"); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	rec := loadOnlyRecord(t)
	recordPath := "run-" + rec.RunID + ".json"
	var kinds []string
	for _, r := range rec.Created {
		kinds = append(kinds, string(r.Kind)+":"+r.Logical)
		if !strings.HasPrefix(r.Name, "dizzyhorizon-"+rec.RunID+"-") {
			t.Errorf("created %s is not named for the run", r.Name)
		}
	}
	if want := "project:project-0001,project:project-0002,user:user-0001,user:user-0002"; strings.Join(kinds, ",") != want {
		t.Errorf("created = %v, want %s", kinds, want)
	}

	out, err := execRoot(t, "horizon", "status", "--os-cloud", "fake", "--run", recordPath)
	if err != nil || strings.Count(out, "enabled") != 4 {
		t.Errorf("status = %v:\n%s\nwant four enabled rows", err, out)
	}
	out, err = execRoot(t, "horizon", "cleanup", "--os-cloud", "fake", "--run", recordPath)
	if want := fmt.Sprintf("deleted 4 resource(s) for run %s\n", rec.RunID); err != nil || out != want {
		t.Errorf("cleanup = %q, %v, want %q", out, err, want)
	}
	if names := f.liveNames(); len(names) != 0 {
		t.Errorf("Keystone still holds %v", names)
	}
	out, err = execRoot(t, "horizon", "status", "--os-cloud", "fake", "--run", recordPath)
	if err != nil || strings.Count(out, "gone") != 4 {
		t.Errorf("status after cleanup = %v:\n%s\nwant four gone rows", err, out)
	}
	if out, err := execRoot(t, "horizon", "cleanup", "--os-cloud", "fake", "--run", recordPath); err != nil || !strings.HasPrefix(out, "deleted 0 ") {
		t.Errorf("second cleanup = %q, %v, want deleted 0", out, err)
	}
}

// TestHorizonStatusEmptyCreated asserts status of a record without created
// resources prints the header alone and needs no clouds.yaml.
func TestHorizonStatusEmptyCreated(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", filepath.Join(t.TempDir(), "missing.yaml"))
	path, err := run.Write(t.TempDir(), &run.Record{RunID: "hz000001", Service: "horizon", Created: []resource.Resource{}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := execRoot(t, "horizon", "status", "--run", path)
	if err != nil || strings.TrimSpace(out) != "LOGICAL  KIND  ID  STATE" {
		t.Errorf("status = %q, %v, want the header alone", out, err)
	}
}

// TestHorizonRecordGuards asserts the keystone commands reject a horizon
// record and the horizon commands a keystone one.
func TestHorizonRecordGuards(t *testing.T) {
	dir := t.TempDir()
	hz, err := run.Write(dir, &run.Record{RunID: "hz000001", Service: "horizon"})
	if err != nil {
		t.Fatal(err)
	}
	ks, err := run.Write(dir, &run.Record{RunID: "ks000001", Service: "keystone"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = execRoot(t, "keystone", "cleanup", "--run", hz)
	if want := `run record is for service "horizon", not "keystone"`; err == nil || err.Error() != want {
		t.Errorf("keystone cleanup of a horizon record = %v, want %q", err, want)
	}
	for _, verb := range []string{"status", "cleanup"} {
		if _, err := execRoot(t, "horizon", verb, "--run", ks); err == nil || !strings.Contains(err.Error(), `not "horizon"`) {
			t.Errorf("horizon %s of a keystone record = %v, want the service guard", verb, err)
		}
	}
}

// TestFinishAbortedHorizonApply asserts an interrupted apply with created
// users deletes them, or with --keep-on-abort prints the cleanup hint, and
// that one without users sends nothing.
func TestFinishAbortedHorizonApply(t *testing.T) {
	cause := fmt.Errorf("interrupted")
	withUsers := &horizonplan.Plan{Users: []horizonplan.User{{Name: "user-0001"}}}
	record := func() (string, error) { return "run-r1.json", nil }

	c := &recordingHorizonCleaner{}
	var out bytes.Buffer
	err := finishAbortedHorizonApply(t.Context(), &out, c, withUsers, "r1", []resource.Resource{{Kind: "user", Name: "dizzyhorizon-r1-user-0001", ID: "u1"}}, false, cause, 2, time.Second, record)
	if err == nil || !strings.Contains(err.Error(), "1 created resource(s) torn down") || !strings.Contains(out.String(), "deleted 1 resource(s) for run r1") {
		t.Errorf("teardown = %v, output %q", err, out.String())
	}

	out.Reset()
	c = &recordingHorizonCleaner{}
	err = finishAbortedHorizonApply(t.Context(), &out, c, withUsers, "r1", nil, true, cause, 2, time.Second, record)
	if err == nil || !strings.Contains(out.String(), "reclaim with: horizon cleanup --run run-r1.json") || c.calls != 0 {
		t.Errorf("keep-on-abort = %v, output %q, %d call(s)", err, out.String(), c.calls)
	}

	out.Reset()
	c = &recordingHorizonCleaner{}
	err = finishAbortedHorizonApply(t.Context(), &out, c, &horizonplan.Plan{}, "r1", nil, false, cause, 2, time.Second, record)
	if err == nil || c.calls != 0 || !strings.Contains(out.String(), "deleted 0 resource(s)") {
		t.Errorf("without users = %v, output %q, %d call(s)", err, out.String(), c.calls)
	}
}

// recordingHorizonCleaner deletes whatever it is handed and counts calls.
type recordingHorizonCleaner struct{ calls int }

func (c *recordingHorizonCleaner) ListUsersByRun(context.Context, string) ([]resource.Resource, error) {
	c.calls++
	return nil, nil
}

func (c *recordingHorizonCleaner) ListProjectsByRun(context.Context, string) ([]resource.Resource, error) {
	c.calls++
	return nil, nil
}

func (c *recordingHorizonCleaner) Delete(context.Context, resource.Resource) error {
	c.calls++
	return nil
}
