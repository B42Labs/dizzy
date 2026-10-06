package nova

import (
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/quotasets"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"

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
	if n.serverGroups != 0 || n.groupMembers != 0 {
		t.Errorf("serverGroups, groupMembers = %d, %d for a plan without groups, want 0, 0", n.serverGroups, n.groupMembers)
	}
}

// TestPlanNeedsServerGroups confirms a plan with server groups needs one
// server_groups unit per group and as many server_group_members as its
// largest group has servers.
func TestPlanNeedsServerGroups(t *testing.T) {
	p := &plan.Plan{
		ServerGroups: []plan.ServerGroup{{Name: "grp-0001"}, {Name: "grp-0002"}},
		Servers: []plan.Server{
			{Name: "a", Group: "grp-0001"},
			{Name: "b", Group: "grp-0002"},
			{Name: "c", Group: "grp-0001"},
			{Name: "d", Group: "grp-0002"},
			{Name: "e", Group: "grp-0001"},
		},
	}
	n := planNeeds(p, Flavor{VCPUs: 1, RAM: 512}, Flavor{})
	if n.serverGroups != 2 || n.groupMembers != 3 {
		t.Errorf("serverGroups, groupMembers = %d, %d, want 2, 3", n.serverGroups, n.groupMembers)
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

// TestProjectIDWithoutAuthResult confirms a client that recorded no auth result
// reports no project rather than guessing one.
func TestProjectIDWithoutAuthResult(t *testing.T) {
	gc := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}}
	if id, ok := ProjectID(gc); id != "" || ok {
		t.Errorf("ProjectID = (%q, %v), want (\"\", false)", id, ok)
	}
}

// TestProjectIDFromToken confirms the project id is read from a v3 token auth
// result.
func TestProjectIDFromToken(t *testing.T) {
	gc := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}}
	ar := tokens.CreateResult{}
	ar.Body = map[string]any{"token": map[string]any{"project": map[string]any{"id": "proj-1"}}}
	if err := gc.SetTokenAndAuthResult(ar); err != nil {
		t.Fatalf("SetTokenAndAuthResult: %v", err)
	}
	if id, ok := ProjectID(gc); id != "proj-1" || !ok {
		t.Errorf("ProjectID = (%q, %v), want (\"proj-1\", true)", id, ok)
	}
}

// TestCheckQuotaServerGroups confirms the server-group limits join the
// itemized error for a plan with groups, count as unlimited when negative, and
// are not checked for a plan without groups.
func TestCheckQuotaServerGroups(t *testing.T) {
	quota := func(groupLimit, groupUsed, memberLimit int) quotasets.QuotaDetailSet {
		return quotasets.QuotaDetailSet{
			Instances:          quotasets.QuotaDetail{Limit: 10},
			Cores:              quotasets.QuotaDetail{Limit: 20},
			RAM:                quotasets.QuotaDetail{Limit: 51200},
			ServerGroups:       quotasets.QuotaDetail{Limit: groupLimit, InUse: groupUsed},
			ServerGroupMembers: quotasets.QuotaDetail{Limit: memberLimit, InUse: 7},
		}
	}
	withGroups := needs{instances: 5, cores: 5, ram: 2560, serverGroups: 2, groupMembers: 3}

	t.Run("over both limits", func(t *testing.T) {
		err := checkQuota(withGroups, quota(10, 9, 2))
		if err == nil {
			t.Fatal("checkQuota() = nil, want error")
		}
		for _, want := range []string{
			"server groups need 2, available 1 (limit 10, used 9)",
			"server group members need 3 per group, limit 2",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("checkQuota() = %q, want it to contain %q", err.Error(), want)
			}
		}
	})

	t.Run("unlimited", func(t *testing.T) {
		if err := checkQuota(withGroups, quota(-1, 9, -1)); err != nil {
			t.Errorf("checkQuota() = %v, want nil", err)
		}
	})

	t.Run("plan without groups", func(t *testing.T) {
		if err := checkQuota(needs{instances: 5, cores: 5, ram: 2560}, quota(10, 12, 0)); err != nil {
			t.Errorf("checkQuota() = %v, want nil", err)
		}
	})
}
