// Package novagraph builds the Nova churn graph the service-neutral chaos engine
// schedules. It maps a Nova plan onto engine nodes: a network node per planned
// network (creating its subnet too), a server node per planned server (mutable
// through its lifecycle operations as the engine's mutate action), a volume node
// per data volume (created, attached, and later detached and deleted), and a
// port node per port. Readiness is folded into each operation — a create
// completes only when the resource reaches its ready state, and a delete only
// when it is gone — and every operation on one server's family (the server and
// its volumes and ports) is serialized behind a per-server gate, since Nova
// rejects concurrent state transitions on one server. BuildLongLived builds the
// graph of a long-lived persona from the same nodes: every node is pinned, so
// the engine creates it first and never deletes it, and the mutations are
// repeatable. A kept server runs one of its enabled lifecycle operations per
// mutation, and a kept volume or port marked for detach toggles its attachment.
// Keeping the Nova coupling here leaves the chaos engine free of any
// service-specific import.
package novagraph

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/nova"
	novaexec "github.com/B42Labs/dizzy/internal/nova/executor"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Nova is the create-drive-delete-and-wait surface the chaos engine drives
// through the nodes this package builds. It is the consumer-defined
// ports-and-adapters seam to the cloud — *nova.Client satisfies it in production
// and a fake satisfies it in tests. It mirrors the apply executor's Nova seam
// plus the DeleteNetworkPorts the network churn deletes need and the
// ColdMigrateServer and WaitForPortDetached the long-lived graph needs.
type Nova interface {
	CreateNetwork(ctx context.Context, n novaplan.Network) (resource.Resource, error)
	CreateSubnet(ctx context.Context, n novaplan.Network, networkID string) (resource.Resource, error)
	DeleteNetworkPorts(ctx context.Context, networkID string) (int, error)
	CreateServer(ctx context.Context, s novaplan.Server, boot nova.BootSpec) (resource.Resource, error)
	CreateVolume(ctx context.Context, v novaplan.Volume) (resource.Resource, error)
	CreatePort(ctx context.Context, pt novaplan.Port, networkID string) (resource.Resource, error)
	StopServer(ctx context.Context, r resource.Resource) error
	StartServer(ctx context.Context, r resource.Resource) error
	RebootServerHard(ctx context.Context, r resource.Resource) error
	ResizeServer(ctx context.Context, r resource.Resource, flavorID string) error
	ConfirmResizeServer(ctx context.Context, r resource.Resource) error
	LiveMigrateServer(ctx context.Context, r resource.Resource) error
	ColdMigrateServer(ctx context.Context, r resource.Resource) error
	AttachVolume(ctx context.Context, server, volume resource.Resource) error
	DetachVolume(ctx context.Context, server, volume resource.Resource) error
	AttachPort(ctx context.Context, server, port resource.Resource) error
	DetachPort(ctx context.Context, server, port resource.Resource) error
	WaitForPortDetached(ctx context.Context, server, port resource.Resource) error
	Delete(ctx context.Context, r resource.Resource) error
	WaitForReady(ctx context.Context, r resource.Resource) error
	WaitForServerStatus(ctx context.Context, r resource.Resource, want string) error
	WaitForVolumeStatus(ctx context.Context, r resource.Resource, want string) error
	WaitForGone(ctx context.Context, r resource.Resource) error
}

// The production *nova.Client must satisfy the seam.
var _ Nova = (*nova.Client)(nil)

// Nova statuses the churn operations wait for.
const (
	statusServerActive       = "ACTIVE"
	statusServerShutoff      = "SHUTOFF"
	statusServerVerifyResize = "VERIFY_RESIZE"
	statusVolumeAvailable    = "available"
	statusVolumeInUse        = "in-use"
)

// Build turns a Nova plan into the churn graph: one network node per network
// (parentless, creating its subnet), one server node per server (parented on its
// networks, mutable when it has a planned lifecycle op), one volume node per data
// volume (parented on its server), and one port node per port (parented on its
// server and its network, so the network outlives the port). Every node's
// closures capture c and r and run through the nova executor's retry policy,
// bounded by opTimeout. The plan is validated first so a dangling reference fails
// loudly instead of yielding a node that can never be created.
func Build(p *novaplan.Plan, c Nova, r novaexec.Resolved, opTimeout time.Duration) ([]chaos.Node, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid plan: %w", err)
	}

	// One gate per server serializes every operation of that server's family (the
	// server and its volumes and ports), since Nova rejects concurrent state
	// transitions on one server. The gate is a capacity-1 channel the engine
	// acquires before granting a concurrency slot, so an op parked behind a busy
	// family never occupies a slot other families could use.
	family := make(map[string]chan struct{}, len(p.Servers))
	for _, s := range p.Servers {
		family[s.Name] = make(chan struct{}, 1)
	}

	var nodes []chaos.Node

	for _, n := range p.Networks {
		n := n
		nodes = append(nodes, chaos.Node{
			Key: n.Name, Kind: nova.KindNetwork,
			Create: func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
				return createNetwork(ctx, opTimeout, c, n)
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				if _, err := c.DeleteNetworkPorts(ctx, res.ID); err != nil {
					return err
				}
				return deleteGone(ctx, opTimeout, c, res)
			},
		})
	}

	for _, s := range p.Servers {
		s := s
		node := chaos.Node{
			Key: s.Name, Kind: nova.KindServer, Parents: append([]string(nil), s.Networks...), Gate: family[s.Name],
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createServer(ctx, opTimeout, c, r, s, ids)
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteGone(ctx, opTimeout, c, res)
			},
		}
		if hasLifecycle(s, r) {
			node.Mutate = func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return mutateServer(ctx, opTimeout, c, r, s, res)
			}
		}
		nodes = append(nodes, node)
	}

	for _, v := range p.Volumes {
		v := v
		nodes = append(nodes, chaos.Node{
			Key: v.Name, Kind: nova.KindVolume, Parents: []string{v.Server}, Gate: family[v.Server],
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createVolume(ctx, opTimeout, c, v, serverRef(v.Server, ids))
			},
			Delete: func(ctx context.Context, ids map[string]string, res resource.Resource) error {
				return deleteVolume(ctx, opTimeout, c, res, serverRef(v.Server, ids))
			},
		})
	}

	for _, pt := range p.Ports {
		pt := pt
		nodes = append(nodes, chaos.Node{
			Key: pt.Name, Kind: nova.KindPort, Parents: []string{pt.Server, pt.Network}, Gate: family[pt.Server],
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createPort(ctx, opTimeout, c, pt, ids[pt.Network], serverRef(pt.Server, ids))
			},
			Delete: func(ctx context.Context, ids map[string]string, res resource.Resource) error {
				return deletePort(ctx, opTimeout, c, res, serverRef(pt.Server, ids))
			},
		})
	}

	return nodes, nil
}

// mutation is the signature of chaos.Node.Mutate.
type mutation = func(ctx context.Context, ids map[string]string, res resource.Resource) error

// BuildLongLived turns a Nova plan into the churn graph of a long-lived
// persona: the nodes Build returns, so an invalid plan fails the same way, with
// every node pinned and every mutation replaced. A server whose operations
// include at least one the run enabled is repeatable, and each of its
// mutations runs one of those operations, drawn uniformly from a generator
// seeded from the plan seed and the server name. Its operations are stop-start
// when it has a stop/start variant, resize when it is marked for resize and the
// resize flavor is resolved, live-migrate when it is marked for it and the run
// enabled live migration, and cold-migrate likewise. A volume or port marked
// for detach is repeatable and toggles its attachment. Every other node has no
// mutation.
//
// The engine runs one node's mutations one after another in decision order, so
// the n-th mutation of a server always gets the n-th draw, and the generator
// and flags a closure holds need no lock. No generator or flag is shared
// between two nodes.
func BuildLongLived(p *novaplan.Plan, c Nova, r novaexec.Resolved, opTimeout time.Duration) ([]chaos.Node, error) {
	nodes, err := Build(p, c, r, opTimeout)
	if err != nil {
		return nil, err
	}
	index := make(map[string]int, len(nodes))
	for i := range nodes {
		nodes[i].Pinned, nodes[i].Mutate = true, nil
		index[nodes[i].Key] = i
	}

	for _, s := range p.Servers {
		if m := serverMutation(p.Seed, s, c, r, opTimeout); m != nil {
			nodes[index[s.Name]].Mutate = m
		}
	}
	for _, v := range p.Volumes {
		if v.Detach {
			nodes[index[v.Name]].Mutate = volumeToggle(v, c, opTimeout)
		}
	}
	for _, pt := range p.Ports {
		if pt.Detach {
			nodes[index[pt.Name]].Mutate = portToggle(pt, c, opTimeout)
		}
	}
	return nodes, nil
}

// serverMutation returns the repeatable mutation of a kept server, or nil when
// the run enabled none of its operations. A resize targets the resize flavor
// while the server is on the boot flavor and the boot flavor afterwards; the
// flag that tracks it flips only once the whole resize sequence succeeded.
// When Nova rejects a resize because the server already has the target
// flavor, an earlier resize or migration finished in the cloud after its wait
// gave up, so the flag flips to match the server and it resizes the other way.
func serverMutation(seed int64, s novaplan.Server, c Nova, r novaexec.Resolved, opTimeout time.Duration) mutation {
	var ops []func(ctx context.Context, res resource.Resource) error
	if s.StopStart != "" {
		ops = append(ops, func(ctx context.Context, res resource.Resource) error {
			return stopStartServer(ctx, opTimeout, c, res, s.StopStart)
		})
	}
	if s.Resize && r.ResizeFlavorID != "" {
		resized := false
		target := func() string {
			if resized {
				return r.FlavorID
			}
			return r.ResizeFlavorID
		}
		ops = append(ops, func(ctx context.Context, res resource.Resource) error {
			err := resizeServer(ctx, opTimeout, c, res, target())
			if nova.IsSameFlavor(err) {
				slog.Info("server already has the resize target flavor; resizing the other way", "logical", res.Logical, "flavor", target())
				resized = !resized
				err = resizeServer(ctx, opTimeout, c, res, target())
			}
			if err != nil {
				return err
			}
			resized = !resized
			return nil
		})
	}
	if s.LiveMigrate && r.LiveMigration {
		ops = append(ops, func(ctx context.Context, res resource.Resource) error {
			return liveMigrateServer(ctx, opTimeout, c, res)
		})
	}
	if s.ColdMigrate && r.ColdMigration {
		ops = append(ops, func(ctx context.Context, res resource.Resource) error {
			return coldMigrateServer(ctx, opTimeout, c, res)
		})
	}
	if len(ops) == 0 {
		return nil
	}
	rng := rand.New(rand.NewSource(opSeed(seed, s.Name)))
	return func(ctx context.Context, _ map[string]string, res resource.Resource) error {
		return ops[rng.Intn(len(ops))](ctx, res)
	}
}

// opSeed derives the seed of a kept server's operation draw from the plan seed
// and the server name: the seed XOR the FNV-64a hash of the name, the
// derivation the mix scenario uses for a persona seed.
func opSeed(seed int64, name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return seed ^ int64(h.Sum64())
}

// volumeToggle returns the repeatable mutation of a kept data volume marked for
// detach: it detaches the volume and waits for available, or attaches it and
// waits for in-use.
func volumeToggle(v novaplan.Volume, c Nova, opTimeout time.Duration) mutation {
	return toggle(v.Server,
		func(ctx context.Context, server, res resource.Resource) error {
			if err := detach(ctx, opTimeout, func(ctx context.Context) error { return c.DetachVolume(ctx, server, res) }); err != nil {
				return err
			}
			return waitVolumeStatus(ctx, opTimeout, c, res, statusVolumeAvailable)
		},
		func(ctx context.Context, server, res resource.Resource) error {
			if err := novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.AttachVolume(ctx, server, res) }); err != nil {
				return err
			}
			return waitVolumeStatus(ctx, opTimeout, c, res, statusVolumeInUse)
		})
}

// portToggle returns the repeatable mutation of a kept port marked for detach:
// it detaches the port and waits until the server no longer has it, or
// attaches it, without a wait, as createPort does.
func portToggle(pt novaplan.Port, c Nova, opTimeout time.Duration) mutation {
	return toggle(pt.Server,
		func(ctx context.Context, server, res resource.Resource) error {
			if err := detach(ctx, opTimeout, func(ctx context.Context) error { return c.DetachPort(ctx, server, res) }); err != nil {
				return err
			}
			return waitPortDetached(ctx, opTimeout, c, server, res)
		},
		func(ctx context.Context, server, res resource.Resource) error {
			return novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.AttachPort(ctx, server, res) })
		})
}

// toggle returns a mutation that runs detachStep while the resource is
// attached to its server and attachStep while it is not. The resource starts
// attached, as its create leaves it. The state flips only when the step
// succeeded, so a failed step is tried again in the same direction by the next
// mutation.
func toggle(server string, detachStep, attachStep func(ctx context.Context, server, res resource.Resource) error) mutation {
	attached := true
	return func(ctx context.Context, ids map[string]string, res resource.Resource) error {
		step := detachStep
		if !attached {
			step = attachStep
		}
		if err := step(ctx, serverRef(server, ids), res); err != nil {
			return err
		}
		attached = !attached
		return nil
	}
}

// hasLifecycle reports whether a server carries any lifecycle mutation the churn
// engine can apply: a stop/start, a resize, or a live migration that the run
// enabled.
func hasLifecycle(s novaplan.Server, r novaexec.Resolved) bool {
	return s.StopStart != "" || s.Resize || (s.LiveMigrate && r.LiveMigration)
}

// serverRef builds the minimal server resource the attach/detach calls need from
// the parent server's cloud id.
func serverRef(name string, ids map[string]string) resource.Resource {
	return resource.Resource{Kind: nova.KindServer, Logical: name, ID: ids[name]}
}

// createRetry runs a create through the retry policy and returns the created
// resource. The per-kind readiness and attach steps stay in the callers.
func createRetry(ctx context.Context, opTimeout time.Duration, create func(context.Context) (resource.Resource, error)) (resource.Resource, error) {
	var res resource.Resource
	err := novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		r, err := create(ctx)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return resource.Resource{}, err
	}
	return res, nil
}

// createNetwork creates a network and its subnet, waiting each to ready, and
// returns the network resource (deletable even if the subnet step fails).
func createNetwork(ctx context.Context, opTimeout time.Duration, c Nova, n novaplan.Network) (resource.Resource, error) {
	net, err := createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return c.CreateNetwork(ctx, n)
	})
	if err != nil {
		return resource.Resource{}, err
	}
	if err := waitReady(ctx, opTimeout, c, net); err != nil {
		return net, err
	}
	if err := novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		_, err := c.CreateSubnet(ctx, n, net.ID)
		return err
	}); err != nil {
		return net, err
	}
	return net, nil
}

// createServer boots a server and waits for it to become ACTIVE, resolving its
// network ids from the engine-supplied parent ids.
func createServer(ctx context.Context, opTimeout time.Duration, c Nova, r novaexec.Resolved, s novaplan.Server, ids map[string]string) (resource.Resource, error) {
	networkIDs := make([]string, 0, len(s.Networks))
	for _, name := range s.Networks {
		networkIDs = append(networkIDs, ids[name])
	}
	boot := nova.BootSpec{ImageID: r.ImageID, FlavorID: r.FlavorID, NetworkIDs: networkIDs}
	res, err := createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return c.CreateServer(ctx, s, boot)
	})
	if err != nil {
		return resource.Resource{}, err
	}
	if err := waitServerStatus(ctx, opTimeout, c, res, statusServerActive); err != nil {
		return res, err
	}
	return res, nil
}

// mutateServer applies a server's planned lifecycle operations in a fixed
// precedence — stop/start, then resize+confirm, then live-migrate (only when the
// run enabled it) — at most once per instance lifetime (the engine bounds the
// call). Attach/detach are handled by the volume and port child nodes.
func mutateServer(ctx context.Context, opTimeout time.Duration, c Nova, r novaexec.Resolved, s novaplan.Server, res resource.Resource) error {
	if err := stopStartServer(ctx, opTimeout, c, res, s.StopStart); err != nil {
		return err
	}
	if s.Resize {
		if err := resizeServer(ctx, opTimeout, c, res, r.ResizeFlavorID); err != nil {
			return err
		}
	}
	if s.LiveMigrate && r.LiveMigration {
		return liveMigrateServer(ctx, opTimeout, c, res)
	}
	return nil
}

// stopStartServer runs a server's stop/start variant: os-stop then os-start for
// "soft", a hard reboot for "hard", nothing for "". Each step waits for the
// state it leads to.
func stopStartServer(ctx context.Context, opTimeout time.Duration, c Nova, res resource.Resource, variant string) error {
	switch variant {
	case novaplan.StopStartSoft:
		if err := novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.StopServer(ctx, res) }); err != nil {
			return err
		}
		if err := waitServerStatus(ctx, opTimeout, c, res, statusServerShutoff); err != nil {
			return err
		}
		if err := novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.StartServer(ctx, res) }); err != nil {
			return err
		}
		if err := waitServerStatus(ctx, opTimeout, c, res, statusServerActive); err != nil {
			return err
		}
	case novaplan.StopStartHard:
		if err := novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.RebootServerHard(ctx, res) }); err != nil {
			return err
		}
		if err := waitServerStatus(ctx, opTimeout, c, res, statusServerActive); err != nil {
			return err
		}
	}
	return nil
}

// resizeServer resizes a server to flavorID, waits for VERIFY_RESIZE, confirms
// the resize and waits for ACTIVE.
func resizeServer(ctx context.Context, opTimeout time.Duration, c Nova, res resource.Resource, flavorID string) error {
	if err := novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		return c.ResizeServer(ctx, res, flavorID)
	}); err != nil {
		return err
	}
	return confirmResize(ctx, opTimeout, c, res)
}

// liveMigrateServer live-migrates a server and waits for ACTIVE.
func liveMigrateServer(ctx context.Context, opTimeout time.Duration, c Nova, res resource.Resource) error {
	if err := novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.LiveMigrateServer(ctx, res) }); err != nil {
		return err
	}
	return waitServerStatus(ctx, opTimeout, c, res, statusServerActive)
}

// coldMigrateServer cold-migrates a server, waits for VERIFY_RESIZE, confirms
// the migration and waits for ACTIVE.
func coldMigrateServer(ctx context.Context, opTimeout time.Duration, c Nova, res resource.Resource) error {
	if err := novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.ColdMigrateServer(ctx, res) }); err != nil {
		return err
	}
	return confirmResize(ctx, opTimeout, c, res)
}

// confirmResize waits for a resized or cold-migrated server to reach
// VERIFY_RESIZE, confirms it and waits for ACTIVE.
func confirmResize(ctx context.Context, opTimeout time.Duration, c Nova, res resource.Resource) error {
	if err := waitServerStatus(ctx, opTimeout, c, res, statusServerVerifyResize); err != nil {
		return err
	}
	if err := novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.ConfirmResizeServer(ctx, res) }); err != nil {
		return err
	}
	return waitServerStatus(ctx, opTimeout, c, res, statusServerActive)
}

// createVolume creates a data volume, waits it to available, attaches it to its
// server, and waits it to in-use, returning the volume resource.
func createVolume(ctx context.Context, opTimeout time.Duration, c Nova, v novaplan.Volume, server resource.Resource) (resource.Resource, error) {
	res, err := createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return c.CreateVolume(ctx, v)
	})
	if err != nil {
		return resource.Resource{}, err
	}
	if err := waitVolumeStatus(ctx, opTimeout, c, res, statusVolumeAvailable); err != nil {
		return res, err
	}
	if err := novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.AttachVolume(ctx, server, res) }); err != nil {
		return res, err
	}
	if err := waitVolumeStatus(ctx, opTimeout, c, res, statusVolumeInUse); err != nil {
		return res, err
	}
	return res, nil
}

// deleteVolume detaches a volume from its server, waits it to available, and
// deletes it, waiting it to be gone.
func deleteVolume(ctx context.Context, opTimeout time.Duration, c Nova, res, server resource.Resource) error {
	if err := detach(ctx, opTimeout, func(ctx context.Context) error { return c.DetachVolume(ctx, server, res) }); err != nil {
		return err
	}
	if err := waitVolumeStatus(ctx, opTimeout, c, res, statusVolumeAvailable); err != nil {
		return err
	}
	return deleteGone(ctx, opTimeout, c, res)
}

// createPort creates a port on networkID and attaches it to its server,
// returning the port resource.
func createPort(ctx context.Context, opTimeout time.Duration, c Nova, pt novaplan.Port, networkID string, server resource.Resource) (resource.Resource, error) {
	res, err := createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return c.CreatePort(ctx, pt, networkID)
	})
	if err != nil {
		return resource.Resource{}, err
	}
	if err := novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.AttachPort(ctx, server, res) }); err != nil {
		return res, err
	}
	return res, nil
}

// deletePort detaches a port from its server and deletes it, waiting it to be
// gone.
func deletePort(ctx context.Context, opTimeout time.Duration, c Nova, res, server resource.Resource) error {
	if err := detach(ctx, opTimeout, func(ctx context.Context) error { return c.DetachPort(ctx, server, res) }); err != nil {
		return err
	}
	return deleteGone(ctx, opTimeout, c, res)
}

// detach runs a detach through the retry policy, folding an already-detached
// (404) into success so a re-created logical name never double-counts.
func detach(ctx context.Context, opTimeout time.Duration, fn func(context.Context) error) error {
	err := novaexec.WithRetry(ctx, opTimeout, fn)
	if err != nil && nova.IsNotFound(err) {
		return nil
	}
	return err
}

// waitReady polls res to its ready state, bounded by opTimeout.
func waitReady(ctx context.Context, opTimeout time.Duration, c Nova, res resource.Resource) error {
	readyCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return c.WaitForReady(readyCtx, res)
}

// waitPortDetached polls until server no longer has port attached, bounded by
// opTimeout.
func waitPortDetached(ctx context.Context, opTimeout time.Duration, c Nova, server, port resource.Resource) error {
	detachCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return c.WaitForPortDetached(detachCtx, server, port)
}

// waitServerStatus polls a server to want, bounded by opTimeout.
func waitServerStatus(ctx context.Context, opTimeout time.Duration, c Nova, res resource.Resource, want string) error {
	statusCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return c.WaitForServerStatus(statusCtx, res, want)
}

// waitVolumeStatus polls a volume to want, bounded by opTimeout.
func waitVolumeStatus(ctx context.Context, opTimeout time.Duration, c Nova, res resource.Resource, want string) error {
	statusCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return c.WaitForVolumeStatus(statusCtx, res, want)
}

// deleteGone deletes res through the retry policy, folds an already-gone (404)
// into success, and waits for the resource to be fully gone (bounded by
// opTimeout) so a re-created logical name can never transiently double-count
// against the envelope.
func deleteGone(ctx context.Context, opTimeout time.Duration, c Nova, res resource.Resource) error {
	err := novaexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		return c.Delete(ctx, res)
	})
	if err != nil {
		if nova.IsNotFound(err) {
			return nil
		}
		return err
	}
	goneCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return c.WaitForGone(goneCtx, res)
}

// Classify labels an operation error for the churn engine's per-bucket error
// breakdown, reusing the nova classification helpers so the labels match the
// kinds operators already see in the metrics report. It is wired into the engine
// via chaos.Config.Classify.
func Classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, nova.ErrQuota):
		return "quota"
	case nova.IsNotFound(err):
		return "not-found"
	case nova.IsRetryable(err):
		return "transient"
	default:
		return "other"
	}
}
