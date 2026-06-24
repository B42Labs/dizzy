package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// globalOptions holds the values of the persistent flags shared by every
// subcommand.
type globalOptions struct {
	osCloud     string
	concurrency int
	timeout     time.Duration
	seed        int64
	logLevel    string
}

// newRootCmd builds the openstack-tester root command, registers the global
// flags, configures structured logging, and attaches the subcommand tree.
func newRootCmd() *cobra.Command {
	opts := &globalOptions{}

	cmd := &cobra.Command{
		Use:   "openstack-tester",
		Short: "Scenario-driven load and consistency tester for OpenStack",
		Long: "openstack-tester builds large, randomized but reproducible Neutron\n" +
			"topologies, records how long every operation takes, and tracks the\n" +
			"states the resources reach.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			level, err := parseLogLevel(opts.logLevel)
			if err != nil {
				return err
			}
			handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
			slog.SetDefault(slog.New(handler))
			return nil
		},
	}

	flags := cmd.PersistentFlags()
	flags.StringVar(&opts.osCloud, "os-cloud", "", "cloud name in clouds.yaml (defaults to $OS_CLOUD)")
	flags.IntVar(&opts.concurrency, "concurrency", 8, "maximum number of parallel API calls")
	flags.DurationVar(&opts.timeout, "timeout", 60*time.Second, "per-operation timeout")
	flags.Int64Var(&opts.seed, "seed", 0, "override the scenario RNG seed")
	flags.StringVar(&opts.logLevel, "log-level", "info", "log level: debug, info, warn, or error")

	cmd.AddCommand(newNeutronCmd(opts))

	return cmd
}

// parseLogLevel maps a log-level name to its slog.Level, returning an error for
// any unrecognized name.
func parseLogLevel(name string) (slog.Level, error) {
	switch name {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q: want debug, info, warn, or error", name)
	}
}
