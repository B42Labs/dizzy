package nova

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// testNetworkClient builds a Client whose Neutron (network) service calls hit
// ts. Only the network-backed kinds (networks, subnets, ports) are exercised
// here, so the compute and block-storage clients are left nil.
func testNetworkClient(ts *httptest.Server) *Client {
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	return New(nil, gc, nil, "run0", metrics.NewCollector())
}

// TestDeleteNetworkPorts confirms the sweep deletes only the plain (empty
// device_owner) ports on a network — the untagged orphans a cancelled run can
// leave — while sparing router-interface and DHCP service ports, and tolerates
// a port already gone (404).
func TestDeleteNetworkPorts(t *testing.T) {
	var deletedPorts []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/ports":
			if got := r.URL.Query().Get("network_id"); got != "net-1" {
				t.Errorf("network_id query = %q, want net-1", got)
			}
			_, _ = io.WriteString(w, `{"ports":[
				{"id":"orphan-1","device_owner":""},
				{"id":"rif-1","device_owner":"network:router_interface"},
				{"id":"dhcp-1","device_owner":"network:dhcp"},
				{"id":"orphan-2","device_owner":""}
			]}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/ports/"):
			id := strings.TrimPrefix(r.URL.Path, "/ports/")
			deletedPorts = append(deletedPorts, id)
			if id == "orphan-2" {
				w.WriteHeader(http.StatusNotFound) // already gone: must be tolerated
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	c := testNetworkClient(ts)
	deleted, err := c.DeleteNetworkPorts(context.Background(), "net-1")
	if err != nil {
		t.Fatalf("DeleteNetworkPorts: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (orphan-1 removed, orphan-2 already gone, others skipped)", deleted)
	}
	if strings.Join(deletedPorts, ",") != "orphan-1,orphan-2" {
		t.Errorf("deleted ports = %v, want only the empty-device-owner ports attempted", deletedPorts)
	}
}

// TestListNetworkServers verifies the discovery behind the churn network
// sweep: it returns the server that holds the network through an untagged
// compute port and carries this run's dizzy:run, read once however many such
// ports it has; it reads no server behind a port dizzy tagged, a service port
// or a plain port; it skips another run's server, a server without metadata
// and a server already gone; an empty network reads no server; and an
// upstream error is wrapped with the network and, for a server read, with the
// server id.
func TestListNetworkServers(t *testing.T) {
	// reply is the status and body a fake answers one request with.
	type reply struct {
		status int
		body   string
	}
	// serve answers the port list of net-1 with ports and the GET of each
	// server in servers with its reply, and counts the GETs per server id. A
	// server without a reply must not be read.
	serve := func(t *testing.T, ports reply, servers map[string]reply) (*Client, func() map[string]int) {
		t.Helper()
		var mu sync.Mutex
		gets := map[string]int{}
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/ports":
				if got := r.URL.Query().Get("network_id"); got != "net-1" {
					t.Errorf("network_id query = %q, want net-1", got)
				}
				w.WriteHeader(ports.status)
				_, _ = io.WriteString(w, ports.body)
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/servers/"):
				id := strings.TrimPrefix(r.URL.Path, "/servers/")
				mu.Lock()
				gets[id]++
				mu.Unlock()
				rep, ok := servers[id]
				if !ok {
					t.Errorf("unexpected GET of server %s", id)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(rep.status)
				_, _ = io.WriteString(w, rep.body)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}))
		t.Cleanup(ts.Close)
		return testComputeNetworkClient(ts, metrics.NewCollector()), func() map[string]int {
			mu.Lock()
			defer mu.Unlock()
			return maps.Clone(gets)
		}
	}
	server := func(id, name, metadata string) reply {
		return reply{http.StatusOK, `{"server":{"id":"` + id + `","name":"` + name + `"` + metadata + `}}`}
	}
	notFound := reply{http.StatusNotFound, `{"itemNotFound":{"code":404,"message":"Instance could not be found."}}`}
	unavailable := reply{http.StatusServiceUnavailable, `{"message":"service unavailable"}`}

	tests := []struct {
		name     string
		ports    reply
		servers  map[string]reply
		want     []resource.Resource
		wantGets map[string]int
	}{
		{
			name: "orphan",
			ports: reply{http.StatusOK, `{"ports":[
				{"id":"nic-1","device_owner":"compute:nova","device_id":"srv-a","tags":[]},
				{"id":"nic-2","device_owner":"compute:nova","device_id":"srv-a"},
				{"id":"port-1","device_owner":"compute:nova","device_id":"srv-b","tags":["dizzy:run=run0","dizzy:type=port"]},
				{"id":"dhcp-1","device_owner":"network:dhcp","device_id":"dhcp-net-1"},
				{"id":"plain-1","device_owner":"","device_id":""}
			]}`},
			servers:  map[string]reply{"srv-a": server("srv-a", "dizzy-run0-srv-0001", `,"metadata":{"dizzy:run":"run0","dizzy:type":"server"}`)},
			want:     []resource.Resource{{Kind: "server", Logical: "srv-0001", Name: "dizzy-run0-srv-0001", ID: "srv-a"}},
			wantGets: map[string]int{"srv-a": 1},
		},
		{
			name: "another run's server",
			ports: reply{http.StatusOK, `{"ports":[
				{"id":"nic-1","device_owner":"compute:nova","device_id":"srv-c"},
				{"id":"nic-2","device_owner":"compute:nova","device_id":"srv-d"}
			]}`},
			servers: map[string]reply{
				"srv-c": server("srv-c", "dizzy-other-srv-0001", `,"metadata":{"dizzy:run":"other","dizzy:type":"server"}`),
				"srv-d": server("srv-d", "someone-elses", ""),
			},
			wantGets: map[string]int{"srv-c": 1, "srv-d": 1},
		},
		{
			// The live server listed after the gone one shows the 404 is skipped,
			// not taken as the end of the listing.
			name: "server already gone",
			ports: reply{http.StatusOK, `{"ports":[
				{"id":"nic-1","device_owner":"compute:nova","device_id":"srv-a"},
				{"id":"nic-2","device_owner":"compute:nova","device_id":"srv-e"}
			]}`},
			servers: map[string]reply{
				"srv-a": notFound,
				"srv-e": server("srv-e", "dizzy-run0-srv-0002", `,"metadata":{"dizzy:run":"run0","dizzy:type":"server"}`),
			},
			want:     []resource.Resource{{Kind: "server", Logical: "srv-0002", Name: "dizzy-run0-srv-0002", ID: "srv-e"}},
			wantGets: map[string]int{"srv-a": 1, "srv-e": 1},
		},
		{
			name:     "empty network",
			ports:    reply{http.StatusOK, `{"ports":[]}`},
			wantGets: map[string]int{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, gets := serve(t, tc.ports, tc.servers)
			got, err := c.ListNetworkServers(context.Background(), "net-1")
			if err != nil {
				t.Fatalf("ListNetworkServers: %v", err)
			}
			if len(tc.want) == 0 {
				if len(got) != 0 {
					t.Errorf("ListNetworkServers = %v, want no server", got)
				}
			} else if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ListNetworkServers = %v, want %v", got, tc.want)
			}
			if g := gets(); !reflect.DeepEqual(g, tc.wantGets) {
				t.Errorf("server GETs = %v, want %v", g, tc.wantGets)
			}
		})
	}

	t.Run("upstream errors", func(t *testing.T) {
		c, gets := serve(t, unavailable, nil)
		_, err := c.ListNetworkServers(context.Background(), "net-1")
		if err == nil || !strings.HasPrefix(err.Error(), "listing servers on network net-1:") {
			t.Errorf("ListNetworkServers with a failing port list = %v, want an error starting with %q", err, "listing servers on network net-1:")
		}
		if !IsRetryable(err) {
			t.Errorf("IsRetryable(%v) = false, want true for a 503", err)
		}
		if g := gets(); len(g) != 0 {
			t.Errorf("server GETs = %v, want none after a failed port list", g)
		}

		c, _ = serve(t, reply{http.StatusOK, `{"ports":[{"id":"nic-1","device_owner":"compute:nova","device_id":"srv-a"}]}`},
			map[string]reply{"srv-a": unavailable})
		_, err = c.ListNetworkServers(context.Background(), "net-1")
		if err == nil || !strings.Contains(err.Error(), "getting server srv-a") {
			t.Errorf("ListNetworkServers with a failing server GET = %v, want an error containing %q", err, "getting server srv-a")
		}
	})
}

// TestCreateTaggedRollsBackOnTagFailure verifies that when tagging a freshly
// created resource keeps failing, createTagged retries the tag in place — never
// re-running create — and then rolls the resource back with a Delete, so no
// created-but-untagged orphan that tag-based cleanup can never reclaim survives.
func TestCreateTaggedRollsBackOnTagFailure(t *testing.T) {
	var puts, deletes atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/tags"):
			puts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	c := testNetworkClient(ts)

	creates := 0
	_, err := c.createTagged(context.Background(), KindNetwork, "net-0001",
		func(ctx context.Context, name string) (string, error) {
			creates++
			return "net-id-1", nil
		})
	if err == nil {
		t.Fatal("expected an error when tagging keeps failing")
	}
	if creates != 1 {
		t.Errorf("create closure called %d times, want 1 (a tag failure must not re-create)", creates)
	}
	if got := int(puts.Load()); got != tagAttempts {
		t.Errorf("tag attempted %d times, want %d", got, tagAttempts)
	}
	if got := deletes.Load(); got != 1 {
		t.Errorf("rollback Delete called %d times, want 1", got)
	}
}

// TestWaitForReadyAbortsOnGone verifies a readiness poll aborts promptly with a
// gone (404) error when the resource is deleted out of band, rather than
// re-polling until the context deadline and returning a DeadlineExceeded the
// apply path would tolerate as if the resource had come ready.
func TestWaitForReadyAbortsOnGone(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/networks/") {
			w.WriteHeader(http.StatusNotFound) // deleted out of band: every Get is a 404
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := testNetworkClient(ts)
	// A generous deadline the fix must return well before: an unfixed pollStatus
	// re-polls the 404 until this elapses and returns context.DeadlineExceeded.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.WaitForReady(ctx, resource.Resource{Kind: KindNetwork, ID: "net-gone", Logical: "net-0001"})
	if err == nil {
		t.Fatal("WaitForReady on a gone resource = nil, want a gone error")
	}
	if !IsNotFound(err) {
		t.Errorf("WaitForReady error = %v, want a 404 gone error", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("WaitForReady spun to the deadline instead of aborting on the 404")
	}
}
