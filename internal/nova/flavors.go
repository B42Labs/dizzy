package nova

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
)

// Flavor is a Nova flavor a server can be booted or resized with. VCPUs and RAM
// (in MB) size the plan against the compute quota.
type Flavor struct {
	ID    string
	Name  string
	VCPUs int
	RAM   int
}

// FindFlavor resolves the flavor named name, matching by name (or id). It errors
// with an actionable message naming the available flavors when name does not
// exist, so a typo in image/flavor fails clearly before any server is booted.
// The lookup is a plain API call, kept out of the run metrics because it is
// discovery, not part of the workload — mirroring cinder.FindVolumeType.
func FindFlavor(ctx context.Context, gc *gophercloud.ServiceClient, name string) (Flavor, error) {
	pages, err := flavors.ListDetail(gc, flavors.ListOpts{}).AllPages(ctx)
	if err != nil {
		return Flavor{}, fmt.Errorf("listing flavors: %w", err)
	}
	list, err := flavors.ExtractFlavors(pages)
	if err != nil {
		return Flavor{}, fmt.Errorf("extracting flavors: %w", err)
	}

	available := make([]string, 0, len(list))
	for _, f := range list {
		if f.Name == name || f.ID == name {
			return Flavor{ID: f.ID, Name: f.Name, VCPUs: f.VCPUs, RAM: f.RAM}, nil
		}
		available = append(available, f.Name)
	}

	sort.Strings(available)
	return Flavor{}, fmt.Errorf("flavor %q not found; available flavors: %s", name, strings.Join(available, ", "))
}
