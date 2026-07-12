package nova

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
)

// Image is a Glance image a server can be booted from. dizzy references images
// by name and never uploads one, so this is only ever a lookup result.
type Image struct {
	ID   string
	Name string
}

// FindImage resolves the image named name against Glance, matching by name. It
// errors with an actionable message when the image does not exist, so a typo in
// the scenario's image reference fails clearly before any server is booted. Like
// FindFlavor it is discovery, not workload, so it is kept out of the run metrics.
func FindImage(ctx context.Context, imageClient *gophercloud.ServiceClient, name string) (Image, error) {
	pages, err := images.List(imageClient, images.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		return Image{}, fmt.Errorf("listing images: %w", err)
	}
	list, err := images.ExtractImages(pages)
	if err != nil {
		return Image{}, fmt.Errorf("extracting images: %w", err)
	}
	for _, img := range list {
		if img.Name == name || img.ID == name {
			return Image{ID: img.ID, Name: img.Name}, nil
		}
	}
	return Image{}, fmt.Errorf("image %q not found; upload it or pass --set image=<name> to name one that exists", name)
}
