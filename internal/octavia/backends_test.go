package octavia

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// subnetsQuery and portsQuery are the requests the address lookups of the
// server srv1 on the network net1 send. The network client of the fake has no
// v2.0/ ResourceBase, so both collections sit at the root.
const (
	subnetsQuery = "GET /subnets?network_id=net1"
	portsQuery   = "GET /ports?device_id=srv1&network_id=net1"
)

// TestAddressLookupsRefuseEmptyIDs confirms NetworkSubnet("") and
// ServerAddress with an empty server or network id fail with their empty-id
// errors without a request or a sample, since Neutron drops an empty filter
// and the list would return every subnet or port of the project.
func TestAddressLookupsRefuseEmptyIDs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lookup func(c *Client) error
		want   string
	}{
		{"network subnet", func(c *Client) error {
			_, err := c.NetworkSubnet(context.Background(), "")
			return err
		}, "network subnet lookup: empty network id"},
		{"server address without server", func(c *Client) error {
			_, _, err := c.ServerAddress(context.Background(), "", "net1")
			return err
		}, "server address lookup: empty server id or network id"},
		{"server address without network", func(c *Client) error {
			_, _, err := c.ServerAddress(context.Background(), "srv1", "")
			return err
		}, "server address lookup: empty server id or network id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{}))
			c, m := f.client()

			if err := tc.lookup(c); err == nil || err.Error() != tc.want {
				t.Errorf("lookup = %v, want %q", err, tc.want)
			}
			f.wantLines(t)
			if n := m.Aggregate(time.Second).Overall.Attempted; n != 0 {
				t.Errorf("lookup recorded %d samples, want none", n)
			}
		})
	}
}

// TestNetworkSubnet confirms NetworkSubnet lists the subnets of the network
// filtered to its id, returns the id of the one subnet, and fails for a
// network with none or with two. Each lookup records one subnet sample, and
// it is successful even when the answer fails the lookup, because the sample
// records the list Neutron answered.
func TestNetworkSubnet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		want    string
		wantErr string
	}{
		{"one subnet", `{"subnets":[{"id":"sub-id-1","network_id":"net1"}]}`, "sub-id-1", ""},
		{"no subnet", `{"subnets":[]}`, "", "network net1 has no subnet"},
		{"two subnets", `{"subnets":[{"id":"sub-id-1","network_id":"net1"},{"id":"sub-id-2","network_id":"net1"}]}`,
			"", "network net1 has 2 subnets, want 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{"GET /subnets": {{http.StatusOK, tc.body}}}))
			c, m := f.client()

			got, err := c.NetworkSubnet(context.Background(), "net1")
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("NetworkSubnet: %v", err)
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
				t.Errorf("NetworkSubnet = %v, want %q", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("NetworkSubnet = %q, want %q", got, tc.want)
			}
			f.wantLines(t, subnetsQuery)
			agg := m.Aggregate(time.Second)
			if s := statsOf(agg, "subnet"); s.Attempted != 1 || s.Succeeded != 1 {
				t.Errorf("subnet stats = %+v, want one successful list", s)
			}
			if n := agg.Overall.Attempted; n != 1 {
				t.Errorf("NetworkSubnet recorded %d samples, want 1", n)
			}
		})
	}
}

// TestNetworkSubnetListError confirms a failed subnet list is wrapped with the
// network id, keeps the gophercloud error so IsRetryable classifies a 500 as
// transient, and records one failed subnet sample.
func TestNetworkSubnetListError(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{"GET /subnets": {{http.StatusInternalServerError, `{}`}}}))
	c, m := f.client()

	got, err := c.NetworkSubnet(context.Background(), "net1")
	const prefix = "listing subnets of network net1: "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("NetworkSubnet = %v, want an error starting with %q", err, prefix)
	}
	if got != "" {
		t.Errorf("NetworkSubnet = %q, want no subnet id", got)
	}
	if httpStatus(err) != http.StatusInternalServerError || !IsRetryable(err) {
		t.Errorf("NetworkSubnet error %v: want a retryable 500 in the chain", err)
	}
	f.wantLines(t, subnetsQuery)
	if s := statsOf(m.Aggregate(time.Second), "subnet"); s.Attempted != 1 || s.Failed != 1 {
		t.Errorf("subnet stats = %+v, want one failed list", s)
	}
}

// TestServerAddress confirms ServerAddress lists the ports of the server on
// the network, filtered by device_id and network_id, and returns the first
// IPv4 fixed IP and its subnet id in port and fixed-IP order, passing over an
// IPv6 address. A server without an IPv4 address on the network, whether it
// has no port there or only an IPv6 one, fails the lookup. Each lookup records
// one successful port sample, because the sample records the list Neutron
// answered.
func TestServerAddress(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantAddr   string
		wantSubnet string
		wantErr    string
	}{
		{"IPv6 then IPv4 fixed IP", `{"ports":[{"id":"port-id-1","device_id":"srv1","network_id":"net1","fixed_ips":[` +
			`{"subnet_id":"sub-v6","ip_address":"fd00::5"},{"subnet_id":"sub-v4","ip_address":"10.0.1.5"}]}]}`,
			"10.0.1.5", "sub-v4", ""},
		{"IPv4 on the second port", `{"ports":[` +
			`{"id":"port-id-1","fixed_ips":[{"subnet_id":"sub-v6","ip_address":"fd00::5"}]},` +
			`{"id":"port-id-2","fixed_ips":[{"subnet_id":"sub-v4","ip_address":"10.0.1.6"},{"subnet_id":"sub-v4b","ip_address":"10.0.2.6"}]}]}`,
			"10.0.1.6", "sub-v4", ""},
		{"no port", `{"ports":[]}`, "", "", "server srv1 has no IPv4 address on network net1"},
		{"IPv6 only", `{"ports":[{"id":"port-id-1","fixed_ips":[{"subnet_id":"sub-v6","ip_address":"fd00::5"}]}]}`,
			"", "", "server srv1 has no IPv4 address on network net1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, scripted(t, map[string][]reply{"GET /ports": {{http.StatusOK, tc.body}}}))
			c, m := f.client()

			addr, subnet, err := c.ServerAddress(context.Background(), "srv1", "net1")
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ServerAddress: %v", err)
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
				t.Errorf("ServerAddress = %v, want %q", err, tc.wantErr)
			}
			if addr != tc.wantAddr || subnet != tc.wantSubnet {
				t.Errorf("ServerAddress = (%q, %q), want (%q, %q)", addr, subnet, tc.wantAddr, tc.wantSubnet)
			}
			f.wantLines(t, portsQuery)
			agg := m.Aggregate(time.Second)
			if s := statsOf(agg, "port"); s.Attempted != 1 || s.Succeeded != 1 {
				t.Errorf("port stats = %+v, want one successful list", s)
			}
			if n := agg.Overall.Attempted; n != 1 {
				t.Errorf("ServerAddress recorded %d samples, want 1", n)
			}
		})
	}
}

// TestServerAddressListError confirms a failed port list is wrapped with the
// server and network ids, keeps the gophercloud error so IsRetryable
// classifies a 500 as transient, and records one failed port sample.
func TestServerAddressListError(t *testing.T) {
	f := newFake(t, scripted(t, map[string][]reply{"GET /ports": {{http.StatusInternalServerError, `{}`}}}))
	c, m := f.client()

	addr, subnet, err := c.ServerAddress(context.Background(), "srv1", "net1")
	const prefix = "listing ports of server srv1 on network net1: "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("ServerAddress = %v, want an error starting with %q", err, prefix)
	}
	if addr != "" || subnet != "" {
		t.Errorf("ServerAddress = (%q, %q), want no address", addr, subnet)
	}
	if httpStatus(err) != http.StatusInternalServerError || !IsRetryable(err) {
		t.Errorf("ServerAddress error %v: want a retryable 500 in the chain", err)
	}
	f.wantLines(t, portsQuery)
	if s := statsOf(m.Aggregate(time.Second), "port"); s.Attempted != 1 || s.Failed != 1 {
		t.Errorf("port stats = %+v, want one failed list", s)
	}
}
