package run

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/B42Labs/dizzy/internal/metrics"
)

// comparisonRecords returns two pressure records of the same load: press001
// is pressureRecord, and press002 is the same run with a validate that is
// faster at p50 (280µs against 350µs), equal at p95 (900µs), slower at p99
// (1.5ms against 1.2ms) and runs 0.5 ops/s against 0.4, and that drew no
// revoke. Neither has stages.
func comparisonRecords() (base, other *Record) {
	base, other = pressureRecord(), pressureRecord()
	other.RunID = "press002"
	ops := &other.Pressure.Operations
	ops.ByType = ops.ByType[1:]
	v := &ops.ByType[0]
	v.Latency.Median, v.Latency.P99, v.Throughput = 280*time.Microsecond, 1500*time.Microsecond, 0.5
	return base, other
}

// pressureStages returns one one-minute stage per override in fans, each
// with a validate entry whose p50 is p50 plus 100µs per stage index.
func pressureStages(p50 time.Duration, fans ...int) []PressureStage {
	stages := make([]PressureStage, 0, len(fans))
	for i, fan := range fans {
		s := metrics.Stats{Attempted: 10, Succeeded: 10, Throughput: 0.17, Latency: metrics.Latency{
			Median: p50 + time.Duration(i)*100*time.Microsecond, P95: 900 * time.Microsecond, P99: 1200 * time.Microsecond}}
		validate := s
		validate.Type = "validate"
		stages = append(stages, PressureStage{
			Index:          i + 1,
			FanAssignments: fan,
			Start:          time.Duration(i) * time.Minute,
			End:            time.Duration(i+1) * time.Minute,
			Operations:     metrics.Aggregate{Wall: time.Minute, Overall: s, ByType: []metrics.Stats{validate}},
		})
	}
	return stages
}

// mustCompare compares base with other, failing the test on an error.
func mustCompare(t *testing.T, base, other *Record, allowMismatch bool) *Comparison {
	t.Helper()
	c, err := Compare(base, other, allowMismatch)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	return c
}

// TestCompareNeedsPressureRecords confirms Compare refuses a record without
// pressure statistics in either position, naming that record's run id, and
// names the base when neither has any.
func TestCompareNeedsPressureRecords(t *testing.T) {
	chaos := chaosRecord()
	chaos.RunID = "chaos001"
	tests := []struct {
		name        string
		base, other *Record
		want        string
	}{
		{"chaos base", chaos, pressureRecord(), "run chaos001 has no pressure statistics; the comparison needs two keystone pressure records"},
		{"chaos other", pressureRecord(), chaos, "run chaos001 has no pressure statistics; the comparison needs two keystone pressure records"},
		{"neither", sampleRecord(), chaos, "run abcd1234 has no pressure statistics; the comparison needs two keystone pressure records"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, allow := range []bool{false, true} {
				c, err := Compare(tc.base, tc.other, allow)
				if err == nil || err.Error() != tc.want {
					t.Errorf("Compare(allowMismatch %v) error = %v, want %q", allow, err, tc.want)
				}
				if c != nil {
					t.Errorf("Compare(allowMismatch %v) returned a comparison with its error: %+v", allow, c)
				}
			}
		})
	}
}

// TestCompareMismatches confirms Compare refuses two records whose loads
// differ in every way at once with one error naming each difference, in the
// order scenario, seed, workers, tokens, mix, stage count and stage override,
// and that allowMismatch returns a comparison carrying the same entries.
func TestCompareMismatches(t *testing.T) {
	base := pressureRecord()
	base.Pressure.Stages = pressureStages(300*time.Microsecond, 500, 1000)
	other := pressureRecord()
	other.RunID = "press002"
	other.Scenario = "large"
	other.Seed = 43
	other.Pressure.Workers = 8
	other.Pressure.Tokens = 6
	other.Pressure.Mix = []PressureWeight{{Operation: "validate", Weight: 9}}
	other.Pressure.Stages = pressureStages(300*time.Microsecond, 1000)

	_, err := Compare(base, other, false)
	const want = `the records come from different loads (scenario "small" vs "large"; seed 42 vs 43; workers 4 vs 8; tokens 3 vs 6; mix revoke=1, validate=9 vs validate=9; stages 2 vs 1; stage 1 fan_assignments 500 vs 1000); pass --allow-mismatch to compare them anyway`
	if err == nil || err.Error() != want {
		t.Fatalf("Compare error = %v, want %q", err, want)
	}

	c := mustCompare(t, base, other, true)
	wantMismatches := []string{
		`scenario "small" vs "large"`,
		"seed 42 vs 43",
		"workers 4 vs 8",
		"tokens 3 vs 6",
		"mix revoke=1, validate=9 vs validate=9",
		"stages 2 vs 1",
		"stage 1 fan_assignments 500 vs 1000",
	}
	if !reflect.DeepEqual(c.Mismatches, wantMismatches) {
		t.Errorf("Mismatches = %q, want %q", c.Mismatches, wantMismatches)
	}
}

// TestCompareAllowMismatch confirms a single difference, a stage override,
// is refused alone and carried with allowMismatch, that the stage whose
// overrides differ carries the base's override in either order, and that two
// records of the same load compare without mismatches whatever allowMismatch
// says.
func TestCompareAllowMismatch(t *testing.T) {
	base, other := comparisonRecords()
	base.Pressure.Stages = pressureStages(300*time.Microsecond, 0, 1000)
	other.Pressure.Stages = pressureStages(300*time.Microsecond, 0, 2000)

	_, err := Compare(base, other, false)
	const want = "the records come from different loads (stage 2 fan_assignments 1000 vs 2000); pass --allow-mismatch to compare them anyway"
	if err == nil || err.Error() != want {
		t.Fatalf("Compare error = %v, want %q", err, want)
	}
	c := mustCompare(t, base, other, true)
	if !reflect.DeepEqual(c.Mismatches, []string{"stage 2 fan_assignments 1000 vs 2000"}) {
		t.Errorf("Mismatches = %q, want the one stage override", c.Mismatches)
	}
	if len(c.Operations) == 0 || len(c.Stages) != 2 {
		t.Fatalf("comparison with a mismatch has %d operations and %d stages, want them all", len(c.Operations), len(c.Stages))
	}
	if fan := c.Stages[1].FanAssignments; fan != 1000 {
		t.Errorf("stage 2 override = %d, want the base's 1000 over the other's 2000", fan)
	}
	if fan := mustCompare(t, other, base, true).Stages[1].FanAssignments; fan != 2000 {
		t.Errorf("stage 2 override with the records swapped = %d, want the base's 2000 over the other's 1000", fan)
	}

	other.Pressure.Stages = pressureStages(300*time.Microsecond, 0, 1000)
	for _, allow := range []bool{false, true} {
		if c := mustCompare(t, base, other, allow); c.Mismatches != nil {
			t.Errorf("Compare(allowMismatch %v) of the same load has mismatches %q", allow, c.Mismatches)
		}
	}
}

// TestCompareOperationsAndStages confirms the comparison lists "all" first
// and then the operations either record has by name, with the metrics of
// each side and the other minus the base as the delta; that an operation
// only one record has is nil on the other side and has no delta; and that a
// stage only one record has is nil on the other side for every entry and
// takes its override from the record that has it.
func TestCompareOperationsAndStages(t *testing.T) {
	base, other := comparisonRecords()
	c := mustCompare(t, base, other, false)

	var names []string
	for _, d := range c.Operations {
		names = append(names, d.Operation)
	}
	if want := []string{"all", "revoke", "validate"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("operations = %v, want %v", names, want)
	}
	all := c.Operations[0]
	if all.Base == nil || all.Base.Throughput != 0.44 || all.Delta == nil {
		t.Errorf("all = %+v, want the overall groups with a delta", all)
	}
	validate := c.Operations[2]
	wantBase := &Metric{P50: 350 * time.Microsecond, P95: 900 * time.Microsecond, P99: 1200 * time.Microsecond, Throughput: 0.4}
	wantOther := &Metric{P50: 280 * time.Microsecond, P95: 900 * time.Microsecond, P99: 1500 * time.Microsecond, Throughput: 0.5}
	if !reflect.DeepEqual(validate.Base, wantBase) || !reflect.DeepEqual(validate.Other, wantOther) {
		t.Errorf("validate base %+v, other %+v, want %+v and %+v", validate.Base, validate.Other, wantBase, wantOther)
	}
	if d := validate.Delta; d == nil || d.P50 != -70*time.Microsecond || d.P95 != 0 || d.P99 != 300*time.Microsecond ||
		math.Abs(d.Throughput-0.1) > 1e-9 {
		t.Errorf("validate delta = %+v, want -70µs, 0, +300µs and +0.1", d)
	}
	if revoke := c.Operations[1]; revoke.Base == nil || revoke.Other != nil || revoke.Delta != nil {
		t.Errorf("revoke, which only the base drew = %+v, want a base and no other or delta", revoke)
	}
	reversed := mustCompare(t, other, base, false)
	if revoke := reversed.Operations[1]; revoke.Operation != "revoke" || revoke.Base != nil || revoke.Other == nil || revoke.Delta != nil {
		t.Errorf("revoke, which only the other drew = %+v, want no base or delta", revoke)
	}
	if c.Stages != nil {
		t.Errorf("records without stages compare with stages %+v", c.Stages)
	}

	base.Pressure.Stages = pressureStages(300*time.Microsecond, 0, 1000)
	other.Pressure.Stages = pressureStages(250*time.Microsecond, 0)
	for _, tc := range []struct {
		name          string
		c             *Comparison
		baseSide      bool // whether the base record is the one with stage 2
		wantFirstP50s [2]time.Duration
	}{
		{"stage only the base has", mustCompare(t, base, other, true), true, [2]time.Duration{300 * time.Microsecond, 250 * time.Microsecond}},
		{"stage only the other has", mustCompare(t, other, base, true), false, [2]time.Duration{250 * time.Microsecond, 300 * time.Microsecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.c.Stages) != 2 {
				t.Fatalf("stages = %+v, want 2", tc.c.Stages)
			}
			first := tc.c.Stages[0]
			if first.Index != 1 || first.FanAssignments != 0 || len(first.Operations) != 2 || first.Operations[0].Operation != "all" ||
				first.Operations[1].Operation != "validate" {
				t.Fatalf("stage 1 = %+v, want index 1, no override, all and validate", first)
			}
			if v := first.Operations[1]; v.Base.P50 != tc.wantFirstP50s[0] || v.Other.P50 != tc.wantFirstP50s[1] ||
				v.Delta.P50 != tc.wantFirstP50s[1]-tc.wantFirstP50s[0] {
				t.Errorf("stage 1 validate = %+v, want p50 %v and %v", v, tc.wantFirstP50s[0], tc.wantFirstP50s[1])
			}
			second := tc.c.Stages[1]
			if second.Index != 2 || second.FanAssignments != 1000 || len(second.Operations) != 2 {
				t.Fatalf("stage 2 = %+v, want index 2, override 1000 and two entries", second)
			}
			for _, d := range second.Operations {
				if d.Delta != nil || (d.Base != nil) != tc.baseSide || (d.Other != nil) == tc.baseSide {
					t.Errorf("stage 2 %s = %+v, want only the side with the stage and no delta", d.Operation, d)
				}
			}
		})
	}
}

// TestWriteComparisonTable confirms the comparison's text: the two load
// lines, the operation table with four rows per operation, latencies rounded
// to the microsecond with a signed delta, a throughput with one decimal, the
// change against the base or "-" without a base value, and "-" for a missing
// side; a checkpoint's "(incomplete)", the mismatch line before the blank
// line, and the stage table of staged records.
func TestWriteComparisonTable(t *testing.T) {
	base, other := comparisonRecords()
	var buf bytes.Buffer
	if err := WriteComparisonTable(&buf, mustCompare(t, base, other, false)); err != nil {
		t.Fatalf("WriteComparisonTable: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	wantHead := []string{
		"Comparison",
		"  base:  run press001, scenario small, seed 42, workers 4, tokens 3, mix revoke=1, validate=9",
		"  other: run press002, scenario small, seed 42, workers 4, tokens 3, mix revoke=1, validate=9",
		"",
		"Operations",
	}
	if len(lines) < len(wantHead) || !reflect.DeepEqual(lines[:len(wantHead)], wantHead) {
		t.Fatalf("comparison = %q, want it to start with %q", lines, wantHead)
	}
	// The overall groups drew 40 operations each but carry no latency, so
	// their latency rows have a base value of 0 and "-" in CHANGE.
	want := [][]string{
		{"OPERATION", "METRIC", "BASE", "OTHER", "DELTA", "CHANGE"},
		{"all", "p50", "0s", "0s", "0s", "-"},
		{"all", "p95", "0s", "0s", "0s", "-"},
		{"all", "p99", "0s", "0s", "0s", "-"},
		{"all", "ops/s", "0.4", "0.4", "+0.0", "+0.0%"},
		{"revoke", "p50", "3ms", "-", "-", "-"},
		{"revoke", "p95", "5ms", "-", "-", "-"},
		{"revoke", "p99", "5ms", "-", "-", "-"},
		{"revoke", "ops/s", "0.0", "-", "-", "-"},
		{"validate", "p50", "350µs", "280µs", "-70µs", "-20.0%"},
		{"validate", "p95", "900µs", "900µs", "0s", "+0.0%"},
		{"validate", "p99", "1.2ms", "1.5ms", "+300µs", "+25.0%"},
		{"validate", "ops/s", "0.4", "0.5", "+0.1", "+25.0%"},
	}
	rows := lines[len(wantHead):]
	if len(rows) != len(want) {
		t.Fatalf("operation table has %d lines, want %d:\n%s", len(rows), len(want), buf.String())
	}
	for i, w := range want {
		if got := strings.Fields(rows[i]); !reflect.DeepEqual(got, w) {
			t.Errorf("operation row %d = %q, want %q", i, got, w)
		}
	}
	// METRIC is left-aligned: every metric starts under its header.
	offset := utf8.RuneCountInString(rows[0][:strings.Index(rows[0], "METRIC")])
	for i, row := range rows[1:] {
		if r := []rune(row); offset > len(r) || !strings.HasPrefix(string(r[offset:]), want[i+1][1]) {
			t.Errorf("metric of operation row %d does not start under its header in %q", i+1, row)
		}
	}

	other.Incomplete = true
	other.Pressure.Workers = 8
	buf.Reset()
	if err := WriteComparisonTable(&buf, mustCompare(t, base, other, true)); err != nil {
		t.Fatalf("WriteComparisonTable(mismatch): %v", err)
	}
	lines = strings.Split(buf.String(), "\n")
	wantHead = []string{
		"Comparison",
		"  base:  run press001, scenario small, seed 42, workers 4, tokens 3, mix revoke=1, validate=9",
		"  other: run press002 (incomplete), scenario small, seed 42, workers 8, tokens 3, mix revoke=1, validate=9",
		"  loads differ: workers 4 vs 8",
		"",
		"Operations",
	}
	if !reflect.DeepEqual(lines[:len(wantHead)], wantHead) {
		t.Errorf("comparison with a mismatch = %q, want it to start with %q", lines, wantHead)
	}
	if strings.Contains(buf.String(), "\nStages\n") {
		t.Errorf("comparison of records without stages has a stage table:\n%s", buf.String())
	}

	base.Pressure.Stages = pressureStages(300*time.Microsecond, 0, 1000)
	other.Pressure.Stages = pressureStages(250*time.Microsecond, 0, 1000)
	buf.Reset()
	if err := WriteComparisonTable(&buf, mustCompare(t, base, other, true)); err != nil {
		t.Fatalf("WriteComparisonTable(stages): %v", err)
	}
	lines = strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	title := slices.Index(lines, "Stages")
	if title < 0 || lines[title-1] != "" {
		t.Fatalf("staged comparison lacks the stage table after a blank line:\n%s", buf.String())
	}
	stageRows := lines[title+1:]
	wantStages := map[int][]string{
		0:  {"STAGE", "FAN", "OPERATION", "METRIC", "BASE", "OTHER", "DELTA", "CHANGE"},
		1:  {"1", "-", "all", "p50", "300µs", "250µs", "-50µs", "-16.7%"},
		5:  {"1", "-", "validate", "p50", "300µs", "250µs", "-50µs", "-16.7%"},
		16: {"2", "1000", "validate", "ops/s", "0.2", "0.2", "+0.0", "+0.0%"},
	}
	if len(stageRows) != 17 {
		t.Fatalf("stage table has %d lines, want 17 (two stages of all and validate, four metrics each):\n%s", len(stageRows), buf.String())
	}
	for i, w := range wantStages {
		if got := strings.Fields(stageRows[i]); !reflect.DeepEqual(got, w) {
			t.Errorf("stage row %d = %q, want %q", i, got, w)
		}
	}
	if !strings.HasPrefix(stageRows[16], "    2  1000  validate") {
		t.Errorf("stage row %q does not right-align STAGE and FAN and left-align OPERATION", stageRows[16])
	}
}

// TestCompareStageWithoutDraws confirms a group that drew nothing counts as a
// missing side. The other record's stage 2 was entered after its window
// ended, so it has Start equal to End and an empty aggregate: its "all" entry
// is nil on that side and has no delta, prints "-" in OTHER, DELTA and CHANGE
// rather than a 0s latency and a -100% change, and leaves that side and the
// delta out of the JSON. With the records swapped the same holds for BASE.
func TestCompareStageWithoutDraws(t *testing.T) {
	base, other := comparisonRecords()
	base.Pressure.Stages = pressureStages(300*time.Microsecond, 0, 1000)
	other.Pressure.Stages = pressureStages(250*time.Microsecond, 0, 1000)
	late := &other.Pressure.Stages[1]
	late.Start = late.End
	late.Operations = metrics.Aggregate{}

	tests := []struct {
		name      string
		c         *Comparison
		emptySide string     // the JSON key of the side that drew nothing
		wantRows  [][]string // the stage table's rows of stage 2's all entry
	}{
		{"other drew nothing", mustCompare(t, base, other, false), "other", [][]string{
			{"2", "1000", "all", "p50", "400µs", "-", "-", "-"},
			{"2", "1000", "all", "p95", "900µs", "-", "-", "-"},
			{"2", "1000", "all", "p99", "1.2ms", "-", "-", "-"},
			{"2", "1000", "all", "ops/s", "0.2", "-", "-", "-"},
		}},
		{"base drew nothing", mustCompare(t, other, base, false), "base", [][]string{
			{"2", "1000", "all", "p50", "-", "400µs", "-", "-"},
			{"2", "1000", "all", "p95", "-", "900µs", "-", "-"},
			{"2", "1000", "all", "p99", "-", "1.2ms", "-", "-"},
			{"2", "1000", "all", "ops/s", "-", "0.2", "-", "-"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.c.Stages) != 2 || len(tc.c.Stages[1].Operations) == 0 {
				t.Fatalf("stages = %+v, want 2 with entries", tc.c.Stages)
			}
			all := tc.c.Stages[1].Operations[0]
			empty, drawn := all.Other, all.Base
			if tc.emptySide == "base" {
				empty, drawn = all.Base, all.Other
			}
			if all.Operation != "all" || empty != nil || drawn == nil || all.Delta != nil {
				t.Errorf("stage 2 all = %+v, want nil on the %s side, which drew nothing, and no delta", all, tc.emptySide)
			}

			var buf bytes.Buffer
			if err := WriteComparisonTable(&buf, tc.c); err != nil {
				t.Fatalf("WriteComparisonTable: %v", err)
			}
			lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
			title := slices.Index(lines, "Stages")
			// The stage table's header, stage 1's eight rows, then stage 2's
			// all rows.
			if title < 0 || len(lines) < title+14 {
				t.Fatalf("comparison lacks a full stage table:\n%s", buf.String())
			}
			for i, w := range tc.wantRows {
				if got := strings.Fields(lines[title+10+i]); !reflect.DeepEqual(got, w) {
					t.Errorf("stage 2 all row %d = %q, want %q", i, got, w)
				}
			}

			data, err := json.Marshal(all)
			if err != nil {
				t.Fatalf("encoding stage 2 all: %v", err)
			}
			for _, key := range []string{`"` + tc.emptySide + `"`, `"delta"`} {
				if strings.Contains(string(data), key) {
					t.Errorf("stage 2 all JSON = %s, want no %s key", data, key)
				}
			}
		})
	}
}

// TestWriteComparisonJSON confirms the comparison's JSON is indented,
// ends in a newline and decodes back to the comparison, and that the
// mismatches and stages keys are present only when there are any.
func TestWriteComparisonJSON(t *testing.T) {
	plainBase, plainOther := comparisonRecords()
	stagedBase, stagedOther := comparisonRecords()
	stagedOther.Pressure.Workers = 8
	stagedBase.Pressure.Stages = pressureStages(300*time.Microsecond, 0, 1000)
	stagedOther.Pressure.Stages = pressureStages(250*time.Microsecond, 0)
	tests := []struct {
		name string
		c    *Comparison
		keys []string
	}{
		{"same load without stages", mustCompare(t, plainBase, plainOther, false), []string{"base", "operations", "other"}},
		{"mismatch with stages", mustCompare(t, stagedBase, stagedOther, true), []string{"base", "mismatches", "operations", "other", "stages"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteComparisonJSON(&buf, tc.c); err != nil {
				t.Fatalf("WriteComparisonJSON: %v", err)
			}
			out := buf.String()
			if !strings.HasPrefix(out, "{\n  \"base\": {\n    \"runID\": \"press001\",") || !strings.HasSuffix(out, "}\n") {
				t.Errorf("comparison JSON is not indented by two spaces and newline-terminated:\n%s", out)
			}
			var got Comparison
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("comparison JSON does not decode: %v", err)
			}
			if !reflect.DeepEqual(&got, tc.c) {
				t.Errorf("decoded comparison = %+v, want %+v", got, *tc.c)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
				t.Fatalf("comparison JSON does not decode as an object: %v", err)
			}
			keys := make([]string, 0, len(raw))
			for k := range raw {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(keys, tc.keys) {
				t.Errorf("comparison JSON keys = %v, want %v", keys, tc.keys)
			}
		})
	}
}

// goldenComparison is the comparison the HTML tests render: two staged
// records of the same scenario and seed whose worker counts differ, compared
// with allowMismatch.
func goldenComparison(t *testing.T) *Comparison {
	t.Helper()
	base, other := comparisonRecords()
	other.Pressure.Workers = 8
	base.Pressure.Stages = pressureStages(300*time.Microsecond, 0, 1000)
	other.Pressure.Stages = pressureStages(250*time.Microsecond, 0, 1000)
	return mustCompare(t, base, other, true)
}

// TestWriteComparisonHTMLGolden locks the rendered comparison page of a
// staged comparison with a mismatch: the heading, a chips line per run, the
// warning naming the mismatch, and the sortable operation and stage tables.
func TestWriteComparisonHTMLGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteComparisonHTML(&buf, goldenComparison(t)); err != nil {
		t.Fatalf("WriteComparisonHTML: %v", err)
	}
	checkGolden(t, "compare-pressure.html", buf.Bytes())

	out := buf.String()
	for _, want := range []string{
		"<title>Comparison: run press001 vs run press002</title>",
		"<h1>Comparison: run press001 vs run press002</h1>",
		`<span class="chip">base run press001</span>`,
		`<span class="chip">other run press002</span>`,
		`<div class="banner warn">Loads differ: workers 4 vs 8</div>`,
		"<h2>Operations</h2>\n<table class=\"sortable\">\n  <thead><tr><th>operation</th><th>metric</th><th>base</th><th>other</th><th>delta</th><th>change</th></tr></thead>",
		"<h2>Stages</h2>\n<table class=\"sortable\">\n  <thead><tr><th>stage</th><th>fan</th><th>operation</th><th>metric</th><th>base</th><th>other</th><th>delta</th><th>change</th></tr></thead>",
		`<tr><td data-sort="validate">validate</td><td data-sort="p50">p50</td><td data-sort="350000">350µs</td><td data-sort="280000">280µs</td><td data-sort="-70000">-70µs</td><td data-sort="-20.00">-20.0%</td></tr>`,
		`<tr><td data-sort="revoke">revoke</td><td data-sort="p50">p50</td><td data-sort="3000000">3ms</td><td data-sort="">-</td><td data-sort="">-</td><td data-sort="">-</td></tr>`,
		`<tr><td data-sort="2">2</td><td data-sort="1000">1000</td><td data-sort="all">all</td>`,
		`<tr><td data-sort="1">1</td><td data-sort="-1">-</td><td data-sort="all">all</td>`,
		"<script>\n\"use strict\";\ndocument.querySelectorAll(\"table.sortable\")",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("comparison page lacks %q", want)
		}
	}
}

// TestWriteComparisonHTMLOffline confirms the comparison page references no
// external resources, and that a comparison without mismatches or stages
// has no warning and no stage table.
func TestWriteComparisonHTMLOffline(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteComparisonHTML(&buf, goldenComparison(t)); err != nil {
		t.Fatalf("WriteComparisonHTML: %v", err)
	}
	for _, bad := range []string{"http://", "https://", "<link ", "src=", "url(http", "@import"} {
		if strings.Contains(buf.String(), bad) {
			t.Errorf("comparison page contains external reference %q, expected a self-contained file", bad)
		}
	}

	buf.Reset()
	if err := WriteComparisonHTML(&buf, mustCompare(t, pressureRecord(), pressureRecord(), false)); err != nil {
		t.Fatalf("WriteComparisonHTML(same load): %v", err)
	}
	for _, unwanted := range []string{"banner warn", "Loads differ", "<h2>Stages</h2>"} {
		if strings.Contains(buf.String(), unwanted) {
			t.Errorf("comparison page of the same load without stages has %q", unwanted)
		}
	}
}

// TestWriteComparisonHTMLEscapes confirms a run id carrying a script payload
// is HTML-escaped in the title, the heading and the chips.
func TestWriteComparisonHTMLEscapes(t *testing.T) {
	base, other := comparisonRecords()
	base.RunID = "<script>alert(1)</script>"
	var buf bytes.Buffer
	if err := WriteComparisonHTML(&buf, mustCompare(t, base, other, false)); err != nil {
		t.Fatalf("WriteComparisonHTML: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Error("run id rendered without escaping (XSS)")
	}
	const escaped = "&lt;script&gt;alert(1)&lt;/script&gt;"
	for _, want := range []string{
		"<title>Comparison: run " + escaped + " vs run press002</title>",
		"<h1>Comparison: run " + escaped + " vs run press002</h1>",
		`<span class="chip">base run ` + escaped + `</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("comparison page lacks the escaped run id in %q", want)
		}
	}
}
