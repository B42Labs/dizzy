package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/run"
)

// writeReportRecord seeds a run record carrying a distinctive network
// attempted-count and returns its path.
func writeReportRecord(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	rec := &run.Record{
		RunID: "abcd1234",
		Metrics: metrics.Aggregate{
			Wall:    time.Second,
			Overall: metrics.Stats{Attempted: 7, Succeeded: 7},
			ByType:  []metrics.Stats{{Type: "network", Attempted: 7, Succeeded: 7}},
		},
	}
	if _, err := run.Write(dir, rec); err != nil {
		t.Fatalf("seeding run record: %v", err)
	}
	return filepath.Join(dir, "run-abcd1234.json")
}

// TestReportConsistentAcrossFormats covers the "report produces consistent
// output across formats" acceptance criterion: the network type and its
// attempted-count appear in the table, JSON, and CSV renderings alike.
func TestReportConsistentAcrossFormats(t *testing.T) {
	path := writeReportRecord(t)

	for _, format := range []string{"table", "json", "csv", "html"} {
		t.Run(format, func(t *testing.T) {
			out, err := execRoot(t, "neutron", "report", "--run", path, "--format", format)
			if err != nil {
				t.Fatalf("report --format %s: %v", format, err)
			}
			if !strings.Contains(out, "network") {
				t.Errorf("%s output missing network type:\n%s", format, out)
			}
			if !strings.Contains(out, "7") {
				t.Errorf("%s output missing the attempted-count 7:\n%s", format, out)
			}
		})
	}
}

func TestReportRejectsUnknownFormat(t *testing.T) {
	path := writeReportRecord(t)
	if _, err := execRoot(t, "neutron", "report", "--run", path, "--format", "xml"); err == nil {
		t.Fatal("report --format xml: expected error, got nil")
	}
}

func TestReportRequiresRunFlag(t *testing.T) {
	if _, err := execRoot(t, "neutron", "report"); err == nil {
		t.Fatal("report without --run: expected error, got nil")
	}
}

func TestReportMissingRecordErrors(t *testing.T) {
	if _, err := execRoot(t, "neutron", "report", "--run", filepath.Join(t.TempDir(), "run-nope.json")); err == nil {
		t.Fatal("report with a missing record: expected error, got nil")
	}
}

// writePressureRecord seeds a keystone pressure record of the small scenario
// with the given run id and seed in dir, whose one operation, validate, has
// the given p50, and returns its path.
func writePressureRecord(t *testing.T, dir, runID string, seed int64, p50 time.Duration) string {
	t.Helper()
	validate := metrics.Stats{Type: "validate", Attempted: 36, Succeeded: 36, Throughput: 0.4,
		Latency: metrics.Latency{Median: p50, P95: 900 * time.Microsecond, P99: 1200 * time.Microsecond}}
	overall := validate
	overall.Type = ""
	rec := &run.Record{
		RunID:    runID,
		Service:  "keystone",
		Scenario: "small",
		Seed:     seed,
		Metrics:  metrics.Aggregate{Wall: 90 * time.Second, Overall: overall},
		Pressure: &run.PressureStats{
			Workers:    4,
			Tokens:     3,
			Mix:        []run.PressureWeight{{Operation: "validate", Weight: 9}},
			Operations: metrics.Aggregate{Wall: 90 * time.Second, Overall: overall, ByType: []metrics.Stats{validate}},
		},
	}
	path, err := run.Write(dir, rec)
	if err != nil {
		t.Fatalf("seeding run record: %v", err)
	}
	return path
}

// TestReportCompare confirms "keystone report --compare" renders the
// comparison of two pressure records of the same load, the --run record as
// the base, in the table, JSON and HTML formats.
func TestReportCompare(t *testing.T) {
	dir := t.TempDir()
	a := writePressureRecord(t, dir, "press001", 42, 350*time.Microsecond)
	b := writePressureRecord(t, dir, "press002", 42, 280*time.Microsecond)

	for format, want := range map[string]string{
		"table": "  base:  run press001, scenario small, seed 42, workers 4, tokens 3, mix validate=9\n" +
			"  other: run press002, scenario small, seed 42, workers 4, tokens 3, mix validate=9\n",
		"json": "{\n  \"base\": {\n    \"runID\": \"press001\",",
		"html": "<h1>Comparison: run press001 vs run press002</h1>",
	} {
		t.Run(format, func(t *testing.T) {
			out, err := execRoot(t, "keystone", "report", "--run", a, "--compare", b, "--format", format)
			if err != nil {
				t.Fatalf("report --compare --format %s: %v", format, err)
			}
			for _, id := range []string{"press001", "press002"} {
				if !strings.Contains(out, id) {
					t.Errorf("%s comparison lacks run id %s:\n%s", format, id, out)
				}
			}
			if !strings.Contains(out, want) {
				t.Errorf("%s comparison lacks %q:\n%s", format, want, out)
			}
		})
	}
}

// TestReportCompareRejectsCSV confirms --compare refuses the CSV format
// before it loads a record, so the error is the same for paths that do not
// exist, and that an unknown format keeps the message of a single report.
func TestReportCompareRejectsCSV(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "run-nope.json")
	_, err := execRoot(t, "keystone", "report", "--run", missing, "--compare", missing, "--format", "csv")
	const want = "--compare is not available for --format csv; use table, json or html"
	if err == nil || err.Error() != want {
		t.Errorf("report --compare --format csv error = %v, want %q", err, want)
	}

	a := writePressureRecord(t, dir, "press001", 42, 350*time.Microsecond)
	b := writePressureRecord(t, dir, "press002", 42, 280*time.Microsecond)
	_, err = execRoot(t, "keystone", "report", "--run", a, "--compare", b, "--format", "xml")
	const wantUnknown = `unknown --format "xml": want table, json, csv, or html`
	if err == nil || err.Error() != wantUnknown {
		t.Errorf("report --compare --format xml error = %v, want %q", err, wantUnknown)
	}
}

// TestReportCompareMismatch confirms --compare refuses two records of
// different loads with the comparison error wrapped with both paths, and
// that --allow-mismatch renders the comparison with the difference named.
func TestReportCompareMismatch(t *testing.T) {
	dir := t.TempDir()
	a := writePressureRecord(t, dir, "press001", 42, 350*time.Microsecond)
	b := writePressureRecord(t, dir, "press002", 43, 280*time.Microsecond)

	_, err := execRoot(t, "keystone", "report", "--run", a, "--compare", b)
	want := "comparing " + a + " with " + b + ": the records come from different loads (seed 42 vs 43)"
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("report --compare error = %v, want it to start with %q", err, want)
	}

	out, err := execRoot(t, "keystone", "report", "--run", a, "--compare", b, "--allow-mismatch")
	if err != nil {
		t.Fatalf("report --compare --allow-mismatch: %v", err)
	}
	if !strings.Contains(out, "\n  loads differ: seed 42 vs 43\n") {
		t.Errorf("comparison with --allow-mismatch does not name the difference:\n%s", out)
	}
}

// TestReportCompareNeedsPressureRecords confirms --compare refuses a record
// without pressure statistics with the comparison error wrapped with both
// paths.
func TestReportCompareNeedsPressureRecords(t *testing.T) {
	plain := writeReportRecord(t)
	pressure := writePressureRecord(t, t.TempDir(), "press001", 42, 350*time.Microsecond)

	_, err := execRoot(t, "keystone", "report", "--run", pressure, "--compare", plain)
	want := "comparing " + pressure + " with " + plain + ": run abcd1234 has no pressure statistics; the comparison needs two keystone pressure records"
	if err == nil || err.Error() != want {
		t.Errorf("report --compare error = %v, want %q", err, want)
	}
}

// TestReportAllowMismatchNeedsCompare confirms --allow-mismatch without
// --compare fails before a record is loaded.
func TestReportAllowMismatchNeedsCompare(t *testing.T) {
	_, err := execRoot(t, "keystone", "report", "--run", filepath.Join(t.TempDir(), "run-nope.json"), "--allow-mismatch")
	const want = "--allow-mismatch needs --compare"
	if err == nil || err.Error() != want {
		t.Errorf("report --allow-mismatch error = %v, want %q", err, want)
	}
}
