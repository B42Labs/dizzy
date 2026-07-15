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

// NewBlockStorageClient authenticates against the cloud described in clouds.yaml
// and returns a BlockStorageV3 (Cinder) service client. When cloudName is empty
// the cloud is selected from the OS_CLOUD environment variable, following the
// standard clouds.yaml search paths.
func NewBlockStorageClient(ctx context.Context, cloudName string) (*gophercloud.ServiceClient, error) {
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

	client, err := openstack.NewBlockStorageV3(provider, endpointOptions)
	if err != nil {
		return nil, fmt.Errorf("creating block storage v3 client: %w", err)
	}

	return client, nil
}

// NewIdentityClient authenticates against the cloud described in clouds.yaml and
// returns an IdentityV3 (Keystone) service client. When cloudName is empty the
// cloud is selected from the OS_CLOUD environment variable, following the
// standard clouds.yaml search paths.
func NewIdentityClient(ctx context.Context, cloudName string) (*gophercloud.ServiceClient, error) {
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

	client, err := openstack.NewIdentityV3(provider, endpointOptions)
	if err != nil {
		return nil, fmt.Errorf("creating identity v3 client: %w", err)
	}

	return client, nil
}

// NewImageClient authenticates against the cloud described in clouds.yaml and
// returns an ImageV2 (Glance) service client. When cloudName is empty the cloud
// is selected from the OS_CLOUD environment variable, following the standard
// clouds.yaml search paths.
func NewImageClient(ctx context.Context, cloudName string) (*gophercloud.ServiceClient, error) {
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

	client, err := openstack.NewImageV2(provider, endpointOptions)
	if err != nil {
		return nil, fmt.Errorf("creating image v2 client: %w", err)
	}

	return client, nil
}

// ComputeStack bundles the four service clients a Nova run needs, all built from
// one authentication: Compute (Nova) for servers and their attachments, Network
// (Neutron) for the companion networks/subnets/ports, BlockStorage (Cinder) for
// the data volumes, and Image (Glance) for resolving the boot image by name.
type ComputeStack struct {
	Compute      *gophercloud.ServiceClient
	Network      *gophercloud.ServiceClient
	BlockStorage *gophercloud.ServiceClient
	Image        *gophercloud.ServiceClient
}

// novaMicroversion is the compute API microversion the stack pins its Nova
// client to. It is required so os-migrateLive accepts block_migration: "auto"
// (valid at 2.25 / Mitaka); every other compute call the run makes is unchanged
// under 2.25.
const novaMicroversion = "2.25"

// NewComputeStack authenticates once against the cloud described in clouds.yaml
// and returns the four service clients a Nova run drives. When cloudName is empty
// the cloud is selected from the OS_CLOUD environment variable, following the
// standard clouds.yaml search paths.
func NewComputeStack(ctx context.Context, cloudName string) (*ComputeStack, error) {
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

	compute, err := openstack.NewComputeV2(provider, endpointOptions)
	if err != nil {
		return nil, fmt.Errorf("creating compute v2 client: %w", err)
	}
	compute.Microversion = novaMicroversion

	network, err := openstack.NewNetworkV2(provider, endpointOptions)
	if err != nil {
		return nil, fmt.Errorf("creating network v2 client: %w", err)
	}

	blockStorage, err := openstack.NewBlockStorageV3(provider, endpointOptions)
	if err != nil {
		return nil, fmt.Errorf("creating block storage v3 client: %w", err)
	}

	image, err := openstack.NewImageV2(provider, endpointOptions)
	if err != nil {
		return nil, fmt.Errorf("creating image v2 client: %w", err)
	}

	return &ComputeStack{Compute: compute, Network: network, BlockStorage: blockStorage, Image: image}, nil
}
