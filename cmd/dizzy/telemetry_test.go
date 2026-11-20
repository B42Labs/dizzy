package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/telemetry"
)

// holdAddress binds a free loopback port for the length of the test and
// returns its address, so a listener started on it fails to bind.
func holdAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("holding a port: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func TestSetupTelemetryDisabled(t *testing.T) {
	opts := &globalOptions{}
	tel, err := setupTelemetry(context.Background(), opts, telemetry.Config{Service: "neutron"})
	if err != nil || tel != nil {
		t.Errorf("setupTelemetry() = (%v, %v), want (nil, nil)", tel, err)
	}
	if opts.server != nil {
		t.Error("a listener was started without --listen-address")
	}
	if opts.health() != nil {
		t.Error("health() is non-nil without a listener")
	}
}

func TestSetupTelemetryServesOnce(t *testing.T) {
	opts := &globalOptions{listenAddress: "127.0.0.1:0", healthStallTimeout: 15 * time.Minute}
	ctx := context.Background()

	persona, err := setupTelemetry(ctx, opts, telemetry.Config{Service: "mix", Persona: "a"})
	if err != nil {
		t.Fatalf("first setupTelemetry: %v", err)
	}
	t.Cleanup(func() { _ = opts.server.Close() })
	t.Cleanup(func() { flushTelemetry(persona) })
	first := opts.server

	lane, err := setupTelemetry(ctx, opts, telemetry.Config{Service: "mix", Lane: "b"})
	if err != nil {
		t.Fatalf("second setupTelemetry: %v", err)
	}
	t.Cleanup(func() { flushTelemetry(lane) })
	if opts.server == nil || opts.server != first {
		t.Fatal("the second setupTelemetry did not reuse the first listener")
	}

	persona.RecordOperation(ctx, "server", "create", time.Second, "")
	lane.RecordOperation(ctx, "volume", "create", time.Second, "")

	healthy, idle := opts.health().Check()
	if !healthy || idle >= time.Second {
		t.Errorf("health right after an operation = (%v, %s), want healthy with idle under 1s", healthy, idle)
	}

	resp, err := http.Get("http://" + opts.server.Addr() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading /metrics: %v", err)
	}
	var personaLines, laneLines int
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "dizzy_operation_duration_seconds_bucket{") {
			continue
		}
		if strings.Contains(line, `persona="a"`) {
			personaLines++
		}
		if strings.Contains(line, `lane="b"`) {
			laneLines++
		}
	}
	if personaLines == 0 || laneLines == 0 {
		t.Errorf("/metrics bucket lines: %d with persona=\"a\", %d with lane=\"b\", want both; body:\n%s",
			personaLines, laneLines, body)
	}
}

// TestSetupTelemetryMarksProgress shows that setupTelemetry wires the
// listener's health tracker into the telemetry: a tracker that has stalled
// turns healthy again on a recorded operation, which reaches it only through
// the OnProgress hook.
func TestSetupTelemetryMarksProgress(t *testing.T) {
	opts := &globalOptions{listenAddress: "127.0.0.1:0", healthStallTimeout: 200 * time.Millisecond}
	tel, err := setupTelemetry(context.Background(), opts, telemetry.Config{Service: "neutron"})
	if err != nil {
		t.Fatalf("setupTelemetry: %v", err)
	}
	t.Cleanup(func() { _ = opts.server.Close() })
	t.Cleanup(func() { flushTelemetry(tel) })

	// Wait for the stall rather than sleeping a fixed time.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if healthy, _ := opts.health().Check(); !healthy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("health did not stall within 5s with a 200ms limit")
		}
		time.Sleep(10 * time.Millisecond)
	}

	tel.RecordOperation(context.Background(), "network", "create", time.Second, "timeout")
	if healthy, idle := opts.health().Check(); !healthy {
		t.Errorf("health after a recorded operation = (false, %s), want healthy", idle)
	}
}

func TestSetupTelemetryListenError(t *testing.T) {
	opts := &globalOptions{listenAddress: holdAddress(t), healthStallTimeout: 15 * time.Minute}
	for _, call := range []string{"first", "second"} {
		tel, err := setupTelemetry(context.Background(), opts, telemetry.Config{Service: "neutron"})
		if err == nil || !strings.Contains(err.Error(), "listening on") {
			t.Errorf("%s call: error = %v, want one containing %q", call, err, "listening on")
		}
		if tel != nil {
			t.Errorf("%s call: telemetry = %v, want nil", call, tel)
		}
	}
}

// TestMonitorFailsOnBusyListenAddress confirms that the listener starts
// before the monitor authenticates: with no usable clouds.yaml, the error is
// still the bind failure.
func TestMonitorFailsOnBusyListenAddress(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")
	path := writeScenario(t, sampleScenarioYAML)

	_, err := execRoot(t, "neutron", "monitor", "--scenario", path, "--listen-address", holdAddress(t))
	if err == nil {
		t.Fatal("monitor on a held --listen-address: expected an error, got nil")
	}
	if want := "setting up telemetry: listening on"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
}

// TestTelemetrySetupHasOneCaller keeps every command on setupTelemetry, so
// --otel and --listen-address reach all of them: telemetry.Setup is called
// only from telemetry.go, and the 27 commands that record metrics call the
// helper.
func TestTelemetrySetupHasOneCaller(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing package files: %v", err)
	}
	var helperCalls int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "telemetry.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		if strings.Contains(string(src), "telemetry.Setup(") {
			t.Errorf("%s calls telemetry.Setup directly; use setupTelemetry", f)
		}
		helperCalls += strings.Count(string(src), "setupTelemetry(")
	}
	if helperCalls != 27 {
		t.Errorf("setupTelemetry calls outside telemetry.go = %d, want 27", helperCalls)
	}
}
