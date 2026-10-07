// Package config builds authenticated OpenStack service clients from the
// standard clouds.yaml configuration.
//
// When the clouds.yaml entry holds credentials rather than a pre-issued token,
// every client re-authenticates on a 401 and repeats the request once. A
// request with a raw body must therefore pass an io.ReadSeeker, which
// gophercloud rewinds before the repeat; a plain io.Reader is resent drained.
// net/http may still read the first attempt after its 401 returned, so the body
// must also be an io.Closer whose rewind waits for the transport's Close. A
// request whose 401 is its own outcome, such as authenticating as another user,
// must go through a copy of the client whose ProviderClient has no ReauthFunc.
package config

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	gcconfig "github.com/gophercloud/gophercloud/v2/openstack/config"
	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
)

// newProvider parses the clouds.yaml entry for cloudName (or $OS_CLOUD when
// empty), enables re-authentication, and authenticates one provider client.
func newProvider(ctx context.Context, cloudName string) (*gophercloud.ProviderClient, gophercloud.EndpointOpts, error) {
	var parseOpts []clouds.ParseOption
	if cloudName != "" {
		parseOpts = append(parseOpts, clouds.WithCloudName(cloudName))
	}

	authOptions, endpointOptions, tlsConfig, err := clouds.Parse(parseOpts...)
	if err != nil {
		return nil, gophercloud.EndpointOpts{}, fmt.Errorf("parsing clouds.yaml: %w", err)
	}

	// A request answered with 401 authenticates again with the clouds.yaml
	// credentials and is repeated once, so a run may outlive its token. An entry
	// that authenticates with a pre-issued token cannot obtain a fresh one, and
	// gophercloud refuses re-authentication for an unscoped token outright, so
	// such an entry keeps its single token.
	authOptions.AllowReauth = authOptions.TokenID == ""

	provider, err := gcconfig.NewProviderClient(ctx, authOptions, gcconfig.WithTLSConfig(tlsConfig))
	if err != nil {
		return nil, gophercloud.EndpointOpts{}, fmt.Errorf("creating provider client: %w", err)
	}

	return provider, endpointOptions, nil
}

// NewNetworkClient authenticates against the cloud described in clouds.yaml and
// returns a NetworkV2 (Neutron) service client. When cloudName is empty the
// cloud is selected from the OS_CLOUD environment variable, following the
// standard clouds.yaml search paths.
func NewNetworkClient(ctx context.Context, cloudName string) (*gophercloud.ServiceClient, error) {
	provider, endpointOptions, err := newProvider(ctx, cloudName)
	if err != nil {
		return nil, err
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
	provider, endpointOptions, err := newProvider(ctx, cloudName)
	if err != nil {
		return nil, err
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
	provider, endpointOptions, err := newProvider(ctx, cloudName)
	if err != nil {
		return nil, err
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
	provider, endpointOptions, err := newProvider(ctx, cloudName)
	if err != nil {
		return nil, err
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
	provider, endpointOptions, err := newProvider(ctx, cloudName)
	if err != nil {
		return nil, err
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
