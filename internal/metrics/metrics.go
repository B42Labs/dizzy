// Package metrics collects per-call timing and time-to-ready samples emitted by
// the Neutron wrappers during a run and aggregates them into latency
// percentiles, throughput, and an error breakdown. The collector is safe for
// use by the concurrent workers of the executor; the aggregation it produces is
// pure data so callers can render or persist it however they need.
package metrics

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sample records one wrapped Neutron API attempt, including retried attempts.
// ErrKind is empty when Success is true.
type Sample struct {
	Type     string
	Duration time.Duration
	Success  bool
	ErrKind  string
}

// Readiness records the time a status-bearing resource took to reach its
// expected status after create returned. OK is false when the resource did not
// reach the expected status before the readiness deadline.
type Readiness struct {
	Type     string
	Duration time.Duration
	OK       bool
}

// Collector accumulates samples and readiness records from concurrent workers.
// Its zero value is not usable; construct it with NewCollector.
type Collector struct {
	mu        sync.Mutex
	samples   []Sample
	readiness []Readiness
}

// NewCollector returns an empty Collector ready for concurrent use.
func NewCollector() *Collector {
	return &Collector{}
}

// Record appends one API-call sample.
func (c *Collector) Record(s Sample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, s)
}

// RecordReadiness appends one time-to-ready record.
func (c *Collector) RecordReadiness(r Readiness) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readiness = append(c.readiness, r)
}

// Aggregate summarizes every recorded sample over the supplied wall-clock
// duration: overall and per-type counts, latency percentiles, and throughput,
// plus an error breakdown by kind and per-type time-to-ready statistics.
func (c *Collector) Aggregate(wall time.Duration) Aggregate {
	c.mu.Lock()
	samples := make([]Sample, len(c.samples))
	copy(samples, c.samples)
	readiness := make([]Readiness, len(c.readiness))
	copy(readiness, c.readiness)
	c.mu.Unlock()

	agg := Aggregate{
		Wall:    wall,
		Overall: computeStats("", samples, wall),
	}

	byType := make(map[string][]Sample)
	for _, s := range samples {
		byType[s.Type] = append(byType[s.Type], s)
	}
	for typ, group := range byType {
		agg.ByType = append(agg.ByType, computeStats(typ, group, wall))
	}
	sort.Slice(agg.ByType, func(i, j int) bool { return agg.ByType[i].Type < agg.ByType[j].Type })

	errCounts := make(map[string]int)
	for _, s := range samples {
		if s.ErrKind != "" {
			errCounts[s.ErrKind]++
		}
	}
	for kind, count := range errCounts {
		agg.Errors = append(agg.Errors, ErrorCount{Kind: kind, Count: count})
	}
	sort.Slice(agg.Errors, func(i, j int) bool { return agg.Errors[i].Kind < agg.Errors[j].Kind })

	readyByType := make(map[string][]Readiness)
	for _, r := range readiness {
		readyByType[r.Type] = append(readyByType[r.Type], r)
	}
	for typ, group := range readyByType {
		stats := ReadinessStats{Type: typ, Count: len(group)}
		durs := make([]time.Duration, 0, len(group))
		for _, r := range group {
			if r.OK {
				stats.OK++
			}
			durs = append(durs, r.Duration)
		}
		stats.Latency = computeLatency(durs)
		agg.Readiness = append(agg.Readiness, stats)
	}
	sort.Slice(agg.Readiness, func(i, j int) bool { return agg.Readiness[i].Type < agg.Readiness[j].Type })

	return agg
}

// Aggregate is the computed summary of a run's samples. All slices are sorted
// by their key so the output is deterministic for a given set of samples.
type Aggregate struct {
	Wall      time.Duration    `json:"wall"`
	Overall   Stats            `json:"overall"`
	ByType    []Stats          `json:"byType"`
	Errors    []ErrorCount     `json:"errors"`
	Readiness []ReadinessStats `json:"readiness"`
}

// Stats holds the counts, latency distribution, and throughput for one group of
// samples. Type is empty for the overall group.
type Stats struct {
	Type       string  `json:"type"`
	Attempted  int     `json:"attempted"`
	Succeeded  int     `json:"succeeded"`
	Failed     int     `json:"failed"`
	Throughput float64 `json:"throughput"`
	Latency    Latency `json:"latency"`
}

// Latency is the distribution of call durations within a group.
type Latency struct {
	Min    time.Duration `json:"min"`
	Mean   time.Duration `json:"mean"`
	Median time.Duration `json:"median"`
	P90    time.Duration `json:"p90"`
	P95    time.Duration `json:"p95"`
	P99    time.Duration `json:"p99"`
	Max    time.Duration `json:"max"`
}

// ErrorCount is the number of failed samples that classified to a given kind.
type ErrorCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// ReadinessStats summarizes the time-to-ready records for one resource type.
type ReadinessStats struct {
	Type    string  `json:"type"`
	Count   int     `json:"count"`
	OK      int     `json:"ok"`
	Latency Latency `json:"latency"`
}

// computeStats builds a Stats for one labeled group of samples over the given
// wall-clock duration.
func computeStats(typ string, samples []Sample, wall time.Duration) Stats {
	stats := Stats{Type: typ, Attempted: len(samples)}
	durs := make([]time.Duration, 0, len(samples))
	for _, s := range samples {
		if s.Success {
			stats.Succeeded++
		}
		durs = append(durs, s.Duration)
	}
	stats.Failed = stats.Attempted - stats.Succeeded
	stats.Latency = computeLatency(durs)
	if wall > 0 {
		stats.Throughput = float64(stats.Succeeded) / wall.Seconds()
	}
	return stats
}

// computeLatency returns the latency distribution of the supplied durations.
// The zero Latency is returned for an empty input.
func computeLatency(durs []time.Duration) Latency {
	if len(durs) == 0 {
		return Latency{}
	}
	sorted := make([]time.Duration, len(durs))
	copy(sorted, durs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}
	return Latency{
		Min:    sorted[0],
		Mean:   sum / time.Duration(len(sorted)),
		Median: percentile(sorted, 50),
		P90:    percentile(sorted, 90),
		P95:    percentile(sorted, 95),
		P99:    percentile(sorted, 99),
		Max:    sorted[len(sorted)-1],
	}
}

// percentile returns the p-th percentile of an already-sorted slice using the
// nearest-rank method: rank = ceil(p/100 * n), clamped into range. It is pure
// and deterministic so callers can pin exact expectations in tests.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// Summary renders the aggregate as compact, human-readable text, mirroring the
// style of plan.Summary. The "neutron apply" command prints it so a run
// produces timing data even before the richer report formats land.
func (a Aggregate) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run metrics (wall %s)\n", a.Wall.Round(time.Millisecond))
	writeStats(&b, "overall", a.Overall)
	for _, s := range a.ByType {
		writeStats(&b, s.Type, s)
	}
	if len(a.Errors) > 0 {
		fmt.Fprint(&b, "  errors:\n")
		for _, e := range a.Errors {
			fmt.Fprintf(&b, "    %-12s %d\n", e.Kind+":", e.Count)
		}
	}
	if len(a.Readiness) > 0 {
		fmt.Fprint(&b, "  time-to-ready:\n")
		for _, r := range a.Readiness {
			fmt.Fprintf(&b, "    %-12s %d/%d ready, median %s, max %s\n",
				r.Type+":", r.OK, r.Count,
				r.Latency.Median.Round(time.Millisecond), r.Latency.Max.Round(time.Millisecond))
		}
	}
	return b.String()
}

// writeStats renders one Stats block under the given label.
func writeStats(b *strings.Builder, label string, s Stats) {
	fmt.Fprintf(b, "  %s: %d ops, %d ok, %d failed, %.1f ops/s\n",
		label, s.Attempted, s.Succeeded, s.Failed, s.Throughput)
	if s.Attempted > 0 {
		fmt.Fprintf(b, "    latency min %s mean %s p50 %s p90 %s p95 %s p99 %s max %s\n",
			s.Latency.Min.Round(time.Millisecond), s.Latency.Mean.Round(time.Millisecond),
			s.Latency.Median.Round(time.Millisecond), s.Latency.P90.Round(time.Millisecond),
			s.Latency.P95.Round(time.Millisecond), s.Latency.P99.Round(time.Millisecond),
			s.Latency.Max.Round(time.Millisecond))
	}
}
