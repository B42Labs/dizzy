package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneexec "github.com/B42Labs/dizzy/internal/keystone/executor"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// newKeystoneApplyCmd builds "keystone apply". With --dry-run it expands the
// scenario into a plan and prints a summary without making any API calls.
// Without --dry-run it authenticates, runs the read-only privilege pre-check
// (which replaces the quota pre-check Neutron/Cinder run), creates domains,
// roles, projects, users, assigns roles, and issues tokens, then prints the
// collected timing metrics. On Ctrl-C / SIGTERM the run record is written and
// the partial topology is torn down (assignments, users, projects, then roles
// and domains); --keep-on-abort leaves it in place with the cleanup hint; a
// second signal aborts hard.
func newKeystoneApplyCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		dryRun       bool
		sets         []string
		keepOnAbort  bool
		priv         keystonePrivilegeFlags
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Create domains, roles, projects, users, assign roles, issue tokens, and record a run",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildKeystonePlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			if dryRun {
				if _, err := fmt.Fprint(cmd.OutOrStdout(), p.Summary()); err != nil {
					return fmt.Errorf("writing summary: %w", err)
				}
				return nil
			}

			// Two-phase shutdown: the first Ctrl-C / SIGTERM cancels the run so
			// in-flight operations unwind instead of being killed, then the abort
			// epilogue tears down what was created. Unregistering the handler right
			// after means a second signal takes the default disposition and kills the
			// process — there is always a hard way out mid-teardown.
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

			// The privilege pre-check runs before any write, replacing the quota
			// pre-check: an admin proceeds with the full plan; a domain manager binds
			// to its in-scope domain and reused roles; neither fails fast.
			tier, res, err := resolveKeystonePrivilege(ctx, gc, priv, p)
			if err != nil {
				return err
			}

			collector := metrics.NewCollector()
			client := keystone.New(gc, runID, collector)
			client.SetTelemetry(tel)

			slog.Info("applying plan", "run", runID, "scenario", p.Scenario, "tier", tier,
				"domains", len(p.Domains), "roles", len(p.Roles), "projects", len(p.Projects),
				"users", len(p.Users), "assignments", len(p.Assignments), "tokens", len(p.Tokens),
				"concurrency", opts.concurrency)

			start := time.Now()
			hb := startHeartbeat(ctx, "apply in progress", collectorSnapshot(collector, start))
			result, applyErr := keystoneexec.Apply(ctx, client, p, tier, res, opts.concurrency, opts.timeout)
			hb.stop()
			finished := time.Now()
			wall := finished.Sub(start)
			agg := collector.Aggregate(wall)

			tel.RecordIteration(ctx, wall, applyErr == nil)
			tel.RecordIterationOperations(ctx, agg.Overall.Attempted, agg.Overall.Succeeded, agg.Overall.Failed)

			if _, err := fmt.Fprint(cmd.OutOrStdout(), agg.Summary()); err != nil {
				return fmt.Errorf("writing metrics: %w", err)
			}

			rec := &run.Record{
				RunID:      runID,
				Service:    "keystone",
				Scenario:   p.Scenario,
				Seed:       p.Seed,
				StartedAt:  start,
				FinishedAt: finished,
				Created:    result.Created,
				Metrics:    agg,
			}
			if applyErr != nil {
				rec.Error = applyErr.Error()
			}

			// An interrupted run tears itself down by default: write the record
			// first, then delete the partial topology. A successful apply keeps its
			// resources for the status/report/cleanup workflow.
			if runAborted(ctx, applyErr) {
				return finishAbortedKeystoneApply(ctx, cmd.OutOrStdout(), client, runID, result.Created, keepOnAbort, applyErr, opts.timeout,
					func() (string, error) { return writeAbortedRunRecord(cmd.OutOrStdout(), rec) })
			}

			recordPath, werr := run.Write(".", rec)
			if werr != nil {
				slog.Error("writing run record failed; clean up by run id", "run", runID, "error", werr)
			} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "run record written to %s\n", recordPath); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}

			if applyErr != nil {
				return fmt.Errorf("applying plan (run %s): %w", runID, applyErr)
			}
			if werr != nil {
				return fmt.Errorf("apply succeeded but writing run record failed (run %s): %w", runID, werr)
			}

			slog.Info("apply complete", "run", runID, "created", len(result.Created), "wall", wall)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.BoolVar(&dryRun, "dry-run", false, "validate the scenario and print the plan summary without making API calls")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.users=20 (repeatable)")
	flags.BoolVar(&keepOnAbort, "keep-on-abort", false, "on interrupt, leave already-created resources in place and print the cleanup hint instead of tearing them down")
	priv.register(flags)
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// finishAbortedKeystoneApply tears down what an interrupted keystone apply
// created, or — with keepOnAbort — leaves it in place with the reclaim hint. It
// is the identity twin of finishAbortedCinderApply: writeRecord runs first so a
// teardown that fails partway, or a second-signal hard abort during it, still
// leaves a record to reclaim from; teardown runs on a context.WithoutCancel of
// ctx (the signal context is already cancelled) with each op bounded by
// opTimeout, in reverse dependency order. Every return is a non-nil error naming
// the run id, so the command exits non-zero.
func finishAbortedKeystoneApply(ctx context.Context, out io.Writer, c keystoneexec.Cleaner, runID string, created []resource.Resource, keepOnAbort bool, applyErr error, opTimeout time.Duration, writeRecord func() (string, error)) error {
	recordPath, _ := writeRecord()
	hint := "--run-id " + runID
	if recordPath != "" {
		hint = "--run " + recordPath
	}

	if keepOnAbort {
		if _, err := fmt.Fprintf(out, "apply interrupted; resources left in place — reclaim with: keystone cleanup %s\n", hint); err != nil {
			slog.Warn("writing interrupt hint to output failed", "run", runID, "error", err)
		}
		return fmt.Errorf("applying plan (run %s): %w", runID, applyErr)
	}

	deleted, cleanupErr := keystoneexec.Cleanup(context.WithoutCancel(ctx), keystoneTimeoutCleaner{c, opTimeout}, runID, created, opTimeout)
	if _, err := fmt.Fprintf(out, "deleted %d resource(s) for run %s\n", deleted, runID); err != nil {
		slog.Warn("writing teardown count to output failed", "run", runID, "error", err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("applying plan (run %s): %w; teardown incomplete after deleting %d resource(s): %w — reclaim the rest with: keystone cleanup %s", runID, applyErr, deleted, cleanupErr, hint)
	}
	return fmt.Errorf("applying plan (run %s): %w (interrupted; %d created resource(s) torn down)", runID, applyErr, deleted)
}

// keystoneTimeoutCleaner wraps a keystoneexec.Cleaner so every cloud operation
// Cleanup performs is bounded by opTimeout. Its callers — an interrupted apply's
// teardown, a chaos run's teardown, and a monitor iteration's teardown — all
// drive Cleanup on a context.WithoutCancel that strips any deadline, and the
// gophercloud client sets no HTTP timeout of its own. Without this a wedged
// Keystone call would hang the teardown — and, in the monitor loop, the whole
// loop — indefinitely. Cleanup additionally bounds each mutating op with the
// same opTimeout; the double bound is equivalent and harmless.
type keystoneTimeoutCleaner struct {
	inner     keystoneexec.Cleaner
	opTimeout time.Duration
}

func (t keystoneTimeoutCleaner) ListProjectsByTag(ctx context.Context, runID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListProjectsByTag(ctx, runID)
}

func (t keystoneTimeoutCleaner) ListUsersByPrefix(ctx context.Context, runID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListUsersByPrefix(ctx, runID)
}

func (t keystoneTimeoutCleaner) ListRolesByPrefix(ctx context.Context, runID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListRolesByPrefix(ctx, runID)
}

func (t keystoneTimeoutCleaner) ListDomainsByPrefix(ctx context.Context, runID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListDomainsByPrefix(ctx, runID)
}

func (t keystoneTimeoutCleaner) ListAssignmentsForUser(ctx context.Context, userID string) ([]resource.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.ListAssignmentsForUser(ctx, userID)
}

func (t keystoneTimeoutCleaner) DisableDomain(ctx context.Context, r resource.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.DisableDomain(ctx, r)
}

func (t keystoneTimeoutCleaner) Delete(ctx context.Context, r resource.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout)
	defer cancel()
	return t.inner.Delete(ctx, r)
}
