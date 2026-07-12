package plan

import (
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
			{Name: "b", StopStart: StopStartHard},
			{Name: "c"},
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
