package metrics

import (
	"reflect"
	"sync"
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

// TestAggregateExactMinMeanMax confirms the histogram-backed aggregate keeps
// min, mean and max exact (only the percentiles are estimates), using the
// samples of TestAggregate.
func TestAggregateExactMinMeanMax(t *testing.T) {
	c := NewCollector()
	c.Record(Sample{Type: "network", Duration: ms(10), Success: true})
	c.Record(Sample{Type: "network", Duration: ms(30), Success: true})
	c.Record(Sample{Type: "network", Duration: ms(20), Success: false, ErrKind: "http_503"})
	c.Record(Sample{Type: "subnet", Duration: ms(40), Success: true})
	c.Record(Sample{Type: "subnet", Duration: ms(5), Success: false, ErrKind: "quota"})

	agg := c.Aggregate(2 * time.Second)
	if len(agg.ByType) == 0 || agg.ByType[0].Type != "network" {
		t.Fatalf("by-type groups = %+v, want network first", agg.ByType)
	}
	lat := agg.ByType[0].Latency
	if lat.Min != ms(10) || lat.Mean != ms(20) || lat.Max != ms(30) {
		t.Errorf("network min/mean/max = %s/%s/%s, want 10ms/20ms/30ms", lat.Min, lat.Mean, lat.Max)
	}
}

// TestAggregateEmptyHasNilGroups confirms an empty collector aggregates to a
// zero overall group and nil (not empty) slices, so its JSON keeps the null
// values records have always carried, and that its snapshot is all zeros.
func TestAggregateEmptyHasNilGroups(t *testing.T) {
	c := NewCollector()
	agg := c.Aggregate(time.Second)
	if agg.Overall != (Stats{}) {
		t.Errorf("empty overall = %+v, want zero", agg.Overall)
	}
	if agg.ByType != nil || agg.Errors != nil || agg.Readiness != nil {
		t.Errorf("empty aggregate groups = %#v / %#v / %#v, want nil", agg.ByType, agg.Errors, agg.Readiness)
	}
	if a, s, f := c.Snapshot(); a != 0 || s != 0 || f != 0 {
		t.Errorf("empty snapshot = (%d,%d,%d), want (0,0,0)", a, s, f)
	}
}

// TestCollectorStateIsBounded confirms the collector's memory does not grow
// with the number of samples: a million samples of one type and a million
// readiness records leave one group each, and no type the collector is built
// from holds a slice that could grow per sample.
func TestCollectorStateIsBounded(t *testing.T) {
	c := NewCollector()
	for i := 0; i < 1_000_000; i++ {
		c.Record(Sample{Type: "network", Duration: time.Duration(i) * time.Microsecond, Success: i%2 == 0})
		c.RecordReadiness(Readiness{Type: "network", Duration: time.Duration(i) * time.Microsecond, OK: true})
	}
	if len(c.byType) != 1 || len(c.readiness) != 1 {
		t.Errorf("groups = %d type / %d readiness, want 1 / 1", len(c.byType), len(c.readiness))
	}
	if a, _, _ := c.Snapshot(); a != 1_000_000 {
		t.Errorf("snapshot attempted = %d, want 1000000", a)
	}
	if path := slicePath(reflect.TypeOf(Collector{}), "Collector", map[reflect.Type]bool{}); path != "" {
		t.Errorf("collector state holds a slice at %s", path)
	}
}

// slicePath walks the types of package metrics reachable from typ (through
// fields, pointers and map keys and values) and returns the path to the first
// slice it finds, or "" when there is none. Types from other packages, such as
// sync.Mutex, are not entered.
func slicePath(typ reflect.Type, path string, seen map[reflect.Type]bool) string {
	if seen[typ] {
		return ""
	}
	seen[typ] = true
	switch typ.Kind() {
	case reflect.Slice, reflect.Array:
		return path
	case reflect.Pointer:
		return slicePath(typ.Elem(), path, seen)
	case reflect.Map:
		if p := slicePath(typ.Key(), path+"[key]", seen); p != "" {
			return p
		}
		return slicePath(typ.Elem(), path+"[value]", seen)
	case reflect.Struct:
		if typ.PkgPath() != reflect.TypeOf(Collector{}).PkgPath() {
			return ""
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if p := slicePath(f.Type, path+"."+f.Name, seen); p != "" {
				return p
			}
		}
	}
	return ""
}

// TestChildRecordsIntoParent confirms a sample and a readiness record on a
// child collector show in the child's aggregate and in its parent's.
func TestChildRecordsIntoParent(t *testing.T) {
	parent := NewCollector()
	child := parent.Child()
	child.Record(Sample{Type: "server", Duration: ms(10), Success: false, ErrKind: "quota"})
	child.RecordReadiness(Readiness{Type: "server", Duration: ms(5), OK: true})

	for name, c := range map[string]*Collector{"child": child, "parent": parent} {
		agg := c.Aggregate(time.Second)
		if agg.Overall.Attempted != 1 || agg.Overall.Failed != 1 {
			t.Errorf("%s overall = %+v, want 1 attempted, 1 failed", name, agg.Overall)
		}
		if len(agg.Errors) != 1 || agg.Errors[0] != (ErrorCount{Kind: "quota", Count: 1}) {
			t.Errorf("%s errors = %+v, want quota=1", name, agg.Errors)
		}
		if len(agg.Readiness) != 1 || agg.Readiness[0].Count != 1 {
			t.Errorf("%s readiness = %+v, want one server record", name, agg.Readiness)
		}
	}
}

// TestParentDoesNotRecordIntoChild confirms recording flows only upward: a
// sample on the parent leaves its child empty.
func TestParentDoesNotRecordIntoChild(t *testing.T) {
	parent := NewCollector()
	child := parent.Child()
	parent.Record(Sample{Type: "server", Duration: ms(10), Success: true})
	parent.RecordReadiness(Readiness{Type: "server", Duration: ms(5), OK: true})

	if a, _, _ := child.Snapshot(); a != 0 {
		t.Errorf("child attempted = %d, want 0", a)
	}
	if agg := child.Aggregate(time.Second); agg.Readiness != nil {
		t.Errorf("child readiness = %+v, want none", agg.Readiness)
	}
	if a, _, _ := parent.Snapshot(); a != 1 {
		t.Errorf("parent attempted = %d, want 1", a)
	}
}

// TestChildrenConcurrentRecord records from 100 goroutines on each of two
// children of one parent; run under -race it also proves the forwarding is
// free of data races.
func TestChildrenConcurrentRecord(t *testing.T) {
	parent := NewCollector()
	children := []*Collector{parent.Child(), parent.Child()}

	var wg sync.WaitGroup
	for _, c := range children {
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.Record(Sample{Type: "server", Duration: ms(i), Success: true})
			}()
		}
	}
	wg.Wait()

	if got := parent.Aggregate(time.Second).Overall.Attempted; got != 200 {
		t.Errorf("parent attempted = %d, want 200", got)
	}
	for i, c := range children {
		if a, _, _ := c.Snapshot(); a != 100 {
			t.Errorf("child %d attempted = %d, want 100", i, a)
		}
	}
}
