package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/barbican"
	barbicanexec "github.com/B42Labs/dizzy/internal/barbican/executor"
	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	barbicanscenario "github.com/B42Labs/dizzy/internal/barbican/scenario"
	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/chaos/barbicangraph"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// defaultChaosRetrieveRatio is the built-in per-step probability of reading
// back the payload of a live stored secret the plan marks for read-back, used
// when neither the scenario chaos block nor the --retrieve-ratio flag supplies
// one. The temporal defaults it joins live in chaos.go, shared with the other
// chaos commands.
const defaultChaosRetrieveRatio = 0.3

// newBarbicanChaosCmd builds "barbican chaos": a random churn/soak run that, for
// a configured duration or until it is stopped (--duration 0), continuously
// stores and deletes secrets (each with its synthetic payload and run identity
// stamp) and groups and ungroups them in generic containers, and occasionally
// reads a live secret's payload back and byte-compares it, bounded by the
// scenario as the spatial envelope. Key orders are not churned. It
// authenticates, pre-checks quota against the secrets and containers the churn
// populates, runs the churn, rewrites its run record every
// chaosCheckpointInterval while it runs, records the run, and — whether it
// completed or was interrupted, unless --no-cleanup — tears the secrets and
// containers down by identity and reports any leak. A read-back that returned
// different bytes does not stop the churn, but once the teardown has run the
// command exits non-zero naming the mismatch count, since a mismatch is a
// correctness failure rather than a latency one. It mirrors "glance chaos" with
// the key-manager-specific --retrieve-ratio.
func newBarbicanChaosCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath  string
		sets          []string
		noCleanup     bool
		f             chaosFlags
		retrieveRatio float64
	)

	cmd := &cobra.Command{
		Use:   "chaos",
		Short: "Run continuous randomized secret/container churn with payload read-backs within a scenario envelope",
		Long: "Run continuous randomized secret/container churn with payload read-backs within a scenario envelope.\n\n" +
			"Run only one dizzy process per project: a concurrent monitor's always-on pre-flight sweep reclaims " +
			"every dizzy-named secret and container in the project, so this run's secrets and containers would be torn down mid-flight.",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, p, err := buildBarbicanPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			cfg := mergeBarbicanChaosConfig(cmd, opts, s, f, retrieveRatio)
			cfg.Classify = barbicangraph.Classify
			if err := cfg.Validate(); err != nil {
				return err
			}

			// Two-phase shutdown: the first Ctrl-C / SIGTERM cancels the run so the
			// churn stops and its resources unwind instead of being killed, then the
			// teardown below tears them down. Unregistering the handler right after
			// means a second signal takes the default disposition and kills the
			// process — there is always a hard way out mid-teardown.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()

			tel, err := telemetry.Setup(ctx, telemetry.Config{
				Enabled: opts.otel, Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "barbican",
			})
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)

			runID, err := newRunID()
			if err != nil {
				return err
			}

			kv, err := config.NewKeyManagerClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating key manager client: %w", err)
			}

			// The envelope is the population's worst case, so quota is pre-checked
			// against the secrets and containers the churn can hold at once; the
			// orders it never creates are left out. The pre-check runs before the
			// collector exists and before any write, so an oversized plan leaves
			// nothing in the cloud and no run record.
			if err := barbican.PrecheckQuota(ctx, kv, barbicanChurnEnvelope(p)); err != nil {
				return err
			}

			collector := metrics.NewCollector()
			// The teardown discovers secrets through this client, so it is built
			// with a request timeout, as barbican cleanup's is.
			client := newBarbicanClient(kv, runID, collector, opts.timeout)
			client.SetTelemetry(tel)

			nodes, err := barbicangraph.Build(p, client, p.Seed, opts.timeout)
			if err != nil {
				return fmt.Errorf("building churn graph: %w", err)
			}

			slog.Info("starting churn run", "run", runID, "scenario", p.Scenario,
				"secrets", len(p.Secrets), "containers", len(p.Containers),
				"duration", chaosDurationLabel(cfg), "minInterval", cfg.MinInterval, "maxInterval", cfg.MaxInterval,
				"maxParallel", cfg.MaxParallel, "retrieveRatio", cfg.ResizeRatio, "concurrency", cfg.Concurrency)

			start := time.Now()
			// buildRecord builds the run record as of finished, for the
			// checkpoints written while the churn runs and for the final record.
			// A read-back mismatch seen so far sets the record's error, so a
			// checkpoint of a killed run carries it too.
			buildRecord := func(r *chaos.Result, finished time.Time) *run.Record {
				rec := &run.Record{
					RunID:      runID,
					Service:    "barbican",
					Scenario:   p.Scenario,
					Seed:       p.Seed,
					StartedAt:  start,
					FinishedAt: finished,
					Created:    r.Created,
					Metrics:    collector.Aggregate(finished.Sub(start)),
					Chaos:      chaosStats(r),
				}
				if err := barbicanMismatchError(runID, rec.Metrics); err != nil {
					rec.Error = err.Error()
				}
				return rec
			}
			cfg.CheckpointInterval = chaosCheckpointInterval
			cfg.OnCheckpoint = chaosCheckpoint(".", buildRecord)

			hb := startHeartbeat(ctx, "churn in progress", collectorSnapshot(collector, start, "duration", chaosDurationLabel(cfg)))
			result, runErr := chaos.Run(ctx, nodes, p.Seed, cfg, chaos.RealClock{})
			hb.stop()
			finished := time.Now()
			if runErr != nil {
				return fmt.Errorf("running churn (run %s): %w", runID, runErr)
			}
			rec := buildRecord(result, finished)
			mismatchErr := barbicanMismatchError(runID, rec.Metrics)

			// A churn run is a single iteration: export the same per-iteration
			// summary metrics from the pre-teardown aggregate. An interrupted run
			// counts as a failed iteration, and so does a run that saw a mismatch.
			interrupted := chaosInterrupted(ctx, cfg)
			tel.RecordIteration(ctx, rec.Metrics.Wall, !interrupted && mismatchErr == nil)
			tel.RecordIterationOperations(ctx, rec.Metrics.Overall.Attempted, rec.Metrics.Overall.Succeeded, rec.Metrics.Overall.Failed)

			if _, err := fmt.Fprint(cmd.OutOrStdout(), rec.Metrics.Summary()); err != nil {
				return fmt.Errorf("writing metrics: %w", err)
			}

			recordPath, werr := run.Write(".", rec)
			if werr != nil {
				slog.Error("writing run record failed; clean up by run id", "run", runID, "error", werr)
			} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "run record written to %s\n", recordPath); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}

			return finishBarbicanChurn(ctx, cmd, client, runID, recordPath, rec.Created, interrupted, noCleanup, mismatchErr, opts.concurrency, opts.timeout)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.secrets=20 (repeatable)")
	flags.DurationVar(&f.duration, "duration", 0, "total wall-clock runtime of the churn; 0 runs until interrupted (required via flag or the scenario chaos block)")
	flags.DurationVar(&f.bucketWidth, "bucket-width", defaultChaosBucketWidth, "width of one time bucket in an unbounded run's time series, at least 1m")
	flags.DurationVar(&f.minInterval, "min-interval", defaultChaosMinInterval, "minimum random delay between scheduled actions")
	flags.DurationVar(&f.maxInterval, "max-interval", defaultChaosMaxInterval, "maximum random delay between scheduled actions")
	flags.IntVar(&f.maxParallel, "max-parallel", 0, "maximum concurrent in-flight churn operations (default: --concurrency)")
	flags.Float64Var(&f.churnRatio, "churn-ratio", defaultChaosChurnRatio, "create bias at steady state, between 0 and 1")
	flags.Float64Var(&f.targetFill, "target-fill", defaultChaosTargetFill, "fraction of the envelope to keep populated on average, between 0 and 1")
	flags.Float64Var(&retrieveRatio, "retrieve-ratio", defaultChaosRetrieveRatio, "probability per churn step of reading back the payload of a live stored secret the plan marks for read-back and byte-comparing it, between 0 and 1")
	flags.BoolVar(&noCleanup, "no-cleanup", false, "leave the secrets and containers in place — at the end of the run or on interrupt — instead of tearing them down by identity")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// mergeBarbicanChaosConfig builds the churn config from three layers, lowest
// precedence first: built-in defaults, the scenario's chaos block (each non-zero
// field), and the dedicated flags (each one explicitly set). A zero field in the
// chaos block falls back to the default; to set a field to zero use the flag —
// except retrieve_ratio, a pointer whose explicit 0 disables read-backs while an
// omitted key falls back to the default. The retrieve ratio maps onto the
// engine's mutate probability (chaos.Config.ResizeRatio). It is the key manager
// counterpart of mergeGlanceChaosConfig.
func mergeBarbicanChaosConfig(cmd *cobra.Command, opts *globalOptions, s barbicanscenario.Scenario, f chaosFlags, retrieveRatio float64) chaos.Config {
	cfg := chaos.Config{
		MinInterval: defaultChaosMinInterval,
		MaxInterval: defaultChaosMaxInterval,
		MaxParallel: opts.concurrency,
		ChurnRatio:  defaultChaosChurnRatio,
		TargetFill:  defaultChaosTargetFill,
		ResizeRatio: defaultChaosRetrieveRatio,
		Concurrency: opts.concurrency,
		BucketWidth: defaultChaosBucketWidth,
	}

	if c := s.Chaos; c != nil {
		if c.Duration > 0 {
			cfg.Duration = time.Duration(c.Duration)
		}
		if c.Interval.Min > 0 {
			cfg.MinInterval = time.Duration(c.Interval.Min)
		}
		if c.Interval.Max > 0 {
			cfg.MaxInterval = time.Duration(c.Interval.Max)
		}
		if c.Parallel.Max > 0 {
			cfg.MaxParallel = c.Parallel.Max
		}
		if c.ChurnRatio > 0 {
			cfg.ChurnRatio = c.ChurnRatio
		}
		if c.TargetFill > 0 {
			cfg.TargetFill = c.TargetFill
		}
		if c.BucketWidth > 0 {
			cfg.BucketWidth = time.Duration(c.BucketWidth)
		}
		if c.RetrieveRatio != nil {
			cfg.ResizeRatio = *c.RetrieveRatio
		}
	}

	if cmd.Flags().Changed("duration") {
		cfg.Duration = f.duration
		cfg.Unbounded = f.duration == 0
	}
	if cmd.Flags().Changed("bucket-width") {
		cfg.BucketWidth = f.bucketWidth
	}
	if cmd.Flags().Changed("min-interval") {
		cfg.MinInterval = f.minInterval
	}
	if cmd.Flags().Changed("max-interval") {
		cfg.MaxInterval = f.maxInterval
	}
	if cmd.Flags().Changed("max-parallel") {
		cfg.MaxParallel = f.maxParallel
	}
	if cmd.Flags().Changed("churn-ratio") {
		cfg.ChurnRatio = f.churnRatio
	}
	if cmd.Flags().Changed("target-fill") {
		cfg.TargetFill = f.targetFill
	}
	if cmd.Flags().Changed("retrieve-ratio") {
		cfg.ResizeRatio = retrieveRatio
	}
	return cfg
}

// barbicanChurnEnvelope returns the part of p the churn populates, for the
// quota pre-check: a shallow copy with Orders set to nil, so the check sizes the
// secrets and containers the graph churns and never the orders (or their
// generated secrets) it ignores. p itself is left untouched, since the graph
// and the run record still read it.
func barbicanChurnEnvelope(p *barbicanplan.Plan) *barbicanplan.Plan {
	env := *p
	env.Orders = nil
	return &env
}

// barbicanMismatchError returns the error a churn run that read back different
// bytes than it stored exits with, or nil when no read-back mismatched. It sums
// the mismatch entries of the run's metrics error breakdown, which the client
// labels barbican.ErrKindMismatch, so the count is the number of read-backs that
// failed the byte comparison.
func barbicanMismatchError(runID string, agg metrics.Aggregate) error {
	var n int
	for _, e := range agg.Errors {
		if e.Kind == barbican.ErrKindMismatch {
			n += e.Count
		}
	}
	if n == 0 {
		return nil
	}
	return fmt.Errorf("payload read-back mismatch (run %s): %d read-back(s) returned different bytes", runID, n)
}

// finishBarbicanChurn applies the teardown policy. Unless --no-cleanup is set,
// the run — whether it completed or was interrupted — tears the secrets and
// containers down by identity (each operation bounded by opTimeout, secret
// discovery per request by the client) and runs a leak check; teardown and the
// leak check run on a context.WithoutCancel of ctx so a first-signal interrupt
// does not kill the teardown it triggered. With --no-cleanup the resources are
// left in place and the cleanup hint is printed. On every path mismatchErr is
// joined to the outcome, so the teardown always runs first and a teardown
// failure and a read-back mismatch are both reported.
func finishBarbicanChurn(ctx context.Context, cmd *cobra.Command, c barbicanexec.Cleaner, runID, recordPath string, created []resource.Resource, interrupted, noCleanup bool, mismatchErr error, concurrency int, opTimeout time.Duration) (err error) {
	defer func() { err = errors.Join(err, mismatchErr) }()

	out := cmd.OutOrStdout()
	if noCleanup {
		reason := "churn complete"
		if interrupted {
			reason = "churn interrupted"
		}
		hint := "--run-id " + runID
		if recordPath != "" {
			hint = "--run " + recordPath
		}
		_, err := fmt.Fprintf(out, "%s; resources left in place — reclaim with: barbican cleanup %s\n", reason, hint)
		return err
	}

	tctx := context.WithoutCancel(ctx)
	cleaner := barbicanTimeoutCleaner{c, opTimeout}

	deleted, cleanupErr := barbicanexec.Cleanup(tctx, cleaner, runID, created, concurrency)
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("tearing down run %s: %w", runID, cleanupErr)
	}

	leaked, err := barbicanLeakCheck(tctx, cleaner, runID)
	if err != nil {
		return err
	}
	if leaked > 0 {
		_, err = fmt.Fprintf(out, "leak check: %d run-named resource(s) still present after teardown\n", leaked)
	} else {
		_, err = fmt.Fprintf(out, "leak check: no run-named resources remain\n")
	}
	return err
}

// barbicanLeakCheck counts the containers and secrets still carrying the run's
// identity after teardown. Orders are not counted, since a churn run never
// creates one. It takes the barbicanexec.Cleaner seam so the listings share the
// teardown's bounds. Like the Glance check it cannot see a secret whose
// identity stamp never landed, since secret discovery confirms a secret by its
// dizzy_run metadata; teardown still reclaimed such a secret through the
// created list.
func barbicanLeakCheck(ctx context.Context, c barbicanexec.Cleaner, runID string) (int, error) {
	containers, err := c.ListContainersByRun(ctx, runID)
	if err != nil {
		return 0, fmt.Errorf("leak check listing containers: %w", err)
	}
	secrets, err := c.ListSecretsByRun(ctx, runID)
	if err != nil {
		return len(containers), fmt.Errorf("leak check listing secrets: %w", err)
	}
	return len(containers) + len(secrets), nil
}
