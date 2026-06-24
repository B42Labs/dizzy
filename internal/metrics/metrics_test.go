package metrics

import (
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// TestPercentileExact pins the nearest-rank percentiles on a known input so a
// regression in the math is caught immediately. It also covers the empty input
// edge case, where every statistic must be zero rather than panic.
func TestPercentileExact(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		if got := percentile(nil, 50); got != 0 {
			t.Errorf("percentile of empty = %s, want 0", got)
		}
		if got := ComputeLatency(nil); got != (Latency{}) {
			t.Errorf("ComputeLatency of empty = %+v, want zero", got)
		}
	})

	t.Run("one-to-hundred", func(t *testing.T) {
		t.Parallel()
		durs := make([]time.Duration, 0, 100)
		for i := 1; i <= 100; i++ {
			durs = append(durs, ms(i))
		}
		lat := ComputeLatency(durs)
		cases := []struct {
			name string
			got  time.Duration
			want time.Duration
		}{
			{"min", lat.Min, ms(1)},
			{"max", lat.Max, ms(100)},
			{"mean", lat.Mean, 50500 * time.Microsecond}, // 50.5ms
			{"median", lat.Median, ms(50)},
			{"p90", lat.P90, ms(90)},
			{"p95", lat.P95, ms(95)},
			{"p99", lat.P99, ms(99)},
		}
		for _, tc := range cases {
			if tc.got != tc.want {
				t.Errorf("%s = %s, want %s", tc.name, tc.got, tc.want)
			}
		}
	})
}

// TestAggregate exercises the full aggregation: per-type grouping, the overall
// group, throughput over wall-clock, the error breakdown (only failed samples
// contribute), and readiness statistics including a not-ready record.
func TestAggregate(t *testing.T) {
	c := NewCollector()
	// Two successful network creates and one failed one.
	c.Record(Sample{Type: "network", Duration: ms(10), Success: true})
	c.Record(Sample{Type: "network", Duration: ms(30), Success: true})
	c.Record(Sample{Type: "network", Duration: ms(20), Success: false, ErrKind: "http_503"})
	// One successful subnet create and one quota failure.
	c.Record(Sample{Type: "subnet", Duration: ms(40), Success: true})
	c.Record(Sample{Type: "subnet", Duration: ms(5), Success: false, ErrKind: "quota"})
	// One ready network and one that never reached its status.
	c.RecordReadiness(Readiness{Type: "network", Duration: ms(200), OK: true})
	c.RecordReadiness(Readiness{Type: "network", Duration: ms(600), OK: false})

	agg := c.Aggregate(2 * time.Second)

	if agg.Overall.Attempted != 5 || agg.Overall.Succeeded != 3 || agg.Overall.Failed != 2 {
		t.Errorf("overall counts = %+v, want 5/3/2", agg.Overall)
	}
	// 3 successes over 2 seconds.
	if agg.Overall.Throughput != 1.5 {
		t.Errorf("overall throughput = %v, want 1.5", agg.Overall.Throughput)
	}

	if len(agg.ByType) != 2 || agg.ByType[0].Type != "network" || agg.ByType[1].Type != "subnet" {
		t.Fatalf("by-type groups = %+v, want sorted network, subnet", agg.ByType)
	}
	if agg.ByType[0].Attempted != 3 || agg.ByType[0].Failed != 1 {
		t.Errorf("network stats = %+v, want 3 attempted / 1 failed", agg.ByType[0])
	}

	wantErr := map[string]int{"http_503": 1, "quota": 1}
	if len(agg.Errors) != 2 {
		t.Fatalf("error breakdown = %+v, want 2 kinds", agg.Errors)
	}
	for _, e := range agg.Errors {
		if wantErr[e.Kind] != e.Count {
			t.Errorf("error %q count = %d, want %d", e.Kind, e.Count, wantErr[e.Kind])
		}
	}
	// Sorted by kind: http_503 before quota.
	if agg.Errors[0].Kind != "http_503" || agg.Errors[1].Kind != "quota" {
		t.Errorf("errors not sorted by kind: %+v", agg.Errors)
	}

	if len(agg.Readiness) != 1 {
		t.Fatalf("readiness groups = %+v, want 1", agg.Readiness)
	}
	r := agg.Readiness[0]
	if r.Type != "network" || r.Count != 2 || r.OK != 1 {
		t.Errorf("readiness = %+v, want network 1/2 ready", r)
	}
}

// TestAggregateEmpty confirms aggregating a collector with no samples is safe
// and yields zeroed statistics rather than a panic or a divide-by-zero.
// TestSnapshot covers the cheap live-count path a progress heartbeat polls:
// the empty collector reports all zeros, and after a mix of successes and
// failures the attempted/succeeded/failed split matches the recorded samples.
func TestSnapshot(t *testing.T) {
	c := NewCollector()
	if a, s, f := c.Snapshot(); a != 0 || s != 0 || f != 0 {
		t.Errorf("empty snapshot = (%d,%d,%d), want (0,0,0)", a, s, f)
	}

	c.Record(Sample{Type: "network", Duration: ms(10), Success: true})
	c.Record(Sample{Type: "network", Duration: ms(20), Success: true})
	c.Record(Sample{Type: "subnet", Duration: ms(30), Success: false, ErrKind: "http_500"})
	// Readiness records are not API-call samples, so they must not move the count.
	c.RecordReadiness(Readiness{Type: "network", Duration: ms(5), OK: true})

	if a, s, f := c.Snapshot(); a != 3 || s != 2 || f != 1 {
		t.Errorf("snapshot = (%d,%d,%d), want (3,2,1)", a, s, f)
	}
}

func TestAggregateEmpty(t *testing.T) {
	agg := NewCollector().Aggregate(0)
	if agg.Overall.Attempted != 0 || agg.Overall.Throughput != 0 {
		t.Errorf("empty overall = %+v, want zero", agg.Overall)
	}
	if len(agg.ByType) != 0 || len(agg.Errors) != 0 || len(agg.Readiness) != 0 {
		t.Errorf("empty aggregate has non-empty groups: %+v", agg)
	}
	// Summary must render without panicking even with no data.
	if agg.Summary() == "" {
		t.Error("Summary returned empty string")
	}
}
