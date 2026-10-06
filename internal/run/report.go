package run

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/B42Labs/dizzy/internal/metrics"
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
// metrics; an apply run (Chaos nil) renders exactly as before. An incomplete
// record (a chaos checkpoint) starts with a line saying so and when it was
// written, then a blank line, then the same output as a complete record. A mix
// record then gets the persona table and, per persona, a section with its own
// metrics and churn summary, and after them the lane table and the same
// section per background lane.
func WriteTable(w io.Writer, r *Record) error {
	if r.Incomplete {
		if _, err := fmt.Fprintf(w, "Run incomplete: checkpoint written at %s; the run was still going or was killed before its final record\n\n",
			r.FinishedAt.UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("writing table report: %w", err)
		}
	}
	if _, err := io.WriteString(w, r.Metrics.Summary()); err != nil {
		return fmt.Errorf("writing table report: %w", err)
	}
	if r.Chaos != nil {
		if err := writeChaosTable(w, r.Chaos); err != nil {
			return fmt.Errorf("writing chaos report: %w", err)
		}
	}
	if err := WritePersonaTable(w, r.Personas); err != nil {
		return err
	}
	for _, p := range r.Personas {
		if _, err := fmt.Fprintf(w, "\nPersona %s\n%s", p.Name, p.Metrics.Summary()); err != nil {
			return fmt.Errorf("writing persona report: %w", err)
		}
		if p.Chaos != nil {
			if err := writeChaosTable(w, p.Chaos); err != nil {
				return fmt.Errorf("writing persona report: %w", err)
			}
		}
	}
	if err := WriteLaneTable(w, r.Lanes); err != nil {
		return err
	}
	for _, l := range r.Lanes {
		if _, err := fmt.Fprintf(w, "\nLane %s\n%s", l.Name, l.Metrics.Summary()); err != nil {
			return fmt.Errorf("writing lane report: %w", err)
		}
		if l.Chaos != nil {
			if err := writeChaosTable(w, l.Chaos); err != nil {
				return fmt.Errorf("writing lane report: %w", err)
			}
		}
	}
	return nil
}

// WritePersonaTable renders one row per persona of a mix run: its name,
// project, share as a whole percentage, servers, operation counts and latency
// percentiles. An empty project shows as "-", and so does the latency of a
// persona without operations. It writes nothing for an empty list.
func WritePersonaTable(w io.Writer, ps []PersonaStats) error {
	if len(ps) == 0 {
		return nil
	}
	rows := [][]string{{"NAME", "PROJECT", "SHARE", "SERVERS", "OPS", "OK", "FAILED", "P50", "P95", "P99"}}
	for _, p := range ps {
		row := []string{p.Name, projectLabel(p.ProjectID), sharePercent(p.Share), strconv.Itoa(p.Servers)}
		rows = append(rows, append(row, statsCells(p.Metrics.Overall)...))
	}
	// The labels, NAME and PROJECT, are left-aligned and the numbers right-aligned.
	if err := writeStatsTable(w, "Personas", rows, 2); err != nil {
		return fmt.Errorf("writing persona table: %w", err)
	}
	return nil
}

// WriteLaneTable renders one row per background lane of a mix run: its name,
// project, scenario, operation counts and latency percentiles, under the
// alignment and "-" rules of WritePersonaTable. It writes nothing for an empty
// list.
func WriteLaneTable(w io.Writer, ls []LaneStats) error {
	if len(ls) == 0 {
		return nil
	}
	rows := [][]string{{"NAME", "PROJECT", "SCENARIO", "OPS", "OK", "FAILED", "P50", "P95", "P99"}}
	for _, l := range ls {
		row := []string{l.Name, projectLabel(l.ProjectID), l.Scenario}
		rows = append(rows, append(row, statsCells(l.Metrics.Overall)...))
	}
	// The labels, NAME, PROJECT and SCENARIO, are left-aligned and the numbers
	// right-aligned.
	if err := writeStatsTable(w, "Lanes", rows, 3); err != nil {
		return fmt.Errorf("writing lane table: %w", err)
	}
	return nil
}

// statsCells renders the operation counts and the p50, p95 and p99 latencies
// of a persona or lane, with "-" for the latencies when it attempted nothing.
func statsCells(o metrics.Stats) []string {
	cells := []string{strconv.Itoa(o.Attempted), strconv.Itoa(o.Succeeded), strconv.Itoa(o.Failed)}
	for _, d := range []time.Duration{o.Latency.Median, o.Latency.P95, o.Latency.P99} {
		if o.Attempted == 0 {
			cells = append(cells, "-")
		} else {
			cells = append(cells, d.Round(time.Millisecond).String())
		}
	}
	return cells
}

// writeStatsTable writes a blank line, title and rows as an aligned table:
// the first labels columns left-aligned, the others right-aligned, two spaces
// apart. It returns the writer's error unwrapped.
func writeStatsTable(w io.Writer, title string, rows [][]string, labels int) error {
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], len(cell))
		}
	}
	var b strings.Builder
	b.WriteString("\n" + title + "\n")
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				b.WriteString("  ")
			}
			if i < labels {
				fmt.Fprintf(&b, "%-*s", widths[i], cell)
			} else {
				fmt.Fprintf(&b, "%*s", widths[i], cell)
			}
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// writeChaosTable renders the churn counters, the population summary, and a
// per-bucket latency/error table.
func writeChaosTable(w io.Writer, c *ChaosStats) error {
	var b strings.Builder
	b.WriteString("\nChurn summary\n")
	fmt.Fprintf(&b, "  creates:    %d\n", c.Creates)
	fmt.Fprintf(&b, "  deletes:    %d\n", c.Deletes)
	// Only a churn run that mutated (a Cinder soak with extends) shows this row,
	// so a Neutron churn report's output stays byte-identical.
	if c.Mutates > 0 {
		fmt.Fprintf(&b, "  mutates:    %d\n", c.Mutates)
	}
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

// projectLabel renders a persona's or lane's project for the reports, "-"
// when unknown.
func projectLabel(id string) string {
	if id == "" {
		return "-"
	}
	return id
}

// sharePercent renders a persona's normalized share as a whole percentage.
func sharePercent(share float64) string {
	return fmt.Sprintf("%.0f%%", share*100)
}

// WriteJSON renders the run's metrics as indented JSON, the machine-readable
// report format. A churn run additionally carries its chaos statistics under a
// "chaos" key, and "incomplete": true when the record is a mid-run checkpoint;
// an apply run (Chaos nil) marshals just the metrics aggregate, so its JSON
// shape is unchanged. A mix run carries its "services", when it has any, its
// "personas" and its "lanes", when it has any, next to the overall metrics.
func WriteJSON(w io.Writer, r *Record) error {
	var payload any = r.Metrics
	switch {
	case len(r.Personas) > 0:
		payload = struct {
			Metrics    metrics.Aggregate `json:"metrics"`
			Services   []string          `json:"services,omitempty"`
			Personas   []PersonaStats    `json:"personas"`
			Lanes      []LaneStats       `json:"lanes,omitempty"`
			Incomplete bool              `json:"incomplete,omitempty"`
		}{Metrics: r.Metrics, Services: r.Services, Personas: r.Personas, Lanes: r.Lanes, Incomplete: r.Incomplete}
	case r.Chaos != nil:
		payload = struct {
			Metrics    metrics.Aggregate `json:"metrics"`
			Chaos      *ChaosStats       `json:"chaos"`
			Incomplete bool              `json:"incomplete,omitempty"`
		}{Metrics: r.Metrics, Chaos: r.Chaos, Incomplete: r.Incomplete}
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
// resource type plus a leading overall row. A mix run appends, per persona, an
// overall row labelled <persona>/overall and one row per type labelled
// <persona>/<type>, and then the same rows per background lane, labelled
// <lane>/overall and <lane>/<type>.
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
	for _, p := range r.Personas {
		if err := writeBreakdownRows(cw, p.Name, p.Metrics); err != nil {
			return err
		}
	}
	for _, l := range r.Lanes {
		if err := writeBreakdownRows(cw, l.Name, l.Metrics); err != nil {
			return err
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flushing csv report: %w", err)
	}
	return nil
}

// writeBreakdownRows writes the overall row of one persona or lane, labelled
// <name>/overall, and one row per type, labelled <name>/<type>.
func writeBreakdownRows(cw *csv.Writer, name string, m metrics.Aggregate) error {
	if err := cw.Write(statsRow(name+"/overall", m.Overall)); err != nil {
		return fmt.Errorf("writing csv row: %w", err)
	}
	for _, s := range m.ByType {
		if err := cw.Write(statsRow(name+"/"+s.Type, s)); err != nil {
			return fmt.Errorf("writing csv row: %w", err)
		}
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
