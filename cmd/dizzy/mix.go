package main

import (
	"github.com/spf13/cobra"
)

// newMixCmd builds the "mix" command namespace, the combined chaos mode: it
// runs several workload personas side by side, each in a churn engine and a
// project of its own, under one seed and one run identifier. There is no apply
// and no monitor, because a persona is a behavior over time. report is the
// same service-agnostic builder the other namespaces use.
func newMixCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mix",
		Short: "Combined multi-persona churn across services",
	}

	cmd.AddCommand(
		newMixGenerateCmd(opts),
		newReportCmd(opts),
	)

	return cmd
}
