package nova

import (
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
)

func TestDecideLiveMigration(t *testing.T) {
	up := func(n int) []hypervisors.Hypervisor {
		hs := make([]hypervisors.Hypervisor, n)
		for i := range hs {
			hs[i] = hypervisors.Hypervisor{State: "up", Status: "enabled"}
		}
		return hs
	}

	tests := []struct {
		name    string
		roles   []string
		rolesOK bool
		hosts   []hypervisors.Hypervisor
		wantOK  bool
		wantSub string
	}{
		{
			name:    "roles unavailable",
			rolesOK: false,
			hosts:   up(2),
			wantSub: "token roles unavailable",
		},
		{
			name:    "non-admin",
			roles:   []string{"member", "reader"},
			rolesOK: true,
			hosts:   up(2),
			wantSub: "lack the admin role",
		},
		{
			name:    "admin, zero usable hosts",
			roles:   []string{"admin"},
			rolesOK: true,
			hosts:   nil,
			wantSub: "0 usable compute host",
		},
		{
			name:    "admin, one usable host",
			roles:   []string{"admin"},
			rolesOK: true,
			hosts:   up(1),
			wantSub: "1 usable compute host",
		},
		{
			name:    "admin, down and disabled hosts excluded",
			roles:   []string{"Admin"}, // case-insensitive
			rolesOK: true,
			hosts: []hypervisors.Hypervisor{
				{State: "up", Status: "enabled"},
				{State: "down", Status: "enabled"},
				{State: "up", Status: "disabled"},
			},
			wantSub: "1 usable compute host",
		},
		{
			name:    "admin, two usable hosts",
			roles:   []string{"admin"},
			rolesOK: true,
			hosts:   up(2),
			wantOK:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := decideLiveMigration(tc.roles, tc.rolesOK, tc.hosts)
			if ok != tc.wantOK {
				t.Fatalf("decideLiveMigration() ok = %v, want %v (reason %q)", ok, tc.wantOK, reason)
			}
			if tc.wantSub != "" && !strings.Contains(reason, tc.wantSub) {
				t.Errorf("reason = %q, want it to mention %q", reason, tc.wantSub)
			}
			if tc.wantOK && reason != "" {
				t.Errorf("reason = %q, want empty on success", reason)
			}
		})
	}
}
