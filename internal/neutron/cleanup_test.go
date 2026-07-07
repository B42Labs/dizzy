package neutron

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/B42Labs/dizzy/internal/metrics"
)

// TestListByTagSendsTagQuery confirms ListByTag filters server-side with the
// run's dizzy:run tag and parses the returned resources.
func TestListByTagSendsTagQuery(t *testing.T) {
	var gotTag string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/networks" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotTag = r.URL.Query().Get("tags")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"networks":[{"id":"n1","name":"dizzy-run0-net-0001"}]}`)
	}))
	defer ts.Close()

	c := testServiceClient(ts)
	got, err := c.ListByTag(context.Background(), KindNetwork, "run0")
	if err != nil {
		t.Fatalf("ListByTag: %v", err)
	}
	if gotTag != "dizzy:run=run0" {
		t.Errorf("tags query = %q, want %q", gotTag, "dizzy:run=run0")
	}
	if len(got) != 1 || got[0].ID != "n1" || got[0].Kind != KindNetwork {
		t.Errorf("ListByTag = %+v, want one network n1", got)
	}
}

// TestListByTypeTagSendsTypeQuery confirms ListByTypeTag filters server-side on
// the dizzy:type tag — the pre-flight sweep's "any tester run" discovery —
// and parses the returned resources.
func TestListByTypeTagSendsTypeQuery(t *testing.T) {
	var gotTag string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/networks" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotTag = r.URL.Query().Get("tags")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"networks":[{"id":"n1","name":"dizzy-old-net-0001"}]}`)
	}))
	defer ts.Close()

	c := testServiceClient(ts)
	got, err := c.ListByTypeTag(context.Background(), KindNetwork)
	if err != nil {
		t.Fatalf("ListByTypeTag: %v", err)
	}
	if gotTag != "dizzy:type=network" {
		t.Errorf("tags query = %q, want %q", gotTag, "dizzy:type=network")
	}
	if len(got) != 1 || got[0].ID != "n1" || got[0].Kind != KindNetwork {
		t.Errorf("ListByTypeTag = %+v, want one network n1", got)
	}
}

// TestListByTypeTagRejectsUntaggableKind confirms an untaggable kind is rejected
// rather than silently returning nothing, mirroring ListByTag.
func TestListByTypeTagRejectsUntaggableKind(t *testing.T) {
	c := New(nil, "run0", metrics.NewCollector())
	if _, err := c.ListByTypeTag(context.Background(), KindSecurityGroupRule); err == nil {
		t.Fatal("ListByTypeTag for an untaggable kind: expected an error, got nil")
	}
}

// TestListByTagRejectsUntaggableKind confirms a kind with no tag-based discovery
// (security-group rules) is rejected rather than silently returning nothing.
func TestListByTagRejectsUntaggableKind(t *testing.T) {
	c := New(nil, "run0", metrics.NewCollector())
	if _, err := c.ListByTag(context.Background(), KindSecurityGroupRule, "run0"); err == nil {
		t.Fatal("ListByTag for an untaggable kind: expected an error, got nil")
	}
}

// TestDeleteNetworkPorts confirms the sweep deletes only the plain (empty
// device_owner) ports on a network — the untagged orphans a cancelled run can
// leave — while leaving router-interface and service ports for detach/cascade,
// and tolerates a port already gone (404).
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

	c := testServiceClient(ts)
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

// TestDetachRouterInterfaces confirms every port on the router is removed with a
// port-scoped RemoveInterface call, and a 404 on one removal is tolerated.
func TestDetachRouterInterfaces(t *testing.T) {
	var removedPorts []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/ports":
			if got := r.URL.Query().Get("device_id"); got != "r1" {
				t.Errorf("device_id query = %q, want r1", got)
			}
			// The listing is constrained to interface ports so a router's gateway
			// or HA ports (which RemoveInterface cannot detach) are never returned.
			if got := r.URL.Query().Get("device_owner"); got != "network:router_interface" {
				t.Errorf("device_owner query = %q, want network:router_interface", got)
			}
			_, _ = io.WriteString(w, `{"ports":[{"id":"p1"},{"id":"p2"}]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/routers/r1/remove_router_interface":
			var body struct {
				PortID string `json:"port_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decoding remove body: %v", err)
			}
			removedPorts = append(removedPorts, body.PortID)
			if body.PortID == "p2" {
				// p2 was already detached out of band: a 404 must be tolerated.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, `{"id":"r1","subnet_id":"s1","port_id":"`+body.PortID+`"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	c := testServiceClient(ts)
	detached, err := c.DetachRouterInterfaces(context.Background(), "r1")
	if err != nil {
		t.Fatalf("DetachRouterInterfaces: %v", err)
	}
	if detached != 1 {
		t.Errorf("detached = %d, want 1 (p1 removed, p2 already gone)", detached)
	}
	if strings.Join(removedPorts, ",") != "p1,p2" {
		t.Errorf("removed ports = %v, want both p1 and p2 attempted", removedPorts)
	}
}
