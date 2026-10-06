package main

import (
	"context"
	"fmt"
	"log/slog"

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
		newReportCmd(opts),
	)

	return cmd
}

// mixLaneInput is what buildMixLane binds a lane from. runID is the lane
// identity, "<runID>-<name>". cloud is the clouds.yaml entry the scenario names
// for the persona, empty for the --os-cloud fallback. persona is nil for the
// cleanup-only lanes of status and cleanup, which need neither cfg nor
// scenario. Each lane's client records into a child of overall.
type mixLaneInput struct {
	name, cloud, runID, scenario string
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
	projectID, _ := nova.ProjectID(cs.Compute)

	l := &mix.Lane{
		Name:      in.name,
		Cloud:     in.cloud,
		RunID:     in.runID,
		ProjectID: projectID,
		Persona:   in.persona,
		Collector: collector,
		Telemetry: tel,
		Cleanup: func(ctx context.Context, recorded []resource.Resource) (int, error) {
			return novaexec.Cleanup(ctx, cleaner, in.runID, recorded, opts.timeout)
		},
		Leaked: func(ctx context.Context) (int, error) {
			return novaLeakCheck(ctx, cleaner, in.runID)
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
	nodes, err := novagraph.Build(in.persona.Nova, client, resolved, opts.timeout)
	if err != nil {
		return nil, fmt.Errorf("building churn graph for persona %q: %w", in.name, err)
	}
	l.Nodes, l.Seed, l.Config = nodes, in.persona.Seed, in.cfg
	return l, nil
}

// warnSharedProjects logs one warning for every project two or more lanes
// authenticated against: each lane pre-checked the compute quota against its
// own plan only, so together they may exceed it. Their resources stay apart by
// identity, so the run goes on. A lane without a known project is skipped.
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
			slog.Warn("personas share a project; each quota pre-check saw only its own plan", "project", id, "personas", names[id])
		}
	}
}

// mixPersonaCloud returns the clouds.yaml entry the scenario names for the
// persona, empty for the --os-cloud fallback.
func mixPersonaCloud(s mixscenario.Scenario, name string) (string, error) {
	switch name {
	case "ci":
		return s.Personas.CI.Cloud, nil
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

// resourcesOfPersona returns the entries of a mix record's created list that
// the named persona created.
func resourcesOfPersona(created []resource.Resource, name string) []resource.Resource {
	var out []resource.Resource
	for _, r := range created {
		if r.Persona == name {
			out = append(out, r)
		}
	}
	return out
}
