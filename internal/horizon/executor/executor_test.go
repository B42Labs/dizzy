package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/horizon"
	"github.com/B42Labs/dizzy/internal/horizon/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeDashboard hands out fakeSessions numbered from 1 in the order they
// are opened and logs every call as "<n> <step> <arg>".
type fakeDashboard struct {
	mu     sync.Mutex
	opened int
	log    []string

	// fail returns the error session n's step (login, page, switch, logout)
	// fails with, or nil.
	fail func(n int, step string) error
	// onLogin runs before session n's login answers.
	onLogin func(n int)
}

func (d *fakeDashboard) open() Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opened++
	return &fakeSession{d: d, n: d.opened}
}

func (d *fakeDashboard) record(n int, step, arg string) error {
	d.mu.Lock()
	d.log = append(d.log, fmt.Sprintf("%d %s %s", n, step, arg))
	fail := d.fail
	d.mu.Unlock()
	if fail != nil {
		return fail(n, step)
	}
	return nil
}

// steps returns session n's logged steps, in order.
func (d *fakeDashboard) steps(n int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	prefix := fmt.Sprintf("%d ", n)
	for _, l := range d.log {
		if s, ok := strings.CutPrefix(l, prefix); ok {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

type fakeSession struct {
	d        *fakeDashboard
	n        int
	loggedIn bool
}

func (s *fakeSession) Login(ctx context.Context, cred horizon.Credentials) error {
	if s.d.onLogin != nil {
		s.d.onLogin(s.n)
	}
	if err := s.d.record(s.n, "login", cred.Username+"@"+cred.Domain); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.loggedIn = true
	return nil
}

func (s *fakeSession) Page(ctx context.Context, p plan.Page) error {
	if err := s.d.record(s.n, "page", p.Name); err != nil {
		return err
	}
	return ctx.Err()
}

func (s *fakeSession) Switch(ctx context.Context, projectID string, landing plan.Page) error {
	if err := s.d.record(s.n, "switch", projectID+"->"+landing.Name); err != nil {
		return err
	}
	return ctx.Err()
}

func (s *fakeSession) Logout(ctx context.Context) error {
	if err := s.d.record(s.n, "logout", ""); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.loggedIn = false
	return nil
}

func (s *fakeSession) LoggedIn() bool { return s.loggedIn }
func (s *fakeSession) Close()         { _ = s.d.record(s.n, "close", "") }

// fakeIdentity creates users and projects with ids <kind>-<logical> and logs
// every call.
type fakeIdentity struct {
	mu        sync.Mutex
	log       []string
	failUser  map[string]error
	failProj  map[string]error
	failAssig error
}

func (f *fakeIdentity) add(l string) {
	f.mu.Lock()
	f.log = append(f.log, l)
	f.mu.Unlock()
}

func (f *fakeIdentity) CreateProject(_ context.Context, p plan.Project, domainID string) (resource.Resource, error) {
	f.add("project " + p.Name + " in " + domainID)
	if err := f.failProj[p.Name]; err != nil {
		return resource.Resource{}, err
	}
	return resource.Resource{Kind: horizon.KindProject, Logical: p.Name, Name: "dizzyhorizon-r-" + p.Name, ID: "id-" + p.Name}, nil
}

func (f *fakeIdentity) CreateUser(_ context.Context, u plan.User, domainID, password string) (resource.Resource, error) {
	if password == "" {
		return resource.Resource{}, errors.New("empty password")
	}
	f.add("user " + u.Name + " in " + domainID)
	if err := f.failUser[u.Name]; err != nil {
		return resource.Resource{}, err
	}
	return resource.Resource{Kind: horizon.KindUser, Logical: u.Name, Name: "dizzyhorizon-r-" + u.Name, ID: "id-" + u.Name}, nil
}

func (f *fakeIdentity) AssignRole(_ context.Context, userID, projectID, roleID string) error {
	f.add("assign " + roleID + " " + userID + " " + projectID)
	return f.failAssig
}

func (f *fakeIdentity) entries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

// cloudPlan is a plan of three sessions of the clouds.yaml user: each loads
// pages a and b and switches.
func cloudPlan() *plan.Plan {
	p := &plan.Plan{
		Scenario: "t",
		Projects: []plan.Project{},
		Users:    []plan.User{},
		Pages:    []plan.Page{{Name: "a", Path: "/a/"}, {Name: "b", Path: "/b/"}},
	}
	for i := 1; i <= 3; i++ {
		p.Sessions = append(p.Sessions, plan.Session{Name: fmt.Sprintf("session-%04d", i), Pages: []string{"a", "b"}, Switch: true, SwitchDraw: i})
	}
	return p
}

// userPlan is cloudPlan with two created users on two created projects.
func userPlan() *plan.Plan {
	p := cloudPlan()
	p.Role = "member"
	p.Projects = []plan.Project{{Name: "project-0001"}, {Name: "project-0002"}}
	p.Users = []plan.User{{Name: "user-0001"}, {Name: "user-0002"}}
	for i := range p.Sessions {
		p.Sessions[i].User = p.Users[i%2].Name
	}
	return p
}

func cloudTarget(d *fakeDashboard) Target {
	return Target{
		Open:       d.open,
		DomainID:   "did",
		DomainName: "Users",
		RoleID:     "rid",
		Cloud:      horizon.Credentials{Username: "cloud", Password: "pw", Domain: "Users"},
		Projects:   []string{"p1", "p2"},
	}
}

// TestJourneyFailures asserts a failed login ends its journey without a
// logout, a failed page does not stop the switch and the logout, and Apply
// then reports one of three journeys failed with the others completed.
func TestJourneyFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		step  string
		want2 []string
	}{
		{"login", "login", []string{"login cloud@Users", "close"}},
		{"page", "page", []string{"login cloud@Users", "page a", "page b", "switch p1->a", "logout", "close"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDashboard{fail: func(n int, step string) error {
				if n == 2 && step == tc.step {
					return fmt.Errorf("%w: boom", horizon.ErrMismatch)
				}
				return nil
			}}
			res, err := Apply(t.Context(), cloudTarget(d), cloudPlan(), 1, time.Second)
			if err == nil || !strings.HasPrefix(err.Error(), "1 of 3 journeys failed") || !errors.Is(err, horizon.ErrMismatch) {
				t.Fatalf("Apply() = %v, want 1 of 3 journeys failed wrapping the mismatch", err)
			}
			if res.Journeys != 3 || res.Failed != 1 || len(res.Created) != 0 {
				t.Errorf("result = %+v, want 3 journeys, 1 failed, nothing created", res)
			}
			if got := d.steps(2); !slices.Equal(got, tc.want2) {
				t.Errorf("session 2 steps = %v, want %v", got, tc.want2)
			}
			for _, n := range []int{1, 3} {
				want := []string{"login cloud@Users", "page a", "page b", fmt.Sprintf("switch p%d->a", n%2+1), "logout", "close"}
				if got := d.steps(n); !slices.Equal(got, want) {
					t.Errorf("session %d steps = %v, want %v", n, got, want)
				}
			}
		})
	}
}

// TestApplyCancelStopsNewJourneysAndLogsOut asserts a context cancelled
// during the journeys starts no further journey, while every session that
// logged in still logs out.
func TestApplyCancelStopsNewJourneysAndLogsOut(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := &fakeDashboard{}
	d.fail = func(n int, step string) error {
		if n == 2 && step == "page" {
			cancel()
		}
		return nil
	}
	res, err := Apply(ctx, cloudTarget(d), cloudPlan(), 1, time.Second)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply() = %v, want an error wrapping context.Canceled", err)
	}
	if d.opened != 2 || res.Journeys != 2 {
		t.Errorf("opened %d sessions and ran %d journeys, want 2: the third must not start", d.opened, res.Journeys)
	}
	for _, n := range []int{1, 2} {
		steps := d.steps(n)
		if !slices.Contains(steps, "logout") {
			t.Errorf("session %d steps = %v, want a logout", n, steps)
		}
	}
}

// TestLogoutAfterCancelUsesUncancelledContext asserts the logout of a
// journey whose context was cancelled succeeds once, on a context the
// cancellation does not reach.
func TestLogoutAfterCancelUsesUncancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	d := &fakeDashboard{}
	sess := d.open()
	if err := sess.Login(ctx, horizon.Credentials{Username: "x"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := Logout(ctx, sess, time.Second); err != nil {
		t.Fatalf("Logout() = %v, want nil", err)
	}
	if sess.LoggedIn() {
		t.Error("the session is still logged in")
	}
	if got := d.steps(1); !slices.Equal(got, []string{"login x@", "logout"}) {
		t.Errorf("steps = %v, want one login and one logout", got)
	}
}

// TestProvisionStages asserts Provision creates the projects, then the
// users, then one assignment per user and project, and the sessions log in
// as their created users and switch between the created projects.
func TestProvisionStages(t *testing.T) {
	id := &fakeIdentity{}
	d := &fakeDashboard{}
	tgt := cloudTarget(d)
	tgt.Identity = id
	p := userPlan()

	res, err := Apply(t.Context(), tgt, p, 1, time.Second)
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}
	want := []string{
		"project project-0001 in did", "project project-0002 in did",
		"user user-0001 in did", "user user-0002 in did",
		"assign rid id-user-0001 id-project-0001", "assign rid id-user-0001 id-project-0002",
		"assign rid id-user-0002 id-project-0001", "assign rid id-user-0002 id-project-0002",
	}
	if got := id.entries(); !slices.Equal(got, want) {
		t.Errorf("identity calls = %v, want %v", got, want)
	}
	var created []string
	for _, r := range res.Created {
		created = append(created, string(r.Kind)+":"+r.Logical)
	}
	if want := []string{"project:project-0001", "project:project-0002", "user:user-0001", "user:user-0002"}; !slices.Equal(created, want) {
		t.Errorf("created = %v, want %v", created, want)
	}
	if got := d.steps(1); !slices.Equal(got[:1], []string{"login dizzyhorizon-r-user-0001@Users"}) || !slices.Contains(got, "switch id-project-0002->a") {
		t.Errorf("session 1 steps = %v, want a login as user-0001 and a switch to project-0002", got)
	}
	if got := d.steps(2); got[0] != "login dizzyhorizon-r-user-0002@Users" {
		t.Errorf("session 2 logged in as %v, want user-0002", got)
	}
}

// TestProvisionStopsAtFirstError asserts a failed user create stops
// Provision before the assignments and returns what it created, and a 403
// is not retried.
func TestProvisionStopsAtFirstError(t *testing.T) {
	forbidden := fmt.Errorf("creating user: %w: %w", horizon.ErrForbidden, gophercloud.ErrUnexpectedResponseCode{Actual: 403})
	id := &fakeIdentity{failUser: map[string]error{"user-0002": forbidden}}
	tgt := cloudTarget(&fakeDashboard{})
	tgt.Identity = id

	prov, err := Provision(t.Context(), tgt, userPlan(), 1, time.Second)
	if !errors.Is(err, horizon.ErrForbidden) {
		t.Fatalf("Provision() = %v, want ErrForbidden", err)
	}
	if n := len(prov.Created); n != 3 {
		t.Errorf("created %d resource(s), want the two projects and user-0001: %+v", n, prov.Created)
	}
	for _, l := range id.entries() {
		if strings.HasPrefix(l, "assign") {
			t.Errorf("an assignment followed the failed user create: %s", l)
		}
	}
	if n := strings.Count(strings.Join(id.entries(), "\n"), "user user-0002"); n != 1 {
		t.Errorf("user-0002 was created %d times, want once: a 403 is not retried", n)
	}
}

// TestProvisionWithoutUsers asserts a plan without users creates nothing.
func TestProvisionWithoutUsers(t *testing.T) {
	id := &fakeIdentity{}
	tgt := cloudTarget(&fakeDashboard{})
	tgt.Identity = id
	prov, err := Provision(t.Context(), tgt, cloudPlan(), 4, time.Second)
	if err != nil || len(prov.Created) != 0 || len(id.entries()) != 0 {
		t.Errorf("Provision() = %+v, %v with calls %v, want nothing", prov, err, id.entries())
	}
}

// TestCredentialsFor asserts a session of a created user logs in with that
// user's credentials and the target's domain name, and one without with
// the clouds.yaml user's.
func TestCredentialsFor(t *testing.T) {
	tgt := cloudTarget(&fakeDashboard{})
	prov := &Provisioned{Credentials: map[string]horizon.Credentials{"user-0001": {Username: "u1", Password: "x", Domain: "Users"}}}
	if got := CredentialsFor(tgt, prov, plan.Session{User: "user-0001"}); got.Username != "u1" {
		t.Errorf("created user's credentials = %+v", got)
	}
	if got := CredentialsFor(tgt, prov, plan.Session{}); got != tgt.Cloud {
		t.Errorf("cloud user's credentials = %+v, want %+v", got, tgt.Cloud)
	}
}

// TestWithRetry asserts a transient error is retried and a sentinel is not.
func TestWithRetry(t *testing.T) {
	var calls int
	err := WithRetry(t.Context(), time.Second, func(context.Context) error {
		calls++
		if calls < 2 {
			return &horizon.StatusError{Step: "page", Status: 503}
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Errorf("WithRetry() = %v after %d call(s), want success on the second", err, calls)
	}
	calls = 0
	err = WithRetry(t.Context(), time.Second, func(context.Context) error {
		calls++
		return horizon.ErrRejected
	})
	if !errors.Is(err, horizon.ErrRejected) || calls != 1 {
		t.Errorf("WithRetry() = %v after %d call(s), want ErrRejected after one", err, calls)
	}
}
