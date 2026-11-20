package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	horizonplan "github.com/B42Labs/dizzy/internal/horizon/plan"
	horizonscenario "github.com/B42Labs/dizzy/internal/horizon/scenario"
)

// newHorizonGenerateCmd builds "horizon generate", which expands a Horizon
// scenario into a plan and writes it as JSON to a file or stdout. It reads
// no clouds.yaml and sends no request.
func newHorizonGenerateCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		outPath      string
		sets         []string
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Expand a Horizon scenario into a plan and dump it (never touches the dashboard or Keystone)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildHorizonPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			data, err := json.MarshalIndent(p, "", "  ")
			if err != nil {
				return fmt.Errorf("encoding plan: %w", err)
			}
			data = append(data, '\n')

			dest, err := writePlanOutput(cmd, outPath, data)
			if err != nil {
				return err
			}

			slog.Info("generated plan", "scenario", p.Scenario, "seed", p.Seed,
				"sessions", len(p.Sessions), "users", len(p.Users), "projects", len(p.Projects),
				"pages", len(p.Pages), "switches", p.Switches(), "destination", dest)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringVar(&outPath, "out", "", "write the plan to this file instead of stdout")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.users=5 (repeatable)")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// buildHorizonPlanFromFlags loads the Horizon scenario file, applies the
// --set overrides and the global --seed override, and expands it into a
// plan. It returns the scenario too, whose chaos block the chaos command
// merges into the churn config. It makes no request.
func buildHorizonPlanFromFlags(cmd *cobra.Command, opts *globalOptions, scenarioPath string, sets []string) (horizonscenario.Scenario, *horizonplan.Plan, error) {
	data, err := os.ReadFile(scenarioPath)
	if err != nil {
		return horizonscenario.Scenario{}, nil, fmt.Errorf("reading scenario: %w", err)
	}

	s, err := horizonscenario.Parse(data)
	if err != nil {
		return horizonscenario.Scenario{}, nil, err
	}

	for _, set := range sets {
		key, value, ok := strings.Cut(set, "=")
		if !ok {
			return horizonscenario.Scenario{}, nil, fmt.Errorf("invalid --set %q: want key=value", set)
		}
		if err := s.Set(key, value); err != nil {
			return horizonscenario.Scenario{}, nil, err
		}
	}

	// The global --seed flag, when explicitly set, overrides the scenario seed.
	if cmd.Flags().Changed("seed") {
		s.Seed = opts.seed
	}

	p, err := s.Generate()
	if err != nil {
		return horizonscenario.Scenario{}, nil, err
	}
	return s, p, nil
}
