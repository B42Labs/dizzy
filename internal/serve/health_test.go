package serve

import (
	"testing"
	"time"
)

// testClock is a settable clock for the health tracker.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestHealth(limit time.Duration) (*Health, *testClock) {
	clk := &testClock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	return NewHealth(limit, clk.now), clk
}

func assertCheck(t *testing.T, h *Health, wantHealthy bool, wantIdle time.Duration) {
	t.Helper()
	healthy, idle := h.Check()
	if healthy != wantHealthy || idle != wantIdle {
		t.Errorf("Check() = (%v, %s), want (%v, %s)", healthy, idle, wantHealthy, wantIdle)
	}
}

func TestHealthFreshIsHealthy(t *testing.T) {
	h, _ := newTestHealth(5 * time.Minute)
	assertCheck(t, h, true, 0)
}

func TestHealthStallsAtLimit(t *testing.T) {
	h, clk := newTestHealth(5 * time.Minute)
	clk.advance(4*time.Minute + 59*time.Second)
	assertCheck(t, h, true, 4*time.Minute+59*time.Second)
	clk.advance(time.Second)
	assertCheck(t, h, false, 5*time.Minute)
}

func TestHealthMarkResets(t *testing.T) {
	h, clk := newTestHealth(5 * time.Minute)
	clk.advance(10 * time.Minute)
	h.Mark()
	assertCheck(t, h, true, 0)
}

func TestHealthWaitingIsHealthy(t *testing.T) {
	h, clk := newTestHealth(5 * time.Minute)
	h.SetWaiting(true)
	clk.advance(time.Hour)
	assertCheck(t, h, true, 0)

	h.SetWaiting(false)
	assertCheck(t, h, true, 0)

	clk.advance(5 * time.Minute)
	assertCheck(t, h, false, 5*time.Minute)
}

func TestHealthZeroLimitNeverStalls(t *testing.T) {
	h, clk := newTestHealth(0)
	clk.advance(24 * time.Hour)
	assertCheck(t, h, true, 0)
}

func TestNilHealthIsNoOp(t *testing.T) {
	var h *Health
	h.Mark()
	h.SetWaiting(true)
	h.SetWaiting(false)
	assertCheck(t, h, true, 0)
}
