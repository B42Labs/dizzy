package run

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
// metrics; an apply run (Chaos nil) renders exactly as before. A pressure run
// appends the pressure tables of WritePressureTable after them. An incomplete
// record (a chaos or pressure checkpoint) starts with a line saying so and
// when it was written, then a blank line, then the same output as a complete
// record. A mix record then gets the persona table and, per persona, a
// section with its own metrics and churn summary, and after them the lane
// table and the same section per background lane.
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
	if r.Pressure != nil {
		if err := WritePressureTable(w, r.Pressure); err != nil {
			return err
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
	left := make([]bool, len(rows[0]))
	for i := range min(labels, len(left)) {
		left[i] = true
	}
	return writeAlignedTable(w, title, rows, left)
}

// writeAlignedTable writes a blank line, title and rows as an aligned table,
// two spaces between columns: column i is left-aligned when left[i] is true
// and right-aligned otherwise. Widths count runes, as fmt pads by runes, so a
// cell such as "350µs" lines up with an ASCII one. Trailing spaces, which a
// row with empty last cells would leave, are cut from every line. It returns
// the writer's error unwrapped.
func writeAlignedTable(w io.Writer, title string, rows [][]string, left []bool) error {
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], utf8.RuneCountInString(cell))
		}
	}
	var b, line strings.Builder
	b.WriteString("\n" + title + "\n")
	for _, row := range rows {
		line.Reset()
		for i, cell := range row {
			if i > 0 {
				line.WriteString("  ")
			}
			if left[i] {
				fmt.Fprintf(&line, "%-*s", widths[i], cell)
			} else {
				fmt.Fprintf(&line, "%*s", widths[i], cell)
			}
		}
		b.WriteString(strings.TrimRight(line.String(), " ") + "\n")
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

// WritePressureTable renders the statistics of a pressure run: a block with
// the workers, the token pool size, the churn users (only when the run
// reserved any), the mix and the revocations; a table with one row per
// operation; and, when the run has time buckets, a table with one row for
// each bucket as a whole, carrying its cumulative revocations and its error
// breakdown, followed by one row per operation of that bucket. The error
// breakdown is recorded per bucket, not per operation, so the operation rows
// leave those two cells empty. Latencies are rounded to the microsecond,
// because the token path moves in sub-millisecond steps, and show as "-" for
// a row without operations. It writes nothing for nil.
func WritePressureTable(w io.Writer, p *PressureStats) error {
	if p == nil {
		return nil
	}
	var b strings.Builder
	b.WriteString("\nPressure\n")
	fmt.Fprintf(&b, "  workers:     %d\n", p.Workers)
	fmt.Fprintf(&b, "  token pool:  %d\n", p.Tokens)
	if p.ChurnUsers != 0 {
		fmt.Fprintf(&b, "  churn users: %d\n", p.ChurnUsers)
	}
	fmt.Fprintf(&b, "  mix:         %s\n", formatPressureMix(p.Mix))
	fmt.Fprintf(&b, "  revocations: %d\n", p.Revocations)

	rows := [][]string{{"OPERATION", "OPS", "OK", "FAILED", "OPS/S", "MIN", "MEAN", "P50", "P90", "P95", "P99", "MAX"}}
	for _, s := range p.Operations.ByType {
		row := []string{s.Type, strconv.Itoa(s.Attempted), strconv.Itoa(s.Succeeded), strconv.Itoa(s.Failed),
			fmt.Sprintf("%.1f", s.Throughput)}
		rows = append(rows, append(row, microCells(s,
			s.Latency.Min, s.Latency.Mean, s.Latency.Median, s.Latency.P90, s.Latency.P95, s.Latency.P99, s.Latency.Max)...))
	}
	// Only OPERATION is a label; every other column is a number.
	if err := writeStatsTable(&b, "Operations", rows, 1); err != nil {
		return fmt.Errorf("writing pressure table: %w", err)
	}

	if len(p.Buckets) > 0 {
		rows := [][]string{{"START", "OPERATION", "OPS", "OK", "FAILED", "P50", "P99", "REVOCATIONS", "ERRORS"}}
		for _, bk := range p.Buckets {
			start := bk.Start.Round(time.Millisecond).String()
			rows = append(rows, pressureBucketRow(start, "all", bk.Operations.Overall,
				strconv.Itoa(bk.Revocations), formatBucketErrors(bk.Operations.Errors)))
			for _, s := range bk.Operations.ByType {
				rows = append(rows, pressureBucketRow(start, s.Type, s, "", ""))
			}
		}
		// START, OPERATION and ERRORS are labels; the others are numbers.
		left := []bool{true, true, false, false, false, false, false, false, true}
		if err := writeAlignedTable(&b, "Latency and errors over time", rows, left); err != nil {
			return fmt.Errorf("writing pressure table: %w", err)
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("writing pressure table: %w", err)
	}
	return nil
}

// pressureBucketRow renders one row of the pressure bucket table: the
// bucket's start, the operation label, the operation counts, the p50 and p99
// latencies, and the revocations and errors cells as given.
func pressureBucketRow(start, label string, s metrics.Stats, revocations, errs string) []string {
	row := []string{start, label, strconv.Itoa(s.Attempted), strconv.Itoa(s.Succeeded), strconv.Itoa(s.Failed)}
	row = append(row, microCells(s, s.Latency.Median, s.Latency.P99)...)
	return append(row, revocations, errs)
}

// microCells renders latencies of s rounded to the microsecond, or "-" for
// each when s attempted nothing, so an empty row does not read as "0s".
func microCells(s metrics.Stats, ds ...time.Duration) []string {
	cells := make([]string, 0, len(ds))
	for _, d := range ds {
		if s.Attempted == 0 {
			cells = append(cells, "-")
		} else {
			cells = append(cells, d.Round(time.Microsecond).String())
		}
	}
	return cells
}

// formatPressureMix renders a pressure run's mix as "operation=weight" pairs
// in the record's order, or "-" when it is empty.
func formatPressureMix(mix []PressureWeight) string {
	if len(mix) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(mix))
	for _, m := range mix {
		parts = append(parts, m.Operation+"="+strconv.Itoa(m.Weight))
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
// shape is unchanged. A pressure run carries its statistics under a
// "pressure" key in the same way. A mix run carries its "services", when it
// has any, its "personas" and its "lanes", when it has any, next to the
// overall metrics.
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
	case r.Pressure != nil:
		payload = struct {
			Metrics    metrics.Aggregate `json:"metrics"`
			Pressure   *PressureStats    `json:"pressure"`
			Incomplete bool              `json:"incomplete,omitempty"`
		}{Metrics: r.Metrics, Pressure: r.Pressure, Incomplete: r.Incomplete}
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
// resource type plus a leading overall row. A pressure run appends one row per
// operation labelled pressure/<operation> and then, per time bucket, one row
// per operation of that bucket labelled pressure@<start>/<operation>; the
// cumulative revocations have no column in the fixed header. A mix run
// appends, per persona, an overall row labelled <persona>/overall and one row
// per type labelled <persona>/<type>, and then the same rows per background
// lane, labelled <lane>/overall and <lane>/<type>.
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
	if r.Pressure != nil {
		if err := writePressureRows(cw, r.Pressure); err != nil {
			return err
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

// writePressureRows writes one row per operation of a pressure run, labelled
// pressure/<operation>, and then, per time bucket, one row per operation of
// that bucket, labelled pressure@<start>/<operation> with start in
// time.Duration notation.
func writePressureRows(cw *csv.Writer, p *PressureStats) error {
	for _, s := range p.Operations.ByType {
		if err := cw.Write(statsRow("pressure/"+s.Type, s)); err != nil {
			return fmt.Errorf("writing csv row: %w", err)
		}
	}
	for _, bk := range p.Buckets {
		prefix := "pressure@" + bk.Start.String() + "/"
		for _, s := range bk.Operations.ByType {
			if err := cw.Write(statsRow(prefix+s.Type, s)); err != nil {
				return fmt.Errorf("writing csv row: %w", err)
			}
		}
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
