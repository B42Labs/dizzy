package serve

import (
	"sync/atomic"
	"time"
)

// Health tracks whether the process still makes progress. Progress is a
// recorded operation or a finished iteration, marked through Mark; a planned
// wait (the pause between two monitor iterations) is announced through
// SetWaiting and never counts as a stall. Mark runs on every API call of
// every worker, so the state is two atomics rather than a mutex.
//
// A nil *Health is the disabled tracker: Mark and SetWaiting do nothing and
// Check always reports healthy.
//
// Times are kept as offsets from a base reading taken at construction, so
// with time.Now the idle time comes from the monotonic clock and a step of the
// wall clock (an NTP correction, a resumed VM) neither fakes a stall nor
// hides one.
type Health struct {
	stallAfter time.Duration
	now        func() time.Time
	base       time.Time
	last       atomic.Int64 // nanoseconds from base to the last progress
	waiting    atomic.Bool
}

// NewHealth returns a tracker that turns unhealthy once no progress was
// marked for stallAfter, measured on now. The clock starts at construction,
// so a process that never records anything stalls stallAfter after it
// started. A stallAfter of 0 disables stall detection.
func NewHealth(stallAfter time.Duration, now func() time.Time) *Health {
	return &Health{stallAfter: stallAfter, now: now, base: now()}
}

// sinceBase returns how far now is past the base reading.
func (h *Health) sinceBase() time.Duration { return h.now().Sub(h.base) }

// Mark records that progress happened now.
func (h *Health) Mark() {
	if h == nil {
		return
	}
	h.last.Store(int64(h.sinceBase()))
}

// SetWaiting announces the start (true) or the end (false) of a planned
// wait. While waiting the process is healthy however long the wait is. The
// end of a wait marks progress before it clears the flag, so a Check that
// runs concurrently never sees the flag cleared with the time from before
// the wait.
func (h *Health) SetWaiting(waiting bool) {
	if h == nil {
		return
	}
	if !waiting {
		h.Mark()
	}
	h.waiting.Store(waiting)
}

// Check reports whether the process is healthy and how long it has been
// since the last progress. It returns (true, 0) on a nil tracker, with stall
// detection disabled, and during a planned wait. An idle time equal to the
// limit is unhealthy.
func (h *Health) Check() (healthy bool, idle time.Duration) {
	if h == nil || h.stallAfter == 0 || h.waiting.Load() {
		return true, 0
	}
	idle = h.sinceBase() - time.Duration(h.last.Load())
	return idle < h.stallAfter, idle
}
