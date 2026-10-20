package run

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// chaosRecord builds a record carrying churn statistics, including a bucket with
// a failed operation, so the chaos report paths have realistic data.
func chaosRecord() *Record {
	r := sampleRecord()
	r.Chaos = &ChaosStats{
		Creates: 40, Deletes: 33, Cycles: 33,
		PopMin: 1, PopMax: 12, PopMean: 7.5, TargetFill: 0.8,
		Buckets: []ChaosBucket{
			{
				Start: 0,
				Stats: metrics.Stats{Attempted: 5, Succeeded: 5, Latency: metrics.Latency{Median: 100 * time.Millisecond, P99: 200 * time.Millisecond}},
			},
			{
				Start:  30 * time.Second,
				Stats:  metrics.Stats{Attempted: 4, Succeeded: 3, Failed: 1, Latency: metrics.Latency{Median: 150 * time.Millisecond, P99: 900 * time.Millisecond}},
				Errors: []metrics.ErrorCount{{Kind: "timeout", Count: 1}},
			},
		},
	}
	return r
}

// TestWriteCSVColumns confirms the CSV header and that each metrics group
// (overall plus one per type) yields exactly one row with the right cell values.
func TestWriteCSVColumns(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, sampleRecord()); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}

	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("parsing CSV: %v", err)
	}
	if !reflect.DeepEqual(rows[0], csvHeader) {
		t.Errorf("header = %v, want %v", rows[0], csvHeader)
	}
	// 1 overall row + 2 per-type rows (network, subnet).
	if len(rows) != 1+1+2 {
		t.Fatalf("got %d lines, want %d (header + overall + 2 types)", len(rows), 4)
	}
	if rows[1][0] != "overall" || rows[1][1] != "3" {
		t.Errorf("overall row = %v, want type overall with attempted 3", rows[1])
	}
	if rows[2][0] != "network" || rows[2][1] != "1" {
		t.Errorf("network row = %v, want type network with attempted 1", rows[2])
	}
}

// TestWriteCSVNeutralizesFormulaLabel confirms a type label that begins with a
// spreadsheet formula trigger is prefixed with an apostrophe, so opening the CSV
// in Excel/LibreOffice renders it as text instead of evaluating the formula.
func TestWriteCSVNeutralizesFormulaLabel(t *testing.T) {
	rec := &Record{
		Metrics: metrics.Aggregate{
			Overall: metrics.Stats{Attempted: 1},
			ByType:  []metrics.Stats{{Type: `=HYPERLINK("http://evil/")`, Attempted: 1}},
		},
	}

	var buf bytes.Buffer
	if err := WriteCSV(&buf, rec); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("parsing CSV: %v", err)
	}
	// header + overall + the one per-type row.
	if got, want := rows[2][0], `'=HYPERLINK("http://evil/")`; got != want {
		t.Errorf("formula label = %q, want %q (apostrophe-prefixed)", got, want)
	}
}

// TestWriteJSONIsMetrics confirms the JSON report decodes back into the run's
// metrics aggregate with its counts intact.
func TestWriteJSONIsMetrics(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, sampleRecord()); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var got metrics.Aggregate
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("report JSON is not a metrics aggregate: %v", err)
	}
	if got.Overall.Attempted != 3 {
		t.Errorf("overall attempted = %d, want 3", got.Overall.Attempted)
	}
	if len(got.ByType) != 2 {
		t.Errorf("byType len = %d, want 2", len(got.ByType))
	}
}

// TestWriteTableChaosBlock confirms a churn record's table report appends the
// churn summary and the per-bucket latency/error table, including the bucket's
// error breakdown. A record with no mutations (Mutates 0) omits the mutates row,
// keeping a Neutron churn report byte-identical to before.
func TestWriteTableChaosBlock(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTable(&buf, chaosRecord()); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Churn summary", "creates:", "cycles:", "target fill 0.80", "Latency and errors over time", "timeout=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("chaos table report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "mutates:") {
		t.Errorf("chaos report with Mutates 0 unexpectedly shows a mutates row:\n%s", out)
	}
}

// TestWriteTableChaosMutatesRow confirms the mutates row appears only when a
// churn run actually mutated (a Cinder soak with volume extends).
func TestWriteTableChaosMutatesRow(t *testing.T) {
	rec := chaosRecord()
	rec.Chaos.Mutates = 7

	var buf bytes.Buffer
	if err := WriteTable(&buf, rec); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "mutates:    7") {
		t.Errorf("chaos report with Mutates 7 missing the mutates row:\n%s", out)
	}
}

// TestWriteTableApplyUnchanged confirms an apply record (no chaos) renders the
// metrics summary with no churn block appended.
func TestWriteTableApplyUnchanged(t *testing.T) {
	var withChaos, withoutChaos bytes.Buffer
	if err := WriteTable(&withoutChaos, sampleRecord()); err != nil {
		t.Fatalf("WriteTable(apply): %v", err)
	}
	if strings.Contains(withoutChaos.String(), "Churn summary") {
		t.Errorf("apply table report unexpectedly contains a churn block:\n%s", withoutChaos.String())
	}
	// The metrics summary itself must be byte-identical to rendering the
	// aggregate directly, i.e. the chaos addition did not perturb the apply path.
	if err := WriteTable(&withChaos, chaosRecord()); err != nil {
		t.Fatalf("WriteTable(chaos): %v", err)
	}
	if !strings.HasPrefix(withChaos.String(), withoutChaos.String()) {
		t.Error("chaos report does not begin with the unchanged apply metrics summary")
	}
}

// TestWriteJSONChaos confirms a churn record's JSON report nests both the
// metrics aggregate and the chaos statistics, while an apply record stays a bare
// metrics aggregate (covered by TestWriteJSONIsMetrics).
func TestWriteJSONChaos(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, chaosRecord()); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var got struct {
		Metrics metrics.Aggregate `json:"metrics"`
		Chaos   *ChaosStats       `json:"chaos"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("chaos report JSON does not decode: %v", err)
	}
	if got.Chaos == nil {
		t.Fatal("chaos report JSON has no chaos object")
	}
	if got.Chaos.Creates != 40 || got.Chaos.Cycles != 33 {
		t.Errorf("chaos stats = %+v, want creates 40 / cycles 33", got.Chaos)
	}
	if got.Metrics.Overall.Attempted != 3 {
		t.Errorf("metrics overall attempted = %d, want 3", got.Metrics.Overall.Attempted)
	}
}

// incompleteChaosRecord is chaosRecord marked as a mid-run checkpoint, with its
// checkpoint time in a non-UTC zone so the renderers' UTC conversion shows.
func incompleteChaosRecord() *Record {
	r := chaosRecord()
	r.Incomplete = true
	r.FinishedAt = time.Date(2026, 6, 24, 12, 1, 30, 0, time.FixedZone("CEST", 2*60*60))
	return r
}

// renderAll renders r in all four report formats, failing the test on any
// renderer error, and returns the outputs keyed by format name.
func renderAll(t *testing.T, r *Record) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for name, write := range map[string]func(*bytes.Buffer, *Record) error{
		"table": func(b *bytes.Buffer, r *Record) error { return WriteTable(b, r) },
		"json":  func(b *bytes.Buffer, r *Record) error { return WriteJSON(b, r) },
		"csv":   func(b *bytes.Buffer, r *Record) error { return WriteCSV(b, r) },
		"html":  func(b *bytes.Buffer, r *Record) error { return WriteHTML(b, r) },
	} {
		var buf bytes.Buffer
		if err := write(&buf, r); err != nil {
			t.Fatalf("rendering %s: %v", name, err)
		}
		out[name] = buf.String()
	}
	return out
}

// TestWriteTableIncomplete confirms a checkpoint's table report opens with the
// incomplete line carrying the checkpoint time in RFC 3339 UTC, and that the
// rest is exactly the report of the same record when complete.
func TestWriteTableIncomplete(t *testing.T) {
	rec := incompleteChaosRecord()
	var incomplete bytes.Buffer
	if err := WriteTable(&incomplete, rec); err != nil {
		t.Fatalf("WriteTable(incomplete): %v", err)
	}
	rec.Incomplete = false
	var complete bytes.Buffer
	if err := WriteTable(&complete, rec); err != nil {
		t.Fatalf("WriteTable(complete): %v", err)
	}

	first, rest, found := strings.Cut(incomplete.String(), "\n\n")
	if !found {
		t.Fatalf("incomplete table report has no blank line after the marker:\n%s", incomplete.String())
	}
	if want := "Run incomplete: checkpoint written at 2026-06-24T10:01:30Z;"; !strings.HasPrefix(first, want) {
		t.Errorf("first line = %q, want prefix %q", first, want)
	}
	if rest != complete.String() {
		t.Errorf("incomplete report after the marker differs from the complete report:\n%s\nwant:\n%s", rest, complete.String())
	}
	if strings.Contains(complete.String(), "Run incomplete") {
		t.Error("complete record's table report carries the incomplete marker")
	}
}

// TestWriteJSONIncomplete confirms a checkpoint's JSON report adds a top-level
// "incomplete": true next to metrics and chaos, and a complete record's report
// has no such key.
func TestWriteJSONIncomplete(t *testing.T) {
	keys := func(r *Record) map[string]json.RawMessage {
		t.Helper()
		var buf bytes.Buffer
		if err := WriteJSON(&buf, r); err != nil {
			t.Fatalf("WriteJSON: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatalf("decoding JSON report: %v", err)
		}
		return m
	}

	rec := incompleteChaosRecord()
	got := keys(rec)
	if len(got) != 3 || got["metrics"] == nil || got["chaos"] == nil || string(got["incomplete"]) != "true" {
		t.Errorf("incomplete report keys = %v, want exactly metrics, chaos and incomplete: true", got)
	}

	rec.Incomplete = false
	if _, ok := keys(rec)["incomplete"]; ok {
		t.Error("complete record's JSON report carries an incomplete key")
	}
}

// TestWriteCSVIncompleteUnchanged confirms the CSV report, which has no place
// for a marker, is byte-identical for a checkpoint and a complete record.
func TestWriteCSVIncompleteUnchanged(t *testing.T) {
	rec := incompleteChaosRecord()
	var incomplete, complete bytes.Buffer
	if err := WriteCSV(&incomplete, rec); err != nil {
		t.Fatalf("WriteCSV(incomplete): %v", err)
	}
	rec.Incomplete = false
	if err := WriteCSV(&complete, rec); err != nil {
		t.Fatalf("WriteCSV(complete): %v", err)
	}
	if incomplete.String() != complete.String() {
		t.Errorf("CSV differs for an incomplete record:\n%s\nwant:\n%s", incomplete.String(), complete.String())
	}
}

// TestReportIncompleteWithoutChaos confirms an incomplete record without chaos
// statistics renders in every format: the table and HTML carry the marker,
// while the JSON stays the bare metrics object with no incomplete key.
func TestReportIncompleteWithoutChaos(t *testing.T) {
	rec := sampleRecord()
	rec.Error = ""
	rec.Incomplete = true
	out := renderAll(t, rec)

	if !strings.HasPrefix(out["table"], "Run incomplete: ") {
		t.Errorf("table report lacks the incomplete marker:\n%s", out["table"])
	}
	if !strings.Contains(out["html"], "banner warn") {
		t.Error("HTML report lacks the incomplete banner")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out["json"]), &m); err != nil {
		t.Fatalf("decoding JSON report: %v", err)
	}
	if m["overall"] == nil || m["metrics"] != nil || m["incomplete"] != nil {
		t.Errorf("JSON report keys = %v, want the bare metrics object", m)
	}
}

// TestReportIncompleteWithoutBuckets confirms a checkpoint taken before the
// first bucket renders in every format with no time-series section.
func TestReportIncompleteWithoutBuckets(t *testing.T) {
	rec := incompleteChaosRecord()
	rec.Chaos.Buckets = nil
	out := renderAll(t, rec)

	if strings.Contains(out["table"], "Latency and errors over time") {
		t.Errorf("table report has a time-series section without buckets:\n%s", out["table"])
	}
	if strings.Contains(out["html"], "Throughput over time") {
		t.Error("HTML report has a time-series section without buckets")
	}
}

// TestReportManyBuckets confirms a long unbounded run's series (three months
// of hourly buckets) renders in every format, one table row per bucket.
func TestReportManyBuckets(t *testing.T) {
	const n = 2160
	rec := chaosRecord()
	rec.Chaos.BucketWidth = time.Hour
	rec.Chaos.Buckets = make([]ChaosBucket, n)
	for i := range rec.Chaos.Buckets {
		rec.Chaos.Buckets[i] = ChaosBucket{
			Start: time.Duration(i) * time.Hour,
			Stats: metrics.Stats{Attempted: i % 7, Succeeded: i % 7, Latency: metrics.Latency{Median: time.Duration(i) * time.Millisecond}},
		}
	}
	out := renderAll(t, rec)

	_, series, found := strings.Cut(out["table"], "\nSTART ")
	if !found {
		t.Fatalf("table report has no time-series header:\n%s", out["table"])
	}
	// The header line ends with the first newline; every later line is a bucket.
	if got := strings.Count(series, "\n") - 1; got != n {
		t.Errorf("table report has %d bucket rows, want %d", got, n)
	}
}

// mixRecord builds a mix record with two personas: ci, with a project, churn
// buckets and a server type, and legacy, with no project, no operations and
// no churn statistics yet.
func mixRecord() *Record {
	r := sampleRecord()
	r.Service = "mix"
	r.Error = ""
	ci := chaosRecord().Chaos
	r.Personas = []PersonaStats{
		{
			Name: "ci", RunID: "abcd1234-ci", Cloud: "tenant-ci", ProjectID: "proj-ci",
			Share: 0.6, Servers: 6, Seed: 7,
			Metrics: metrics.Aggregate{
				Wall: 90 * time.Second,
				Overall: metrics.Stats{Attempted: 3, Succeeded: 2, Failed: 1, Throughput: 0.02,
					Latency: metrics.Latency{Median: 120 * time.Millisecond, P95: 800 * time.Millisecond, P99: 900 * time.Millisecond}},
				ByType: []metrics.Stats{{Type: "server", Attempted: 3, Succeeded: 2, Failed: 1}},
			},
			Chaos: ci,
		},
		{Name: "legacy", RunID: "abcd1234-legacy", Share: 0.4, Servers: 4, Seed: 9},
	}
	return r
}

// mixLanesRecord is mixRecord with two background lanes: keystone, with a
// project, churn buckets and a user type, and glance, with no project, no
// operations and no churn statistics yet.
func mixLanesRecord() *Record {
	r := mixRecord()
	r.Lanes = []LaneStats{
		{
			Name: "keystone", RunID: "abcd1234-keystone", Cloud: "admin", ProjectID: "proj-admin",
			Scenario: "small/keystone", Seed: 11,
			Metrics: metrics.Aggregate{
				Wall: 90 * time.Second,
				Overall: metrics.Stats{Attempted: 4, Succeeded: 4, Throughput: 0.04,
					Latency: metrics.Latency{Median: 50 * time.Millisecond, P95: 200 * time.Millisecond, P99: 300 * time.Millisecond}},
				ByType: []metrics.Stats{{Type: "user", Attempted: 4, Succeeded: 4}},
			},
			Chaos: chaosRecord().Chaos,
		},
		{Name: "glance", RunID: "abcd1234-glance", Scenario: "small/glance", Seed: 12},
	}
	return r
}

// failingWriter fails every write with errWrite.
type failingWriter struct{}

var errWrite = errors.New("disk full")

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

func TestWritePersonaTableEmpty(t *testing.T) {
	for name, ps := range map[string][]PersonaStats{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WritePersonaTable(&buf, ps); err != nil {
				t.Fatalf("WritePersonaTable: %v", err)
			}
			if buf.Len() != 0 {
				t.Errorf("WritePersonaTable wrote %q, want nothing", buf.String())
			}
		})
	}
}

// TestWritePersonaTableRows confirms the table opens with a blank line and its
// title, and that each row carries the persona's project ("-" when unknown),
// whole-percent share, servers, counts and latencies ("-" without operations).
func TestWritePersonaTableRows(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePersonaTable(&buf, mixRecord().Personas); err != nil {
		t.Fatalf("WritePersonaTable: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 5 || lines[0] != "" || lines[1] != "Personas" {
		t.Fatalf("table = %q, want a blank line, the title, a header and two rows", buf.String())
	}
	want := [][]string{
		{"NAME", "PROJECT", "SHARE", "SERVERS", "OPS", "OK", "FAILED", "P50", "P95", "P99"},
		{"ci", "proj-ci", "60%", "6", "3", "2", "1", "120ms", "800ms", "900ms"},
		{"legacy", "-", "40%", "4", "0", "0", "0", "-", "-", "-"},
	}
	for i, w := range want {
		if got := strings.Fields(lines[i+2]); !reflect.DeepEqual(got, w) {
			t.Errorf("row %d = %q, want %q", i, got, w)
		}
	}
}

func TestWritePersonaTableWriteError(t *testing.T) {
	err := WritePersonaTable(failingWriter{}, mixRecord().Personas)
	if !errors.Is(err, errWrite) {
		t.Fatalf("WritePersonaTable error = %v, want it to wrap the writer's error", err)
	}
	if !strings.HasPrefix(err.Error(), "writing persona table: ") {
		t.Errorf("error %q does not start with %q", err, "writing persona table: ")
	}
}

func TestWriteLaneTableEmpty(t *testing.T) {
	for name, ls := range map[string][]LaneStats{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteLaneTable(&buf, ls); err != nil {
				t.Fatalf("WriteLaneTable: %v", err)
			}
			if buf.Len() != 0 {
				t.Errorf("WriteLaneTable wrote %q, want nothing", buf.String())
			}
		})
	}
}

// TestWriteLaneTableRows confirms the table opens with a blank line and its
// title, that each row carries the lane's project ("-" when unknown),
// scenario, counts and latencies ("-" without operations), and that every row
// is aligned to the same width.
func TestWriteLaneTableRows(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteLaneTable(&buf, mixLanesRecord().Lanes); err != nil {
		t.Fatalf("WriteLaneTable: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 5 || lines[0] != "" || lines[1] != "Lanes" {
		t.Fatalf("table = %q, want a blank line, the title, a header and two rows", buf.String())
	}
	want := [][]string{
		{"NAME", "PROJECT", "SCENARIO", "OPS", "OK", "FAILED", "P50", "P95", "P99"},
		{"keystone", "proj-admin", "small/keystone", "4", "4", "0", "50ms", "200ms", "300ms"},
		{"glance", "-", "small/glance", "0", "0", "0", "-", "-", "-"},
	}
	for i, w := range want {
		if got := strings.Fields(lines[i+2]); !reflect.DeepEqual(got, w) {
			t.Errorf("row %d = %q, want %q", i, got, w)
		}
		if len(lines[i+2]) != len(lines[2]) {
			t.Errorf("row %d is %d wide, want the header's %d:\n%s", i, len(lines[i+2]), len(lines[2]), buf.String())
		}
	}
	if !strings.HasPrefix(lines[4], "glance    -         ") {
		t.Errorf("glance row %q does not left-align its name and project", lines[4])
	}
}

func TestWriteLaneTableWriteError(t *testing.T) {
	err := WriteLaneTable(failingWriter{}, mixLanesRecord().Lanes)
	if !errors.Is(err, errWrite) {
		t.Fatalf("WriteLaneTable error = %v, want it to wrap the writer's error", err)
	}
	if !strings.HasPrefix(err.Error(), "writing lane table: ") {
		t.Errorf("error %q does not start with %q", err, "writing lane table: ")
	}
}

// TestWriteTableMixLanes confirms a mix record with lanes gets the lane table
// and one section per lane after the persona sections, with the churn summary
// only for the lane that has churn statistics.
func TestWriteTableMixLanes(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTable(&buf, mixLanesRecord()); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	out := buf.String()
	last := -1
	for _, want := range []string{"\nPersonas\n", "\nPersona ci\nRun metrics", "\nPersona legacy\nRun metrics",
		"\nLanes\n", "\nLane keystone\nRun metrics", "\nLane glance\nRun metrics"} {
		i := strings.Index(out, want)
		if i <= last {
			t.Errorf("mix table report has %q at %d, want it after offset %d:\n%s", want, i, last, out)
		}
		last = i
	}
	if got := strings.Count(out, "Churn summary"); got != 2 {
		t.Errorf("mix table report has %d churn summaries, want 2 (ci and keystone):\n%s", got, out)
	}
}

// TestWriteTableMix confirms a mix record's table report carries the persona
// table and one section per persona, with the churn summary only for the
// persona that has churn statistics.
func TestWriteTableMix(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTable(&buf, mixRecord()); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"\nPersonas\n", "\nPersona ci\nRun metrics", "\nPersona legacy\nRun metrics", "Churn summary", "target fill 0.80"} {
		if !strings.Contains(out, want) {
			t.Errorf("mix table report missing %q:\n%s", want, out)
		}
	}
	if got := strings.Count(out, "Churn summary"); got != 1 {
		t.Errorf("mix table report has %d churn summaries, want 1 (only ci has churn statistics):\n%s", got, out)
	}
	if !strings.HasPrefix(out, sampleRecord().Metrics.Summary()) {
		t.Errorf("mix table report does not start with the overall metrics:\n%s", out)
	}
}

// TestWriteJSONMix confirms a mix record's JSON report keeps the overall
// metrics at .metrics.overall, carries the personas, omits services when the
// record has none, and marks a checkpoint incomplete.
func TestWriteJSONMix(t *testing.T) {
	decode := func(t *testing.T, r *Record) map[string]json.RawMessage {
		t.Helper()
		var buf bytes.Buffer
		if err := WriteJSON(&buf, r); err != nil {
			t.Fatalf("WriteJSON: %v", err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("report JSON does not decode: %v", err)
		}
		return got
	}

	t.Run("complete without services", func(t *testing.T) {
		got := decode(t, mixRecord())
		var m struct {
			Overall metrics.Stats `json:"overall"`
		}
		if err := json.Unmarshal(got["metrics"], &m); err != nil || m.Overall.Attempted != 3 {
			t.Errorf(".metrics.overall = %+v (err %v), want attempted 3", m.Overall, err)
		}
		var ps []PersonaStats
		if err := json.Unmarshal(got["personas"], &ps); err != nil || len(ps) != 2 || ps[0].Name != "ci" {
			t.Errorf(".personas = %+v (err %v), want ci first of two", ps, err)
		}
		for _, key := range []string{"services", "incomplete", "chaos"} {
			if _, ok := got[key]; ok {
				t.Errorf("report JSON carries %q although the record has none", key)
			}
		}
	})

	t.Run("services and checkpoint", func(t *testing.T) {
		r := mixRecord()
		r.Services = []string{"octavia"}
		r.Incomplete = true
		got := decode(t, r)
		if string(got["services"]) == "" || !strings.Contains(string(got["services"]), "octavia") {
			t.Errorf(".services = %s, want [\"octavia\"]", got["services"])
		}
		if string(got["incomplete"]) != "true" {
			t.Errorf(".incomplete = %s, want true", got["incomplete"])
		}
	})
}

// TestWriteJSONMixLanes confirms a mix record's JSON report carries the lanes
// after the personas, and omits the key for a record without lanes.
func TestWriteJSONMixLanes(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, mixLanesRecord()); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	out := buf.String()
	if p, l := strings.Index(out, `"personas"`), strings.Index(out, `"lanes"`); p < 0 || l < p {
		t.Errorf("report JSON has personas at %d and lanes at %d, want lanes after personas:\n%s", p, l, out)
	}
	var got struct {
		Lanes []LaneStats `json:"lanes"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || len(got.Lanes) != 2 || got.Lanes[0].Name != "keystone" || got.Lanes[0].RunID != "abcd1234-keystone" {
		t.Errorf(".lanes = %+v (err %v), want keystone first of two", got.Lanes, err)
	}

	buf.Reset()
	if err := WriteJSON(&buf, mixRecord()); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if strings.Contains(buf.String(), `"lanes"`) {
		t.Errorf("report JSON of a record without lanes has a lanes key:\n%s", buf.String())
	}
}

// TestWriteCSVMixLanes confirms a mix record's CSV appends the per-lane rows
// after the per-persona ones.
func TestWriteCSVMixLanes(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, mixLanesRecord()); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("parsing csv: %v", err)
	}
	var labels []string
	for _, rec := range records[1:] {
		labels = append(labels, rec[0])
	}
	want := []string{"overall", "network", "subnet", "ci/overall", "ci/server", "legacy/overall", "keystone/overall", "keystone/user", "glance/overall"}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("row labels = %v, want %v", labels, want)
	}
}

// TestWriteCSVMix confirms a mix record's CSV keeps the header and appends
// the per-persona rows after the overall ones.
func TestWriteCSVMix(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, mixRecord()); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("parsing csv: %v", err)
	}
	if !reflect.DeepEqual(records[0], csvHeader) {
		t.Errorf("header = %v, want %v", records[0], csvHeader)
	}
	var labels []string
	for _, rec := range records[1:] {
		labels = append(labels, rec[0])
	}
	want := []string{"overall", "network", "subnet", "ci/overall", "ci/server", "legacy/overall"}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("row labels = %v, want %v", labels, want)
	}
}

// TestReportEmptyPersonasRendersAsNone confirms an empty, non-nil persona list
// renders exactly like no personas at all in every format.
func TestReportEmptyPersonasRendersAsNone(t *testing.T) {
	for name, build := range map[string]func() *Record{"apply": sampleRecord, "chaos": chaosRecord} {
		t.Run(name, func(t *testing.T) {
			empty := build()
			empty.Personas = []PersonaStats{}
			want, got := renderAll(t, build()), renderAll(t, empty)
			for format := range want {
				if got[format] != want[format] {
					t.Errorf("%s report with an empty persona list differs from one without personas", format)
				}
			}
		})
	}
}

// pressureRecord builds the record of an unbounded keystone pressure run with
// two operations, validate and revoke, over two one-minute buckets, with a
// failed operation and an error kind in each bucket and the revocations
// cumulative across the buckets (1, then 3), so the pressure report paths
// have realistic data. Its latencies sit below a millisecond for validate, as
// a token validation does.
func pressureRecord() *Record {
	return &Record{
		RunID:      "press001",
		Service:    "keystone",
		Scenario:   "small",
		Seed:       42,
		StartedAt:  time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 6, 24, 10, 2, 0, 0, time.UTC),
		Created: []resource.Resource{
			{Kind: "project", Logical: "project-0001", Name: "dizzy-press001-project-0001", ID: "proj-id-1"},
			{Kind: "user", Logical: "user-0001", Name: "dizzy-press001-user-0001", ID: "user-id-1"},
		},
		Metrics: metrics.Aggregate{
			Wall:    2 * time.Minute,
			Overall: metrics.Stats{Attempted: 46, Succeeded: 44, Failed: 2, Throughput: 0.38},
			ByType: []metrics.Stats{
				{Type: "project", Attempted: 1, Succeeded: 1, Latency: metrics.Latency{Min: 40 * time.Millisecond, Median: 40 * time.Millisecond, Max: 40 * time.Millisecond}},
				{Type: "token", Attempted: 45, Succeeded: 43, Failed: 2, Latency: metrics.Latency{Min: 180 * time.Microsecond, Median: 400 * time.Microsecond, Max: 180 * time.Millisecond}},
			},
			Errors: []metrics.ErrorCount{{Kind: "http_404", Count: 1}, {Kind: "timeout", Count: 1}},
		},
		Pressure: &PressureStats{
			Workers:    4,
			Tokens:     3,
			ChurnUsers: 1,
			Mix:        []PressureWeight{{Operation: "revoke", Weight: 1}, {Operation: "validate", Weight: 9}},
			Operations: metrics.Aggregate{
				Wall:    90 * time.Second,
				Overall: metrics.Stats{Attempted: 40, Succeeded: 38, Failed: 2, Throughput: 0.44},
				ByType: []metrics.Stats{
					{Type: "revoke", Attempted: 4, Succeeded: 3, Failed: 1, Throughput: 0.04, Latency: metrics.Latency{
						Min: 2 * time.Millisecond, Mean: 3500 * time.Microsecond, Median: 3 * time.Millisecond,
						P90: 5 * time.Millisecond, P95: 5 * time.Millisecond, P99: 5 * time.Millisecond, Max: 5 * time.Millisecond}},
					{Type: "validate", Attempted: 36, Succeeded: 35, Failed: 1, Throughput: 0.4, Latency: metrics.Latency{
						Min: 180 * time.Microsecond, Mean: 420 * time.Microsecond, Median: 350 * time.Microsecond,
						P90: 700 * time.Microsecond, P95: 900 * time.Microsecond, P99: 1200 * time.Microsecond, Max: 1500 * time.Microsecond}},
				},
				Errors: []metrics.ErrorCount{{Kind: "http_404", Count: 1}, {Kind: "timeout", Count: 1}},
			},
			Revocations: 3,
			Buckets: []PressureBucket{
				{
					Start: 0,
					Operations: metrics.Aggregate{
						Wall:    time.Minute,
						Overall: metrics.Stats{Attempted: 22, Succeeded: 21, Failed: 1, Latency: metrics.Latency{Median: 360 * time.Microsecond, P99: 4 * time.Millisecond}},
						ByType: []metrics.Stats{
							{Type: "revoke", Attempted: 2, Succeeded: 1, Failed: 1, Latency: metrics.Latency{Median: 3 * time.Millisecond, P99: 4 * time.Millisecond}},
							{Type: "validate", Attempted: 20, Succeeded: 20, Latency: metrics.Latency{Median: 340 * time.Microsecond, P99: 1100 * time.Microsecond}},
						},
						Errors: []metrics.ErrorCount{{Kind: "http_404", Count: 1}},
					},
					Revocations: 1,
				},
				{
					Start: time.Minute,
					Operations: metrics.Aggregate{
						Wall:    time.Minute,
						Overall: metrics.Stats{Attempted: 18, Succeeded: 17, Failed: 1, Latency: metrics.Latency{Median: 380 * time.Microsecond, P99: 5 * time.Millisecond}},
						ByType: []metrics.Stats{
							{Type: "revoke", Attempted: 2, Succeeded: 2, Latency: metrics.Latency{Median: 4 * time.Millisecond, P99: 5 * time.Millisecond}},
							{Type: "validate", Attempted: 16, Succeeded: 15, Failed: 1, Latency: metrics.Latency{Median: 370 * time.Microsecond, P99: 1200 * time.Microsecond}},
						},
						Errors: []metrics.ErrorCount{{Kind: "timeout", Count: 1}},
					},
					Revocations: 3,
				},
			},
			BucketWidth: time.Minute,
		},
	}
}

// TestWritePressureTable confirms the pressure block lists the settings and
// the revocations, that the operation table carries one row per operation
// with its latencies rounded to the microsecond, and that the bucket table
// gives each bucket an "all" row with its cumulative revocations and error
// breakdown followed by one row per operation with those two cells empty.
func TestWritePressureTable(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePressureTable(&buf, pressureRecord().Pressure); err != nil {
		t.Fatalf("WritePressureTable: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	wantHead := []string{
		"",
		"Pressure",
		"  workers:     4",
		"  token pool:  3",
		"  churn users: 1",
		"  mix:         revoke=1, validate=9",
		"  revocations: 3",
		"",
		"Operations",
	}
	if len(lines) < len(wantHead) || !reflect.DeepEqual(lines[:len(wantHead)], wantHead) {
		t.Fatalf("pressure block = %q, want it to start with %q", lines, wantHead)
	}
	rest := lines[len(wantHead):]
	wantOps := [][]string{
		{"OPERATION", "OPS", "OK", "FAILED", "OPS/S", "MIN", "MEAN", "P50", "P90", "P95", "P99", "MAX"},
		{"revoke", "4", "3", "1", "0.0", "2ms", "3.5ms", "3ms", "5ms", "5ms", "5ms", "5ms"},
		{"validate", "36", "35", "1", "0.4", "180µs", "420µs", "350µs", "700µs", "900µs", "1.2ms", "1.5ms"},
	}
	wantBuckets := [][]string{
		{"START", "OPERATION", "OPS", "OK", "FAILED", "P50", "P99", "REVOCATIONS", "ERRORS"},
		{"0s", "all", "22", "21", "1", "360µs", "4ms", "1", "http_404=1"},
		{"0s", "revoke", "2", "1", "1", "3ms", "4ms"},
		{"0s", "validate", "20", "20", "0", "340µs", "1.1ms"},
		{"1m0s", "all", "18", "17", "1", "380µs", "5ms", "3", "timeout=1"},
		{"1m0s", "revoke", "2", "2", "0", "4ms", "5ms"},
		{"1m0s", "validate", "16", "15", "1", "370µs", "1.2ms"},
	}
	if want := len(wantOps) + 2 + len(wantBuckets); len(rest) != want {
		t.Fatalf("pressure tables have %d lines, want %d:\n%s", len(rest), want, buf.String())
	}
	for i, w := range wantOps {
		if got := strings.Fields(rest[i]); !reflect.DeepEqual(got, w) {
			t.Errorf("operation row %d = %q, want %q", i, got, w)
		}
	}
	if rest[len(wantOps)] != "" || rest[len(wantOps)+1] != "Latency and errors over time" {
		t.Errorf("bucket table title = %q, want a blank line and %q", rest[len(wantOps):len(wantOps)+2], "Latency and errors over time")
	}
	buckets := rest[len(wantOps)+2:]
	for i, w := range wantBuckets {
		if got := strings.Fields(buckets[i]); !reflect.DeepEqual(got, w) {
			t.Errorf("bucket row %d = %q, want %q", i, got, w)
		}
		if strings.HasSuffix(buckets[i], " ") {
			t.Errorf("bucket row %d %q ends in a space", i, buckets[i])
		}
	}
	// A microsecond cell holds a two-byte rune; the columns still line up.
	if a, b := strings.Index(rest[1], "3.5ms"), strings.Index(rest[2], "420µs"); a < 0 || b < 0 ||
		len([]rune(rest[1][:a])) != len([]rune(rest[2][:b])) {
		t.Errorf("MEAN column is not aligned across %q and %q", rest[1], rest[2])
	}
}

// TestWritePressureTableEdges confirms the pressure block omits the churn
// users line when the run reserved none, renders an empty mix as "-", shows
// "-" for the latencies of an operation that attempted nothing, writes no
// bucket table without buckets, and writes nothing at all for nil.
func TestWritePressureTableEdges(t *testing.T) {
	var none bytes.Buffer
	if err := WritePressureTable(&none, nil); err != nil {
		t.Fatalf("WritePressureTable(nil): %v", err)
	}
	if none.Len() != 0 {
		t.Errorf("WritePressureTable(nil) wrote %q, want nothing", none.String())
	}

	p := &PressureStats{
		Workers: 16, Tokens: 100,
		Operations: metrics.Aggregate{ByType: []metrics.Stats{{Type: "issue"}}},
	}
	var buf bytes.Buffer
	if err := WritePressureTable(&buf, p); err != nil {
		t.Fatalf("WritePressureTable: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "churn users:") {
		t.Errorf("pressure block shows churn users although the run reserved none:\n%s", out)
	}
	if !strings.Contains(out, "\n  mix:         -\n") {
		t.Errorf("pressure block does not render an empty mix as \"-\":\n%s", out)
	}
	if strings.Contains(out, "Latency and errors over time") {
		t.Errorf("pressure block has a bucket table without buckets:\n%s", out)
	}
	want := []string{"issue", "0", "0", "0", "0.0", "-", "-", "-", "-", "-", "-", "-"}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if got := strings.Fields(lines[len(lines)-1]); !reflect.DeepEqual(got, want) {
		t.Errorf("operation row without operations = %q, want %q", got, want)
	}
}

func TestWritePressureTableWriteError(t *testing.T) {
	err := WritePressureTable(failingWriter{}, pressureRecord().Pressure)
	if !errors.Is(err, errWrite) {
		t.Fatalf("WritePressureTable error = %v, want it to wrap the writer's error", err)
	}
	if !strings.HasPrefix(err.Error(), "writing pressure table: ") {
		t.Errorf("error %q does not start with %q", err, "writing pressure table: ")
	}
}

// TestWriteTablePressure confirms a pressure record's table report carries
// the metrics summary first and then the pressure block, its token pool, a
// validate row and the second bucket's "all" row with the cumulative
// revocations, and that an incomplete pressure record starts with the
// incomplete line followed by the complete report.
func TestWriteTablePressure(t *testing.T) {
	rec := pressureRecord()
	var buf bytes.Buffer
	if err := WriteTable(&buf, rec); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, rec.Metrics.Summary()+"\nPressure\n") {
		t.Errorf("table report does not start with the metrics summary followed by the pressure block:\n%s", out)
	}
	for _, want := range []string{"token pool:  3", "Latency and errors over time"} {
		if !strings.Contains(out, want) {
			t.Errorf("table report missing %q:\n%s", want, out)
		}
	}
	var validate, all bool
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "validate" {
			validate = true
		}
		if len(f) == 9 && f[0] == "1m0s" && f[1] == "all" && f[7] == "3" {
			all = true
		}
	}
	if !validate {
		t.Errorf("table report has no validate operation row:\n%s", out)
	}
	if !all {
		t.Errorf("table report has no \"all\" row for the 1m0s bucket with 3 cumulative revocations:\n%s", out)
	}
	if strings.Contains(out, "Churn summary") {
		t.Errorf("pressure table report carries a churn summary:\n%s", out)
	}

	rec.Incomplete = true
	var incomplete bytes.Buffer
	if err := WriteTable(&incomplete, rec); err != nil {
		t.Fatalf("WriteTable(incomplete): %v", err)
	}
	first, rest, found := strings.Cut(incomplete.String(), "\n\n")
	if !found || !strings.HasPrefix(first, "Run incomplete: checkpoint written at 2026-06-24T10:02:00Z;") {
		t.Errorf("incomplete pressure report starts with %q, want the incomplete line", first)
	}
	if rest != out {
		t.Errorf("incomplete pressure report after the marker differs from the complete report:\n%s\nwant:\n%s", rest, out)
	}
}

// TestWriteJSONPressure confirms a pressure record's JSON report carries
// exactly the metrics and pressure keys, plus "incomplete": true for a
// checkpoint, and no chaos key, and that the pressure object decodes back to
// the record's statistics.
func TestWriteJSONPressure(t *testing.T) {
	tests := []struct {
		name       string
		incomplete bool
		want       []string
	}{
		{"complete", false, []string{"metrics", "pressure"}},
		{"checkpoint", true, []string{"incomplete", "metrics", "pressure"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := pressureRecord()
			rec.Incomplete = tc.incomplete
			var buf bytes.Buffer
			if err := WriteJSON(&buf, rec); err != nil {
				t.Fatalf("WriteJSON: %v", err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("report JSON does not decode: %v", err)
			}
			keys := make([]string, 0, len(got))
			for k := range got {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(keys, tc.want) {
				t.Errorf("report JSON keys = %v, want %v", keys, tc.want)
			}
			if tc.incomplete && string(got["incomplete"]) != "true" {
				t.Errorf(".incomplete = %s, want true", got["incomplete"])
			}
			var p PressureStats
			if err := json.Unmarshal(got["pressure"], &p); err != nil {
				t.Fatalf("decoding .pressure: %v", err)
			}
			if !reflect.DeepEqual(&p, rec.Pressure) {
				t.Errorf(".pressure = %+v, want %+v", p, rec.Pressure)
			}
		})
	}
}

// TestWriteCSVPressure confirms a pressure record's CSV keeps the header and
// appends, after the per-type rows, one row per operation and then one per
// bucket and operation, labelled with the bucket start in duration notation.
func TestWriteCSVPressure(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, pressureRecord()); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("parsing csv: %v", err)
	}
	if !reflect.DeepEqual(records[0], csvHeader) {
		t.Errorf("header = %v, want %v", records[0], csvHeader)
	}
	var labels []string
	for _, rec := range records[1:] {
		labels = append(labels, rec[0])
	}
	want := []string{
		"overall", "project", "token",
		"pressure/revoke", "pressure/validate",
		"pressure@0s/revoke", "pressure@0s/validate",
		"pressure@1m0s/revoke", "pressure@1m0s/validate",
	}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("row labels = %v, want %v", labels, want)
	}
	// pressure@0s/validate: 20 attempted, 20 succeeded, p50 0.340 ms.
	if row := records[7]; row[1] != "20" || row[2] != "20" || row[7] != "0.340" {
		t.Errorf("pressure@0s/validate row = %v, want attempted 20, succeeded 20, median 0.340", row)
	}
}

// TestReportPressureWithoutBuckets confirms a pressure checkpoint taken
// before the first bucket renders in every format with no time-series
// section.
func TestReportPressureWithoutBuckets(t *testing.T) {
	rec := pressureRecord()
	rec.Incomplete = true
	rec.Pressure.Buckets = nil
	out := renderAll(t, rec)

	if strings.Contains(out["table"], "Latency and errors over time") {
		t.Errorf("table report has a time-series section without buckets:\n%s", out["table"])
	}
	if strings.Contains(out["csv"], "pressure@") {
		t.Errorf("CSV report has bucket rows without buckets:\n%s", out["csv"])
	}
	if !strings.Contains(out["csv"], "pressure/validate") {
		t.Errorf("CSV report lacks the operation rows:\n%s", out["csv"])
	}
}

// pressureStagesRecord builds the record of a bounded, staged keystone
// pressure run over the operations effective and validate, two one-minute
// buckets and two stages. The first stage sets no fan_assignments override
// and drew no effective operation, so its effective row has no latencies.
// The second grew every fan user to 1000 grants and resumed the workers
// 1.5004 s into its window, so its start shows rounded to the millisecond.
// The stages' counts add up to the run's.
func pressureStagesRecord() *Record {
	r := pressureRecord()
	p := r.Pressure
	p.ChurnUsers, p.Revocations, p.BucketWidth = 0, 0, 0
	p.Mix = []PressureWeight{{Operation: "effective", Weight: 20}, {Operation: "validate", Weight: 10}}
	p.Operations.ByType[0].Type = "effective"
	for i := range p.Buckets {
		p.Buckets[i].Operations.ByType[0].Type = "effective"
		p.Buckets[i].Revocations = 0
	}
	secondStart := time.Minute + 1500*time.Millisecond + 400*time.Microsecond
	p.Stages = []PressureStage{
		{
			Index: 1,
			Start: 0,
			End:   time.Minute,
			Operations: metrics.Aggregate{
				Wall:    time.Minute,
				Overall: metrics.Stats{Attempted: 20, Succeeded: 19, Failed: 1, Throughput: 0.33, Latency: metrics.Latency{Median: 340 * time.Microsecond, P95: 800 * time.Microsecond, P99: 1100 * time.Microsecond}},
				ByType: []metrics.Stats{
					{Type: "effective"},
					{Type: "validate", Attempted: 20, Succeeded: 19, Failed: 1, Throughput: 0.33, Latency: metrics.Latency{Median: 340 * time.Microsecond, P95: 800 * time.Microsecond, P99: 1100 * time.Microsecond}},
				},
				Errors: []metrics.ErrorCount{{Kind: "http_404", Count: 1}},
			},
		},
		{
			Index:          2,
			FanAssignments: 1000,
			Start:          secondStart,
			End:            2 * time.Minute,
			Operations: metrics.Aggregate{
				Wall:    2*time.Minute - secondStart,
				Overall: metrics.Stats{Attempted: 20, Succeeded: 19, Failed: 1, Throughput: 0.34, Latency: metrics.Latency{Median: 900 * time.Microsecond, P95: 4 * time.Millisecond, P99: 4500 * time.Microsecond}},
				ByType: []metrics.Stats{
					{Type: "effective", Attempted: 4, Succeeded: 3, Failed: 1, Throughput: 0.07, Latency: metrics.Latency{Median: 4 * time.Millisecond, P95: 4500 * time.Microsecond, P99: 4500 * time.Microsecond}},
					{Type: "validate", Attempted: 16, Succeeded: 16, Throughput: 0.27, Latency: metrics.Latency{Median: 370 * time.Microsecond, P95: time.Millisecond, P99: 1200 * time.Microsecond}},
				},
				Errors: []metrics.ErrorCount{{Kind: "timeout", Count: 1}},
			},
		},
	}
	return r
}

// TestWritePressureTableStages confirms a staged run's pressure block puts
// the stage table between the operation table and the bucket table, with
// per stage an "all" row and then one row per operation, "-" for a stage
// without an override and for the latencies of an operation without draws,
// the window rounded to the millisecond, and START, END and OPERATION
// left-aligned.
func TestWritePressureTableStages(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePressureTable(&buf, pressureStagesRecord().Pressure); err != nil {
		t.Fatalf("WritePressureTable: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	title := slices.Index(lines, "Stages")
	ops := slices.Index(lines, "Operations")
	buckets := slices.Index(lines, "Latency and errors over time")
	if ops < 0 || title < ops || buckets < title {
		t.Fatalf("tables missing or out of order (operations %d, stages %d, buckets %d):\n%s", ops, title, buckets, buf.String())
	}
	if lines[title-1] != "" {
		t.Errorf("stage table title is preceded by %q, want a blank line", lines[title-1])
	}
	want := [][]string{
		{"STAGE", "FAN", "START", "END", "OPERATION", "OPS", "OK", "FAILED", "OPS/S", "P50", "P95", "P99"},
		{"1", "-", "0s", "1m0s", "all", "20", "19", "1", "0.3", "340µs", "800µs", "1.1ms"},
		{"1", "-", "0s", "1m0s", "effective", "0", "0", "0", "0.0", "-", "-", "-"},
		{"1", "-", "0s", "1m0s", "validate", "20", "19", "1", "0.3", "340µs", "800µs", "1.1ms"},
		{"2", "1000", "1m1.5s", "2m0s", "all", "20", "19", "1", "0.3", "900µs", "4ms", "4.5ms"},
		{"2", "1000", "1m1.5s", "2m0s", "effective", "4", "3", "1", "0.1", "4ms", "4.5ms", "4.5ms"},
		{"2", "1000", "1m1.5s", "2m0s", "validate", "16", "16", "0", "0.3", "370µs", "1ms", "1.2ms"},
	}
	rows := lines[title+1 : buckets-1]
	if len(rows) != len(want) {
		t.Fatalf("stage table has %d lines, want %d:\n%s", len(rows), len(want), buf.String())
	}
	for i, w := range want {
		if got := strings.Fields(rows[i]); !reflect.DeepEqual(got, w) {
			t.Errorf("stage row %d = %q, want %q", i, got, w)
		}
	}
	// The cells of a left-aligned column start at the rune offset of its
	// header in every row; STAGE and FAN are right-aligned.
	for _, col := range []string{"START", "END", "OPERATION"} {
		offset := utf8.RuneCountInString(rows[0][:strings.Index(rows[0], col)])
		for i, row := range rows[1:] {
			cell := want[i+1][slices.Index(want[0], col)]
			if r := []rune(row); offset > len(r) || !strings.HasPrefix(string(r[offset:]), cell) {
				t.Errorf("%s cell %q of stage row %d does not start under its header in %q", col, cell, i+1, row)
			}
		}
	}
	for i, prefix := range map[int]string{1: "    1     -  0s", 4: "    2  1000  1m1.5s"} {
		if !strings.HasPrefix(rows[i], prefix) {
			t.Errorf("stage row %d = %q, want it to start with %q (STAGE and FAN right-aligned)", i, rows[i], prefix)
		}
	}
}

// TestWriteCSVPressureStages confirms a staged run's CSV puts one row per
// stage and operation, labelled pressure/stage-<index>/<operation> without an
// "all" row, after the run's operation rows and before the bucket rows.
func TestWriteCSVPressureStages(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, pressureStagesRecord()); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("parsing csv: %v", err)
	}
	var labels []string
	for _, rec := range records[1:] {
		labels = append(labels, rec[0])
	}
	want := []string{
		"overall", "project", "token",
		"pressure/effective", "pressure/validate",
		"pressure/stage-1/effective", "pressure/stage-1/validate",
		"pressure/stage-2/effective", "pressure/stage-2/validate",
		"pressure@0s/effective", "pressure@0s/validate",
		"pressure@1m0s/effective", "pressure@1m0s/validate",
	}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("row labels = %v, want %v", labels, want)
	}
	// pressure/stage-2/validate: 16 attempted, 16 succeeded, p50 0.370 ms.
	if row := records[9]; row[1] != "16" || row[2] != "16" || row[7] != "0.370" {
		t.Errorf("pressure/stage-2/validate row = %v, want attempted 16, succeeded 16, median 0.370", row)
	}
}

// TestWriteJSONPressureStages confirms a staged run's JSON report carries the
// stages inside the pressure object, decoding back to the record's stages,
// with fanAssignments omitted for the stage without an override, and that a
// run without stages carries no stages key.
func TestWriteJSONPressureStages(t *testing.T) {
	rec := pressureStagesRecord()
	var buf bytes.Buffer
	if err := WriteJSON(&buf, rec); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var got struct {
		Pressure struct {
			Stages []json.RawMessage `json:"stages"`
		} `json:"pressure"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("report JSON does not decode: %v", err)
	}
	stages := make([]PressureStage, 0, len(got.Pressure.Stages))
	for i, raw := range got.Pressure.Stages {
		var st PressureStage
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatalf("decoding .pressure.stages[%d]: %v", i, err)
		}
		stages = append(stages, st)
	}
	if !reflect.DeepEqual(stages, rec.Pressure.Stages) {
		t.Errorf(".pressure.stages = %+v, want %+v", stages, rec.Pressure.Stages)
	}
	if len(got.Pressure.Stages) == 2 {
		if bytes.Contains(got.Pressure.Stages[0], []byte(`"fanAssignments"`)) {
			t.Errorf("stage without an override carries fanAssignments: %s", got.Pressure.Stages[0])
		}
		if !bytes.Contains(got.Pressure.Stages[1], []byte(`"fanAssignments": 1000`)) {
			t.Errorf("stage with an override lacks \"fanAssignments\": 1000: %s", got.Pressure.Stages[1])
		}
	}

	var plain bytes.Buffer
	if err := WriteJSON(&plain, pressureRecord()); err != nil {
		t.Fatalf("WriteJSON(without stages): %v", err)
	}
	if strings.Contains(plain.String(), `"stages"`) {
		t.Errorf("JSON report of a run without stages carries a stages key:\n%s", plain.String())
	}
}

// pressureServicesRecord builds the record of a staged keystone pressure run
// that also sent its pool tokens to the other services: pressureStagesRecord
// with probes at glance and nova, bad tokens at nova only, and a propagation
// probe every 30s with a 10m ceiling that completed 12 probes, none timed
// out, at a delay of p50 2.1s, p95 4.8s, p99 6.2s and max 6.2s. Its p95 lies
// 200µs above 4.8s and nova's probe p99 300ns above 3.1ms, so the rounding of
// the reports shows. The operation aggregate stays pressureStagesRecord's:
// the reports render the per-service lists on their own.
func pressureServicesRecord() *Record {
	r := pressureStagesRecord()
	p := r.Pressure
	p.Probes = []metrics.Stats{
		{Type: "glance", Attempted: 60, Succeeded: 60, Throughput: 0.67, Latency: metrics.Latency{
			Min: 2100 * time.Microsecond, Mean: 3400 * time.Microsecond, Median: 3100 * time.Microsecond,
			P90: 4800 * time.Microsecond, P95: 5500 * time.Microsecond, P99: 7200 * time.Microsecond, Max: 9 * time.Millisecond}},
		{Type: "nova", Attempted: 64, Succeeded: 63, Failed: 1, Throughput: 0.71, Latency: metrics.Latency{
			Min: 800 * time.Microsecond, Mean: 1350 * time.Microsecond, Median: 1200 * time.Microsecond,
			P90: 2 * time.Millisecond, P95: 2400 * time.Microsecond, P99: 3100*time.Microsecond + 300, Max: 4200 * time.Microsecond}},
	}
	p.BadTokens = []metrics.Stats{
		{Type: "nova", Attempted: 30, Succeeded: 29, Failed: 1, Throughput: 0.33, Latency: metrics.Latency{
			Min: 600 * time.Microsecond, Mean: 950 * time.Microsecond, Median: 900 * time.Microsecond,
			P90: 1400 * time.Microsecond, P95: 1600 * time.Microsecond, P99: 2 * time.Millisecond, Max: 2300 * time.Microsecond}},
	}
	p.Propagation = &PressurePropagation{
		Every:   30 * time.Second,
		Timeout: 10 * time.Minute,
		Probes:  12,
		Delay: metrics.Latency{
			Min: 1200 * time.Millisecond, Mean: 2600 * time.Millisecond, Median: 2100 * time.Millisecond,
			P90: 4100 * time.Millisecond, P95: 4800*time.Millisecond + 200*time.Microsecond,
			P99: 6200 * time.Millisecond, Max: 6200 * time.Millisecond},
	}
	return r
}

// TestWritePressureTableServices confirms a run with probes, bad tokens and
// the propagation probe gets, between the Operations and the Stages table, a
// Probes table with one row per service in the columns of the operation
// table, a Bad tokens table the same way and the Propagation block with its
// durations rounded to the millisecond, and that the rest of the report is
// the report of the same run without them, line for line.
func TestWritePressureTableServices(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTable(&buf, pressureServicesRecord()); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	titles := []string{"Operations", "Probes", "Bad tokens", "Propagation", "Stages"}
	at := make([]int, len(titles))
	for i, title := range titles {
		at[i] = slices.Index(lines, title)
		if at[i] < 1 || lines[at[i]-1] != "" {
			t.Fatalf("report lacks the %q title after a blank line:\n%s", title, buf.String())
		}
		if i > 0 && at[i] < at[i-1] {
			t.Fatalf("%q at line %d precedes %q at line %d:\n%s", title, at[i], titles[i-1], at[i-1], buf.String())
		}
	}
	probes, bad, propagation, stages := at[1], at[2], at[3], at[4]

	header := []string{"SERVICE", "OPS", "OK", "FAILED", "OPS/S", "MIN", "MEAN", "P50", "P90", "P95", "P99", "MAX"}
	for _, tc := range []struct {
		title string
		rows  []string
		want  [][]string
	}{
		{"Probes", lines[probes+1 : bad-1], [][]string{
			header,
			{"glance", "60", "60", "0", "0.7", "2.1ms", "3.4ms", "3.1ms", "4.8ms", "5.5ms", "7.2ms", "9ms"},
			{"nova", "64", "63", "1", "0.7", "800µs", "1.35ms", "1.2ms", "2ms", "2.4ms", "3.1ms", "4.2ms"},
		}},
		{"Bad tokens", lines[bad+1 : propagation-1], [][]string{
			header,
			{"nova", "30", "29", "1", "0.3", "600µs", "950µs", "900µs", "1.4ms", "1.6ms", "2ms", "2.3ms"},
		}},
	} {
		if len(tc.rows) != len(tc.want) {
			t.Errorf("%s table has %d lines, want %d:\n%s", tc.title, len(tc.rows), len(tc.want), buf.String())
			continue
		}
		for i, w := range tc.want {
			if got := strings.Fields(tc.rows[i]); !reflect.DeepEqual(got, w) {
				t.Errorf("%s row %d = %q, want %q", tc.title, i, got, w)
			}
		}
		if !strings.HasPrefix(tc.rows[0], "SERVICE  ") {
			t.Errorf("%s header %q does not start with the left-aligned SERVICE column", tc.title, tc.rows[0])
		}
	}

	wantPropagation := []string{
		"  every:     30s",
		"  timeout:   10m0s",
		"  probes:    12",
		"  timed out: 0",
		"  delay:     p50 2.1s / p95 4.8s / p99 6.2s / max 6.2s",
	}
	if got := lines[propagation+1 : stages-1]; !reflect.DeepEqual(got, wantPropagation) {
		t.Errorf("propagation block = %q, want %q", got, wantPropagation)
	}

	var plain bytes.Buffer
	if err := WriteTable(&plain, pressureStagesRecord()); err != nil {
		t.Fatalf("WriteTable(without services): %v", err)
	}
	// Cutting the three new sections, from the blank line before Probes up to
	// the blank line before Stages, leaves the report of the run without them.
	if got, want := strings.Join(slices.Concat(lines[:probes-1], lines[stages-1:]), "\n")+"\n", plain.String(); got != want {
		t.Errorf("report without its service sections differs from the report of the run without them:\n%s\nwant:\n%s", got, want)
	}
}

// TestWritePressureTablePropagationWithoutProbes confirms a run with the
// propagation probe that completed no probe shows its block with "-" as the
// delay rather than 0s percentiles, and that a run without probe and
// bad-token draws gets neither table.
func TestWritePressureTablePropagationWithoutProbes(t *testing.T) {
	p := pressureRecord().Pressure
	p.Propagation = &PressurePropagation{Every: 30 * time.Second, Timeout: 10 * time.Minute}
	var buf bytes.Buffer
	if err := WritePressureTable(&buf, p); err != nil {
		t.Fatalf("WritePressureTable: %v", err)
	}
	out := buf.String()
	const want = "\n\nPropagation\n  every:     30s\n  timeout:   10m0s\n  probes:    0\n  timed out: 0\n  delay:     -\n\nLatency and errors over time\n"
	if !strings.Contains(out, want) {
		t.Errorf("pressure table lacks the propagation block %q between the operation and the bucket table:\n%s", want, out)
	}
	for _, title := range []string{"\nProbes\n", "\nBad tokens\n"} {
		if strings.Contains(out, title) {
			t.Errorf("pressure table of a run without such draws has the %q table:\n%s", strings.TrimSpace(title), out)
		}
	}
}

// TestWriteCSVPressureServices confirms a run with probes, bad tokens and
// the propagation probe gets one row per service labelled
// pressure/probe/<service> and pressure/bad_token/<service>, and one row
// pressure/propagation counting the completed probes as attempted, the
// timed-out ones as failed and carrying the delays, all after the operation
// rows and before the stage and bucket rows.
func TestWriteCSVPressureServices(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, pressureServicesRecord()); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("parsing csv: %v", err)
	}
	var labels []string
	for _, rec := range records[1:] {
		labels = append(labels, rec[0])
	}
	want := []string{
		"overall", "project", "token",
		"pressure/effective", "pressure/validate",
		"pressure/probe/glance", "pressure/probe/nova", "pressure/bad_token/nova", "pressure/propagation",
		"pressure/stage-1/effective", "pressure/stage-1/validate",
		"pressure/stage-2/effective", "pressure/stage-2/validate",
		"pressure@0s/effective", "pressure@0s/validate",
		"pressure@1m0s/effective", "pressure@1m0s/validate",
	}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("row labels = %v, want %v", labels, want)
	}
	row := records[1+slices.Index(labels, "pressure/propagation")]
	if got := row[1:4]; !reflect.DeepEqual(got, []string{"12", "12", "0"}) {
		t.Errorf("pressure/propagation attempted, succeeded, failed = %v, want [12 12 0]", got)
	}
	if row[7] != "2100.000" || row[11] != "6200.000" {
		t.Errorf("pressure/propagation row = %v, want median_ms 2100.000 and max_ms 6200.000", row)
	}
	// pressure/probe/nova: 64 attempted, 1 failed, p50 1.200 ms.
	if row := records[1+slices.Index(labels, "pressure/probe/nova")]; row[1] != "64" || row[3] != "1" || row[7] != "1.200" {
		t.Errorf("pressure/probe/nova row = %v, want attempted 64, failed 1, median 1.200", row)
	}
}

// TestWriteJSONPressureServices confirms a run with probes, bad tokens and
// the propagation probe carries them inside the pressure object of the JSON
// report, the probes sorted by service and the propagation with its probe
// count, and that the pressure object decodes back to the record's.
func TestWriteJSONPressureServices(t *testing.T) {
	rec := pressureServicesRecord()
	var buf bytes.Buffer
	if err := WriteJSON(&buf, rec); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var got struct {
		Pressure struct {
			Probes []struct {
				Type string `json:"type"`
			} `json:"probes"`
			BadTokens []struct {
				Type string `json:"type"`
			} `json:"badTokens"`
			Propagation struct {
				Probes int `json:"probes"`
			} `json:"propagation"`
		} `json:"pressure"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("report JSON does not decode: %v", err)
	}
	if len(got.Pressure.Probes) != 2 || got.Pressure.Probes[0].Type != "glance" || got.Pressure.Probes[1].Type != "nova" {
		t.Errorf(".pressure.probes = %+v, want glance and nova", got.Pressure.Probes)
	}
	if len(got.Pressure.BadTokens) != 1 || got.Pressure.BadTokens[0].Type != "nova" {
		t.Errorf(".pressure.badTokens = %+v, want nova", got.Pressure.BadTokens)
	}
	if got.Pressure.Propagation.Probes != 12 {
		t.Errorf(".pressure.propagation.probes = %d, want 12", got.Pressure.Propagation.Probes)
	}

	var whole struct {
		Pressure *PressureStats `json:"pressure"`
	}
	if err := json.Unmarshal(buf.Bytes(), &whole); err != nil {
		t.Fatalf("decoding .pressure: %v", err)
	}
	if !reflect.DeepEqual(whole.Pressure, rec.Pressure) {
		t.Errorf(".pressure = %+v, want %+v", whole.Pressure, rec.Pressure)
	}
}
