package octavia

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// tagQuery is the query of a list filtered to run0's run tag.
const tagQuery = "tags=dizzy%3Arun%3Drun0"

// TestDeleteLoadBalancerCascades confirms a load balancer is deleted with
// ?cascade=true, so its children go with it, and the call is recorded under
// loadbalancer.
func TestDeleteLoadBalancerCascades(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{
		"DELETE " + loadBalancerPath: {{http.StatusNoContent, ``}},
	}))
	c, m := f.client()

	if err := c.Delete(context.Background(), createdLB); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	f.wantLines(t, "DELETE "+loadBalancerPath+"?cascade=true")
	if s := statsOf(m.Aggregate(time.Second), "loadbalancer"); s.Attempted != 1 || s.Succeeded != 1 {
		t.Errorf("loadbalancer stats = %+v, want one successful delete", s)
	}
}

// TestDeleteVIPNetworkAndSubnet confirms a VIP subnet and network are deleted
// by id through Neutron, and a 404 surfaces for the caller to treat as gone.
func TestDeleteVIPNetworkAndSubnet(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{
		"DELETE /subnets/sub-id-1":  {{http.StatusNoContent, ``}},
		"DELETE /networks/net-id-1": {{http.StatusNotFound, `{"NeutronError":{"type":"NetworkNotFound"}}`}},
	}))
	c, _ := f.client()

	if err := c.Delete(context.Background(), resource.Resource{Kind: KindSubnet, ID: "sub-id-1"}); err != nil {
		t.Errorf("Delete(subnet) = %v, want nil", err)
	}
	if err := c.Delete(context.Background(), resource.Resource{Kind: KindNetwork, ID: "net-id-1"}); !IsNotFound(err) {
		t.Errorf("Delete(gone network) = %v, want a 404", err)
	}
	f.wantLines(t, "DELETE /subnets/sub-id-1", "DELETE /networks/net-id-1")
}

// TestDeleteRefusesChildKinds confirms a listener, pool, member, or health
// monitor is never deleted on its own: the delete is refused without a request
// or a sample, since the load balancer's cascade removes it.
func TestDeleteRefusesChildKinds(t *testing.T) {
	for _, kind := range []resource.Kind{KindListener, KindPool, KindMember, KindHealthMonitor} {
		f := newFake(t, scripted(t, map[string][]reply{}))
		c, m := f.client()

		err := c.Delete(context.Background(), resource.Resource{Kind: kind, ID: "id-1"})
		if want := `delete not supported for kind "` + string(kind) + `"`; err == nil || err.Error() != want {
			t.Errorf("Delete(%s) = %v, want %q", kind, err, want)
		}
		f.wantLines(t)
		if n := m.Aggregate(time.Second).Overall.Attempted; n != 0 {
			t.Errorf("Delete(%s) recorded %d samples, want none", kind, n)
		}
	}
}

// TestListByTag confirms each discoverable kind is listed filtered to the run
// tag server side, that an item without the tag a server returned anyway is
// dropped client side, and that the list is recorded under <kind>/list.
func TestListByTag(t *testing.T) {
	tagged := []string{"dizzy:run=run0", "dizzy:type=x"}
	for _, tc := range []struct {
		kind resource.Kind
		path string
		body string
	}{
		{KindLoadBalancer, loadBalancersPath, `{"loadbalancers":[` +
			item("lb-id-1", "dizzy-run0-lb-0001", "ACTIVE", tagged...) + `,` +
			item("lb-id-x", "someone-elses", "ACTIVE") + `,` +
			item("lb-id-2", "dizzy-run0-lb-0002", "ERROR", tagged...) + `]}`},
		{KindNetwork, "/networks", `{"networks":[` +
			`{"id":"lb-id-1","name":"dizzy-run0-lb-0001","tags":["dizzy:run=run0"]},` +
			`{"id":"lb-id-x","name":"someone-elses","tags":["dizzy:run=run9"]},` +
			`{"id":"lb-id-2","name":"dizzy-run0-lb-0002","tags":["dizzy:run=run0"]}]}`},
		{KindSubnet, "/subnets", `{"subnets":[` +
			`{"id":"lb-id-1","name":"dizzy-run0-lb-0001","tags":["dizzy:run=run0"]},` +
			`{"id":"lb-id-x","name":"someone-elses"},` +
			`{"id":"lb-id-2","name":"dizzy-run0-lb-0002","tags":["dizzy:run=run0"]}]}`},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{"GET " + tc.path: {{http.StatusOK, tc.body}}}))
			c, m := f.client()

			got, err := c.ListByTag(context.Background(), tc.kind, "run0")
			if err != nil {
				t.Fatalf("ListByTag: %v", err)
			}
			want := []resource.Resource{
				{Kind: tc.kind, Name: "dizzy-run0-lb-0001", ID: "lb-id-1"},
				{Kind: tc.kind, Name: "dizzy-run0-lb-0002", ID: "lb-id-2"},
			}
			if !slices.Equal(got, want) {
				t.Errorf("ListByTag = %+v, want %+v", got, want)
			}
			f.wantLines(t, "GET "+tc.path+"?"+tagQuery)
			if s := statsOf(m.Aggregate(time.Second), string(tc.kind)); s.Attempted != 1 || s.Succeeded != 1 {
				t.Errorf("%s stats = %+v, want one successful list", tc.kind, s)
			}
		})
	}
}

// TestListByTagFailures confirms a failed list is wrapped with the kind and
// keeps its status, and a kind cleanup never discovers is refused without a
// request.
func TestListByTagFailures(t *testing.T) {
	t.Run("list error", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{"GET " + loadBalancersPath: {{http.StatusInternalServerError, `{}`}}}))
		c, _ := f.client()
		_, err := c.ListByTag(context.Background(), KindLoadBalancer, "run0")
		if err == nil || !strings.HasPrefix(err.Error(), "listing loadbalancer by tag: ") || httpStatus(err) != http.StatusInternalServerError {
			t.Errorf("ListByTag = %v, want the wrapped 500", err)
		}
	})
	t.Run("unsupported kind", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{}))
		c, _ := f.client()
		_, err := c.ListByTag(context.Background(), KindListener, "run0")
		if err == nil || err.Error() != `list by tag not supported for kind "listener"` {
			t.Errorf("ListByTag(listener) = %v, want the unsupported-kind error", err)
		}
		f.wantLines(t)
	})
}

// projectClient builds a Client for run0 whose load-balancer side is lb and
// whose network side hits the fake the way f.client() builds it, recording into
// the returned Collector.
func projectClient(f *fake, lb *gophercloud.ServiceClient) (*Client, *metrics.Collector) {
	m := metrics.NewCollector()
	network := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       f.srv.URL + "/",
	}
	return New(lb, network, "run0", m), m
}

// listItem renders one object of a list answer with its id, name, project, and
// tags. An empty project leaves project_id out of the object.
func listItem(id, name, project string, tags ...string) string {
	quoted := make([]string, 0, len(tags))
	for _, tag := range tags {
		quoted = append(quoted, `"`+tag+`"`)
	}
	projectField := ""
	if project != "" {
		projectField = fmt.Sprintf(`"project_id":%q,`, project)
	}
	return fmt.Sprintf(`{"id":%q,"name":%q,%s"tags":[%s]}`, id, name, projectField, strings.Join(quoted, ","))
}

// TestListByTypeTag confirms each discoverable kind is listed filtered to the
// type tag and to the token's project server side; that the items of any run
// in that project are returned; that an item of another project, an item
// without the type tag, and an item whose answer omits its project are dropped
// client side when a server returns them anyway; and that the list is recorded
// under <kind>/list.
func TestListByTypeTag(t *testing.T) {
	for _, tc := range []struct {
		kind       resource.Kind
		path       string
		collection string
	}{
		{KindLoadBalancer, loadBalancersPath, "loadbalancers"},
		{KindSubnet, "/subnets", "subnets"},
		{KindNetwork, "/networks", "networks"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			typeTag := "dizzy:type=" + string(tc.kind)
			body := `{"` + tc.collection + `":[` + strings.Join([]string{
				listItem("id-1", "dizzy-run1-x-0001", "proj-1", "dizzy:run=run1", typeTag),
				listItem("id-9", "dizzy-run9-x-0001", "proj-9", "dizzy:run=run9", typeTag),
				listItem("id-u", "dizzy-run1-x-0002", "proj-1", "dizzy:run=run1"),
				listItem("id-n", "dizzy-run1-x-0003", "", "dizzy:run=run1", typeTag),
				listItem("id-2", "dizzy-run2-x-0001", "proj-1", "dizzy:run=run2", typeTag),
			}, ",") + `]}`
			f := newFake(t, scripted(t, map[string][]reply{"GET " + tc.path: {{http.StatusOK, body}}}))
			c, m := projectClient(f, quotaClient(t, f))

			got, err := c.ListByTypeTag(context.Background(), tc.kind)
			if err != nil {
				t.Fatalf("ListByTypeTag: %v", err)
			}
			want := []resource.Resource{
				{Kind: tc.kind, Name: "dizzy-run1-x-0001", ID: "id-1"},
				{Kind: tc.kind, Name: "dizzy-run2-x-0001", ID: "id-2"},
			}
			if !slices.Equal(got, want) {
				t.Errorf("ListByTypeTag = %+v, want %+v", got, want)
			}
			f.wantLines(t, "GET "+tc.path+"?project_id=proj-1&tags=dizzy%3Atype%3D"+string(tc.kind))
			if s := statsOf(m.Aggregate(time.Second), string(tc.kind)); s.Attempted != 1 || s.Succeeded != 1 {
				t.Errorf("%s stats = %+v, want one successful list", tc.kind, s)
			}
		})
	}
}

// TestListByTagKeepsEveryProject confirms a run-tag listing stays unscoped even
// on a client whose token names a project: it sends no project_id and keeps the
// run's items whatever project they name, so cleanup, status, and teardown
// reach every object of the run.
func TestListByTagKeepsEveryProject(t *testing.T) {
	for _, tc := range []struct {
		kind       resource.Kind
		path       string
		collection string
	}{
		{KindLoadBalancer, loadBalancersPath, "loadbalancers"},
		{KindSubnet, "/subnets", "subnets"},
		{KindNetwork, "/networks", "networks"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			body := `{"` + tc.collection + `":[` +
				listItem("id-1", "dizzy-run0-x-0001", "proj-1", "dizzy:run=run0") + `,` +
				listItem("id-9", "dizzy-run0-x-0002", "proj-9", "dizzy:run=run0") + `]}`
			f := newFake(t, scripted(t, map[string][]reply{"GET " + tc.path: {{http.StatusOK, body}}}))
			c, _ := projectClient(f, quotaClient(t, f))

			got, err := c.ListByTag(context.Background(), tc.kind, "run0")
			if err != nil {
				t.Fatalf("ListByTag: %v", err)
			}
			want := []resource.Resource{
				{Kind: tc.kind, Name: "dizzy-run0-x-0001", ID: "id-1"},
				{Kind: tc.kind, Name: "dizzy-run0-x-0002", ID: "id-9"},
			}
			if !slices.Equal(got, want) {
				t.Errorf("ListByTag = %+v, want %+v", got, want)
			}
			f.wantLines(t, "GET "+tc.path+"?"+tagQuery)
		})
	}
}

// TestListByTypeTagEmpty confirms an empty list answer is an empty result, not
// an error.
func TestListByTypeTagEmpty(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{"GET " + loadBalancersPath: {{http.StatusOK, `{"loadbalancers":[]}`}}}))
	c, _ := projectClient(f, quotaClient(t, f))

	got, err := c.ListByTypeTag(context.Background(), KindLoadBalancer)
	if err != nil || len(got) != 0 {
		t.Errorf("ListByTypeTag = (%+v, %v), want an empty result and no error", got, err)
	}
}

// TestListByTypeTagWithoutProject confirms a client whose auth result names no
// project, because there is none or because the token is domain-scoped, refuses
// the listing with ErrNoProject before any request or sample, so the sweep never
// lists beyond the token's project.
func TestListByTypeTagWithoutProject(t *testing.T) {
	for name, build := range map[string]func(t *testing.T, f *fake) (*Client, *metrics.Collector){
		"no auth result": func(_ *testing.T, f *fake) (*Client, *metrics.Collector) { return f.client() },
		"domain-scoped": func(t *testing.T, f *fake) (*Client, *metrics.Collector) {
			return projectClient(f, domainScopedClient(t, f))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{}))
			c, m := build(t, f)

			_, err := c.ListByTypeTag(context.Background(), KindLoadBalancer)
			const want = "listing loadbalancer by type tag: octavia: project id unavailable from the auth result; the sweep never lists beyond the token's project"
			if !errors.Is(err, ErrNoProject) || err.Error() != want {
				t.Errorf("ListByTypeTag = %v, want %q wrapping ErrNoProject", err, want)
			}
			f.wantLines(t)
			if n := m.Aggregate(time.Second).Overall.Attempted; n != 0 {
				t.Errorf("ListByTypeTag recorded %d samples, want none", n)
			}
		})
	}
}

// TestListByTypeTagFailures confirms a failed type-tag list is wrapped with the
// kind and keeps its status, and a kind the sweep never discovers is refused
// without a request.
func TestListByTypeTagFailures(t *testing.T) {
	t.Run("list error", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{"GET " + loadBalancersPath: {{http.StatusInternalServerError, `{}`}}}))
		c, _ := projectClient(f, quotaClient(t, f))
		_, err := c.ListByTypeTag(context.Background(), KindLoadBalancer)
		if err == nil || !strings.HasPrefix(err.Error(), "listing loadbalancer by tag: ") || httpStatus(err) != http.StatusInternalServerError {
			t.Errorf("ListByTypeTag = %v, want the wrapped 500", err)
		}
	})
	t.Run("unsupported kind", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{}))
		c, _ := projectClient(f, quotaClient(t, f))
		_, err := c.ListByTypeTag(context.Background(), KindListener)
		if err == nil || err.Error() != `list by tag not supported for kind "listener"` {
			t.Errorf("ListByTypeTag(listener) = %v, want the unsupported-kind error", err)
		}
		f.wantLines(t)
	})
}

// TestObserve confirms Observe returns the provisioning status of each Octavia
// kind, the status of a VIP network, an empty status for an existing subnet,
// ("", false, nil) on a 404, and any other error, recording each read under
// <kind>/get; and that a member, whose pool its Resource does not carry, is
// refused without a request.
func TestObserve(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kind       resource.Kind
		id         string
		path       string
		rep        reply
		wantStatus string
		wantExists bool
		wantErr    bool
	}{
		{"loadbalancer", KindLoadBalancer, "lb-id-1", loadBalancerPath, reply{http.StatusOK, lbAnswer("ACTIVE", "ovn")}, "ACTIVE", true, false},
		{"listener", KindListener, "id-1", "/v2.0/lbaas/listeners/id-1",
			reply{http.StatusOK, `{"listener":` + item("id-1", "dizzy-run0-lsn-0001", "PENDING_UPDATE") + `}`}, "PENDING_UPDATE", true, false},
		{"pool", KindPool, "id-1", "/v2.0/lbaas/pools/id-1",
			reply{http.StatusOK, `{"pool":` + item("id-1", "dizzy-run0-pool-0001", "ACTIVE") + `}`}, "ACTIVE", true, false},
		{"healthmonitor", KindHealthMonitor, "id-1", "/v2.0/lbaas/healthmonitors/id-1",
			reply{http.StatusOK, `{"healthmonitor":` + item("id-1", "dizzy-run0-hm-0001", "ERROR") + `}`}, "ERROR", true, false},
		{"network", KindNetwork, "id-1", "/networks/id-1", reply{http.StatusOK, `{"network":{"id":"id-1","status":"ACTIVE"}}`}, "ACTIVE", true, false},
		{"subnet", KindSubnet, "id-1", "/subnets/id-1", reply{http.StatusOK, `{"subnet":{"id":"id-1"}}`}, "", true, false},
		{"gone", KindPool, "id-1", "/v2.0/lbaas/pools/id-1", reply{http.StatusNotFound, `{}`}, "", false, false},
		{"error", KindListener, "id-1", "/v2.0/lbaas/listeners/id-1", reply{http.StatusInternalServerError, `{}`}, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{"GET " + tc.path: {tc.rep}}))
			c, m := f.client()

			status, exists, err := c.Observe(context.Background(), resource.Resource{Kind: tc.kind, ID: tc.id})
			if status != tc.wantStatus || exists != tc.wantExists || (err != nil) != tc.wantErr {
				t.Errorf("Observe = (%q, %v, %v), want (%q, %v, error %v)", status, exists, err, tc.wantStatus, tc.wantExists, tc.wantErr)
			}
			f.wantLines(t, "GET "+tc.path)
			if s := statsOf(m.Aggregate(time.Second), string(tc.kind)); s.Attempted != 1 {
				t.Errorf("%s stats = %+v, want one get recorded", tc.kind, s)
			}
		})
	}
	t.Run("member without its pool", func(t *testing.T) {
		f := newFake(t, scripted(t, map[string][]reply{}))
		c, _ := f.client()
		_, _, err := c.Observe(context.Background(), resource.Resource{Kind: KindMember, Logical: "pool-0001-member-01", ID: "mem-id-1"})
		if want := `observing member "pool-0001-member-01" needs its pool; use ObserveMember`; err == nil || err.Error() != want {
			t.Errorf("Observe(member) = %v, want %q", err, want)
		}
		f.wantLines(t)
	})
}

// TestObserveMember confirms a member is read below its pool and reports its
// provisioning status, and reads as gone on a 404.
func TestObserveMember(t *testing.T) {
	const path = "/v2.0/lbaas/pools/pool-id-1/members/mem-id-1"
	member := resource.Resource{Kind: KindMember, Logical: "pool-0001-member-01", ID: "mem-id-1"}
	f := newFake(t, scripted(t, map[string][]reply{"GET " + path: {
		{http.StatusOK, `{"member":` + item("mem-id-1", "dizzy-run0-pool-0001-member-01", "ACTIVE") + `}`},
		{http.StatusNotFound, `{}`},
	}}))
	c, m := f.client()

	if status, exists, err := c.ObserveMember(context.Background(), member, "pool-id-1"); status != "ACTIVE" || !exists || err != nil {
		t.Errorf("ObserveMember = (%q, %v, %v), want (ACTIVE, true, nil)", status, exists, err)
	}
	if status, exists, err := c.ObserveMember(context.Background(), member, "pool-id-1"); status != "" || exists || err != nil {
		t.Errorf("ObserveMember of a gone member = (%q, %v, %v), want (\"\", false, nil)", status, exists, err)
	}
	f.wantLines(t, "GET "+path, "GET "+path)
	if s := statsOf(m.Aggregate(time.Second), "member"); s.Attempted != 2 {
		t.Errorf("member stats = %+v, want two gets recorded", s)
	}
}
