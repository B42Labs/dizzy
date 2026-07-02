package plan

import (
	"math"
	"strings"
	"testing"
)

// validPlan is a small plan exercising resized and unresized volumes plus
// snapshots of both, used as the baseline the Validate cases mutate.
func validPlan() *Plan {
	return &Plan{
		Scenario: "small",
		Seed:     42,
		Volumes: []Volume{
			{Name: "vol-0001", SizeGiB: 2, ResizeToGiB: 5},
			{Name: "vol-0002", SizeGiB: 3},
		},
		Snapshots: []Snapshot{
			{Name: "snap-0001", Volume: "vol-0001"},
			{Name: "snap-0002", Volume: "vol-0002"},
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Plan)
		wantErr bool
	}{
		{name: "valid", mutate: func(*Plan) {}},
		{
			name:    "zero size rejected",
			mutate:  func(p *Plan) { p.Volumes[1].SizeGiB = 0 },
			wantErr: true,
		},
		{
			name:    "negative size rejected",
			mutate:  func(p *Plan) { p.Volumes[0].SizeGiB = -1 },
			wantErr: true,
		},
		{
			name:    "resize target equal to size rejected",
			mutate:  func(p *Plan) { p.Volumes[0].ResizeToGiB = p.Volumes[0].SizeGiB },
			wantErr: true,
		},
		{
			name:    "resize target below size rejected",
			mutate:  func(p *Plan) { p.Volumes[0].ResizeToGiB = 1 },
			wantErr: true,
		},
		{
			name:    "snapshot of unknown volume rejected",
			mutate:  func(p *Plan) { p.Snapshots[0].Volume = "vol-9999" },
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validPlan()
			tc.mutate(p)
			err := p.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestResizedVolumesAndTotalGiB(t *testing.T) {
	p := validPlan()

	if got := p.ResizedVolumes(); got != 1 {
		t.Errorf("ResizedVolumes() = %d, want 1", got)
	}

	// Final volume sizes: vol-0001 resizes 2->5, vol-0002 stays 3, so 8 GiB of
	// volumes. Snapshots count at their source's final size: snap of vol-0001 is
	// 5, snap of vol-0002 is 3, so 8 GiB of snapshots. Total 16.
	if got := p.TotalGiB(); got != 16 {
		t.Errorf("TotalGiB() = %d, want 16", got)
	}
}

// TestTotalGiBBeyondInt32 pins the int64 accumulation that keeps the quota
// pre-check correct: a large-but-legal plan can sum past math.MaxInt32, and
// TotalGiB must return it intact rather than wrapping to a small or negative
// value that would let an over-quota plan slip past checkQuota. On a 64-bit
// build int is already 64-bit so this cannot wrap at runtime here; the
// assertion locks the int64 contract that keeps 32-bit builds correct.
func TestTotalGiBBeyondInt32(t *testing.T) {
	const size = 1_500_000_000 // GiB; two of these sum past math.MaxInt32
	const want = 2 * int64(size)
	if want <= math.MaxInt32 {
		t.Fatalf("test setup: want %d does not exceed MaxInt32", want)
	}
	p := &Plan{
		Volumes: []Volume{
			{Name: "vol-0001", SizeGiB: size},
			{Name: "vol-0002", SizeGiB: size},
		},
	}
	if got := p.TotalGiB(); got != want {
		t.Errorf("TotalGiB() = %d, want %d", got, want)
	}
}

func TestSummary(t *testing.T) {
	got := validPlan().Summary()
	for _, want := range []string{`scenario "small"`, "volumes:", "1 resized", "snapshots:", "total GiB:", "16"} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary() missing %q:\n%s", want, got)
		}
	}
}
