package horizongraph

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/horizon"
	horizonexec "github.com/B42Labs/dizzy/internal/horizon/executor"
	"github.com/B42Labs/dizzy/internal/horizon/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeDashboard opens fakeSessions and counts what they send; it tracks the
// logged-in population and every invariant the graph must keep.
type fakeDashboard struct {
	mu        sync.Mutex
	failLogin bool
	log       []string
	loggedIn  int
	maxLogged int
	opened    int
	closed    int
}

func (d *fakeDashboard) open() horizonexec.Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opened++
	return &fakeSession{d: d, n: d.opened}
}

func (d *fakeDashboard) add(n int, step string) {
	d.mu.Lock()
	d.log = append(d.log, fmt.Sprintf("%d %s", n, step))
	d.mu.Unlock()
}

func (d *fakeDashboard) count(step string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	var c int
	for _, l := range d.log {
		var n int
		var s string
		if _, err := fmt.Sscanf(l, "%d %s", &n, &s); err == nil && s == step {
			c++
		}
	}
	return c
}

type fakeSession struct {
	d        *fakeDashboard
	n        int
	loggedIn bool
}

func (s *fakeSession) Login(context.Context, horizon.Credentials) error {
	s.d.add(s.n, "login")
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if s.d.failLogin {
		return fmt.Errorf("%w: form again", horizon.ErrRejected)
	}
	s.loggedIn = true
	s.d.loggedIn++
	s.d.maxLogged = max(s.d.maxLogged, s.d.loggedIn)
	return nil
}

func (s *fakeSession) Page(context.Context, plan.Page) error {
	s.d.add(s.n, "page")
	return nil
}

func (s *fakeSession) Switch(context.Context, string, plan.Page) error {
	s.d.add(s.n, "switch")
	return nil
}

func (s *fakeSession) Logout(context.Context) error {
	s.d.add(s.n, "logout")
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if s.loggedIn {
		s.loggedIn = false
		s.d.loggedIn--
	}
	return nil
}

func (s *fakeSession) LoggedIn() bool {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	return s.loggedIn
}

func (s *fakeSession) Close() {
	s.d.mu.Lock()
	s.d.closed++
	s.d.mu.Unlock()
}

// testPlan has n sessions of the clouds.yaml user, each loading page a and
// switching.
func testPlan(n int) *plan.Plan {
	p := &plan.Plan{Scenario: "t", Seed: 3, Projects: []plan.Project{}, Users: []plan.User{}, Pages: []plan.Page{{Name: "a", Path: "/a/"}}}
	for i := 1; i <= n; i++ {
		p.Sessions = append(p.Sessions, plan.Session{Name: fmt.Sprintf("session-%04d", i), Pages: []string{"a"}, Switch: true, SwitchDraw: i})
	}
	return p
}

func testTarget(d *fakeDashboard) horizonexec.Target {
	return horizonexec.Target{Open: d.open, Cloud: horizon.Credentials{Username: "cloud"}, Projects: []string{"p1"}}
}

// TestBuildNodes asserts Build returns one parentless session node per plan
// session, in plan order, each with a mutate.
func TestBuildNodes(t *testing.T) {
	nodes, sessions, err := Build(testPlan(3), testTarget(&fakeDashboard{}), &horizonexec.Provisioned{}, "run1", time.Second)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(nodes) != 3 || sessions.Len() != 0 {
		t.Fatalf("Build gave %d nodes and %d live sessions, want 3 and 0", len(nodes), sessions.Len())
	}
	for i, n := range nodes {
		if want := fmt.Sprintf("session-%04d", i+1); n.Key != want || n.Kind != "session" || len(n.Parents) != 0 || n.Mutate == nil || n.Pinned {
			t.Errorf("node %d = %+v, want a parentless, unpinned session node %s with a mutate", i, n, want)
		}
	}
}

// TestBuildRejectsInvalidPlan asserts an invalid plan fails Build.
func TestBuildRejectsInvalidPlan(t *testing.T) {
	p := testPlan(1)
	p.Sessions[0].Pages = []string{"missing"}
	if _, _, err := Build(p, testTarget(&fakeDashboard{}), nil, "run1", time.Second); err == nil {
		t.Fatal("Build() of an invalid plan = nil error")
	}
}

// TestNodeLifecycle asserts Create logs in and records the session,
// Mutate browses, Delete logs out, and the session is then forgotten.
func TestNodeLifecycle(t *testing.T) {
	d := &fakeDashboard{}
	nodes, sessions, err := Build(testPlan(1), testTarget(d), &horizonexec.Provisioned{}, "run1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	n := nodes[0]
	res, err := n.Create(ctx, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Kind != "session" || res.Logical != "session-0001" || res.Name != "dizzyhorizon-run1-session-0001" || res.ID != "session-0001" {
		t.Errorf("created %+v", res)
	}
	if sessions.Len() != 1 || d.loggedIn != 1 {
		t.Fatalf("after Create: %d held, %d logged in, want 1 and 1", sessions.Len(), d.loggedIn)
	}
	if err := n.Mutate(ctx, nil, res); err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if d.count("page") != 1 || d.count("switch") != 1 {
		t.Errorf("Mutate sent %d page(s) and %d switch(es), want 1 and 1", d.count("page"), d.count("switch"))
	}
	if err := n.Delete(ctx, nil, res); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if sessions.Len() != 0 || d.loggedIn != 0 || d.count("logout") != 1 || d.closed != 1 {
		t.Errorf("after Delete: %d held, %d logged in, %d logout(s), %d closed", sessions.Len(), d.loggedIn, d.count("logout"), d.closed)
	}
}

// TestFailedLoginSendsNothingLater asserts Mutate and Delete of a session
// whose login failed send no request.
func TestFailedLoginSendsNothingLater(t *testing.T) {
	d := &fakeDashboard{failLogin: true}
	nodes, sessions, err := Build(testPlan(1), testTarget(d), &horizonexec.Provisioned{}, "run1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := nodes[0].Create(ctx, nil); !errors.Is(err, horizon.ErrRejected) {
		t.Fatalf("Create() = %v, want ErrRejected", err)
	}
	if err := nodes[0].Mutate(ctx, nil, resource.Resource{}); err != nil {
		t.Errorf("Mutate() = %v, want nil", err)
	}
	if err := nodes[0].Delete(ctx, nil, resource.Resource{}); err != nil {
		t.Errorf("Delete() = %v, want nil", err)
	}
	if got := d.log; !slices.Equal(got, []string{"1 login"}) {
		t.Errorf("sent %v, want the login alone", got)
	}
	if sessions.Len() != 0 || d.closed != 1 {
		t.Errorf("%d held and %d closed, want 0 and 1", sessions.Len(), d.closed)
	}
}

// TestLogoutAll asserts LogoutAll logs out every live session and returns
// the count, and 0, nil when none is live.
func TestLogoutAll(t *testing.T) {
	d := &fakeDashboard{}
	nodes, sessions, err := Build(testPlan(3), testTarget(d), &horizonexec.Provisioned{}, "run1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := sessions.LogoutAll(t.Context(), time.Second); n != 0 || err != nil {
		t.Errorf("LogoutAll() with none live = %d, %v, want 0, nil", n, err)
	}
	for _, n := range nodes[:2] {
		if _, err := n.Create(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
	}
	n, err := sessions.LogoutAll(t.Context(), time.Second)
	if n != 2 || err != nil {
		t.Errorf("LogoutAll() = %d, %v, want 2, nil", n, err)
	}
	if d.loggedIn != 0 || sessions.Len() != 0 || d.closed != 2 {
		t.Errorf("after LogoutAll: %d logged in, %d held, %d closed", d.loggedIn, sessions.Len(), d.closed)
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "timeout"},
		{fmt.Errorf("x: %w", horizon.ErrRejected), "rejected"},
		{fmt.Errorf("x: %w", horizon.ErrMismatch), "mismatch"},
		{&horizon.StatusError{Status: 503}, "transient"},
		{&horizon.StatusError{Status: 404}, "other"},
	} {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("Classify(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
