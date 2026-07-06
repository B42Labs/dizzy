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
