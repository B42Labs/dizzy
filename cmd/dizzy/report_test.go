package main

import (
	"encoding/json"
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

// TestKeystoneReportCompareServices confirms "keystone report --compare"
// renders the probes and the propagation probe of two pressure records of
// the same load, the propagation settings equal so they compare without a
// mismatch, in the table, JSON and HTML formats: each output names nova, the
// probed service, and carries the propagation delays of both runs as the
// format renders them. The CSV format stays refused.
func TestKeystoneReportCompareServices(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, r := range []struct {
		runID              string
		novaP50            time.Duration
		delayP50, delayMax time.Duration
	}{
		{"press001", 1200 * time.Microsecond, 180 * time.Second, 300 * time.Second},
		{"press002", 900 * time.Microsecond, 3 * time.Second, 6 * time.Second},
	} {
		// The pressure record of TestReportCompare, with probes at nova and
		// the propagation probe added and written again under its run id.
		rec, err := run.Load(writePressureRecord(t, dir, r.runID, 42, 350*time.Microsecond))
		if err != nil {
			t.Fatalf("loading the seeded record: %v", err)
		}
		rec.Pressure.Probes = []metrics.Stats{{Type: "nova", Attempted: 12, Succeeded: 12, Throughput: 0.1,
			Latency: metrics.Latency{Median: r.novaP50, P95: 2 * r.novaP50, P99: 3 * r.novaP50}}}
		rec.Pressure.Propagation = &run.PressurePropagation{Every: 30 * time.Second, Timeout: 10 * time.Minute, Probes: 12,
			Delay: metrics.Latency{Median: r.delayP50, P95: r.delayMax, P99: r.delayMax, Max: r.delayMax}}
		path, err := run.Write(dir, rec)
		if err != nil {
			t.Fatalf("writing run record: %v", err)
		}
		paths = append(paths, path)
	}
	a, b := paths[0], paths[1]

	compare := func(t *testing.T, format string) string {
		t.Helper()
		out, err := execRoot(t, "keystone", "report", "--run", a, "--compare", b, "--format", format)
		if err != nil {
			t.Fatalf("report --compare --format %s: %v", format, err)
		}
		return out
	}

	t.Run("table", func(t *testing.T) {
		out := compare(t, "table")
		for _, want := range []string{
			"  base:  run press001, scenario small, seed 42, workers 4, tokens 3, mix validate=9, propagation every=30s,timeout=10m0s\n",
			"\nProbes\n", "\nPropagation\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("table comparison lacks %q:\n%s", want, out)
			}
		}
		// The tables are aligned, so the rows are found by their fields.
		rows := make(map[string]bool)
		for _, line := range strings.Split(out, "\n") {
			rows[strings.Join(strings.Fields(line), " ")] = true
		}
		for _, want := range []string{
			"nova p50 1.2ms 900µs -300µs -25.0%",
			"p50 3m0s 3s -2m57s -98.3%",
			"max 5m0s 6s -4m54s -98.0%",
			"probes 12 12 +0 +0.0%",
		} {
			if !rows[want] {
				t.Errorf("table comparison has no row %q:\n%s", want, out)
			}
		}
	})

	t.Run("json", func(t *testing.T) {
		var c run.Comparison
		if err := json.Unmarshal([]byte(compare(t, "json")), &c); err != nil {
			t.Fatalf("decoding the JSON comparison: %v", err)
		}
		if len(c.Probes) != 1 || c.Probes[0].Operation != "nova" || c.Probes[0].Delta == nil || c.Probes[0].Delta.P50 != -300*time.Microsecond {
			t.Errorf("probes = %+v, want nova with a p50 delta of -300µs", c.Probes)
		}
		p := c.Propagation
		if p == nil || p.Base == nil || p.Other == nil || p.Base.P50 != 180*time.Second || p.Other.P50 != 3*time.Second || p.Base.Max != 300*time.Second || p.Other.Max != 6*time.Second {
			t.Errorf("propagation = %+v, want base p50 3m0s and max 5m0s, other p50 3s and max 6s", p)
		}
		if len(c.Mismatches) != 0 {
			t.Errorf("mismatches = %v, want none for the same load", c.Mismatches)
		}
	})

	t.Run("html", func(t *testing.T) {
		out := compare(t, "html")
		for _, want := range []string{
			"<h2>Probes</h2>",
			`<td data-sort="nova">nova</td>`,
			"<h2>Propagation</h2>",
			`<td data-sort="p50">p50</td><td data-sort="180000000000">3m0s</td><td data-sort="3000000000">3s</td>`,
			`<td data-sort="max">max</td><td data-sort="300000000000">5m0s</td><td data-sort="6000000000">6s</td>`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("html comparison lacks %q", want)
			}
		}
	})

	t.Run("csv", func(t *testing.T) {
		_, err := execRoot(t, "keystone", "report", "--run", a, "--compare", b, "--format", "csv")
		const want = "--compare is not available for --format csv; use table, json or html"
		if err == nil || err.Error() != want {
			t.Errorf("report --compare --format csv error = %v, want %q", err, want)
		}
	})
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
