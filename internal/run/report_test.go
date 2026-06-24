package run

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/B42Labs/openstack-tester/internal/metrics"
)

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
