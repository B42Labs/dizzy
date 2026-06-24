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

	"github.com/B42Labs/openstack-tester/internal/config"
	"github.com/B42Labs/openstack-tester/internal/metrics"
	"github.com/B42Labs/openstack-tester/internal/neutron"
	"github.com/B42Labs/openstack-tester/internal/plan"
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

	router, err := c.CreateRouter(ctx, plan.Router{Name: "router-0001"})
	if err != nil {
		t.Fatalf("CreateRouter: %v", err)
	}
	deleteOnCleanup(router)
	assertTagged("routers", router.ID)

	rif, err := c.CreateRouterInterface(ctx, plan.RouterInterface{
		Name: "rif-0001", Router: "router-0001", Subnet: "subnet-0001",
	}, router.ID, sub.ID)
	if err != nil {
		t.Fatalf("CreateRouterInterface: %v", err)
	}
	if rif.ID == "" {
		t.Error("router interface returned an empty port id")
	}
	// The interface must be detached before the router or subnet can be deleted.
	cleanups = append(cleanups, func(ctx context.Context) {
		opts := routers.RemoveInterfaceOpts{SubnetID: sub.ID}
		if _, err := routers.RemoveInterface(ctx, gc, router.ID, opts).Extract(); err != nil {
			t.Logf("cleanup: detaching interface from router %s: %v", router.ID, err)
		}
	})

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

	// Time-to-ready and Get both round-trip against a live resource.
	if err := c.WaitForReady(ctx, port); err != nil {
		t.Errorf("WaitForReady(port): %v", err)
	}
	if status, err := c.Get(ctx, net); err != nil || status == "" {
		t.Errorf("Get(network) = %q, %v; want a status and no error", status, err)
	}
}
