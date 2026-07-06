//go:build integration

package neutron_test

import (
	"context"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/attributestags"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/neutron"
	"github.com/B42Labs/dizzy/internal/plan"
)

// TestWrappers_Integration creates one of each Phase 1 resource against a real
// cloud, asserts every taggable resource carries the run tag, gets it, and
// tears the topology down again. neutron is a ports-and-adapters seam to
// OpenStack, so the external dependency is exercised here rather than mocked.
// Run with: OS_CLOUD=<cloud> go test -tags integration ./internal/neutron/
func TestWrappers_Integration(t *testing.T) {
	if os.Getenv("OS_CLOUD") == "" {
		t.Skip("OS_CLOUD not set; skipping integration test")
	}

	ctx := t.Context()
	gc, err := config.NewNetworkClient(ctx, "")
	if err != nil {
		t.Fatalf("NewNetworkClient: %v", err)
	}

	runID := "it" + strconv.FormatInt(time.Now().Unix(), 36)
	c := neutron.New(gc, runID, metrics.NewCollector())

	// Register cleanups as resources are created and run them in reverse, so a
	// failure partway through still tears down what already exists.
	var cleanups []func(context.Context)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i](cctx)
		}
	})
	deleteOnCleanup := func(r neutron.Resource) {
		cleanups = append(cleanups, func(ctx context.Context) {
			if err := c.Delete(ctx, r); err != nil {
				t.Logf("cleanup: deleting %s %s: %v", r.Kind, r.ID, err)
			}
		})
	}

	assertTagged := func(collection, id string) {
		t.Helper()
		tags, err := attributestags.List(ctx, gc, collection, id).Extract()
		if err != nil {
			t.Fatalf("listing tags on %s %s: %v", collection, id, err)
		}
		if !slices.Contains(tags, "ostester:run="+runID) {
			t.Errorf("%s %s missing run tag; tags=%v", collection, id, tags)
		}
	}

	net, err := c.CreateNetwork(ctx, plan.Network{Name: "net-0001"})
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	deleteOnCleanup(net)
	assertTagged("networks", net.ID)

	sub, err := c.CreateSubnet(ctx, plan.Subnet{
		Name: "subnet-0001", Network: "net-0001", IPVersion: 4, CIDR: "10.231.0.0/24",
	}, net.ID, "")
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	deleteOnCleanup(sub)
	assertTagged("subnets", sub.ID)

	router, err := c.CreateRouter(ctx, plan.Router{Name: "router-0001"}, "")
	if err != nil {
		t.Fatalf("CreateRouter: %v", err)
	}
	deleteOnCleanup(router)
	assertTagged("routers", router.ID)

	rif, err := c.CreateRouterInterface(ctx, plan.RouterInterface{
		Name: "rif-0001", Router: "router-0001", Subnet: "subnet-0001",
	}, router.ID, sub.ID, "")
	if err != nil {
		t.Fatalf("CreateRouterInterface: %v", err)
	}
	if rif.ID == "" {
		t.Error("router interface returned an empty port id")
	}
	// Exercise the per-interface removal wrapper the chaos engine uses: detach
	// this interface now (so the router/subnet can be deleted later) and confirm
	// a repeat detach is a 404 the caller can treat as already-gone. No cleanup is
	// registered for rif-0001 because it is detached here in the body.
	if err := c.RemoveRouterInterface(ctx, router.ID, sub.ID, ""); err != nil {
		t.Fatalf("RemoveRouterInterface: %v", err)
	}
	if err := c.RemoveRouterInterface(ctx, router.ID, sub.ID, ""); err == nil || !neutron.IsNotFound(err) {
		t.Errorf("repeat RemoveRouterInterface = %v, want a 404 (IsNotFound)", err)
	}

	sg, err := c.CreateSecurityGroup(ctx, plan.SecurityGroup{Name: "sg-0001"})
	if err != nil {
		t.Fatalf("CreateSecurityGroup: %v", err)
	}
	deleteOnCleanup(sg)
	assertTagged("security-groups", sg.ID)

	if _, err := c.CreateSecurityGroupRule(ctx, plan.SecurityGroupRule{
		Direction: "ingress", EtherType: "IPv4", Protocol: "tcp",
		PortRangeMin: 80, PortRangeMax: 80, RemoteIPPrefix: "0.0.0.0/0",
	}, sg.ID, ""); err != nil {
		t.Fatalf("CreateSecurityGroupRule: %v", err)
	}

	port, err := c.CreatePort(ctx, plan.Port{
		Name: "port-0001", Network: "net-0001",
		FixedIPs:       []plan.FixedIP{{Subnet: "subnet-0001"}},
		SecurityGroups: []string{"sg-0001"},
	}, net.ID, map[string]string{"subnet-0001": sub.ID}, []string{sg.ID})
	if err != nil {
		t.Fatalf("CreatePort: %v", err)
	}
	deleteOnCleanup(port)
	assertTagged("ports", port.ID)

	// Router-to-router link: a dedicated transit network/subnet, with router-0001
	// attached via the subnet (owning the gateway address) and a second router
	// attached via an explicit port — the mechanism that wires two routers
	// together.
	linkNet, err := c.CreateNetwork(ctx, plan.Network{Name: "link-net-0001"})
	if err != nil {
		t.Fatalf("CreateNetwork(link): %v", err)
	}
	deleteOnCleanup(linkNet)
	linkSub, err := c.CreateSubnet(ctx, plan.Subnet{
		Name: "link-subnet-0001", Network: "link-net-0001", IPVersion: 4, CIDR: "192.168.250.0/30",
	}, linkNet.ID, "")
	if err != nil {
		t.Fatalf("CreateSubnet(link): %v", err)
	}
	deleteOnCleanup(linkSub)
	router2, err := c.CreateRouter(ctx, plan.Router{Name: "router-0002"}, "")
	if err != nil {
		t.Fatalf("CreateRouter(router-0002): %v", err)
	}
	deleteOnCleanup(router2)
	if _, err := c.CreateRouterInterface(ctx, plan.RouterInterface{
		Name: "link-rif-a-0001", Router: "router-0001", Subnet: "link-subnet-0001",
	}, router.ID, linkSub.ID, ""); err != nil {
		t.Fatalf("CreateRouterInterface(link subnet side): %v", err)
	}
	cleanups = append(cleanups, func(ctx context.Context) {
		opts := routers.RemoveInterfaceOpts{SubnetID: linkSub.ID}
		if _, err := routers.RemoveInterface(ctx, gc, router.ID, opts).Extract(); err != nil {
			t.Logf("cleanup: detaching link subnet interface from router %s: %v", router.ID, err)
		}
	})
	linkPort, err := c.CreatePort(ctx, plan.Port{
		Name: "link-port-0001", Network: "link-net-0001",
		FixedIPs: []plan.FixedIP{{Subnet: "link-subnet-0001", IPAddress: "192.168.250.2"}},
	}, linkNet.ID, map[string]string{"link-subnet-0001": linkSub.ID}, nil)
	if err != nil {
		t.Fatalf("CreatePort(link): %v", err)
	}
	deleteOnCleanup(linkPort)
	if _, err := c.CreateRouterInterface(ctx, plan.RouterInterface{
		Name: "link-rif-b-0001", Router: "router-0002", Port: "link-port-0001",
	}, router2.ID, "", linkPort.ID); err != nil {
		t.Fatalf("CreateRouterInterface(link port side): %v", err)
	}
	cleanups = append(cleanups, func(ctx context.Context) {
		opts := routers.RemoveInterfaceOpts{PortID: linkPort.ID}
		if _, err := routers.RemoveInterface(ctx, gc, router2.ID, opts).Extract(); err != nil {
			t.Logf("cleanup: detaching link port interface from router %s: %v", router2.ID, err)
		}
	})

	// External connectivity, exercised only when the cloud has an external
	// network: a gateway router plugged into it and a floating IP allocated from
	// it. Skipped (with a log) otherwise so the test stays runnable everywhere.
	if extNet, ok, err := neutron.FindExternalNetwork(ctx, gc, ""); err != nil {
		t.Logf("external network discovery failed; skipping external checks: %v", err)
	} else if !ok {
		t.Log("no external network available; skipping gateway and floating-IP checks")
	} else {
		gwRouter, err := c.CreateRouter(ctx, plan.Router{Name: "router-0003", ExternalGateway: true}, extNet.ID)
		if err != nil {
			t.Fatalf("CreateRouter(gateway): %v", err)
		}
		deleteOnCleanup(gwRouter)
		assertTagged("routers", gwRouter.ID)

		fip, err := c.CreateFloatingIP(ctx, plan.FloatingIP{Name: "fip-0001"}, extNet.ID, "")
		if err != nil {
			t.Fatalf("CreateFloatingIP: %v", err)
		}
		deleteOnCleanup(fip)
		assertTagged("floatingips", fip.ID)
	}

	// Time-to-ready and Get both round-trip against a live resource.
	if err := c.WaitForReady(ctx, port); err != nil {
		t.Errorf("WaitForReady(port): %v", err)
	}
	if status, err := c.Get(ctx, net); err != nil || status == "" {
		t.Errorf("Get(network) = %q, %v; want a status and no error", status, err)
	}

	// Tag-based discovery (the basis of cleanup) finds this run's network.
	tagged, err := c.ListByTag(ctx, neutron.KindNetwork, runID)
	if err != nil {
		t.Errorf("ListByTag(network): %v", err)
	}
	if !slices.ContainsFunc(tagged, func(r neutron.Resource) bool { return r.ID == net.ID }) {
		t.Errorf("ListByTag(network) did not find %s; got %v", net.ID, tagged)
	}

	// The quota pre-check passes for a one-network plan (or fails open if the
	// project cannot read its own quota), so it must not error here.
	if err := neutron.PrecheckQuota(ctx, gc, &plan.Plan{Networks: []plan.Network{{Name: "net-0001"}}}, false); err != nil {
		t.Errorf("PrecheckQuota for a one-network plan: %v", err)
	}
}
