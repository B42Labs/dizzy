package horizon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/horizon/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
)

// Fixed values the fake Horizon hands out, so a test can look for them in
// error texts.
const (
	fakePassword   = "pa55-word-secret"
	fakeCSRF       = "csrf-form-token-value"
	fakeCSRFCookie = "csrf-cookie-value"
	fakeSessionID  = "session-cookie-value"
	fakeRegion     = "https://keystone.example/v3"
)

// hiddenRegionForm is Horizon's login form with one region, rendered hidden.
var hiddenRegionForm = `<html><body><form method="POST">
<input type="hidden" name="csrfmiddlewaretoken" value="` + fakeCSRF + `">
<input type="hidden" name="region" value="` + fakeRegion + `" id="id_region">
<input type="text" name="username"><input type="password" name="password">
</form></body></html>`

// selectRegionForm is Horizon's login form with two regions, the second
// selected.
var selectRegionForm = `<html><body><form method="POST">
<input type="hidden" name="csrfmiddlewaretoken" value="` + fakeCSRF + `">
<select name="region" id="id_region"><option value="https://one.example/v3">one</option>
<option value="https://two.example/v3" selected>two</option></select>
<input type="text" name="username"><input type="password" name="password">
</form></body></html>`

// seen is one request the fake received.
type seen struct {
	method  string
	path    string
	query   url.Values
	form    url.Values
	header  http.Header
	cookies map[string]string
}

// fakeHorizon is an httptest Horizon below prefix that answers like
// openstack_auth on stable/2026.1 unless a test overrides a handler. It logs
// in user "alice" with fakePassword.
type fakeHorizon struct {
	t      *testing.T
	srv    *httptest.Server
	prefix string

	// loginPage, loginPost, switchView, logout and page answer the views;
	// each defaults to Horizon's behavior and a test may replace one.
	loginPage  http.HandlerFunc
	loginPost  http.HandlerFunc
	switchView http.HandlerFunc
	logout     http.HandlerFunc
	page       http.HandlerFunc

	mu       sync.Mutex
	requests []seen
}

// newFakeHorizon starts a fake Horizon whose WEBROOT is prefix ("" or e.g.
// "/dashboard").
func newFakeHorizon(t *testing.T, prefix string) *fakeHorizon {
	t.Helper()
	f := &fakeHorizon{t: t, prefix: prefix}
	f.loginPage = func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "csrftoken", Value: fakeCSRFCookie, Path: "/"})
		_, _ = fmt.Fprint(w, hiddenRegionForm)
	}
	f.loginPost = func(w http.ResponseWriter, r *http.Request) {
		if r.PostForm.Get("username") != "alice" || r.PostForm.Get("password") != fakePassword {
			_, _ = fmt.Fprint(w, hiddenRegionForm)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: fakeSessionID, Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "csrftoken", Value: fakeCSRFCookie, Path: "/"})
		http.Redirect(w, r, f.prefix+"/project/", http.StatusFound)
	}
	f.switchView = func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, f.prefix+"/auth/switch/"), "/")
		http.SetCookie(w, &http.Cookie{Name: "recent_project", Value: id, Path: "/"})
		http.Redirect(w, r, r.URL.Query().Get("next"), http.StatusFound)
	}
	f.logout = func(w http.ResponseWriter, r *http.Request) {
		if r.PostForm.Get("csrfmiddlewaretoken") != fakeCSRFCookie {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		http.Redirect(w, r, f.prefix+"/auth/login/", http.StatusFound)
	}
	f.page = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body>page</body></html>")
	}

	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// serve records the request and routes it to the view it addresses.
func (f *fakeHorizon) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	cookies := map[string]string{}
	for _, c := range r.Cookies() {
		cookies[c.Name] = c.Value
	}
	f.mu.Lock()
	f.requests = append(f.requests, seen{
		method: r.Method, path: r.URL.Path, query: r.URL.Query(), form: r.PostForm,
		header: r.Header.Clone(), cookies: cookies,
	})
	f.mu.Unlock()

	path, ok := strings.CutPrefix(r.URL.Path, f.prefix)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch {
	case path == "/auth/login/" && r.Method == http.MethodGet:
		f.loginPage(w, r)
	case path == "/auth/login/" && r.Method == http.MethodPost:
		f.loginPost(w, r)
	case strings.HasPrefix(path, "/auth/switch/"):
		f.switchView(w, r)
	case path == "/auth/logout/" && r.Method == http.MethodPost:
		f.logout(w, r)
	case path == "/auth/logout/":
		w.WriteHeader(http.StatusMethodNotAllowed)
	default:
		f.page(w, r)
	}
}

// seenRequests returns a copy of every request so far.
func (f *fakeHorizon) seenRequests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.requests...)
}

// posts counts the POST requests so far.
func (f *fakeHorizon) posts() int {
	var n int
	for _, r := range f.seenRequests() {
		if r.method == http.MethodPost {
			n++
		}
	}
	return n
}

// client returns a Client for the fake and its collector.
func (f *fakeHorizon) client() (*Client, *metrics.Collector) {
	f.t.Helper()
	base, err := url.Parse(f.srv.URL + f.prefix)
	if err != nil {
		f.t.Fatalf("parsing base URL: %v", err)
	}
	m := metrics.NewCollector()
	return New(base, nil, "run1", m), m
}

// alice are the credentials the fake accepts.
var alice = Credentials{Username: "alice", Password: fakePassword, Domain: "Default"}

// stats returns the collector's stats of kind, and false when it has none.
func stats(m *metrics.Collector, kind string) (metrics.Stats, bool) {
	for _, s := range m.Aggregate(0).ByType {
		if s.Type == kind {
			return s, true
		}
	}
	return metrics.Stats{}, false
}

// errorCount returns how many samples carried the error kind ek.
func errorCount(m *metrics.Collector, ek string) int {
	for _, e := range m.Aggregate(0).Errors {
		if e.Kind == ek {
			return e.Count
		}
	}
	return 0
}

// TestJourneyRecordsOneSampleEach asserts a login, two pages, a switch and a
// logout record one successful sample of each step's kind and one session
// time-to-ready sample, and leave the session logged out.
func TestJourneyRecordsOneSampleEach(t *testing.T) {
	f := newFakeHorizon(t, "")
	c, m := f.client()
	s := c.NewSession()
	defer s.Close()
	ctx := t.Context()

	if err := s.Login(ctx, alice); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !s.LoggedIn() {
		t.Fatal("LoggedIn() = false after a login")
	}
	api := plan.Page{Name: "api_access", Path: "/project/api_access/"}
	identity := plan.Page{Name: "identity", Path: "/identity/"}
	for _, p := range []plan.Page{api, identity} {
		if err := s.Page(ctx, p); err != nil {
			t.Fatalf("Page(%s): %v", p.Name, err)
		}
	}
	if err := s.Switch(ctx, "p2", api); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if err := s.Logout(ctx); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if s.LoggedIn() {
		t.Error("LoggedIn() = true after a logout")
	}

	for _, kind := range []string{"login_page", "login", "page_api_access", "page_identity", "switch", "switch_landing", "logout"} {
		st, ok := stats(m, kind)
		if !ok || st.Attempted != 1 || st.Succeeded != 1 {
			t.Errorf("kind %s: %+v (found %v), want one successful sample", kind, st, ok)
		}
	}
	agg := m.Aggregate(0)
	if agg.Overall.Attempted != 7 {
		t.Errorf("samples = %d, want 7", agg.Overall.Attempted)
	}
	if len(agg.Readiness) != 1 || agg.Readiness[0].Type != "session" || agg.Readiness[0].Count != 1 || agg.Readiness[0].OK != 1 {
		t.Errorf("readiness = %+v, want one successful session sample", agg.Readiness)
	}
}

// TestLoginPostCarriesFormAndHeaders asserts the POST carries the form's
// token and region, the domain, a Referer and the User-Agent, for a hidden
// region input and for a region select.
func TestLoginPostCarriesFormAndHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, form, region string
	}{
		{"hidden region", hiddenRegionForm, fakeRegion},
		{"region select", selectRegionForm, "https://two.example/v3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHorizon(t, "")
			f.loginPage = func(w http.ResponseWriter, _ *http.Request) {
				http.SetCookie(w, &http.Cookie{Name: "csrftoken", Value: fakeCSRFCookie, Path: "/"})
				_, _ = fmt.Fprint(w, tc.form)
			}
			c, _ := f.client()
			c.SetUserAgent("dizzy/test")
			if err := c.NewSession().Login(t.Context(), alice); err != nil {
				t.Fatalf("Login: %v", err)
			}
			var post *seen
			for _, r := range f.seenRequests() {
				if r.method == http.MethodPost {
					post = &r
				}
				if ua := r.header.Get("User-Agent"); ua != "dizzy/test" {
					t.Errorf("%s %s User-Agent = %q, want dizzy/test", r.method, r.path, ua)
				}
				if al := r.header.Get("Accept-Language"); al != "en" {
					t.Errorf("%s %s Accept-Language = %q, want en", r.method, r.path, al)
				}
			}
			if post == nil {
				t.Fatal("no POST was sent")
			}
			want := map[string]string{
				"csrfmiddlewaretoken": fakeCSRF, "region": tc.region, "domain": "Default",
				"username": "alice", "password": fakePassword,
			}
			for k, v := range want {
				if got := post.form.Get(k); got != v {
					t.Errorf("form %s = %q, want %q", k, got, v)
				}
			}
			if ref := post.header.Get("Referer"); ref != f.srv.URL+"/auth/login/" {
				t.Errorf("Referer = %q, want the login page", ref)
			}
			if post.cookies["csrftoken"] != fakeCSRFCookie {
				t.Errorf("the POST carried csrftoken %q, want the cookie the form set", post.cookies["csrftoken"])
			}
		})
	}
}

// TestLoginPageFailures asserts a login page answered with 500 records
// http_500 and sends no POST, and one without a CSRF token or with an empty
// body returns ErrMismatch and sends no POST.
func TestLoginPageFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		handle http.HandlerFunc
		kind   string
	}{
		{"500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, "http_500"},
		{"no token", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `<form><input name="region" value="r"></form>`)
		}, "mismatch"},
		{"empty body", func(http.ResponseWriter, *http.Request) {}, "mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHorizon(t, "")
			f.loginPage = tc.handle
			c, m := f.client()
			err := c.NewSession().Login(t.Context(), alice)
			if err == nil {
				t.Fatal("Login() = nil, want an error")
			}
			if got := ErrorKind(err); got != tc.kind {
				t.Errorf("error kind = %q, want %q (%v)", got, tc.kind, err)
			}
			if tc.kind == "mismatch" && !errors.Is(err, ErrMismatch) {
				t.Errorf("Login() = %v, want ErrMismatch", err)
			}
			if st, _ := stats(m, "login_page"); st.Failed != 1 {
				t.Errorf("login_page samples = %+v, want one failure", st)
			}
			if errorCount(m, tc.kind) != 1 {
				t.Errorf("error %s counted %d times, want 1", tc.kind, errorCount(m, tc.kind))
			}
			if n := f.posts(); n != 0 {
				t.Errorf("sent %d POST(s), want none", n)
			}
		})
	}
}

// TestLoginOutcomes asserts a login answered with 200 is ErrRejected, and
// one redirected into /auth/password/ or redirected without a sessionid
// cookie is ErrMismatch.
func TestLoginOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		post   http.HandlerFunc
		target error
		kind   string
	}{
		{"form again", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, hiddenRegionForm) }, ErrRejected, "rejected"},
		{"expired password", func(w http.ResponseWriter, r *http.Request) {
			http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: fakeSessionID, Path: "/"})
			http.Redirect(w, r, "/auth/password/x/", http.StatusFound)
		}, ErrMismatch, "mismatch"},
		{"no session cookie", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/project/", http.StatusFound)
		}, ErrMismatch, "mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHorizon(t, "")
			f.loginPost = tc.post
			c, m := f.client()
			s := c.NewSession()
			err := s.Login(t.Context(), alice)
			if !errors.Is(err, tc.target) {
				t.Fatalf("Login() = %v, want %v", err, tc.target)
			}
			if ErrorKind(err) != tc.kind || errorCount(m, tc.kind) != 1 {
				t.Errorf("error kind %q counted %d times, want %q once", ErrorKind(err), errorCount(m, tc.kind), tc.kind)
			}
			if s.LoggedIn() {
				t.Error("LoggedIn() = true after a failed login")
			}
			if agg := m.Aggregate(0); len(agg.Readiness) != 1 || agg.Readiness[0].OK != 0 {
				t.Errorf("readiness = %+v, want one failed session sample", agg.Readiness)
			}
			if IsRetryable(err) {
				t.Errorf("IsRetryable(%v) = true, want false for a sentinel", err)
			}
		})
	}
}

// TestPageRedirectIsNotFollowed asserts a page answered with 302 records
// http_302 and the redirect's target is never requested.
func TestPageRedirectIsNotFollowed(t *testing.T) {
	f := newFakeHorizon(t, "")
	f.page = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere/" {
			t.Error("the redirect was followed")
		}
		http.Redirect(w, r, "/elsewhere/", http.StatusFound)
	}
	c, m := f.client()
	err := c.NewSession().Page(t.Context(), plan.Page{Name: "identity", Path: "/identity/"})
	if ErrorKind(err) != "http_302" {
		t.Fatalf("Page() = %v (kind %q), want http_302", err, ErrorKind(err))
	}
	if errorCount(m, "http_302") != 1 {
		t.Error("http_302 was not recorded")
	}
	if n := len(f.seenRequests()); n != 1 {
		t.Errorf("sent %d request(s), want 1", n)
	}
}

// TestSwitchMismatches asserts each way a switch can fail to land is
// ErrMismatch: a recent_project of another project, no recent_project, a
// redirect to the login page, and a landing page reporting the failure.
func TestSwitchMismatches(t *testing.T) {
	landing := plan.Page{Name: "identity", Path: "/identity/"}
	for _, tc := range []struct {
		name       string
		switchView http.HandlerFunc
		page       http.HandlerFunc
	}{
		{"other project", func(w http.ResponseWriter, r *http.Request) {
			http.SetCookie(w, &http.Cookie{Name: "recent_project", Value: "p1", Path: "/"})
			http.Redirect(w, r, r.URL.Query().Get("next"), http.StatusFound)
		}, nil},
		{"no cookie", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, r.URL.Query().Get("next"), http.StatusFound)
		}, nil},
		{"login page", func(w http.ResponseWriter, r *http.Request) {
			http.SetCookie(w, &http.Cookie{Name: "recent_project", Value: "p2", Path: "/"})
			http.Redirect(w, r, "/auth/login/", http.StatusFound)
		}, nil},
		{"failure message", nil, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `<div class="alert">Project switch failed for user "alice".</div>`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHorizon(t, "")
			if tc.switchView != nil {
				f.switchView = tc.switchView
			}
			if tc.page != nil {
				f.page = tc.page
			}
			c, m := f.client()
			s := c.NewSession()
			if err := s.Login(t.Context(), alice); err != nil {
				t.Fatalf("Login: %v", err)
			}
			err := s.Switch(t.Context(), "p2", landing)
			if !errors.Is(err, ErrMismatch) {
				t.Fatalf("Switch() = %v, want ErrMismatch", err)
			}
			if errorCount(m, "mismatch") != 1 {
				t.Error("the mismatch was not recorded")
			}
		})
	}
}

// TestLogoutFailures asserts a logout without a csrftoken cookie returns
// ErrMismatch without a request, and one answered with 405 records
// http_405.
func TestLogoutFailures(t *testing.T) {
	t.Run("no cookie", func(t *testing.T) {
		f := newFakeHorizon(t, "")
		c, _ := f.client()
		err := c.NewSession().Logout(t.Context())
		if !errors.Is(err, ErrMismatch) {
			t.Fatalf("Logout() = %v, want ErrMismatch", err)
		}
		if n := len(f.seenRequests()); n != 0 {
			t.Errorf("sent %d request(s), want none", n)
		}
	})
	t.Run("405", func(t *testing.T) {
		f := newFakeHorizon(t, "")
		f.logout = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusMethodNotAllowed) }
		c, m := f.client()
		s := c.NewSession()
		if err := s.Login(t.Context(), alice); err != nil {
			t.Fatalf("Login: %v", err)
		}
		err := s.Logout(t.Context())
		if ErrorKind(err) != "http_405" || errorCount(m, "http_405") != 1 {
			t.Fatalf("Logout() = %v (kind %q), want http_405 recorded", err, ErrorKind(err))
		}
		if !s.LoggedIn() {
			t.Error("LoggedIn() = false after a logout that did not answer 302")
		}
	})
}

// TestWebrootPrefixesEveryRequest asserts a base URL with the path
// /dashboard prefixes every request and the switch's next parameter.
func TestWebrootPrefixesEveryRequest(t *testing.T) {
	f := newFakeHorizon(t, "/dashboard")
	c, _ := f.client()
	s := c.NewSession()
	ctx := t.Context()
	landing := plan.Page{Name: "identity", Path: "/identity/"}
	if err := s.Login(ctx, alice); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := s.Page(ctx, landing); err != nil {
		t.Fatalf("Page: %v", err)
	}
	if err := s.Switch(ctx, "p 2", landing); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if err := s.Logout(ctx); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	reqs := f.seenRequests()
	if len(reqs) != 6 {
		t.Fatalf("sent %d requests, want 6", len(reqs))
	}
	for _, r := range reqs {
		if !strings.HasPrefix(r.path, "/dashboard/") {
			t.Errorf("%s %s lacks the /dashboard prefix", r.method, r.path)
		}
		if strings.HasPrefix(r.path, "/dashboard/auth/switch/") {
			if r.path != "/dashboard/auth/switch/p 2/" {
				t.Errorf("switch path = %q, want the escaped project id", r.path)
			}
			if next := r.query.Get("next"); next != "/dashboard/identity/" {
				t.Errorf("next = %q, want /dashboard/identity/", next)
			}
		}
	}
}

// TestTimeoutAndClosedServer asserts a server that does not answer within
// the context's deadline records timeout with an error that wraps
// context.DeadlineExceeded, and a closed server records other and is
// retryable.
func TestTimeoutAndClosedServer(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		f := newFakeHorizon(t, "")
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		f.loginPage = func(http.ResponseWriter, *http.Request) { <-release }
		c, m := f.client()
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		err := c.NewSession().Login(ctx, alice)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Login() = %v, want it to wrap context.DeadlineExceeded", err)
		}
		if errorCount(m, "timeout") != 1 {
			t.Errorf("errors = %+v, want one timeout", m.Aggregate(0).Errors)
		}
		if IsRetryable(err) {
			t.Error("a context deadline is retryable, want not")
		}
	})
	t.Run("closed", func(t *testing.T) {
		f := newFakeHorizon(t, "")
		c, m := f.client()
		f.srv.Close()
		err := c.NewSession().Login(t.Context(), alice)
		if err == nil {
			t.Fatal("Login() = nil against a closed server")
		}
		if errorCount(m, "other") != 1 {
			t.Errorf("errors = %+v, want one other", m.Aggregate(0).Errors)
		}
		if !IsRetryable(err) {
			t.Errorf("IsRetryable(%v) = false, want true for a refused connection", err)
		}
	})
}

// TestErrorsCarryNoSecret asserts no error a session returns carries the
// password, a cookie value or the CSRF token.
func TestErrorsCarryNoSecret(t *testing.T) {
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	landing := plan.Page{Name: "identity", Path: "/identity/"}

	// A rejected login.
	f := newFakeHorizon(t, "")
	f.loginPost = func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, hiddenRegionForm) }
	c, _ := f.client()
	collect(c.NewSession().Login(t.Context(), alice))

	// Every step failing after a good login.
	f = newFakeHorizon(t, "")
	f.page = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
	f.switchView = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/auth/login/?next="+fakeSessionID, http.StatusFound)
	}
	f.logout = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }
	c, _ = f.client()
	s := c.NewSession()
	if err := s.Login(t.Context(), alice); err != nil {
		t.Fatalf("Login: %v", err)
	}
	collect(s.Page(t.Context(), landing))
	collect(s.Switch(t.Context(), "p2", landing))
	collect(s.Logout(t.Context()))

	// A transport error.
	f = newFakeHorizon(t, "")
	c, _ = f.client()
	f.srv.Close()
	collect(c.NewSession().Login(t.Context(), alice))

	if len(errs) != 5 {
		t.Fatalf("collected %d errors, want 5: %v", len(errs), errs)
	}
	for _, err := range errs {
		for _, secret := range []string{fakePassword, fakeCSRF, fakeCSRFCookie, fakeSessionID} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q carries a secret", err)
			}
		}
	}
}
