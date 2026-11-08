package octavia

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
)

// typePort is the metrics type of the port list ServerAddress issues. A port
// is no kind this client creates or tags, so it has no Kind constant and is
// recorded under Neutron's own name.
const typePort = "port"

// NetworkSubnet returns the id of the one subnet of the network networkID, the
// subnet a load balancer whose VIP goes on that network is created on. The
// network is one another graph created, the cluster network of the Gardener
// persona, so no create of this client returned its subnet and it is read from
// Neutron instead. An empty networkID is refused without a request, because
// Neutron drops an empty filter from the query and the list would return every
// subnet of the project.
//
// The list is timed under subnet/list, and its sample records the call's own
// outcome: a network with no subnet or with more than one is an error decided
// after the sample is taken, so a list Neutron answered never counts as a
// failed call. A failed list is returned wrapped around the gophercloud error,
// so IsRetryable classifies it.
func (c *Client) NetworkSubnet(ctx context.Context, networkID string) (string, error) {
	if networkID == "" {
		return "", errors.New("network subnet lookup: empty network id")
	}
	var items []subnets.Subnet
	err := c.timed(ctx, string(KindSubnet), "list", func(ctx context.Context) error {
		pages, err := subnets.List(c.network, subnets.ListOpts{NetworkID: networkID}).AllPages(ctx)
		if err != nil {
			return err
		}
		items, err = subnets.ExtractSubnets(pages)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("listing subnets of network %s: %w", networkID, err)
	}
	switch len(items) {
	case 0:
		return "", fmt.Errorf("network %s has no subnet", networkID)
	case 1:
		return items[0].ID, nil
	default:
		return "", fmt.Errorf("network %s has %d subnets, want 1", networkID, len(items))
	}
}

// ServerAddress returns the first IPv4 fixed IP of the server serverID on the
// network networkID and the id of the subnet that address is on: what a member
// backed by that server is created with. A member of a Gardener load balancer
// is a real server another graph created, and no create returns a server's
// fixed IP, so it is read from the server's ports on that network. The ports
// are walked in list order and each port's fixed IPs in order, and the first
// address that parses as IPv4 wins, so the IPv6 address of a dual-stack port
// is passed over. Both ids are required and an empty one is refused without a
// request, because Neutron drops an empty filter from the query and the list
// would return the ports of other servers, up to every port of the project.
//
// The list is timed under port/list, and as in NetworkSubnet its sample
// records the call's own outcome: a server without an IPv4 address on the
// network is an error decided after the sample is taken. A failed list is
// returned wrapped around the gophercloud error, so IsRetryable classifies it.
func (c *Client) ServerAddress(ctx context.Context, serverID, networkID string) (address, subnetID string, err error) {
	if serverID == "" || networkID == "" {
		return "", "", errors.New("server address lookup: empty server id or network id")
	}
	var items []ports.Port
	err = c.timed(ctx, typePort, "list", func(ctx context.Context) error {
		pages, listErr := ports.List(c.network, ports.ListOpts{DeviceID: serverID, NetworkID: networkID}).AllPages(ctx)
		if listErr != nil {
			return listErr
		}
		items, listErr = ports.ExtractPorts(pages)
		return listErr
	})
	if err != nil {
		return "", "", fmt.Errorf("listing ports of server %s on network %s: %w", serverID, networkID, err)
	}
	for _, p := range items {
		for _, ip := range p.FixedIPs {
			if addr, parseErr := netip.ParseAddr(ip.IPAddress); parseErr == nil && addr.Is4() {
				return ip.IPAddress, ip.SubnetID, nil
			}
		}
	}
	return "", "", fmt.Errorf("server %s has no IPv4 address on network %s", serverID, networkID)
}
