package run

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/B42Labs/openstack-tester/internal/metrics"
)

// csvHeader names the columns WriteCSV emits, one stats row per resource type
// plus an overall row. Latencies are milliseconds so the file opens cleanly in a
// spreadsheet without unit conversion.
var csvHeader = []string{
	"type", "attempted", "succeeded", "failed", "throughput_ops_per_s",
	"min_ms", "mean_ms", "median_ms", "p90_ms", "p95_ms", "p99_ms", "max_ms",
}

// WriteTable renders the run's metrics as the compact human-readable summary,
// the default report format. For a churn run it appends the churn-specific
// summary and the per-time-bucket latency/error table after the standard
// metrics; an apply run (Chaos nil) renders exactly as before.
func WriteTable(w io.Writer, r *Record) error {
	if _, err := io.WriteString(w, r.Metrics.Summary()); err != nil {
		return fmt.Errorf("writing table report: %w", err)
	}
	if r.Chaos != nil {
		if err := writeChaosTable(w, r.Chaos); err != nil {
			return fmt.Errorf("writing chaos report: %w", err)
		}
	}
	return nil
}

// writeChaosTable renders the churn counters, the population summary, and a
// per-bucket latency/error table.
func writeChaosTable(w io.Writer, c *ChaosStats) error {
	var b strings.Builder
	b.WriteString("\nChurn summary\n")
	fmt.Fprintf(&b, "  creates:    %d\n", c.Creates)
	fmt.Fprintf(&b, "  deletes:    %d\n", c.Deletes)
	fmt.Fprintf(&b, "  cycles:     %d\n", c.Cycles)
	fmt.Fprintf(&b, "  population: min %d / mean %.1f / max %d (target fill %.2f)\n",
		c.PopMin, c.PopMean, c.PopMax, c.TargetFill)

	if len(c.Buckets) > 0 {
		b.WriteString("\nLatency and errors over time\n")
		fmt.Fprintf(&b, "%-12s  %5s  %5s  %6s  %10s  %10s  %s\n",
			"START", "OPS", "OK", "FAILED", "P50", "P99", "ERRORS")
		for _, bk := range c.Buckets {
			fmt.Fprintf(&b, "%-12s  %5d  %5d  %6d  %10s  %10s  %s\n",
				bk.Start.Round(time.Millisecond), bk.Stats.Attempted, bk.Stats.Succeeded, bk.Stats.Failed,
				bk.Stats.Latency.Median.Round(time.Millisecond), bk.Stats.Latency.P99.Round(time.Millisecond),
				formatBucketErrors(bk.Errors))
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("writing chaos table: %w", err)
	}
	return nil
}

// formatBucketErrors renders a bucket's error breakdown as "kind=count" pairs,
// or "-" when the bucket had no errors.
func formatBucketErrors(errs []metrics.ErrorCount) string {
	if len(errs) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, fmt.Sprintf("%s=%d", e.Kind, e.Count))
	}
	return strings.Join(parts, ", ")
}

// WriteJSON renders the run's metrics as indented JSON, the machine-readable
// report format. A churn run additionally carries its chaos statistics under a
// "chaos" key; an apply run (Chaos nil) marshals just the metrics aggregate, so
// its JSON shape is unchanged.
func WriteJSON(w io.Writer, r *Record) error {
	var payload any = r.Metrics
	if r.Chaos != nil {
		payload = struct {
			Metrics metrics.Aggregate `json:"metrics"`
			Chaos   *ChaosStats       `json:"chaos"`
		}{Metrics: r.Metrics, Chaos: r.Chaos}
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding metrics: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("writing json report: %w", err)
	}
	return nil
}

// WriteCSV renders the run's per-type and overall metrics as CSV, one row per
// resource type plus a leading overall row.
func WriteCSV(w io.Writer, r *Record) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return fmt.Errorf("writing csv header: %w", err)
	}
	if err := cw.Write(statsRow("overall", r.Metrics.Overall)); err != nil {
		return fmt.Errorf("writing csv row: %w", err)
	}
	for _, s := range r.Metrics.ByType {
		if err := cw.Write(statsRow(s.Type, s)); err != nil {
			return fmt.Errorf("writing csv row: %w", err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flushing csv report: %w", err)
	}
	return nil
}

// statsRow formats one Stats group as a CSV record under the given label.
func statsRow(label string, s metrics.Stats) []string {
	return []string{
		csvSafe(label),
		strconv.Itoa(s.Attempted),
		strconv.Itoa(s.Succeeded),
		strconv.Itoa(s.Failed),
		strconv.FormatFloat(s.Throughput, 'f', 2, 64),
		ms(s.Latency.Min), ms(s.Latency.Mean), ms(s.Latency.Median),
		ms(s.Latency.P90), ms(s.Latency.P95), ms(s.Latency.P99), ms(s.Latency.Max),
	}
}

// ms formats a duration as a millisecond value for CSV output.
func ms(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 3, 64)
}

// csvSafe neutralizes a leading formula-trigger character so a label opens as
// text, not a formula, in a spreadsheet. encoding/csv quotes separators but does
// not defend against a leading =, +, -, @ (or tab/CR), so a metric type such as
// =HYPERLINK(...) would otherwise be evaluated on open. Prefixing an apostrophe
// is the standard CSV-injection mitigation.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
