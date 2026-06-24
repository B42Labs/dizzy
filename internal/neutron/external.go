package neutron

import (
	"context"
	"fmt"
	"sort"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/external"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
)

// ExternalNetwork is an external (router:external=true) network a router gateway
// or floating IP can be attached to.
type ExternalNetwork struct {
	ID   string
	Name string
}

// externalNetwork decodes a network together with its router:external flag, so
// the discovery below can filter to genuinely external networks even on clouds
// where the server-side filter is not honored.
type externalNetwork struct {
	networks.Network
	external.NetworkExternalExt
}

// FindExternalNetwork discovers the external network to use for router gateways
// and floating IPs. When name is set it returns the external network with that
// name, erroring if it does not exist or is not external. When name is empty it
// returns the first external network in name order, or ok=false when the cloud
// has none. The selection is deterministic (sorted by name) so repeated runs
// against the same cloud pick the same network. The lookup is a plain API call,
// kept out of the run metrics because it is discovery, not part of the workload.
func FindExternalNetwork(ctx context.Context, gc *gophercloud.ServiceClient, name string) (ExternalNetwork, bool, error) {
	isExternal := true
	opts := external.ListOptsExt{ListOptsBuilder: networks.ListOpts{}, External: &isExternal}
	pages, err := networks.List(gc, opts).AllPages(ctx)
	if err != nil {
		return ExternalNetwork{}, false, fmt.Errorf("listing external networks: %w", err)
	}

	var found []externalNetwork
	if err := networks.ExtractNetworksInto(pages, &found); err != nil {
		return ExternalNetwork{}, false, fmt.Errorf("extracting external networks: %w", err)
	}

	candidates := make([]ExternalNetwork, 0, len(found))
	for _, n := range found {
		// Defend against a cloud that ignores the router:external query filter by
		// re-checking the decoded flag.
		if !n.External {
			continue
		}
		if name != "" && n.Name != name {
			continue
		}
		candidates = append(candidates, ExternalNetwork{ID: n.ID, Name: n.Name})
	}

	if len(candidates) == 0 {
		if name != "" {
			return ExternalNetwork{}, false, fmt.Errorf("external network %q not found (or not external)", name)
		}
		return ExternalNetwork{}, false, nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Name != candidates[j].Name {
			return candidates[i].Name < candidates[j].Name
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates[0], true, nil
}
