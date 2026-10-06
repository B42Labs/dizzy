package mix

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeNodes returns n parentless nodes whose creates count into creates and
// fail with createErr when it is non-nil.
func fakeNodes(n int, creates *atomic.Int64, createErr error) []chaos.Node {
	nodes := make([]chaos.Node, 0, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("srv-%04d", i+1)
		nodes = append(nodes, chaos.Node{
			Key:  key,
			Kind: "server",
			Create: func(context.Context, map[string]string) (resource.Resource, error) {
				creates.Add(1)
				if createErr != nil {
					return resource.Resource{}, createErr
				}
				return resource.Resource{Kind: "server", Logical: key, ID: key + "-id"}, nil
			},
			Delete: func(context.Context, map[string]string, resource.Resource) error { return nil },
		})
	}
	return nodes
}

// laneConfig is a short, bounded churn config for the tests.
func laneConfig(targetFill float64) chaos.Config {
	return chaos.Config{
		Duration:    300 * time.Millisecond,
		MinInterval: time.Millisecond,
		MaxInterval: 5 * time.Millisecond,
		MaxParallel: 2,
		ChurnRatio:  0.5,
		TargetFill:  targetFill,
		Concurrency: 2,
	}
}

// fakeLane returns a lane of four fake nodes under cfg.
func fakeLane(name string, cfg chaos.Config, creates *atomic.Int64) *Lane {
	return &Lane{Name: name, RunID: "run-" + name, Seed: 42, Config: cfg, Nodes: fakeNodes(4, creates, nil)}
}

func TestRunReturnsResultPerLane(t *testing.T) {
	var creates atomic.Int64
	lanes := []*Lane{fakeLane("ci", laneConfig(0.1), &creates), fakeLane("legacy", laneConfig(0.9), &creates)}

	results, err := Run(context.Background(), lanes, chaos.RealClock{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Run returned %d results, want 2", len(results))
	}
	for name, want := range map[string]float64{"ci": 0.1, "legacy": 0.9} {
		r := results[name]
		if r == nil {
			t.Fatalf("no result for lane %q", name)
		}
		if r.Creates < 1 {
			t.Errorf("lane %q made %d creates, want at least 1", name, r.Creates)
		}
		if r.TargetFill != want {
			t.Errorf("lane %q ran with target fill %v, want its own %v", name, r.TargetFill, want)
		}
	}
}

func TestRunNoLanes(t *testing.T) {
	results, err := Run(context.Background(), nil, chaos.RealClock{}, nil)
	if err != nil || results == nil || len(results) != 0 {
		t.Errorf("Run(no lanes) = %v, %v, want an empty non-nil map and nil", results, err)
	}
}

// TestRunRejectsBeforeAnyCreate confirms a duplicate lane name and an invalid
// lane config fail before a single node of any lane runs.
func TestRunRejectsBeforeAnyCreate(t *testing.T) {
	zeroParallel := laneConfig(0.5)
	zeroParallel.MaxParallel = 0
	noInterval := laneConfig(0.5)

	tests := []struct {
		name         string
		lanes        func(*atomic.Int64) []*Lane
		onCheckpoint func(map[string]*chaos.Result)
		want         string
		exact        bool
	}{
		{
			name: "duplicate lane name",
			lanes: func(c *atomic.Int64) []*Lane {
				return []*Lane{fakeLane("ci", laneConfig(0.5), c), fakeLane("ci", laneConfig(0.5), c)}
			},
			want: `duplicate lane name "ci"`, exact: true,
		},
		{
			name: "invalid config",
			lanes: func(c *atomic.Int64) []*Lane {
				return []*Lane{fakeLane("legacy", laneConfig(0.5), c), fakeLane("ci", zeroParallel, c)}
			},
			want: `lane "ci": `,
		},
		{
			name: "zero checkpoint interval with a callback",
			lanes: func(c *atomic.Int64) []*Lane {
				return []*Lane{fakeLane("ci", noInterval, c)}
			},
			onCheckpoint: func(map[string]*chaos.Result) {},
			want:         `lane "ci": `,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var creates atomic.Int64
			_, err := Run(context.Background(), tc.lanes(&creates), chaos.RealClock{}, tc.onCheckpoint)
			switch {
			case err == nil:
				t.Fatalf("Run = nil, want %q", tc.want)
			case tc.exact && err.Error() != tc.want:
				t.Errorf("Run = %q, want %q", err, tc.want)
			case !strings.HasPrefix(err.Error(), tc.want):
				t.Errorf("Run = %q, want it to start with %q", err, tc.want)
			}
			if n := creates.Load(); n != 0 {
				t.Errorf("%d creates ran before Run rejected the lanes, want 0", n)
			}
		})
	}
}

// TestRunCancelledContext confirms cancelling the context ends every engine,
// here two runs without an end, and Run still returns every lane's result.
func TestRunCancelledContext(t *testing.T) {
	unbounded := laneConfig(0.5)
	unbounded.Duration = 0
	unbounded.Unbounded = true
	unbounded.BucketWidth = time.Minute

	var creates atomic.Int64
	lanes := []*Lane{fakeLane("ci", unbounded, &creates), fakeLane("legacy", unbounded, &creates)}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	results, err := Run(ctx, lanes, chaos.RealClock{}, nil)
	if err != nil {
		t.Fatalf("Run = %v, want nil after cancellation", err)
	}
	for _, name := range []string{"ci", "legacy"} {
		if results[name] == nil {
			t.Errorf("no result for lane %q after cancellation", name)
		}
	}
}

// TestRunCreateFailureCounted confirms a lane whose creates all fail still
// yields a result, with the failures in its buckets.
func TestRunCreateFailureCounted(t *testing.T) {
	var creates atomic.Int64
	l := &Lane{Name: "ci", Seed: 1, Config: laneConfig(0.5), Nodes: fakeNodes(2, &creates, errors.New("quota exceeded"))}

	results, err := Run(context.Background(), []*Lane{l}, chaos.RealClock{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := results["ci"]
	if r == nil {
		t.Fatal("no result for the failing lane")
	}
	var failed int
	for _, b := range r.Buckets {
		failed += b.Stats.Failed
	}
	if failed == 0 {
		t.Errorf("buckets count no failure although every create failed: %+v", r.Buckets)
	}
}

// TestRunCheckpointSerialized confirms the checkpoint callback is never
// entered concurrently and each call holds the latest snapshot of every lane
// that has one: from one call to the next exactly one lane's snapshot changes
// and the others are carried over, even when the callback empties the map it
// received.
func TestRunCheckpointSerialized(t *testing.T) {
	cfg := laneConfig(0.5)
	cfg.CheckpointInterval = time.Millisecond

	var (
		creates  atomic.Int64
		inside   atomic.Int32
		overlaps atomic.Int32
		mu       sync.Mutex
		calls    []map[string]*chaos.Result
	)
	onCheckpoint := func(latest map[string]*chaos.Result) {
		if inside.Add(1) > 1 {
			overlaps.Add(1)
		}
		defer inside.Add(-1)
		time.Sleep(200 * time.Microsecond) // widen the window for an overlap

		seen := make(map[string]*chaos.Result, len(latest))
		for name, r := range latest {
			seen[name] = r
			delete(latest, name)
		}
		mu.Lock()
		calls = append(calls, seen)
		mu.Unlock()
	}

	lanes := []*Lane{fakeLane("ci", cfg, &creates), fakeLane("legacy", cfg, &creates)}
	if _, err := Run(context.Background(), lanes, chaos.RealClock{}, onCheckpoint); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if n := overlaps.Load(); n != 0 {
		t.Errorf("the checkpoint callback was entered concurrently %d times", n)
	}
	if len(calls) < 2 {
		t.Fatalf("got %d checkpoint calls, want at least 2", len(calls))
	}
	if len(calls[0]) != 1 {
		t.Errorf("first call carries %d lanes, want only the one that checkpointed", len(calls[0]))
	}
	sawBoth := false
	for i := 1; i < len(calls); i++ {
		prev, cur := calls[i-1], calls[i]
		changed := 0
		for name, r := range cur {
			if p, ok := prev[name]; !ok || p != r {
				changed++
				if ok && r.Creates < p.Creates {
					t.Errorf("call %d: lane %q creates went from %d to %d", i, name, p.Creates, r.Creates)
				}
			}
		}
		for name := range prev {
			if _, ok := cur[name]; !ok {
				t.Errorf("call %d dropped lane %q that the previous call carried", i, name)
			}
		}
		if changed != 1 {
			t.Errorf("call %d changed %d lane snapshots, want exactly 1", i, changed)
		}
		if len(cur) == 2 {
			sawBoth = true
		}
	}
	if !sawBoth {
		t.Error("no checkpoint call carried both lanes")
	}
}
