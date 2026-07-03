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

	"github.com/B42Labs/openstack-tester/internal/chaos"
	"github.com/B42Labs/openstack-tester/internal/chaos/keystonegraph"
	"github.com/B42Labs/openstack-tester/internal/config"
	"github.com/B42Labs/openstack-tester/internal/keystone"
	keystoneexec "github.com/B42Labs/openstack-tester/internal/keystone/executor"
	keystonescenario "github.com/B42Labs/openstack-tester/internal/keystone/scenario"
	"github.com/B42Labs/openstack-tester/internal/metrics"
	"github.com/B42Labs/openstack-tester/internal/resource"
	"github.com/B42Labs/openstack-tester/internal/run"
	"github.com/B42Labs/openstack-tester/internal/telemetry"
)

// defaultChaosTokenRatio is the built-in per-step probability of issuing a token
// as a live, assigned user, used when neither the scenario chaos block nor the
// --token-ratio flag supplies one. The temporal defaults it joins live in
// chaos.go, shared with the neutron and cinder chaos commands.
const defaultChaosTokenRatio = 0.3

// newKeystoneChaosCmd builds "keystone chaos": a random churn/soak run that, for
// a configured duration, continuously creates and deletes projects, users, and
// role assignments within the stable domain/role scaffold, and occasionally
// issues a scoped token as a live, assigned user. It authenticates, runs the
// privilege pre-check, provisions the scaffold once, runs the churn, records the
// run, and — whether it completed or was interrupted, unless --no-cleanup —
// tears the resources down by name prefix and reports any leak. It mirrors
// "cinder chaos" with the identity-specific --token-ratio and privilege flags.
func newKeystoneChaosCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		sets         []string
		noCleanup    bool
		f            chaosFlags
		tokenRatio   float64
		priv         keystonePrivilegeFlags
	)

	cmd := &cobra.Command{
		Use:   "chaos",
		Short: "Run continuous randomized project/user/assignment churn within a scenario envelope",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, p, err := buildKeystonePlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			cfg := mergeKeystoneChaosConfig(cmd, opts, s, f, tokenRatio)
			cfg.Classify = keystonegraph.Classify
			if err := cfg.Validate(); err != nil {
				return err
			}

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

			tier, res, err := resolveKeystonePrivilege(ctx, gc, priv, p)
			if err != nil {
				return err
			}

			collector := metrics.NewCollector()
			client := keystone.New(gc, runID, collector)
			client.SetTelemetry(tel)

			// The domains and roles are the stable scaffold: provisioned once here
			// (created in admin mode, bound in domain-manager mode) so chaos behaves
			// identically in both tiers. The created roots join the run record so
			// teardown reclaims them.
			bindings, roots, err := keystoneexec.BindRoots(ctx, client, p, tier, res, opts.concurrency, opts.timeout)
			if err != nil {
				return fmt.Errorf("binding scaffold (run %s): %w", runID, err)
			}

			nodes, err := keystonegraph.Build(p, client, bindings, opts.timeout)
			if err != nil {
				return fmt.Errorf("building churn graph: %w", err)
			}

			slog.Info("starting churn run", "run", runID, "scenario", p.Scenario, "tier", tier,
				"duration", cfg.Duration, "minInterval", cfg.MinInterval, "maxInterval", cfg.MaxInterval,
				"maxParallel", cfg.MaxParallel, "tokenRatio", cfg.ResizeRatio, "concurrency", cfg.Concurrency)

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

			tel.RecordIteration(ctx, wall, ctx.Err() == nil)
			tel.RecordIterationOperations(ctx, agg.Overall.Attempted, agg.Overall.Succeeded, agg.Overall.Failed)

			if _, err := fmt.Fprint(cmd.OutOrStdout(), agg.Summary()); err != nil {
				return fmt.Errorf("writing metrics: %w", err)
			}

			created := append(append([]resource.Resource{}, roots...), result.Created...)
			rec := &run.Record{
				RunID:      runID,
				Service:    "keystone",
				Scenario:   p.Scenario,
				Seed:       p.Seed,
				StartedAt:  start,
				FinishedAt: finished,
				Created:    created,
				Metrics:    agg,
				Chaos:      chaosStats(result),
			}
			recordPath, werr := run.Write(".", rec)
			if werr != nil {
				slog.Error("writing run record failed; clean up by run id", "run", runID, "error", werr)
			} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "run record written to %s\n", recordPath); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}

			return finishKeystoneChurn(ctx, cmd, client, runID, recordPath, created, ctx.Err() != nil, noCleanup, opts.timeout)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.users=20 (repeatable)")
	flags.DurationVar(&f.duration, "duration", 0, "total wall-clock runtime of the churn (required via flag or the scenario chaos block)")
	flags.DurationVar(&f.minInterval, "min-interval", defaultChaosMinInterval, "minimum random delay between scheduled actions")
	flags.DurationVar(&f.maxInterval, "max-interval", defaultChaosMaxInterval, "maximum random delay between scheduled actions")
	flags.IntVar(&f.maxParallel, "max-parallel", 0, "maximum concurrent in-flight churn operations (default: --concurrency)")
	flags.Float64Var(&f.churnRatio, "churn-ratio", defaultChaosChurnRatio, "create bias at steady state, between 0 and 1")
	flags.Float64Var(&f.targetFill, "target-fill", defaultChaosTargetFill, "fraction of the envelope to keep populated on average, between 0 and 1")
	flags.Float64Var(&tokenRatio, "token-ratio", defaultChaosTokenRatio, "probability per churn step of issuing a token as a live, assigned user, between 0 and 1")
	flags.BoolVar(&noCleanup, "no-cleanup", false, "leave the identity resources in place — at the end of the run or on interrupt — instead of tearing them down by name prefix")
	priv.register(flags)
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// mergeKeystoneChaosConfig builds the churn config from three layers, lowest
// precedence first: built-in defaults, the scenario's chaos block (each non-zero
// field), and the dedicated flags (each one explicitly set). A zero field in the
// chaos block falls back to the default; to set a field to zero use the flag —
// except token_ratio, a pointer whose explicit 0 disables token issues while an
// omitted key falls back to the default. It maps the identity-specific
// token_ratio onto the engine's generic mutate-draw probability
// (chaos.Config.ResizeRatio), which the neutron/cinder code the issue leaves
// untouched keeps its own name for.
func mergeKeystoneChaosConfig(cmd *cobra.Command, opts *globalOptions, s keystonescenario.Scenario, f chaosFlags, tokenRatio float64) chaos.Config {
	cfg := chaos.Config{
		MinInterval: defaultChaosMinInterval,
		MaxInterval: defaultChaosMaxInterval,
		MaxParallel: opts.concurrency,
		ChurnRatio:  defaultChaosChurnRatio,
		TargetFill:  defaultChaosTargetFill,
		ResizeRatio: defaultChaosTokenRatio,
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
		if c.TokenRatio != nil {
			cfg.ResizeRatio = *c.TokenRatio
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
	if cmd.Flags().Changed("token-ratio") {
		cfg.ResizeRatio = tokenRatio
	}
	return cfg
}

// finishKeystoneChurn applies the teardown policy. Unless --no-cleanup is set,
// the run — whether it completed or was interrupted — tears the identity
// resources down by name prefix (each op bounded by opTimeout) and runs a leak
// check; teardown and the leak check run on a context.WithoutCancel of ctx so a
// first-signal interrupt does not kill the teardown it triggered. With
// --no-cleanup the resources are left in place and the cleanup hint is printed.
func finishKeystoneChurn(ctx context.Context, cmd *cobra.Command, c keystoneexec.Cleaner, runID, recordPath string, created []resource.Resource, interrupted, noCleanup bool, opTimeout time.Duration) error {
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
		_, err := fmt.Fprintf(out, "%s; resources left in place — reclaim with: keystone cleanup %s\n", reason, hint)
		return err
	}

	tctx := context.WithoutCancel(ctx)
	cleaner := keystoneTimeoutCleaner{c, opTimeout}

	deleted, cleanupErr := keystoneexec.Cleanup(tctx, cleaner, runID, created, opTimeout)
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("tearing down run %s: %w", runID, cleanupErr)
	}

	leaked, err := keystoneLeakCheck(tctx, cleaner, runID)
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

// keystoneLeakCheck counts the domains, roles, projects, and users still
// carrying the run's name prefix (projects by tag) after teardown. It takes the
// keystoneexec.Cleaner seam so the listing shares the teardown's per-op timeout
// bound.
func keystoneLeakCheck(ctx context.Context, c keystoneexec.Cleaner, runID string) (int, error) {
	var total int
	for _, kind := range []struct {
		name string
		list func(context.Context, string) ([]resource.Resource, error)
	}{
		{"projects", c.ListProjectsByTag},
		{"users", c.ListUsersByPrefix},
		{"roles", c.ListRolesByPrefix},
		{"domains", c.ListDomainsByPrefix},
	} {
		found, err := kind.list(ctx, runID)
		if err != nil {
			return total, fmt.Errorf("leak check listing %s: %w", kind.name, err)
		}
		total += len(found)
	}
	return total, nil
}
