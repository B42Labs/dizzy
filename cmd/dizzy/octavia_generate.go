package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	octaviaplan "github.com/B42Labs/dizzy/internal/octavia/plan"
	octaviascenario "github.com/B42Labs/dizzy/internal/octavia/scenario"
)

// newOctaviaGenerateCmd builds "octavia generate", which expands an Octavia
// scenario into a plan and writes it as JSON to a file or stdout. It never
// touches the API.
func newOctaviaGenerateCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		outPath      string
		sets         []string
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Expand an Octavia scenario into a plan and dump it (never touches the API)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildOctaviaPlanFromFlags(cmd, opts, scenarioPath, sets)
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

			slog.Info("generated plan", "scenario", p.Scenario, "seed", p.Seed, "provider", p.Provider,
				"networks", len(p.Networks), "loadbalancers", len(p.LoadBalancers),
				"listeners", len(p.Listeners), "pools", len(p.Pools), "members", len(p.Members),
				"healthmonitors", len(p.HealthMonitors), "destination", dest)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringVar(&outPath, "out", "", "write the plan to this file instead of stdout")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set provider=ovn (repeatable)")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// buildOctaviaPlanFromFlags loads the Octavia scenario file, applies the --set
// overrides and the global --seed override, and expands it into a plan. It
// returns the scenario too, in the shape the other services' builders have: the
// chaos command merges its chaos block into the churn config, while the
// generate and apply commands ignore it. It makes no API calls.
func buildOctaviaPlanFromFlags(cmd *cobra.Command, opts *globalOptions, scenarioPath string, sets []string) (octaviascenario.Scenario, *octaviaplan.Plan, error) {
	data, err := os.ReadFile(scenarioPath)
	if err != nil {
		return octaviascenario.Scenario{}, nil, fmt.Errorf("reading scenario: %w", err)
	}

	s, err := octaviascenario.Parse(data)
	if err != nil {
		return octaviascenario.Scenario{}, nil, err
	}

	for _, set := range sets {
		key, value, ok := strings.Cut(set, "=")
		if !ok {
			return octaviascenario.Scenario{}, nil, fmt.Errorf("invalid --set %q: want key=value", set)
		}
		if err := s.Set(key, value); err != nil {
			return octaviascenario.Scenario{}, nil, err
		}
	}

	// The global --seed flag, when explicitly set, overrides the scenario seed.
	if cmd.Flags().Changed("seed") {
		s.Seed = opts.seed
	}

	p, err := s.Generate()
	if err != nil {
		return octaviascenario.Scenario{}, nil, err
	}
	return s, p, nil
}
