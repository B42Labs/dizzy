package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/B42Labs/dizzy/internal/horizon"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Cleaner is the teardown surface Cleanup drives: list a run's users and
// projects, and delete one. *horizon.Client satisfies it in production, the
// monitor's sweep adapter in the command, and a fake in tests.
type Cleaner interface {
	ListUsersByRun(ctx context.Context, runID string) ([]resource.Resource, error)
	ListProjectsByRun(ctx context.Context, runID string) ([]resource.Resource, error)
	Delete(ctx context.Context, r resource.Resource) error
}

// The production *horizon.Client must satisfy the seam.
var _ Cleaner = (*horizon.Client)(nil)

// Cleanup deletes the users and then the projects of run runID and returns
// how many it deleted. Each kind's listing is unioned by id with the
// recorded resources of that kind, so a resource the listing missed is
// still deleted. A resource whose non-empty name lacks the dizzyhorizon-
// prefix is skipped with a warning, so Cleanup never deletes what a Horizon
// run did not create. Every listing and delete runs through WithRetry under
// opTimeout, the deletes up to concurrency at a time. A 404 counts as
// already gone and is not counted, so a second run deletes nothing. The
// first other error stops it and is returned with the count so far. An
// empty runID is refused before any request.
func Cleanup(ctx context.Context, c Cleaner, runID string, recorded []resource.Resource, concurrency int, opTimeout time.Duration) (int, error) {
	if runID == "" {
		return 0, errors.New("cleanup: empty run id; refusing to delete by an empty prefix")
	}

	var deleted int
	for _, step := range []struct {
		kind resource.Kind
		list func(context.Context, string) ([]resource.Resource, error)
	}{
		{horizon.KindUser, c.ListUsersByRun},
		{horizon.KindProject, c.ListProjectsByRun},
	} {
		var listed []resource.Resource
		err := WithRetry(ctx, opTimeout, func(ctx context.Context) error {
			var err error
			listed, err = step.list(ctx, runID)
			return err
		})
		if err != nil {
			return deleted, fmt.Errorf("listing %ss of run %s: %w", step.kind, runID, err)
		}
		targets := guarded(union(listed, ofKind(recorded, step.kind)))
		gone, err := runStage(ctx, targets, concurrency, func(ctx context.Context, r resource.Resource) (bool, error) {
			slog.Info("deleting resource", "kind", r.Kind, "name", r.Name, "id", r.ID)
			err := WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.Delete(ctx, r) })
			switch {
			case err == nil:
				return true, nil
			case horizon.IsNotFound(err):
				return false, nil
			default:
				return false, fmt.Errorf("deleting %s %s: %w", r.Kind, r.ID, err)
			}
		})
		for _, g := range gone {
			if g {
				deleted++
			}
		}
		if err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// union concatenates the lists, dropping entries without an id and
// deduplicating by id.
func union(lists ...[]resource.Resource) []resource.Resource {
	seen := map[string]bool{}
	var out []resource.Resource
	for _, list := range lists {
		for _, r := range list {
			if r.ID == "" || seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	return out
}

// ofKind returns the resources of kind.
func ofKind(rs []resource.Resource, kind resource.Kind) []resource.Resource {
	var out []resource.Resource
	for _, r := range rs {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// guarded drops, with a warning, every resource whose non-empty name lacks
// the dizzyhorizon- prefix.
func guarded(rs []resource.Resource) []resource.Resource {
	var out []resource.Resource
	for _, r := range rs {
		if r.Name != "" && !horizon.HasNamePrefix(r.Name) {
			slog.Warn("skipping a resource without the dizzyhorizon- prefix", "kind", r.Kind, "name", r.Name, "id", r.ID)
			continue
		}
		out = append(out, r)
	}
	return out
}
