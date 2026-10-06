package run

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/metrics"
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
