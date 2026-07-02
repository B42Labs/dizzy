package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/B42Labs/openstack-tester/internal/telemetry"
)

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
