package mix

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"

	"github.com/B42Labs/dizzy/internal/chaos"
)

// Run runs every lane's nodes in a churn engine of its own, one goroutine per
// lane, and returns each lane's result keyed by lane name once every engine has
// returned. Each engine runs under its lane's Config and Seed on clk; a
// cancelled ctx ends every engine the way it ends a single one, and Run still
// returns all results with a nil error.
//
// Before any node runs, Run rejects a duplicate lane name and any lane config
// the engine would reject, as it will see it: with onCheckpoint non-nil, every
// lane's OnCheckpoint is replaced by one that stores the lane's snapshot and
// calls onCheckpoint with a copy of the latest snapshot of every lane that has
// produced one. One mutex serializes those calls, so onCheckpoint is never
// entered concurrently; it holds the mutex while onCheckpoint runs, which
// delays only the other lanes' schedulers that checkpoint at the same moment.
//
// No lanes yield an empty non-nil map and a nil error.
func Run(ctx context.Context, lanes []*Lane, clk chaos.Clock, onCheckpoint func(map[string]*chaos.Result)) (map[string]*chaos.Result, error) {
	seen := make(map[string]bool, len(lanes))
	for _, l := range lanes {
		if seen[l.Name] {
			return nil, fmt.Errorf("duplicate lane name %q", l.Name)
		}
		seen[l.Name] = true
	}

	var (
		checkpointMu sync.Mutex
		latest       = make(map[string]*chaos.Result, len(lanes))
	)
	cfgs := make([]chaos.Config, len(lanes))
	for i, l := range lanes {
		cfg := l.Config
		if onCheckpoint != nil {
			name := l.Name
			cfg.OnCheckpoint = func(r *chaos.Result) {
				checkpointMu.Lock()
				defer checkpointMu.Unlock()
				latest[name] = r
				onCheckpoint(maps.Clone(latest))
			}
		}
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("lane %q: %w", l.Name, err)
		}
		cfgs[i] = cfg
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex // guards results and errs
		results = make(map[string]*chaos.Result, len(lanes))
		errs    []error
	)
	for i, l := range lanes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := chaos.Run(ctx, l.Nodes, l.Seed, cfgs[i], clk)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("lane %q: %w", l.Name, err))
				return
			}
			results[l.Name] = r
		}()
	}
	wg.Wait()
	return results, errors.Join(errs...)
}
