package run

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/neutron"
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

// TestRecordServiceRoundTrip confirms a cinder record's service and volume type
// survive a write/load round trip, so a run's provenance is not lost.
func TestRecordServiceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := sampleRecord()
	rec.RunID = "cinder01"
	rec.Service = "cinder"
	rec.VolumeType = "ssd"

	path, err := Write(dir, rec)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(rec, loaded) {
		t.Errorf("service round-trip mismatch:\n got %+v\nwant %+v", loaded, rec)
	}
}

// TestLegacyRecordWithoutServiceLoads covers the "old run records without a
// service field still load" acceptance criterion: a pre-Cinder record carries
// no service key and must decode to an empty Service (read as neutron) rather
// than fail to load.
func TestLegacyRecordWithoutServiceLoads(t *testing.T) {
	dir := t.TempDir()
	legacy := `{
  "runID": "legacy01",
  "scenario": "small",
  "seed": 1,
  "startedAt": "2026-06-24T10:00:00Z",
  "finishedAt": "2026-06-24T10:00:05Z",
  "created": [
    {"kind": "network", "logical": "net-0001", "name": "ostester-legacy01-net-0001", "id": "n1"}
  ],
  "metrics": {"wall": 5000000000, "overall": {"type": "", "attempted": 1, "succeeded": 1, "failed": 0, "throughput": 0.2, "latency": {"min": 0, "mean": 0, "median": 0, "p90": 0, "p95": 0, "p99": 0, "max": 0}}, "byType": null, "errors": null, "readiness": null}
}` + "\n"
	path := filepath.Join(dir, "run-legacy01.json")
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("writing legacy record: %v", err)
	}

	rec, err := Load(path)
	if err != nil {
		t.Fatalf("Load of a legacy record without a service field: %v", err)
	}
	if rec.Service != "" {
		t.Errorf("legacy record Service = %q, want empty (read as neutron)", rec.Service)
	}
	if len(rec.Created) != 1 || rec.Created[0].ID != "n1" {
		t.Errorf("legacy record Created = %+v, want the one recorded network", rec.Created)
	}
}
