package run

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
			{Kind: neutron.KindNetwork, Logical: "net-0001", Name: "dizzy-abcd1234-net-0001", ID: "net-id-1"},
			{Kind: neutron.KindSubnet, Logical: "subnet-0001", Name: "dizzy-abcd1234-subnet-0001", ID: "sub-id-1"},
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
    {"kind": "network", "logical": "net-0001", "name": "dizzy-legacy01-net-0001", "id": "n1"}
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

// TestRecordIncompleteAndBucketWidthRoundTrip confirms a checkpoint's incomplete
// marker and an unbounded run's bucket width survive a write/load round trip.
func TestRecordIncompleteAndBucketWidthRoundTrip(t *testing.T) {
	rec := sampleRecord()
	rec.RunID = "ckpt0001"
	rec.Incomplete = true
	rec.Chaos = &ChaosStats{Creates: 3, BucketWidth: time.Hour}

	path, err := Write(t.TempDir(), rec)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.Incomplete || loaded.Chaos == nil || loaded.Chaos.BucketWidth != time.Hour {
		t.Errorf("loaded incomplete/bucketWidth = %v/%+v, want true/1h", loaded.Incomplete, loaded.Chaos)
	}
}

// TestRecordOmitsIncompleteAndBucketWidth confirms a final record of a bounded
// run carries neither key, so its shape is unchanged.
func TestRecordOmitsIncompleteAndBucketWidth(t *testing.T) {
	rec := sampleRecord()
	rec.Chaos = &ChaosStats{Creates: 3}

	path, err := Write(t.TempDir(), rec)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading record: %v", err)
	}
	for _, key := range []string{`"incomplete"`, `"bucketWidth"`} {
		if bytes.Contains(data, []byte(key)) {
			t.Errorf("record carries %s although it is unset:\n%s", key, data)
		}
	}
}

// TestLegacyChaosRecordLoadsComplete confirms a chaos record written before the
// incomplete marker and the bucket width existed loads as a complete record of
// a bounded run.
func TestLegacyChaosRecordLoadsComplete(t *testing.T) {
	legacy := `{
  "runID": "legacy02",
  "scenario": "small",
  "seed": 1,
  "startedAt": "2026-06-24T10:00:00Z",
  "finishedAt": "2026-06-24T10:10:00Z",
  "created": null,
  "metrics": {"wall": 600000000000, "overall": {"type": "", "attempted": 0, "succeeded": 0, "failed": 0, "throughput": 0, "latency": {"min": 0, "mean": 0, "median": 0, "p90": 0, "p95": 0, "p99": 0, "max": 0}}, "byType": null, "errors": null, "readiness": null},
  "chaos": {"creates": 4, "deletes": 2, "cycles": 2, "popMin": 0, "popMax": 3, "popMean": 1.5, "targetFill": 0.8}
}` + "\n"
	path := filepath.Join(t.TempDir(), "run-legacy02.json")
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("writing legacy record: %v", err)
	}

	rec, err := Load(path)
	if err != nil {
		t.Fatalf("Load of a legacy chaos record: %v", err)
	}
	if rec.Incomplete {
		t.Error("legacy record loaded as incomplete, want complete")
	}
	if rec.Chaos == nil || rec.Chaos.BucketWidth != 0 || rec.Chaos.Creates != 4 {
		t.Errorf("legacy chaos stats = %+v, want creates 4 and bucket width 0", rec.Chaos)
	}
}

// TestWriteToMissingDirFails confirms a write into a directory that does not
// exist fails with the wrapped path error and leaves nothing behind.
func TestWriteToMissingDirFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	_, err := Write(dir, sampleRecord())
	if err == nil {
		t.Fatal("Write into a missing directory: expected an error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "writing run record to") {
		t.Errorf("error %q does not start with %q", err.Error(), "writing run record to")
	}
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Errorf("error %q does not wrap an *fs.PathError", err.Error())
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("Write left %s behind (stat: %v)", dir, statErr)
	}
}

// TestWriteTwiceKeepsLatest confirms rewriting a record for the same run id,
// as a chaos checkpoint does, replaces it and leaves no temp file behind.
func TestWriteTwiceKeepsLatest(t *testing.T) {
	dir := t.TempDir()
	first := sampleRecord()
	first.Incomplete = true
	if _, err := Write(dir, first); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	second := sampleRecord()
	second.FinishedAt = first.FinishedAt.Add(time.Minute)
	path, err := Write(dir, second)
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(second, loaded) {
		t.Errorf("Load returned %+v, want the second record %+v", loaded, second)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("temp file %s.tmp remains after the write (stat: %v)", path, err)
	}
}

// TestRecordRoundTripWithPersonas confirms a mix record's services, personas
// and per-resource persona survive a write/load round trip intact.
func TestRecordRoundTripWithPersonas(t *testing.T) {
	rec := sampleRecord()
	rec.RunID = "mix00001"
	rec.Service = "mix"
	rec.Created[0].Persona = "ci"
	rec.Created[1].Persona = "ci"
	rec.Services = []string{"octavia"}
	rec.Personas = []PersonaStats{{
		Name: "ci", RunID: "mix00001-ci", Cloud: "tenant-ci", ProjectID: "proj-1",
		Share: 1, Servers: 6, Seed: 7,
		Metrics: metrics.Aggregate{Wall: time.Minute, Overall: metrics.Stats{Attempted: 3, Succeeded: 3}},
		Chaos:   &ChaosStats{Creates: 3, TargetFill: 0.6},
	}, {
		Name: "legacy", RunID: "mix00001-legacy", Share: 0, Servers: 0,
	}}

	path, err := Write(t.TempDir(), rec)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(rec, loaded) {
		t.Errorf("persona round-trip mismatch:\n got %+v\nwant %+v", loaded, rec)
	}
}

// TestRecordRoundTripWithLanes confirms a mix record's lanes and the lane of
// each of their resources survive a write/load round trip intact.
func TestRecordRoundTripWithLanes(t *testing.T) {
	rec := sampleRecord()
	rec.RunID = "mix00001"
	rec.Service = "mix"
	rec.Created[0].Persona = "ci"
	rec.Created[1].Lane = "keystone"
	rec.Personas = []PersonaStats{{Name: "ci", RunID: "mix00001-ci", Share: 1, Servers: 6, Seed: 7}}
	rec.Lanes = []LaneStats{{
		Name: "keystone", RunID: "mix00001-keystone", Cloud: "admin", ProjectID: "proj-admin",
		Scenario: "small/keystone", Seed: 9,
		Metrics: metrics.Aggregate{Wall: time.Minute, Overall: metrics.Stats{Attempted: 4, Succeeded: 3, Failed: 1}},
		Chaos:   &ChaosStats{Creates: 2, Mutates: 1, TargetFill: 0.8},
	}, {
		Name: "glance", RunID: "mix00001-glance", Scenario: "small/glance",
	}}

	path, err := Write(t.TempDir(), rec)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(rec, loaded) {
		t.Errorf("lane round-trip mismatch:\n got %+v\nwant %+v", loaded, rec)
	}
}

// TestRecordOmitsPersonaKeys confirms a record of a single-service run carries
// none of the mix keys, so its shape is unchanged.
func TestRecordOmitsPersonaKeys(t *testing.T) {
	path, err := Write(t.TempDir(), sampleRecord())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading record: %v", err)
	}
	for _, key := range []string{`"services"`, `"personas"`, `"persona"`, `"lanes"`, `"lane"`} {
		if bytes.Contains(data, []byte(key)) {
			t.Errorf("record carries %s although it is unset:\n%s", key, data)
		}
	}
}
