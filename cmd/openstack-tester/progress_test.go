package main

import (
	"testing"
	"time"

	"github.com/B42Labs/openstack-tester/internal/metrics"
)

// attrValue pulls the value following key in a flat slog-style key/value slice.
func attrValue(t *testing.T, attrs []any, key string) any {
	t.Helper()
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i] == key {
			return attrs[i+1]
		}
	}
	t.Fatalf("attr %q not found in %v", key, attrs)
	return nil
}

// TestCollectorSnapshotDelta confirms the heartbeat snapshot reports cumulative
// op counts, the ok/failed split, and the per-tick delta in "new": each call
// reports only the ops recorded since the previous call, so the heartbeat shows
// the live rate rather than an ever-growing single number.
func TestCollectorSnapshotDelta(t *testing.T) {
	c := metrics.NewCollector()
	snap := collectorSnapshot(c, time.Now())

	c.Record(metrics.Sample{Type: "network", Success: true})
	c.Record(metrics.Sample{Type: "network", Success: false, ErrKind: "http_500"})

	first := snap()
	if got := attrValue(t, first, "ops"); got != 2 {
		t.Errorf("ops = %v, want 2", got)
	}
	if got := attrValue(t, first, "new"); got != 2 {
		t.Errorf("new = %v, want 2 (all ops are new on the first tick)", got)
	}
	if got := attrValue(t, first, "ok"); got != 1 {
		t.Errorf("ok = %v, want 1", got)
	}
	if got := attrValue(t, first, "failed"); got != 1 {
		t.Errorf("failed = %v, want 1", got)
	}

	c.Record(metrics.Sample{Type: "subnet", Success: true})

	second := snap()
	if got := attrValue(t, second, "ops"); got != 3 {
		t.Errorf("ops = %v, want 3 (cumulative)", got)
	}
	if got := attrValue(t, second, "new"); got != 1 {
		t.Errorf("new = %v, want 1 (only the op since the previous tick)", got)
	}
}

// TestCollectorSnapshotExtra confirms caller-supplied attrs (e.g. the churn
// duration) are appended after the standard activity attrs.
func TestCollectorSnapshotExtra(t *testing.T) {
	c := metrics.NewCollector()
	snap := collectorSnapshot(c, time.Now(), "duration", 90*time.Second)
	if got := attrValue(t, snap(), "duration"); got != 90*time.Second {
		t.Errorf("duration = %v, want 1m30s", got)
	}
}
