package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
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
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// Built-in defaults for the keystone pressure knobs, used when neither the
// scenario's pressure block nor a flag supplies a value. As for chaos there is
// no default duration: the block or --duration must set it. The bucket width
// default is the chaos one, defaultChaosBucketWidth.
const (
	defaultPressureWorkers    = 16
	defaultPressureTokens     = 100
	defaultPressureChurnUsers = 1
)

// The names of the two mix keys the scenario and --mix accept but this build
// does not run. A later release delivers them; until then a positive weight is
// rejected before any cloud call.
const (
	opProbe    = "probe"
	opBadToken = "bad_token"
)

// supportedPressureOps are the operations this build runs, in the order the
// rejection of an unsupported weight lists them.
var supportedPressureOps = []string{
	keystoneops.OpValidate,
	keystoneops.OpEffective,
	keystoneops.OpRevoke,
	keystoneops.OpGrantChurn,
	keystoneops.OpIssue,
}

// defaultPressureMix returns the built-in operation weights: mostly token
// validations, the request the CobaltCore patches speed up, with a trickle of
// effective listings, revocations, grant churn and issues. probe and bad_token
// are listed with weight 0 so every mix key has a default. It returns a fresh
// map on every call, so a caller may change it.
func defaultPressureMix() map[string]int {
	return map[string]int{
		keystoneops.OpValidate:   60,
		keystoneops.OpEffective:  5,
		keystoneops.OpRevoke:     2,
		keystoneops.OpGrantChurn: 2,
		keystoneops.OpIssue:      1,
		opProbe:                  0,
		opBadToken:               0,
	}
}

// pressureFlags holds the dedicated keystone pressure flag values; whether
// each one overrides the scenario block is decided by cmd.Flags().Changed.
type pressureFlags struct {
	duration    time.Duration
	bucketWidth time.Duration
	workers     int
	tokens      int
	churnUsers  int
	mix         []string
}

// newKeystonePressureCmd builds "keystone pressure": a closed-loop load on
// Keystone's token validation, revocation and role-listing path. It
// provisions the plan's domains, roles, projects, users and role assignments,
// primes a pool of project-scoped tokens, and runs a fixed number of workers
// that each send one weighted operation after another for the configured
// duration, or until it is stopped (--duration 0). It rewrites its run record
// every chaosCheckpointInterval while the workers run, prints the metrics
// summary and the Pressure table, records the run, and, unless --no-cleanup,
// tears the resources down and reports any leak, as keystone chaos does. Only
// the admin tier runs, and everything this build cannot run is rejected
// before the first cloud call.
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
drawn by weight from validate, effective, revoke, grant_churn and issue, for
the configured duration or, with --duration 0, until it is interrupted. It
prints the metrics summary and the Pressure table, writes run-<id>.json, and
tears its resources down unless --no-cleanup is given. Only the admin tier
runs: validating and revoking other users' tokens and listing their role
assignments are admin operations.

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

			cfg, o, err := mergePressureConfig(cmd, opts, s, f)
			if err != nil {
				return err
			}
			if err := checkPressureSupported(o, s); err != nil {
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
			cfg.Seed = p.Seed
			cfg.Classify = keystoneops.Classify
			cfg.CheckpointInterval = chaosCheckpointInterval
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

			gc, err := config.NewIdentityClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating identity client: %w", err)
			}

			if _, err := resolvePressureTier(ctx, gc, priv); err != nil {
				return err
			}

			collector := metrics.NewCollector()
			client := keystone.New(gc, runID, collector)
			client.SetTelemetry(tel)
			return runKeystonePressure(ctx, cmd, client, pressureRun{
				runID:       runID,
				plan:        p,
				cfg:         cfg,
				o:           o,
				concurrency: opts.concurrency,
				opTimeout:   opts.timeout,
				noCleanup:   noCleanup,
				collector:   collector,
				tel:         tel,
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
	flags.IntVar(&f.churnUsers, "churn-users", defaultPressureChurnUsers, "users reserved for grant_churn, taken from the end of the plan; they hold no pool token")
	flags.StringArrayVar(&f.mix, "mix", nil, "weight of one operation as <operation>=<weight>, e.g. --mix revoke=0; operations: "+strings.Join(supportedPressureOps, ", ")+" (repeatable)")
	flags.BoolVar(&noCleanup, "no-cleanup", false, "leave the identity resources in place — at the end of the run, on interrupt or after a failed setup — instead of tearing them down by name prefix")
	priv.register(flags)
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// pressureKeystone is what keystone pressure asks of Keystone once the caller
// passed the privilege pre-check: the executor's provisioning seam, the
// operations' seam and the teardown seam. The production *keystone.Client
// satisfies it; a test fake stands in for it to drive a run end to end
// without a cloud.
type pressureKeystone interface {
	keystoneexec.Keystone
	keystoneops.Keystone
	keystoneexec.Cleaner
}

// The production *keystone.Client must satisfy the seam.
var _ pressureKeystone = (*keystone.Client)(nil)

// pressureRun carries what runKeystonePressure needs besides Keystone: the run
// id, the plan, the engine config and the operation options that passed every
// pre-cloud check, the global concurrency and per-operation timeout,
// --no-cleanup, and the collector and telemetry the Keystone client records
// into. A nil tel is the disabled export.
type pressureRun struct {
	runID       string
	plan        *keystoneplan.Plan
	cfg         pressure.Config
	o           keystoneops.Options
	concurrency int
	opTimeout   time.Duration
	noCleanup   bool
	collector   *metrics.Collector
	tel         *telemetry.Telemetry
}

// runKeystonePressure is keystone pressure from its first write on, once
// every pre-cloud check and the privilege pre-check passed. It provisions the
// plan in the admin tier, builds the operation set, primes the token pool,
// runs the workers with a run-record checkpoint every
// r.cfg.CheckpointInterval, prints the metrics summary and the Pressure table,
// writes the run record, and tears down through finishKeystoneTeardown. A
// failure before the workers finish goes through failPressureSetup, which
// writes a record whose error names the failure and tears down what was
// created so far. It is split from the command so a test can drive it with a
// fake Keystone; it prints to cmd's output.
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

	set, err := keystoneops.Build(p, c, prov.Bindings, prov.IDs, prov.Passwords, o)
	if err != nil {
		return setupFailed(prov.Created, "building pressure set", err)
	}
	// Close is idempotent; the explicit calls below stop the refresher before
	// the teardown, this one covers any other way out.
	defer set.Close()

	slog.Info("priming token pool", "tokens", o.Tokens, "concurrency", r.concurrency)
	if err := set.Prime(ctx, r.concurrency); err != nil {
		set.Close()
		return setupFailed(prov.Created, "starting pressure run", err)
	}

	// buildRecord builds the run record as of finished, for the checkpoints
	// written while the workers run and for the final record.
	buildRecord := func(res *pressure.Result, finished time.Time) *run.Record {
		return &run.Record{
			RunID:      r.runID,
			Service:    "keystone",
			Scenario:   p.Scenario,
			Seed:       p.Seed,
			StartedAt:  start,
			FinishedAt: finished,
			Created:    prov.Created,
			Metrics:    r.collector.Aggregate(finished.Sub(start)),
			Pressure:   pressureStats(res, o),
		}
	}
	cfg.OnCheckpoint = chaosCheckpoint(".", buildRecord)

	duration := pressureDurationLabel(cfg)
	slog.Info("starting pressure run", "run", r.runID, "scenario", p.Scenario, "duration", duration,
		"workers", cfg.Workers, "tokens", o.Tokens, "churnUsers", pressureChurnUsers(o),
		"mix", pressureMixLabel(o.Weights), "concurrency", r.concurrency)

	hb := startHeartbeat(ctx, "pressure in progress", collectorSnapshot(r.collector, start, "duration", duration))
	result, runErr := pressure.Run(ctx, set.Operations(), cfg)
	hb.stop()
	set.Close()
	finished := time.Now()
	if runErr != nil {
		// Run fails only on a config or operation list the checks before the
		// first cloud call should have rejected; the provisioned resources
		// still go.
		return setupFailed(prov.Created, "running pressure", runErr)
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

// parseMixFlag parses the --mix values, each "<operation>=<weight>", into
// operation weights. The operation must be one of the mix keys of the
// scenario's pressure block, probe and bad_token included, so they reach the
// support check with their own message; the weight must be a non-negative
// integer. A later value for the same operation wins.
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
// or the checkpoint, which the command adds once the plan exists.
func mergePressureConfig(cmd *cobra.Command, opts *globalOptions, s keystonescenario.Scenario, f pressureFlags) (pressure.Config, keystoneops.Options, error) {
	cfg := pressure.Config{
		Workers:     defaultPressureWorkers,
		BucketWidth: defaultChaosBucketWidth,
		OpTimeout:   opts.timeout,
	}
	o := keystoneops.Options{
		Tokens:     defaultPressureTokens,
		ChurnUsers: defaultPressureChurnUsers,
		Weights:    defaultPressureMix(),
		OpTimeout:  opts.timeout,
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
				opProbe:                  m.Probe,
				keystoneops.OpEffective:  m.Effective,
				keystoneops.OpRevoke:     m.Revoke,
				keystoneops.OpGrantChurn: m.GrantChurn,
				keystoneops.OpIssue:      m.Issue,
				opBadToken:               m.BadToken,
			} {
				if w != nil {
					o.Weights[name] = *w
				}
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
			return pressure.Config{}, keystoneops.Options{}, err
		}
		for name, w := range weights {
			o.Weights[name] = w
		}
	}

	if o.ChurnUsers < 0 {
		return pressure.Config{}, keystoneops.Options{}, fmt.Errorf("--churn-users must not be negative, got %d", o.ChurnUsers)
	}
	if o.Tokens < 1 {
		return pressure.Config{}, keystoneops.Options{}, fmt.Errorf("pressure tokens must be at least 1, got %d", o.Tokens)
	}
	return cfg, o, nil
}

// checkPressureSupported rejects what the scenario and the flags may set but
// this build does not run: a positive probe or bad_token weight and a
// propagation probe. The keys parse so a scenario written for a later release
// loads, and fail here, before any cloud call, rather than being ignored.
func checkPressureSupported(o keystoneops.Options, s keystonescenario.Scenario) error {
	for _, name := range []string{opProbe, opBadToken} {
		if o.Weights[name] > 0 {
			return fmt.Errorf("pressure mix weight %q is not supported by this build; supported operations: %s", name, strings.Join(supportedPressureOps, ", "))
		}
	}
	if b := s.Pressure; b != nil && b.Propagation != nil && b.Propagation.Every > 0 {
		return errors.New("pressure.propagation is not supported by this build")
	}
	return nil
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
// pool, or the engine rejecting its input. It has the shape of
// finishAbortedKeystoneApply: writeRecord runs first, so a teardown that fails
// partway, or a second-signal hard abort during it, still leaves a record to
// reclaim from. With noCleanup it prints the reclaim hint and leaves the
// resources; otherwise it tears them down on a context.WithoutCancel of ctx,
// each op bounded by opTimeout, in reverse dependency order. Every return is a
// non-nil error that starts with what failed and names the run id, so the
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

// pressureStats maps the engine result and the merged options onto the
// persisted run-record schema. Revocations counts the run's succeeded revoke
// operations, and each bucket's Revocations those of that bucket and every
// earlier one, so a latency change reads next to the revocations that had
// piled up by then. ChurnUsers is 0 when grant_churn is off, since no user is
// reserved then.
func pressureStats(r *pressure.Result, o keystoneops.Options) *run.PressureStats {
	ps := &run.PressureStats{
		Workers:     r.Workers,
		Tokens:      o.Tokens,
		ChurnUsers:  pressureChurnUsers(o),
		Mix:         pressureMix(o.Weights),
		Operations:  r.Operations,
		Revocations: succeededRevocations(r.Operations),
		BucketWidth: r.BucketWidth,
	}
	cumulative := 0
	for _, b := range r.Buckets {
		cumulative += succeededRevocations(b.Operations)
		ps.Buckets = append(ps.Buckets, run.PressureBucket{Start: b.Start, Operations: b.Operations, Revocations: cumulative})
	}
	return ps
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
// the five operations keystoneops.Set.Operations returns, with the merged
// weights; their Do is never called, as the check reads only names and
// weights. probe and bad_token are left out: checkPressureSupported has
// rejected a positive weight for either by then.
func checkPressureMix(weights map[string]int) error {
	ops := make([]pressure.Operation, 0, len(supportedPressureOps))
	for _, name := range supportedPressureOps {
		ops = append(ops, pressure.Operation{
			Name:   name,
			Weight: weights[name],
			Do:     func(context.Context, *rand.Rand) error { return nil },
		})
	}
	return pressure.ValidateOperations(ops)
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
