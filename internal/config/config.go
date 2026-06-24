// Package config builds authenticated OpenStack service clients from the
// standard clouds.yaml configuration.
package config

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	gcconfig "github.com/gophercloud/gophercloud/v2/openstack/config"
	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
)

// NewNetworkClient authenticates against the cloud described in clouds.yaml and
// returns a NetworkV2 (Neutron) service client. When cloudName is empty the
// cloud is selected from the OS_CLOUD environment variable, following the
// standard clouds.yaml search paths.
func NewNetworkClient(ctx context.Context, cloudName string) (*gophercloud.ServiceClient, error) {
	var parseOpts []clouds.ParseOption
	if cloudName != "" {
		parseOpts = append(parseOpts, clouds.WithCloudName(cloudName))
	}

	authOptions, endpointOptions, tlsConfig, err := clouds.Parse(parseOpts...)
	if err != nil {
		return nil, fmt.Errorf("parsing clouds.yaml: %w", err)
	}

	provider, err := gcconfig.NewProviderClient(ctx, authOptions, gcconfig.WithTLSConfig(tlsConfig))
	if err != nil {
		return nil, fmt.Errorf("creating provider client: %w", err)
	}

	client, err := openstack.NewNetworkV2(provider, endpointOptions)
	if err != nil {
		return nil, fmt.Errorf("creating network v2 client: %w", err)
	}

	return client, nil
}
