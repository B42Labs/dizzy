package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	barbicanscenario "github.com/B42Labs/dizzy/internal/barbican/scenario"
)

// newBarbicanGenerateCmd builds "barbican generate", which expands a Barbican
// scenario into a plan and writes it as JSON to a file or stdout. It never
// touches the API.
func newBarbicanGenerateCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		outPath      string
		sets         []string
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Expand a Barbican scenario into a plan and dump it (never touches the API)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildBarbicanPlanFromFlags(cmd, opts, scenarioPath, sets)
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
				"secrets", len(p.Secrets), "orders", len(p.Orders), "containers", len(p.Containers),
				"payloadBytes", p.TotalPayloadBytes(), "destination", dest)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringVar(&outPath, "out", "", "write the plan to this file instead of stdout")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.secrets=20 (repeatable)")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// buildBarbicanPlanFromFlags loads the Barbican scenario file, applies the --set
// overrides and the global --seed override, and expands it into a plan. It
// returns the scenario too, in the shape the other services' builders have; the
// generate and apply commands ignore it. It makes no API calls.
func buildBarbicanPlanFromFlags(cmd *cobra.Command, opts *globalOptions, scenarioPath string, sets []string) (barbicanscenario.Scenario, *barbicanplan.Plan, error) {
	data, err := os.ReadFile(scenarioPath)
	if err != nil {
		return barbicanscenario.Scenario{}, nil, fmt.Errorf("reading scenario: %w", err)
	}

	s, err := barbicanscenario.Parse(data)
	if err != nil {
		return barbicanscenario.Scenario{}, nil, err
	}

	for _, set := range sets {
		key, value, ok := strings.Cut(set, "=")
		if !ok {
			return barbicanscenario.Scenario{}, nil, fmt.Errorf("invalid --set %q: want key=value", set)
		}
		if err := s.Set(key, value); err != nil {
			return barbicanscenario.Scenario{}, nil, err
		}
	}

	// The global --seed flag, when explicitly set, overrides the scenario seed.
	if cmd.Flags().Changed("seed") {
		s.Seed = opts.seed
	}

	p, err := s.Generate()
	if err != nil {
		return barbicanscenario.Scenario{}, nil, err
	}
	return s, p, nil
}
