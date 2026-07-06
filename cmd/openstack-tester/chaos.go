package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/chaos/neutrongraph"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/executor"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/neutron"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/scenario"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// Built-in defaults for the chaos knobs, used when neither the scenario chaos
// block nor a flag supplies a value. There is no default duration: it must be
// set by the chaos block or the --duration flag.
const (
	defaultChaosMinInterval = 200 * time.Millisecond
	defaultChaosMaxInterval = 3 * time.Second
	defaultChaosChurnRatio  = 0.5
	defaultChaosTargetFill  = 0.8
)

// chaosFlags holds the dedicated chaos flag values; whether each one overrides
// the scenario block is decided by cmd.Flags().Changed.
type chaosFlags struct {
	duration    time.Duration
	minInterval time.Duration
	maxInterval time.Duration
	maxParallel int
	churnRatio  float64
	targetFill  float64
}

// newChaosCmd builds "neutron chaos": a random churn/soak run that, for a
// configured duration, continuously creates and deletes Neutron resources at
// random intervals and parallelism, bounded by the scenario as the spatial
// envelope. It authenticates, pre-checks quota against the full plan, runs the
// churn, records the run, and — whether it completed or was interrupted, unless
// --no-cleanup — tears the topology down by tag and reports any leak.
func newChaosCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath    string
		sets            []string
		externalNetwork string
		noCleanup       bool
		f               chaosFlags
	)

	cmd := &cobra.Command{
		Use:   "chaos",
		Short: "Run continuous randomized create/delete churn within a scenario envelope",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, p, err := buildPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			cfg := mergeChaosConfig(cmd, opts, s, f)
			cfg.Classify = neutrongraph.Classify
			if err := cfg.Validate(); err != nil {
				return err
			}

			// Two-phase shutdown: the first Ctrl-C / SIGTERM cancels the run so the
			// churn stops and its resources unwind instead of being killed, then the
			// teardown below tears the topology down. Unregistering the handler right
			// after means a second signal takes the default disposition and kills the
			// process — there is always a hard way out mid-teardown.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()

			// Set up OTEL export (a no-op unless --otel is set) and flush it on
			// exit so an ad-hoc churn run lands in the same database as monitor.
			tel, err := telemetry.Setup(ctx, telemetry.Config{
				Enabled: opts.otel, Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "neutron",
			})
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)

			runID, err := newRunID()
			if err != nil {
				return err
			}

			gc, err := config.NewNetworkClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating network client: %w", err)
			}

			extNet, haveExternal, err := neutron.FindExternalNetwork(ctx, gc, externalNetwork)
			if err != nil {
				return err
			}
			var externalNetworkID string
			switch {
			case haveExternal:
				externalNetworkID = extNet.ID
				slog.Info("using external network for gateways and floating IPs", "id", extNet.ID, "name", extNet.Name)
			case p.RoutersWithExternalGateway() > 0 || len(p.FloatingIPs) > 0:
				slog.Warn("plan wants external connectivity but no external network was found; gateways and floating IPs will be skipped",
					"externalGatewayRouters", p.RoutersWithExternalGateway(), "floatingIPs", len(p.FloatingIPs))
			}

			// The envelope is the population's worst case, so quota is pre-checked
			// against the full plan exactly as apply does.
			if err := neutron.PrecheckQuota(ctx, gc, p, haveExternal); err != nil {
				return err
			}

			collector := metrics.NewCollector()
			client := neutron.New(gc, runID, collector)
			client.SetTelemetry(tel)

			nodes, err := neutrongraph.Build(p, externalNetworkID, client, opts.timeout)
			if err != nil {
				return fmt.Errorf("building churn graph: %w", err)
			}

			slog.Info("starting churn run", "run", runID, "scenario", p.Scenario,
				"duration", cfg.Duration, "minInterval", cfg.MinInterval, "maxInterval", cfg.MaxInterval,
				"maxParallel", cfg.MaxParallel, "concurrency", cfg.Concurrency)

			start := time.Now()
			hb := startHeartbeat(ctx, "churn in progress", collectorSnapshot(collector, start, "duration", cfg.Duration))
			result, runErr := chaos.Run(ctx, nodes, p.Seed, cfg, chaos.RealClock{})
			hb.stop()
			finished := time.Now()
			if runErr != nil {
				return fmt.Errorf("running churn (run %s): %w", runID, runErr)
			}
			wall := finished.Sub(start)
			agg := collector.Aggregate(wall)

			// A churn run is a single iteration: export the same per-iteration
			// summary metrics from the pre-teardown aggregate, mirroring the run
			// record. An interrupted run counts as a failed iteration.
			tel.RecordIteration(ctx, wall, ctx.Err() == nil)
			tel.RecordIterationOperations(ctx, agg.Overall.Attempted, agg.Overall.Succeeded, agg.Overall.Failed)

			if _, err := fmt.Fprint(cmd.OutOrStdout(), agg.Summary()); err != nil {
				return fmt.Errorf("writing metrics: %w", err)
			}

			// Persist the run record before teardown so the resources still live
			// stay reclaimable (by tag, and address scopes by id) even if teardown
			// fails partway or the operator wants to inspect them.
			rec := &run.Record{
				RunID:      runID,
				Service:    "neutron",
				Scenario:   p.Scenario,
				Seed:       p.Seed,
				StartedAt:  start,
				FinishedAt: finished,
				Created:    result.Created,
				Metrics:    agg,
				Chaos:      chaosStats(result),
			}
			recordPath, werr := run.Write(".", rec)
			if werr != nil {
				slog.Error("writing run record failed; clean up by run id", "run", runID, "error", werr)
			} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "run record written to %s\n", recordPath); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}

			return finishChurn(ctx, cmd, client, runID, recordPath, result.Created, ctx.Err() != nil, noCleanup, opts.timeout)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.networks=200 (repeatable)")
	flags.DurationVar(&f.duration, "duration", 0, "total wall-clock runtime of the churn (required via flag or the scenario chaos block)")
	flags.DurationVar(&f.minInterval, "min-interval", defaultChaosMinInterval, "minimum random delay between scheduled actions")
	flags.DurationVar(&f.maxInterval, "max-interval", defaultChaosMaxInterval, "maximum random delay between scheduled actions")
	flags.IntVar(&f.maxParallel, "max-parallel", 0, "maximum concurrent in-flight churn operations (default: --concurrency)")
	flags.Float64Var(&f.churnRatio, "churn-ratio", defaultChaosChurnRatio, "create bias at steady state, between 0 and 1")
	flags.Float64Var(&f.targetFill, "target-fill", defaultChaosTargetFill, "fraction of the envelope to keep populated on average, between 0 and 1")
	flags.BoolVar(&noCleanup, "no-cleanup", false, "leave the topology in place — at the end of the run or on interrupt — instead of tearing it down by tag")
	flags.StringVar(&externalNetwork, "external-network", "", "name of the external network for gateways and floating IPs (default: auto-detect the first external network)")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// mergeChaosConfig builds the churn config from three layers, lowest precedence
// first: built-in defaults, the scenario's chaos block (each non-zero field), and
// the dedicated flags (each one explicitly set). A zero field in the chaos block
// falls back to the default; to set a field to zero use the flag.
func mergeChaosConfig(cmd *cobra.Command, opts *globalOptions, s scenario.Scenario, f chaosFlags) chaos.Config {
	cfg := chaos.Config{
		MinInterval: defaultChaosMinInterval,
		MaxInterval: defaultChaosMaxInterval,
		MaxParallel: opts.concurrency,
		ChurnRatio:  defaultChaosChurnRatio,
		TargetFill:  defaultChaosTargetFill,
		Concurrency: opts.concurrency,
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
	}

	if cmd.Flags().Changed("duration") {
		cfg.Duration = f.duration
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
	return cfg
}

// finishChurn applies the teardown policy. Unless --no-cleanup is set, the run —
// whether it completed or was interrupted — tears the topology down by tag and
// runs a leak check, exactly as a completed run always has; teardown and the
// leak check run on a context.WithoutCancel of ctx so a first-signal interrupt
// does not kill the teardown it triggered, and each op is bounded by opTimeout
// so a wedged call cannot hang shutdown. With --no-cleanup the topology is left
// in place and the cleanup hint is printed so an operator can inspect it and
// reclaim it later — interrupt-to-inspect is now this explicit opt-out.
func finishChurn(ctx context.Context, cmd *cobra.Command, c executor.Cleaner, runID, recordPath string, created []neutron.Resource, interrupted, noCleanup bool, opTimeout time.Duration) error {
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
		_, err := fmt.Fprintf(out, "%s; resources left in place — reclaim with: neutron cleanup %s\n", reason, hint)
		return err
	}

	// The signal context is already cancelled on interrupt; run teardown on a
	// context.WithoutCancel with a per-op bound so the first signal triggers it
	// and a second signal (the hard abort) is the only thing that kills it.
	tctx := context.WithoutCancel(ctx)
	cleaner := timeoutCleaner{c, opTimeout}

	deleted, cleanupErr := executor.Cleanup(tctx, cleaner, runID, created)
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("tearing down run %s: %w", runID, cleanupErr)
	}

	leaked, err := leakCheck(tctx, cleaner, runID)
	if err != nil {
		return err
	}
	if leaked > 0 {
		_, err = fmt.Fprintf(out, "leak check: %d run-tagged resource(s) still present after teardown\n", leaked)
	} else {
		_, err = fmt.Fprintf(out, "leak check: no run-tagged resources remain\n")
	}
	return err
}

// leakCheck counts the resources still carrying the run tag after teardown,
// across every tag-discoverable kind. Address scopes cannot be discovered by
// tag (they are reclaimed from the run record by id), so they are not counted
// here — the same limitation cleanup itself has. It takes the executor.Cleaner
// seam so the listing shares the teardown's per-op timeout bound.
func leakCheck(ctx context.Context, c executor.Cleaner, runID string) (int, error) {
	kinds := []neutron.Kind{
		neutron.KindFloatingIP, neutron.KindPort, neutron.KindNetwork, neutron.KindSubnet,
		neutron.KindRouter, neutron.KindSecurityGroup, neutron.KindSubnetPool,
	}
	var total int
	for _, kind := range kinds {
		found, err := c.ListByTag(ctx, kind, runID)
		if err != nil {
			return total, fmt.Errorf("leak check listing %s: %w", kind, err)
		}
		total += len(found)
	}
	return total, nil
}

// chaosStats maps the engine result onto the persisted run-record schema.
func chaosStats(r *chaos.Result) *run.ChaosStats {
	cs := &run.ChaosStats{
		Creates:    r.Creates,
		Deletes:    r.Deletes,
		Mutates:    r.Mutates,
		Cycles:     r.Cycles,
		PopMin:     r.PopMin,
		PopMax:     r.PopMax,
		PopMean:    r.PopMean,
		TargetFill: r.TargetFill,
	}
	for _, b := range r.Buckets {
		cs.Buckets = append(cs.Buckets, run.ChaosBucket{Start: b.Start, Stats: b.Stats, Errors: b.Errors})
	}
	return cs
}
