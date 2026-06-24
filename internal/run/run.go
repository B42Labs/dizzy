// Package run persists the outcome of one apply as a run record and renders its
// metrics. A record (run-<id>.json) captures the created resource IDs, the run's
// provenance and timing, the aggregated metrics, and any apply error, so a run
// can be reported on, re-checked (status), or cleaned up (cleanup) after the
// process that produced it has exited.
package run

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/B42Labs/openstack-tester/internal/metrics"
	"github.com/B42Labs/openstack-tester/internal/neutron"
)

// Record is the persisted result of one apply. It is the canonical,
// machine-readable hand-off surface that report, status, and cleanup consume.
// Created lists every resource the run actually created (in dependency order),
// Metrics holds the aggregated timing, and Error is the apply error message when
// the run failed partway, empty otherwise.
type Record struct {
	RunID      string             `json:"runID"`
	Scenario   string             `json:"scenario"`
	Seed       int64              `json:"seed"`
	StartedAt  time.Time          `json:"startedAt"`
	FinishedAt time.Time          `json:"finishedAt"`
	Created    []neutron.Resource `json:"created"`
	Error      string             `json:"error,omitempty"`
	Metrics    metrics.Aggregate  `json:"metrics"`
}

// Write marshals r as indented JSON and writes it to dir as run-<id>.json,
// returning the path written. It writes to a temp file and renames so a kill
// mid-write never leaves a truncated record, mirroring the generate command.
func Write(dir string, r *Record) (string, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding run record: %w", err)
	}
	data = append(data, '\n')

	path := filepath.Join(dir, "run-"+r.RunID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", fmt.Errorf("writing run record to %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("finalizing run record %s: %w", path, err)
	}
	return path, nil
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
