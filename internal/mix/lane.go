// Package mix runs the combined chaos mode: several workload personas side by
// side in one churn run, each in a churn engine of its own (a lane) with its
// own config, seed, identity and cloud project, all under one run identifier.
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
// workload it churns. Name is the persona name, RunID the identity every
// resource of the lane carries ("<runID>-<Name>"), Cloud the clouds.yaml entry
// the scenario names for it (empty for the --os-cloud fallback), and ProjectID
// the project it authenticated against, empty when unknown.
//
// Seed, Config and Nodes are what Run hands the engine. Persona is the lane's
// plan persona; it is nil for the cleanup-only lanes of status and cleanup,
// which churn nothing. Collector receives the lane's samples and Telemetry
// exports them; Telemetry is nil when export is disabled.
//
// Cleanup deletes the lane's resources by identity, unioned with the recorded
// ones, and returns how many it deleted. Leaked counts the resources still
// carrying the identity. Observe reports one resource's live state as the
// status table needs it.
type Lane struct {
	Name, Cloud, RunID, ProjectID string
	Seed                          int64
	Config                        chaos.Config
	Nodes                         []chaos.Node
	Persona                       *mixplan.Persona
	Collector                     *metrics.Collector
	Telemetry                     *telemetry.Telemetry
	Cleanup                       func(ctx context.Context, recorded []resource.Resource) (int, error)
	Leaked                        func(ctx context.Context) (int, error)
	Observe                       func(ctx context.Context, r resource.Resource) (string, bool, error)
}
