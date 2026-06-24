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
	"strconv"
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

// Summary renders the aggregate as human-readable tables: a per-type
// throughput/latency table (with "overall" as the first row), followed by an
// error breakdown and time-to-ready table when those have data. The "neutron
// apply" command prints it so a run produces timing data even before the
// richer report formats land.
func (a Aggregate) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run metrics (wall %s)\n\n", a.Wall.Round(time.Millisecond))

	headers := []string{"TYPE", "OPS", "OK", "FAILED", "OPS/S", "MIN", "MEAN", "P50", "P90", "P95", "P99", "MAX"}
	// Only the TYPE column is text; every metric column is right-aligned.
	align := []bool{false, true, true, true, true, true, true, true, true, true, true, true}
	rows := [][]string{statsRow("overall", a.Overall)}
	for _, s := range a.ByType {
		rows = append(rows, statsRow(s.Type, s))
	}
	writeTable(&b, headers, rows, align)

	if len(a.Errors) > 0 {
		fmt.Fprint(&b, "\nErrors\n")
		rows := make([][]string, 0, len(a.Errors))
		for _, e := range a.Errors {
			rows = append(rows, []string{e.Kind, strconv.Itoa(e.Count)})
		}
		writeTable(&b, []string{"KIND", "COUNT"}, rows, []bool{false, true})
	}

	if len(a.Readiness) > 0 {
		fmt.Fprint(&b, "\nTime to ready\n")
		rows := make([][]string, 0, len(a.Readiness))
		for _, r := range a.Readiness {
			rows = append(rows, []string{
				r.Type,
				fmt.Sprintf("%d/%d", r.OK, r.Count),
				dur(r.Latency.Median),
				dur(r.Latency.Max),
			})
		}
		writeTable(&b, []string{"TYPE", "READY", "MEDIAN", "MAX"}, rows, []bool{false, true, true, true})
	}

	return b.String()
}

// statsRow renders one Stats as a table row under the given label. Groups with
// no samples show "-" in every latency column rather than a misleading "0s".
func statsRow(label string, s Stats) []string {
	row := []string{label, strconv.Itoa(s.Attempted), strconv.Itoa(s.Succeeded),
		strconv.Itoa(s.Failed), fmt.Sprintf("%.1f", s.Throughput)}
	if s.Attempted == 0 {
		return append(row, "-", "-", "-", "-", "-", "-", "-")
	}
	return append(row,
		dur(s.Latency.Min), dur(s.Latency.Mean), dur(s.Latency.Median),
		dur(s.Latency.P90), dur(s.Latency.P95), dur(s.Latency.P99), dur(s.Latency.Max))
}

// dur formats a duration for the tables, rounded to the millisecond.
func dur(d time.Duration) string { return d.Round(time.Millisecond).String() }

// writeTable renders headers and rows as a column-aligned table with a dashed
// separator under the header. Columns whose rightAlign entry is true are
// right-aligned (numbers); the rest are left-aligned (labels). Every row,
// including headers, is assumed to have len(headers) cells.
func writeTable(b *strings.Builder, headers []string, rows [][]string, rightAlign []bool) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	writeRow := func(cells []string) {
		for i, cell := range cells {
			if i > 0 {
				b.WriteString("  ")
			}
			if rightAlign[i] {
				fmt.Fprintf(b, "%*s", widths[i], cell)
			} else if i == len(cells)-1 {
				b.WriteString(cell) // avoid trailing padding on a left-aligned last column
			} else {
				fmt.Fprintf(b, "%-*s", widths[i], cell)
			}
		}
		b.WriteString("\n")
	}

	sep := make([]string, len(headers))
	for i := range sep {
		sep[i] = strings.Repeat("-", widths[i])
	}

	writeRow(headers)
	writeRow(sep)
	for _, row := range rows {
		writeRow(row)
	}
}
