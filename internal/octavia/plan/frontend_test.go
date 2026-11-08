package plan

import (
	"encoding/json"
	"testing"
)

// wellFormedFrontend returns a small, internally consistent frontend the
// error-case tests mutate into invalidity, shaped as the mix generator shapes
// one: lb-0001 on net-0001 listens on 80 and 443 with srv-0001 and srv-0002 in
// both pools, and lb-0002 on net-0002 listens on 80 too, which is valid because
// ports differ only per load balancer, with srv-0003 in its pool. Every member
// of a pool shares the pool's NodePort.
func wellFormedFrontend() *Frontend {
	return &Frontend{
		Provider: "ovn",
		LoadBalancers: []LoadBalancer{
			{Name: "lb-0001", Network: "net-0001"},
			{Name: "lb-0002", Network: "net-0002"},
		},
		Listeners: []Listener{
			{Name: "lsn-0001", LoadBalancer: "lb-0001", Port: 80},
			{Name: "lsn-0002", LoadBalancer: "lb-0001", Port: 443},
			{Name: "lsn-0003", LoadBalancer: "lb-0002", Port: 80},
		},
		Pools: []Pool{
			{Name: "pool-0001", Listener: "lsn-0001"},
			{Name: "pool-0002", Listener: "lsn-0002"},
			{Name: "pool-0003", Listener: "lsn-0003"},
		},
		Members: []ServerMember{
			{Name: "pool-0001-member-01", Pool: "pool-0001", Server: "srv-0001", Port: 30000},
			{Name: "pool-0001-member-02", Pool: "pool-0001", Server: "srv-0002", Port: 30000},
			{Name: "pool-0002-member-01", Pool: "pool-0002", Server: "srv-0001", Port: 30001},
			{Name: "pool-0002-member-02", Pool: "pool-0002", Server: "srv-0002", Port: 30001},
			{Name: "pool-0003-member-01", Pool: "pool-0003", Server: "srv-0003", Port: 30002},
		},
		HealthMonitors: []HealthMonitor{
			{Name: "hm-0001", Pool: "pool-0001"},
			{Name: "hm-0002", Pool: "pool-0002"},
			{Name: "hm-0003", Pool: "pool-0003"},
		},
	}
}

func TestFrontendValidateAcceptsWellFormedFrontend(t *testing.T) {
	if err := wellFormedFrontend().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// TestFrontendValidateAcceptsEmptyCollections asserts a frontend without any
// resource is valid, whether its slices are nil or empty, with or without a
// provider.
func TestFrontendValidateAcceptsEmptyCollections(t *testing.T) {
	for name, f := range map[string]*Frontend{
		"nil":                 {},
		"empty":               {LoadBalancers: []LoadBalancer{}, Listeners: []Listener{}, Pools: []Pool{}, Members: []ServerMember{}, HealthMonitors: []HealthMonitor{}},
		"provider only":       {Provider: "amphora"},
		"load balancers only": {LoadBalancers: []LoadBalancer{{Name: "lb-0001", Network: "net-0001"}}},
	} {
		if err := f.Validate(); err != nil {
			t.Errorf("%s collections: Validate() = %v, want nil", name, err)
		}
	}
}

// TestFrontendValidateAcceptsBoundaryValues asserts the edges of the port
// range are inside it, that what is unique only per load balancer or per pool
// may repeat across them, and that a network or server name is not resolved,
// since the plan that creates them is the caller's to check against.
func TestFrontendValidateAcceptsBoundaryValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Frontend)
	}{
		{name: "member port 1", mutate: func(f *Frontend) { f.Members[0].Port = 1 }},
		{name: "member port 65535", mutate: func(f *Frontend) { f.Members[0].Port = 65535 }},
		{name: "member number of three digits", mutate: func(f *Frontend) { f.Members[1].Name = "pool-0001-member-100" }},
		{name: "two load balancers on one network", mutate: func(f *Frontend) { f.LoadBalancers[1].Network = "net-0001" }},
		{name: "one server in the pools of two load balancers", mutate: func(f *Frontend) { f.Members[4].Server = "srv-0001" }},
		{name: "members of one pool on different ports", mutate: func(f *Frontend) { f.Members[1].Port = 30005 }},
		{name: "network no other collection names", mutate: func(f *Frontend) { f.LoadBalancers[1].Network = "net-0099" }},
		{name: "pool without members or monitor", mutate: func(f *Frontend) { f.Members = f.Members[:4]; f.HealthMonitors = f.HealthMonitors[:2] }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := wellFormedFrontend()
			tc.mutate(f)
			if err := f.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

// TestFrontendValidateRejectsMalformedFrontends asserts Validate rejects each
// malformed variant with exactly the error naming its offender: an empty or
// duplicate name in every collection, a load balancer without a network, the
// listener, pool, and health monitor faults Plan.Validate reports in the same
// words, and a member of an unknown pool, not named MemberName(<its pool>, n),
// without a server, off the port range, or putting its server into its pool a
// second time.
func TestFrontendValidateRejectsMalformedFrontends(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Frontend)
		wantErr string
	}{
		// Load balancers.
		{name: "empty load balancer name", mutate: func(f *Frontend) { f.LoadBalancers[1].Name = "" }, wantErr: `load balancer at index 1 has an empty name`},
		{name: "duplicate load balancer name", mutate: func(f *Frontend) { f.LoadBalancers[1].Name = "lb-0001" }, wantErr: `duplicate load balancer name "lb-0001"`},
		{name: "load balancer without network", mutate: func(f *Frontend) { f.LoadBalancers[0].Network = "" }, wantErr: `load balancer "lb-0001" has an empty network`},

		// Listeners, with the texts of Plan.Validate.
		{name: "empty listener name", mutate: func(f *Frontend) { f.Listeners[0].Name = "" }, wantErr: `listener at index 0 has an empty name`},
		{name: "duplicate listener name", mutate: func(f *Frontend) { f.Listeners[2].Name = "lsn-0001" }, wantErr: `duplicate listener name "lsn-0001"`},
		{name: "listener references unknown load balancer", mutate: func(f *Frontend) { f.Listeners[2].LoadBalancer = "lb-0009" }, wantErr: `listener "lsn-0003" references unknown load balancer "lb-0009"`},
		{name: "listener port 0", mutate: func(f *Frontend) { f.Listeners[1].Port = 0 }, wantErr: `listener "lsn-0002" has port 0, want 1 to 65535`},
		{name: "duplicate listener port on one load balancer", mutate: func(f *Frontend) { f.Listeners[1].Port = 80 }, wantErr: `listener "lsn-0002" uses port 80 of load balancer "lb-0001", already taken by listener "lsn-0001"`},

		// Pools, with the texts of Plan.Validate.
		{name: "empty pool name", mutate: func(f *Frontend) { f.Pools[2].Name = "" }, wantErr: `pool at index 2 has an empty name`},
		{name: "duplicate pool name", mutate: func(f *Frontend) { f.Pools[1].Name = "pool-0001" }, wantErr: `duplicate pool name "pool-0001"`},
		{name: "pool references unknown listener", mutate: func(f *Frontend) { f.Pools[2].Listener = "lsn-0009" }, wantErr: `pool "pool-0003" references unknown listener "lsn-0009"`},
		{name: "second pool on one listener", mutate: func(f *Frontend) { f.Pools[1].Listener = "lsn-0001" }, wantErr: `pool "pool-0002" is a second pool on listener "lsn-0001", which already has pool "pool-0001"`},

		// Health monitors, with the texts of Plan.Validate.
		{name: "empty health monitor name", mutate: func(f *Frontend) { f.HealthMonitors[1].Name = "" }, wantErr: `health monitor at index 1 has an empty name`},
		{name: "duplicate health monitor name", mutate: func(f *Frontend) { f.HealthMonitors[1].Name = "hm-0001" }, wantErr: `duplicate health monitor name "hm-0001"`},
		{name: "health monitor references unknown pool", mutate: func(f *Frontend) { f.HealthMonitors[2].Pool = "pool-0009" }, wantErr: `health monitor "hm-0003" references unknown pool "pool-0009"`},
		{name: "second health monitor on one pool", mutate: func(f *Frontend) { f.HealthMonitors[1].Pool = "pool-0001" }, wantErr: `health monitor "hm-0002" is a second health monitor on pool "pool-0001", which already has health monitor "hm-0001"`},

		// Member names and pools.
		{name: "empty member name", mutate: func(f *Frontend) { f.Members[0].Name = "" }, wantErr: `member at index 0 has an empty name`},
		{name: "duplicate member name", mutate: func(f *Frontend) { f.Members[1].Name = "pool-0001-member-01" }, wantErr: `duplicate member name "pool-0001-member-01"`},
		{name: "member references unknown pool", mutate: func(f *Frontend) { f.Members[4].Pool = "pool-0009" }, wantErr: `member "pool-0003-member-01" references unknown pool "pool-0009"`},
		{name: "member named for another pool", mutate: func(f *Frontend) { f.Members[4].Pool = "pool-0001" }, wantErr: `member "pool-0003-member-01" of pool "pool-0001" is not named pool-0001-member-<nn>`},
		{name: "member named after its server", mutate: func(f *Frontend) { f.Members[0].Name = "srv-0001" }, wantErr: `member "srv-0001" of pool "pool-0001" is not named pool-0001-member-<nn>`},
		{name: "member number of one digit", mutate: func(f *Frontend) { f.Members[0].Name = "pool-0001-member-1" }, wantErr: `member "pool-0001-member-1" of pool "pool-0001" is not named pool-0001-member-<nn>`},

		// Member servers and ports.
		{name: "member without server", mutate: func(f *Frontend) { f.Members[0].Server = "" }, wantErr: `member "pool-0001-member-01" has an empty server`},
		{name: "member port 0", mutate: func(f *Frontend) { f.Members[0].Port = 0 }, wantErr: `member "pool-0001-member-01" has port 0, want 1 to 65535`},
		{name: "member port 65536", mutate: func(f *Frontend) { f.Members[0].Port = 65536 }, wantErr: `member "pool-0001-member-01" has port 65536, want 1 to 65535`},
		{name: "server twice in one pool", mutate: func(f *Frontend) { f.Members[1].Server = "srv-0001" }, wantErr: `member "pool-0001-member-02" puts server "srv-0001" into pool "pool-0001" a second time, after member "pool-0001-member-01"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := wellFormedFrontend()
			tc.mutate(f)
			err := f.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want %q", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("Validate() = %q, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestFrontendValidateNamesFirstOffender asserts that of several offenders
// Validate names the first, within one collection and across collections in
// the order load balancers, listeners, pools, health monitors, members. The
// health monitors come before the members, unlike in Plan.Validate.
func TestFrontendValidateNamesFirstOffender(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Frontend)
		wantErr string
	}{
		{
			name: "within the members",
			mutate: func(f *Frontend) {
				f.Members[1].Port = 0
				f.Members[3].Server = ""
			},
			wantErr: `member "pool-0001-member-02" has port 0, want 1 to 65535`,
		},
		{
			name: "load balancers before listeners",
			mutate: func(f *Frontend) {
				f.Listeners[0].LoadBalancer = "lb-0009"
				f.LoadBalancers[1].Network = ""
			},
			wantErr: `load balancer "lb-0002" has an empty network`,
		},
		{
			name: "health monitors before members",
			mutate: func(f *Frontend) {
				f.Members[0].Server = ""
				f.HealthMonitors[2].Pool = "pool-0009"
			},
			wantErr: `health monitor "hm-0003" references unknown pool "pool-0009"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := wellFormedFrontend()
			tc.mutate(f)
			if err := f.Validate(); err == nil || err.Error() != tc.wantErr {
				t.Errorf("Validate() = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestFrontendJSONEncoding pins the JSON field names and their order, the
// shape of a mix plan persona's octavia key.
func TestFrontendJSONEncoding(t *testing.T) {
	f := &Frontend{
		Provider:       "ovn",
		LoadBalancers:  []LoadBalancer{{Name: "lb-0001", Network: "net-0001"}},
		Listeners:      []Listener{{Name: "lsn-0001", LoadBalancer: "lb-0001", Port: 80}},
		Pools:          []Pool{{Name: "pool-0001", Listener: "lsn-0001"}},
		Members:        []ServerMember{{Name: "pool-0001-member-01", Pool: "pool-0001", Server: "srv-0001", Port: 30000}},
		HealthMonitors: []HealthMonitor{{Name: "hm-0001", Pool: "pool-0001"}},
	}
	got, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	want := `{"provider":"ovn",` +
		`"loadBalancers":[{"name":"lb-0001","network":"net-0001"}],` +
		`"listeners":[{"name":"lsn-0001","loadBalancer":"lb-0001","port":80}],` +
		`"pools":[{"name":"pool-0001","listener":"lsn-0001"}],` +
		`"members":[{"name":"pool-0001-member-01","pool":"pool-0001","server":"srv-0001","port":30000}],` +
		`"healthMonitors":[{"name":"hm-0001","pool":"pool-0001"}]}`
	if string(got) != want {
		t.Errorf("json.Marshal =\n%s\nwant\n%s", got, want)
	}

	var back Frontend
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("Validate() of the decoded frontend = %v, want nil", err)
	}
}

// TestFrontendJSONEncodingOmitsEmptyProvider asserts a frontend without a
// provider encodes without a provider key, and that empty collections encode
// as [].
func TestFrontendJSONEncodingOmitsEmptyProvider(t *testing.T) {
	f := &Frontend{
		LoadBalancers:  []LoadBalancer{},
		Listeners:      []Listener{},
		Pools:          []Pool{},
		Members:        []ServerMember{},
		HealthMonitors: []HealthMonitor{},
	}
	got, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	want := `{"loadBalancers":[],"listeners":[],"pools":[],"members":[],"healthMonitors":[]}`
	if string(got) != want {
		t.Errorf("json.Marshal =\n%s\nwant\n%s", got, want)
	}
}
