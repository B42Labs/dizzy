package metrics

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// TestHistogramAccuracy confirms the histogram's percentiles stay within 1% (plus
// one nanosecond of float truncation) of the exact nearest-rank percentiles over
// a wide spread of durations, while min, max and mean stay exact.
func TestHistogramAccuracy(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	lo, hi := int64(time.Millisecond), int64(10*time.Minute)
	durs := make([]time.Duration, 10_000)
	var h histogram
	for i := range durs {
		durs[i] = time.Duration(lo + rng.Int63n(hi-lo+1))
		h.add(durs[i])
	}

	got, want := h.latency(), ComputeLatency(durs)
	for _, tc := range []struct {
		name      string
		got, want time.Duration
	}{
		{"median", got.Median, want.Median},
		{"p90", got.P90, want.P90},
		{"p95", got.P95, want.P95},
		{"p99", got.P99, want.P99},
	} {
		if diff := math.Abs(float64(tc.got - tc.want)); diff > float64(tc.want)/100+1 {
			t.Errorf("%s = %s, want %s within 1%% (off by %s)", tc.name, tc.got, tc.want, time.Duration(diff))
		}
	}
	if got.Min != want.Min || got.Max != want.Max {
		t.Errorf("min/max = %s/%s, want exact %s/%s", got.Min, got.Max, want.Min, want.Max)
	}
	if diff := got.Mean - want.Mean; diff > time.Microsecond || diff < -time.Microsecond {
		t.Errorf("mean = %s, want %s within 1µs", got.Mean, want.Mean)
	}
}

// TestHistogramEmpty confirms an empty histogram reports the zero Latency rather
// than dividing by zero.
func TestHistogramEmpty(t *testing.T) {
	var h histogram
	if got := h.latency(); got != (Latency{}) {
		t.Errorf("latency() of an empty histogram = %+v, want zero", got)
	}
}

// TestHistogramSingleValue confirms one recorded duration is reported exactly in
// every field: the percentile estimate is clamped to the exact min and max.
func TestHistogramSingleValue(t *testing.T) {
	var h histogram
	h.add(42 * time.Millisecond)
	want := 42 * time.Millisecond
	if got := h.latency(); got != (Latency{Min: want, Mean: want, Median: want, P90: want, P95: want, P99: want, Max: want}) {
		t.Errorf("latency() = %+v, want 42ms in every field", got)
	}
}

// TestHistogramZeroAndNegative confirms a zero and a negative duration both
// count as 0, so every field reads 0 instead of a negative latency.
func TestHistogramZeroAndNegative(t *testing.T) {
	var h histogram
	h.add(0)
	h.add(-5 * time.Millisecond)
	if got := h.latency(); got != (Latency{}) {
		t.Errorf("latency() = %+v, want 0 in every field", got)
	}
	if h.n != 2 || h.zeros != 2 {
		t.Errorf("n/zeros = %d/%d, want 2/2", h.n, h.zeros)
	}
}

// TestHistogramKeyBound confirms the histogram's size does not grow with the
// number of durations: a million durations spread over the whole positive
// time.Duration range, extremes included, land on at most 2,185 keys.
func TestHistogramKeyBound(t *testing.T) {
	var h histogram
	h.add(1)
	h.add(math.MaxInt64)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 1_000_000; i++ {
		// A log-uniform spread touches every key between 1ns and 2^63ns.
		h.add(time.Duration(math.Exp(rng.Float64() * math.Log(math.MaxInt64))))
	}
	if got := len(h.counts); got > 2185 {
		t.Errorf("len(counts) = %d after a million durations, want at most 2185", got)
	}
	if got := h.latency(); got.Max != math.MaxInt64 || got.P99 > got.Max {
		t.Errorf("latency() = %+v, want max %d and p99 clamped below it", got, int64(math.MaxInt64))
	}
}
