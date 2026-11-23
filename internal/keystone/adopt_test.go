package keystone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// foundID is the id the fake Keystone of the adoption tests lists for the
// resource a 409 refers to.
const foundID = "found1"

// lookupFailedMsg is the warning a failed lookup after a 409 logs.
const lookupFailedMsg = "lookup after a 409 on create failed; the 409 stands"

// adoptKind is one of the four creates that send the admin token, as the
// adoption tests drive it against a fake Keystone.
type adoptKind struct {
	kind    resource.Kind
	logical string
	// path is the collection the create posts to and the lookup lists, and
	// key the collection's key in a list body.
	path, key string
	// domain is whether the lookup filters by the create's domain id.
	domain bool
	// match returns a list entry the lookup confirms for the cloud name
	// name: this run's resource of that name, with the id foundID.
	match func(name string) string
	// create runs the kind's create in domain domainID, where the kind has
	// one.
	create func(ctx context.Context, c *Client, domainID string) (resource.Resource, error)
}

// name is the cloud name the create sends for the kind's logical name.
func (k adoptKind) name() string { return resourceName("run0", k.logical) }

// routes answers the create's POST with post and the lookup's GET with get.
func (k adoptKind) routes(post, get keystoneReply) map[string]keystoneReply {
	return map[string]keystoneReply{"POST " + k.path: post, "GET " + k.path: get}
}

// adoptKinds are the four creates that send the admin token: user, project,
// domain and role, each with a list entry the lookup after a 409 confirms.
var adoptKinds = []adoptKind{
	{
		kind: KindUser, logical: "user-0001", path: "/users", key: "users", domain: true,
		match: func(name string) string {
			return fmt.Sprintf(`{"id":%q,"name":%q,"domain_id":"did1"}`, foundID, name)
		},
		create: func(ctx context.Context, c *Client, domainID string) (resource.Resource, error) {
			return c.CreateUser(ctx, keystoneplan.User{Name: "user-0001"}, domainID, "user-pw")
		},
	},
	{
		kind: KindProject, logical: "proj-0001", path: "/projects", key: "projects", domain: true,
		match: func(name string) string {
			return fmt.Sprintf(`{"id":%q,"name":%q,"domain_id":"did1","tags":["dizzy:run=run0","dizzy:type=project"]}`, foundID, name)
		},
		create: func(ctx context.Context, c *Client, domainID string) (resource.Resource, error) {
			return c.CreateProject(ctx, keystoneplan.Project{Name: "proj-0001"}, domainID)
		},
	},
	{
		kind: KindDomain, logical: "dom-0001", path: "/domains", key: "domains",
		match: func(name string) string {
			return fmt.Sprintf(`{"id":%q,"name":%q}`, foundID, name)
		},
		create: func(ctx context.Context, c *Client, _ string) (resource.Resource, error) {
			return c.CreateDomain(ctx, keystoneplan.Domain{Name: "dom-0001"})
		},
	},
	{
		kind: KindRole, logical: "role-0001", path: "/roles", key: "roles",
		match: func(name string) string {
			return fmt.Sprintf(`{"id":%q,"name":%q,"domain_id":null}`, foundID, name)
		},
		create: func(ctx context.Context, c *Client, _ string) (resource.Resource, error) {
			return c.CreateRole(ctx, keystoneplan.Role{Name: "role-0001"})
		},
	},
}

// adoptKindOf returns the adoptKinds entry of kind.
func adoptKindOf(t *testing.T, kind resource.Kind) adoptKind {
	t.Helper()
	for _, k := range adoptKinds {
		if k.kind == kind {
			return k
		}
	}
	t.Fatalf("no adoptKinds entry for %s", kind)
	return adoptKind{}
}

// conflictReply is Keystone's 409 for a create whose name is taken.
func conflictReply(name string) keystoneReply {
	return errorReply(http.StatusConflict, "Conflict occurred attempting to store - Duplicate entry found with name "+name+".")
}

// listReply answers a listing with the entries, each a JSON object, under key.
func listReply(key string, entries ...string) keystoneReply {
	return keystoneReply{status: http.StatusOK, body: fmt.Sprintf(`{%q:[%s]}`, key, strings.Join(entries, ","))}
}

// checkLookupSent confirms the create posted once and then sent the lookup.
func checkLookupSent(t *testing.T, k adoptKind, reqs []seenRequest) {
	t.Helper()
	want := []string{"POST " + k.path, "GET " + k.path}
	if got := routesOf(reqs); !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
}

// checkUnconfirmed confirms a create whose 409 the lookup did not confirm
// returns and records what it does without adoption: a zero resource and the
// POST's terminal 409, counted as one failed sample with error kind
// http_409.
func checkUnconfirmed(t *testing.T, k adoptKind, res resource.Resource, err error, m *metrics.Collector) {
	t.Helper()
	if !gophercloud.ResponseCodeIs(err, http.StatusConflict) {
		t.Fatalf("create error = %v, want the POST's 409", err)
	}
	if got := ErrorKind(err); got != "http_409" {
		t.Errorf("ErrorKind(%v) = %q, want http_409", err, got)
	}
	if IsRetryable(err) {
		t.Errorf("IsRetryable(%v) = true, want false for a 409", err)
	}
	if res != (resource.Resource{}) {
		t.Errorf("resource = %+v, want the zero resource", res)
	}
	agg := m.Aggregate(time.Second)
	if st := statsOf(agg, string(k.kind)); st.Attempted != 1 || st.Failed != 1 {
		t.Errorf("%s samples = %+v, want 1 attempted and 1 failed", k.kind, st)
	}
	if got := errorCount(agg, "http_409"); got != 1 {
		t.Errorf("http_409 samples = %d, want 1", got)
	}
}

// recordingHandler is a slog.Handler that keeps every record it handles, for
// a test that asserts what the client logs.
type recordingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

// records returns the records handled so far, in order.
func (h *recordingHandler) records() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.recs)
}

// withMessage returns the records handled so far whose message is msg.
func (h *recordingHandler) withMessage(msg string) []slog.Record {
	var out []slog.Record
	for _, r := range h.records() {
		if r.Message == msg {
			out = append(out, r)
		}
	}
	return out
}

// captureLogs installs a recordingHandler as the default logger for the rest
// of the test, in place of the discarding one TestMain installs, and restores
// the previous default in t.Cleanup.
func captureLogs(t *testing.T) *recordingHandler {
	t.Helper()
	prev := slog.Default()
	h := &recordingHandler{}
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// recordAttrs returns the attributes of r by key.
func recordAttrs(r slog.Record) map[string]slog.Value {
	attrs := map[string]slog.Value{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value
		return true
	})
	return attrs
}

// recordText renders r's message and attributes as text, as a log handler
// would write them.
func recordText(r slog.Record) string {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%s", a.Key, a.Value)
		return true
	})
	return b.String()
}

// TestCreateAdoptsOnConflict confirms a create answered 409 adopts this run's
// resource of that name: the lookup lists the kind filtered by the cloud
// name (and, for a user or project, by its domain), the create returns the
// listed id as a fresh create would return its own, and it records one
// successful create sample and no http_409. The lookup records no sample and
// sends no third request.
func TestCreateAdoptsOnConflict(t *testing.T) {
	for _, k := range adoptKinds {
		t.Run(string(k.kind), func(t *testing.T) {
			ts, seen := delegationKeystone(t, k.routes(conflictReply(k.name()), listReply(k.key, k.match(k.name()))))
			c, m, reader := telemetryTestClient(t, ts)

			got, err := k.create(context.Background(), c, "did1")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			want := resource.Resource{Kind: k.kind, Logical: k.logical, Name: k.name(), ID: foundID}
			if got != want {
				t.Errorf("create = %+v, want %+v", got, want)
			}

			reqs := seen()
			checkLookupSent(t, k, reqs)
			if len(reqs) != 2 {
				t.FailNow()
			}
			q, err := url.ParseQuery(reqs[1].rawQuery)
			if err != nil {
				t.Fatalf("lookup query %q: %v", reqs[1].rawQuery, err)
			}
			if got := q.Get("name"); got != k.name() {
				t.Errorf("lookup name = %q, want %q", got, k.name())
			}
			switch {
			case k.domain && q.Get("domain_id") != "did1":
				t.Errorf("lookup domain_id = %q, want did1", q.Get("domain_id"))
			case !k.domain && q.Has("domain_id"):
				t.Errorf("lookup query %q carries a domain_id, want none", reqs[1].rawQuery)
			}

			agg := m.Aggregate(time.Second)
			if st := statsOf(agg, string(k.kind)); st.Attempted != 1 || st.Succeeded != 1 {
				t.Errorf("%s samples = %+v, want 1 attempted and 1 succeeded", k.kind, st)
			}
			if got := errorCount(agg, "http_409"); got != 0 {
				t.Errorf("http_409 samples = %d, want 0", got)
			}
			wantOp := map[string]string{"kind": string(k.kind), "operation": "create", "outcome": "success"}
			if !hasOperationPoint(t, reader, wantOp) {
				t.Errorf("no dizzy.operation.duration point with %v", wantOp)
			}
		})
	}
}

// TestCreateSuccessSendsNoLookup confirms a create answered 201 returns the
// created id and sends no lookup.
func TestCreateSuccessSendsNoLookup(t *testing.T) {
	for _, k := range adoptKinds {
		t.Run(string(k.kind), func(t *testing.T) {
			single := strings.TrimSuffix(k.key, "s")
			created := keystoneReply{status: http.StatusCreated, body: fmt.Sprintf(`{%q:{"id":"new1","name":%q}}`, single, k.name())}
			ts, seen := delegationKeystone(t, map[string]keystoneReply{"POST " + k.path: created})
			c, _ := testClient(ts)

			got, err := k.create(context.Background(), c, "did1")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if got.ID != "new1" {
				t.Errorf("create id = %q, want new1", got.ID)
			}
			if routes := routesOf(seen()); !slices.Equal(routes, []string{"POST " + k.path}) {
				t.Errorf("requests = %v, want only the POST", routes)
			}
		})
	}
}

// TestCreateConflictEmptyLookup confirms a 409 whose lookup lists nothing
// stays the POST's terminal 409.
func TestCreateConflictEmptyLookup(t *testing.T) {
	for _, k := range adoptKinds {
		t.Run(string(k.kind), func(t *testing.T) {
			ts, seen := delegationKeystone(t, k.routes(conflictReply(k.name()), listReply(k.key)))
			c, m := testClient(ts)

			res, err := k.create(context.Background(), c, "did1")
			checkUnconfirmed(t, k, res, err, m)
			checkLookupSent(t, k, seen())
		})
	}
}

// TestCreateConflictAbsentList confirms a 409 whose lookup answers 200 with
// no list at all ({}) stays the POST's terminal 409, as for an empty list.
func TestCreateConflictAbsentList(t *testing.T) {
	for _, k := range adoptKinds {
		t.Run(string(k.kind), func(t *testing.T) {
			ts, seen := delegationKeystone(t, k.routes(conflictReply(k.name()), keystoneReply{status: http.StatusOK, body: `{}`}))
			c, m := testClient(ts)

			res, err := k.create(context.Background(), c, "did1")
			checkUnconfirmed(t, k, res, err, m)
			checkLookupSent(t, k, seen())
		})
	}
}

// TestCreateConflictForeignMatch confirms a 409 stays unconfirmed when the
// lookup lists only an entry that is not this run's resource of the name:
// another name, a user or project in another domain, a project without this
// run's tag, or a domain-scoped role. A server that ignores the list filter
// never widens the match.
func TestCreateConflictForeignMatch(t *testing.T) {
	type foreignCase struct {
		name  string
		k     adoptKind
		entry string
	}
	var cases []foreignCase
	for _, k := range adoptKinds {
		cases = append(cases, foreignCase{string(k.kind) + " with another name", k, k.match(resourceName("run0", "other-0001"))})
	}
	user := adoptKindOf(t, KindUser)
	project := adoptKindOf(t, KindProject)
	role := adoptKindOf(t, KindRole)
	cases = append(cases,
		foreignCase{"user in another domain", user,
			fmt.Sprintf(`{"id":%q,"name":%q,"domain_id":"did2"}`, foundID, user.name())},
		foreignCase{"project in another domain", project,
			fmt.Sprintf(`{"id":%q,"name":%q,"domain_id":"did2","tags":["dizzy:run=run0","dizzy:type=project"]}`, foundID, project.name())},
		foreignCase{"project of another run", project,
			fmt.Sprintf(`{"id":%q,"name":%q,"domain_id":"did1","tags":["dizzy:run=run1","dizzy:type=project"]}`, foundID, project.name())},
		foreignCase{"project without tags", project,
			fmt.Sprintf(`{"id":%q,"name":%q,"domain_id":"did1"}`, foundID, project.name())},
		foreignCase{"domain-scoped role", role,
			fmt.Sprintf(`{"id":%q,"name":%q,"domain_id":"did1"}`, foundID, role.name())},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, seen := delegationKeystone(t, tc.k.routes(conflictReply(tc.k.name()), listReply(tc.k.key, tc.entry)))
			c, m := testClient(ts)

			res, err := tc.k.create(context.Background(), c, "did1")
			checkUnconfirmed(t, tc.k, res, err, m)
			checkLookupSent(t, tc.k, seen())
		})
	}
}

// TestCreateConflictEmptyDomainID confirms a user or project create without
// a domain id never adopts on a 409, although the lookup lists an entry of
// that name that carries no domain id either.
func TestCreateConflictEmptyDomainID(t *testing.T) {
	entries := map[resource.Kind]func(name string) string{
		KindUser: func(name string) string {
			return fmt.Sprintf(`{"id":%q,"name":%q}`, foundID, name)
		},
		KindProject: func(name string) string {
			return fmt.Sprintf(`{"id":%q,"name":%q,"tags":["dizzy:run=run0","dizzy:type=project"]}`, foundID, name)
		},
	}
	for kind, entry := range entries {
		k := adoptKindOf(t, kind)
		t.Run(string(kind), func(t *testing.T) {
			ts, seen := delegationKeystone(t, k.routes(conflictReply(k.name()), listReply(k.key, entry(k.name()))))
			c, m := testClient(ts)

			res, err := k.create(context.Background(), c, "")
			checkUnconfirmed(t, k, res, err, m)
			checkLookupSent(t, k, seen())
		})
	}
}

// TestCreateConflictLookupFails confirms a lookup that fails leaves the
// create's result as the POST's 409, not the lookup's error: one failed
// http_409 sample, no sample of the lookup's status, and no ErrForbidden for
// a lookup denied with 403.
func TestCreateConflictLookupFails(t *testing.T) {
	for _, k := range adoptKinds {
		for _, status := range []int{http.StatusServiceUnavailable, http.StatusForbidden} {
			t.Run(fmt.Sprintf("%s/%d", k.kind, status), func(t *testing.T) {
				ts, seen := delegationKeystone(t, k.routes(conflictReply(k.name()), errorReply(status, "lookup refused")))
				c, m := testClient(ts)

				res, err := k.create(context.Background(), c, "did1")
				checkUnconfirmed(t, k, res, err, m)
				checkLookupSent(t, k, seen())
				if gophercloud.ResponseCodeIs(err, status) {
					t.Errorf("create error %v carries the lookup's %d, want only the POST's 409", err, status)
				}
				if errors.Is(err, ErrForbidden) {
					t.Errorf("errors.Is(%v, ErrForbidden) = true, want false", err)
				}
				agg := m.Aggregate(time.Second)
				for _, kind := range []string{"http_503", "http_403"} {
					if got := errorCount(agg, kind); got != 0 {
						t.Errorf("%s samples = %d, want 0", kind, got)
					}
				}
			})
		}
	}
}

// TestCreateConflictLookupTimesOut confirms a lookup still pending when the
// operation's deadline passes fails with context.DeadlineExceeded and leaves
// the create's result as the POST's 409, with no timeout sample: the POST
// answered at once, and the lookup records nothing.
func TestCreateConflictLookupTimesOut(t *testing.T) {
	for _, k := range adoptKinds {
		t.Run(string(k.kind), func(t *testing.T) {
			conflict := conflictReply(k.name())
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "POST " + k.path:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(conflict.status)
					_, _ = io.WriteString(w, conflict.body)
				case "GET " + k.path:
					// Block until the client gives up; the timer only bounds a
					// broken test.
					select {
					case <-r.Context().Done():
					case <-time.After(10 * time.Second):
					}
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer ts.Close()
			logs := captureLogs(t)
			c, m := testClient(ts)

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			res, err := k.create(ctx, c, "did1")
			checkUnconfirmed(t, k, res, err, m)
			if got := errorCount(m.Aggregate(time.Second), "timeout"); got != 0 {
				t.Errorf("timeout samples = %d, want 0", got)
			}

			warns := logs.withMessage(lookupFailedMsg)
			if len(warns) != 1 {
				t.Fatalf("%q records = %d, want 1", lookupFailedMsg, len(warns))
			}
			lookupErr, _ := recordAttrs(warns[0])["error"].Any().(error)
			if !errors.Is(lookupErr, context.DeadlineExceeded) {
				t.Errorf("lookup error = %v, want context.DeadlineExceeded", lookupErr)
			}
		})
	}
}

// TestCreateOtherErrorsSkipLookup confirms a create refused with anything but
// 409 sends no lookup and returns and records what it does today: the
// POST's status as its error kind, with a 403 wrapped with ErrForbidden.
func TestCreateOtherErrorsSkipLookup(t *testing.T) {
	statuses := []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError, http.StatusServiceUnavailable}
	for _, k := range adoptKinds {
		for _, status := range statuses {
			t.Run(fmt.Sprintf("%s/%d", k.kind, status), func(t *testing.T) {
				ts, seen := delegationKeystone(t, map[string]keystoneReply{"POST " + k.path: errorReply(status, "refused")})
				c, m := testClient(ts)

				res, err := k.create(context.Background(), c, "did1")
				if !gophercloud.ResponseCodeIs(err, status) {
					t.Fatalf("create error = %v, want the POST's %d", err, status)
				}
				wantKind := fmt.Sprintf("http_%d", status)
				if got := ErrorKind(err); got != wantKind {
					t.Errorf("ErrorKind(%v) = %q, want %q", err, got, wantKind)
				}
				if got, want := errors.Is(err, ErrForbidden), status == http.StatusForbidden; got != want {
					t.Errorf("errors.Is(%v, ErrForbidden) = %v, want %v", err, got, want)
				}
				if res != (resource.Resource{}) {
					t.Errorf("resource = %+v, want the zero resource", res)
				}
				if routes := routesOf(seen()); !slices.Equal(routes, []string{"POST " + k.path}) {
					t.Errorf("requests = %v, want only the POST", routes)
				}
				agg := m.Aggregate(time.Second)
				if st := statsOf(agg, string(k.kind)); st.Attempted != 1 || st.Failed != 1 {
					t.Errorf("%s samples = %+v, want 1 attempted and 1 failed", k.kind, st)
				}
				if got := errorCount(agg, wantKind); got != 1 {
					t.Errorf("%s samples = %d, want 1", wantKind, got)
				}
			})
		}
	}
}

// TestCreateUserAdoptsAfterLostAnswer replays the incident: Keystone keeps
// the user of the first create but the connection closes before the answer
// arrives, so the create fails with error kind other. The node's next create
// answers 409, adopts the kept user and returns its id, and the run counts
// one failed and one successful create and no http_409.
func TestCreateUserAdoptsAfterLostAnswer(t *testing.T) {
	const name = "dizzy-run0-user-0001"
	var (
		mu    sync.Mutex
		posts int
		kept  bool
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /users":
			_, _ = io.ReadAll(r.Body)
			posts++
			if posts == 1 {
				kept = true // Keystone stores the user, then the answer is lost.
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijacking the first create: %v", err)
					return
				}
				_ = conn.Close()
				return
			}
			reply := conflictReply(name)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(reply.status)
			_, _ = io.WriteString(w, reply.body)
		case "GET /users":
			body := `{"users":[]}`
			if kept {
				body = `{"users":[{"id":"uid-kept","name":"` + name + `","domain_id":"did1"}]}`
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()
	c, m := testClient(ts)
	u := keystoneplan.User{Name: "user-0001"}

	_, err := c.CreateUser(context.Background(), u, "did1", "user-pw")
	if err == nil {
		t.Fatal("first CreateUser succeeded, want the lost answer to fail it")
	}
	if got := ErrorKind(err); got != "other" {
		t.Errorf("first CreateUser ErrorKind(%v) = %q, want other", err, got)
	}

	got, err := c.CreateUser(context.Background(), u, "did1", "user-pw")
	if err != nil {
		t.Fatalf("second CreateUser: %v", err)
	}
	want := resource.Resource{Kind: KindUser, Logical: "user-0001", Name: name, ID: "uid-kept"}
	if got != want {
		t.Errorf("second CreateUser = %+v, want %+v", got, want)
	}

	agg := m.Aggregate(time.Second)
	if st := statsOf(agg, string(KindUser)); st.Attempted != 2 || st.Succeeded != 1 || st.Failed != 1 {
		t.Errorf("user samples = %+v, want 2 attempted, 1 succeeded and 1 failed", st)
	}
	if got := errorCount(agg, "http_409"); got != 0 {
		t.Errorf("http_409 samples = %d, want 0", got)
	}
}

// TestAdoptOnConflictLogs confirms what the fold decision logs for a 409: an
// adoption one INFO record with the kind, name and adopted id, a failed
// lookup one WARN record with the kind, name and lookup error, and an empty
// lookup nothing.
func TestAdoptOnConflictLogs(t *testing.T) {
	const name = "dizzy-run0-user-0001"
	lookupErr := errors.New(`listing users named "dizzy-run0-user-0001": boom`)
	cases := []struct {
		name      string
		found     string
		findErr   error
		wantOK    bool
		wantLevel slog.Level
		wantMsg   string
		wantAttrs map[string]any
	}{
		{
			name: "adoption", found: "uid1", wantOK: true,
			wantLevel: slog.LevelInfo, wantMsg: "already created; treating a refused create as success",
			wantAttrs: map[string]any{"kind": "user", "name": name, "id": "uid1"},
		},
		{
			name: "failed lookup", findErr: lookupErr,
			wantLevel: slog.LevelWarn, wantMsg: lookupFailedMsg,
			wantAttrs: map[string]any{"kind": "user", "name": name, "error": lookupErr},
		},
		{name: "empty lookup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			var id string
			applied := adoptOnConflict(KindUser, name, &id, func(context.Context) (string, error) {
				return tc.found, tc.findErr
			})

			if got := applied(context.Background(), respErr(http.StatusConflict, "duplicate")); got != tc.wantOK {
				t.Errorf("applied = %v, want %v", got, tc.wantOK)
			}
			if id != tc.found {
				t.Errorf("id = %q, want %q", id, tc.found)
			}
			recs := logs.records()
			if tc.wantMsg == "" {
				if len(recs) != 0 {
					t.Errorf("records = %d (first %q), want none", len(recs), recs[0].Message)
				}
				return
			}
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			r := recs[0]
			if r.Level != tc.wantLevel || r.Message != tc.wantMsg {
				t.Errorf("record = %s %q, want %s %q", r.Level, r.Message, tc.wantLevel, tc.wantMsg)
			}
			attrs := recordAttrs(r)
			if len(attrs) != len(tc.wantAttrs) {
				t.Errorf("record attrs = %v, want exactly %v", attrs, tc.wantAttrs)
			}
			for k, want := range tc.wantAttrs {
				if got, ok := attrs[k]; !ok || got.Any() != want {
					t.Errorf("record attr %s = %v, want %v", k, got, want)
				}
			}
		})
	}
}
