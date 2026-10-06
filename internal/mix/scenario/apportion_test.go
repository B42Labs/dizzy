package scenario

import (
	"math"
	"reflect"
	"testing"
)

func TestApportion(t *testing.T) {
	tests := []struct {
		name   string
		total  int
		shares []float64
		want   []int
	}{
		{"exact division", 20, []float64{5, 3, 2}, []int{10, 6, 4}},
		{"tie goes to the lower index", 1, []float64{1, 1}, []int{1, 0}},
		{"leftover by largest remainder", 7, []float64{1, 1, 1}, []int{3, 2, 2}},
		{"largest remainder wins over index", 10, []float64{1, 2}, []int{3, 7}},
		{"zero total", 0, []float64{1}, []int{0}},
		{"negative total", -3, []float64{1}, []int{0}},
		{"zero shares", 5, []float64{0, 0}, []int{0, 0}},
		{"zero share never receives a unit", 5, []float64{0, 1, 0}, []int{0, 5, 0}},
		{"huge share", 6, []float64{1e308}, []int{6}},
		{"two huge shares", 3, []float64{1e308, 1e308}, []int{2, 1}},
		{"non-finite shares count as zero", 4, []float64{math.NaN(), 1, math.Inf(1), -1}, []int{0, 4, 0, 0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Apportion(tc.total, tc.shares); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Apportion(%d, %v) = %v, want %v", tc.total, tc.shares, got, tc.want)
			}
		})
	}
}

func TestApportionNoShares(t *testing.T) {
	for name, shares := range map[string][]float64{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			got := Apportion(5, shares)
			if got == nil || len(got) != 0 {
				t.Errorf("Apportion(5, %v) = %#v, want an empty non-nil slice", shares, got)
			}
		})
	}
}
