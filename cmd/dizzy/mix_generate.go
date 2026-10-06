package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/mix"
	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	mixscenario "github.com/B42Labs/dizzy/internal/mix/scenario"
)

// newMixGenerateCmd builds "mix generate", which expands a mix scenario into a
// plan, one compute plan per persona and one service plan per enabled lane,
// and writes it as JSON to a file or stdout. It never touches the API.
func newMixGenerateCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		outPath      string
		sets         []string
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Expand a mix scenario into a plan and dump it (never touches the API)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, p, err := buildMixPlanFromFlags(cmd, opts, scenarioPath, sets)
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

			var servers int
			for _, ps := range p.Personas {
				servers += ps.Servers
			}
			slog.Info("generated plan", "scenario", p.Scenario, "seed", p.Seed,
				"personas", len(p.Personas), "servers", servers, "lanes", len(p.Lanes), "destination", dest)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringVar(&outPath, "out", "", "write the plan to this file instead of stdout")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.servers=20 (repeatable)")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// buildMixPlanFromFlags loads the mix scenario file, applies the --set
// overrides and the global --seed override, reads the scenario of every
// enabled lane, from the service's bundled profile or from the file the lane
// names, expands it all into a plan, and checks that this build supports every
// opt-in service the plan names. It returns the scenario and the lane
// scenarios too so the chaos command can read their persona, lane and chaos
// blocks. It makes no API calls.
func buildMixPlanFromFlags(cmd *cobra.Command, opts *globalOptions, scenarioPath string, sets []string) (mixscenario.Scenario, mixscenario.LaneScenarios, *mixplan.Plan, error) {
	fail := func(err error) (mixscenario.Scenario, mixscenario.LaneScenarios, *mixplan.Plan, error) {
		return mixscenario.Scenario{}, mixscenario.LaneScenarios{}, nil, err
	}

	s, err := loadMixScenarioFromFlags(cmd, opts, scenarioPath, sets)
	if err != nil {
		return fail(err)
	}
	ls, err := s.LoadLanes(os.ReadFile)
	if err != nil {
		return fail(err)
	}
	p, err := s.Generate(ls)
	if err != nil {
		return fail(err)
	}
	if err := mix.CheckServices(p.Services); err != nil {
		return fail(err)
	}
	return s, ls, p, nil
}

// buildMixPersonaPlanFromFlags is buildMixPlanFromFlags without the lanes: it
// validates the whole scenario, the lanes block included, but reads no lane
// scenario and expands the personas alone, so the plan has no lanes. mix
// cleanup --run-id uses it: it needs only each lane's name and cloud, which
// the scenario names, and must not fail on a lane scenario file that moved,
// changed or cannot be read since the run.
func buildMixPersonaPlanFromFlags(cmd *cobra.Command, opts *globalOptions, scenarioPath string, sets []string) (mixscenario.Scenario, *mixplan.Plan, error) {
	s, err := loadMixScenarioFromFlags(cmd, opts, scenarioPath, sets)
	if err != nil {
		return mixscenario.Scenario{}, nil, err
	}
	if err := s.Validate(); err != nil {
		return mixscenario.Scenario{}, nil, fmt.Errorf("invalid scenario: %w", err)
	}
	personas := s
	personas.Lanes = mixscenario.Lanes{}
	p, err := personas.Generate(mixscenario.LaneScenarios{})
	if err != nil {
		return mixscenario.Scenario{}, nil, err
	}
	if err := mix.CheckServices(p.Services); err != nil {
		return mixscenario.Scenario{}, nil, err
	}
	return s, p, nil
}

// loadMixScenarioFromFlags reads and parses the mix scenario file and applies
// the --set overrides and the global --seed override. It reads no lane
// scenario.
func loadMixScenarioFromFlags(cmd *cobra.Command, opts *globalOptions, scenarioPath string, sets []string) (mixscenario.Scenario, error) {
	data, err := os.ReadFile(scenarioPath)
	if err != nil {
		return mixscenario.Scenario{}, fmt.Errorf("reading scenario: %w", err)
	}

	s, err := mixscenario.Parse(data)
	if err != nil {
		return mixscenario.Scenario{}, err
	}

	for _, set := range sets {
		key, value, ok := strings.Cut(set, "=")
		if !ok {
			return mixscenario.Scenario{}, fmt.Errorf("invalid --set %q: want key=value", set)
		}
		if err := s.Set(key, value); err != nil {
			return mixscenario.Scenario{}, err
		}
	}

	// The global --seed flag, when explicitly set, overrides the scenario seed.
	if cmd.Flags().Changed("seed") {
		s.Seed = opts.seed
	}
	return s, nil
}
