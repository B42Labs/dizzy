// Package mix runs the combined chaos mode: several workload personas side by
// side in one churn run, each in a churn engine of its own (a lane) with its
// own config, seed, identity and cloud project, all under one run identifier.
// Background lanes run the churn of a single service next to them in the same
// frame.
package mix

import (
	"context"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/metrics"
	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// Lane is one churn engine of a combined run and the cloud handles of the
// workload it churns: a persona's, or the churn of a single service for a
// background lane. Name is the persona or lane name, RunID the identity every
// resource of the lane carries ("<runID>-<Name>"), Cloud the clouds.yaml entry
// the scenario names for it (empty for the --os-cloud fallback), and ProjectID
// the project it authenticated against, empty when unknown. Service is the
// service a background lane churns, equal to its Name, and empty for a
// persona's lane.
//
// Seed, Config and Nodes are what Run hands the engine. Persona is the lane's
// plan persona; it is nil for a background lane and for the cleanup-only lanes
// of status and cleanup, which churn nothing. Scenario is the scenario name of
// a background lane's plan, for the record. Collector receives the lane's
// samples and Telemetry exports them; Telemetry is nil when export is
// disabled.
//
// Provision, nil unless the lane must create something before its engine
// starts, creates that, records it in Roots and builds Nodes. Roots are what
// the lane created before its engine started; they join the record and the
// teardown.
//
// Cleanup deletes the lane's resources by identity, unioned with the recorded
// ones, and returns how many it deleted. Leaked counts the resources still
// carrying the identity. Observe reports one resource's live state as the
// status table needs it.
type Lane struct {
	Name, Cloud, RunID, ProjectID string
	Service, Scenario             string
	Seed                          int64
	Config                        chaos.Config
	Nodes                         []chaos.Node
	Persona                       *mixplan.Persona
	Roots                         []resource.Resource
	Provision                     func(ctx context.Context) error
	Collector                     *metrics.Collector
	Telemetry                     *telemetry.Telemetry
	Cleanup                       func(ctx context.Context, recorded []resource.Resource) (int, error)
	Leaked                        func(ctx context.Context) (int, error)
	Observe                       func(ctx context.Context, r resource.Resource) (string, bool, error)
}

// Background reports whether l is a background lane rather than a persona's.
func (l *Lane) Background() bool { return l.Service != "" }
