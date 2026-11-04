package nova

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// testComputeClient builds a Client whose Nova (compute) service calls hit ts.
// Only the compute-backed lifecycle ops are exercised here, so the network and
// block-storage clients are left nil.
func testComputeClient(ts *httptest.Server) *Client {
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	return New(gc, nil, nil, "run0", metrics.NewCollector())
}

// conflictActionServer answers every server-action POST with a 409 carrying
// body, the shape a retried lifecycle op hits once its first request already
// committed and moved the instance out of its prior state.
func conflictActionServer(t *testing.T, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/action") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(body))
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
}

// TestLifecycleOpsTolerateCommittedConflict verifies the async lifecycle ops
// treat a retried 409 that names their settled target state as success — a
// committed-then-lost first attempt must not fail the run once the retry sees
// the instance already transitioned — while a genuine non-target 409 (an ERROR
// state) still surfaces as an error.
func TestLifecycleOpsTolerateCommittedConflict(t *testing.T) {
	srv := resource.Resource{Kind: KindServer, ID: "srv-1", Logical: "srv-0001"}

	tolerated := []struct {
		name string
		body string
		call func(*Client) error
	}{
		{
			name: "resize already in verify-resize",
			body: `{"conflictingRequest":{"message":"Cannot 'resize' instance srv-1 while it is in vm_state resized"}}`,
			call: func(c *Client) error { return c.ResizeServer(context.Background(), srv, "flavor-2") },
		},
		{
			name: "confirm-resize already active",
			body: `{"conflictingRequest":{"message":"Cannot 'confirmResize' instance srv-1 while it is in vm_state active"}}`,
			call: func(c *Client) error { return c.ConfirmResizeServer(context.Background(), srv) },
		},
		{
			name: "live-migrate already migrating",
			body: `{"conflictingRequest":{"message":"Cannot 'os-migrateLive' instance srv-1 while it is in task_state migrating"}}`,
			call: func(c *Client) error { return c.LiveMigrateServer(context.Background(), srv) },
		},
	}
	for _, tc := range tolerated {
		t.Run(tc.name, func(t *testing.T) {
			ts := conflictActionServer(t, tc.body)
			defer ts.Close()
			if err := tc.call(testComputeClient(ts)); err != nil {
				t.Errorf("op = %v, want nil (a committed-then-retried 409 is success)", err)
			}
		})
	}

	// A resize that 409s because the instance is in ERROR is a real failure and
	// must not be swallowed: the body carries the 'resize' action verb but not
	// the target vm_state word "resized", so the tolerance must not match.
	t.Run("resize error state is not tolerated", func(t *testing.T) {
		ts := conflictActionServer(t, `{"conflictingRequest":{"message":"Cannot 'resize' instance srv-1 while it is in vm_state error"}}`)
		defer ts.Close()
		if err := testComputeClient(ts).ResizeServer(context.Background(), srv, "flavor-2"); err == nil {
			t.Error("ResizeServer on an ERROR-state 409 = nil, want an error")
		}
	})
}

// TestColdMigrateServer verifies the cold-migrate action body, that a retried
// 409 naming a resize task state is success, and that every other failure
// surfaces wrapped with the logical name.
func TestColdMigrateServer(t *testing.T) {
	srv := resource.Resource{Kind: KindServer, ID: "srv-1", Logical: "srv-0001"}

	t.Run("accepted", func(t *testing.T) {
		var body string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/servers/srv-1/action" {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			data, _ := io.ReadAll(r.Body)
			body = string(data)
			w.WriteHeader(http.StatusAccepted)
		}))
		defer ts.Close()
		if err := testComputeClient(ts).ColdMigrateServer(context.Background(), srv); err != nil {
			t.Fatalf("ColdMigrateServer = %v, want nil", err)
		}
		if body != `{"migrate":null}` {
			t.Errorf("posted body = %s, want {\"migrate\":null}", body)
		}
	})

	t.Run("already migrating", func(t *testing.T) {
		ts := conflictActionServer(t, `{"conflictingRequest":{"message":"Cannot 'migrate' instance srv-1 while it is in task_state resize_migrating"}}`)
		defer ts.Close()
		if err := testComputeClient(ts).ColdMigrateServer(context.Background(), srv); err != nil {
			t.Errorf("ColdMigrateServer = %v, want nil (a committed-then-retried 409 is success)", err)
		}
	})

	t.Run("error state is not tolerated", func(t *testing.T) {
		ts := conflictActionServer(t, `{"conflictingRequest":{"message":"Cannot 'migrate' instance srv-1 while it is in vm_state error"}}`)
		defer ts.Close()
		err := testComputeClient(ts).ColdMigrateServer(context.Background(), srv)
		if want := `cold-migrating server "srv-0001":`; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("ColdMigrateServer = %v, want an error starting with %q", err, want)
		}
	})

	t.Run("forbidden", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer ts.Close()
		err := testComputeClient(ts).ColdMigrateServer(context.Background(), srv)
		if want := `cold-migrating server "srv-0001":`; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("ColdMigrateServer = %v, want an error starting with %q", err, want)
		}
		var code gophercloud.ErrUnexpectedResponseCode
		if !errors.As(err, &code) || code.Actual != http.StatusForbidden {
			t.Errorf("ColdMigrateServer = %v, want it to wrap the gophercloud 403", err)
		}
	})
}

// TestIsSameFlavor verifies Nova's 400 for a resize to the flavor the server
// already has surfaces from ResizeServer as an error IsSameFlavor recognizes,
// and that no other rejection does.
func TestIsSameFlavor(t *testing.T) {
	srv := resource.Resource{Kind: KindServer, ID: "srv-1", Logical: "srv-0001"}
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"resize to the current flavor", http.StatusBadRequest, `{"badRequest":{"code":400,"message":"When resizing, instances must change flavor!"}}`, true},
		{"another bad request", http.StatusBadRequest, `{"badRequest":{"code":400,"message":"Invalid flavorRef provided."}}`, false},
		{"a conflict", http.StatusConflict, `{"conflictingRequest":{"message":"Cannot 'resize' instance srv-1 while it is in vm_state error"}}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			err := testComputeClient(ts).ResizeServer(context.Background(), srv, "flavor-2")
			if err == nil {
				t.Fatal("ResizeServer = nil, want the rejection")
			}
			if got := IsSameFlavor(err); got != tc.want {
				t.Errorf("IsSameFlavor(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
	if IsSameFlavor(nil) {
		t.Error("IsSameFlavor(nil) = true, want false")
	}
}

// TestCreateServerSchedulerHint verifies a server with a group id boots with
// the group scheduler hint, a server without one with a body free of any
// hint, and that a group id that is not a UUID fails before any request.
func TestCreateServerSchedulerHint(t *testing.T) {
	const groupID = "3f2a8c1e-5b7d-4e9a-8c6f-1a2b3c4d5e6f"
	srv := novaplan.Server{Name: "srv-0001", Networks: []string{"net-0001"}}
	boot := BootSpec{ImageID: "img-1", FlavorID: "flv-1", NetworkIDs: []string{"net-id-1"}}

	tests := []struct {
		name     string
		groupID  string
		wantHint bool
	}{
		{name: "with group", groupID: groupID, wantHint: true},
		{name: "without group"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/servers" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				data, _ := io.ReadAll(r.Body)
				body = string(data)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"server":{"id":"srv-id-1"}}`))
			}))
			defer ts.Close()

			b := boot
			b.GroupID = tc.groupID
			if _, err := testComputeClient(ts).CreateServer(context.Background(), srv, b); err != nil {
				t.Fatalf("CreateServer = %v, want nil", err)
			}
			hint := `"os:scheduler_hints":{"group":"` + groupID + `"}`
			if got := strings.Contains(body, hint); got != tc.wantHint {
				t.Errorf("body %s carries %s = %v, want %v", body, hint, got, tc.wantHint)
			}
			if !tc.wantHint && strings.Contains(body, "os:scheduler_hints") {
				t.Errorf("body %s carries os:scheduler_hints, want none", body)
			}
		})
	}

	t.Run("group id not a UUID", func(t *testing.T) {
		requests := 0
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer ts.Close()

		b := boot
		b.GroupID = "not-a-uuid"
		_, err := testComputeClient(ts).CreateServer(context.Background(), srv, b)
		if want := `creating server "srv-0001":`; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("CreateServer = %v, want an error starting with %q", err, want)
		}
		var invalid gophercloud.ErrInvalidInput
		if !errors.As(err, &invalid) {
			t.Errorf("CreateServer = %v, want it to wrap gophercloud.ErrInvalidInput", err)
		}
		if requests != 0 {
			t.Errorf("CreateServer sent %d requests, want 0", requests)
		}
	})
}

// createAPI names what a lostAnswerFake answers for one resource kind: the
// create and list paths, the JSON keys of one resource and of a page, the id
// prefix and status of a resource it records, and the creation-time field in
// the service's own format.
type createAPI struct {
	create, list string
	one, many    string
	prefix       string
	status       string
	createdKey   string
	createdAt    string
}

// novaServers is the createAPI of Nova servers.
var novaServers = createAPI{
	create: "/servers", list: "/servers/detail", one: "server", many: "servers",
	prefix: "srv", status: "BUILD", createdKey: "created", createdAt: "2026-01-01T00:00:00Z",
}

// rec returns a resource record named name that carries md, in the shape a list
// answers with. A nil md omits the metadata key, as a resource created without
// metadata lists.
func (api createAPI) rec(id, name string, md map[string]string) map[string]any {
	r := map[string]any{"id": id, "name": name, "status": api.status, api.createdKey: api.createdAt}
	if md != nil {
		r["metadata"] = md
	}
	return r
}

// createAnswer is how a lostAnswerFake answers one create request.
type createAnswer struct {
	record bool   // record the resource first, as a service that committed it
	status int    // the status of the answer; 0 closes the connection instead
	body   string // the body of a refusal
	hold   bool   // hold the request until the client gives up, recording nothing
}

// The answers the lost-answer tests script. answerAccepted is what a create
// whose token the script does not name gets.
var (
	answerAccepted        = createAnswer{record: true, status: http.StatusAccepted}
	answerLost            = createAnswer{}
	answerLostAfterCommit = createAnswer{record: true}
	answer503AfterCommit  = createAnswer{record: true, status: http.StatusServiceUnavailable}
	answerHeld            = createAnswer{hold: true}
)

// lostAnswerFake is an httptest Nova or Cinder for the create and list calls of
// one resource kind. A create is answered by answers under the dizzy:create
// token it carries, so a script does not depend on when a held request reaches
// the fake. A create without a token, as a client that stamps none sends, is
// answered under its arrival number, so the script loses its answer just the
// same. A create the script does not name gets answerAccepted. A list is
// answered with the next status of lists, the last one repeating and none
// meaning 200, and a 200 holds every record whatever its name, so the
// client-side checks decide what matches.
type lostAnswerFake struct {
	t       *testing.T
	api     createAPI
	answers map[string]createAnswer
	lists   []int
	ts      *httptest.Server

	mu      sync.Mutex
	records []map[string]any // what a 200 list answers with, in list order
	bodies  []string         // the body of every create request, in arrival order
	names   []string         // the name query of every list request
	calls   []string         // "create <token>" and "list" since the last take
	nlist   int
}

// newLostAnswerFake starts a lostAnswerFake for api that the test closes when
// it ends.
func newLostAnswerFake(t *testing.T, api createAPI, answers map[string]createAnswer, lists ...int) *lostAnswerFake {
	t.Helper()
	f := &lostAnswerFake{t: t, api: api, answers: answers, lists: lists, records: []map[string]any{}}
	f.ts = httptest.NewServer(f)
	t.Cleanup(f.ts.Close)
	return f
}

// ServeHTTP answers a create by its token's script entry and a list by the
// next list status, and fails the test on any other request.
func (f *lostAnswerFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == f.api.create:
		body, _ := io.ReadAll(r.Body)
		name, md := f.decode(body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(body))
		token := md["dizzy:create"]
		f.calls = append(f.calls, "create "+token)
		key := token
		if key == "" {
			key = strconv.Itoa(len(f.bodies))
		}
		answer, ok := f.answers[key]
		if !ok {
			answer = answerAccepted
		}
		id := fmt.Sprintf("%s-%d", f.api.prefix, len(f.bodies))
		if answer.record {
			f.records = append(f.records, f.api.rec(id, name, md))
		}
		f.mu.Unlock()

		switch {
		case answer.hold:
			<-r.Context().Done()
		case answer.status == 0:
			loseAnswer(f.t, w)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(answer.status)
			if answer.status == http.StatusAccepted {
				_, _ = fmt.Fprintf(w, `{%q:{"id":%q}}`, f.api.one, id)
				return
			}
			_, _ = w.Write([]byte(answer.body))
		}
	case r.Method == http.MethodGet && r.URL.Path == f.api.list:
		f.mu.Lock()
		status := http.StatusOK
		if len(f.lists) > 0 {
			status = f.lists[min(f.nlist, len(f.lists)-1)]
		}
		f.nlist++
		f.names = append(f.names, r.URL.Query().Get("name"))
		f.calls = append(f.calls, "list")
		page, err := json.Marshal(map[string]any{f.api.many: f.records})
		f.mu.Unlock()
		if err != nil {
			f.t.Errorf("encoding the list: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write(page)
		}
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// decode returns the name and metadata a create request body carries.
func (f *lostAnswerFake) decode(body []byte) (string, map[string]string) {
	var req map[string]struct {
		Name     string            `json:"name"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		f.t.Errorf("decoding the create body %s: %v", body, err)
	}
	return req[f.api.one].Name, req[f.api.one].Metadata
}

// add records resources the cloud holds beside what the creates recorded, such
// as a create the service committed after its lookup.
func (f *lostAnswerFake) add(records ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, records...)
}

// body returns the body of the i-th create request.
func (f *lostAnswerFake) body(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}

// sent returns the metadata of the i-th create request.
func (f *lostAnswerFake) sent(i int) map[string]string {
	_, md := f.decode([]byte(f.body(i)))
	return md
}

// listNames returns the name query of every list request.
func (f *lostAnswerFake) listNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.names)
}

// take returns the calls since the last take and forgets them.
func (f *lostAnswerFake) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

// wantCalls fails t unless the calls since the last take are want.
func (f *lostAnswerFake) wantCalls(t *testing.T, want ...string) {
	t.Helper()
	if got := f.take(); !slices.Equal(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

// loseAnswer hijacks the connection of w and closes it without an answer, which
// gophercloud reports as a *url.Error wrapping io.EOF.
func loseAnswer(t *testing.T, w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Error("the response writer cannot hijack its connection")
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Errorf("hijacking the connection: %v", err)
		return
	}
	_ = conn.Close()
}

// captureLog sends the process-wide default logger to the returned buffer until
// the test ends. A test that calls it must not run in parallel.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// TestCreateServerAdoptsLostAnswer verifies that a server create whose answer
// may have been lost looks the server up by name, run and create token, right
// after the failure and before the next create of the name, and adopts a match
// without posting again; that otherwise the POST's error is returned; that a
// refusal or a request never sent looks nothing up; and that every request
// carries its own token. The fake names each create by the token it carries.
// The subtests run serially because some of them read the process-wide default
// logger.
func TestCreateServerAdoptsLostAnswer(t *testing.T) {
	const name = "dizzy-run0-srv-0001"
	srv := novaplan.Server{Name: "srv-0001", Networks: []string{"net-0001"}}
	boot := BootSpec{ImageID: "img-1", FlavorID: "flv-1", NetworkIDs: []string{"net-id-1"}}
	adopted := func(id string) resource.Resource {
		return resource.Resource{Kind: "server", Logical: "srv-0001", Name: name, ID: id}
	}
	create := func(c *Client) (resource.Resource, error) {
		return c.CreateServer(context.Background(), srv, boot)
	}
	md := func(run, token string) map[string]string {
		return map[string]string{"dizzy:run": run, "dizzy:type": "server", "dizzy:create": token}
	}

	for _, tc := range []struct {
		name   string
		answer createAnswer
	}{
		{"answer lost after commit", answerLostAfterCommit},
		{"5xx after commit", answer503AfterCommit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLostAnswerFake(t, novaServers, map[string]createAnswer{"1": tc.answer})
			got, err := create(testComputeClient(f.ts))
			if err != nil || got != adopted("srv-1") {
				t.Fatalf("CreateServer = %+v, %v, want %+v, nil", got, err, adopted("srv-1"))
			}
			f.wantCalls(t, "create 1", "list")
			if names := f.listNames(); !slices.Equal(names, []string{"^dizzy-run0-srv-0001$"}) {
				t.Errorf("list name queries = %q, want [^dizzy-run0-srv-0001$]", names)
			}
		})
	}

	t.Run("answer lost, nothing created", func(t *testing.T) {
		f := newLostAnswerFake(t, novaServers, map[string]createAnswer{"1": answerLost})
		c := testComputeClient(f.ts)
		_, err := create(c)
		if want := `creating server "srv-0001":`; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("CreateServer = %v, want an error starting with %q", err, want)
		}
		if !errors.Is(err, io.EOF) {
			t.Errorf("CreateServer = %v, want it to wrap io.EOF", err)
		}
		f.wantCalls(t, "create 1", "list")

		if _, err := create(c); err != nil {
			t.Fatalf("second CreateServer = %v, want nil", err)
		}
		f.wantCalls(t, "list", "create 2")
		if _, err := create(c); err != nil {
			t.Fatalf("third CreateServer = %v, want nil", err)
		}
		f.wantCalls(t, "create 3")
	})

	t.Run("late commit", func(t *testing.T) {
		f := newLostAnswerFake(t, novaServers, map[string]createAnswer{"1": answerLost})
		c := testComputeClient(f.ts)
		if _, err := create(c); err == nil {
			t.Fatal("CreateServer = nil, want the lost answer's error")
		}
		f.wantCalls(t, "create 1", "list")

		f.add(novaServers.rec("srv-late", name, f.sent(0)))
		got, err := create(c)
		if err != nil || got != adopted("srv-late") {
			t.Fatalf("second CreateServer = %+v, %v, want %+v, nil", got, err, adopted("srv-late"))
		}
		f.wantCalls(t, "list")
	})

	t.Run("pending lookup fails", func(t *testing.T) {
		f := newLostAnswerFake(t, novaServers, map[string]createAnswer{"1": answerLost},
			http.StatusOK, http.StatusServiceUnavailable, http.StatusOK)
		c := testComputeClient(f.ts)
		if _, err := create(c); err == nil {
			t.Fatal("CreateServer = nil, want the lost answer's error")
		}
		f.wantCalls(t, "create 1", "list")

		_, err := create(c)
		if want := "looking up an earlier create whose answer was lost"; err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("second CreateServer = %v, want an error containing %q", err, want)
		}
		if !IsRetryable(err) {
			t.Errorf("IsRetryable(%v) = false, want true", err)
		}
		f.wantCalls(t, "list")

		if _, err := create(c); err != nil {
			t.Fatalf("third CreateServer = %v, want nil", err)
		}
		f.wantCalls(t, "list", "create 2")
	})

	t.Run("lookup after the failure fails", func(t *testing.T) {
		logs := captureLog(t)
		f := newLostAnswerFake(t, novaServers, map[string]createAnswer{"1": answerLost},
			http.StatusServiceUnavailable, http.StatusOK)
		c := testComputeClient(f.ts)
		_, err := create(c)
		if !errors.Is(err, io.EOF) || httpStatus(err) != 0 {
			t.Fatalf("CreateServer = %v, want the POST's io.EOF, not the list's 503", err)
		}
		f.wantCalls(t, "create 1", "list")
		if want := `lookup="listing servers named dizzy-run0-srv-0001: `; !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %s:\n%s", want, logs.String())
		}

		if _, err := create(c); err != nil {
			t.Fatalf("second CreateServer = %v, want nil", err)
		}
		f.wantCalls(t, "list", "create 2")
	})

	t.Run("no lookup when nothing was sent or Nova refused", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			answer  createAnswer
			groupID string
			sent    []string // the first call's calls
			check   func(error) bool
		}{
			{
				name:   "quota 403",
				answer: createAnswer{status: http.StatusForbidden, body: `{"forbidden":{"message":"Quota exceeded for instances"}}`},
				sent:   []string{"create 1"},
				check:  func(err error) bool { return errors.Is(err, ErrQuota) },
			},
			{
				name:   "bad request 400",
				answer: createAnswer{status: http.StatusBadRequest, body: `{"badRequest":{"code":400,"message":"Invalid flavorRef provided."}}`},
				sent:   []string{"create 1"},
				check:  func(err error) bool { return httpStatus(err) == http.StatusBadRequest },
			},
			{
				name:    "group id not a UUID",
				groupID: "not-a-uuid",
				check: func(err error) bool {
					var invalid gophercloud.ErrInvalidInput
					return errors.As(err, &invalid)
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newLostAnswerFake(t, novaServers, map[string]createAnswer{"1": tc.answer})
				c := testComputeClient(f.ts)
				b := boot
				b.GroupID = tc.groupID
				_, err := c.CreateServer(context.Background(), srv, b)
				if want := `creating server "srv-0001":`; err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Fatalf("CreateServer = %v, want an error starting with %q", err, want)
				}
				if !tc.check(err) {
					t.Errorf("CreateServer = %v, want the %s rejection", err, tc.name)
				}
				f.wantCalls(t, tc.sent...)

				if _, err := create(c); err != nil {
					t.Fatalf("second CreateServer = %v, want nil", err)
				}
				f.wantCalls(t, "create 2")
			})
		}
	})

	t.Run("the token tells instances apart", func(t *testing.T) {
		f := newLostAnswerFake(t, novaServers, map[string]createAnswer{"1": answerLost})
		deleting := novaServers.rec("srv-deleting", name, md("run0", "1"))
		deleting["status"] = "ACTIVE"
		deleting["OS-EXT-STS:task_state"] = "deleting"
		f.add(
			novaServers.rec("srv-other-token", name, md("run0", "7")),
			novaServers.rec("srv-other-run", name, md("other", "1")),
			novaServers.rec("srv-no-metadata", name, nil),
			deleting,
		)
		got, err := create(testComputeClient(f.ts))
		if !errors.Is(err, io.EOF) || got != (resource.Resource{}) {
			t.Fatalf("CreateServer = %+v, %v, want no adoption and the POST's io.EOF", got, err)
		}
		f.wantCalls(t, "create 1", "list")
	})

	t.Run("several matches", func(t *testing.T) {
		logs := captureLog(t)
		f := newLostAnswerFake(t, novaServers, nil)
		newer := novaServers.rec("srv-new", name, md("run0", "2"))
		newer["created"] = "2026-01-01T00:01:00Z"
		f.add(novaServers.rec("srv-old", name, md("run0", "1")), newer)
		c := testComputeClient(f.ts)
		c.addPending(KindServer, name, "1")
		c.addPending(KindServer, name, "2")

		got, err := create(c)
		if err != nil || got != adopted("srv-new") {
			t.Fatalf("CreateServer = %+v, %v, want %+v, nil", got, err, adopted("srv-new"))
		}
		f.wantCalls(t, "list")
		if want := `name=dizzy-run0-srv-0001 ids=[srv-old]`; !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %s:\n%s", want, logs.String())
		}
	})

	t.Run("expired context", func(t *testing.T) {
		f := newLostAnswerFake(t, novaServers, map[string]createAnswer{"1": answerHeld})
		c := testComputeClient(f.ts)
		logs := captureLog(t)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := c.CreateServer(ctx, srv, boot); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("CreateServer = %v, want context.DeadlineExceeded", err)
		}
		// The held request can reach the fake after the client gave up, so this
		// call is checked for the list it must not issue, and the held create is
		// left out of the next call's calls.
		if calls := f.take(); slices.Contains(calls, "list") {
			t.Errorf("calls = %q, want no list", calls)
		}
		// A lookup on the expired context fails before it reaches the wire, so
		// the warning, which carries a failed lookup's error, shows none ran.
		if got := logs.String(); !strings.Contains(got, "create failed; a server with this name may be orphaned") || strings.Contains(got, "lookup=") {
			t.Errorf("log = %q, want the create warning without a lookup error", got)
		}

		if _, err := create(c); err != nil {
			t.Fatalf("second CreateServer = %v, want nil", err)
		}
		calls := slices.DeleteFunc(f.take(), func(call string) bool { return call == "create 1" })
		if want := []string{"list", "create 2"}; !slices.Equal(calls, want) {
			t.Errorf("calls = %q, want %q", calls, want)
		}
	})

	t.Run("every successful POST carries the create token", func(t *testing.T) {
		f := newLostAnswerFake(t, novaServers, nil)
		c := testComputeClient(f.ts)
		for range 2 {
			if _, err := create(c); err != nil {
				t.Fatalf("CreateServer = %v, want nil", err)
			}
		}
		f.wantCalls(t, "create 1", "create 2")
		for i, token := range []string{"1", "2"} {
			if got, want := f.sent(i), md("run0", token); !maps.Equal(got, want) {
				t.Errorf("request %d metadata = %v, want %v", i+1, got, want)
			}
			if want := `"dizzy:create":"` + token + `"`; !strings.Contains(f.body(i), want) {
				t.Errorf("request %d body %s lacks %s", i+1, f.body(i), want)
			}
		}
	})

	t.Run("adoption logs one info line", func(t *testing.T) {
		logs := captureLog(t)
		f := newLostAnswerFake(t, novaServers, map[string]createAnswer{"1": answerLostAfterCommit})
		if _, err := create(testComputeClient(f.ts)); err != nil {
			t.Fatalf("CreateServer = %v, want nil", err)
		}
		line := `level=INFO msg="adopted a server whose create answer was lost" name=dizzy-run0-srv-0001 id=srv-1`
		if n := strings.Count(logs.String(), line); n != 1 {
			t.Errorf("logs carry %s %d times, want once:\n%s", line, n, logs.String())
		}
		if n := strings.Count(logs.String(), "level="); n != 1 {
			t.Errorf("logs carry %d lines, want only the info line:\n%s", n, logs.String())
		}
	})
}
