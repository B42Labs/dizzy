package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneexec "github.com/B42Labs/dizzy/internal/keystone/executor"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// newKeystoneMonitorCmd builds "keystone monitor": the identity counterpart to
// "neutron monitor" and "cinder monitor". It drives the same service-agnostic
// loop (runMonitorLoop, runIteration) with a Keystone runOnce that composes the
// single-shot pipeline — opt-in pre-flight sweep (--reclaim-orphans) -> apply ->
// cleanup — continuously (the default) or on a fixed cadence, unattended, exporting the
// same per-operation and per-iteration metrics (via --otel, tagged
// service=keystone). The privilege pre-check and domain-manager resolution run
// once at startup so a wrong tier fails fast before the loop begins. An
// iteration's cleanup retries transient failures for up to
// --cleanup-retry-budget; an iteration whose cleanup still fails is kept in
// memory (at most maxPendingKeystoneCleanups) and cleaned up again by its run
// id before each later iteration, and any still pending at shutdown is logged
// with the command that reclaims it.
func newKeystoneMonitorCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath       string
		sets               []string
		keepRunRecords     bool
		reclaimOrphans     bool
		cleanupRetryBudget time.Duration
		cfg                monitorConfig
		priv               keystonePrivilegeFlags
	)

	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Run sweep→apply→cleanup identity iterations continuously or on a cadence and export metrics",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildKeystonePlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}
			if err := cfg.validate(); err != nil {
				return err
			}
			if cleanupRetryBudget < 0 {
				return fmt.Errorf("--cleanup-retry-budget must not be negative, got %s", cleanupRetryBudget)
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()

			tel, err := setupTelemetry(ctx, opts, telemetry.Config{
				Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "keystone",
			})
			if err != nil {
				return fmt.Errorf("setting up telemetry: %w", err)
			}
			defer flushTelemetry(tel)
			cfg.health = opts.health()

			// One startup client runs the read-only privilege pre-check and, in
			// domain-manager mode, resolves the in-scope domain and reused roles, so
			// a wrong tier fails fast before the loop; each iteration authenticates
			// its own client (see the runOnce closure) so token expiry over a
			// multi-day loop fails one iteration rather than dead-looping.
			gc, err := config.NewIdentityClient(ctx, opts.osCloud)
			if err != nil {
				return fmt.Errorf("creating identity client: %w", err)
			}
			tier, res, err := resolveKeystonePrivilege(ctx, gc, priv, p)
			if err != nil {
				return err
			}

			// 0 means continuous; log it as such so the startup line explains itself.
			interval := cfg.interval.String()
			if cfg.interval == 0 {
				interval = "continuous"
			}
			slog.Info("starting monitor", "scenario", p.Scenario, "interval", interval,
				"iterations", cfg.iterations, "errorWait", cfg.errorWait, "tier", tier, "otel", opts.otel)

			// The plan is expanded once at startup, so every iteration reuses the
			// same seed and topology: comparable across time (the issue's default).
			pending := &keystonePendingCleanups{max: maxPendingKeystoneCleanups}
			runOnce := keystoneMonitorRunOnce(opts, p, tel, tier, res, keepRunRecords, reclaimOrphans, cleanupRetryBudget, pending)

			iterations, failures := runMonitorLoop(ctx, cfg, chaos.RealClock{}, runOnce)
			pending.logRemaining()
			slog.Info("monitor finished", "iterations", iterations, "failures", failures)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.users=20 (repeatable)")
	flags.DurationVar(&cfg.interval, "interval", 0, "target cadence between iteration starts, e.g. 15m; a longer iteration starts the next immediately (default 0 = continuous: iterations run back-to-back)")
	flags.IntVar(&cfg.iterations, "iterations", 0, "stop after this many iterations (0 = run forever)")
	flags.DurationVar(&cfg.errorWait, "error-wait", 0, "extra pause after a failed iteration before the next starts (0 = off)")
	flags.BoolVar(&keepRunRecords, "keep-run-records", false, "write a run-<id>.json per iteration (off by default: in monitor mode they accumulate unboundedly)")
	flags.DurationVar(&cleanupRetryBudget, "cleanup-retry-budget", 5*time.Minute, "how long one iteration's cleanup may retry list and delete calls that fail with a 5xx, 429, timeout, or connection error (0 = one attempt per call)")
	flags.BoolVar(&reclaimOrphans, "reclaim-orphans", false, "before each iteration, delete leftover dizzy- identity resources across ALL tester runs cloud-wide (off by default); only safe when no other tester process targets this cloud, since with an admin token it deletes a concurrent run's in-flight users, roles, and whole domains")
	priv.register(flags)
	// MarkFlagRequired only fails for an unknown flag; scenario was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// keystoneMonitorRunOnce builds the production per-iteration closure the loop
// drives. Each iteration gets a fresh run id, a fresh metrics collector, and its
// own authenticated client. It first cleans up again the earlier iterations
// whose cleanup failed, each by its own run id with one attempt per call,
// recorded into a collector of their own so the iteration's op counts and wall
// time stay comparable; a signal during that reclaim ends the iteration before
// it starts. It then runs the opt-in pre-flight orphan sweep (only
// with --reclaim-orphans; see keystonePreflight), applies the plan in the
// startup-fixed tier (admin mode recreates its roots per iteration under the
// fresh run id; domain-manager mode reuses the startup resolution), and cleans
// up, retrying transient failures for up to cleanupRetryBudget and adding the
// iteration to pending when its cleanup still fails (keystoneCleanupStep),
// then records the per-iteration summary metrics and logs a one-line summary.
func keystoneMonitorRunOnce(opts *globalOptions, p *keystoneplan.Plan, tel *telemetry.Telemetry, tier keystone.Tier, res keystone.Resolution, keepRunRecords, reclaimOrphans bool, cleanupRetryBudget time.Duration, pending *keystonePendingCleanups) func(ctx context.Context, iter int) bool {
	return func(ctx context.Context, iter int) bool {
		runID, err := newRunID()
		if err != nil {
			slog.Error("generating run id failed; skipping iteration", "iteration", iter, "error", err)
			return false
		}
		gc, err := config.NewIdentityClient(ctx, opts.osCloud)
		if err != nil {
			slog.Error("iteration authentication failed", "iteration", iter, "run", runID, "error", err)
			return false
		}
		collector := metrics.NewCollector()
		client := keystone.New(gc, runID, collector)
		client.SetTelemetry(tel)

		// A client's run id only names creates; each pending run id reaches
		// Cleanup as its argument.
		reclaimClient := keystone.New(gc, runID, metrics.NewCollector())
		reclaimClient.SetTelemetry(tel)
		pending.reclaim(ctx, func(ctx context.Context, id string, recorded []resource.Resource) (int, error) {
			return keystoneexec.Cleanup(ctx, keystoneTimeoutCleaner{reclaimClient, opts.timeout}, id, recorded, opts.timeout)
		})
		// A signal during the reclaim ends the iteration here: nothing of it
		// exists yet, and Apply must not start on a cancelled context.
		if ctx.Err() != nil {
			return false
		}

		start := time.Now()
		hb := startHeartbeat(ctx, "monitor iteration in progress",
			collectorSnapshot(collector, start, "iteration", iter, "run", runID))
		result := runIteration(ctx, iterationDeps{
			preflight: keystonePreflight(client, opts.timeout, runID, reclaimOrphans),
			apply: func(ctx context.Context) ([]resource.Resource, error) {
				r, err := keystoneexec.Apply(ctx, client, p, tier, res, opts.concurrency, opts.timeout)
				return r.Created, err
			},
			cleanup: keystoneCleanupStep(pending, runID, func(ctx context.Context, created []resource.Resource) (int, error) {
				return keystoneexec.CleanupWithRetry(ctx, keystoneTimeoutCleaner{client, opts.timeout}, runID, created, opts.timeout, cleanupRetryBudget)
			}),
		})
		hb.stop()

		wall := time.Since(start)
		agg := collector.Aggregate(wall)
		tel.RecordIteration(context.WithoutCancel(ctx), wall, result.ok)
		tel.RecordIterationOperations(context.WithoutCancel(ctx),
			agg.Overall.Attempted, agg.Overall.Succeeded, agg.Overall.Failed)

		if keepRunRecords {
			rec := &run.Record{
				RunID:      runID,
				Service:    "keystone",
				Scenario:   p.Scenario,
				Seed:       p.Seed,
				StartedAt:  start,
				FinishedAt: start.Add(wall),
				Created:    result.created,
				Metrics:    agg,
			}
			if result.err != nil {
				rec.Error = result.err.Error()
			}
			writeIterationRecord(rec)
		}

		attrs := []any{
			"iteration", iter, "run", runID,
			"duration", wall.Round(time.Millisecond),
			"ok", result.ok, "ops", agg.Overall.Attempted, "failed", agg.Overall.Failed,
			"swept", result.swept, "deleted", result.deleted,
			"pending", pending.len(),
		}
		if result.err != nil {
			attrs = append(attrs, "error", result.err.Error())
		}
		slog.Info("iteration complete", attrs...)
		return result.ok
	}
}

// maxPendingKeystoneCleanups caps how many iterations whose cleanup failed
// keystone monitor keeps for a later cleanup; past it, the oldest is dropped.
const maxPendingKeystoneCleanups = 20

// keystonePendingCleanup is one iteration whose cleanup failed: its run id and
// the resources its apply recorded.
type keystonePendingCleanup struct {
	runID    string
	recorded []resource.Resource
}

// keystonePendingCleanups holds the iterations whose cleanup failed, oldest
// first, so each later iteration can clean them up again by their run ids,
// which stays safe beside a concurrent run, unlike --reclaim-orphans. It is
// kept in memory only, so a restart forgets it. It is used only from the
// monitor loop's single goroutine, so it takes no lock.
type keystonePendingCleanups struct {
	max     int
	entries []keystonePendingCleanup
}

// add appends an iteration whose cleanup failed. When that exceeds max, the
// oldest entry is dropped and logged with the command that reclaims it.
func (p *keystonePendingCleanups) add(runID string, recorded []resource.Resource) {
	p.entries = append(p.entries, keystonePendingCleanup{runID: runID, recorded: recorded})
	if len(p.entries) > p.max {
		id := p.entries[0].runID
		p.entries = slices.Delete(p.entries, 0, 1)
		slog.Error("too many pending cleanups; dropping the oldest", "run", id, "reclaim", "dizzy keystone cleanup --run-id "+id)
	}
}

// reclaim runs clean for each entry, oldest first, and stops before the next
// one once ctx is done. An entry whose cleanup succeeds is removed; one whose
// cleanup fails is kept for the next iteration.
func (p *keystonePendingCleanups) reclaim(ctx context.Context, clean func(ctx context.Context, runID string, recorded []resource.Resource) (int, error)) {
	var kept []keystonePendingCleanup
	for i, e := range p.entries {
		if ctx.Err() != nil {
			kept = append(kept, p.entries[i:]...)
			break
		}
		n, err := clean(ctx, e.runID, e.recorded)
		if err != nil {
			slog.Warn("pending cleanup failed; retrying before the next iteration", "run", e.runID, "deleted", n, "error", err)
			kept = append(kept, e)
			continue
		}
		slog.Info("pending cleanup reclaimed", "run", e.runID, "deleted", n)
	}
	p.entries = kept
}

// len returns how many iterations are pending a cleanup.
func (p *keystonePendingCleanups) len() int { return len(p.entries) }

// logRemaining logs each iteration still pending a cleanup, with the command
// that reclaims it, since a restart forgets them.
func (p *keystonePendingCleanups) logRemaining() {
	for _, e := range p.entries {
		slog.Warn("cleanup still pending at shutdown", "run", e.runID, "reclaim", "dizzy keystone cleanup --run-id "+e.runID)
	}
}

// keystoneCleanupStep wraps one iteration's cleanup: when clean fails, the
// iteration is added to pending, so a later iteration cleans it up again. It
// returns clean's result unchanged, so runIteration stays shared with every
// other namespace.
func keystoneCleanupStep(pending *keystonePendingCleanups, runID string, clean func(ctx context.Context, created []resource.Resource) (int, error)) func(ctx context.Context, created []resource.Resource) (int, error) {
	return func(ctx context.Context, created []resource.Resource) (int, error) {
		n, err := clean(ctx, created)
		if err != nil {
			pending.add(runID, created)
			slog.Warn("cleanup failed; retrying it before the next iteration", "run", runID, "pending", pending.len(), "error", err)
		}
		return n, err
	}
}

// keystonePreflight builds one iteration's pre-flight sweep. The sweep reclaims
// tester leftovers across every run cloud-wide — by the any-run dizzy- name
// prefix and the dizzy:type=project tag — so it is opt-in via --reclaim-orphans
// and off by default: with an admin token those listings are unscoped, so an
// unconditional sweep would delete a concurrent tester run's in-flight identity
// resources (users and roles are cloud-global, and disabling then deleting an
// dizzy- domain cascades away its remaining contents). Off, it is a no-op that
// issues no identity calls, so a monitor loop reclaims only its own iterations —
// each iteration's own run-scoped cleanup still runs regardless.
func keystonePreflight(client *keystone.Client, opTimeout time.Duration, runID string, reclaimOrphans bool) func(ctx context.Context) (int, error) {
	if !reclaimOrphans {
		return func(context.Context) (int, error) { return 0, nil }
	}
	return func(ctx context.Context) (int, error) {
		// The fresh run id satisfies Cleanup's non-empty guard; the orphan adapter
		// ignores it and discovers by the any-run name prefix / type tag.
		return keystoneexec.Cleanup(ctx, keystoneTimeoutCleaner{keystoneOrphanCleaner{client}, opTimeout}, runID, nil, opTimeout)
	}
}

// keystoneOrphanCleaner adapts a Keystone client to the keystoneexec.Cleaner
// seam for the pre-flight sweep, discovering leftovers across every tester run
// (by the any-run name prefix, and by the dizzy:type=project tag for
// projects) instead of one run's prefix. It is the identity analog of Cinder's
// orphanCleaner, so the sweep reuses keystoneexec.Cleanup's exact reverse-order
// teardown unchanged. ListAssignmentsForUser, DisableDomain, and Delete promote
// from the embedded client.
type keystoneOrphanCleaner struct{ *keystone.Client }

// ListProjectsByTag ignores the run id and lists by the type tag.
func (o keystoneOrphanCleaner) ListProjectsByTag(ctx context.Context, _ string) ([]resource.Resource, error) {
	return o.ListProjectsByType(ctx)
}

// ListUsersByPrefix ignores the run id and lists by the any-run name prefix.
func (o keystoneOrphanCleaner) ListUsersByPrefix(ctx context.Context, _ string) ([]resource.Resource, error) {
	return o.ListUsersByAnyRun(ctx)
}

// ListRolesByPrefix ignores the run id and lists by the any-run name prefix.
func (o keystoneOrphanCleaner) ListRolesByPrefix(ctx context.Context, _ string) ([]resource.Resource, error) {
	return o.ListRolesByAnyRun(ctx)
}

// ListDomainsByPrefix ignores the run id and lists by the any-run name prefix.
func (o keystoneOrphanCleaner) ListDomainsByPrefix(ctx context.Context, _ string) ([]resource.Resource, error) {
	return o.ListDomainsByAnyRun(ctx)
}
