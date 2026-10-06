// Package run persists the outcome of one apply as a run record and renders its
// metrics. A record (run-<id>.json) captures the created resource IDs, the run's
// provenance and timing, the aggregated metrics, and any apply error, so a run
// can be reported on, re-checked (status), or cleaned up (cleanup) after the
// process that produced it has exited. A chaos run also rewrites its record
// periodically while it runs, marked incomplete, so a killed process or a lost
// node still leaves a recent record.
package run

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Record is the persisted result of one apply. It is the canonical,
// machine-readable hand-off surface that report, status, and cleanup consume.
// Created lists every resource the run actually created (in dependency order),
// Metrics holds the aggregated timing, and Error is the apply error message when
// the run failed partway, empty otherwise.
type Record struct {
	RunID string `json:"runID"`
	// Service names the OpenStack service the run exercised (neutron, cinder,
	// keystone, nova, or glance). It is omitempty and read as "neutron" when
	// absent, so run records written before Cinder support (which carry no service
	// field) still load and report unchanged.
	Service    string              `json:"service,omitempty"`
	Scenario   string              `json:"scenario"`
	Seed       int64               `json:"seed"`
	StartedAt  time.Time           `json:"startedAt"`
	FinishedAt time.Time           `json:"finishedAt"`
	Created    []resource.Resource `json:"created"`
	Error      string              `json:"error,omitempty"`
	Metrics    metrics.Aggregate   `json:"metrics"`
	// VolumeType records the Cinder volume type every volume of a cinder run was
	// created with, for provenance. It is empty for a neutron run and for a
	// cinder run that used the cloud's default type.
	VolumeType string `json:"volumeType,omitempty"`
	// Chaos holds the churn-specific statistics of a soak/chaos run. It is nil
	// for an apply run, so an apply record's shape is unchanged.
	Chaos *ChaosStats `json:"chaos,omitempty"`
	// Incomplete marks a checkpoint a chaos run wrote while it was still
	// running: the run was still going or was killed before its final record.
	// FinishedAt is then the checkpoint time. The final write omits the key.
	Incomplete bool `json:"incomplete,omitempty"`
}

// ChaosStats holds the churn-specific statistics of a soak/chaos run, persisted
// alongside the standard metrics: the create/delete and completed-cycle counts,
// the live-population summary over the run, the controller's target fill, and
// per-time-bucket latency/error statistics. The schema lives here, not in the
// chaos package, so the read-side commands (report, status) can render it
// without importing the engine. It mirrors chaos.Result.
type ChaosStats struct {
	Creates int `json:"creates"`
	Deletes int `json:"deletes"`
	// Mutates counts the in-place mutations (Cinder volume extends) a churn run
	// drew. It is omitempty so a Neutron churn record, which never mutates, keeps
	// its shape, and an older record without the field loads as zero.
	Mutates    int           `json:"mutates,omitempty"`
	Cycles     int           `json:"cycles"`
	PopMin     int           `json:"popMin"`
	PopMax     int           `json:"popMax"`
	PopMean    float64       `json:"popMean"`
	TargetFill float64       `json:"targetFill"`
	Buckets    []ChaosBucket `json:"buckets,omitempty"`
	// BucketWidth is the fixed width of every time bucket of an unbounded run
	// (--duration 0). It is omitempty and 0 for a bounded run, whose ten
	// buckets divide its duration equally.
	BucketWidth time.Duration `json:"bucketWidth,omitempty"`
}

// ChaosBucket is one time slice of a churn run: the operations whose decision
// offset fell within it, summarized so latency and error degradation over time
// is visible rather than only an aggregate.
type ChaosBucket struct {
	Start  time.Duration        `json:"start"`
	Stats  metrics.Stats        `json:"stats"`
	Errors []metrics.ErrorCount `json:"errors,omitempty"`
}

// Write marshals r as indented JSON and writes it to dir as run-<id>.json,
// returning the path written. It writes to a temp file, syncs it to disk and
// renames it over the record, so a kill or a node loss mid-write leaves the
// previous record or the new one, never a truncated one. Writing the same run
// id again replaces the record, which is how a chaos run checkpoints.
func Write(dir string, r *Record) (string, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding run record: %w", err)
	}
	data = append(data, '\n')

	path := filepath.Join(dir, "run-"+r.RunID+".json")
	tmp := path + ".tmp"
	if err := writeSynced(tmp, data); err != nil {
		return "", fmt.Errorf("writing run record to %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("finalizing run record %s: %w", path, err)
	}
	return path, nil
}

// writeSynced writes data to path and syncs the file before closing it, so the
// rename that follows cannot publish a record whose data is not yet on disk.
// The first error of the write, the sync and the close is returned.
func writeSynced(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// Load reads and decodes the run record at path.
func Load(path string) (*Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading run record: %w", err)
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decoding run record %s: %w", path, err)
	}
	return &r, nil
}
