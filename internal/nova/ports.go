package nova

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/attachinterfaces"
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
func (c *Client) AttachPort(ctx context.Context, server, port resource.Resource) error {
	err := c.timed(ctx, string(KindPort), "attach", func(ctx context.Context) error {
		_, err := attachinterfaces.Create(ctx, c.compute, server.ID, attachinterfaces.CreateOpts{PortID: port.ID}).Extract()
		return err
	})
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
