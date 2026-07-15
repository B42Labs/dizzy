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
	otel        bool
}

// cloudName returns the cloud identity used as the OTEL "cloud" resource
// attribute: the --os-cloud value, falling back to $OS_CLOUD (the same source
// the network client resolves from when the flag is empty).
func (o *globalOptions) cloudName() string {
	if o.osCloud != "" {
		return o.osCloud
	}
	return os.Getenv("OS_CLOUD")
}

// newRootCmd builds the dizzy root command, registers the global
// flags, configures structured logging, and attaches the subcommand tree.
func newRootCmd() *cobra.Command {
	opts := &globalOptions{}

	cmd := &cobra.Command{
		Use:   "dizzy",
		Short: "Scenario-driven load and consistency tester for OpenStack",
		Long: "dizzy builds large, randomized but reproducible Neutron\n" +
			"topologies, Cinder volume workloads, Keystone identity workloads,\n" +
			"Nova server fleets, and Glance image lifecycles, records how long\n" +
			"every operation takes, and tracks the states the resources reach.",
		// Version is set from the main package's build-time variable, so
		// "dizzy --version" prints the release tag (or "dev" for local builds).
		Version:       version,
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

	// Print "dizzy <version>" for --version rather than cobra's default
	// "dizzy version <version>".
	cmd.SetVersionTemplate("dizzy {{.Version}}\n")

	flags := cmd.PersistentFlags()
	flags.StringVar(&opts.osCloud, "os-cloud", "", "cloud name in clouds.yaml (defaults to $OS_CLOUD)")
	flags.IntVar(&opts.concurrency, "concurrency", 8, "maximum number of parallel API calls")
	flags.DurationVar(&opts.timeout, "timeout", 60*time.Second, "per-operation timeout")
	flags.Int64Var(&opts.seed, "seed", 0, "override the scenario RNG seed")
	flags.StringVar(&opts.logLevel, "log-level", "info", "log level: debug, info, warn, or error")
	flags.BoolVar(&opts.otel, "otel", false, "export metrics via OpenTelemetry OTLP; endpoint, protocol, headers, and TLS come from the OTEL_EXPORTER_OTLP_* environment variables")

	cmd.AddCommand(newNeutronCmd(opts), newCinderCmd(opts), newKeystoneCmd(opts), newNovaCmd(opts), newGlanceCmd(opts))

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
