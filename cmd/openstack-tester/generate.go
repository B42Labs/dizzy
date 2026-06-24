package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/B42Labs/openstack-tester/internal/plan"
	"github.com/B42Labs/openstack-tester/internal/scenario"
)

// newGenerateCmd builds "neutron generate", which expands a scenario into a plan
// and writes it as JSON to a file or stdout. It never touches the API.
func newGenerateCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		outPath      string
		sets         []string
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Expand a scenario into a plan and dump it (never touches the API)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildPlanFromFlags(cmd, opts, scenarioPath, sets)
			if err != nil {
				return err
			}

			data, err := json.MarshalIndent(p, "", "  ")
			if err != nil {
				return fmt.Errorf("encoding plan: %w", err)
			}
			data = append(data, '\n')

			dest := "stdout"
			if outPath == "" {
				if _, err := cmd.OutOrStdout().Write(data); err != nil {
					return fmt.Errorf("writing plan: %w", err)
				}
			} else {
				// Write to a temp file and rename so a kill mid-write (SIGINT,
				// OOM, disk-full) never leaves a truncated plan at outPath.
				tmp := outPath + ".tmp"
				if err := os.WriteFile(tmp, data, 0o644); err != nil {
					return fmt.Errorf("writing plan to %s: %w", tmp, err)
				}
				if err := os.Rename(tmp, outPath); err != nil {
					return fmt.Errorf("finalizing plan %s: %w", outPath, err)
				}
				dest = outPath
			}

			slog.Info("generated plan", "scenario", p.Scenario, "seed", p.Seed,
				"networks", len(p.Networks), "subnets", len(p.Subnets), "ports", len(p.Ports),
				"destination", dest)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringVar(&outPath, "out", "", "write the plan to this file instead of stdout")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.networks=200 (repeatable)")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// buildPlanFromFlags loads the scenario file, applies the --set overrides and
// the global --seed override, and expands it into a plan. It returns the parsed
// scenario alongside the plan so the chaos command can read the scenario's chaos
// block; generate and apply ignore the scenario return. It is shared by the
// generate, apply, and chaos commands and makes no API calls.
func buildPlanFromFlags(cmd *cobra.Command, opts *globalOptions, scenarioPath string, sets []string) (scenario.Scenario, *plan.Plan, error) {
	data, err := os.ReadFile(scenarioPath)
	if err != nil {
		return scenario.Scenario{}, nil, fmt.Errorf("reading scenario: %w", err)
	}

	s, err := scenario.Parse(data)
	if err != nil {
		return scenario.Scenario{}, nil, err
	}

	for _, set := range sets {
		key, value, ok := strings.Cut(set, "=")
		if !ok {
			return scenario.Scenario{}, nil, fmt.Errorf("invalid --set %q: want key=value", set)
		}
		if err := s.Set(key, value); err != nil {
			return scenario.Scenario{}, nil, err
		}
	}

	// The global --seed flag, when explicitly set, overrides the scenario seed.
	if cmd.Flags().Changed("seed") {
		s.Seed = opts.seed
	}

	p, err := s.Generate()
	if err != nil {
		return scenario.Scenario{}, nil, err
	}
	return s, p, nil
}
