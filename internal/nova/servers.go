package nova

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/pagination"

	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// BootSpec carries the cloud ids resolved at apply time for a server boot: the
// image and flavor (referenced by name in the scenario) and the network ids the
// server is wired into.
type BootSpec struct {
	ImageID    string
	FlavorID   string
	NetworkIDs []string
}

// CreateServer boots a server with the deterministic name and run metadata. It
// boots directly from the image, or — when s.BootFromVolume — from a
// dizzy-created root volume of s.RootVolumeGiB that is deleted on termination
// (so cleanup reaches it through the server delete). When s.UserData is set a
// deterministic cloud-config naming the logical server is injected at boot. A
// quota rejection is wrapped with ErrQuota so the executor fails fast.
func (c *Client) CreateServer(ctx context.Context, s novaplan.Server, boot BootSpec) (resource.Resource, error) {
	name := resourceName(c.runID, s.Name)

	networks := make([]servers.Network, 0, len(boot.NetworkIDs))
	for _, id := range boot.NetworkIDs {
		networks = append(networks, servers.Network{UUID: id})
	}

	opts := servers.CreateOpts{
		Name:      name,
		FlavorRef: boot.FlavorID,
		Networks:  networks,
		Metadata:  runMetadata(c.runID, KindServer),
	}
	if s.BootFromVolume {
		opts.BlockDevice = []servers.BlockDevice{{
			BootIndex:           0,
			SourceType:          servers.SourceImage,
			DestinationType:     servers.DestinationVolume,
			UUID:                boot.ImageID,
			VolumeSize:          s.RootVolumeGiB,
			DeleteOnTermination: true,
		}}
	} else {
		opts.ImageRef = boot.ImageID
	}
	if s.UserData {
		opts.UserData = userData(s.Name)
	}

	var id string
	err := c.timed(ctx, string(KindServer), "create", func(ctx context.Context) error {
		created, err := servers.Create(ctx, c.compute, opts, nil).Extract()
		if err != nil {
			return err
		}
		id = created.ID
		return nil
	})
	if err != nil {
		// A create that fails after the request reached Nova (a lost response or a
		// 5xx past commit) can leave a server behind. Log the deterministic name so
		// an operator can locate any such orphan; its metadata still makes it
		// discoverable at cleanup.
		slog.Warn("create failed; a server with this name may be orphaned in the cloud",
			"name", name, "error", err)
		return resource.Resource{}, wrapCreate(KindServer, s.Name, err)
	}
	return resource.Resource{Kind: KindServer, Logical: s.Name, Name: name, ID: id}, nil
}

// userData returns a deterministic cloud-config for a logical server name, so a
// scenario expands to byte-identical user data every run.
func userData(logical string) []byte {
	return []byte("#cloud-config\n# dizzy user-data for " + logical + "\n")
}

// StopServer requests an os-stop of the server. A retried stop can hit a 409
// because the instance is already stopped; that 409 confirms the earlier request
// committed, so it is treated as success rather than failing an otherwise-correct
// run, mirroring the cinder extend-already-applied idiom.
func (c *Client) StopServer(ctx context.Context, r resource.Resource) error {
	err := c.timed(ctx, string(KindServer), "stop", func(ctx context.Context) error {
		return servers.Stop(ctx, c.compute, r.ID).ExtractErr()
	})
	if err != nil && conflictMentions(err, "stopped") {
		slog.Info("server already stopped; treating a retried stop as success", "logical", r.Logical, "id", r.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("stopping server %q: %w", r.Logical, err)
	}
	return nil
}

// StartServer requests an os-start of the server, with the symmetric 409
// tolerance StopServer applies: an already-active instance confirms an earlier
// start committed.
func (c *Client) StartServer(ctx context.Context, r resource.Resource) error {
	err := c.timed(ctx, string(KindServer), "start", func(ctx context.Context) error {
		return servers.Start(ctx, c.compute, r.ID).ExtractErr()
	})
	if err != nil && conflictMentions(err, "active") {
		slog.Info("server already active; treating a retried start as success", "logical", r.Logical, "id", r.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("starting server %q: %w", r.Logical, err)
	}
	return nil
}

// RebootServerHard performs a hard reboot (a power-cycle-and-start) of the
// server: Nova's only hard power-cycle verb, the hard variant of the stop/start
// pair.
func (c *Client) RebootServerHard(ctx context.Context, r resource.Resource) error {
	err := c.timed(ctx, string(KindServer), "reboot", func(ctx context.Context) error {
		return servers.Reboot(ctx, c.compute, r.ID, servers.RebootOpts{Type: servers.HardReboot}).ExtractErr()
	})
	if err != nil {
		return fmt.Errorf("hard-rebooting server %q: %w", r.Logical, err)
	}
	return nil
}

// ResizeServer resizes the server to flavorID. The resize leaves the server in
// VERIFY_RESIZE, which ConfirmResizeServer then confirms. A retried resize can
// hit a 409 because the instance is already in VERIFY_RESIZE (vm_state resized)
// from the first request; that 409 confirms the earlier resize committed, so it
// is treated as success and the waitServerStatus(VERIFY_RESIZE) that follows
// stays the source of truth, mirroring the stop/start tolerance.
func (c *Client) ResizeServer(ctx context.Context, r resource.Resource, flavorID string) error {
	err := c.timed(ctx, string(KindServer), "resize", func(ctx context.Context) error {
		return servers.Resize(ctx, c.compute, r.ID, servers.ResizeOpts{FlavorRef: flavorID}).ExtractErr()
	})
	if err != nil && conflictMentions(err, "resized") {
		slog.Info("server already in verify-resize; treating a retried resize as success", "logical", r.Logical, "id", r.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("resizing server %q: %w", r.Logical, err)
	}
	return nil
}

// ConfirmResizeServer confirms a resize, returning the server from VERIFY_RESIZE
// to ACTIVE on the new flavor. A retried confirm can hit a 409 because the
// instance is already ACTIVE (vm_state active) from the first request; that 409
// confirms the earlier confirm committed, so it is treated as success, the
// symmetric tolerance StartServer applies.
func (c *Client) ConfirmResizeServer(ctx context.Context, r resource.Resource) error {
	err := c.timed(ctx, string(KindServer), "confirm-resize", func(ctx context.Context) error {
		return servers.ConfirmResize(ctx, c.compute, r.ID).ExtractErr()
	})
	if err != nil && conflictMentions(err, "active") {
		slog.Info("server already active; treating a retried confirm-resize as success", "logical", r.Logical, "id", r.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("confirming resize of server %q: %w", r.Logical, err)
	}
	return nil
}

// liveMigrateOpts is dizzy's live-migration request body. It sets host to null
// (let the scheduler pick) and block_migration to "auto" (let Nova decide
// between block and shared-storage migration), which the typed
// servers.LiveMigrateOpts cannot express because its BlockMigration is a *bool.
// "auto" is valid at compute microversion >= 2.25, which the compute client is
// pinned to.
type liveMigrateOpts struct{}

// ToLiveMigrateMap builds the os-migrateLive request body.
func (liveMigrateOpts) ToLiveMigrateMap() (map[string]any, error) {
	return map[string]any{
		"os-migrateLive": map[string]any{
			"host":            nil,
			"block_migration": "auto",
		},
	}, nil
}

// LiveMigrateServer live-migrates the server, letting the scheduler pick the
// destination host. It is only called when the admin pre-check permits live
// migration for the run. A retried migrate can hit a 409 because the instance is
// already migrating (task_state migrating) from the first request; that 409
// confirms the earlier migrate committed, so it is treated as success and the
// waitServerStatus(ACTIVE) that follows waits for the migration to finish.
func (c *Client) LiveMigrateServer(ctx context.Context, r resource.Resource) error {
	err := c.timed(ctx, string(KindServer), "live-migrate", func(ctx context.Context) error {
		return servers.LiveMigrate(ctx, c.compute, r.ID, liveMigrateOpts{}).ExtractErr()
	})
	if err != nil && conflictMentions(err, "migrating") {
		slog.Info("server already migrating; treating a retried live-migrate as success", "logical", r.Logical, "id", r.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("live-migrating server %q: %w", r.Logical, err)
	}
	return nil
}

// ListServersByMetadata returns the servers carrying this run's
// dizzy:run=<runID> metadata, the discovery step metadata-based cleanup deletes
// from.
func (c *Client) ListServersByMetadata(ctx context.Context, runID string) ([]resource.Resource, error) {
	return c.listServersByMetadata(ctx, map[string]string{metaRun: runID})
}

// listServersByMetadata is the shared streamed, client-side-filtered server
// listing behind ListServersByMetadata (one run's metadata) and
// ListByTypeMetadata (any run of one kind). Nova's server list has no server-
// side metadata filter, so the project's servers are listed a page at a time and
// filtered client-side against every filter entry on the metadata the tool
// itself wrote — streaming rather than accumulating the whole list, since at
// cleanup a project created by this tool is at peak resource count and one
// allocation of every server is a memory spike that could OOM the very step that
// frees the billable resources.
func (c *Client) listServersByMetadata(ctx context.Context, filter map[string]string) ([]resource.Resource, error) {
	var found []resource.Resource
	err := c.timed(ctx, string(KindServer), "list", func(ctx context.Context) error {
		found = nil
		return servers.List(c.compute, servers.ListOpts{}).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
			items, err := servers.ExtractServers(page)
			if err != nil {
				return false, err
			}
			for _, s := range items {
				if metadataMatches(s.Metadata, filter) {
					found = append(found, resource.Resource{Kind: KindServer, Name: s.Name, ID: s.ID})
				}
			}
			return true, nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing servers by metadata: %w", err)
	}
	return found, nil
}

// conflictMentions reports whether err is a 409 whose body contains substr
// (case-insensitively). Nova rejects a redundant state transition — stopping an
// already-stopped instance, starting an already-active one — with a 409 whose
// body names the current vm_state, so a retried lifecycle op that hits it is
// confirmation the first request already committed.
func conflictMentions(err error, substr string) bool {
	var code gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &code) || code.Actual != 409 {
		return false
	}
	return strings.Contains(strings.ToLower(string(code.Body)), substr)
}
