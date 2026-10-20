package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneexec "github.com/B42Labs/dizzy/internal/keystone/executor"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	keystonescenario "github.com/B42Labs/dizzy/internal/keystone/scenario"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/pressure"
	"github.com/B42Labs/dizzy/internal/pressure/keystoneops"
	"github.com/B42Labs/dizzy/internal/pressure/probe"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// Built-in defaults for the keystone pressure knobs, used when neither the
// scenario's pressure block nor a flag supplies a value. As for chaos there is
// no default duration: the block or --duration must set it. The bucket width
// default is the chaos one, defaultChaosBucketWidth. The propagation probe is
// off by default; defaultPropagationTimeout is its ceiling once an interval
// switches it on.
const (
	defaultPressureWorkers    = 16
	defaultPressureTokens     = 100
	defaultPressureChurnUsers = 1
	defaultPropagationTimeout = 10 * time.Minute
)

// defaultPressureMix returns the built-in operation weights: mostly token
// validations, the request the CobaltCore patches speed up, with a trickle of
// effective listings, revocations, grant churn and issues. probe and bad_token
// are listed with weight 0, so every mix key has a default and neither sends
// a request to another service unless the scenario or --mix gives it a
// weight. It returns a fresh map on every call, so a caller may change it.
func defaultPressureMix() map[string]int {
	return map[string]int{
		keystoneops.OpValidate:   60,
		keystoneops.OpEffective:  5,
		keystoneops.OpRevoke:     2,
		keystoneops.OpGrantChurn: 2,
		keystoneops.OpIssue:      1,
		keystoneops.OpProbe:      0,
		keystoneops.OpBadToken:   0,
	}
}

// pressureFlags holds the dedicated keystone pressure flag values; whether
// each one overrides the scenario block is decided by cmd.Flags().Changed.
type pressureFlags struct {
	duration           time.Duration
	bucketWidth        time.Duration
	workers            int
	tokens             int
	churnUsers         int
	mix                []string
	propagationEvery   time.Duration
	propagationTimeout time.Duration
}

// newKeystonePressureCmd builds "keystone pressure": a closed-loop load on
// Keystone's token validation, revocation and role-listing path and, through
// probe, bad_token and the propagation probe, on the token validation inside
// Nova, Neutron, Cinder and Glance. It provisions the plan's domains, roles,
// projects, users and role assignments, primes a pool of project-scoped
// tokens, and runs a fixed number of workers that each send one weighted
// operation after another for the configured duration, or until it is
// stopped (--duration 0). It rewrites its run record every
// chaosCheckpointInterval while the workers run, prints the metrics summary
// and the Pressure table, records the run, and, unless --no-cleanup, tears
// the resources down and reports any leak, as keystone chaos does. Only the
// admin tier runs, and everything the engine would refuse is rejected before
// the first cloud call.
func newKeystonePressureCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		sets         []string
		noCleanup    bool
		f            pressureFlags
		priv         keystonePrivilegeFlags
	)

	cmd := &cobra.Command{
		Use:   "pressure",
		Short: "Run closed-loop token validation, revocation and role-listing load against Keystone",
		Long: `Run a closed-loop load against Keystone's token validation, revocation and
role-listing path. The command provisions the scenario's domains, roles,
projects, users and role assignments, primes a pool of project-scoped tokens,
and runs a fixed number of workers that each send one request after another,
drawn by weight from validate, effective, revoke, grant_churn, issue, probe
and bad_token, for the configured duration or, with --duration 0, until it is
interrupted. It prints the metrics summary and the Pressure table, writes
run-<id>.json, and tears its resources down unless --no-cleanup is given.
Only the admin tier runs: validating and revoking other users' tokens and
listing their role assignments are admin operations.

probe sends a pool token to Nova, Neutron, Cinder or Glance, as far as the
token's catalog lists them, for the cheapest read the service offers.
bad_token, off by default, floods a service with tokens it has to reject and
exists to load the services' rejection path. The propagation probe
(--propagation-every) revokes a pool token and measures how long a service
keeps accepting it. All three, like revoke, belong on a lab, never on a
shared cloud.

Run it on a lab, never on a shared cloud. Every revoke is a real revocation
event, and on Keystone 2026.2 each one flushes the token validation cache for
every user of that Keystone, not only for the run's own users.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Everything up to cfg.Validate makes no cloud call, so input this
			// build cannot run fails before any write.
			s, p, err := buildKeystonePlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			cfg, o, stages, err := mergePressureConfig(cmd, opts, s, f)
			if err != nil {
				return err
			}
			switch priv.privilege {
			case "auto", "admin":
			case "domain-manager":
				return requirePressureAdminTier(keystone.TierDomainManager)
			default:
				return fmt.Errorf("invalid --privilege %q: want auto, admin, or domain-manager", priv.privilege)
			}
			if err := checkPressureMix(o.Weights); err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return err
			}

			// Two-phase shutdown, as in keystone chaos: the first signal ends the
			// run, and unregistering the handler right after lets a second signal
			// kill the process mid-teardown.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()

			tel, err := telemetry.Setup(ctx, telemetry.Config{
				Enabled: opts.otel, Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "keystone",
			})
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)

			runID, err := newRunID()
			if err != nil {
				return err
			}

			stack, err := config.NewIdentityStack(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating identity client: %w", err)
			}
			gc := stack.Identity
			// The workers and the token refresher, whose reissues run next
			// to them with up to --concurrency in flight, all go to the one
			// Keystone endpoint, as provisioning and priming do before them.
			// The probes share the identity client's transport: its per-host
			// cap applies to each service host too, while its total cap is
			// shared by Keystone and the services.
			sizeConnectionPool(gc, cfg.Workers+opts.concurrency)

			if _, err := resolvePressureTier(ctx, gc, priv); err != nil {
				return err
			}

			collector := metrics.NewCollector()
			client, svc := newPressureClients(gc, stack.EndpointOpts, runID, collector, tel)
			return runKeystonePressure(ctx, cmd, client, pressureRun{
				runID:       runID,
				plan:        p,
				cfg:         cfg,
				stages:      stages,
				o:           o,
				concurrency: opts.concurrency,
				opTimeout:   opts.timeout,
				noCleanup:   noCleanup,
				collector:   collector,
				tel:         tel,
				services:    svc,
			})
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.users=20 (repeatable)")
	flags.DurationVar(&f.duration, "duration", 0, "total wall-clock runtime of the workers; 0 runs until interrupted (required via flag or the scenario pressure block)")
	flags.DurationVar(&f.bucketWidth, "bucket-width", defaultChaosBucketWidth, "width of one time bucket in an unbounded run's time series, at least 1m")
	flags.IntVar(&f.workers, "workers", 0, fmt.Sprintf("closed-loop workers, 1 to 1024; 0 takes the scenario pressure block's value or %d", defaultPressureWorkers))
	flags.IntVar(&f.tokens, "tokens", 0, fmt.Sprintf("size of the token pool; 0 takes the scenario pressure block's value or %d", defaultPressureTokens))
	flags.IntVar(&f.churnUsers, "churn-users", defaultPressureChurnUsers, "users reserved for grant_churn, taken from the end of the plan's regular users, never a fan user; they hold no pool token")
	flags.StringArrayVar(&f.mix, "mix", nil, "weight of one operation as <operation>=<weight>, e.g. --mix revoke=0; operations: "+strings.Join(keystoneops.OperationNames, ", ")+" (repeatable)")
	flags.DurationVar(&f.propagationEvery, "propagation-every", 0, "revoke one pool token at this interval and poll a service with it until it answers 401; 0 switches the propagation probe off, also when given as a flag over a scenario block")
	flags.DurationVar(&f.propagationTimeout, "propagation-timeout", defaultPropagationTimeout, "ceiling of one propagation probe; a probe still accepted after it counts as a timeout")
	flags.BoolVar(&noCleanup, "no-cleanup", false, "leave the identity resources in place — at the end of the run, on interrupt or after a failed setup — instead of tearing them down by name prefix")
	priv.register(flags)
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// pressureKeystone is what keystone pressure asks of Keystone once the caller
// passed the privilege pre-check: the executor's provisioning seam, the
// operations' seam, the teardown seam and the lookup of the cloud's reader
// role. The production *keystone.Client satisfies it; a test fake stands in
// for it to drive a run end to end without a cloud.
type pressureKeystone interface {
	keystoneexec.Keystone
	keystoneops.Keystone
	keystoneexec.Cleaner
	FindRoleByName(ctx context.Context, name string) (string, error)
}

// pressureReaderRole is the role a run that sends live pool tokens to the
// other services grants the pool users on their projects. The plan's roles
// are the run's own and imply no other, while a service's read policy may
// ask for reader on the token's project, as Glance's get_images does under
// its default policy, and would refuse every probe with a 403 without it.
const pressureReaderRole = "reader"

// The production *keystone.Client must satisfy the seam.
var _ pressureKeystone = (*keystone.Client)(nil)

// newPressureClients builds the two clients a pressure run sends through, both
// over gc and both recording into collector and tel: the Keystone client,
// which names the run's resources after runID and reads every pool token's
// endpoints from the token's own catalog, selected with eo, the region and
// interface of the clouds.yaml entry; and the probe client, with which probe,
// bad_token and the propagation probe send those tokens to the other
// services. The probe client shares gc's HTTP client, and with it the
// transport sizeConnectionPool sized. A nil tel is the disabled export.
func newPressureClients(gc *gophercloud.ServiceClient, eo gophercloud.EndpointOpts, runID string, collector *metrics.Collector, tel *telemetry.Telemetry) (*keystone.Client, *probe.Client) {
	client := keystone.New(gc, runID, collector)
	client.SetEndpointOpts(eo)
	client.SetTelemetry(tel)
	svc := probe.New(gc, collector)
	svc.SetTelemetry(tel)
	return client, svc
}

// pressureRun carries what runKeystonePressure needs besides Keystone: the run
// id, the plan, the engine config, the stages' fan_assignments targets and the
// operation options that passed every pre-cloud check, the global concurrency
// and per-operation timeout, --no-cleanup, the collector and telemetry the
// Keystone client and the services record into, and the services seam probe,
// bad_token and the propagation probe send through. cfg and stages come as
// mergePressureConfig returns them: cfg without the seed, the classifier, the
// checkpoint and the stages' Enter, which runKeystonePressure sets, and stages
// with one target per stage of cfg, 0 for a stage without one; o comes
// without the seed, which runKeystonePressure sets too. A nil tel is the
// disabled export. A nil services is accepted only for a run that sends
// nothing to the other services, as keystoneops.Build accepts it.
type pressureRun struct {
	runID       string
	plan        *keystoneplan.Plan
	cfg         pressure.Config
	stages      []int
	o           keystoneops.Options
	concurrency int
	opTimeout   time.Duration
	noCleanup   bool
	collector   *metrics.Collector
	tel         *telemetry.Telemetry
	services    keystoneops.Services
}

// runKeystonePressure is keystone pressure from its first write on, once
// every pre-cloud check and the privilege pre-check passed. It provisions the
// plan in the admin tier, builds the operation set over r.services, with the
// propagation probe drawing from the plan's seed, grants the pool users the
// reader role for a run that sends live pool tokens to the services
// (grantPoolReader), the grants joining the created resources, primes the
// token pool and logs the services the first pool token's catalog lists for
// a run that sends to them, runs the workers from the plan's seed, with
// keystoneops.Classify labeling their errors and a run-record checkpoint
// every chaosCheckpointInterval, prints the metrics summary and the Pressure
// table, writes the run record, and tears down through
// finishKeystoneTeardown. In a staged run, entering a stage with a
// fan_assignments target grows every fan user to it through
// keystoneops.Set.GrowFan while no operation is in flight, and the grants
// join the run's created resources. A failure before the workers finish, a
// failed stage entry included, goes through failPressureSetup, which writes a
// record whose error names the failure and tears down what was created so
// far. It is split from the command so a test can drive it with a fake
// Keystone and fake services; it prints to cmd's output.
func runKeystonePressure(ctx context.Context, cmd *cobra.Command, c pressureKeystone, r pressureRun) error {
	p, cfg, o := r.plan, r.cfg, r.o
	out := cmd.OutOrStdout()
	start := time.Now()

	// setupFailed writes a record of the failed setup with the resources
	// created so far, then tears them down unless --no-cleanup.
	setupFailed := func(created []resource.Resource, what string, cause error) error {
		writeRecord := func() (string, error) {
			finished := time.Now()
			return writeAbortedRunRecord(out, &run.Record{
				RunID:      r.runID,
				Service:    "keystone",
				Scenario:   p.Scenario,
				Seed:       p.Seed,
				StartedAt:  start,
				FinishedAt: finished,
				Created:    created,
				Metrics:    r.collector.Aggregate(finished.Sub(start)),
				Error:      cause.Error(),
			})
		}
		return failPressureSetup(ctx, out, c, r.runID, created, r.noCleanup, r.opTimeout, what, cause, writeRecord)
	}

	slog.Info("provisioning plan", "run", r.runID, "scenario", p.Scenario, "tier", keystone.TierAdmin,
		"domains", len(p.Domains), "roles", len(p.Roles), "projects", len(p.Projects),
		"users", len(p.Users), "assignments", len(p.Assignments), "concurrency", r.concurrency)
	prov, err := keystoneexec.Provision(ctx, c, p, keystone.TierAdmin, keystone.Resolution{}, r.concurrency, r.opTimeout)
	if err != nil {
		return setupFailed(prov.Created, "provisioning plan", err)
	}

	// The propagation probe draws the tokens it revokes from the plan's seed.
	// Which token a draw picks also depends on the pool as the workers left
	// it, so the seed reproduces the workers' operations but not the probe's
	// tokens.
	o.Seed = p.Seed
	set, err := keystoneops.Build(p, c, r.services, prov.Bindings, prov.IDs, prov.Passwords, o)
	if err != nil {
		return setupFailed(prov.Created, "building pressure set", err)
	}
	// Close is idempotent; the explicit calls below stop the refresher and the
	// propagation probe before the teardown, this one covers any other way
	// out.
	defer set.Close()

	grants, err := grantPoolReader(ctx, c, set, o, r.concurrency)
	provisioned := slices.Concat(prov.Created, grants)
	if err != nil {
		return setupFailed(provisioned, "granting reader role", err)
	}

	slog.Info("priming token pool", "tokens", o.Tokens, "concurrency", r.concurrency)
	if err := set.Prime(ctx, r.concurrency); err != nil {
		set.Close()
		return setupFailed(provisioned, "starting pressure run", err)
	}
	if services := set.Services(); len(services) > 0 {
		slog.Info("probe services", "services", services)
	}

	// created lists every resource the run has created: the provisioned ones
	// with the reader grants and, from here on, the grants of the stage
	// entries. A stage entry appends to it while a checkpoint may read it, so
	// createdMu guards it, and createdSoFar returns a copy.
	var createdMu sync.Mutex
	created := provisioned
	createdSoFar := func() []resource.Resource {
		createdMu.Lock()
		defer createdMu.Unlock()
		return slices.Clone(created)
	}

	// buildRecord builds the run record as of finished, for the checkpoints
	// written while the workers run and for the final record. The probe and
	// bad-token samples are aggregated over the engine's wall,
	// res.Operations.Wall, not over finished minus start, which takes in the
	// provisioning and the priming, so the OPS/S of the Probes and Bad tokens
	// tables divides by the time the Operations table's does. In the final
	// record a service's rows add up to the operation's, but for the draws
	// that failed before any request, on an empty pool or a catalog without
	// the four services, which only the operation counts. A checkpoint reads
	// the set after the engine's snapshot while the workers keep drawing, so
	// its service rows may be a few draws ahead of the operation's.
	buildRecord := func(res *pressure.Result, finished time.Time) *run.Record {
		return &run.Record{
			RunID:      r.runID,
			Service:    "keystone",
			Scenario:   p.Scenario,
			Seed:       p.Seed,
			StartedAt:  start,
			FinishedAt: finished,
			Created:    createdSoFar(),
			Metrics:    r.collector.Aggregate(finished.Sub(start)),
			Pressure:   pressureStats(res, o, r.stages, set.ServiceStats(res.Operations.Wall)),
		}
	}
	// The engine settings that make the run this command's run are set here,
	// not by the caller, so the run a test drives through this function is the
	// run the command makes: the workers draw from the plan's seed, so a
	// scenario's seed reproduces their operation sequence; errors carry
	// keystoneops' labels rather than the engine's canceled/timeout/other; and
	// the record is rewritten every chaosCheckpointInterval.
	cfg.Seed = p.Seed
	cfg.Classify = keystoneops.Classify
	cfg.CheckpointInterval = chaosCheckpointInterval
	cfg.OnCheckpoint = chaosCheckpoint(".", buildRecord)

	// The engine enters each stage while no operation is in flight. A stage
	// with a fan_assignments target grows every fan user to it, and its
	// grants join created, those of a failed entry included, so the record
	// lists them and the teardown removes them. cfg.Stages is cloned first,
	// so setting Enter leaves the caller's config as it was.
	cfg.Stages = slices.Clone(cfg.Stages)
	for i := range cfg.Stages {
		target := stageTarget(r.stages, i)
		cfg.Stages[i].Enter = func(ctx context.Context) error {
			if target == 0 {
				slog.Info("entering stage", "run", r.runID, "stage", i+1)
				return nil
			}
			began := time.Now()
			res, err := set.GrowFan(ctx, target, r.concurrency)
			createdMu.Lock()
			created = append(created, res...)
			createdMu.Unlock()
			slog.Info("entering stage", "run", r.runID, "stage", i+1, "fanAssignments", target,
				"granted", len(res), "took", time.Since(began))
			return err
		}
	}

	duration := pressureDurationLabel(cfg)
	slog.Info("starting pressure run", "run", r.runID, "scenario", p.Scenario, "duration", duration,
		"workers", cfg.Workers, "tokens", o.Tokens, "churnUsers", pressureChurnUsers(o),
		"mix", pressureMixLabel(o.Weights), "propagation", pressurePropagationLabel(o), "stages", len(cfg.Stages),
		"concurrency", r.concurrency)

	hb := startHeartbeat(ctx, "pressure in progress", collectorSnapshot(r.collector, start, "duration", duration))
	result, runErr := pressure.Run(ctx, set.Operations(), cfg)
	hb.stop()
	set.Close()
	finished := time.Now()
	if runErr != nil {
		// Run fails on a config or operation list the checks before the first
		// cloud call should have rejected, and on a stage entry that failed;
		// everything created so far still goes, that entry's grants included.
		return setupFailed(createdSoFar(), "running pressure", runErr)
	}
	rec := buildRecord(result, finished)

	interrupted := ctx.Err() != nil && !cfg.Unbounded
	r.tel.RecordIteration(ctx, rec.Metrics.Wall, !interrupted)
	r.tel.RecordIterationOperations(ctx, rec.Metrics.Overall.Attempted, rec.Metrics.Overall.Succeeded, rec.Metrics.Overall.Failed)

	if _, err := fmt.Fprint(out, rec.Metrics.Summary()); err != nil {
		return fmt.Errorf("writing metrics: %w", err)
	}
	if err := run.WritePressureTable(out, rec.Pressure); err != nil {
		return fmt.Errorf("writing pressure table: %w", err)
	}

	recordPath, werr := run.Write(".", rec)
	if werr != nil {
		slog.Error("writing run record failed; clean up by run id", "run", r.runID, "error", werr)
	} else if _, err := fmt.Fprintf(out, "run record written to %s\n", recordPath); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	return finishKeystoneTeardown(ctx, "pressure", cmd, c, r.runID, recordPath, rec.Created, interrupted, r.noCleanup, r.opTimeout)
}

// grantPoolReader grants pressureReaderRole to every pool pair of set, before
// the pool is primed, when the run sends live pool tokens to the other
// services: a positive probe weight or the propagation probe. bad_token
// needs no role, since a service refuses its tokens before any policy check.
// It returns the grants, those made before a failure included, and the
// failure. The role is looked up by name outside the run's metrics, through
// keystoneexec.WithRetry bounded by o.OpTimeout as every setup call is, so a
// transient failure on a busy Keystone does not cost the run its grants;
// when the lookup still fails, as on a cloud without a reader role, the run
// goes on without the grants and logs a warning, since the probes still
// reach every service whose read policy does not ask for the role.
func grantPoolReader(ctx context.Context, c pressureKeystone, set *keystoneops.Set, o keystoneops.Options, concurrency int) ([]resource.Resource, error) {
	if o.Weights[keystoneops.OpProbe] <= 0 && o.Propagation.Every <= 0 {
		return nil, nil
	}
	var roleID string
	err := keystoneexec.WithRetry(ctx, o.OpTimeout, func(ctx context.Context) error {
		id, err := c.FindRoleByName(ctx, pressureReaderRole)
		roleID = id
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			// The lookup's not-found error goes on to point at --roles, the
			// domain-manager binder's flag, which keystone pressure ignores.
			if errors.Is(err, keystone.ErrRoleNotFound) {
				err = fmt.Errorf("role %q %w", pressureReaderRole, keystone.ErrRoleNotFound)
			}
			slog.Warn("pool users get no reader role; a service whose read policy asks for it refuses the probes with 403", "error", err)
		}
		return nil, nil
	}
	grants, err := set.GrantPool(ctx, pressureReaderRole, roleID, concurrency)
	if err == nil {
		slog.Info("granted reader role to the pool users", "grants", len(grants))
	}
	return grants, err
}

// parseMixFlag parses the --mix values, each "<operation>=<weight>", into
// operation weights. The operation must be one of the mix keys of the
// scenario's pressure block, the seven operations of keystoneops; the weight
// must be a non-negative integer. A later value for the same operation wins.
func parseMixFlag(values []string) (map[string]int, error) {
	known := defaultPressureMix()
	weights := make(map[string]int, len(values))
	for _, v := range values {
		op, w, ok := strings.Cut(v, "=")
		if !ok {
			return nil, fmt.Errorf("--mix: want <operation>=<weight>, got %q", v)
		}
		if _, ok := known[op]; !ok {
			return nil, fmt.Errorf("--mix: unknown operation %q", op)
		}
		n, err := strconv.Atoi(w)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("--mix: weight for %q must be a non-negative integer, got %q", op, w)
		}
		weights[op] = n
	}
	return weights, nil
}

// mergePressureConfig builds the engine config and the operation options from
// three layers, lowest precedence first, as mergeKeystoneChaosConfig does:
// the built-in defaults, the scenario's pressure block (each non-zero or
// non-nil field), and the dedicated flags (each one explicitly set). --duration
// 0 given as a flag selects the unbounded run, while the block's duration 0
// means unset. --workers 0 and --tokens 0 also mean unset and keep the block's
// value or the default; --churn-users 0 reserves no user. Each mix weight the
// block or a --mix value names overrides the default of that operation alone,
// and an explicit 0 switches it off. It does not set the seed, the classifier
// or the checkpoint, which runKeystonePressure adds once the plan is
// provisioned; pressure.Config.Validate does not read them while OnCheckpoint
// is unset, so the command's pre-cloud check is the same without them.
//
// The propagation probe takes the same layers: off with a ceiling of
// defaultPropagationTimeout by default, then the block's propagation.every and
// propagation.timeout where positive, a block's timeout 0 meaning the
// default, then --propagation-every and --propagation-timeout where set, so
// --propagation-every 0 switches off a block's probe. Once every layer is
// applied, a negative interval fails, as a negative block value does, rather
// than switching the probe off unnoticed; a negative ceiling fails, and so
// does one that is not positive while the probe is on. A block's negative
// value never gets here, since the scenario's own validation rejects it.
//
// The block's stages, which no flag overrides, divide the merged duration:
// once every duration layer is applied, stageWindows resolves their windows
// into cfg.Stages, each with a nil Enter for runKeystonePressure to set, and
// the fourth value holds each stage's fan_assignments, 0 for a stage without
// one. Stages need a bounded run, so --duration 0 with stages fails; with no
// duration anywhere cfg.Stages stays empty, and cfg.Validate reports the
// missing duration as it does for a run without stages.
func mergePressureConfig(cmd *cobra.Command, opts *globalOptions, s keystonescenario.Scenario, f pressureFlags) (pressure.Config, keystoneops.Options, []int, error) {
	cfg := pressure.Config{
		Workers:     defaultPressureWorkers,
		BucketWidth: defaultChaosBucketWidth,
		OpTimeout:   opts.timeout,
	}
	o := keystoneops.Options{
		Tokens:      defaultPressureTokens,
		ChurnUsers:  defaultPressureChurnUsers,
		Weights:     defaultPressureMix(),
		OpTimeout:   opts.timeout,
		Propagation: keystoneops.Propagation{Timeout: defaultPropagationTimeout},
	}

	if b := s.Pressure; b != nil {
		if b.Duration > 0 {
			cfg.Duration = time.Duration(b.Duration)
		}
		if b.Workers > 0 {
			cfg.Workers = b.Workers
		}
		if b.Tokens > 0 {
			o.Tokens = b.Tokens
		}
		if b.ChurnUsers != nil {
			o.ChurnUsers = *b.ChurnUsers
		}
		if b.BucketWidth > 0 {
			cfg.BucketWidth = time.Duration(b.BucketWidth)
		}
		if m := b.Mix; m != nil {
			for name, w := range map[string]*int{
				keystoneops.OpValidate:   m.Validate,
				keystoneops.OpProbe:      m.Probe,
				keystoneops.OpEffective:  m.Effective,
				keystoneops.OpRevoke:     m.Revoke,
				keystoneops.OpGrantChurn: m.GrantChurn,
				keystoneops.OpIssue:      m.Issue,
				keystoneops.OpBadToken:   m.BadToken,
			} {
				if w != nil {
					o.Weights[name] = *w
				}
			}
		}
		if pr := b.Propagation; pr != nil {
			if pr.Every > 0 {
				o.Propagation.Every = time.Duration(pr.Every)
			}
			if pr.Timeout > 0 {
				o.Propagation.Timeout = time.Duration(pr.Timeout)
			}
		}
	}

	flags := cmd.Flags()
	if flags.Changed("duration") {
		cfg.Duration = f.duration
		cfg.Unbounded = f.duration == 0
	}
	if flags.Changed("bucket-width") {
		cfg.BucketWidth = f.bucketWidth
	}
	if flags.Changed("workers") && f.workers != 0 {
		cfg.Workers = f.workers
	}
	if flags.Changed("tokens") && f.tokens != 0 {
		o.Tokens = f.tokens
	}
	if flags.Changed("churn-users") {
		o.ChurnUsers = f.churnUsers
	}
	if flags.Changed("mix") {
		weights, err := parseMixFlag(f.mix)
		if err != nil {
			return pressure.Config{}, keystoneops.Options{}, nil, err
		}
		for name, w := range weights {
			o.Weights[name] = w
		}
	}
	if flags.Changed("propagation-every") {
		o.Propagation.Every = f.propagationEvery
	}
	if flags.Changed("propagation-timeout") {
		o.Propagation.Timeout = f.propagationTimeout
	}

	if o.ChurnUsers < 0 {
		return pressure.Config{}, keystoneops.Options{}, nil, fmt.Errorf("--churn-users must not be negative, got %d", o.ChurnUsers)
	}
	if o.Tokens < 1 {
		return pressure.Config{}, keystoneops.Options{}, nil, fmt.Errorf("pressure tokens must be at least 1, got %d", o.Tokens)
	}
	if o.Propagation.Every < 0 {
		return pressure.Config{}, keystoneops.Options{}, nil, fmt.Errorf("--propagation-every must not be negative, got %s", o.Propagation.Every)
	}
	if t := o.Propagation.Timeout; t < 0 || (o.Propagation.Every > 0 && t <= 0) {
		return pressure.Config{}, keystoneops.Options{}, nil, fmt.Errorf("--propagation-timeout must be positive, got %s", t)
	}

	var targets []int
	if b := s.Pressure; b != nil && len(b.Stages) > 0 {
		switch {
		case cfg.Unbounded:
			return pressure.Config{}, keystoneops.Options{}, nil, errors.New("pressure.stages need a bounded run; --duration 0 selects a run without an end")
		case cfg.Duration > 0:
			windows, err := stageWindows(b.Stages, cfg.Duration)
			if err != nil {
				return pressure.Config{}, keystoneops.Options{}, nil, err
			}
			for i, st := range b.Stages {
				cfg.Stages = append(cfg.Stages, pressure.Stage{Duration: windows[i]})
				target := 0
				if st.FanAssignments != nil {
					target = *st.FanAssignments
				}
				targets = append(targets, target)
			}
		}
	}
	return cfg, o, targets, nil
}

// stageWindows resolves the stage durations against the run duration. A
// stage with a duration keeps it; the stages without one share the rest
// equally, the last of them taking the remainder of the division; when every
// stage has a duration and they add up to less than duration, the last stage
// runs to the end. The result adds up to duration exactly.
//
// stages must not be empty. A rest too short to give each stage without a
// duration a positive share counts as leaving nothing for them, so every
// window is positive.
func stageWindows(stages []keystonescenario.Stage, duration time.Duration) ([]time.Duration, error) {
	windows := make([]time.Duration, len(stages))
	var (
		fixed time.Duration // the sum of the stages' own durations
		open  []int         // the indexes of the stages without a duration
	)
	for i, st := range stages {
		d := time.Duration(st.Duration)
		if d == 0 {
			open = append(open, i)
			continue
		}
		windows[i] = d
		// The sum saturates rather than wraps, so durations that overflow it
		// are reported as more than the run duration.
		if d > math.MaxInt64-fixed {
			fixed = math.MaxInt64
		} else {
			fixed += d
		}
	}
	if fixed > duration {
		return nil, fmt.Errorf("pressure.stages: the stage durations add up to %s, more than the run duration %s", fixed, duration)
	}
	rest := duration - fixed
	if len(open) == 0 {
		windows[len(windows)-1] += rest
		return windows, nil
	}
	n := time.Duration(len(open))
	share := rest / n
	if share == 0 {
		return nil, fmt.Errorf("pressure.stages: the stages with a duration fill the run duration %s, leaving nothing for the %d stage(s) without one", duration, len(open))
	}
	for _, i := range open {
		windows[i] = share
	}
	windows[open[len(open)-1]] += rest % n
	return windows, nil
}

// requirePressureAdminTier rejects every tier but the admin one: validating
// and revoking other users' tokens and listing their role assignments are
// admin operations in Keystone's default policy, so a lower tier could not run
// three of the five operations and would measure something else.
func requirePressureAdminTier(tier keystone.Tier) error {
	if tier == keystone.TierAdmin {
		return nil
	}
	return fmt.Errorf("keystone pressure needs the admin tier: validating and revoking other users' tokens and listing their role assignments are admin operations; the caller resolved to tier %q", tier)
}

// resolvePressureTier is the privilege pre-check of keystone pressure. It
// classifies the caller from the token only for --privilege auto, takes the
// asserted tier otherwise, and accepts the admin tier only. It does not use
// resolveKeystonePrivilege, whose domain-manager binding would fail an
// auto-detected domain manager with the binder's errors instead of naming the
// tier pressure needs. --domain and --roles parametrize that binding and have
// no effect here, which a warning says when either is set.
func resolvePressureTier(ctx context.Context, gc *gophercloud.ServiceClient, priv keystonePrivilegeFlags) (keystone.Tier, error) {
	var tier keystone.Tier
	switch priv.privilege {
	case "admin":
		tier = keystone.TierAdmin
	case "domain-manager":
		tier = keystone.TierDomainManager
	case "auto":
		cls, err := keystone.ClassifyPrivilege(ctx, gc)
		if err != nil {
			return "", err
		}
		tier = cls.Tier
	default:
		return "", fmt.Errorf("invalid --privilege %q: want auto, admin, or domain-manager", priv.privilege)
	}
	if err := requirePressureAdminTier(tier); err != nil {
		return "", err
	}
	if priv.domain != "" || priv.roles != defaultKeystoneRoles {
		slog.Warn("--domain and --roles are ignored: keystone pressure runs in the admin tier only")
	}
	slog.Info("privilege pre-check", "tier", tier)
	return tier, nil
}

// failPressureSetup handles a keystone pressure run that failed before or
// while its workers ran: provisioning, building the operation set, priming the
// pool, the engine rejecting its input, or a stage entry failing. It has the
// shape of finishAbortedKeystoneApply: writeRecord runs first, so a teardown
// that fails partway, or a second-signal hard abort during it, still leaves a
// record to reclaim from. With noCleanup it prints the reclaim hint and leaves
// the resources; otherwise it tears them down on a context.WithoutCancel of
// ctx, each op bounded by opTimeout, in reverse dependency order. Every return
// is a non-nil error that starts with what failed and names the run id, so the
// command exits non-zero.
func failPressureSetup(ctx context.Context, out io.Writer, c keystoneexec.Cleaner, runID string, created []resource.Resource, noCleanup bool, opTimeout time.Duration, what string, cause error, writeRecord func() (string, error)) error {
	recordPath, _ := writeRecord()
	hint := "--run-id " + runID
	if recordPath != "" {
		hint = "--run " + recordPath
	}

	if noCleanup {
		if _, err := fmt.Fprintf(out, "pressure setup failed; resources left in place — reclaim with: keystone cleanup %s\n", hint); err != nil {
			slog.Warn("writing reclaim hint to output failed", "run", runID, "error", err)
		}
		return fmt.Errorf("%s (run %s): %w", what, runID, cause)
	}

	deleted, cleanupErr := keystoneexec.Cleanup(context.WithoutCancel(ctx), keystoneTimeoutCleaner{c, opTimeout}, runID, created, opTimeout)
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		slog.Warn("writing teardown count to output failed", "run", runID, "error", err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("%s (run %s): %w; teardown incomplete after deleting %d resource(s): %w — reclaim the rest with: keystone cleanup %s", what, runID, cause, deleted, cleanupErr, hint)
	}
	return fmt.Errorf("%s (run %s): %w", what, runID, cause)
}

// pressureStats maps the engine result, the merged options and the set's
// service statistics onto the persisted run-record schema. Revocations counts
// the run's succeeded revoke operations, and each bucket's Revocations those
// of that bucket and every earlier one, so a latency change reads next to the
// revocations that had piled up by then; the propagation probe's revocations
// are in neither. ChurnUsers is 0 when grant_churn is off, since no user is
// reserved then. Stages holds the stages the run entered, in order, each with
// its 1-based index and its fan_assignments target from stages, 0 for a stage
// without one; it is nil for a run without stages. Probes and BadTokens hold
// ss's probe and bad_token draws per service, nil when the run drew none, and
// Propagation the propagation probe's settings and outcome from ss while
// o.Propagation.Every is positive; it is nil for a run without the probe.
func pressureStats(r *pressure.Result, o keystoneops.Options, stages []int, ss keystoneops.ServiceStats) *run.PressureStats {
	ps := &run.PressureStats{
		Workers:     r.Workers,
		Tokens:      o.Tokens,
		ChurnUsers:  pressureChurnUsers(o),
		Mix:         pressureMix(o.Weights),
		Operations:  r.Operations,
		Revocations: succeededRevocations(r.Operations),
		Probes:      ss.Probes.ByType,
		BadTokens:   ss.BadTokens.ByType,
		BucketWidth: r.BucketWidth,
	}
	if o.Propagation.Every > 0 {
		pp := ss.Propagation
		ps.Propagation = &run.PressurePropagation{
			Every:    pp.Every,
			Timeout:  pp.Timeout,
			Probes:   pp.Probes,
			TimedOut: pp.TimedOut,
			Delay:    pp.Delay,
		}
	}
	cumulative := 0
	for _, b := range r.Buckets {
		cumulative += succeededRevocations(b.Operations)
		ps.Buckets = append(ps.Buckets, run.PressureBucket{Start: b.Start, Operations: b.Operations, Revocations: cumulative})
	}
	for i, st := range r.Stages {
		ps.Stages = append(ps.Stages, run.PressureStage{
			Index:          i + 1,
			FanAssignments: stageTarget(stages, i),
			Start:          st.Start,
			End:            st.End,
			Operations:     st.Operations,
		})
	}
	return ps
}

// stageTarget returns the fan_assignments target of stage i, the 0-based
// index into stages, and 0 when the stage sets none or stages has no entry
// for it.
func stageTarget(stages []int, i int) int {
	if i < len(stages) {
		return stages[i]
	}
	return 0
}

// succeededRevocations returns the succeeded samples of the revoke operation
// in agg, 0 when agg holds none.
func succeededRevocations(agg metrics.Aggregate) int {
	for _, s := range agg.ByType {
		if s.Type == keystoneops.OpRevoke {
			return s.Succeeded
		}
	}
	return 0
}

// pressureChurnUsers returns the number of users the run reserves for
// grant_churn: o.ChurnUsers while grant_churn has a positive weight, 0
// otherwise, as keystoneops.Build reserves them.
func pressureChurnUsers(o keystoneops.Options) int {
	if o.Weights[keystoneops.OpGrantChurn] > 0 {
		return o.ChurnUsers
	}
	return 0
}

// pressureMix lists every operation with a positive weight, sorted by
// operation name, the order the run record keeps.
func pressureMix(weights map[string]int) []run.PressureWeight {
	mix := make([]run.PressureWeight, 0, len(weights))
	for name, w := range weights {
		if w > 0 {
			mix = append(mix, run.PressureWeight{Operation: name, Weight: w})
		}
	}
	sort.Slice(mix, func(i, j int) bool { return mix[i].Operation < mix[j].Operation })
	return mix
}

// pressureMixLabel renders the mix for the start log line as sorted
// "operation=weight" entries of the positive weights.
func pressureMixLabel(weights map[string]int) []string {
	mix := pressureMix(weights)
	label := make([]string, 0, len(mix))
	for _, w := range mix {
		label = append(label, w.Operation+"="+strconv.Itoa(w.Weight))
	}
	return label
}

// checkPressureMix checks the merged weights, before any cloud call, with the
// rules pressure.Run applies to the operation list it is handed, so a mix the
// engine would refuse fails here rather than after the plan is provisioned and
// the pool primed: at least one weight must be positive (pressure.ErrNoWeight
// otherwise) and the weights must not sum past the largest int. The list holds
// the seven operations keystoneops.Set.Operations returns, named by
// keystoneops.OperationNames, with the merged weights; their Do is never
// called, as the check reads only names and weights.
func checkPressureMix(weights map[string]int) error {
	ops := make([]pressure.Operation, 0, len(keystoneops.OperationNames))
	for _, name := range keystoneops.OperationNames {
		ops = append(ops, pressure.Operation{
			Name:   name,
			Weight: weights[name],
			Do:     func(context.Context, *rand.Rand) error { return nil },
		})
	}
	return pressure.ValidateOperations(ops)
}

// sizeConnectionPool lets gc keep up to conns idle connections to its
// endpoint. The identity client gophercloud builds uses http.DefaultTransport
// or a clone of it, which keeps net/http's default of two idle connections per
// host, so closed-loop workers against one Keystone endpoint would otherwise
// churn connections: a request that finds no idle connection opens a new TCP
// and TLS connection, and the latency samples include handshakes rather than
// measuring Keystone alone. The pool-token calls share this transport, since
// they copy gc's HTTP client, and so do the probes at the other services:
// each service host gets the per-host cap below, and all hosts, Keystone's
// included, share the total one.
//
// A nil transport, which stands for http.DefaultTransport, and an
// *http.Transport are both replaced by a clone, so neither the process-wide
// default nor a transport another client may share changes; the clone keeps
// the TLS settings and the HTTP/2 negotiation of its origin. The clone keeps
// conns idle connections per host and at least conns idle connections in
// all: a larger MaxIdleConns stays, as does 0, which means no limit. Any
// other RoundTripper is left alone with a warning, since its pool is out of
// reach.
func sizeConnectionPool(gc *gophercloud.ServiceClient, conns int) {
	rt := gc.HTTPClient.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	base, ok := rt.(*http.Transport)
	if !ok {
		slog.Warn("the connection pool could not be sized: the identity client's transport is not an *http.Transport, so latency samples may include connection handshakes",
			"transport", fmt.Sprintf("%T", rt), "connections", conns)
		return
	}
	t := base.Clone()
	t.MaxIdleConnsPerHost = conns
	if t.MaxIdleConns != 0 && t.MaxIdleConns < conns {
		t.MaxIdleConns = conns
	}
	gc.HTTPClient.Transport = t
}

// pressureDurationLabel is the duration a pressure run logs: "unbounded" for a
// run without an end, the configured duration otherwise, as
// chaosDurationLabel does for a churn run.
func pressureDurationLabel(cfg pressure.Config) any {
	if cfg.Unbounded {
		return "unbounded"
	}
	return cfg.Duration
}

// pressurePropagationLabel is the propagation probe a pressure run logs: its
// interval, or "off" for a run without the probe.
func pressurePropagationLabel(o keystoneops.Options) any {
	if o.Propagation.Every > 0 {
		return o.Propagation.Every
	}
	return "off"
}
