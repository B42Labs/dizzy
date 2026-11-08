package octavia

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Paths and names of the listener the adoption tests create.
const (
	listenersPath = "/v2.0/lbaas/listeners"
	listenerName  = "dizzy-run0-lsn-0001"
	listenerQuery = "loadbalancer_id=lb-id-1&name=dizzy-run0-lsn-0001&tags=dizzy%3Arun%3Drun0"
)

// planListener is the plan entry of the listener the adoption tests create.
var planListener = plan.Listener{Name: "lsn-0001", LoadBalancer: "lb-0001", Port: 8080}

// listenerTags are the run tags a listener of run0 carries.
var listenerTags = []string{"dizzy:run=run0", "dizzy:type=listener"}

// listenerList renders a listener list answer holding items.
func listenerList(items ...string) string {
	return `{"listeners":[` + strings.Join(items, ",") + `]}`
}

// adoptedListener is the Resource of the listener lsn-0001 with id.
func adoptedListener(id string) resource.Resource {
	return resource.Resource{Kind: KindListener, Logical: "lsn-0001", Name: listenerName, ID: id}
}

// isPending reports whether c holds a pending create of the listener lsn-0001.
func isPending(c *Client) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.pending[string(KindListener)+"/"+listenerName]
	return ok
}

// TestCreateAdoptsListenerAfter503 confirms a listener create answered 503,
// whose lookup lists a matching tagged listener, returns that listener after
// one POST, logs the adoption, and leaves nothing pending.
func TestCreateAdoptsListenerAfter503(t *testing.T) {
	logs := captureLog(t)
	f := newFake(t, scripted(t, map[string][]reply{
		"POST " + listenersPath: {{http.StatusServiceUnavailable, `{"faultstring":"unavailable"}`}},
		"GET " + listenersPath:  {{http.StatusOK, listenerList(item("lsn-id-1", listenerName, "PENDING_CREATE", listenerTags...))}},
	}))
	c, m := f.client()

	got, err := c.CreateListener(context.Background(), planListener, "lb-id-1")
	if err != nil {
		t.Fatalf("CreateListener: %v", err)
	}
	if want := adoptedListener("lsn-id-1"); got != want {
		t.Errorf("CreateListener = %+v, want %+v", got, want)
	}
	f.wantLines(t, "POST "+listenersPath, "GET "+listenersPath+"?"+listenerQuery)
	if !strings.Contains(logs.String(), "adopted an octavia resource whose create answer was lost") {
		t.Errorf("log %q does not report the adoption", logs.String())
	}
	if isPending(c) {
		t.Error("an adopted listener is still pending")
	}
	agg := m.Aggregate(time.Second)
	if s := statsOf(agg, "listener"); s.Attempted != 2 || s.Failed != 1 {
		t.Errorf("listener stats = %+v, want the failed create and the lookup", s)
	}
}

// TestCreateKeepsLostListenerPending confirms a 503 whose lookup finds nothing
// returns the POST's retryable error and leaves the name pending, and that the
// next create of that name lists first and adopts a late match without
// posting.
func TestCreateKeepsLostListenerPending(t *testing.T) {
	logs := captureLog(t)
	f := newFake(t, scripted(t, map[string][]reply{
		"POST " + listenersPath: {{http.StatusServiceUnavailable, `{}`}},
		"GET " + listenersPath: {
			{http.StatusOK, listenerList()},
			{http.StatusOK, listenerList(item("lsn-id-late", listenerName, "ACTIVE", listenerTags...))},
		},
	}))
	c, _ := f.client()

	_, err := c.CreateListener(context.Background(), planListener, "lb-id-1")
	if err == nil || !strings.HasPrefix(err.Error(), `creating listener "lsn-0001": `) {
		t.Fatalf("CreateListener = %v, want the wrapped POST error", err)
	}
	if httpStatus(err) != http.StatusServiceUnavailable || !IsRetryable(err) {
		t.Errorf("CreateListener error = %v, want the POST's retryable 503", err)
	}
	if !isPending(c) {
		t.Fatal("a lost create whose lookup found nothing is not pending")
	}
	if !strings.Contains(logs.String(), "create failed; an octavia resource with this name may be orphaned in the cloud") {
		t.Errorf("log %q does not warn of the possible orphan", logs.String())
	}

	got, err := c.CreateListener(context.Background(), planListener, "lb-id-1")
	if err != nil {
		t.Fatalf("second CreateListener: %v", err)
	}
	if want := adoptedListener("lsn-id-late"); got != want {
		t.Errorf("second CreateListener = %+v, want %+v", got, want)
	}
	f.wantLines(t,
		"POST "+listenersPath,
		"GET "+listenersPath+"?"+listenerQuery,
		"GET "+listenersPath+"?"+listenerQuery)
	if isPending(c) {
		t.Error("an adopted listener is still pending")
	}
}

// TestCreatePendingLookupFailureDoesNotPost confirms a pending lookup answered
// 500 fails with an error wrapping "looking up an earlier create whose answer
// was lost" without posting, stays retryable, and keeps the name pending, so
// the next create looks it up again before it posts.
func TestCreatePendingLookupFailureDoesNotPost(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{
		"POST " + listenersPath: {
			{http.StatusServiceUnavailable, `{}`},
			{http.StatusCreated, `{"listener":` + item("lsn-id-2", listenerName, "PENDING_CREATE", listenerTags...) + `}`},
		},
		"GET " + listenersPath: {
			{http.StatusOK, listenerList()},
			{http.StatusInternalServerError, `{"faultstring":"boom"}`},
			{http.StatusOK, listenerList()},
		},
	}))
	c, _ := f.client()

	if _, err := c.CreateListener(context.Background(), planListener, "lb-id-1"); err == nil {
		t.Fatal("first CreateListener = nil, want the 503")
	}

	_, err := c.CreateListener(context.Background(), planListener, "lb-id-1")
	if err == nil || !strings.Contains(err.Error(), "looking up an earlier create whose answer was lost") {
		t.Fatalf("CreateListener = %v, want the lookup error", err)
	}
	if httpStatus(err) != http.StatusInternalServerError || !IsRetryable(err) {
		t.Errorf("lookup error = %v, want the list's retryable 500", err)
	}
	if !isPending(c) {
		t.Error("a failed pending lookup dropped the pending name")
	}
	f.wantLines(t,
		"POST "+listenersPath,
		"GET "+listenersPath+"?"+listenerQuery,
		"GET "+listenersPath+"?"+listenerQuery)

	got, err := c.CreateListener(context.Background(), planListener, "lb-id-1")
	if err != nil {
		t.Fatalf("third CreateListener: %v", err)
	}
	if want := adoptedListener("lsn-id-2"); got != want {
		t.Errorf("third CreateListener = %+v, want %+v", got, want)
	}
	f.wantLines(t,
		"POST "+listenersPath,
		"GET "+listenersPath+"?"+listenerQuery,
		"GET "+listenersPath+"?"+listenerQuery,
		"GET "+listenersPath+"?"+listenerQuery,
		"POST "+listenersPath)
}

// TestCreateRefusalIssuesNoLookup confirms a create the service refused below
// 500 issues no lookup and leaves nothing pending, since nothing was created.
func TestCreateRefusalIssuesNoLookup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantQuota bool
	}{
		{"400 bad request", http.StatusBadRequest, `{"faultstring":"Invalid input for field/attribute protocol_port."}`, false},
		{"409 duplicate listener", http.StatusConflict, `{"faultstring":"Another Listener on this Load Balancer is already using protocol_port 8080"}`, false},
		{"403 quota", http.StatusForbidden, `{"faultstring":"Quota has been met for resources: Listener"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{
				"POST " + listenersPath: {{tc.status, tc.body}},
			}))
			c, _ := f.client()

			_, err := c.CreateListener(context.Background(), planListener, "lb-id-1")
			if httpStatus(err) != tc.status {
				t.Errorf("CreateListener = %v, want the %d", err, tc.status)
			}
			if errors.Is(err, ErrQuota) != tc.wantQuota {
				t.Errorf("errors.Is(%v, ErrQuota) = %v, want %v", err, !tc.wantQuota, tc.wantQuota)
			}
			f.wantLines(t, "POST "+listenersPath)
			if isPending(c) {
				t.Error("a refused create is pending")
			}
		})
	}
}

// TestLookupIgnoresUntaggedAndDeletingObjects confirms a lookup adopts only an
// object with the name that carries the run tag and is neither PENDING_DELETE
// nor DELETED, and that a list holding none of those adopts nothing.
func TestLookupIgnoresUntaggedAndDeletingObjects(t *testing.T) {
	ignored := []string{
		item("untagged", listenerName, "ACTIVE"),
		item("other-run", listenerName, "ACTIVE", "dizzy:run=run9", "dizzy:type=listener"),
		item("doomed", listenerName, "PENDING_DELETE", listenerTags...),
		item("dead", listenerName, "DELETED", listenerTags...),
		item("other-name", "dizzy-run0-lsn-0002", "ACTIVE", listenerTags...),
	}
	t.Run("only ignored objects", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{
			"POST " + listenersPath: {{http.StatusServiceUnavailable, `{}`}},
			"GET " + listenersPath:  {{http.StatusOK, listenerList(ignored...)}},
		}))
		c, _ := f.client()
		if _, err := c.CreateListener(context.Background(), planListener, "lb-id-1"); err == nil {
			t.Error("CreateListener adopted an ignored object, want the 503")
		}
		if !isPending(c) {
			t.Error("a lookup that found only ignored objects left nothing pending")
		}
	})
	t.Run("a match among them", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{
			"POST " + listenersPath: {{http.StatusServiceUnavailable, `{}`}},
			"GET " + listenersPath:  {{http.StatusOK, listenerList(append(ignored, item("lsn-id-ok", listenerName, "PENDING_UPDATE", listenerTags...))...)}},
		}))
		c, _ := f.client()
		got, err := c.CreateListener(context.Background(), planListener, "lb-id-1")
		if err != nil {
			t.Fatalf("CreateListener: %v", err)
		}
		if want := adoptedListener("lsn-id-ok"); got != want {
			t.Errorf("CreateListener = %+v, want %+v", got, want)
		}
	})
}

// TestLookupLogsFurtherMatches confirms a lookup that finds more than one
// matching object adopts the first and logs the ids of the others.
func TestLookupLogsFurtherMatches(t *testing.T) {
	logs := captureLog(t)
	f := newFake(t, scripted(t, map[string][]reply{
		"POST " + listenersPath: {{http.StatusBadGateway, `{}`}},
		"GET " + listenersPath: {{http.StatusOK, listenerList(
			item("lsn-id-a", listenerName, "ACTIVE", listenerTags...),
			item("lsn-id-b", listenerName, "ACTIVE", listenerTags...),
		)}},
	}))
	c, _ := f.client()

	got, err := c.CreateListener(context.Background(), planListener, "lb-id-1")
	if err != nil {
		t.Fatalf("CreateListener: %v", err)
	}
	if want := adoptedListener("lsn-id-a"); got != want {
		t.Errorf("CreateListener = %+v, want %+v", got, want)
	}
	out := logs.String()
	if !strings.Contains(out, "more octavia resources of an earlier create whose answer was lost; cleanup's cascade deletes them") ||
		!strings.Contains(out, "lsn-id-b") {
		t.Errorf("log %q does not report the further match lsn-id-b", out)
	}
}

// TestCreateLostConnectionLooksUp confirms a create whose connection closed
// without an answer, which carries no status, may have landed and is looked up
// and adopted.
func TestCreateLostConnectionLooksUp(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{
		"POST " + listenersPath: {{0, ""}},
		"GET " + listenersPath:  {{http.StatusOK, listenerList(item("lsn-id-1", listenerName, "PENDING_CREATE", listenerTags...))}},
	}))
	c, _ := f.client()

	got, err := c.CreateListener(context.Background(), planListener, "lb-id-1")
	if err != nil {
		t.Fatalf("CreateListener: %v", err)
	}
	if want := adoptedListener("lsn-id-1"); got != want {
		t.Errorf("CreateListener = %+v, want %+v", got, want)
	}
}

// TestCreateWithDoneContextSkipsLookup confirms a create that fails because its
// context is already done looks nothing up but keeps the name pending, so the
// next create lists first.
func TestCreateWithDoneContextSkipsLookup(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{
		"GET " + listenersPath: {{http.StatusOK, listenerList(item("lsn-id-1", listenerName, "ACTIVE", listenerTags...))}},
	}))
	c, _ := f.client()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.CreateListener(ctx, planListener, "lb-id-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateListener = %v, want context.Canceled", err)
	}
	f.wantLines(t)
	if !isPending(c) {
		t.Fatal("a create that may have landed is not pending")
	}

	got, err := c.CreateListener(context.Background(), planListener, "lb-id-1")
	if err != nil {
		t.Fatalf("second CreateListener: %v", err)
	}
	if want := adoptedListener("lsn-id-1"); got != want {
		t.Errorf("second CreateListener = %+v, want %+v", got, want)
	}
	f.wantLines(t, "GET "+listenersPath+"?"+listenerQuery)
}

// TestLookupFiltersPerKind confirms each kind's lookup lists by name, its
// parent, and the run tag where the API takes one: a load balancer by name and
// tag, a listener and a pool by load balancer, a monitor by pool, and a member
// below its pool by name, address, and port, its tag checked client side only.
func TestLookupFiltersPerKind(t *testing.T) {
	tag := "dizzy%3Arun%3Drun0"
	for _, tc := range []struct {
		name      string
		post      string
		list      string
		listBody  string
		create    func(*Client) (resource.Resource, error)
		wantQuery string
		wantID    string
	}{
		{
			name:     "load balancer",
			post:     "/v2.0/lbaas/loadbalancers",
			list:     "/v2.0/lbaas/loadbalancers",
			listBody: `{"loadbalancers":[` + item("lb-id-1", "dizzy-run0-lb-0001", "PENDING_CREATE", "dizzy:run=run0", "dizzy:type=loadbalancer") + `]}`,
			create: func(c *Client) (resource.Resource, error) {
				r, _, err := c.CreateLoadBalancer(context.Background(), plan.LoadBalancer{Name: "lb-0001", Network: "net-0001"}, "sub-id-1", "")
				return r, err
			},
			wantQuery: "name=dizzy-run0-lb-0001&tags=" + tag,
			wantID:    "lb-id-1",
		},
		{
			name:     "pool",
			post:     "/v2.0/lbaas/pools",
			list:     "/v2.0/lbaas/pools",
			listBody: `{"pools":[` + item("pool-id-1", "dizzy-run0-pool-0001", "ACTIVE", "dizzy:run=run0", "dizzy:type=pool") + `]}`,
			create: func(c *Client) (resource.Resource, error) {
				return c.CreatePool(context.Background(), plan.Pool{Name: "pool-0001", Listener: "lsn-0001"}, "lb-id-1", "lsn-id-1", "ROUND_ROBIN")
			},
			wantQuery: "loadbalancer_id=lb-id-1&name=dizzy-run0-pool-0001&tags=" + tag,
			wantID:    "pool-id-1",
		},
		{
			name: "member",
			post: "/v2.0/lbaas/pools/pool-id-1/members",
			list: "/v2.0/lbaas/pools/pool-id-1/members",
			listBody: `{"members":[` +
				`{"id":"mem-id-x","name":"dizzy-run0-pool-0001-member-01","provisioning_status":"ACTIVE","tags":[],"address":"10.0.1.128","protocol_port":8080},` +
				`{"id":"mem-id-1","name":"dizzy-run0-pool-0001-member-01","provisioning_status":"ACTIVE","tags":["dizzy:run=run0","dizzy:type=member"],"address":"10.0.1.128","protocol_port":8080}]}`,
			create: func(c *Client) (resource.Resource, error) {
				return c.CreateMember(context.Background(), plan.Member{Name: "pool-0001-member-01", Pool: "pool-0001", Address: "10.0.1.128", Port: 8080}, "pool-id-1", "sub-id-1")
			},
			wantQuery: "address=10.0.1.128&name=dizzy-run0-pool-0001-member-01&protocol_port=8080",
			wantID:    "mem-id-1",
		},
		{
			name:     "health monitor",
			post:     "/v2.0/lbaas/healthmonitors",
			list:     "/v2.0/lbaas/healthmonitors",
			listBody: `{"healthmonitors":[` + item("hm-id-1", "dizzy-run0-hm-0001", "ACTIVE", "dizzy:run=run0", "dizzy:type=healthmonitor") + `]}`,
			create: func(c *Client) (resource.Resource, error) {
				return c.CreateHealthMonitor(context.Background(), plan.HealthMonitor{Name: "hm-0001", Pool: "pool-0001"}, "pool-id-1")
			},
			wantQuery: "name=dizzy-run0-hm-0001&pool_id=pool-id-1&tags=" + tag,
			wantID:    "hm-id-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{
				"POST " + tc.post: {{http.StatusServiceUnavailable, `{}`}},
				"GET " + tc.list:  {{http.StatusOK, tc.listBody}},
			}))
			c, _ := f.client()

			got, err := tc.create(c)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if got.ID != tc.wantID {
				t.Errorf("adopted id = %q, want %q", got.ID, tc.wantID)
			}
			f.wantLines(t, "POST "+tc.post, "GET "+tc.list+"?"+tc.wantQuery)
			if q, _ := url.ParseQuery(f.requests()[1].query); tc.name == "member" && q.Has("tags") {
				t.Errorf("member lookup query %q carries tags, which the member list does not take", f.requests()[1].query)
			}
		})
	}
}

// TestCreateMemberAdoptsOnlyItsOwnAddress confirms a lost member create is
// adopted only by a create of the same member at the same address and port.
// The mix Gardener persona creates a member under one logical name for every
// instance of its worker, each at that instance's own address, so the orphan
// of an earlier instance, found by name and run tag, is posted past instead of
// adopted, and the lookup asks Octavia for the address and port as well.
func TestCreateMemberAdoptsOnlyItsOwnAddress(t *testing.T) {
	const (
		membersPath = "/v2.0/lbaas/pools/pool-id-1/members"
		name        = "dizzy-run0-pool-0001-member-01"
	)
	orphan := `{"members":[{"id":"mem-id-old","name":"` + name + `","provisioning_status":"ACTIVE",` +
		`"tags":["dizzy:run=run0","dizzy:type=member"],"address":"10.0.1.5","protocol_port":30000}]}`
	f := newFake(t, scripted(t, map[string][]reply{
		"POST " + membersPath: {
			{http.StatusServiceUnavailable, `{}`},
			{http.StatusCreated, `{"member":{"id":"mem-id-new","name":"` + name + `","provisioning_status":"PENDING_CREATE",` +
				`"tags":["dizzy:run=run0","dizzy:type=member"],"address":"10.0.1.9","protocol_port":30000}}`},
		},
		"GET " + membersPath: {
			{http.StatusOK, `{"members":[]}`},
			{http.StatusOK, orphan},
		},
	}))
	c, _ := f.client()

	old := plan.Member{Name: "pool-0001-member-01", Pool: "pool-0001", Address: "10.0.1.5", Port: 30000}
	if _, err := c.CreateMember(context.Background(), old, "pool-id-1", "sub-id-1"); err == nil {
		t.Fatal("CreateMember answered 503 = nil error, want the POST error")
	}

	replacement := old
	replacement.Address = "10.0.1.9"
	got, err := c.CreateMember(context.Background(), replacement, "pool-id-1", "sub-id-1")
	if err != nil {
		t.Fatalf("CreateMember of the replacement: %v", err)
	}
	if got.ID != "mem-id-new" {
		t.Fatalf("CreateMember of the replacement = %+v, want the posted mem-id-new, not the earlier instance's member", got)
	}
	f.wantLines(t,
		"POST "+membersPath,
		"GET "+membersPath+"?address=10.0.1.5&name="+name+"&protocol_port=30000",
		"GET "+membersPath+"?address=10.0.1.9&name="+name+"&protocol_port=30000",
		"POST "+membersPath)
}
