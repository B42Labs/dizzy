package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/chaos/novagraph"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/mix"
	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	mixscenario "github.com/B42Labs/dizzy/internal/mix/scenario"
	novascenario "github.com/B42Labs/dizzy/internal/nova/scenario"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
)

// newMixChaosCmd builds "mix chaos": the combined churn run. It starts one
// churn engine per persona of the plan, each authenticated with the clouds.yaml
// entry its scenario block names and churning its share of the server envelope
// under the identity <runID>-<persona>, and one per enabled background lane,
// churning its service's plan under the identity <runID>-<lane>, all under one
// seed and one run id. Every lane is built and pre-checked before anything is
// created; then the lanes that need it provision what their engine starts
// from. It rewrites one run record with a per-persona and per-lane breakdown
// every chaosCheckpointInterval while it runs, records the run, and, unless
// --no-cleanup, tears every lane's resources down by identity and reports any
// leak. It mirrors "nova chaos" without the per-persona knob flags, which
// --set covers.
func newMixChaosCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		sets         []string
		noCleanup    bool
		f            chaosFlags
	)

	cmd := &cobra.Command{
		Use:   "chaos",
		Short: "Run one randomized churn engine per persona and per enabled lane",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, ls, p, err := buildMixPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			// Every persona's lane runs under a config of its own, built and
			// checked before any API call.
			cfgs, err := mixLaneConfigs(cmd, opts, s, f, p)
			if err != nil {
				return err
			}
			// Duration and unbounded mode are run-wide, so the first persona's
			// config stands for the run in the heartbeat and the iteration
			// outcome, and every background lane takes them from it.
			runCfg := cfgs[0]
			laneCfgs, err := mixServiceLaneConfigs(cmd, opts, ls, f, p, runCfg)
			if err != nil {
				return err
			}

			// Two-phase shutdown, as in nova chaos: the first Ctrl-C / SIGTERM
			// cancels the run so every engine stops and the teardown below runs,
			// and a second signal takes the default disposition and kills the
			// process.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()

			runID, err := newRunID()
			if err != nil {
				return err
			}

			overall := metrics.NewCollector()
			inputs, err := planLaneInputs(s, p, runID, overall)
			if err != nil {
				return err
			}
			var lanes []*mix.Lane
			defer func() {
				for _, l := range lanes {
					flushTelemetry(l.Telemetry)
				}
			}()
			for i, in := range inputs {
				in.persona, in.scenario, in.cfg = &p.Personas[i], p.Scenario, cfgs[i]
				l, err := buildMixLane(ctx, opts, in)
				if err != nil {
					return err
				}
				logColdMigrationOff(s, in.name)
				lanes = append(lanes, l)
			}
			for i, in := range planServiceLaneInputs(s, runID, overall) {
				in.lane, in.cfg = &p.Lanes[i], laneCfgs[i]
				l, err := buildServiceLane(ctx, opts, in)
				if err != nil {
					return err
				}
				lanes = append(lanes, l)
			}
			warnSharedProjects(lanes)

			// Every lane passed its pre-checks and nothing exists yet: only now
			// do the lanes that need it create what their engine starts from.
			if err := provisionLanes(ctx, lanes); err != nil {
				return err
			}

			for _, l := range lanes {
				slog.Info("starting churn run", "run", l.RunID, laneNoun(l), l.Name, "scenario", p.Scenario,
					"duration", chaosDurationLabel(l.Config), "minInterval", l.Config.MinInterval, "maxInterval", l.Config.MaxInterval,
					"maxParallel", l.Config.MaxParallel, "concurrency", l.Config.Concurrency)
			}

			start := time.Now()
			build := func(results map[string]*chaos.Result, finished time.Time) *run.Record {
				return buildMixRecord(p, lanes, results, overall, runID, start, finished)
			}

			hb := startHeartbeat(ctx, "churn in progress", collectorSnapshot(overall, start, "duration", chaosDurationLabel(runCfg)))
			results, runErr := mix.Run(ctx, lanes, chaos.RealClock{}, chaosCheckpoint(".", build))
			hb.stop()
			finished := time.Now()
			if runErr != nil {
				return fmt.Errorf("running churn (run %s): %w", runID, runErr)
			}
			rec := build(results, finished)

			interrupted := chaosInterrupted(ctx, runCfg)
			recordLaneIterations(ctx, lanes, finished.Sub(start), interrupted)

			out := cmd.OutOrStdout()
			if _, err := fmt.Fprint(out, rec.Metrics.Summary()); err != nil {
				return fmt.Errorf("writing metrics: %w", err)
			}
			if err := run.WritePersonaTable(out, rec.Personas); err != nil {
				return err
			}
			if err := run.WriteLaneTable(out, rec.Lanes); err != nil {
				return err
			}

			recordPath, werr := run.Write(".", rec)
			if werr != nil {
				slog.Error("writing run record failed; clean up by run id", "run", runID, "error", werr)
			} else if _, err := fmt.Fprintf(out, "run record written to %s\n", recordPath); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}

			return finishMixChurn(ctx, cmd, opts, lanes, rec.Created, runID, recordPath, scenarioPath, sets, interrupted, noCleanup)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set personas.ci.cloud=tenant-ci (repeatable)")
	flags.DurationVar(&f.duration, "duration", 0, "total wall-clock runtime of the churn; 0 runs until interrupted (required via flag or the scenario chaos block)")
	flags.DurationVar(&f.bucketWidth, "bucket-width", defaultChaosBucketWidth, "width of one time bucket in an unbounded run's time series, at least 1m")
	flags.IntVar(&f.maxParallel, "max-parallel", 0, "maximum concurrent in-flight churn operations of each persona and lane (default: --concurrency)")
	flags.BoolVar(&noCleanup, "no-cleanup", false, "leave every persona's and lane's resources in place — at the end of the run or on interrupt — instead of tearing them down by identity")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// mixLaneConfigs builds the churn config of every persona's lane of p, in plan
// order, each with the checkpoint interval set and validated.
func mixLaneConfigs(cmd *cobra.Command, opts *globalOptions, s mixscenario.Scenario, f chaosFlags, p *mixplan.Plan) ([]chaos.Config, error) {
	cfgs := make([]chaos.Config, len(p.Personas))
	for i := range p.Personas {
		cfg, err := mergeMixChaosConfig(cmd, opts, s, f, &p.Personas[i])
		if err != nil {
			return nil, err
		}
		cfg.CheckpointInterval = chaosCheckpointInterval
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		cfgs[i] = cfg
	}
	return cfgs, nil
}

// mergeMixChaosConfig builds the churn config of one persona's lane from three
// layers, lowest precedence first: built-in defaults, the scenario (the
// persona's block and the run-wide chaos block), and the dedicated flags (each
// one explicitly set). The run-wide part (duration, unbounded mode, bucket
// width, max parallel, concurrency and classifier) is the same for every
// persona. For ci the block sets the interval, churn ratio and target fill,
// and the mutate probability stays 0, since a CI server has no lifecycle
// operation. For gardener and legacy the block sets the interval. A long-lived
// persona runs with target fill 1, since it keeps every planned resource, and
// mutate probability 1, since its graph of pinned nodes only mutates once the
// creates are done; its churn ratio keeps the default, which such a graph never
// consults. A rolling persona runs with target fill 1, since it keeps its
// clusters whole; its churn ratio and mutate probability keep their defaults,
// since a graph of pinned and rolling nodes never draws a mutation and never
// reaches the create bias. A zero scenario value falls back to the default;
// --duration 0 given as a flag selects the unbounded mode. A persona this
// build does not define is an error.
func mergeMixChaosConfig(cmd *cobra.Command, opts *globalOptions, s mixscenario.Scenario, f chaosFlags, ps *mixplan.Persona) (chaos.Config, error) {
	cfg := chaos.Config{
		MinInterval: defaultChaosMinInterval,
		MaxInterval: defaultChaosMaxInterval,
		MaxParallel: opts.concurrency,
		ChurnRatio:  defaultChaosChurnRatio,
		TargetFill:  defaultChaosTargetFill,
		Concurrency: opts.concurrency,
		BucketWidth: defaultChaosBucketWidth,
		Classify:    novagraph.Classify,
	}

	var interval novascenario.Interval
	switch ps.Name {
	case "ci":
		ci := s.Personas.CI
		interval = ci.Interval
		if ci.ChurnRatio > 0 {
			cfg.ChurnRatio = ci.ChurnRatio
		}
		if ci.TargetFill > 0 {
			cfg.TargetFill = ci.TargetFill
		}
	case "gardener":
		interval = s.Personas.Gardener.Interval
	case "legacy":
		interval = s.Personas.Legacy.Interval
	default:
		return chaos.Config{}, fmt.Errorf("persona %q is not defined by this build of dizzy", ps.Name)
	}
	// A graph of pinned nodes only mutates once its creates are done.
	if ps.LongLived {
		cfg.TargetFill, cfg.ResizeRatio = 1, 1
	}
	// A rolling persona keeps its envelope full.
	if ps.Rolling {
		cfg.TargetFill = 1
	}
	if interval.Min > 0 {
		cfg.MinInterval = time.Duration(interval.Min)
	}
	if interval.Max > 0 {
		cfg.MaxInterval = time.Duration(interval.Max)
	}

	if c := s.Chaos; c != nil {
		if c.Duration > 0 {
			cfg.Duration = time.Duration(c.Duration)
		}
		if c.Parallel.Max > 0 {
			cfg.MaxParallel = c.Parallel.Max
		}
		if c.BucketWidth > 0 {
			cfg.BucketWidth = time.Duration(c.BucketWidth)
		}
	}

	if cmd.Flags().Changed("duration") {
		cfg.Duration = f.duration
		cfg.Unbounded = f.duration == 0
	}
	if cmd.Flags().Changed("bucket-width") {
		cfg.BucketWidth = f.bucketWidth
	}
	if cmd.Flags().Changed("max-parallel") {
		cfg.MaxParallel = f.maxParallel
	}
	return cfg, nil
}

// buildMixRecord builds the run record of a mix run as of finished, for the
// checkpoints written while the churn runs and for the final record. Its
// metrics are the overall collector's, the exact aggregate of every persona
// and lane, and it has no top-level chaos statistics: each persona and lane
// carries its own, because the percentiles of separate engines do not merge.
// A background lane gets an entry under lanes, every other lane one under
// personas. Created lists, in lane order, every lane's roots and then its
// live resources, each marked with the lane for a background lane and with
// the persona otherwise. A lane without a result yet (a checkpoint before its
// engine's first snapshot) has its collector's metrics, no chaos statistics
// and only its roots under created.
func buildMixRecord(p *mixplan.Plan, lanes []*mix.Lane, results map[string]*chaos.Result, overall *metrics.Collector, runID string, start, finished time.Time) *run.Record {
	wall := finished.Sub(start)
	rec := &run.Record{
		RunID:      runID,
		Service:    "mix",
		Scenario:   p.Scenario,
		Seed:       p.Seed,
		StartedAt:  start,
		FinishedAt: finished,
		Created:    []resource.Resource{},
		Metrics:    overall.Aggregate(wall),
		Services:   p.Services,
		Personas:   make([]run.PersonaStats, 0, len(lanes)),
	}
	for _, l := range lanes {
		r := results[l.Name]
		addCreated := func(created []resource.Resource) {
			for _, res := range created {
				if l.Background() {
					res.Lane = l.Name
				} else {
					res.Persona = l.Name
				}
				rec.Created = append(rec.Created, res)
			}
		}
		addCreated(l.Roots)
		var cs *run.ChaosStats
		if r != nil {
			cs = chaosStats(r)
			addCreated(r.Created)
		}

		m := l.Collector.Aggregate(wall)
		if l.Background() {
			rec.Lanes = append(rec.Lanes, run.LaneStats{
				Name:      l.Name,
				RunID:     l.RunID,
				Cloud:     l.Cloud,
				ProjectID: l.ProjectID,
				Scenario:  l.Scenario,
				Seed:      l.Seed,
				Metrics:   m,
				Chaos:     cs,
			})
			continue
		}
		ps := run.PersonaStats{
			Name:      l.Name,
			RunID:     l.RunID,
			Cloud:     l.Cloud,
			ProjectID: l.ProjectID,
			Metrics:   m,
			Chaos:     cs,
		}
		if l.Persona != nil {
			ps.Share, ps.Servers, ps.Seed = l.Persona.Share, l.Persona.Servers, l.Persona.Seed
		}
		rec.Personas = append(rec.Personas, ps)
	}
	return rec
}

// recordLaneIterations records each persona's and lane's churn as one
// iteration of its own telemetry resource, from the lane's own collector over
// wall; an interrupted run counts as a failed iteration.
func recordLaneIterations(ctx context.Context, lanes []*mix.Lane, wall time.Duration, interrupted bool) {
	for _, l := range lanes {
		m := l.Collector.Aggregate(wall)
		l.Telemetry.RecordIteration(ctx, m.Wall, !interrupted)
		l.Telemetry.RecordIterationOperations(ctx, m.Overall.Attempted, m.Overall.Succeeded, m.Overall.Failed)
	}
}

// finishMixChurn applies the teardown policy to every lane. Unless --no-cleanup
// is set, the run, whether it completed or was interrupted, tears each lane's
// resources down by its identity, in lane order, on a context.WithoutCancel of
// ctx so a first-signal interrupt does not kill the teardown it triggered. A
// failing lane does not stop the others; when any failed it returns their
// errors joined and runs no leak check. Otherwise it sums every lane's leak
// check, personas' and background lanes' alike, into the one line nova chaos
// prints. With --no-cleanup the resources are left in place and the cleanup
// hint is printed: by record when one was written, else by run id, scenario
// and every --set the run used, since those name the personas' and lanes'
// clouds. Either way it carries the run's --os-cloud, or the $OS_CLOUD it fell
// back to, the cloud of every persona and lane that names none, and
// shell-quotes what the operator passed in.
func finishMixChurn(ctx context.Context, cmd *cobra.Command, opts *globalOptions, lanes []*mix.Lane, created []resource.Resource, runID, recordPath, scenarioPath string, sets []string, interrupted, noCleanup bool) error {
	out := cmd.OutOrStdout()
	if noCleanup {
		reason := "churn complete"
		if interrupted {
			reason = "churn interrupted"
		}
		hint := "--run-id " + runID + " --scenario " + shellQuote(scenarioPath)
		for _, s := range sets {
			hint += " --set " + shellQuote(s)
		}
		if recordPath != "" {
			hint = "--run " + recordPath
		}
		if cloud := opts.cloudName(); cloud != "" {
			hint += " --os-cloud " + shellQuote(cloud)
		}
		_, err := fmt.Fprintf(out, "%s; resources left in place — reclaim with: mix cleanup %s\n", reason, hint)
		return err
	}

	tctx := context.WithoutCancel(ctx)
	if err := deleteLaneResources(tctx, out, lanes, created, "tearing down"); err != nil {
		return err
	}

	var leaked int
	for _, l := range lanes {
		n, err := l.Leaked(tctx)
		if err != nil {
			return fmt.Errorf("leak check for %s %q: %w", laneNoun(l), l.Name, err)
		}
		leaked += n
	}
	var err error
	if leaked > 0 {
		_, err = fmt.Fprintf(out, "leak check: %d run-tagged resource(s) still present after teardown\n", leaked)
	} else {
		_, err = fmt.Fprintf(out, "leak check: no run-tagged resources remain\n")
	}
	return err
}

// shellQuote quotes s for a POSIX shell: single quotes, with each embedded
// single quote closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
