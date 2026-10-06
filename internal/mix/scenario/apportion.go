package scenario

import (
	"math"
	"sort"
)

// Apportion divides total among shares with the largest-remainder method: each
// index gets floor(total*share/sum) and the units left over go one each to the
// indices with the largest fractional remainder, ties to the lower index. A
// share that is not a positive finite number counts as 0 and never receives a
// unit. A total of 0 or less, or no positive share, yields all zeros; nil or
// empty shares yield an empty non-nil slice.
func Apportion(total int, shares []float64) []int {
	out := make([]int, len(shares))
	if total <= 0 {
		return out
	}

	// Scaling every share by the largest one keeps the sum finite however
	// large the shares are.
	var largest float64
	for _, s := range shares {
		if counts(s) && s > largest {
			largest = s
		}
	}
	if largest == 0 {
		return out
	}
	scaled := make([]float64, len(shares))
	var sum float64
	for i, s := range shares {
		if counts(s) {
			scaled[i] = s / largest
			sum += scaled[i]
		}
	}

	remainders := make([]float64, len(shares))
	var order []int
	left := total
	for i, s := range scaled {
		if s == 0 {
			continue
		}
		quota := float64(total) * s / sum
		out[i] = int(math.Floor(quota))
		remainders[i] = quota - math.Floor(quota)
		left -= out[i]
		order = append(order, i)
	}
	sort.SliceStable(order, func(a, b int) bool { return remainders[order[a]] > remainders[order[b]] })
	for k := 0; k < left && k < len(order); k++ {
		out[order[k]]++
	}
	return out
}

// counts reports whether a share takes part in the apportionment.
func counts(share float64) bool {
	return share > 0 && !math.IsInf(share, 1)
}
