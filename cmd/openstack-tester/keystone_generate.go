package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	keystoneplan "github.com/B42Labs/openstack-tester/internal/keystone/plan"
	keystonescenario "github.com/B42Labs/openstack-tester/internal/keystone/scenario"
)

// newKeystoneGenerateCmd builds "keystone generate", which expands a Keystone
// scenario into a plan and writes it as JSON to a file or stdout. It never
// touches the API.
func newKeystoneGenerateCmd(opts *globalOptions) *cobra.Command {
	var (
		scenarioPath string
		outPath      string
		sets         []string
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Expand a Keystone scenario into a plan and dump it (never touches the API)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, p, err := buildKeystonePlanFromFlags(cmd, opts, scenarioPath, sets)
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
				"domains", len(p.Domains), "roles", len(p.Roles), "projects", len(p.Projects),
				"users", len(p.Users), "assignments", len(p.Assignments), "tokens", len(p.Tokens),
				"destination", dest)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scenarioPath, "scenario", "", "path to the scenario YAML file (required)")
	flags.StringVar(&outPath, "out", "", "write the plan to this file instead of stdout")
	flags.StringArrayVar(&sets, "set", nil, "override a scenario value, e.g. --set resources.users=20 (repeatable)")
	// MarkFlagRequired only fails for an unknown flag; "scenario" was just added.
	_ = cmd.MarkFlagRequired("scenario")

	return cmd
}

// buildKeystonePlanFromFlags loads the Keystone scenario file, applies the --set
// overrides and the global --seed override, and expands it into a plan. It
// returns the scenario too so the chaos command can read its chaos block; the
// generate, apply, and monitor commands ignore it. It makes no API calls.
func buildKeystonePlanFromFlags(cmd *cobra.Command, opts *globalOptions, scenarioPath string, sets []string) (keystonescenario.Scenario, *keystoneplan.Plan, error) {
	data, err := os.ReadFile(scenarioPath)
	if err != nil {
		return keystonescenario.Scenario{}, nil, fmt.Errorf("reading scenario: %w", err)
	}

	s, err := keystonescenario.Parse(data)
	if err != nil {
		return keystonescenario.Scenario{}, nil, err
	}

	for _, set := range sets {
		key, value, ok := strings.Cut(set, "=")
		if !ok {
			return keystonescenario.Scenario{}, nil, fmt.Errorf("invalid --set %q: want key=value", set)
		}
		if err := s.Set(key, value); err != nil {
			return keystonescenario.Scenario{}, nil, err
		}
	}

	// The global --seed flag, when explicitly set, overrides the scenario seed.
	if cmd.Flags().Changed("seed") {
		s.Seed = opts.seed
	}

	p, err := s.Generate()
	if err != nil {
		return keystonescenario.Scenario{}, nil, err
	}
	return s, p, nil
}
