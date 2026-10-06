package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/chaos/cindergraph"
	"github.com/B42Labs/dizzy/internal/chaos/glancegraph"
	"github.com/B42Labs/dizzy/internal/chaos/keystonegraph"
	"github.com/B42Labs/dizzy/internal/chaos/neutrongraph"
	"github.com/B42Labs/dizzy/internal/cinder"
	cinderexec "github.com/B42Labs/dizzy/internal/cinder/executor"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/executor"
	"github.com/B42Labs/dizzy/internal/glance"
	glanceexec "github.com/B42Labs/dizzy/internal/glance/executor"
	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneexec "github.com/B42Labs/dizzy/internal/keystone/executor"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/mix"
	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	mixscenario "github.com/B42Labs/dizzy/internal/mix/scenario"
	"github.com/B42Labs/dizzy/internal/neutron"
	"github.com/B42Labs/dizzy/internal/nova"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// serviceLaneInput is what buildServiceLane binds a background lane from. name
// is the service the lane churns and runID the lane identity,
// "<runID>-<name>". cloud is the clouds.yaml entry the scenario's lane block
// names, empty for the --os-cloud fallback. project is the project a run
// record says the lane ran in, empty without one; buildServiceLane refuses a
// lane whose cloud authenticates against another project. lane is nil for
// the cleanup-only lanes of status and cleanup, which need neither block nor
// cfg. block is the scenario's lanes block, read only with a plan lane. Each
// lane's client records into a child of overall.
type serviceLaneInput struct {
	name, cloud, runID string
	project            string
	lane               *mixplan.Lane
	block              mixscenario.Lanes
	cfg                chaos.Config
	overall            *metrics.Collector
}

// serviceLaneKind binds a background lane to its service: api names the API
// in errors, client authenticates against it, and bind wraps the service
// client of the lane identity into the lane's handles and, for a plan lane,
// runs the service's read-only pre-checks and builds its churn graph or its
// provisioning step.
type serviceLaneKind struct {
	api    string
	client func(ctx context.Context, cloud string) (*gophercloud.ServiceClient, error)
	bind   func(ctx context.Context, opts *globalOptions, in serviceLaneInput, gc *gophercloud.ServiceClient, l *mix.Lane) error
}

// serviceLaneKinds holds every background lane this build defines, by name.
var serviceLaneKinds = map[string]serviceLaneKind{
	"cinder":   {"block storage", config.NewBlockStorageClient, bindCinderLane},
	"glance":   {"image", config.NewImageClient, bindGlanceLane},
	"keystone": {"identity", config.NewIdentityClient, bindKeystoneLane},
	"neutron":  {"network", config.NewNetworkClient, bindNeutronLane},
}

// buildServiceLane authenticates one background lane against its cloud and
// binds its service client, under the lane identity, to the lane's cleanup,
// leak-check and observe handles, the ones the service's own chaos command
// tears down and checks with. For a plan lane it also sets up the lane's
// telemetry, runs the service's read-only pre-checks and builds its churn
// graph, or for the Keystone lane the step that provisions its scaffold. It
// creates nothing in the cloud. An unknown lane fails before any call.
func buildServiceLane(ctx context.Context, opts *globalOptions, in serviceLaneInput) (_ *mix.Lane, err error) {
	kind, ok := serviceLaneKinds[in.name]
	if !ok {
		return nil, fmt.Errorf("lane %q is not defined by this build of dizzy", in.name)
	}
	cloud := in.cloud
	if cloud == "" {
		cloud = opts.osCloud
	}

	var tel *telemetry.Telemetry
	if in.lane != nil {
		telCloud := in.cloud
		if telCloud == "" {
			telCloud = opts.cloudName()
		}
		tel, err = telemetry.Setup(ctx, telemetry.Config{
			Enabled: opts.otel, Cloud: telCloud, Scenario: in.lane.Scenario(), Service: "mix", Lane: in.name,
		})
		if err != nil {
			return nil, fmt.Errorf("setting up telemetry for lane %q: %w", in.name, err)
		}
		defer func() {
			if err != nil {
				flushTelemetry(tel)
			}
		}()
	}

	gc, err := kind.client(ctx, cloud)
	if err != nil {
		return nil, fmt.Errorf("creating %s client for lane %q: %w", kind.api, in.name, err)
	}
	projectID, _ := nova.ProjectID(gc)
	if err := refuseOtherProject("lane", in.name, projectID, in.project); err != nil {
		return nil, err
	}

	l := &mix.Lane{
		Name:      in.name,
		Service:   in.name,
		Cloud:     in.cloud,
		RunID:     in.runID,
		ProjectID: projectID,
		Collector: in.overall.Child(),
		Telemetry: tel,
	}
	if err := kind.bind(ctx, opts, in, gc, l); err != nil {
		return nil, fmt.Errorf("lane %q: %w", in.name, err)
	}
	if in.lane != nil {
		l.Seed, l.Config, l.Scenario = in.lane.Seed, in.cfg, in.lane.Scenario()
	}
	return l, nil
}

// bindCinderLane binds the Cinder lane as cinder chaos binds its run: for a
// plan lane it resolves the block's volume type, when it names one, and uses
// its resolved name, pre-checks the volume quota against the lane's plan and
// builds the churn graph.
func bindCinderLane(ctx context.Context, opts *globalOptions, in serviceLaneInput, gc *gophercloud.ServiceClient, l *mix.Lane) error {
	client := cinder.New(gc, in.runID, l.Collector)
	client.SetTelemetry(l.Telemetry)
	cleaner := cinderTimeoutCleaner{client, opts.timeout}
	l.Cleanup = func(ctx context.Context, recorded []resource.Resource) (int, error) {
		return cinderexec.Cleanup(ctx, cleaner, in.runID, recorded, opts.timeout)
	}
	l.Leaked = func(ctx context.Context) (int, error) {
		return cinderLeakCheck(ctx, cleaner, in.runID)
	}
	l.Observe = client.Observe
	if in.lane == nil {
		return nil
	}

	p := in.lane.Cinder
	volumeType, err := precheckCinder(ctx, gc, in.block.Cinder.VolumeType, p)
	if err != nil {
		return err
	}
	nodes, err := cindergraph.Build(p, client, volumeType, opts.timeout)
	if err != nil {
		return fmt.Errorf("building churn graph: %w", err)
	}
	l.Nodes = nodes
	return nil
}

// bindGlanceLane binds the Glance lane as glance chaos binds its run: for a
// plan lane it builds the churn graph. Glance has no quota pre-check.
func bindGlanceLane(_ context.Context, opts *globalOptions, in serviceLaneInput, gc *gophercloud.ServiceClient, l *mix.Lane) error {
	client := glance.New(gc, in.runID, l.Collector)
	client.SetTelemetry(l.Telemetry)
	cleaner := glanceTimeoutCleaner{client, opts.timeout}
	l.Cleanup = func(ctx context.Context, recorded []resource.Resource) (int, error) {
		return glanceexec.Cleanup(ctx, cleaner, in.runID, recorded, opts.concurrency, opts.timeout)
	}
	l.Leaked = func(ctx context.Context) (int, error) {
		return glanceLeakCheck(ctx, cleaner, in.runID)
	}
	l.Observe = client.Observe
	if in.lane == nil {
		return nil
	}

	p := in.lane.Glance
	nodes, err := glancegraph.Build(p, client, p.Seed, opts.timeout)
	if err != nil {
		return fmt.Errorf("building churn graph: %w", err)
	}
	l.Nodes = nodes
	return nil
}

// bindNeutronLane binds the Neutron lane as neutron chaos binds its run: for a
// plan lane it looks up the block's external network, or the first one when
// the block names none, pre-checks the network quota against the lane's plan
// and builds the churn graph.
func bindNeutronLane(ctx context.Context, opts *globalOptions, in serviceLaneInput, gc *gophercloud.ServiceClient, l *mix.Lane) error {
	client := neutron.New(gc, in.runID, l.Collector)
	client.SetTelemetry(l.Telemetry)
	cleaner := timeoutCleaner{client, opts.timeout}
	l.Cleanup = func(ctx context.Context, recorded []resource.Resource) (int, error) {
		return executor.Cleanup(ctx, cleaner, in.runID, recorded)
	}
	l.Leaked = func(ctx context.Context) (int, error) {
		return leakCheck(ctx, cleaner, in.runID)
	}
	l.Observe = client.Observe
	if in.lane == nil {
		return nil
	}

	p := in.lane.Neutron
	externalNetworkID, err := precheckNeutron(ctx, gc, in.block.Neutron.ExternalNetwork, p)
	if err != nil {
		return err
	}
	nodes, err := neutrongraph.Build(p, externalNetworkID, client, opts.timeout)
	if err != nil {
		return fmt.Errorf("building churn graph: %w", err)
	}
	l.Nodes = nodes
	return nil
}

// bindKeystoneLane binds the Keystone lane as keystone chaos binds its run:
// for a plan lane it runs the privilege pre-check under the block's privilege,
// auto when empty, domain and roles, member,reader when empty. It builds no
// graph: the graph needs the domain and role scaffold, which keystone chaos
// creates right after its pre-check, so the lane leaves that to its Provision
// step, run once every lane's pre-check has passed.
func bindKeystoneLane(ctx context.Context, opts *globalOptions, in serviceLaneInput, gc *gophercloud.ServiceClient, l *mix.Lane) error {
	client := keystone.New(gc, in.runID, l.Collector)
	client.SetTelemetry(l.Telemetry)
	cleaner := keystoneTimeoutCleaner{client, opts.timeout}
	l.Cleanup = func(ctx context.Context, recorded []resource.Resource) (int, error) {
		return keystoneexec.Cleanup(ctx, cleaner, in.runID, recorded, opts.timeout)
	}
	l.Leaked = func(ctx context.Context) (int, error) {
		return keystoneLeakCheck(ctx, cleaner, in.runID)
	}
	l.Observe = client.Observe
	if in.lane == nil {
		return nil
	}

	p, b := in.lane.Keystone, in.block.Keystone
	priv := keystonePrivilegeFlags{
		privilege: cmp.Or(b.Privilege, "auto"),
		domain:    b.Domain,
		roles:     cmp.Or(b.Roles, defaultKeystoneRoles),
	}
	tier, res, err := resolveKeystonePrivilege(ctx, gc, priv, p)
	if err != nil {
		return err
	}
	l.Provision = keystoneProvision(l,
		func(ctx context.Context) (keystoneexec.Bindings, []resource.Resource, error) {
			return keystoneexec.BindRoots(ctx, client, p, tier, res, opts.concurrency, opts.timeout)
		},
		func(bindings keystoneexec.Bindings) ([]chaos.Node, error) {
			return keystonegraph.Build(p, client, bindings, opts.timeout)
		})
	return nil
}

// keystoneProvision returns the Provision step of the Keystone lane l: it binds
// the scaffold with bind, stores the resources bind created in l.Roots whether
// or not it failed, so a teardown reclaims them, and then builds l.Nodes with
// build from the bindings.
func keystoneProvision(l *mix.Lane,
	bind func(context.Context) (keystoneexec.Bindings, []resource.Resource, error),
	build func(keystoneexec.Bindings) ([]chaos.Node, error)) func(context.Context) error {
	return func(ctx context.Context) error {
		bindings, roots, err := bind(ctx)
		l.Roots = roots
		if err != nil {
			return fmt.Errorf("binding scaffold: %w", err)
		}
		nodes, err := build(bindings)
		if err != nil {
			return fmt.Errorf("building churn graph: %w", err)
		}
		l.Nodes = nodes
		return nil
	}
}

// provisionLanes runs the Provision step of every lane that has one, in lane
// order. On the first failure it provisions no further lane and tears down,
// on a context.WithoutCancel of ctx, the roots of the failing lane and of
// every lane provisioned before it. It returns the failure joined with every
// teardown failure, each naming its lane and identity.
func provisionLanes(ctx context.Context, lanes []*mix.Lane) error {
	var provisioned []*mix.Lane
	for _, l := range lanes {
		if l.Provision == nil {
			continue
		}
		provisioned = append(provisioned, l)
		err := l.Provision(ctx)
		if err == nil {
			continue
		}
		errs := []error{fmt.Errorf("provisioning lane %q (run %s): %w", l.Name, l.RunID, err)}
		tctx := context.WithoutCancel(ctx)
		for _, p := range provisioned {
			if _, err := p.Cleanup(tctx, p.Roots); err != nil {
				errs = append(errs, fmt.Errorf("tearing down lane %q (run %s): %w", p.Name, p.RunID, err))
			}
		}
		return errors.Join(errs...)
	}
	return nil
}

// unsetRatioFlag fills the ratio-flag parameter of the cinder, glance and
// keystone merge functions. mix chaos defines no --resize-ratio,
// --lifecycle-ratio or --token-ratio flag, so the merge never reads it and the
// ratio comes from the lane scenario's chaos block or the service default.
const unsetRatioFlag = 0

// mixServiceLaneConfigs builds the churn config of every background lane of p,
// in plan order. Each lane takes interval, churn ratio, target fill, mutate
// ratio and max parallel from its own scenario's chaos block through the merge
// function of its service's chaos command, which reads only the flags mix
// chaos defines, so --max-parallel overrides the block. It takes duration,
// unbounded mode and bucket width from runCfg, the run's config. Every config
// gets its service graph's classifier and the checkpoint interval and is
// validated.
func mixServiceLaneConfigs(cmd *cobra.Command, opts *globalOptions, ls mixscenario.LaneScenarios, f chaosFlags, p *mixplan.Plan, runCfg chaos.Config) ([]chaos.Config, error) {
	cfgs := make([]chaos.Config, 0, len(p.Lanes))
	for _, l := range p.Lanes {
		var (
			cfg    chaos.Config
			loaded bool
		)
		switch l.Name {
		case "cinder":
			if loaded = ls.Cinder != nil; loaded {
				cfg = mergeCinderChaosConfig(cmd, opts, *ls.Cinder, f, unsetRatioFlag)
				cfg.Classify = cindergraph.Classify
			}
		case "glance":
			if loaded = ls.Glance != nil; loaded {
				cfg = mergeGlanceChaosConfig(cmd, opts, *ls.Glance, f, unsetRatioFlag)
				cfg.Classify = glancegraph.Classify
			}
		case "keystone":
			if loaded = ls.Keystone != nil; loaded {
				cfg = mergeKeystoneChaosConfig(cmd, opts, *ls.Keystone, f, unsetRatioFlag)
				cfg.Classify = keystonegraph.Classify
			}
		case "neutron":
			if loaded = ls.Neutron != nil; loaded {
				cfg = mergeChaosConfig(cmd, opts, *ls.Neutron, f)
				cfg.Classify = neutrongraph.Classify
			}
		default:
			return nil, fmt.Errorf("lane %q is not defined by this build of dizzy", l.Name)
		}
		if !loaded {
			return nil, fmt.Errorf("lane %q has no scenario", l.Name)
		}
		cfg.Duration, cfg.Unbounded, cfg.BucketWidth = runCfg.Duration, runCfg.Unbounded, runCfg.BucketWidth
		cfg.CheckpointInterval = chaosCheckpointInterval
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("lane %q: %w", l.Name, err)
		}
		cfgs = append(cfgs, cfg)
	}
	return cfgs, nil
}

// planServiceLaneInputs returns the input of every background lane the
// scenario enables, in the order of the plan's lanes, under the cloud the
// scenario names for it, empty for the --os-cloud fallback, and the lane
// identity "<runID>-<lane>", with the scenario's lanes block. It reads no lane
// scenario. It sets no plan lane, so the inputs bind cleanup-only lanes; mix
// chaos sets lane and cfg on each itself.
func planServiceLaneInputs(s mixscenario.Scenario, runID string, overall *metrics.Collector) []serviceLaneInput {
	enabled := s.Lanes.Enabled()
	inputs := make([]serviceLaneInput, 0, len(enabled))
	for _, l := range enabled {
		inputs = append(inputs, serviceLaneInput{name: l.Name, cloud: l.Cloud, runID: runID + "-" + l.Name, block: s.Lanes, overall: overall})
	}
	return inputs
}

// recordServiceLaneInputs returns the cleanup-only input of every background
// lane of a mix record, under the cloud, identity and project the record
// names.
func recordServiceLaneInputs(rec *run.Record, overall *metrics.Collector) []serviceLaneInput {
	inputs := make([]serviceLaneInput, 0, len(rec.Lanes))
	for _, l := range rec.Lanes {
		inputs = append(inputs, serviceLaneInput{name: l.Name, cloud: l.Cloud, runID: l.RunID, project: l.ProjectID, overall: overall})
	}
	return inputs
}

// warnLaneUnreclaimable logs, before mix cleanup deletes anything, what the
// background lanes may leave behind: for the Neutron lane the warning neutron
// cleanup logs, for a missing or a checkpoint record, and for the Keystone lane
// without a record the warning keystone cleanup logs, each under the lane
// identity. The other lanes reclaim everything by identity.
func warnLaneUnreclaimable(lanes []*mix.Lane, rec *run.Record) {
	for _, l := range lanes {
		switch l.Service {
		case "neutron":
			warnUnreclaimable(l.RunID, rec)
		case "keystone":
			warnKeystoneUnreclaimable(l.RunID, rec)
		}
	}
}
