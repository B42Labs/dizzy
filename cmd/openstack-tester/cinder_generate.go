package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	cinderplan "github.com/B42Labs/openstack-tester/internal/cinder/plan"
	cinderscenario "github.com/B42Labs/openstack-tester/internal/cinder/scenario"
)

// newCinderGenerateCmd builds "cinder generate", which expands a Cinder scenario
// into a plan and writes it as JSON to a file or stdout. It never touches the
// API.
func newCinderGenerateCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		outPath      string
		sets         []string
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Expand a Cinder scenario into a plan and dump it (never touches the API)",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := buildCinderPlanFromFlags(cmd, opts, scenarioPath, sets)
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
				"volumes", len(p.Volumes), "snapshots", len(p.Snapshots), "destination", dest)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringVar(&outPath, "out", "", "write the plan to this file instead of stdout")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.volumes=20 (repeatable)")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// buildCinderPlanFromFlags loads the Cinder scenario file, applies the --set
// overrides and the global --seed override, and expands it into a plan. It is
// shared by the cinder generate and apply commands and makes no API calls.
func buildCinderPlanFromFlags(cmd *cobra.Command, opts *globalOptions, scenarioPath string, sets []string) (*cinderplan.Plan, error) {
	data, err := os.ReadFile(scenarioPath)
	if err != nil {
		return nil, fmt.Errorf("reading scenario: %w", err)
	}

	s, err := cinderscenario.Parse(data)
	if err != nil {
		return nil, err
	}

	for _, set := range sets {
		key, value, ok := strings.Cut(set, "=")
		if !ok {
			return nil, fmt.Errorf("invalid --set %q: want key=value", set)
		}
		if err := s.Set(key, value); err != nil {
			return nil, err
		}
	}

	// The global --seed flag, when explicitly set, overrides the scenario seed.
	if cmd.Flags().Changed("seed") {
		s.Seed = opts.seed
	}

	p, err := s.Generate()
	if err != nil {
		return nil, err
	}
	return p, nil
}
