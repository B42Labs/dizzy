package run

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/B42Labs/openstack-tester/internal/metrics"
	"github.com/B42Labs/openstack-tester/internal/neutron"
)

// sampleRecord builds a Record exercising every field, including a non-empty
// Error and per-type metrics, so round-trip and rendering tests have real data.
func sampleRecord() *Record {
	return &Record{
		RunID:      "abcd1234",
		Scenario:   "medium",
		Seed:       42,
		StartedAt:  time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 6, 24, 10, 1, 30, 0, time.UTC),
		Created: []neutron.Resource{
			{Kind: neutron.KindNetwork, Logical: "net-0001", Name: "ostester-abcd1234-net-0001", ID: "net-id-1"},
			{Kind: neutron.KindSubnet, Logical: "subnet-0001", Name: "ostester-abcd1234-subnet-0001", ID: "sub-id-1"},
		},
		Error: "applying plan (run abcd1234): creating port \"port-0001\": boom",
		Metrics: metrics.Aggregate{
			Wall:    90 * time.Second,
			Overall: metrics.Stats{Attempted: 3, Succeeded: 2, Failed: 1, Throughput: 0.02},
			ByType: []metrics.Stats{
				{Type: "network", Attempted: 1, Succeeded: 1, Latency: metrics.Latency{Min: time.Second, Max: 2 * time.Second}},
				{Type: "subnet", Attempted: 1, Succeeded: 1},
			},
			Errors: []metrics.ErrorCount{{Kind: "http_500", Count: 1}},
		},
	}
}

// TestRecordRoundTrip covers the "a run record round-trips" acceptance
// criterion: a record written to disk loads back equal to the original.
func TestRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := sampleRecord()

	path, err := Write(dir, rec)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if want := filepath.Join(dir, "run-abcd1234.json"); path != want {
		t.Errorf("Write path = %q, want %q", path, want)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(rec, loaded) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", loaded, rec)
	}
}

// TestRecordRoundTripWithChaos confirms a churn record's chaos statistics
// survive a write/load round trip intact.
func TestRecordRoundTripWithChaos(t *testing.T) {
	dir := t.TempDir()
	rec := sampleRecord()
	rec.RunID = "chaos001"
	rec.Chaos = &ChaosStats{
		Creates: 12, Deletes: 9, Cycles: 9,
		PopMin: 0, PopMax: 5, PopMean: 3.25, TargetFill: 0.6,
		Buckets: []ChaosBucket{{
			Start:  10 * time.Second,
			Stats:  metrics.Stats{Attempted: 2, Succeeded: 1, Failed: 1},
			Errors: []metrics.ErrorCount{{Kind: "quota", Count: 1}},
		}},
	}

	path, err := Write(dir, rec)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(rec, loaded) {
		t.Errorf("chaos round-trip mismatch:\n got %+v\nwant %+v", loaded, rec)
	}
}

// TestLoadMissingFile confirms loading a record that does not exist returns an
// error rather than a zero-valued record.
func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "run-nope.json")); err == nil {
		t.Fatal("Load of a missing record: expected an error, got nil")
	}
}
