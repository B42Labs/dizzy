package horizon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/B42Labs/dizzy/internal/horizon/plan"
)

// Credentials are what a login form is posted with: the user's name, its
// password, and the name of the user's domain. Their values never appear in
// an error text.
type Credentials struct {
	Username string
	Password string
	Domain   string
}

// Session is one browser-like login to the dashboard: its own cookie jar,
// its own connections, and whether it is logged in. Its methods are called
// one after another, never concurrently.
type Session struct {
	c         *Client
	http      *http.Client
	transport *http.Transport
	jar       *cookiejar.Jar

	mu       sync.Mutex
	loggedIn bool
}

// NewSession returns a logged-out session with an empty cookie jar and its
// own transport, which carries the client's TLS settings. It follows no
// redirect, so every check sees the status Horizon sent.
func (c *Client) NewSession() *Session {
	// cookiejar.New only fails for an Options value it cannot use; nil is
	// always valid.
	jar, _ := cookiejar.New(nil)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = c.tlsConfig.Clone()
	return &Session{
		c:         c,
		transport: tr,
		jar:       jar,
		http: &http.Client{
			Transport: tr,
			Jar:       jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// LoggedIn reports whether the last login succeeded and no logout followed.
func (s *Session) LoggedIn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loggedIn
}

// setLoggedIn records whether the session is logged in.
func (s *Session) setLoggedIn(v bool) {
	s.mu.Lock()
	s.loggedIn = v
	s.mu.Unlock()
}

// Close closes the session's idle connections, as a browser that ends does.
func (s *Session) Close() {
	s.transport.CloseIdleConnections()
}

// loginURL is the URL of Horizon's login view.
func (s *Session) loginURL() *url.URL {
	return s.c.url("/auth/login/", "/auth/login/")
}

// send sends one request: a GET without form, a POST of form otherwise.
// Every request carries the User-Agent and Accept-Language: en; a POST also
// carries the login page as its Referer, which Django's CSRF check needs
// over HTTPS.
func (s *Session) send(ctx context.Context, u *url.URL, form url.Values) (*http.Response, error) {
	method, body := http.MethodGet, io.Reader(nil)
	if form != nil {
		method, body = http.MethodPost, strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.c.userAgent)
	req.Header.Set("Accept-Language", "en")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Referer", s.loginURL().String())
	}
	return s.http.Do(req)
}

// drain reads what is left of a body, up to maxBody, and closes it, so the
// connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
}

// readBody reads a body that is parsed, up to maxBody, and closes it.
func readBody(resp *http.Response) ([]byte, error) {
	defer drain(resp)
	return io.ReadAll(io.LimitReader(resp.Body, maxBody))
}

// redirectPath returns the path a redirect's Location points at, resolved
// against the request, or "" when it has none.
func redirectPath(resp *http.Response) string {
	loc, err := resp.Location()
	if err != nil {
		return ""
	}
	return loc.Path
}

// statusError builds the StatusError of an unexpected answer to step.
func statusError(step string, resp *http.Response) error {
	return &StatusError{Step: step, Status: resp.StatusCode, Location: redirectPath(resp)}
}

// cookie returns the value of the cookie name the jar would send to u, or
// "" when it holds none.
func (s *Session) cookie(u *url.URL, name string) string {
	for _, ck := range s.jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

// Login loads the login form and posts it with cred. The form must answer
// 200 and carry a CSRF token and a region; otherwise no POST follows. The
// POST succeeds with a 302 to a path outside <base>/auth/ that leaves a
// sessionid cookie in the jar. A 200, the form again, is ErrRejected. A 302
// into /auth/ (an expired password, a TOTP step) or one without the cookie
// is ErrMismatch, naming the redirect's path. Each request is one sample,
// and the whole login one time-to-ready sample of kind session.
func (s *Session) Login(ctx context.Context, cred Credentials) error {
	start := time.Now()
	err := s.login(ctx, cred)
	s.c.recordReady(ctx, KindSession, time.Since(start), err == nil)
	return err
}

// login is Login without its time-to-ready sample.
func (s *Session) login(ctx context.Context, cred Credentials) error {
	loginURL := s.loginURL()
	var token, region string
	err := s.c.timed(ctx, KindLoginPage, "get", func(ctx context.Context) error {
		resp, err := s.send(ctx, loginURL, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			drain(resp)
			return statusError("login page", resp)
		}
		body, err := readBody(resp)
		if err != nil {
			return err
		}
		token, region, err = parseLoginForm(bytes.NewReader(body))
		return err
	})
	if err != nil {
		return fmt.Errorf("loading the login page: %w", err)
	}

	form := url.Values{
		"csrfmiddlewaretoken": {token},
		"region":              {region},
		"domain":              {cred.Domain},
		"username":            {cred.Username},
		"password":            {cred.Password},
	}
	authPrefix := s.c.basePath() + "/auth/"
	err = s.c.timed(ctx, KindLogin, "authenticate", func(ctx context.Context) error {
		resp, err := s.send(ctx, loginURL, form)
		if err != nil {
			return err
		}
		drain(resp)
		switch resp.StatusCode {
		case http.StatusOK:
			return fmt.Errorf("%w: Horizon rendered the login form again", ErrRejected)
		case http.StatusFound:
		default:
			return statusError("login", resp)
		}
		to := redirectPath(resp)
		if strings.HasPrefix(to, authPrefix) {
			return fmt.Errorf("%w: the login redirected to %s", ErrMismatch, to)
		}
		if s.cookie(loginURL, "sessionid") == "" {
			return fmt.Errorf("%w: the login redirected to %s without a sessionid cookie", ErrMismatch, to)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("logging in as %q: %w", cred.Username, err)
	}
	s.setLoggedIn(true)
	return nil
}

// Page loads p, which must answer 200.
func (s *Session) Page(ctx context.Context, p plan.Page) error {
	u := s.c.url(p.Path, p.Path)
	err := s.c.timed(ctx, PageKind(p.Name), "get", func(ctx context.Context) error {
		resp, err := s.send(ctx, u, nil)
		if err != nil {
			return err
		}
		drain(resp)
		if resp.StatusCode != http.StatusOK {
			return statusError("page "+p.Name, resp)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("loading page %s: %w", p.Name, err)
	}
	return nil
}

// Switch switches the session to projectID through Horizon's switch view,
// with landing as the page to go to next, and loads that page. The switch
// must answer 302 to exactly <base path><landing path> and leave the
// recent_project cookie equal to projectID; the landing page must answer
// 200 and not carry Horizon's "Project switch failed" message. Any other
// outcome is ErrMismatch. The cookie catches a failed switch away from
// another project, the message one into the project the session was
// already in.
func (s *Session) Switch(ctx context.Context, projectID string, landing plan.Page) error {
	next := s.c.basePath() + landing.Path
	u := s.c.url("/auth/switch/"+projectID+"/", "/auth/switch/"+url.PathEscape(projectID)+"/")
	u.RawQuery = url.Values{"next": {next}}.Encode()
	landingURL := s.c.url(landing.Path, landing.Path)

	err := s.c.timed(ctx, KindSwitch, "switch", func(ctx context.Context) error {
		resp, err := s.send(ctx, u, nil)
		if err != nil {
			return err
		}
		drain(resp)
		if resp.StatusCode != http.StatusFound {
			return statusError("switch", resp)
		}
		if to := redirectPath(resp); to != next {
			return fmt.Errorf("%w: the switch redirected to %s, not %s", ErrMismatch, to, next)
		}
		if got := s.cookie(landingURL, "recent_project"); got != projectID {
			if got == "" {
				return fmt.Errorf("%w: the switch to project %s set no recent_project cookie", ErrMismatch, projectID)
			}
			return fmt.Errorf("%w: the switch to project %s left the session in project %s", ErrMismatch, projectID, got)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("switching to project %s: %w", projectID, err)
	}

	err = s.c.timed(ctx, KindSwitchLanding, "get", func(ctx context.Context) error {
		resp, err := s.send(ctx, landingURL, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			drain(resp)
			return statusError("switch landing", resp)
		}
		body, err := readBody(resp)
		if err != nil {
			return err
		}
		if bytes.Contains(body, []byte("Project switch failed")) {
			return fmt.Errorf("%w: the landing page reports that the switch to project %s failed", ErrMismatch, projectID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("loading the page after the switch to project %s: %w", projectID, err)
	}
	return nil
}

// Logout posts the value of the jar's csrftoken cookie to Horizon's logout
// view, which must answer 302 to a path that ends in /auth/login/. Without
// the cookie it sends nothing and returns ErrMismatch, recorded as a failed
// logout. After a 302 the session reads as logged out, whatever the
// redirect's path.
func (s *Session) Logout(ctx context.Context) error {
	u := s.c.url("/auth/logout/", "/auth/logout/")
	err := s.c.timed(ctx, KindLogout, "logout", func(ctx context.Context) error {
		token := s.cookie(u, "csrftoken")
		if token == "" {
			return fmt.Errorf("%w: no csrftoken cookie to log out with", ErrMismatch)
		}
		resp, err := s.send(ctx, u, url.Values{"csrfmiddlewaretoken": {token}})
		if err != nil {
			return err
		}
		drain(resp)
		if resp.StatusCode != http.StatusFound {
			return statusError("logout", resp)
		}
		s.setLoggedIn(false)
		if to := redirectPath(resp); !strings.HasSuffix(to, "/auth/login/") {
			return fmt.Errorf("%w: the logout redirected to %s, not the login page", ErrMismatch, to)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("logging out: %w", err)
	}
	return nil
}
