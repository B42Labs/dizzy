package plan

import (
	"encoding/json"
	"strings"
	"testing"
)

// validPlan returns a small, internally consistent plan the error-case tests
// mutate into invalidity.
func validPlan() *Plan {
	return &Plan{
		Scenario:     "test",
		Seed:         42,
		Image:        "cirros",
		Flavor:       "m1.tiny",
		ResizeFlavor: "m1.small",
		Networks: []Network{
			{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"},
			{Name: "net-0002", Subnet: "sub-0002", CIDR: "10.0.2.0/24"},
		},
		Servers: []Server{
			{Name: "srv-0001", Networks: []string{"net-0001"}, Resize: true},
			{Name: "srv-0002", Networks: []string{"net-0001", "net-0002"}, BootFromVolume: true, RootVolumeGiB: 5},
		},
		Volumes: []Volume{{Name: "vol-0001", SizeGiB: 2, Server: "srv-0001"}},
		Ports:   []Port{{Name: "port-0001", Network: "net-0002", Server: "srv-0002"}},
	}
}

func TestValidateAcceptsWellFormedPlan(t *testing.T) {
	if err := validPlan().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateRejectsMalformedPlans(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Plan)
		wantSub string
	}{
		{
			name:    "server with no network",
			mutate:  func(p *Plan) { p.Servers[0].Networks = nil },
			wantSub: "references no network",
		},
		{
			name:    "server references unknown network",
			mutate:  func(p *Plan) { p.Servers[0].Networks = []string{"net-9999"} },
			wantSub: "unknown network",
		},
		{
			name:    "boot from volume with zero root size",
			mutate:  func(p *Plan) { p.Servers[1].RootVolumeGiB = 0 },
			wantSub: "root size 0",
		},
		{
			name:    "resize without resize flavor",
			mutate:  func(p *Plan) { p.ResizeFlavor = "" },
			wantSub: "no resize flavor",
		},
		{
			name:    "volume with zero size",
			mutate:  func(p *Plan) { p.Volumes[0].SizeGiB = 0 },
			wantSub: "size 0 GiB",
		},
		{
			name:    "volume references unknown server",
			mutate:  func(p *Plan) { p.Volumes[0].Server = "srv-9999" },
			wantSub: "unknown server",
		},
		{
			name:    "port references unknown server",
			mutate:  func(p *Plan) { p.Ports[0].Server = "srv-9999" },
			wantSub: "unknown server",
		},
		{
			name:    "port references unknown network",
			mutate:  func(p *Plan) { p.Ports[0].Network = "net-9999" },
			wantSub: "unknown network",
		},
		{
			// srv-0001 joins only net-0001, so putting its port on net-0002 is a
			// membership violation even though net-0002 exists.
			name:    "port on a network the server does not join",
			mutate:  func(p *Plan) { p.Ports[0].Server = "srv-0001"; p.Ports[0].Network = "net-0002" },
			wantSub: "not one of server",
		},
		{
			name:    "server group with another policy",
			mutate:  func(p *Plan) { p.ServerGroups = []ServerGroup{{Name: "grp-0001", Policy: "affinity"}} },
			wantSub: `server group "grp-0001" has policy "affinity", want "anti-affinity" or "soft-anti-affinity"`,
		},
		{
			name: "server references unknown server group",
			mutate: func(p *Plan) {
				p.ServerGroups = []ServerGroup{{Name: "grp-0001", Policy: PolicySoftAntiAffinity}}
				p.Servers[0].Group = "grp-0009"
			},
			wantSub: `server "srv-0001" references unknown server group "grp-0009"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validPlan()
			tc.mutate(p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate() = %q, want it to mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestCountHelpers(t *testing.T) {
	p := &Plan{
		Servers: []Server{
			{Name: "a", Resize: true, LiveMigrate: true, Delete: true, StopStart: StopStartSoft, BootFromVolume: true, RootVolumeGiB: 1},
			{Name: "b", StopStart: StopStartHard, ColdMigrate: true},
			{Name: "c", ColdMigrate: true},
		},
		Volumes: []Volume{{Name: "v1", Detach: true}, {Name: "v2"}},
		Ports:   []Port{{Name: "p1"}, {Name: "p2", Detach: true}, {Name: "p3", Detach: true}},
	}
	if got := p.Resizes(); got != 1 {
		t.Errorf("Resizes() = %d, want 1", got)
	}
	if got := p.LiveMigrations(); got != 1 {
		t.Errorf("LiveMigrations() = %d, want 1", got)
	}
	if got := p.ColdMigrations(); got != 2 {
		t.Errorf("ColdMigrations() = %d, want 2", got)
	}
	if got := (&Plan{}).ColdMigrations(); got != 0 {
		t.Errorf("ColdMigrations() of a plan without servers = %d, want 0", got)
	}
	if got := p.Deletes(); got != 1 {
		t.Errorf("Deletes() = %d, want 1", got)
	}
	if soft, hard := p.StopStarts(); soft != 1 || hard != 1 {
		t.Errorf("StopStarts() = (%d, %d), want (1, 1)", soft, hard)
	}
	if got := p.BootsFromVolume(); got != 1 {
		t.Errorf("BootsFromVolume() = %d, want 1", got)
	}
	if got := p.DetachedVolumes(); got != 1 {
		t.Errorf("DetachedVolumes() = %d, want 1", got)
	}
	if got := p.DetachedPorts(); got != 2 {
		t.Errorf("DetachedPorts() = %d, want 2", got)
	}
}

func TestSummaryIsDeterministic(t *testing.T) {
	p := validPlan()
	first := p.Summary()
	if second := p.Summary(); first != second {
		t.Errorf("Summary() not deterministic:\n%q\n%q", first, second)
	}
	if !strings.Contains(first, "cirros") || !strings.Contains(first, "m1.tiny") {
		t.Errorf("Summary() = %q, want image and flavor named", first)
	}
}

// TestServerColdMigrateOmittedWhenFalse confirms a server that is not
// cold-migrated encodes without a coldMigrate key, so the plans of every
// existing scenario keep their bytes.
func TestServerColdMigrateOmittedWhenFalse(t *testing.T) {
	for _, tc := range []struct {
		server Server
		want   bool
	}{
		{Server{Name: "srv-0001", Networks: []string{"net-0001"}}, false},
		{Server{Name: "srv-0001", Networks: []string{"net-0001"}, ColdMigrate: true}, true},
	} {
		data, err := json.Marshal(tc.server)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if got := strings.Contains(string(data), `"coldMigrate"`); got != tc.want {
			t.Errorf("JSON %s has a coldMigrate key = %v, want %v", data, got, tc.want)
		}
	}
}

// TestValidateServerGroups confirms a plan whose servers boot into planned
// groups of either policy validates, as does one without groups.
func TestValidateServerGroups(t *testing.T) {
	p := validPlan()
	p.ServerGroups = []ServerGroup{
		{Name: "grp-0001", Policy: PolicyAntiAffinity},
		{Name: "grp-0002", Policy: PolicySoftAntiAffinity},
	}
	p.Servers[0].Group, p.Servers[1].Group = "grp-0001", "grp-0002"
	if err := p.Validate(); err != nil {
		t.Errorf("Validate() with server groups = %v, want nil", err)
	}
	if err := validPlan().Validate(); err != nil {
		t.Errorf("Validate() without server groups = %v, want nil", err)
	}
}

// TestServerGroupKeysOmittedWhenEmpty confirms a plan without server groups
// and a server without a group encode without the serverGroups and group
// keys, so the plans of every existing scenario keep their bytes.
func TestServerGroupKeysOmittedWhenEmpty(t *testing.T) {
	data, err := json.Marshal(validPlan())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"serverGroups"`, `"group"`} {
		if strings.Contains(string(data), key) {
			t.Errorf("JSON %s has the key %s, want none", data, key)
		}
	}

	p := validPlan()
	p.ServerGroups = []ServerGroup{{Name: "grp-0001", Policy: PolicyAntiAffinity}}
	p.Servers[0].Group = "grp-0001"
	if data, err = json.Marshal(p); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"serverGroups"`, `"group":"grp-0001"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("JSON %s lacks %s", data, key)
		}
	}
}
