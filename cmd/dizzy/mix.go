package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/chaos/novagraph"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/mix"
	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	mixscenario "github.com/B42Labs/dizzy/internal/mix/scenario"
	"github.com/B42Labs/dizzy/internal/nova"
	novaexec "github.com/B42Labs/dizzy/internal/nova/executor"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// newMixCmd builds the "mix" command namespace, the combined chaos mode: it
// runs several workload personas side by side, each in a churn engine and a
// project of its own, under one seed and one run identifier. There is no apply
// and no monitor, because a persona is a behavior over time. report is the
// same service-agnostic builder the other namespaces use.
func newMixCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mix",
		Short: "Combined multi-persona churn across services",
	}

	cmd.AddCommand(
		newMixGenerateCmd(opts),
		newMixChaosCmd(opts),
		newMixStatusCmd(opts),
		newReportCmd(opts),
		newMixCleanupCmd(opts),
	)

	return cmd
}

// mixLaneInput is what buildMixLane binds a lane from. runID is the lane
// identity, "<runID>-<name>". cloud is the clouds.yaml entry the scenario names
// for the persona, empty for the --os-cloud fallback. project is the project a
// run record says the persona ran in, empty without one; buildMixLane refuses
// a lane whose cloud authenticates against another project. persona is nil for
// the cleanup-only lanes of status and cleanup, which need neither cfg nor
// scenario. Each lane's client records into a child of overall.
type mixLaneInput struct {
	name, cloud, runID, scenario string
	project                      string
	persona                      *mixplan.Persona
	cfg                          chaos.Config
	overall                      *metrics.Collector
}

// buildMixLane authenticates one lane against its cloud and binds its compute
// client, under the lane identity, to the lane's cleanup, leak-check and
// observe handles. For a persona lane it also sets up the persona's telemetry,
// resolves image and flavor and pre-checks the compute quota of the persona's
// project against the persona's plan, and builds its churn graph. It creates
// nothing in the cloud.
func buildMixLane(ctx context.Context, opts *globalOptions, in mixLaneInput) (_ *mix.Lane, err error) {
	cloud := in.cloud
	if cloud == "" {
		cloud = opts.osCloud
	}

	var tel *telemetry.Telemetry
	if in.persona != nil {
		telCloud := in.cloud
		if telCloud == "" {
			telCloud = opts.cloudName()
		}
		tel, err = telemetry.Setup(ctx, telemetry.Config{
			Enabled: opts.otel, Cloud: telCloud, Scenario: in.scenario, Service: "mix", Persona: in.name,
		})
		if err != nil {
			return nil, fmt.Errorf("setting up telemetry for persona %q: %w", in.name, err)
		}
		defer func() {
			if err != nil {
				flushTelemetry(tel)
			}
		}()
	}

	cs, err := config.NewComputeStack(ctx, cloud)
	if err != nil {
		return nil, fmt.Errorf("creating compute clients for persona %q: %w", in.name, err)
	}

	collector := in.overall.Child()
	client := nova.New(cs.Compute, cs.Network, cs.BlockStorage, in.runID, collector)
	client.SetTelemetry(tel)
	cleaner := novaTimeoutCleaner{client, opts.timeout}
	groups := serverGroupTimeoutCleaner{client, opts.timeout}
	projectID, _ := nova.ProjectID(cs.Compute)
	if err := refuseOtherProject("persona", in.name, projectID, in.project); err != nil {
		return nil, err
	}

	l := &mix.Lane{
		Name:      in.name,
		Cloud:     in.cloud,
		RunID:     in.runID,
		ProjectID: projectID,
		Persona:   in.persona,
		Collector: collector,
		Telemetry: tel,
		Cleanup: func(ctx context.Context, recorded []resource.Resource) (int, error) {
			return laneCleanup(ctx, cleaner, groups, in.runID, recorded, opts.timeout)
		},
		Leaked: func(ctx context.Context) (int, error) {
			return laneLeaked(ctx, cleaner, groups, in.runID)
		},
		Observe: client.Observe,
	}
	if in.persona == nil {
		return l, nil
	}

	resolved, err := resolveNovaRefs(ctx, cs, in.persona.Nova)
	if err != nil {
		return nil, fmt.Errorf("persona %q: %w", in.name, err)
	}
	nodes, err := buildPersonaNodes(in.persona, client, resolved, opts.timeout)
	if err != nil {
		return nil, fmt.Errorf("building churn graph for persona %q: %w", in.name, err)
	}
	l.Nodes, l.Seed, l.Config = nodes, in.persona.Seed, in.cfg
	return l, nil
}

// refuseOtherProject returns an error when a lane authenticated against
// project got although the run record says it ran in project recorded, and
// nil when either is unknown. Discovery is project-scoped, so in another
// project it finds nothing and the recorded ids are 404s that count as
// deleted. noun names the kind of lane, as laneNoun does.
func refuseOtherProject(noun, name, got, recorded string) error {
	if recorded == "" || got == "" || got == recorded {
		return nil
	}
	return fmt.Errorf("%s %q authenticated against project %s, but the run record says it ran in project %s; authenticate with the cloud the run used",
		noun, name, got, recorded)
}

// laneNoun names the kind of a lane in messages: "lane" for a background lane
// and "persona" for a persona's lane.
func laneNoun(l *mix.Lane) string {
	if l.Background() {
		return "lane"
	}
	return "persona"
}

// laneCleanup tears down one lane under its identity: it runs novaexec.Cleanup
// and then deletes the lane's server groups, even when that failed, since Nova
// deletes a group whatever its members and a group left behind holds a unit of
// the server_groups quota. It returns the sum of both counts and both errors
// joined.
func laneCleanup(ctx context.Context, c novaexec.Cleaner, g novaexec.ServerGroupCleaner, runID string, recorded []resource.Resource, opTimeout time.Duration) (int, error) {
	deleted, err := novaexec.Cleanup(ctx, c, runID, recorded, opTimeout)
	groups, gerr := novaexec.CleanupServerGroups(ctx, g, runID, recorded)
	return deleted + groups, errors.Join(err, gerr)
}

// laneLeaked counts what is left of one lane after teardown: the resources
// novaLeakCheck finds by the lane identity and the server groups found by its
// name prefix. A failing novaLeakCheck is returned unchanged.
func laneLeaked(ctx context.Context, c novaexec.Cleaner, g novaexec.ServerGroupCleaner, runID string) (int, error) {
	leaked, err := novaLeakCheck(ctx, c, runID)
	if err != nil {
		return leaked, err
	}
	groups, err := g.ListServerGroupsByName(ctx, runID)
	if err != nil {
		return leaked, fmt.Errorf("leak check listing server groups: %w", err)
	}
	return leaked + len(groups), nil
}

// serverGroupTimeoutCleaner bounds every call of a novaexec.ServerGroupCleaner
// by opTimeout, as novaTimeoutCleaner bounds the calls of Cleanup.
type serverGroupTimeoutCleaner struct {
	inner     novaexec.ServerGroupCleaner
	opTimeout time.Duration
}

func (t serverGroupTimeoutCleaner) ListServerGroupsByName(ctx context.Context, runID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListServerGroupsByName(ctx, runID)
}

func (t serverGroupTimeoutCleaner) Delete(ctx context.Context, r resource.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.Delete(ctx, r)
}

// buildPersonaNodes builds the churn graph of a persona's lane: the rolling
// graph, whose clusters stay whole and replace their workers one at a time, for
// a rolling persona, the long-lived graph, whose nodes stay until teardown and
// are mutated repeatedly, for a long-lived persona, and the graph nova chaos
// churns otherwise.
func buildPersonaNodes(ps *mixplan.Persona, c novagraph.Nova, r novaexec.Resolved, opTimeout time.Duration) ([]chaos.Node, error) {
	if ps.Rolling {
		return novagraph.BuildRolling(ps.Nova, c, r, opTimeout)
	}
	if ps.LongLived {
		return novagraph.BuildLongLived(ps.Nova, c, r, opTimeout)
	}
	return novagraph.Build(ps.Nova, c, r, opTimeout)
}

// warnSharedProjects logs one warning for every project two or more lanes,
// personas' or background ones, authenticated against: each lane pre-checked
// the quota against its own plan only, so together they may exceed it. Their
// resources stay apart by identity, so the run goes on. A lane without a known
// project is skipped.
func warnSharedProjects(lanes []*mix.Lane) {
	var projects []string
	names := make(map[string][]string)
	for _, l := range lanes {
		if l.ProjectID == "" {
			continue
		}
		if _, ok := names[l.ProjectID]; !ok {
			projects = append(projects, l.ProjectID)
		}
		names[l.ProjectID] = append(names[l.ProjectID], l.Name)
	}
	for _, id := range projects {
		if len(names[id]) > 1 {
			slog.Warn("lanes share a project; each quota pre-check saw only its own plan", "project", id, "lanes", names[id])
		}
	}
}

// mixPersonaCloud returns the clouds.yaml entry the scenario names for the
// persona, empty for the --os-cloud fallback.
func mixPersonaCloud(s mixscenario.Scenario, name string) (string, error) {
	switch name {
	case "ci":
		return s.Personas.CI.Cloud, nil
	case "gardener":
		return s.Personas.Gardener.Cloud, nil
	case "legacy":
		return s.Personas.Legacy.Cloud, nil
	default:
		return "", fmt.Errorf("persona %q is not defined by this build of dizzy", name)
	}
}

// planLaneInputs returns the lane input of every persona of p, under the cloud
// the scenario names for it and the lane identity "<runID>-<persona>", which is
// built here only. It sets no persona, so the inputs bind cleanup-only lanes;
// mix chaos sets persona, scenario and cfg on each itself.
func planLaneInputs(s mixscenario.Scenario, p *mixplan.Plan, runID string, overall *metrics.Collector) ([]mixLaneInput, error) {
	inputs := make([]mixLaneInput, 0, len(p.Personas))
	for _, ps := range p.Personas {
		cloud, err := mixPersonaCloud(s, ps.Name)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, mixLaneInput{name: ps.Name, cloud: cloud, runID: runID + "-" + ps.Name, overall: overall})
	}
	return inputs, nil
}

// resourcesOfLane returns the entries of a mix record's created list that the
// lane created: those whose lane is its name for a background lane, and those
// whose persona is its name for a persona's lane.
func resourcesOfLane(created []resource.Resource, l *mix.Lane) []resource.Resource {
	var out []resource.Resource
	for _, r := range created {
		owner := r.Persona
		if l.Background() {
			owner = r.Lane
		}
		if owner == l.Name {
			out = append(out, r)
		}
	}
	return out
}

// recordLaneInputs returns the cleanup-only lane input of every persona of a
// mix record, under the cloud, identity and project the record names.
func recordLaneInputs(rec *run.Record, overall *metrics.Collector) []mixLaneInput {
	inputs := make([]mixLaneInput, 0, len(rec.Personas))
	for _, ps := range rec.Personas {
		inputs = append(inputs, mixLaneInput{name: ps.Name, cloud: ps.Cloud, runID: ps.RunID, project: ps.ProjectID, overall: overall})
	}
	return inputs
}

// buildCleanupLanes builds one cleanup-only lane per input, the personas
// first and then the background lanes.
func buildCleanupLanes(ctx context.Context, opts *globalOptions, personas []mixLaneInput, services []serviceLaneInput) ([]*mix.Lane, error) {
	lanes := make([]*mix.Lane, 0, len(personas)+len(services))
	for _, in := range personas {
		l, err := buildMixLane(ctx, opts, in)
		if err != nil {
			return nil, err
		}
		lanes = append(lanes, l)
	}
	for _, in := range services {
		l, err := buildServiceLane(ctx, opts, in)
		if err != nil {
			return nil, err
		}
		lanes = append(lanes, l)
	}
	return lanes, nil
}

// deleteLaneResources runs every lane's Cleanup with the entries of created
// the lane made, in lane order, and prints how many each deleted under its
// identity. A failing lane does not stop the others; their errors come back
// joined, each naming the persona or lane after action ("tearing down",
// "cleaning up").
func deleteLaneResources(ctx context.Context, out io.Writer, lanes []*mix.Lane, created []resource.Resource, action string) error {
	var errs []error
	for _, l := range lanes {
		deleted, err := l.Cleanup(ctx, resourcesOfLane(created, l))
		if _, werr := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, l.RunID); werr != nil {
			return fmt.Errorf("writing output: %w", werr)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s %q (run %s): %w", action, laneNoun(l), l.Name, l.RunID, err))
		}
	}
	return errors.Join(errs...)
}

// observeFunc adapts a lane's Observe handle to the observer the status table
// drives.
type observeFunc func(ctx context.Context, r resource.Resource) (string, bool, error)

// Observe reports r's live state through f.
func (f observeFunc) Observe(ctx context.Context, r resource.Resource) (string, bool, error) {
	return f(ctx, r)
}
