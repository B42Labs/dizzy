package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/run"
)

// newReportCmd builds "neutron report", which loads a run record and renders its
// metrics as a human-readable table, machine-readable JSON, CSV, or a
// self-contained visual HTML report. With --compare it loads a second
// keystone pressure record instead and renders the comparison of the two, the
// --run record as the base, in every format but CSV. It never touches the
// cloud.
func newReportCmd(opts *globalOptions) *cobra.Command {
	var (
		runPath       string
		format        string
		comparePath   string
		allowMismatch bool
	)

	cmd := &cobra.Command{
		Use:   "report",
		Short: "Render metrics from a run record",
		RunE: func(cmd *cobra.Command, args []string) error {
			if allowMismatch && comparePath == "" {
				return errors.New("--allow-mismatch needs --compare")
			}
			if comparePath != "" && format == "csv" {
				return errors.New("--compare is not available for --format csv; use table, json or html")
			}
			rec, err := run.Load(runPath)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if comparePath != "" {
				other, err := run.Load(comparePath)
				if err != nil {
					return err
				}
				c, err := run.Compare(rec, other, allowMismatch)
				if err != nil {
					return fmt.Errorf("comparing %s with %s: %w", runPath, comparePath, err)
				}
				return writeComparison(out, c, format)
			}
			switch format {
			case "table":
				return run.WriteTable(out, rec)
			case "json":
				return run.WriteJSON(out, rec)
			case "csv":
				return run.WriteCSV(out, rec)
			case "html":
				return run.WriteHTML(out, rec)
			default:
				return unknownFormat(format)
			}
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&runPath, "run", "", "path to the run record (run-<id>.json) to report on (required)")
	flags.StringVar(&format, "format", "table", "output format: table, json, csv, or html")
	flags.StringVar(&comparePath, "compare", "", "path to a second keystone pressure run record; the report then prints both records' percentiles and throughput per operation and per stage, and the second minus the first as the difference")
	flags.BoolVar(&allowMismatch, "allow-mismatch", false, "compare two records whose scenario, seed, workers, token pool, mix or stages differ")
	// MarkFlagRequired only fails for an unknown flag; "run" was just added.
	_ = cmd.MarkFlagRequired("run")

	return cmd
}

// writeComparison renders c in format, which --compare allows to be table,
// json or html.
func writeComparison(w io.Writer, c *run.Comparison, format string) error {
	switch format {
	case "table":
		return run.WriteComparisonTable(w, c)
	case "json":
		return run.WriteComparisonJSON(w, c)
	case "html":
		return run.WriteComparisonHTML(w, c)
	default:
		return unknownFormat(format)
	}
}

// unknownFormat is the error for a --format no renderer handles.
func unknownFormat(format string) error {
	return fmt.Errorf("unknown --format %q: want table, json, csv, or html", format)
}
