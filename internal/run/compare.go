package run

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/B42Labs/dizzy/internal/metrics"
)

// Comparison is two pressure records read side by side: Base is the record
// of --run, Other the record of --compare, and every delta is Other minus
// Base. Mismatches names what differs between the two loads and is empty
// when they match.
type Comparison struct {
	Base       RunSummary       `json:"base"`
	Other      RunSummary       `json:"other"`
	Mismatches []string         `json:"mismatches,omitempty"`
	Operations []OperationDelta `json:"operations"`
	Stages     []StageDelta     `json:"stages,omitempty"`
}

// RunSummary is what identifies a pressure run's load. Stages holds the
// fan_assignments override of every stage, 0 for none.
type RunSummary struct {
	RunID      string           `json:"runID"`
	Scenario   string           `json:"scenario"`
	Seed       int64            `json:"seed"`
	Incomplete bool             `json:"incomplete,omitempty"`
	Workers    int              `json:"workers"`
	Tokens     int              `json:"tokens"`
	Mix        []PressureWeight `json:"mix"`
	Stages     []int            `json:"stages,omitempty"`
}

// Metric is the part of one Stats group the comparison reads.
type Metric struct {
	P50        time.Duration `json:"p50"`
	P95        time.Duration `json:"p95"`
	P99        time.Duration `json:"p99"`
	Throughput float64       `json:"throughput"`
}

// OperationDelta compares one operation, or "all" for the overall group.
// Base or Other is nil when only one record has the operation or its group
// drew nothing, and Delta is nil whenever one side is.
type OperationDelta struct {
	Operation string  `json:"operation"`
	Base      *Metric `json:"base,omitempty"`
	Other     *Metric `json:"other,omitempty"`
	Delta     *Metric `json:"delta,omitempty"`
}

// StageDelta compares the stages of the same index. FanAssignments is the
// base's override, or the other's when only it has the stage.
type StageDelta struct {
	Index          int              `json:"index"`
	FanAssignments int              `json:"fanAssignments,omitempty"`
	Operations     []OperationDelta `json:"operations"`
}

// Compare reads two keystone pressure records side by side, base first. It
// fails for a record without pressure statistics, base checked first, and,
// unless allowMismatch is set, for two records whose loads differ in their
// scenario, seed, workers, token pool, mix or stage overrides, naming every
// difference; with allowMismatch the comparison carries them in Mismatches.
// Operations holds "all" first and then every operation either record has,
// sorted by name. Stages pairs the i-th stage of each record up to the
// longer list, each with its own "all" entry and operations, nil on the side
// that lacks the stage or the operation or drew nothing in it.
func Compare(base, other *Record, allowMismatch bool) (*Comparison, error) {
	for _, r := range []*Record{base, other} {
		if r.Pressure == nil {
			return nil, fmt.Errorf("run %s has no pressure statistics; the comparison needs two keystone pressure records", r.RunID)
		}
	}
	c := &Comparison{Base: summarizeRun(base), Other: summarizeRun(other)}
	c.Mismatches = loadMismatches(c.Base, c.Other)
	if len(c.Mismatches) > 0 && !allowMismatch {
		return nil, fmt.Errorf("the records come from different loads (%s); pass --allow-mismatch to compare them anyway",
			strings.Join(c.Mismatches, "; "))
	}

	c.Operations = compareAggregates(&base.Pressure.Operations, &other.Pressure.Operations)
	baseStages, otherStages := base.Pressure.Stages, other.Pressure.Stages
	for i := range max(len(baseStages), len(otherStages)) {
		sd := StageDelta{Index: i + 1}
		var b, o *metrics.Aggregate
		// The base's override, set last, wins when both records have the stage.
		if i < len(otherStages) {
			o = &otherStages[i].Operations
			sd.FanAssignments = otherStages[i].FanAssignments
		}
		if i < len(baseStages) {
			b = &baseStages[i].Operations
			sd.FanAssignments = baseStages[i].FanAssignments
		}
		sd.Operations = compareAggregates(b, o)
		c.Stages = append(c.Stages, sd)
	}
	return c, nil
}

// summarizeRun returns what identifies the load of r, a pressure record.
func summarizeRun(r *Record) RunSummary {
	s := RunSummary{
		RunID:      r.RunID,
		Scenario:   r.Scenario,
		Seed:       r.Seed,
		Incomplete: r.Incomplete,
		Workers:    r.Pressure.Workers,
		Tokens:     r.Pressure.Tokens,
		Mix:        r.Pressure.Mix,
	}
	for _, st := range r.Pressure.Stages {
		s.Stages = append(s.Stages, st.FanAssignments)
	}
	return s
}

// loadMismatches names every way the loads of base and other differ, in the
// order scenario, seed, workers, token pool, mix, stage count and, per stage
// both records have, its fan_assignments override. It returns nil when the
// loads match.
func loadMismatches(base, other RunSummary) []string {
	var m []string
	if base.Scenario != other.Scenario {
		m = append(m, fmt.Sprintf("scenario %q vs %q", base.Scenario, other.Scenario))
	}
	if base.Seed != other.Seed {
		m = append(m, fmt.Sprintf("seed %d vs %d", base.Seed, other.Seed))
	}
	if base.Workers != other.Workers {
		m = append(m, fmt.Sprintf("workers %d vs %d", base.Workers, other.Workers))
	}
	if base.Tokens != other.Tokens {
		m = append(m, fmt.Sprintf("tokens %d vs %d", base.Tokens, other.Tokens))
	}
	if !slices.Equal(base.Mix, other.Mix) {
		m = append(m, fmt.Sprintf("mix %s vs %s", formatPressureMix(base.Mix), formatPressureMix(other.Mix)))
	}
	if len(base.Stages) != len(other.Stages) {
		m = append(m, fmt.Sprintf("stages %d vs %d", len(base.Stages), len(other.Stages)))
	}
	for i := range min(len(base.Stages), len(other.Stages)) {
		if base.Stages[i] != other.Stages[i] {
			m = append(m, fmt.Sprintf("stage %d fan_assignments %d vs %d", i+1, base.Stages[i], other.Stages[i]))
		}
	}
	return m
}

// compareAggregates compares two aggregates of the same scope: "all" from
// their overall groups first, then every operation either has, sorted by
// name. A nil aggregate stands for a record without the stage, so every
// entry is nil on its side.
func compareAggregates(base, other *metrics.Aggregate) []OperationDelta {
	deltas := []OperationDelta{operationDelta("all", overallStats(base), overallStats(other))}
	var names []string
	for _, a := range []*metrics.Aggregate{base, other} {
		if a == nil {
			continue
		}
		for _, s := range a.ByType {
			names = append(names, s.Type)
		}
	}
	slices.Sort(names)
	for _, name := range slices.Compact(names) {
		deltas = append(deltas, operationDelta(name, operationStats(base, name), operationStats(other, name)))
	}
	return deltas
}

// overallStats returns the overall group of a, or nil for a nil aggregate.
func overallStats(a *metrics.Aggregate) *metrics.Stats {
	if a == nil {
		return nil
	}
	return &a.Overall
}

// operationStats returns the group of the named operation in a, or nil when
// a is nil or has no such operation.
func operationStats(a *metrics.Aggregate, name string) *metrics.Stats {
	if a == nil {
		return nil
	}
	for i := range a.ByType {
		if a.ByType[i].Type == name {
			return &a.ByType[i]
		}
	}
	return nil
}

// operationDelta compares the groups base and other of one operation, either
// nil when its record lacks the operation. A group that drew nothing counts
// as missing too, and the delta is set only when both sides are present.
func operationDelta(name string, base, other *metrics.Stats) OperationDelta {
	d := OperationDelta{Operation: name, Base: metricOf(base), Other: metricOf(other)}
	if d.Base != nil && d.Other != nil {
		d.Delta = &Metric{
			P50:        d.Other.P50 - d.Base.P50,
			P95:        d.Other.P95 - d.Base.P95,
			P99:        d.Other.P99 - d.Base.P99,
			Throughput: d.Other.Throughput - d.Base.Throughput,
		}
	}
	return d
}

// metricOf returns the part of s the comparison reads, or nil when s is nil,
// its record lacking the operation, or its group drew nothing. A group
// without draws has no latency and no throughput to compare, so it shows as
// a missing side rather than as a 0s latency and a -100% change.
func metricOf(s *metrics.Stats) *Metric {
	if s == nil || s.Attempted == 0 {
		return nil
	}
	return &Metric{P50: s.Latency.Median, P95: s.Latency.P95, P99: s.Latency.P99, Throughput: s.Throughput}
}

// comparisonMetric is one of the rows a comparison shows per operation: its
// name and, for a latency, how to read it off a Metric; latency is nil for
// the throughput.
type comparisonMetric struct {
	name    string
	latency func(*Metric) time.Duration
}

// comparisonMetrics are the four rows a comparison shows per operation, in
// their order.
var comparisonMetrics = []comparisonMetric{
	{"p50", func(m *Metric) time.Duration { return m.P50 }},
	{"p95", func(m *Metric) time.Duration { return m.P95 }},
	{"p99", func(m *Metric) time.Duration { return m.P99 }},
	{"ops/s", nil},
}

// value returns the metric of m as a number, a latency in nanoseconds.
func (cm comparisonMetric) value(m *Metric) float64 {
	if cm.latency == nil {
		return m.Throughput
	}
	return float64(cm.latency(m))
}

// cell renders the metric of m, "-" with an empty sort key for nil. A
// latency is rounded to the microsecond and sorts by its nanoseconds, a
// throughput has one decimal and sorts by two; signed, for a delta, puts an
// explicit "+" before a positive value.
func (cm comparisonMetric) cell(m *Metric, signed bool) cell {
	if m == nil {
		return cell{Text: "-"}
	}
	if cm.latency == nil {
		format := "%.1f"
		if signed {
			format = "%+.1f"
		}
		return cell{Text: fmt.Sprintf(format, m.Throughput), Sort: f2(m.Throughput)}
	}
	d := cm.latency(m).Round(time.Microsecond)
	text := d.String()
	if signed && d > 0 {
		text = "+" + text
	}
	return cell{Text: text, Sort: strconv.FormatInt(int64(cm.latency(m)), 10)}
}

// cells renders the metric of d as its BASE, OTHER, DELTA and CHANGE cells.
// CHANGE is the delta as a signed percentage of the base value with one
// decimal, sorting by two, and "-" with an empty sort key when a side is
// missing or the base value is 0. An empty sort key orders before every
// number in the HTML page's sorter.
func (cm comparisonMetric) cells(d OperationDelta) []cell {
	change := cell{Text: "-"}
	if d.Delta != nil {
		if base := cm.value(d.Base); base != 0 {
			pct := cm.value(d.Delta) / base * 100
			change = cell{Text: fmt.Sprintf("%+.1f%%", pct), Sort: f2(pct)}
		}
	}
	return []cell{cm.cell(d.Base, false), cm.cell(d.Other, false), cm.cell(d.Delta, true), change}
}

// comparisonRows renders the four metric rows of every delta, each row
// starting with the given lead cells, then the operation and the metric,
// then the BASE, OTHER, DELTA and CHANGE cells.
func comparisonRows(lead []cell, deltas []OperationDelta) []typeRow {
	var rows []typeRow
	for _, d := range deltas {
		for _, cm := range comparisonMetrics {
			row := append(slices.Clone(lead), cell{Text: d.Operation, Sort: d.Operation}, cell{Text: cm.name, Sort: cm.name})
			rows = append(rows, typeRow{Cells: append(row, cm.cells(d)...)})
		}
	}
	return rows
}

// stageComparisonRows renders the rows of the stage table: per stage the
// comparisonRows of its operations, led by the stage's index and its
// fan_assignments override, "-" sorted as -1 for none.
func stageComparisonRows(stages []StageDelta) []typeRow {
	var rows []typeRow
	for _, sd := range stages {
		index := strconv.Itoa(sd.Index)
		fan := cell{Text: fanLabel(sd.FanAssignments), Sort: strconv.Itoa(sd.FanAssignments)}
		if sd.FanAssignments == 0 {
			fan.Sort = "-1"
		}
		rows = append(rows, comparisonRows([]cell{{Text: index, Sort: index}, fan}, sd.Operations)...)
	}
	return rows
}

// WriteComparisonTable renders c as text: a block naming the base and the
// other run with their loads, a checkpoint marked "(incomplete)", and the
// mismatches, when there are any; a table with four rows per operation, the
// p50, p95 and p99 latencies and the throughput; and, when the records have
// stages, the same rows per stage and operation. A latency is rounded to the
// microsecond and a throughput has one decimal, a delta carries an explicit
// sign, CHANGE is the delta as a percentage of the base value, and "-" stands
// for a side without the operation or without a draw of it and for a change
// without a base value.
func WriteComparisonTable(w io.Writer, c *Comparison) error {
	var b strings.Builder
	b.WriteString("Comparison\n")
	fmt.Fprintf(&b, "  base:  %s\n", formatRunSummary(c.Base))
	fmt.Fprintf(&b, "  other: %s\n", formatRunSummary(c.Other))
	if len(c.Mismatches) > 0 {
		fmt.Fprintf(&b, "  loads differ: %s\n", strings.Join(c.Mismatches, "; "))
	}

	rows := [][]string{{"OPERATION", "METRIC", "BASE", "OTHER", "DELTA", "CHANGE"}}
	rows = append(rows, cellTexts(comparisonRows(nil, c.Operations))...)
	// OPERATION and METRIC are labels; the others are numbers.
	if err := writeAlignedTable(&b, "Operations", rows, []bool{true, true, false, false, false, false}); err != nil {
		return fmt.Errorf("writing comparison table: %w", err)
	}

	if len(c.Stages) > 0 {
		rows := [][]string{{"STAGE", "FAN", "OPERATION", "METRIC", "BASE", "OTHER", "DELTA", "CHANGE"}}
		rows = append(rows, cellTexts(stageComparisonRows(c.Stages))...)
		// OPERATION and METRIC are labels; the others, STAGE and FAN among
		// them, are numbers.
		if err := writeAlignedTable(&b, "Stages", rows, []bool{false, false, true, true, false, false, false, false}); err != nil {
			return fmt.Errorf("writing comparison table: %w", err)
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("writing comparison table: %w", err)
	}
	return nil
}

// formatRunSummary renders the load of one compared run on one line.
func formatRunSummary(s RunSummary) string {
	id := s.RunID
	if s.Incomplete {
		id += " (incomplete)"
	}
	return fmt.Sprintf("run %s, scenario %s, seed %d, workers %d, tokens %d, mix %s",
		id, s.Scenario, s.Seed, s.Workers, s.Tokens, formatPressureMix(s.Mix))
}

// cellTexts returns the shown text of every cell of rows.
func cellTexts(rows []typeRow) [][]string {
	texts := make([][]string, 0, len(rows))
	for _, row := range rows {
		text := make([]string, 0, len(row.Cells))
		for _, c := range row.Cells {
			text = append(text, c.Text)
		}
		texts = append(texts, text)
	}
	return texts
}

// WriteComparisonJSON renders c as indented JSON, the machine-readable form
// of the comparison.
func WriteComparisonJSON(w io.Writer, c *Comparison) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding comparison: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("writing json comparison: %w", err)
	}
	return nil
}

// WriteComparisonHTML renders c as a single self-contained HTML page in the
// style of the report: the two runs as chips, the mismatches as a warning,
// and the operation and stage tables of WriteComparisonTable as sortable
// tables. Like the report it references no external resources, and every
// string from a record flows through html/template auto-escaping.
func WriteComparisonHTML(w io.Writer, c *Comparison) error {
	if err := compareTemplate.Execute(w, buildComparisonView(c)); err != nil {
		return fmt.Errorf("writing html comparison: %w", err)
	}
	return nil
}
