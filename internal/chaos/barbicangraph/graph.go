// Package barbicangraph builds the Barbican churn graph the service-neutral
// chaos engine schedules. The churning population is stored secrets, generic
// containers, secret ACLs, and container consumers: one parentless secret node
// per planned secret, stored with its synthetic payload and stamped with the
// run identity, one container node per planned container, parented on the
// stored secrets it references, one ACL node per planned ACL, parented on its
// secret, and one consumer node per planned consumer, parented on its
// container. The engine's parents-outlive-children invariant gives the right
// lifecycle for free: a container is only created while every secret it
// references is live, an ACL only while its secret is, and a consumer only
// while its container is, and a secret or container with a live child is never
// deleted. Key orders are not churned, a scope Meta Issue #54 settles, so the
// plan's orders yield no node. The mutations are read-backs, modeled as the
// engine's at-most-once mutate action and drawn with one ratio: the payload
// read-back on the secrets the plan marks for retrieval, byte-comparing a live
// secret's payload with the bytes it was stored with; the ACL read-back on
// every ACL, comparing the users its secret's read ACL lists with the plan's;
// and the consumer read-back on every consumer, checking that its container
// still lists it. Each records latency without changing the population.
// Barbican deletes are synchronous, so a delete folds in no wait for gone.
// Keeping the Barbican coupling here leaves the chaos engine free of any
// service-specific import.
package barbicangraph

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/B42Labs/dizzy/internal/barbican"
	barbicanexec "github.com/B42Labs/dizzy/internal/barbican/executor"
	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/payload"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Barbican is the store/stamp/group/ACL/consumer/read-back/delete surface the
// chaos engine drives through the nodes this package builds. It is the
// consumer-defined ports-and-adapters seam to the cloud — *barbican.Client
// satisfies it in production and a fake satisfies it in tests. It takes the
// apply executor's Barbican seam without the order calls, since orders are not
// churned, and adds the client's Delete, since a churn run deletes as it goes.
type Barbican interface {
	StoreSecret(ctx context.Context, s barbicanplan.Secret, seed int64) (resource.Resource, error)
	StampSecret(ctx context.Context, r resource.Resource) error
	CreateContainer(ctx context.Context, ct barbicanplan.Container, stored map[string]resource.Resource) (resource.Resource, error)
	SetACL(ctx context.Context, a barbicanplan.ACL, secret resource.Resource) (resource.Resource, error)
	RegisterConsumer(ctx context.Context, con barbicanplan.Consumer, container resource.Resource) (resource.Resource, error)
	RetrievePayload(ctx context.Context, r resource.Resource, seed int64, n int) error
	VerifyACL(ctx context.Context, a barbicanplan.ACL, r resource.Resource) error
	VerifyConsumers(ctx context.Context, container resource.Resource, want []resource.Resource, exact bool) error
	Delete(ctx context.Context, r resource.Resource) error
}

// The production *barbican.Client must satisfy the seam.
var _ Barbican = (*barbican.Client)(nil)

// Build turns a Barbican plan into the churn graph: one parentless secret node
// per planned secret, in plan order, then one container node per planned
// container, in plan order, parented on the secrets it references so the engine
// never groups an absent secret and never deletes a secret a live container
// still references, then one ACL node per planned ACL, parented on its secret,
// and one consumer node per planned consumer, parented on its container, each
// in plan order. The plan's orders yield no node. A secret the plan marks for
// retrieval carries the engine's mutate action, reading its payload back and
// byte-comparing it at most once per secret lifetime, and every ACL and
// consumer carries one too, reading itself back. Every closure captures
// c and runs through the barbican executor's retry policy, bounded by opTimeout;
// a store uses the same per-secret payload seed apply does (derived from seed
// and the secret's logical name), so the read-back compares against the bytes
// the store sent. The plan is validated first so a dangling container reference
// fails loudly instead of yielding a node that can never be created.
func Build(p *barbicanplan.Plan, c Barbican, seed int64, opTimeout time.Duration) ([]chaos.Node, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid plan: %w", err)
	}

	var nodes []chaos.Node

	for i := range p.Secrets {
		s := p.Secrets[i]
		node := chaos.Node{
			Key: s.Name, Kind: barbican.KindSecret,
			Create: func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
				return storeSecret(ctx, opTimeout, c, s, seed)
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteRetry(ctx, opTimeout, c, res)
			},
		}
		// A secret the plan marks for retrieval can be mutated: read its payload
		// back and byte-compare it, at most once per secret lifetime. A mismatch is
		// never retried, so it reaches the engine as one failed outcome.
		if s.Retrieve {
			node.Mutate = func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return barbicanexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
					return c.RetrievePayload(ctx, res, payload.Seed(seed, s.Name), s.PayloadBytes)
				})
			}
		}
		nodes = append(nodes, node)
	}

	for i := range p.Containers {
		ct := p.Containers[i]
		nodes = append(nodes, chaos.Node{
			Key: ct.Name, Kind: barbican.KindContainer, Parents: append([]string(nil), ct.Secrets...),
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createContainer(ctx, opTimeout, c, ct, ids)
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteRetry(ctx, opTimeout, c, res)
			},
		})
	}

	for i := range p.ACLs {
		a := p.ACLs[i]
		nodes = append(nodes, chaos.Node{
			Key: a.Name, Kind: barbican.KindACL, Parents: []string{a.Secret},
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				secret := resource.Resource{Kind: barbican.KindSecret, Logical: a.Secret, ID: ids[a.Secret]}
				return createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
					return c.SetACL(ctx, a, secret)
				})
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteRetry(ctx, opTimeout, c, res)
			},
			// Read the ACL back and compare its users with the plan's, at most once
			// per ACL lifetime. A mismatch is never retried.
			Mutate: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return barbicanexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
					return c.VerifyACL(ctx, a, res)
				})
			},
		})
	}

	for i := range p.Consumers {
		con := p.Consumers[i]
		nodes = append(nodes, chaos.Node{
			Key: con.Name, Kind: barbican.KindConsumer, Parents: []string{con.Container},
			Create: func(ctx context.Context, ids map[string]string) (resource.Resource, error) {
				return createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
					return c.RegisterConsumer(ctx, con, containerOf(con, ids))
				})
			},
			Delete: func(ctx context.Context, _ map[string]string, res resource.Resource) error {
				return deleteRetry(ctx, opTimeout, c, res)
			},
			// Check that the container's current instance still lists the consumer,
			// at most once per consumer lifetime. Other consumers of the container
			// come and go meanwhile, so the check is for presence, not for the whole
			// list. A mismatch is never retried.
			Mutate: func(ctx context.Context, ids map[string]string, res resource.Resource) error {
				return barbicanexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
					return c.VerifyConsumers(ctx, containerOf(con, ids), []resource.Resource{res}, false)
				})
			},
		})
	}

	return nodes, nil
}

// containerOf returns the container con is registered on as a resource whose
// id is the cloud id of the container's current instance, from ids, the
// engine's map from a parent's logical name to that id.
func containerOf(con barbicanplan.Consumer, ids map[string]string) resource.Resource {
	return resource.Resource{Kind: barbican.KindContainer, Logical: con.Container, ID: ids[con.Container]}
}

// storeSecret stores a secret (retrying transient errors) with the payload
// derived from seed and the secret's logical name, the derivation apply uses,
// then stamps the run identity onto it. A stamp failure still returns the
// stored secret with the error, so the engine keeps it in the run record, skips
// its containers and its read-back, and deletes it later by id; discovery
// reaches it only on its creator, since the stamp never landed.
func storeSecret(ctx context.Context, opTimeout time.Duration, c Barbican, s barbicanplan.Secret, seed int64) (resource.Resource, error) {
	res, err := createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return c.StoreSecret(ctx, s, payload.Seed(seed, s.Name))
	})
	if err != nil {
		return resource.Resource{}, err
	}
	if err := barbicanexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		return c.StampSecret(ctx, res)
	}); err != nil {
		return res, err
	}
	return res, nil
}

// createContainer creates a generic container (retrying transient errors) that
// references the stored secrets ct names. Each reference is resolved from ids,
// the engine's map from a parent's logical name to the cloud id of its current
// instance, so the container always points at the live secrets rather than at
// an earlier, deleted instance.
func createContainer(ctx context.Context, opTimeout time.Duration, c Barbican, ct barbicanplan.Container, ids map[string]string) (resource.Resource, error) {
	stored := make(map[string]resource.Resource, len(ct.Secrets))
	for _, logical := range ct.Secrets {
		stored[logical] = resource.Resource{Kind: barbican.KindSecret, Logical: logical, ID: ids[logical]}
	}
	return createRetry(ctx, opTimeout, func(ctx context.Context) (resource.Resource, error) {
		return c.CreateContainer(ctx, ct, stored)
	})
}

// createRetry runs a create through the retry policy, returning the created
// resource, or the zero Resource when the create failed.
func createRetry(ctx context.Context, opTimeout time.Duration, create func(context.Context) (resource.Resource, error)) (resource.Resource, error) {
	var res resource.Resource
	err := barbicanexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
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

// deleteRetry deletes res through the retry policy, folding an already-gone
// (404) into success. Barbican deletes are synchronous, so there is no
// wait-for-gone.
func deleteRetry(ctx context.Context, opTimeout time.Duration, c Barbican, res resource.Resource) error {
	err := barbicanexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error {
		return c.Delete(ctx, res)
	})
	if err != nil && !barbican.IsNotFound(err) {
		return err
	}
	return nil
}

// Classify labels an operation error for the churn engine's per-bucket error
// breakdown with the labels glancegraph.Classify uses, plus mismatch for a
// read-back that returned different data: a payload, an ACL, or a consumer
// list. Mismatch is checked first, so a wrong read-back is never hidden under a
// broader label, and it is the barbican.ErrKindMismatch value the metrics
// report and the chaos command's mismatch count name. It is wired into the
// engine via chaos.Config.Classify.
func Classify(err error) string {
	switch {
	case err == nil:
		return ""
	case barbican.IsMismatch(err):
		return barbican.ErrKindMismatch
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, barbican.ErrQuota):
		return "quota"
	case barbican.IsNotFound(err):
		return "not-found"
	case barbican.IsRetryable(err):
		return "transient"
	default:
		return "other"
	}
}
