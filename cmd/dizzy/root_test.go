package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// findSubcommand returns the immediate child of parent with the given name, or
// nil if no such child exists.
func findSubcommand(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

func TestNeutronSubcommandsRegistered(t *testing.T) {
	root := newRootCmd()
	neutron := findSubcommand(root, "neutron")
	if neutron == nil {
		t.Fatal("neutron command not registered on root")
	}

	want := []string{"generate", "apply", "chaos", "monitor", "status", "report", "cleanup", "verify"}
	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			if findSubcommand(neutron, name) == nil {
				t.Errorf("neutron subcommand %q not registered", name)
			}
		})
	}
}

// TestMixSubcommandsRegistered confirms the mix namespace carries its
// subcommands and no apply or monitor, since a persona is a behavior over time.
func TestMixSubcommandsRegistered(t *testing.T) {
	mix := findSubcommand(newRootCmd(), "mix")
	if mix == nil {
		t.Fatal("mix command not registered on root")
	}
	for _, name := range []string{"generate", "chaos", "status", "report", "cleanup"} {
		if findSubcommand(mix, name) == nil {
			t.Errorf("mix subcommand %q not registered", name)
		}
	}
	for _, name := range []string{"apply", "monitor"} {
		if findSubcommand(mix, name) != nil {
			t.Errorf("mix subcommand %q registered, want none", name)
		}
	}
}

// TestMixHelpListsSubcommands confirms dizzy mix --help lists the five
// subcommands and neither apply nor monitor.
func TestMixHelpListsSubcommands(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"mix", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("mix --help: %v", err)
	}
	listed := map[string]bool{}
	for _, line := range strings.Split(out.String(), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && strings.HasPrefix(line, "  ") {
			listed[fields[0]] = true
		}
	}
	for _, name := range []string{"generate", "chaos", "status", "report", "cleanup"} {
		if !listed[name] {
			t.Errorf("mix --help does not list %q:\n%s", name, out.String())
		}
	}
	for _, name := range []string{"apply", "monitor"} {
		if listed[name] {
			t.Errorf("mix --help lists %q:\n%s", name, out.String())
		}
	}
}

func TestGlobalFlagsRegistered(t *testing.T) {
	flags := newRootCmd().PersistentFlags()
	for _, name := range []string{"os-cloud", "concurrency", "timeout", "seed", "log-level", "otel", "listen-address", "health-stall-timeout"} {
		t.Run(name, func(t *testing.T) {
			if flags.Lookup(name) == nil {
				t.Errorf("persistent flag %q not registered", name)
			}
		})
	}

	// The listener is off unless asked for, and the stall limit defaults to
	// 15 minutes.
	for name, want := range map[string]string{"listen-address": "", "health-stall-timeout": "15m0s"} {
		if f := flags.Lookup(name); f != nil && f.DefValue != want {
			t.Errorf("--%s default = %q, want %q", name, f.DefValue, want)
		}
	}
}

func TestValidateServing(t *testing.T) {
	tests := []struct {
		name    string
		opts    *globalOptions
		wantErr string
	}{
		{
			name:    "negative stall timeout without a listener",
			opts:    &globalOptions{timeout: time.Minute, healthStallTimeout: -time.Second},
			wantErr: "--health-stall-timeout must not be negative",
		},
		{
			name:    "negative stall timeout with a listener",
			opts:    &globalOptions{listenAddress: ":0", timeout: time.Minute, healthStallTimeout: -time.Second},
			wantErr: "--health-stall-timeout must not be negative",
		},
		{
			name:    "stall timeout equal to the operation timeout",
			opts:    &globalOptions{listenAddress: ":0", timeout: time.Minute, healthStallTimeout: time.Minute},
			wantErr: "must be greater than twice --timeout",
		},
		{
			// A wait of a whole --timeout and the call after it can leave
			// that long without progress.
			name:    "stall timeout above the operation timeout but not above twice it",
			opts:    &globalOptions{listenAddress: ":0", timeout: time.Minute, healthStallTimeout: 61 * time.Second},
			wantErr: "must be greater than twice --timeout",
		},
		{
			name:    "stall timeout equal to twice the operation timeout",
			opts:    &globalOptions{listenAddress: ":0", timeout: time.Minute, healthStallTimeout: 2 * time.Minute},
			wantErr: "must be greater than twice --timeout",
		},
		{
			name: "stall detection disabled",
			opts: &globalOptions{listenAddress: ":0", timeout: time.Minute, healthStallTimeout: 0},
		},
		{
			name: "stall timeout above twice the operation timeout",
			opts: &globalOptions{listenAddress: ":0", timeout: time.Minute, healthStallTimeout: 121 * time.Second},
		},
		{
			// Without a listener the stall timeout has no effect, so a short
			// one is not an error.
			name: "short stall timeout without a listener",
			opts: &globalOptions{timeout: time.Minute, healthStallTimeout: time.Second},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.validateServing()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("validateServing() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("validateServing() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestMonitorRejectsStallTimeoutNotAboveTimeout confirms the root command
// validates the listener flags before a subcommand runs: a 30s stall limit
// under the default 1m --timeout fails.
func TestMonitorRejectsStallTimeoutNotAboveTimeout(t *testing.T) {
	path := writeScenario(t, sampleScenarioYAML)
	_, err := execRoot(t, "neutron", "monitor", "--scenario", path,
		"--listen-address", "127.0.0.1:0", "--health-stall-timeout", "30s")
	if err == nil {
		t.Fatal("monitor with --health-stall-timeout below --timeout: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "--health-stall-timeout") {
		t.Errorf("error = %q, want it to mention --health-stall-timeout", err.Error())
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name    string
		want    slog.Level
		wantErr bool
	}{
		{name: "debug", want: slog.LevelDebug},
		{name: "info", want: slog.LevelInfo},
		{name: "warn", want: slog.LevelWarn},
		{name: "error", want: slog.LevelError},
		{name: "trace", wantErr: true},
		{name: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLogLevel(tc.name)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got level %v", tc.name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.name, err)
			}
			if got != tc.want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestNeutronHelpRuns(t *testing.T) {
	root := newRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"neutron", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("neutron --help: %v", err)
	}
}

func TestNeutronStubsReturnNotImplemented(t *testing.T) {
	for _, name := range []string{"verify"} {
		t.Run(name, func(t *testing.T) {
			root := newRootCmd()
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs([]string{"neutron", name})

			if err := root.Execute(); err == nil {
				t.Errorf("neutron %s: expected not-implemented error, got nil", name)
			}
		})
	}
}
