package chaos

import (
	"context"
	"time"
)

// Clock is the time seam the churn scheduler runs on: the real wall clock in
// production and a virtual clock in tests, so the determinism test is not at the
// mercy of wall-clock jitter. Only the single scheduler goroutine uses a Clock;
// the concurrent operation tasks measure their own latency off the real clock.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// RealClock implements Clock against the wall clock.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// Sleep blocks for d or until ctx is cancelled, returning ctx.Err() when the
// context ends first. A non-positive delay is a no-op (but still observes a
// cancelled context).
func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
