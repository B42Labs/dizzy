package main

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/serve"
)

// globalOptions holds the values of the persistent flags shared by every
// subcommand, and the one metrics listener a process opens for them. It holds
// a sync.Once and must not be copied; every command takes it by pointer.
type globalOptions struct {
	osCloud            string
	concurrency        int
	timeout            time.Duration
	seed               int64
	logLevel           string
	otel               bool
	listenAddress      string
	healthStallTimeout time.Duration

	serveOnce sync.Once
	server    *serve.Server
	serveErr  error
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

// validateServing rejects listener settings that cannot work, before any
// command runs. A negative stall timeout is always an error. With a listener
// and stall detection on, the stall timeout must exceed --timeout: an
// operation is recorded when it returns, which can take the whole --timeout,
// so a shorter limit would fail the health check on a merely slow call.
func (o *globalOptions) validateServing() error {
	if o.healthStallTimeout < 0 {
		return fmt.Errorf("--health-stall-timeout must not be negative, got %s", o.healthStallTimeout)
	}
	if o.listenAddress != "" && o.healthStallTimeout != 0 && o.healthStallTimeout <= o.timeout {
		return fmt.Errorf("--health-stall-timeout (%s) must be greater than --timeout (%s), or 0 to disable the check",
			o.healthStallTimeout, o.timeout)
	}
	return nil
}

// serving returns the process's metrics listener, starting it on the first
// call. It returns (nil, nil) without --listen-address. Every later call
// returns what the first one did, so the personas and lanes of a mix run
// share one listener, and a bind failure is reported to each caller.
func (o *globalOptions) serving() (*serve.Server, error) {
	o.serveOnce.Do(func() {
		if o.listenAddress == "" {
			return
		}
		o.server, o.serveErr = serve.Listen(o.listenAddress, o.healthStallTimeout)
		if o.serveErr == nil {
			slog.Info("serving metrics and health", "address", o.server.Addr(), "stallTimeout", o.healthStallTimeout)
		}
	})
	return o.server, o.serveErr
}

// health returns the listener's health tracker, or nil while no listener
// runs; a nil tracker is a no-op.
func (o *globalOptions) health() *serve.Health {
	if o.server == nil {
		return nil
	}
	return o.server.Health()
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
			"Nova server fleets, Glance image lifecycles, Barbican secret\n" +
			"lifecycles, and Octavia load balancer lifecycles, records how long\n" +
			"every operation takes, and tracks the states the resources reach.\n\n" +
			"dizzy mix runs several workload personas side by side in one churn run.",
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
			return opts.validateServing()
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
	flags.StringVar(&opts.listenAddress, "listen-address", "", "serve Prometheus metrics on /metrics and a health check on /healthz at this address, e.g. :9464 (default: no listener)")
	flags.DurationVar(&opts.healthStallTimeout, "health-stall-timeout", 15*time.Minute, "with --listen-address: /healthz turns unhealthy when no operation or iteration finished for this long; 0 disables the check")

	cmd.AddCommand(newNeutronCmd(opts), newCinderCmd(opts), newKeystoneCmd(opts), newNovaCmd(opts), newGlanceCmd(opts), newBarbicanCmd(opts), newOctaviaCmd(opts), newMixCmd(opts))

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
