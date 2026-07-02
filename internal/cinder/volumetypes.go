package cinder

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumetypes"
)

// VolumeType is a Cinder volume type a volume can be created with.
type VolumeType struct {
	ID   string
	Name string
}

// FindVolumeType resolves the volume type named name, matching by name (or id).
// It errors with an actionable message naming the available types when name
// does not exist, so a typo in --volume-type fails clearly before any volume is
// created. The lookup is a plain API call, kept out of the run metrics because
// it is discovery, not part of the workload.
func FindVolumeType(ctx context.Context, gc *gophercloud.ServiceClient, name string) (VolumeType, error) {
	pages, err := volumetypes.List(gc, volumetypes.ListOpts{}).AllPages(ctx)
	if err != nil {
		return VolumeType{}, fmt.Errorf("listing volume types: %w", err)
	}
	types, err := volumetypes.ExtractVolumeTypes(pages)
	if err != nil {
		return VolumeType{}, fmt.Errorf("extracting volume types: %w", err)
	}

	available := make([]string, 0, len(types))
	for _, t := range types {
		if t.Name == name || t.ID == name {
			return VolumeType{ID: t.ID, Name: t.Name}, nil
		}
		available = append(available, t.Name)
	}

	sort.Strings(available)
	return VolumeType{}, fmt.Errorf("volume type %q not found; available types: %s", name, strings.Join(available, ", "))
}
