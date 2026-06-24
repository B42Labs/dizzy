package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/B42Labs/openstack-tester/internal/run"
)

// newReportCmd builds "neutron report", which loads a run record and renders its
// metrics as a human-readable table, machine-readable JSON, or CSV. It never
// touches the cloud.
func newReportCmd(opts *globalOptions) *cobra.Command {
	var (
		runPath string
		format  string
	)

	cmd := &cobra.Command{
		Use:   "report",
		Short: "Render metrics from a run record",
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := run.Load(runPath)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			switch format {
			case "table":
				return run.WriteTable(out, rec)
			case "json":
				return run.WriteJSON(out, rec)
			case "csv":
				return run.WriteCSV(out, rec)
			default:
				return fmt.Errorf("unknown --format %q: want table, json, or csv", format)
			}
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) to report on (required)")
	flags.StringVar(&format, "format", "table", "output format: table, json, or csv")
	// MarkFlagRequired only fails for an unknown flag; "run" was just added.
	_ = cmd.MarkFlagRequired("run")

	return cmd
}
