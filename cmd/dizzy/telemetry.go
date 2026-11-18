package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/B42Labs/dizzy/internal/telemetry"
)

// setupTelemetry is the only caller of telemetry.Setup. It fills in Enabled
// from --otel and, with --listen-address, the listener's registry and
// progress hook, starting the listener on the first call. A listener that
// cannot bind is returned as the error, so the command fails before it
// touches the cloud.
func setupTelemetry(ctx context.Context, opts *globalOptions, cfg telemetry.Config) (*telemetry.Telemetry, error) {
	cfg.Enabled = opts.otel
	srv, err := opts.serving()
	if err != nil {
		return nil, err
	}
	if srv != nil {
		cfg.Registerer = srv.Registerer()
		cfg.OnProgress = srv.Health().Mark
	}
	return telemetry.Setup(ctx, cfg)
}

// flushTelemetry shuts down the OTEL export seam on command exit, flushing any
// buffered metrics. It runs on a fresh short-lived context because the run's
// signal context may already be cancelled (Ctrl-C) yet the final export must
// still go out. A shutdown error is logged, never fatal: a run must not fail
// because the collector was unreachable. A nil t (export disabled) is a no-op.
func flushTelemetry(t *telemetry.Telemetry) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := t.Shutdown(ctx); err != nil {
		slog.Warn("flushing telemetry failed", "error", err)
	}
}
