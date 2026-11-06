package octavia

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// vipNetwork is the plan entry of the first VIP network of a small plan.
var vipNetwork = plan.Network{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"}

// TestCreateNetworkTagsTheNetwork confirms a VIP network is created with its
// deterministic name and then tagged with the run tags for network.
func TestCreateNetworkTagsTheNetwork(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c call) {
		switch c.method + " " + c.path {
		case "POST /networks":
			writeJSON(w, http.StatusCreated, `{"network":{"id":"net-id-1","name":"dizzy-run0-net-0001"}}`)
		case "PUT /networks/net-id-1/tags":
			writeJSON(w, http.StatusOK, `{"tags":["dizzy:run=run0","dizzy:type=network"]}`)
		default:
			writeJSON(w, http.StatusInternalServerError, `{}`)
		}
	})
	c, m := f.client()

	got, err := c.CreateNetwork(context.Background(), vipNetwork)
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	want := resource.Resource{Kind: KindNetwork, Logical: "net-0001", Name: "dizzy-run0-net-0001", ID: "net-id-1"}
	if got != want {
		t.Errorf("CreateNetwork = %+v, want %+v", got, want)
	}
	f.wantLines(t, "POST /networks", "PUT /networks/net-id-1/tags")
	reqs := f.requests()
	if obj := decodeBody(t, reqs[0].body, "network"); obj["name"] != "dizzy-run0-net-0001" {
		t.Errorf("network name = %v, want dizzy-run0-net-0001", obj["name"])
	}
	if !strings.Contains(reqs[1].body, `"dizzy:run=run0"`) || !strings.Contains(reqs[1].body, `"dizzy:type=network"`) {
		t.Errorf("tag body = %s, want the network run tags", reqs[1].body)
	}
	if s := statsOf(m.Aggregate(time.Second), "network"); s.Attempted != 2 || s.Succeeded != 2 {
		t.Errorf("network stats = %+v, want the create and the tag recorded", s)
	}
}

// TestCreateSubnetBody confirms the VIP subnet create carries the plan CIDR,
// its gateway at .1, and one allocation pool from .2 to .127, so Neutron never
// hands a VIP or amphora port an address the plan gives a member, and that the
// subnet is tagged with the run tags for subnet.
func TestCreateSubnetBody(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c call) {
		switch c.method + " " + c.path {
		case "POST /subnets":
			writeJSON(w, http.StatusCreated, `{"subnet":{"id":"sub-id-1","name":"dizzy-run0-sub-0001"}}`)
		case "PUT /subnets/sub-id-1/tags":
			writeJSON(w, http.StatusOK, `{"tags":["dizzy:run=run0","dizzy:type=subnet"]}`)
		default:
			writeJSON(w, http.StatusInternalServerError, `{}`)
		}
	})
	c, _ := f.client()

	got, err := c.CreateSubnet(context.Background(), plan.Network{Name: "net-0003", Subnet: "sub-0003", CIDR: "10.0.3.0/24"}, "net-id-3")
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	want := resource.Resource{Kind: KindSubnet, Logical: "sub-0003", Name: "dizzy-run0-sub-0003", ID: "sub-id-1"}
	if got != want {
		t.Errorf("CreateSubnet = %+v, want %+v", got, want)
	}
	f.wantLines(t, "POST /subnets", "PUT /subnets/sub-id-1/tags")

	obj := decodeBody(t, f.requests()[0].body, "subnet")
	for key, want := range map[string]any{
		"network_id": "net-id-3",
		"name":       "dizzy-run0-sub-0003",
		"ip_version": float64(4),
		"cidr":       "10.0.3.0/24",
		"gateway_ip": "10.0.3.1",
	} {
		if obj[key] != want {
			t.Errorf("subnet %s = %v, want %v", key, obj[key], want)
		}
	}
	wantPools := []any{map[string]any{"start": "10.0.3.2", "end": "10.0.3.127"}}
	if !reflect.DeepEqual(obj["allocation_pools"], wantPools) {
		t.Errorf("allocation_pools = %v, want %v", obj["allocation_pools"], wantPools)
	}
	if body := f.requests()[1].body; !strings.Contains(body, `"dizzy:type=subnet"`) {
		t.Errorf("tag body = %s, want the subnet type tag", body)
	}
}

// TestCreateSubnetRefusesBadCIDR confirms a CIDR that is not an IPv4 /24 fails
// before any request, since the gateway and allocation pool derive from it.
func TestCreateSubnetRefusesBadCIDR(t *testing.T) {
	for _, cidr := range []string{"10.0.1.0/16", "fd00::/64", "not-a-cidr"} {
		f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ call) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
		c, _ := f.client()
		_, err := c.CreateSubnet(context.Background(), plan.Network{Name: "net-0001", Subnet: "sub-0001", CIDR: cidr}, "net-id-1")
		if err == nil || !strings.HasPrefix(err.Error(), `creating subnet "sub-0001": `) {
			t.Errorf("CreateSubnet(%q) = %v, want a creating subnet error", cidr, err)
		}
		f.wantLines(t)
	}
}

// TestCreateNetworkRollsBackOnTagFailure confirms a network whose tag keeps
// failing is tagged tagAttempts times, never re-created, and deleted again, and
// that the create fails with the tagging error, so no untagged network that
// tag-based cleanup could never reclaim survives.
func TestCreateNetworkRollsBackOnTagFailure(t *testing.T) {
	fastPolls(t)
	var posts atomic.Int32
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c call) {
		switch c.method + " " + c.path {
		case "POST /networks":
			posts.Add(1)
			writeJSON(w, http.StatusCreated, `{"network":{"id":"net-id-1"}}`)
		case "PUT /networks/net-id-1/tags":
			writeJSON(w, http.StatusServiceUnavailable, `{"message":"unavailable"}`)
		case "DELETE /networks/net-id-1":
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSON(w, http.StatusInternalServerError, `{}`)
		}
	})
	c, _ := f.client()

	_, err := c.CreateNetwork(context.Background(), vipNetwork)
	if err == nil || !strings.HasPrefix(err.Error(), `tagging network "net-0001": `) {
		t.Fatalf("CreateNetwork = %v, want an error starting with %q", err, `tagging network "net-0001": `)
	}
	if got := httpStatus(err); got != http.StatusServiceUnavailable {
		t.Errorf("tagging error status = %d, want 503", got)
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("network created %d times, want 1 (a tag failure must not re-create)", got)
	}
	want := []string{"POST /networks"}
	for range tagAttempts {
		want = append(want, "PUT /networks/net-id-1/tags")
	}
	want = append(want, "DELETE /networks/net-id-1")
	f.wantLines(t, want...)
}

// TestCreateNetworkQuota confirms Neutron's 409 over-quota answer is wrapped
// with ErrQuota and tags nothing.
func TestCreateNetworkQuota(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ call) {
		writeJSON(w, http.StatusConflict, `{"NeutronError":{"type":"OverQuota","message":"Quota exceeded for resources: ['network']."}}`)
	})
	c, _ := f.client()

	_, err := c.CreateNetwork(context.Background(), vipNetwork)
	if !errors.Is(err, ErrQuota) {
		t.Errorf("CreateNetwork = %v, want ErrQuota", err)
	}
	f.wantLines(t, "POST /networks")
}

// TestWaitForReadyNetwork confirms a VIP network is polled through BUILD and a
// transient error to ACTIVE, recording one network readiness sample and no
// operation sample.
func TestWaitForReadyNetwork(t *testing.T) {
	fastPolls(t)
	var polls atomic.Int32
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c call) {
		if c.method+" "+c.path != "GET /networks/net-id-1" {
			writeJSON(w, http.StatusInternalServerError, `{}`)
			return
		}
		switch polls.Add(1) {
		case 1:
			writeJSON(w, http.StatusOK, `{"network":{"id":"net-id-1","status":"BUILD"}}`)
		case 2:
			writeJSON(w, http.StatusServiceUnavailable, `{}`)
		default:
			writeJSON(w, http.StatusOK, `{"network":{"id":"net-id-1","status":"ACTIVE"}}`)
		}
	})
	c, m := f.client()

	if err := c.WaitForReady(waitCtx(t), resource.Resource{Kind: KindNetwork, ID: "net-id-1"}); err != nil {
		t.Fatalf("WaitForReady: %v", err)
	}
	if got := polls.Load(); got != 3 {
		t.Errorf("polls = %d, want 3", got)
	}
	agg := m.Aggregate(time.Second)
	if r := readinessOf(agg, "network"); r.Count != 1 || r.OK != 1 {
		t.Errorf("network readiness = %+v, want one OK sample", r)
	}
	if agg.Overall.Attempted != 0 {
		t.Errorf("recorded %d operation samples, want none for readiness polls", agg.Overall.Attempted)
	}
}

// TestWaitForReadySubnetReturnsAtOnce confirms a subnet, which reports no
// status, is ready without a request or a readiness sample.
func TestWaitForReadySubnetReturnsAtOnce(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ call) {
		writeJSON(w, http.StatusInternalServerError, `{}`)
	})
	c, m := f.client()

	if err := c.WaitForReady(context.Background(), resource.Resource{Kind: KindSubnet, ID: "sub-id-1"}); err != nil {
		t.Errorf("WaitForReady(subnet) = %v, want nil", err)
	}
	f.wantLines(t)
	if r := m.Aggregate(time.Second).Readiness; len(r) != 0 {
		t.Errorf("readiness = %+v, want none for a subnet", r)
	}
}

// TestWaitForReadyNetworkFailures confirms a network deleted out of band aborts
// the wait with a gone error, an expired context returns its error, both record
// a failed readiness sample, and a kind WaitForReady does not poll is refused
// without a request.
func TestWaitForReadyNetworkFailures(t *testing.T) {
	fastPolls(t)
	t.Run("gone", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ call) {
			writeJSON(w, http.StatusNotFound, `{"NeutronError":{"type":"NetworkNotFound"}}`)
		})
		c, m := f.client()
		err := c.WaitForReady(waitCtx(t), resource.Resource{Kind: KindNetwork, ID: "net-gone"})
		if !IsNotFound(err) || !strings.Contains(err.Error(), "is gone while waiting for readiness") {
			t.Errorf("WaitForReady = %v, want a gone 404 error", err)
		}
		if r := readinessOf(m.Aggregate(time.Second), "network"); r.Count != 1 || r.OK != 0 {
			t.Errorf("network readiness = %+v, want one failed sample", r)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ call) {
			writeJSON(w, http.StatusOK, `{"network":{"id":"net-id-1","status":"BUILD"}}`)
		})
		c, m := f.client()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if err := c.WaitForReady(ctx, resource.Resource{Kind: KindNetwork, ID: "net-id-1"}); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("WaitForReady = %v, want context.DeadlineExceeded", err)
		}
		if r := readinessOf(m.Aggregate(time.Second), "network"); r.Count != 1 || r.OK != 0 {
			t.Errorf("network readiness = %+v, want one failed sample", r)
		}
	})
	t.Run("unsupported kind", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ call) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
		c, _ := f.client()
		err := c.WaitForReady(context.Background(), resource.Resource{Kind: KindLoadBalancer, ID: "lb-1"})
		if err == nil || err.Error() != `wait for ready not supported for kind "loadbalancer"` {
			t.Errorf("WaitForReady(loadbalancer) = %v, want the unsupported-kind error", err)
		}
		f.wantLines(t)
	})
}
