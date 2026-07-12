package nova

import (
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/quotasets"

	"github.com/B42Labs/dizzy/internal/nova/plan"
)

func TestPlanNeeds(t *testing.T) {
	p := &plan.Plan{
		Servers: []plan.Server{
			{Name: "a"},
			{Name: "b", Resize: true},
			{Name: "c"},
		},
	}
	boot := Flavor{VCPUs: 1, RAM: 512}
	resize := Flavor{VCPUs: 2, RAM: 2048}
	n := planNeeds(p, boot, resize)
	if n.instances != 3 {
		t.Errorf("instances = %d, want 3", n.instances)
	}
	// a and c at boot (1 vCPU), b at the resize max (2 vCPUs) => 1 + 2 + 1.
	if n.cores != 4 {
		t.Errorf("cores = %d, want 4", n.cores)
	}
	// a and c at 512 MB, b at max(512, 2048) => 512 + 2048 + 512.
	if n.ram != 3072 {
		t.Errorf("ram = %d, want 3072", n.ram)
	}
}

func TestCheckQuota(t *testing.T) {
	// detail builds a QuotaDetailSet from per-dimension (limit, used) pairs, the
	// shape the pre-check now compares the plan against so pre-existing usage
	// counts toward the ceiling.
	detail := func(insLimit, insUsed, coreLimit, coreUsed, ramLimit, ramUsed int) quotasets.QuotaDetailSet {
		return quotasets.QuotaDetailSet{
			Instances: quotasets.QuotaDetail{Limit: insLimit, InUse: insUsed},
			Cores:     quotasets.QuotaDetail{Limit: coreLimit, InUse: coreUsed},
			RAM:       quotasets.QuotaDetail{Limit: ramLimit, InUse: ramUsed},
		}
	}
	tests := []struct {
		name    string
		need    needs
		quota   quotasets.QuotaDetailSet
		wantErr bool
		wantSub []string
	}{
		{
			name:  "within quota",
			need:  needs{instances: 3, cores: 3, ram: 1536},
			quota: detail(10, 0, 20, 0, 51200, 0),
		},
		{
			name:    "over instances",
			need:    needs{instances: 11, cores: 3, ram: 1536},
			quota:   detail(10, 0, 20, 0, 51200, 0),
			wantErr: true,
			wantSub: []string{"instances"},
		},
		{
			name:    "over cores and ram",
			need:    needs{instances: 3, cores: 40, ram: 999999},
			quota:   detail(10, 0, 20, 0, 51200, 0),
			wantErr: true,
			wantSub: []string{"cores", "ram"},
		},
		{
			// Fits the raw limit (5 <= 10) but not the remaining headroom after
			// pre-existing usage (8 in use leaves only 2): the whole point of
			// reading in-use rather than the bare limit.
			name:    "fits limit but not remaining headroom",
			need:    needs{instances: 5, cores: 3, ram: 1536},
			quota:   detail(10, 8, 20, 0, 51200, 0),
			wantErr: true,
			wantSub: []string{"instances"},
		},
		{
			name:  "negative limit is unlimited",
			need:  needs{instances: 1000, cores: 1000, ram: 1000000},
			quota: detail(-1, 500, -1, 500, -1, 500),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkQuota(tc.need, tc.quota)
			if tc.wantErr && err == nil {
				t.Fatalf("checkQuota() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkQuota() = %v, want nil", err)
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("checkQuota() = %q, want it to mention %q", err.Error(), sub)
				}
			}
		})
	}
}
