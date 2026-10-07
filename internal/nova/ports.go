package nova

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/attachinterfaces"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"

	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// CreatePort creates a tagged port on networkID. It is created unattached; the
// executor attaches it to its server as a separate step (an attachinterfaces
// call), which the plan can later detach.
func (c *Client) CreatePort(ctx context.Context, pt novaplan.Port, networkID string) (resource.Resource, error) {
	return c.createTagged(ctx, KindPort, pt.Name, func(ctx context.Context, name string) (string, error) {
		created, err := ports.Create(ctx, c.network, ports.CreateOpts{NetworkID: networkID, Name: name}).Extract()
		if err != nil {
			return "", err
		}
		return created.ID, nil
	})
}

// AttachPort attaches an existing port to a server through the compute
// attach-interface API. It records under the port type and the attach operation.
// A repeated attach can hit a 409 (the port is in use) because an earlier
// request whose answer was lost already attached it. When the server then has
// the port's interface attachment, that 409 confirms the earlier attach
// committed, so it is treated as success; otherwise the 409 surfaces.
func (c *Client) AttachPort(ctx context.Context, server, port resource.Resource) error {
	err := c.timed(ctx, string(KindPort), "attach", func(ctx context.Context) error {
		_, err := attachinterfaces.Create(ctx, c.compute, server.ID, attachinterfaces.CreateOpts{PortID: port.ID}).Extract()
		return err
	})
	if err != nil && gophercloud.ResponseCodeIs(err, 409) {
		if _, gerr := attachinterfaces.Get(ctx, c.compute, server.ID, port.ID).Extract(); gerr == nil {
			slog.Info("port already attached; treating a repeated attach as success",
				"port", port.Logical, "server", server.Logical, "id", port.ID)
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("attaching port %q to server %q: %w", port.Logical, server.Logical, err)
	}
	return nil
}

// DetachPort detaches a port from a server. A 404 (already detached) surfaces to
// the caller, which treats it as success to keep the operation idempotent.
func (c *Client) DetachPort(ctx context.Context, server, port resource.Resource) error {
	err := c.timed(ctx, string(KindPort), "detach", func(ctx context.Context) error {
		return attachinterfaces.Delete(ctx, c.compute, server.ID, port.ID).ExtractErr()
	})
	if err != nil {
		return fmt.Errorf("detaching port %q from server %q: %w", port.Logical, server.Logical, err)
	}
	return nil
}

// WaitForPortDetached waits until the server no longer has the port, both in
// Nova's interface view and in the server's own addresses. DetachPort returns
// once Nova accepted the detach, not once it is done, so a re-attach or a move
// of the server must wait here first. It polls the server's interface
// attachment of the port until Nova answers 404. That view clears when Neutron
// clears the port's device_id, which precedes the compute host's refresh of
// the instance's network info cache, the view Nova's own move operations read.
// So it then reads the port's MAC and fixed IPs from Neutron, since the
// attachment that carried them is gone, and polls the server until its
// addresses no longer carry them. A Neutron 404 ends the wait, as nothing is
// left to match and the next attach of the port fails on its own. A server 404
// ends it too, as the interface poll ends on the 404 Nova answers for a
// deleted server. Every other answer, an error other than a 404 included, is
// polled again with the backoff WaitForGone uses, carried across the steps:
// the caller's context is the only bound, and it returns ctx.Err() when that
// ends first. It records no metrics sample.
func (c *Client) WaitForPortDetached(ctx context.Context, server, port resource.Resource) error {
	backoff := 200 * time.Millisecond
	pause := func() error {
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff = time.Duration(float64(backoff) * 1.5); backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
		return nil
	}

	for {
		if _, err := attachinterfaces.Get(ctx, c.compute, server.ID, port.ID).Extract(); IsNotFound(err) {
			break
		}
		if werr := pause(); werr != nil {
			return werr
		}
	}

	var mac string
	var ips []string
	for {
		p, err := ports.Get(ctx, c.network, port.ID).Extract()
		if IsNotFound(err) {
			slog.Info("port gone from Neutron; nothing left to wait for", "port", port.Logical, "id", port.ID)
			return nil
		}
		if err == nil {
			mac = p.MACAddress
			for _, ip := range p.FixedIPs {
				ips = append(ips, ip.IPAddress)
			}
			break
		}
		if werr := pause(); werr != nil {
			return werr
		}
	}

	for {
		s, err := servers.Get(ctx, c.compute, server.ID).Extract()
		if IsNotFound(err) {
			return nil
		}
		if err == nil && !addressesCarry(s.Addresses, mac, ips) {
			return nil
		}
		if werr := pause(); werr != nil {
			return werr
		}
	}
}

// addressesCarry reports whether a server's addresses still list the port
// with mac and ips. Nova keys addresses by network name, and each value is a
// list of entries with the keys addr, version, OS-EXT-IPS-MAC:mac_addr and
// OS-EXT-IPS:type. An entry matches by its MAC under a case-insensitive
// compare, when mac is not empty, or by an addr that is one of ips. A floating
// IP of the port is a "floating" entry with the port's MAC, so it matches by
// MAC. Values and entries of another shape are skipped, and an empty mac with
// no ips matches nothing.
func addressesCarry(addresses map[string]any, mac string, ips []string) bool {
	for _, v := range addresses {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		for _, e := range list {
			entry, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if got, ok := entry["OS-EXT-IPS-MAC:mac_addr"].(string); ok && mac != "" && strings.EqualFold(got, mac) {
				return true
			}
			if addr, ok := entry["addr"].(string); ok && slices.Contains(ips, addr) {
				return true
			}
		}
	}
	return false
}
