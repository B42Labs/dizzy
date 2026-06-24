package run

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/openstack-tester/internal/metrics"
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
// error breakdown.
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
