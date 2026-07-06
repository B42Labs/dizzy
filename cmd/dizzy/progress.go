package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/B42Labs/dizzy/internal/metrics"
)

// heartbeatInterval is how often a long-running command logs a one-line progress
// summary while it works, so apply, cleanup, and chaos are not silent between
// their start and their final-summary lines. The per-operation lines the
// executor and churn engine emit show what each individual call is doing; this
// heartbeat is the periodic digest on top of them. Both are logged at info and
// are silenced together with --log-level warn.
const heartbeatInterval = 5 * time.Second

// heartbeat is a running progress reporter. It owns a goroutine that logs a
// summary line every heartbeatInterval until stop is called; stop cancels the
// goroutine and waits for it to finish, so no goroutine outlives the command.
type heartbeat struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startHeartbeat launches a heartbeat that logs msg with the attributes snapshot
// returns, every heartbeatInterval, until the returned heartbeat is stopped or
// ctx is cancelled. The first line appears after one interval, so a run that
// finishes quickly stays quiet. snapshot is only ever called from the single
// heartbeat goroutine, so it may keep unsynchronized state (e.g. the previous
// tick's counts) to report per-interval deltas.
func startHeartbeat(ctx context.Context, msg string, snapshot func() []any) *heartbeat {
	ctx, cancel := context.WithCancel(ctx)
	hb := &heartbeat{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(hb.done)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				slog.Info(msg, snapshot()...)
			}
		}
	}()
	return hb
}

// stop ends the heartbeat and waits for its goroutine to return. It is safe to
// call exactly once; callers defer it or call it right after the work finishes.
func (h *heartbeat) stop() {
	h.cancel()
	<-h.done
}

// collectorSnapshot builds a heartbeat snapshot over c that reports the API-call
// activity accumulated so far: the cumulative op count, how many new ops landed
// since the previous tick (the live rate), the ok/failed split, and elapsed time
// since start. Any extra key/value attrs are appended verbatim, so a caller can
// add context such as the configured churn duration.
func collectorSnapshot(c *metrics.Collector, start time.Time, extra ...any) func() []any {
	var prev int
	return func() []any {
		attempted, ok, failed := c.Snapshot()
		newOps := attempted - prev
		prev = attempted
		attrs := []any{
			"ops", attempted,
			"new", newOps,
			"ok", ok,
			"failed", failed,
			"elapsed", time.Since(start).Round(time.Second),
		}
		return append(attrs, extra...)
	}
}
