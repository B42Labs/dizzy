//go:build integration

package config_test

import (
	"os"
	"testing"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
)

// TestNewNetworkClient_Integration authenticates against a real cloud and issues
// one trivial NetworkV2 call. internal/config is a ports-and-adapters seam to
// OpenStack, so the external dependency is exercised here rather than mocked.
// Run with: go test -tags integration ./internal/config/
func TestNewNetworkClient_Integration(t *testing.T) {
	if os.Getenv("OS_CLOUD") == "" {
		t.Skip("OS_CLOUD not set; skipping integration test")
	}

	ctx := t.Context()
	client, err := config.NewNetworkClient(ctx, "")
	if err != nil {
		t.Fatalf("NewNetworkClient: %v", err)
	}

	pages, err := networks.List(client, networks.ListOpts{}).AllPages(ctx)
	if err != nil {
		t.Fatalf("listing networks: %v", err)
	}
	if _, err := networks.ExtractNetworks(pages); err != nil {
		t.Fatalf("extracting networks: %v", err)
	}
}
