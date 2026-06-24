package main

import (
	"io"
	"log/slog"
	"testing"

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

	want := []string{"generate", "apply", "chaos", "status", "report", "cleanup", "verify"}
	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			if findSubcommand(neutron, name) == nil {
				t.Errorf("neutron subcommand %q not registered", name)
			}
		})
	}
}

func TestGlobalFlagsRegistered(t *testing.T) {
	flags := newRootCmd().PersistentFlags()
	for _, name := range []string{"os-cloud", "concurrency", "timeout", "seed", "log-level"} {
		t.Run(name, func(t *testing.T) {
			if flags.Lookup(name) == nil {
				t.Errorf("persistent flag %q not registered", name)
			}
		})
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
