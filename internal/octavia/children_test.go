package octavia

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/octavia/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// TestCreateChildBodies confirms each child create posts the deterministic
// name, the run tags of its kind, and its own fields — a listener TCP on the
// plan port of its load balancer, a pool TCP as its listener's default pool
// with the bound algorithm, a member at the plan address and port on the VIP
// subnet, and a monitor TCP on its pool with delay 5, timeout 3, and 3 retries
// — and returns the created Resource after one POST, recording one create
// sample of its kind.
func TestCreateChildBodies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     resource.Kind
		path     string
		envelope string
		answer   string
		create   func(*Client) (resource.Resource, error)
		want     resource.Resource
		fields   map[string]any
		absent   []string
	}{
		{
			name:     "listener",
			kind:     KindListener,
			path:     "/v2.0/lbaas/listeners",
			envelope: "listener",
			answer:   `{"listener":` + item("lsn-id-1", "dizzy-run0-lsn-0001", "PENDING_CREATE") + `}`,
			create: func(c *Client) (resource.Resource, error) {
				return c.CreateListener(context.Background(), plan.Listener{Name: "lsn-0001", LoadBalancer: "lb-0001", Port: 31337}, "lb-id-1")
			},
			want: resource.Resource{Kind: KindListener, Logical: "lsn-0001", Name: "dizzy-run0-lsn-0001", ID: "lsn-id-1"},
			fields: map[string]any{
				"name":            "dizzy-run0-lsn-0001",
				"loadbalancer_id": "lb-id-1",
				"protocol":        "TCP",
				"protocol_port":   float64(31337),
			},
		},
		{
			name:     "pool",
			kind:     KindPool,
			path:     "/v2.0/lbaas/pools",
			envelope: "pool",
			answer:   `{"pool":` + item("pool-id-1", "dizzy-run0-pool-0001", "PENDING_CREATE") + `}`,
			create: func(c *Client) (resource.Resource, error) {
				return c.CreatePool(context.Background(), plan.Pool{Name: "pool-0001", Listener: "lsn-0001"}, "lb-id-1", "lsn-id-1", PoolAlgorithm("ovn"))
			},
			want: resource.Resource{Kind: KindPool, Logical: "pool-0001", Name: "dizzy-run0-pool-0001", ID: "pool-id-1"},
			fields: map[string]any{
				"name":         "dizzy-run0-pool-0001",
				"listener_id":  "lsn-id-1",
				"protocol":     "TCP",
				"lb_algorithm": "SOURCE_IP_PORT",
			},
			// The load balancer id scopes the lookup only; the listener id
			// already makes the pool its listener's default pool.
			absent: []string{"loadbalancer_id"},
		},
		{
			name:     "member",
			kind:     KindMember,
			path:     "/v2.0/lbaas/pools/pool-id-1/members",
			envelope: "member",
			answer:   `{"member":` + item("mem-id-1", "dizzy-run0-pool-0001-member-01", "PENDING_CREATE") + `}`,
			create: func(c *Client) (resource.Resource, error) {
				return c.CreateMember(context.Background(),
					plan.Member{Name: "pool-0001-member-01", Pool: "pool-0001", Address: "10.0.1.130", Port: 40000}, "pool-id-1", "sub-id-1")
			},
			want: resource.Resource{Kind: KindMember, Logical: "pool-0001-member-01", Name: "dizzy-run0-pool-0001-member-01", ID: "mem-id-1"},
			fields: map[string]any{
				"name":          "dizzy-run0-pool-0001-member-01",
				"address":       "10.0.1.130",
				"protocol_port": float64(40000),
				"subnet_id":     "sub-id-1",
			},
		},
		{
			name:     "health monitor",
			kind:     KindHealthMonitor,
			path:     "/v2.0/lbaas/healthmonitors",
			envelope: "healthmonitor",
			answer:   `{"healthmonitor":` + item("hm-id-1", "dizzy-run0-hm-0001", "PENDING_CREATE") + `}`,
			create: func(c *Client) (resource.Resource, error) {
				return c.CreateHealthMonitor(context.Background(), plan.HealthMonitor{Name: "hm-0001", Pool: "pool-0001"}, "pool-id-1")
			},
			want: resource.Resource{Kind: KindHealthMonitor, Logical: "hm-0001", Name: "dizzy-run0-hm-0001", ID: "hm-id-1"},
			fields: map[string]any{
				"name":        "dizzy-run0-hm-0001",
				"pool_id":     "pool-id-1",
				"type":        "TCP",
				"delay":       float64(5),
				"timeout":     float64(3),
				"max_retries": float64(3),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{
				"POST " + tc.path: {{http.StatusCreated, tc.answer}},
			}))
			c, m := f.client()

			got, err := tc.create(c)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if got != tc.want {
				t.Errorf("create = %+v, want %+v", got, tc.want)
			}
			f.wantLines(t, "POST "+tc.path)

			obj := decodeBody(t, f.requests()[0].body, tc.envelope)
			for key, want := range tc.fields {
				if obj[key] != want {
					t.Errorf("%s = %v, want %v", key, obj[key], want)
				}
			}
			for _, key := range tc.absent {
				if v, ok := obj[key]; ok {
					t.Errorf("%s = %v, want it absent", key, v)
				}
			}
			wantTags(t, obj, tc.kind)

			if s := statsOf(m.Aggregate(time.Second), string(tc.kind)); s.Attempted != 1 || s.Succeeded != 1 {
				t.Errorf("%s stats = %+v, want one successful create", tc.kind, s)
			}
		})
	}
}

// TestCreatePoolRoundRobin confirms a pool of a load balancer whose provider is
// not OVN posts the ROUND_ROBIN algorithm PoolAlgorithm binds for it.
func TestCreatePoolRoundRobin(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{
		"POST /v2.0/lbaas/pools": {{http.StatusCreated, `{"pool":` + item("pool-id-1", "dizzy-run0-pool-0001", "PENDING_CREATE") + `}`}},
	}))
	c, _ := f.client()

	if _, err := c.CreatePool(context.Background(), plan.Pool{Name: "pool-0001", Listener: "lsn-0001"}, "lb-id-1", "lsn-id-1", PoolAlgorithm("amphora")); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	if got := decodeBody(t, f.requests()[0].body, "pool")["lb_algorithm"]; got != "ROUND_ROBIN" {
		t.Errorf("lb_algorithm = %v, want ROUND_ROBIN", got)
	}
}
