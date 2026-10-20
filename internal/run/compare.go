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
// when they match. Probes and BadTokens compare the per-service draws, one
// entry per service, and Propagation the propagation probes; each is nil,
// and omitted, when neither record has any.
type Comparison struct {
	Base        RunSummary        `json:"base"`
	Other       RunSummary        `json:"other"`
	Mismatches  []string          `json:"mismatches,omitempty"`
	Operations  []OperationDelta  `json:"operations"`
	Probes      []OperationDelta  `json:"probes,omitempty"`
	BadTokens   []OperationDelta  `json:"badTokens,omitempty"`
	Propagation *PropagationDelta `json:"propagation,omitempty"`
	Stages      []StageDelta      `json:"stages,omitempty"`
}

// RunSummary is what identifies a pressure run's load. Stages holds the
// fan_assignments override of every stage, 0 for none. PropagationEvery and
// PropagationTimeout are the propagation probe's interval and ceiling, both 0
// and omitted for a run without the probe.
type RunSummary struct {
	RunID              string           `json:"runID"`
	Scenario           string           `json:"scenario"`
	Seed               int64            `json:"seed"`
	Incomplete         bool             `json:"incomplete,omitempty"`
	Workers            int              `json:"workers"`
	Tokens             int              `json:"tokens"`
	Mix                []PressureWeight `json:"mix"`
	PropagationEvery   time.Duration    `json:"propagationEvery,omitempty"`
	PropagationTimeout time.Duration    `json:"propagationTimeout,omitempty"`
	Stages             []int            `json:"stages,omitempty"`
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

// DelayMetric is the part of a propagation probe the comparison reads.
type DelayMetric struct {
	P50      time.Duration `json:"p50"`
	P95      time.Duration `json:"p95"`
	P99      time.Duration `json:"p99"`
	Max      time.Duration `json:"max"`
	Probes   int           `json:"probes"`
	TimedOut int           `json:"timedOut"`
}

// PropagationDelta compares the propagation probes of the two records. A
// side is nil when its record ran no probe or completed none, and Delta is
// nil whenever one side is.
type PropagationDelta struct {
	Base  *DelayMetric `json:"base,omitempty"`
	Other *DelayMetric `json:"other,omitempty"`
	Delta *DelayMetric `json:"delta,omitempty"`
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
// scenario, seed, workers, token pool, mix, propagation probe settings or
// stage overrides, naming every difference; with allowMismatch the
// comparison carries them in Mismatches. Operations holds "all" first and
// then every operation either record has, sorted by name. Probes and
// BadTokens hold every service either record drew for, sorted by name and
// without an "all" entry, since the per-service lists have no overall group.
// Propagation compares the two propagation probes, nil when neither record
// ran one. Stages pairs the i-th stage of each record up to the longer list,
// each with its own "all" entry and operations, nil on the side that lacks
// the stage or the operation or drew nothing in it.
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
	c.Probes = compareByType(base.Pressure.Probes, other.Pressure.Probes)
	c.BadTokens = compareByType(base.Pressure.BadTokens, other.Pressure.BadTokens)
	c.Propagation = comparePropagation(base.Pressure.Propagation, other.Pressure.Propagation)
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
	if p := r.Pressure.Propagation; p != nil {
		s.PropagationEvery, s.PropagationTimeout = p.Every, p.Timeout
	}
	for _, st := range r.Pressure.Stages {
		s.Stages = append(s.Stages, st.FanAssignments)
	}
	return s
}

// loadMismatches names every way the loads of base and other differ, in the
// order scenario, seed, workers, token pool, mix, propagation probe
// settings, stage count and, per stage both records have, its
// fan_assignments override. It returns nil when the loads match.
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
	if base.PropagationEvery != other.PropagationEvery || base.PropagationTimeout != other.PropagationTimeout {
		m = append(m, fmt.Sprintf("propagation %s vs %s",
			formatPropagationLoad(base.PropagationEvery, base.PropagationTimeout),
			formatPropagationLoad(other.PropagationEvery, other.PropagationTimeout)))
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

// formatPropagationLoad renders the propagation probe settings of one
// compared run for a mismatch and its summary line: "off" for a run without
// the probe, "every=<every>,timeout=<timeout>" in time.Duration notation
// otherwise.
func formatPropagationLoad(every, timeout time.Duration) string {
	if every == 0 {
		return "off"
	}
	return "every=" + every.String() + ",timeout=" + timeout.String()
}

// compareAggregates compares two aggregates of the same scope: "all" from
// their overall groups first, then every operation either has, sorted by
// name. A nil aggregate stands for a record without the stage, so every
// entry is nil on its side.
func compareAggregates(base, other *metrics.Aggregate) []OperationDelta {
	deltas := []OperationDelta{operationDelta("all", overallStats(base), overallStats(other))}
	return append(deltas, compareByType(byTypeStats(base), byTypeStats(other))...)
}

// compareByType compares two lists of groups keyed by Type, the operations
// of an aggregate or the services of the probe and bad-token draws: one
// entry for every Type either list has, sorted by name. It returns nil when
// neither list has an entry.
func compareByType(base, other []metrics.Stats) []OperationDelta {
	var names []string
	for _, list := range [][]metrics.Stats{base, other} {
		for _, s := range list {
			names = append(names, s.Type)
		}
	}
	slices.Sort(names)
	var deltas []OperationDelta
	for _, name := range slices.Compact(names) {
		deltas = append(deltas, operationDelta(name, typeStats(base, name), typeStats(other, name)))
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

// byTypeStats returns the per-operation groups of a, or nil for a nil
// aggregate.
func byTypeStats(a *metrics.Aggregate) []metrics.Stats {
	if a == nil {
		return nil
	}
	return a.ByType
}

// typeStats returns the group of the named Type in list, or nil when list has
// no such group.
func typeStats(list []metrics.Stats, name string) *metrics.Stats {
	for i := range list {
		if list[i].Type == name {
			return &list[i]
		}
	}
	return nil
}

// comparePropagation compares the propagation probes of two records, either
// nil for a record without the probe. It returns nil when neither has one.
// A side that completed no probe is nil, as for a group that drew nothing,
// and the delta, other minus base field by field, is set only when both
// sides are present.
func comparePropagation(base, other *PressurePropagation) *PropagationDelta {
	if base == nil && other == nil {
		return nil
	}
	d := &PropagationDelta{Base: delayOf(base), Other: delayOf(other)}
	if d.Base != nil && d.Other != nil {
		d.Delta = &DelayMetric{
			P50:      d.Other.P50 - d.Base.P50,
			P95:      d.Other.P95 - d.Base.P95,
			P99:      d.Other.P99 - d.Base.P99,
			Max:      d.Other.Max - d.Base.Max,
			Probes:   d.Other.Probes - d.Base.Probes,
			TimedOut: d.Other.TimedOut - d.Base.TimedOut,
		}
	}
	return d
}

// delayOf returns the part of p the comparison reads, or nil when p is nil or
// completed no probe: without a probe there is no delay to compare, so the
// side shows as missing rather than as a 0s delay and a -100% change.
func delayOf(p *PressurePropagation) *DelayMetric {
	if p == nil || p.Probes == 0 {
		return nil
	}
	return &DelayMetric{
		P50:      p.Delay.Median,
		P95:      p.Delay.P95,
		P99:      p.Delay.P99,
		Max:      p.Delay.Max,
		Probes:   p.Probes,
		TimedOut: p.TimedOut,
	}
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
	return comparedLatencyCell(cm.latency(m), signed)
}

// comparedLatencyCell renders a latency of a comparison rounded to the
// microsecond, sorting by its nanoseconds; signed, for a delta, puts an
// explicit "+" before a positive value.
func comparedLatencyCell(d time.Duration, signed bool) cell {
	rounded := d.Round(time.Microsecond)
	text := rounded.String()
	if signed && rounded > 0 {
		text = "+" + text
	}
	return cell{Text: text, Sort: strconv.FormatInt(int64(d), 10)}
}

// cells renders the metric of d as its BASE, OTHER, DELTA and CHANGE cells.
// CHANGE is the changeCell of the delta, and "-" with an empty sort key when
// a side is missing.
func (cm comparisonMetric) cells(d OperationDelta) []cell {
	change := cell{Text: "-"}
	if d.Delta != nil {
		change = changeCell(cm.value(d.Delta), cm.value(d.Base))
	}
	return []cell{cm.cell(d.Base, false), cm.cell(d.Other, false), cm.cell(d.Delta, true), change}
}

// changeCell renders the CHANGE cell of a delta: the delta as a signed
// percentage of the base value with one decimal, sorting by two, and "-"
// with an empty sort key when the base value is 0. An empty sort key orders
// before every number in the HTML page's sorter.
func changeCell(delta, base float64) cell {
	if base == 0 {
		return cell{Text: "-"}
	}
	pct := delta / base * 100
	return cell{Text: fmt.Sprintf("%+.1f%%", pct), Sort: f2(pct)}
}

// propagationMetric is one of the rows the comparison shows for the
// propagation probes: its name and how to read it off a DelayMetric, either
// a latency or, when latency is nil, a count.
type propagationMetric struct {
	name    string
	latency func(*DelayMetric) time.Duration
	count   func(*DelayMetric) int
}

// propagationMetrics are the six rows of the propagation comparison, in
// their order: the four delay percentiles, then the completed and the
// timed-out probes.
var propagationMetrics = []propagationMetric{
	{name: "p50", latency: func(m *DelayMetric) time.Duration { return m.P50 }},
	{name: "p95", latency: func(m *DelayMetric) time.Duration { return m.P95 }},
	{name: "p99", latency: func(m *DelayMetric) time.Duration { return m.P99 }},
	{name: "max", latency: func(m *DelayMetric) time.Duration { return m.Max }},
	{name: "probes", count: func(m *DelayMetric) int { return m.Probes }},
	{name: "timed out", count: func(m *DelayMetric) int { return m.TimedOut }},
}

// value returns the metric of m as a number, a latency in nanoseconds.
func (pm propagationMetric) value(m *DelayMetric) float64 {
	if pm.latency == nil {
		return float64(pm.count(m))
	}
	return float64(pm.latency(m))
}

// cell renders the metric of m, "-" with an empty sort key for nil. A
// latency renders as comparedLatencyCell does and a count as an integer
// sorting by itself; signed, for a delta, gives a count an explicit sign,
// "+0" included, as a throughput delta has.
func (pm propagationMetric) cell(m *DelayMetric, signed bool) cell {
	if m == nil {
		return cell{Text: "-"}
	}
	if pm.latency != nil {
		return comparedLatencyCell(pm.latency(m), signed)
	}
	format := "%d"
	if signed {
		format = "%+d"
	}
	n := pm.count(m)
	return cell{Text: fmt.Sprintf(format, n), Sort: strconv.Itoa(n)}
}

// propagationRows renders the rows of the propagation table, one per
// propagationMetrics entry: the metric and its BASE, OTHER, DELTA and CHANGE
// cells, CHANGE being the changeCell of the delta and "-" when a side is
// missing. It returns nil for nil, a comparison without the probe.
func propagationRows(d *PropagationDelta) []typeRow {
	if d == nil {
		return nil
	}
	rows := make([]typeRow, 0, len(propagationMetrics))
	for _, pm := range propagationMetrics {
		change := cell{Text: "-"}
		if d.Delta != nil {
			change = changeCell(pm.value(d.Delta), pm.value(d.Base))
		}
		rows = append(rows, typeRow{Cells: []cell{
			{Text: pm.name, Sort: pm.name},
			pm.cell(d.Base, false), pm.cell(d.Other, false), pm.cell(d.Delta, true), change,
		}})
	}
	return rows
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
// other run with their loads, a run with the propagation probe carrying its
// settings, a checkpoint marked "(incomplete)", and the mismatches, when
// there are any; a table with four rows per operation, the p50, p95 and p99
// latencies and the throughput; when the records drew probes or bad tokens,
// a table "Probes" and a table "Bad tokens" with the same four rows per
// service; when either record ran the propagation probe, a table
// "Propagation" with the p50, p95, p99 and max delays and the completed and
// timed-out probes; and, when the records have stages, the same rows as the
// operation table per stage and operation. A latency is rounded to the
// microsecond, a throughput has one decimal and a count is an integer, a
// delta carries an explicit sign, CHANGE is the delta as a percentage of the
// base value, and "-" stands for a side without the operation, the service
// or the probe or without a draw of it and for a change without a base
// value.
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

	for _, t := range []struct {
		title  string
		deltas []OperationDelta
	}{{"Probes", c.Probes}, {"Bad tokens", c.BadTokens}} {
		if len(t.deltas) == 0 {
			continue
		}
		rows := [][]string{{"SERVICE", "METRIC", "BASE", "OTHER", "DELTA", "CHANGE"}}
		rows = append(rows, cellTexts(comparisonRows(nil, t.deltas))...)
		// SERVICE and METRIC are labels; the others are numbers.
		if err := writeAlignedTable(&b, t.title, rows, []bool{true, true, false, false, false, false}); err != nil {
			return fmt.Errorf("writing comparison table: %w", err)
		}
	}

	if c.Propagation != nil {
		rows := [][]string{{"METRIC", "BASE", "OTHER", "DELTA", "CHANGE"}}
		rows = append(rows, cellTexts(propagationRows(c.Propagation))...)
		// Only METRIC is a label; the others are numbers.
		if err := writeAlignedTable(&b, "Propagation", rows, []bool{true, false, false, false, false}); err != nil {
			return fmt.Errorf("writing comparison table: %w", err)
		}
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

// formatRunSummary renders the load of one compared run on one line. The
// propagation probe settings close the line only for a run with the probe,
// so the line of a run without it reads as it did before the probe existed.
func formatRunSummary(s RunSummary) string {
	id := s.RunID
	if s.Incomplete {
		id += " (incomplete)"
	}
	line := fmt.Sprintf("run %s, scenario %s, seed %d, workers %d, tokens %d, mix %s",
		id, s.Scenario, s.Seed, s.Workers, s.Tokens, formatPressureMix(s.Mix))
	if s.PropagationEvery > 0 {
		line += ", propagation " + formatPropagationLoad(s.PropagationEvery, s.PropagationTimeout)
	}
	return line
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
// and the operation, probe, bad-token, propagation and stage tables of
// WriteComparisonTable as sortable tables. Like the report it references no
// external resources, and every string from a record flows through
// html/template auto-escaping.
func WriteComparisonHTML(w io.Writer, c *Comparison) error {
	if err := compareTemplate.Execute(w, buildComparisonView(c)); err != nil {
		return fmt.Errorf("writing html comparison: %w", err)
	}
	return nil
}
