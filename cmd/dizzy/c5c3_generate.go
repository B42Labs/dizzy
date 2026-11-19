package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	c5c3plan "github.com/B42Labs/dizzy/internal/c5c3/plan"
	c5c3scenario "github.com/B42Labs/dizzy/internal/c5c3/scenario"
)

// newC5C3GenerateCmd builds "c5c3 generate", which expands a c5c3 scenario
// into a plan and writes it as JSON to a file or stdout. It never touches the
// Kubernetes API or Keystone. A c5c3 plan has no seed, so the global --seed has
// no effect and the same scenario always yields byte-identical output.
func newC5C3GenerateCmd(_ *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		outPath      string
		sets         []string
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Expand a c5c3 scenario into a plan and dump it (never touches the API)",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := buildC5C3PlanFromFlags(scenarioPath, sets)
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

			slog.Info("generated plan", "scenario", p.Scenario, "namespace", p.Namespace,
				"orders", len(p.Orders), "refusals", len(p.Refusals), "destination", dest)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringVar(&outPath, "out", "", "write the plan to this file instead of stdout")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set role=reader (repeatable)")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// buildC5C3PlanFromFlags loads the c5c3 scenario file, applies the --set
// overrides, and expands it into a plan. Unlike the other services' builders
// it reads no global flag and returns no scenario: a c5c3 plan has no seed,
// so --seed has nothing to override, and the namespace has no chaos command
// to read a churn block. It makes no API calls.
func buildC5C3PlanFromFlags(scenarioPath string, sets []string) (*c5c3plan.Plan, error) {
	data, err := os.ReadFile(scenarioPath)
	if err != nil {
		return nil, fmt.Errorf("reading scenario: %w", err)
	}

	s, err := c5c3scenario.Parse(data)
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

	return s.Generate()
}
