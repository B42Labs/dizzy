package main

import (
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/mix"
	"github.com/B42Labs/dizzy/internal/resource"
)

// newMixCleanupCmd builds "mix cleanup", which deletes every persona's
// resources of a mix run, each in the project of the cloud the persona ran
// under and strictly by the persona's identity. It is idempotent. With --run
// the personas, their clouds and identities come from the run record, whose
// created list also serves as a handle. With --run-id they come from the
// scenario the run used, which --scenario (and --set) must name again.
func newMixCleanupCmd(opts *globalOptions) *cobra.Command {
	var (
		runPath      string
		runID        string
		scenarioPath string
		sets         []string
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete every persona's resources of a mix run, by identity",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runPath != "" && runID == "" && scenarioPath != "" {
				return errors.New("--scenario is only used with --run-id")
			}
			if runID != "" && runPath == "" && scenarioPath == "" {
				return errors.New("--run-id needs --scenario to name the personas and their clouds")
			}
			id, rec, err := resolveRun(runPath, runID)
			if err != nil {
				return err
			}
			if err := requireService(rec, "mix"); err != nil {
				return err
			}

			overall := metrics.NewCollector()
			var (
				inputs  []mixLaneInput
				created []resource.Resource
			)
			if rec != nil {
				if err := mix.CheckServices(rec.Services); err != nil {
					return err
				}
				inputs, created = recordLaneInputs(rec, overall), rec.Created
			} else {
				s, p, err := buildMixPlanFromFlags(cmd, opts, scenarioPath, sets)
				if err != nil {
					return err
				}
				if inputs, err = planLaneInputs(s, p, id, overall); err != nil {
					return err
				}
			}

			// Stop cleanly on Ctrl-C / SIGTERM, like apply.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			lanes, err := buildCleanupLanes(ctx, opts, inputs)
			if err != nil {
				return err
			}

			hb := startHeartbeat(ctx, "cleanup in progress", collectorSnapshot(overall, time.Now()))
			defer hb.stop()
			return deleteLaneResources(ctx, cmd.OutOrStdout(), lanes, created, "cleaning up")
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&runPath, "run", "", "path to the mix run record (run-<id>.json) whose resources to delete")
	flags.StringVar(&runID, "run-id", "", "delete resources for this run id directly, without a run record (needs --scenario)")
	flags.StringVar(&scenarioPath, "scenario", "", "with --run-id, the scenario the run used, which names the personas and their clouds")
	flags.StringArrayVar(&sets, "set", nil, "with --run-id, an override the run used, e.g. --set personas.ci.cloud=tenant-ci (repeatable)")

	return cmd
}
