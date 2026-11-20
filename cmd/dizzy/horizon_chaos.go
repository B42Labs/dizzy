package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/chaos/horizongraph"
	"github.com/B42Labs/dizzy/internal/horizon"
	horizonexec "github.com/B42Labs/dizzy/internal/horizon/executor"
	horizonplan "github.com/B42Labs/dizzy/internal/horizon/plan"
	horizonscenario "github.com/B42Labs/dizzy/internal/horizon/scenario"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
)

// defaultChaosBrowseRatio is the built-in per-step probability that a live
// session browses (loads its pages and switches project), used when neither
// the scenario chaos block nor --browse-ratio supplies one.
const defaultChaosBrowseRatio = 0.3

// newHorizonChaosCmd builds "horizon chaos": a churn of dashboard sessions
// that, for a configured duration or until it is stopped (--duration 0),
// logs sessions in and out at random within the plan's sessions as the
// envelope, and lets a live session browse now and then. It parses
// --horizon-url, authenticates the clouds.yaml entry, provisions the plan's
// users and projects once (a failed provisioning deletes what it created),
// runs the churn while it rewrites its run record every
// chaosCheckpointInterval, and records the run. Its teardown always logs
// every live session out; unless --no-cleanup it then deletes the created
// users and projects and checks that none is left. A rejected login or a
// mismatched response does not stop the churn, but once the teardown ran
// the command exits non-zero naming the counts.
func newHorizonChaosCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		sets         []string
		noCleanup    bool
		f            chaosFlags
		browseRatio  float64
		horizonURL   string
	)

	cmd := &cobra.Command{
		Use:   "chaos",
		Short: "Run a randomized churn of dashboard logins, browsing and logouts within a scenario envelope",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, p, err := buildHorizonPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			cfg := mergeHorizonChaosConfig(cmd, opts, s, f, browseRatio)
			cfg.Classify = horizongraph.Classify
			if err := cfg.Validate(); err != nil {
				return err
			}

			base, err := parseHorizonURL(horizonURL)
			if err != nil {
				return err
			}

			// Two-phase shutdown: the first signal stops the churn, then the
			// teardown below logs out and deletes; a second signal kills the
			// process.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()

			tel, err := setupTelemetry(ctx, opts, horizonTelemetryConfig(opts, p))
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)

			runID, err := newRunID()
			if err != nil {
				return err
			}

			target, err := connectHorizon(ctx, opts, base, p)
			if err != nil {
				return err
			}

			collector := metrics.NewCollector()
			client := target.client(runID, collector, tel)
			et := target.execTarget(client)

			// The users and projects are the churn's scaffold: provisioned once,
			// deleted after the churn. A failed provisioning deletes what it
			// created before it returns.
			prov, err := horizonexec.Provision(ctx, et, p, opts.concurrency, opts.timeout)
			if err != nil {
				return rollBackHorizonProvision(ctx, cmd.OutOrStdout(), client, runID, prov.Created, err, opts.concurrency, opts.timeout)
			}

			nodes, sessions, err := horizongraph.Build(p, et, prov, runID, opts.timeout)
			if err != nil {
				return rollBackHorizonProvision(ctx, cmd.OutOrStdout(), client, runID, prov.Created, fmt.Errorf("building churn graph: %w", err), opts.concurrency, opts.timeout)
			}

			slog.Info("starting churn run", "run", runID, "scenario", p.Scenario,
				"sessions", len(p.Sessions), "users", len(p.Users), "projects", len(p.Projects),
				"duration", chaosDurationLabel(cfg), "minInterval", cfg.MinInterval, "maxInterval", cfg.MaxInterval,
				"maxParallel", cfg.MaxParallel, "browseRatio", cfg.ResizeRatio, "concurrency", cfg.Concurrency)

			start := time.Now()
			// buildRecord builds the run record as of finished. Its created list
			// is the provisioned users and projects, never the engine's sessions,
			// which carry no identifier and need no cleanup. A failed check seen
			// so far sets the record's error, so a checkpoint carries it too.
			buildRecord := func(r *chaos.Result, finished time.Time) *run.Record {
				rec := horizonRunRecord(runID, p, start, finished, prov.Created, collector.Aggregate(finished.Sub(start)), nil)
				rec.Chaos = chaosStats(r)
				if err := horizonCheckError(runID, rec.Metrics); err != nil {
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
				return rollBackHorizonProvision(ctx, cmd.OutOrStdout(), client, runID, prov.Created, fmt.Errorf("running churn (run %s): %w", runID, runErr), opts.concurrency, opts.timeout)
			}
			rec := buildRecord(result, finished)
			checkErr := horizonCheckError(runID, rec.Metrics)

			// A churn run is a single iteration. An interrupted run counts as a
			// failed iteration, and so does one whose checks failed.
			interrupted := chaosInterrupted(ctx, cfg)
			tel.RecordIteration(ctx, rec.Metrics.Wall, !interrupted && checkErr == nil)
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

			return finishHorizonChurn(ctx, cmd.OutOrStdout(), client, sessions, p, runID, recordPath, prov.Created, interrupted, noCleanup, checkErr, opts.concurrency, opts.timeout)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.users=5 (repeatable)")
	flags.DurationVar(&f.duration, "duration", 0, "total wall-clock runtime of the churn; 0 runs until interrupted (required via flag or the scenario chaos block)")
	flags.DurationVar(&f.bucketWidth, "bucket-width", defaultChaosBucketWidth, "width of one time bucket in an unbounded run's time series, at least 1m")
	flags.DurationVar(&f.minInterval, "min-interval", defaultChaosMinInterval, "minimum random delay between scheduled actions")
	flags.DurationVar(&f.maxInterval, "max-interval", defaultChaosMaxInterval, "maximum random delay between scheduled actions")
	flags.IntVar(&f.maxParallel, "max-parallel", 0, "maximum concurrent in-flight churn operations (default: --concurrency)")
	flags.Float64Var(&f.churnRatio, "churn-ratio", defaultChaosChurnRatio, "login bias at steady state, between 0 and 1")
	flags.Float64Var(&f.targetFill, "target-fill", defaultChaosTargetFill, "fraction of the sessions to keep logged in on average, between 0 and 1")
	flags.Float64Var(&browseRatio, "browse-ratio", defaultChaosBrowseRatio, "probability per churn step that a live session browses: loads its pages and switches project as its plan says, at most once per login, between 0 and 1; 0 disables browsing")
	flags.BoolVar(&noCleanup, "no-cleanup", false, "leave the created users and projects in place at the end of the run or on interrupt and print the cleanup hint; sessions are logged out either way")
	flags.StringVar(&horizonURL, "horizon-url", "", horizonURLFlagHelp)
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// mergeHorizonChaosConfig builds the churn config from three layers, lowest
// precedence first: built-in defaults, the scenario's chaos block (each
// non-zero field), and the dedicated flags (each one explicitly set). The
// browse ratio, a pointer in the block so an explicit 0 turns browsing off,
// maps onto the engine's mutate probability (chaos.Config.ResizeRatio). It
// is the Horizon counterpart of mergeBarbicanChaosConfig.
func mergeHorizonChaosConfig(cmd *cobra.Command, opts *globalOptions, s horizonscenario.Scenario, f chaosFlags, browseRatio float64) chaos.Config {
	cfg := chaos.Config{
		MinInterval: defaultChaosMinInterval,
		MaxInterval: defaultChaosMaxInterval,
		MaxParallel: opts.concurrency,
		ChurnRatio:  defaultChaosChurnRatio,
		TargetFill:  defaultChaosTargetFill,
		ResizeRatio: defaultChaosBrowseRatio,
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
		if c.BrowseRatio != nil {
			cfg.ResizeRatio = *c.BrowseRatio
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
	if cmd.Flags().Changed("browse-ratio") {
		cfg.ResizeRatio = browseRatio
	}
	return cfg
}

// horizonCheckError returns the error a Horizon run whose checks failed
// exits with, naming how many logins Horizon rejected and how many
// responses did not match, or nil when there were none.
func horizonCheckError(runID string, agg metrics.Aggregate) error {
	var rejected, mismatched int
	for _, e := range agg.Errors {
		switch e.Kind {
		case horizon.ErrorKind(horizon.ErrRejected):
			rejected += e.Count
		case horizon.ErrorKind(horizon.ErrMismatch):
			mismatched += e.Count
		}
	}
	if rejected == 0 && mismatched == 0 {
		return nil
	}
	return fmt.Errorf("horizon checks failed (run %s): %d login(s) rejected, %d response(s) mismatched", runID, rejected, mismatched)
}

// rollBackHorizonProvision deletes what a chaos run created before its churn
// could start, on a context.WithoutCancel of ctx, prints the count, and
// returns cause joined with any teardown error.
func rollBackHorizonProvision(ctx context.Context, out io.Writer, c horizonexec.Cleaner, runID string, created []resource.Resource, cause error, concurrency int, opTimeout time.Duration) error {
	if len(created) == 0 {
		return cause
	}
	deleted, err := horizonexec.Cleanup(context.WithoutCancel(ctx), c, runID, created, concurrency, opTimeout)
	if _, werr := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); werr != nil {
		slog.Warn("writing teardown count to output failed", "run", runID, "error", werr)
	}
	if err != nil {
		return errors.Join(cause, fmt.Errorf("tearing down run %s: %w; reclaim the rest with: horizon cleanup --run-id %s", runID, err, runID))
	}
	return cause
}

// finishHorizonChurn applies the teardown policy on a context.WithoutCancel
// of ctx, so a first-signal interrupt does not stop the teardown it
// triggered. It always logs every live session out. A plan with users then,
// unless noCleanup, deletes the users and projects and runs a leak check;
// with noCleanup it prints the cleanup hint instead. checkErr is joined to
// the outcome on every path, so the teardown always runs first.
func finishHorizonChurn(ctx context.Context, out io.Writer, c horizonexec.Cleaner, sessions *horizongraph.Sessions, p *horizonplan.Plan, runID, recordPath string, created []resource.Resource, interrupted, noCleanup bool, checkErr error, concurrency int, opTimeout time.Duration) (err error) {
	defer func() { err = errors.Join(err, checkErr) }()
	tctx := context.WithoutCancel(ctx)

	n, logoutErr := sessions.LogoutAll(tctx, opTimeout)
	if _, err := fmt.Fprintf(out, "logged out %d session(s)\n", n); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	if logoutErr != nil {
		logoutErr = fmt.Errorf("logging out the sessions of run %s: %w", runID, logoutErr)
	}
	if len(p.Users) == 0 {
		return logoutErr
	}

	if noCleanup {
		reason := "churn complete"
		if interrupted {
			reason = "churn interrupted"
		}
		hint := "--run-id " + runID
		if recordPath != "" {
			hint = "--run " + recordPath
		}
		_, err := fmt.Fprintf(out, "%s; users and projects left in place — reclaim with: horizon cleanup %s\n", reason, hint)
		return errors.Join(logoutErr, err)
	}

	deleted, cleanupErr := horizonexec.Cleanup(tctx, c, runID, created, concurrency, opTimeout)
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		return errors.Join(logoutErr, fmt.Errorf("writing output: %w", err))
	}
	if cleanupErr != nil {
		return errors.Join(logoutErr, fmt.Errorf("tearing down run %s: %w", runID, cleanupErr))
	}

	leaked, err := horizonLeakCheck(tctx, c, runID, opTimeout)
	if err != nil {
		return errors.Join(logoutErr, err)
	}
	if leaked > 0 {
		_, err = fmt.Fprintf(out, "leak check: %d run-named user(s) and project(s) still present after teardown\n", leaked)
	} else {
		_, err = fmt.Fprintf(out, "leak check: no run-named users or projects remain\n")
	}
	return errors.Join(logoutErr, err)
}

// horizonLeakCheck counts the run's users and projects still listed after
// teardown, each listing bounded by opTimeout.
func horizonLeakCheck(ctx context.Context, c horizonexec.Cleaner, runID string, opTimeout time.Duration) (int, error) {
	var total int
	for _, list := range []struct {
		what string
		fn   func(context.Context, string) ([]resource.Resource, error)
	}{
		{"users", c.ListUsersByRun},
		{"projects", c.ListProjectsByRun},
	} {
		lctx, cancel := context.WithTimeout(ctx, opTimeout)
		found, err := list.fn(lctx, runID)
		cancel()
		if err != nil {
			return total, fmt.Errorf("leak check listing %s: %w", list.what, err)
		}
		total += len(found)
	}
	return total, nil
}
