// Package run persists the outcome of one apply as a run record and renders its
// metrics. A record (run-<id>.json) captures the created resource IDs, the run's
// provenance and timing, the aggregated metrics, and any apply error, so a run
// can be reported on, re-checked (status), or cleaned up (cleanup) after the
// process that produced it has exited. A chaos or pressure run also rewrites
// its record periodically while it runs, marked incomplete, so a killed
// process or a lost node still leaves a recent record.
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
	// keystone, nova, or glance), or mix for a combined run of several
	// workload personas. It is omitempty and read as "neutron" when
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
	// for an apply run, so an apply record's shape is unchanged. It is also nil
	// for a mix run, whose churn statistics are per persona and lane.
	Chaos *ChaosStats `json:"chaos,omitempty"`
	// Services lists the opt-in services a mix run bound to its personas. It is
	// omitted when empty.
	Services []string `json:"services,omitempty"`
	// Personas holds one entry per workload persona of a mix run, in the order
	// the run ran them. It is omitted for every other run.
	Personas []PersonaStats `json:"personas,omitempty"`
	// Lanes holds one entry per background lane of a mix run, in the order the
	// run ran them. It is omitted when the run had none.
	Lanes []LaneStats `json:"lanes,omitempty"`
	// Pressure holds the statistics of a keystone pressure run. It is nil for
	// every other run, so their records carry no pressure key, and a record
	// written before pressure runs existed loads with it nil.
	Pressure *PressureStats `json:"pressure,omitempty"`
	// Incomplete marks a checkpoint a chaos or pressure run wrote while it was
	// still running: the run was still going or was killed before its final
	// record. FinishedAt is then the checkpoint time. The final write omits
	// the key.
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

// PersonaStats is one workload persona of a mix run. RunID is the identity its
// resources carry, the run id suffixed with the persona name. Cloud is the
// clouds.yaml entry the scenario named for it, empty when the persona used the
// --os-cloud fallback, and ProjectID the project it authenticated against,
// empty when unknown. Share is the persona's normalized share of the server
// envelope and Servers its part of it. Metrics and Chaos are the persona's own
// aggregate and churn statistics; Chaos is nil in a checkpoint written before
// the persona's engine produced a snapshot.
type PersonaStats struct {
	Name      string            `json:"name"`
	RunID     string            `json:"runID"`
	Cloud     string            `json:"cloud,omitempty"`
	ProjectID string            `json:"projectID,omitempty"`
	Share     float64           `json:"share"`
	Servers   int               `json:"servers"`
	Seed      int64             `json:"seed"`
	Metrics   metrics.Aggregate `json:"metrics"`
	Chaos     *ChaosStats       `json:"chaos,omitempty"`
}

// LaneStats is one background lane of a mix run, the churn of a single
// service. Name is the service, and RunID the identity its resources carry,
// the run id suffixed with the lane name. Cloud and ProjectID mean what they
// mean for a persona. Scenario and Seed are the name and seed of the lane's
// plan. Metrics and Chaos are the lane's own aggregate and churn statistics;
// Chaos is nil in a checkpoint written before the lane's engine produced a
// snapshot.
type LaneStats struct {
	Name      string            `json:"name"`
	RunID     string            `json:"runID"`
	Cloud     string            `json:"cloud,omitempty"`
	ProjectID string            `json:"projectID,omitempty"`
	Scenario  string            `json:"scenario"`
	Seed      int64             `json:"seed"`
	Metrics   metrics.Aggregate `json:"metrics"`
	Chaos     *ChaosStats       `json:"chaos,omitempty"`
}

// ChaosBucket is one time slice of a churn run: the operations whose decision
// offset fell within it, summarized so latency and error degradation over time
// is visible rather than only an aggregate.
type ChaosBucket struct {
	Start  time.Duration        `json:"start"`
	Stats  metrics.Stats        `json:"stats"`
	Errors []metrics.ErrorCount `json:"errors,omitempty"`
}

// PressureStats holds the statistics of a closed-loop pressure run, persisted
// next to the standard metrics: the worker count, the token pool size, the
// users reserved for role churn, the operation mix, the per-operation
// aggregate, the revocations the run made, per-time-bucket statistics and,
// for a staged run, per-stage statistics. The schema lives here, not in the
// pressure package, so the read-side commands can render it without
// importing the engine. The percentiles in Operations, in every bucket and in
// every stage are the metrics collector's estimates within 1%.
type PressureStats struct {
	Workers int `json:"workers"`
	Tokens  int `json:"tokens"`
	// ChurnUsers is the number of users reserved for the grant_churn
	// operation. It is omitempty and 0 when the run reserved none.
	ChurnUsers int `json:"churnUsers,omitempty"`
	// Mix lists every operation with a positive weight, sorted by operation
	// name.
	Mix []PressureWeight `json:"mix"`
	// Operations aggregates the run's samples, one ByType entry per operation
	// name.
	Operations metrics.Aggregate `json:"operations"`
	// Revocations counts the revoke operations of the run that succeeded.
	Revocations int              `json:"revocations"`
	Buckets     []PressureBucket `json:"buckets,omitempty"`
	// Stages holds one entry per stage of a staged run, in stage order. It is
	// omitted for a run without stages, so such a record keeps its shape.
	Stages []PressureStage `json:"stages,omitempty"`
	// BucketWidth is the fixed width of every time bucket of an unbounded run.
	// It is omitempty and 0 for a bounded run, whose ten buckets divide its
	// duration equally.
	BucketWidth time.Duration `json:"bucketWidth,omitempty"`
}

// PressureWeight is one operation of a pressure run's mix: a worker draws the
// operation with probability Weight divided by the sum of the weights.
type PressureWeight struct {
	Operation string `json:"operation"`
	Weight    int    `json:"weight"`
}

// PressureBucket is one time slice of a pressure run: the operations whose
// start offset fell within it, aggregated per operation, so a latency change
// over the run is visible next to the revocations that had accumulated by
// then. Revocations is cumulative: the revocations that succeeded in this
// bucket and every earlier one.
type PressureBucket struct {
	Start       time.Duration     `json:"start"`
	Operations  metrics.Aggregate `json:"operations"`
	Revocations int               `json:"revocations"`
}

// PressureStage is one stage of a staged pressure run. Index is 1-based.
// FanAssignments is the stage's fan_assignments override, 0 and omitted
// when the stage set none. Start is the offset at which the workers resumed
// after the stage was entered, End the offset at which its window ended.
// Operations are the draws that started in between, shaped like Metrics,
// with one byType entry per operation; its wall is End minus Start.
type PressureStage struct {
	Index          int               `json:"index"`
	FanAssignments int               `json:"fanAssignments,omitempty"`
	Start          time.Duration     `json:"start"`
	End            time.Duration     `json:"end"`
	Operations     metrics.Aggregate `json:"operations"`
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
