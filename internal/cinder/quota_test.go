package cinder

import (
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/quotasets"

	"github.com/B42Labs/dizzy/internal/cinder/plan"
)

// quotaPlan is a plan needing 3 volumes, 2 snapshots, and (2+3+4 volumes)+(2+3
// snapshot sizes) GiB. With vol-0001 resizing 2->4, totals are 4+3+2 volume GiB
// plus a snapshot each of vol-0001 (4) and vol-0002 (3): 9 + 7 = 16 GiB.
func quotaPlan() *plan.Plan {
	return &plan.Plan{
		Volumes: []plan.Volume{
			{Name: "vol-0001", SizeGiB: 2, ResizeToGiB: 4},
			{Name: "vol-0002", SizeGiB: 3},
			{Name: "vol-0003", SizeGiB: 2},
		},
		Snapshots: []plan.Snapshot{
			{Name: "snap-0001", Volume: "vol-0001"},
			{Name: "snap-0002", Volume: "vol-0002"},
		},
	}
}

func TestPlanNeeds(t *testing.T) {
	n := planNeeds(quotaPlan())
	if n.volumes != 3 || n.snapshots != 2 || n.gigabytes != 16 {
		t.Errorf("planNeeds = %+v, want {volumes:3 snapshots:2 gigabytes:16}", n)
	}
}

func TestCheckQuotaGlobal(t *testing.T) {
	need := planNeeds(quotaPlan())

	// Ample global quotas: no error.
	if err := checkQuota(need, &quotasets.QuotaSet{Volumes: 10, Snapshots: 10, Gigabytes: 1000}, ""); err != nil {
		t.Errorf("checkQuota with ample quota = %v, want nil", err)
	}

	// A negative limit means unlimited and never blocks.
	if err := checkQuota(need, &quotasets.QuotaSet{Volumes: -1, Snapshots: -1, Gigabytes: -1}, ""); err != nil {
		t.Errorf("checkQuota with unlimited quota = %v, want nil", err)
	}

	// Every dimension too small: the itemized error names all three.
	err := checkQuota(need, &quotasets.QuotaSet{Volumes: 1, Snapshots: 1, Gigabytes: 5}, "")
	if err == nil {
		t.Fatal("checkQuota with tight quota = nil, want error")
	}
	for _, want := range []string{"volumes", "snapshots", "gigabytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestCheckQuotaPerType(t *testing.T) {
	need := planNeeds(quotaPlan())

	// Global quota is ample, but the per-type gigabytes quota is tight: the
	// per-type check must catch it.
	q := &quotasets.QuotaSet{
		Volumes: 10, Snapshots: 10, Gigabytes: 1000,
		Extra: map[string]any{
			"volumes_ssd":   float64(10),
			"snapshots_ssd": float64(10),
			"gigabytes_ssd": float64(5),
		},
	}
	err := checkQuota(need, q, "ssd")
	if err == nil {
		t.Fatal("checkQuota with tight per-type gigabytes = nil, want error")
	}
	if !strings.Contains(err.Error(), "gigabytes_ssd") {
		t.Errorf("error %q does not name the per-type gigabytes quota", err.Error())
	}

	// A per-type quota Cinder never materialized (absent key) is unrestricted and
	// must not block, even when the global quota is ample.
	if err := checkQuota(need, &quotasets.QuotaSet{Volumes: 10, Snapshots: 10, Gigabytes: 1000}, "ssd"); err != nil {
		t.Errorf("checkQuota with absent per-type keys = %v, want nil", err)
	}
}
