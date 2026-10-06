package metrics

import (
	"math"
	"sort"
	"time"
)

// relativeAccuracy is the histogram's relative error bound: a percentile it
// reports lies within 1% of the recorded duration at that rank.
const relativeAccuracy = 0.01

// gamma is the ratio between the upper bounds of two adjacent histogram keys.
// Key i counts the durations in (gamma^(i-1), gamma^i] nanoseconds.
const gamma = (1 + relativeAccuracy) / (1 - relativeAccuracy)

// logGamma is ln(gamma), the divisor that maps a duration onto its key.
var logGamma = math.Log(gamma)

// histogram summarizes a stream of durations in fixed-size state, following
// the log-bucket scheme of DDSketch (Masson, Rim, Lee, VLDB 2019). Count, sum,
// min and max are exact; every positive duration is counted under the key
// ceil(log_gamma(d)), so a percentile read back is within relativeAccuracy of
// the true value. A time.Duration is below 2^63 ns, so counts holds at most
// 2,185 keys however many durations are added. The zero value is ready to use.
// It is not safe for concurrent use; the Collector guards it.
type histogram struct {
	n      int
	sum    float64 // nanoseconds; a float cannot overflow over a long run
	min    time.Duration
	max    time.Duration
	zeros  int
	counts map[int]int
}

// add records one duration. A negative duration counts as 0.
func (h *histogram) add(d time.Duration) {
	if d < 0 {
		d = 0
	}
	if h.n == 0 || d < h.min {
		h.min = d
	}
	if h.n == 0 || d > h.max {
		h.max = d
	}
	h.n++
	h.sum += float64(d)
	if d == 0 {
		h.zeros++
		return
	}
	if h.counts == nil {
		h.counts = make(map[int]int)
	}
	h.counts[int(math.Ceil(math.Log(float64(d))/logGamma))]++
}

// latency returns the distribution of the recorded durations: Min and Max are
// exact, Mean is exact up to float rounding, and the percentiles are estimates
// within relativeAccuracy. It returns the zero Latency when nothing was added.
func (h *histogram) latency() Latency {
	if h.n == 0 {
		return Latency{}
	}
	keys := make([]int, 0, len(h.counts))
	for k := range h.counts {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return Latency{
		Min:    h.min,
		Mean:   time.Duration(h.sum / float64(h.n)),
		Median: h.percentile(keys, 50),
		P90:    h.percentile(keys, 90),
		P95:    h.percentile(keys, 95),
		P99:    h.percentile(keys, 99),
		Max:    h.max,
	}
}

// percentile estimates the p-th percentile with the nearest-rank method that
// percentile applies to a sorted slice: it walks the zero count and then the
// ascending keys until the cumulative count reaches rank ceil(p/100 * n). Key i
// stands for 2 * gamma^i / (gamma + 1) ns, the value within relativeAccuracy of
// every duration the key covers. The estimate is clamped to [min, max] while it
// is still a float, so a key near 2^63 cannot overflow the conversion.
func (h *histogram) percentile(keys []int, p float64) time.Duration {
	rank := int(math.Ceil(p / 100 * float64(h.n)))
	rank = max(1, min(rank, h.n))

	var v float64
	cum := h.zeros
	for _, k := range keys {
		if cum >= rank {
			break
		}
		cum += h.counts[k]
		v = 2 * math.Pow(gamma, float64(k)) / (gamma + 1)
	}

	switch {
	case v <= float64(h.min):
		return h.min
	case v >= float64(h.max):
		return h.max
	}
	return time.Duration(v)
}
