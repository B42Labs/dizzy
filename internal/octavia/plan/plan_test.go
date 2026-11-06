package plan

import (
	"encoding/json"
	"testing"
)

// wellFormed returns a small, internally consistent plan the error-case tests
// mutate into invalidity. Two load balancers share net-0001 and a third sits on
// net-0002; lb-0001 and lb-0002 both listen on port 80, which is valid because
// the ports differ only per load balancer.
func wellFormed() *Plan {
	return &Plan{
		Scenario: "test",
		Seed:     42,
		Provider: "ovn",
		Networks: []Network{
			{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"},
			{Name: "net-0002", Subnet: "sub-0002", CIDR: "10.0.2.0/24"},
		},
		LoadBalancers: []LoadBalancer{
			{Name: "lb-0001", Network: "net-0001"},
			{Name: "lb-0002", Network: "net-0001"},
			{Name: "lb-0003", Network: "net-0002"},
		},
		Listeners: []Listener{
			{Name: "lsn-0001", LoadBalancer: "lb-0001", Port: 80},
			{Name: "lsn-0002", LoadBalancer: "lb-0002", Port: 80},
			{Name: "lsn-0003", LoadBalancer: "lb-0001", Port: 443},
			{Name: "lsn-0004", LoadBalancer: "lb-0003", Port: 65535},
		},
		Pools: []Pool{
			{Name: "pool-0001", Listener: "lsn-0001"},
			{Name: "pool-0002", Listener: "lsn-0002"},
			{Name: "pool-0003", Listener: "lsn-0003"},
			{Name: "pool-0004", Listener: "lsn-0004"},
		},
		Members: []Member{
			{Name: "pool-0001-member-01", Pool: "pool-0001", Address: "10.0.1.128", Port: 8080},
			{Name: "pool-0002-member-01", Pool: "pool-0002", Address: "10.0.1.129", Port: 1},
			{Name: "pool-0003-member-01", Pool: "pool-0003", Address: "10.0.1.254", Port: 65535},
			{Name: "pool-0004-member-01", Pool: "pool-0004", Address: "10.0.2.128", Port: 8080},
			{Name: "pool-0001-member-02", Pool: "pool-0001", Address: "10.0.1.130", Port: 8081},
		},
		HealthMonitors: []HealthMonitor{
			{Name: "hm-0001", Pool: "pool-0001"},
			{Name: "hm-0002", Pool: "pool-0003"},
		},
	}
}

func TestValidateAcceptsWellFormedPlan(t *testing.T) {
	if err := wellFormed().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// TestValidateAcceptsEmptyCollections asserts a plan without any resource is
// valid, whether its slices are nil or empty, with or without a provider.
func TestValidateAcceptsEmptyCollections(t *testing.T) {
	for name, p := range map[string]*Plan{
		"nil":           {Scenario: "t", Seed: 1},
		"empty":         {Scenario: "t", Seed: 1, Networks: []Network{}, LoadBalancers: []LoadBalancer{}, Listeners: []Listener{}, Pools: []Pool{}, Members: []Member{}, HealthMonitors: []HealthMonitor{}},
		"provider only": {Scenario: "t", Seed: 1, Provider: "amphora"},
		"networks only": {Scenario: "t", Seed: 1, Networks: []Network{{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"}}},
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("%s collections: Validate() = %v, want nil", name, err)
		}
	}
}

// TestValidateAcceptsBoundaryValues asserts the edges of every range Validate
// enforces are inside it, and that what is unique only per load balancer or
// per network may repeat across them.
func TestValidateAcceptsBoundaryValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Plan)
	}{
		{name: "listener port 1", mutate: func(p *Plan) { p.Listeners[0].Port = 1 }},
		{name: "listener port 65535", mutate: func(p *Plan) { p.Listeners[0].Port = 65535 }},
		{name: "member port 1", mutate: func(p *Plan) { p.Members[0].Port = 1 }},
		{name: "member port 65535", mutate: func(p *Plan) { p.Members[0].Port = 65535 }},
		{name: "member address .128", mutate: func(p *Plan) { p.Members[3].Address = "10.0.2.128" }},
		{name: "member address .254", mutate: func(p *Plan) { p.Members[3].Address = "10.0.2.254" }},
		{name: "member number of three digits", mutate: func(p *Plan) { p.Members[4].Name = "pool-0001-member-100" }},
		{name: "same listener port on two load balancers", mutate: func(p *Plan) { p.Listeners[3].Port = 80 }},
		{
			// net-0002 overlaps net-0001, so lb-0003's member may reuse the
			// address of a member on net-0001: it is a different network.
			name: "same address on two networks",
			mutate: func(p *Plan) {
				p.Networks[1].CIDR = "10.0.1.0/24"
				p.Members[3].Address = "10.0.1.128"
			},
		},
		{name: "pool without members or monitor", mutate: func(p *Plan) { p.Members = p.Members[:3]; p.HealthMonitors = nil }},
		{name: "pool name containing the member separator", mutate: func(p *Plan) {
			p.Pools[3].Name = "pool-member-0004"
			p.Members[3] = Member{Name: "pool-member-0004-member-01", Pool: "pool-member-0004", Address: "10.0.2.128", Port: 8080}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := wellFormed()
			tc.mutate(p)
			if err := p.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

// TestValidateRejectsMalformedPlans asserts Validate rejects each malformed
// variant with exactly the error naming its offender: an empty or duplicate
// name in every collection, a CIDR that is no masked IPv4 /24, a dangling
// reference of every kind, a port out of range, a duplicate listener port on
// one load balancer, a second pool on one listener, a second monitor on one
// pool, a member named for another pool or not by MemberName, and a member
// address off the upper half of its network or used twice there.
func TestValidateRejectsMalformedPlans(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Plan)
		wantErr string
	}{
		// Empty and duplicate names, per collection.
		{name: "empty network name", mutate: func(p *Plan) { p.Networks[1].Name = "" }, wantErr: `network at index 1 has an empty name`},
		{name: "duplicate network name", mutate: func(p *Plan) { p.Networks[1].Name = "net-0001" }, wantErr: `duplicate network name "net-0001"`},
		{name: "empty subnet name", mutate: func(p *Plan) { p.Networks[1].Subnet = "" }, wantErr: `network "net-0002" has an empty subnet name`},
		{name: "duplicate subnet name", mutate: func(p *Plan) { p.Networks[1].Subnet = "sub-0001" }, wantErr: `duplicate subnet name "sub-0001"`},
		{name: "empty load balancer name", mutate: func(p *Plan) { p.LoadBalancers[2].Name = "" }, wantErr: `load balancer at index 2 has an empty name`},
		{name: "duplicate load balancer name", mutate: func(p *Plan) { p.LoadBalancers[1].Name = "lb-0001" }, wantErr: `duplicate load balancer name "lb-0001"`},
		{name: "empty listener name", mutate: func(p *Plan) { p.Listeners[0].Name = "" }, wantErr: `listener at index 0 has an empty name`},
		{name: "duplicate listener name", mutate: func(p *Plan) { p.Listeners[3].Name = "lsn-0002" }, wantErr: `duplicate listener name "lsn-0002"`},
		{name: "empty pool name", mutate: func(p *Plan) { p.Pools[3].Name = "" }, wantErr: `pool at index 3 has an empty name`},
		{name: "duplicate pool name", mutate: func(p *Plan) { p.Pools[1].Name = "pool-0001" }, wantErr: `duplicate pool name "pool-0001"`},
		{name: "empty member name", mutate: func(p *Plan) { p.Members[4].Name = "" }, wantErr: `member at index 4 has an empty name`},
		{name: "duplicate member name", mutate: func(p *Plan) { p.Members[4].Name = "pool-0001-member-01" }, wantErr: `duplicate member name "pool-0001-member-01"`},
		{name: "empty health monitor name", mutate: func(p *Plan) { p.HealthMonitors[1].Name = "" }, wantErr: `health monitor at index 1 has an empty name`},
		{name: "duplicate health monitor name", mutate: func(p *Plan) { p.HealthMonitors[1].Name = "hm-0001" }, wantErr: `duplicate health monitor name "hm-0001"`},

		// CIDRs.
		{name: "CIDR that does not parse", mutate: func(p *Plan) { p.Networks[1].CIDR = "10.0.2.0" }, wantErr: `network "net-0002" has CIDR "10.0.2.0", want a masked IPv4 /24 such as 10.0.1.0/24`},
		{name: "empty CIDR", mutate: func(p *Plan) { p.Networks[1].CIDR = "" }, wantErr: `network "net-0002" has CIDR "", want a masked IPv4 /24 such as 10.0.1.0/24`},
		{name: "IPv4 CIDR wider than /24", mutate: func(p *Plan) { p.Networks[1].CIDR = "10.0.0.0/16" }, wantErr: `network "net-0002" has CIDR "10.0.0.0/16", want a masked IPv4 /24 such as 10.0.1.0/24`},
		{name: "IPv4 CIDR narrower than /24", mutate: func(p *Plan) { p.Networks[1].CIDR = "10.0.2.0/25" }, wantErr: `network "net-0002" has CIDR "10.0.2.0/25", want a masked IPv4 /24 such as 10.0.1.0/24`},
		{name: "unmasked IPv4 /24", mutate: func(p *Plan) { p.Networks[1].CIDR = "10.0.2.5/24" }, wantErr: `network "net-0002" has CIDR "10.0.2.5/24", want a masked IPv4 /24 such as 10.0.1.0/24`},
		{name: "IPv6 CIDR", mutate: func(p *Plan) { p.Networks[1].CIDR = "fd00::/24" }, wantErr: `network "net-0002" has CIDR "fd00::/24", want a masked IPv4 /24 such as 10.0.1.0/24`},

		// Dangling references, one per kind.
		{name: "load balancer references unknown network", mutate: func(p *Plan) { p.LoadBalancers[2].Network = "net-0009" }, wantErr: `load balancer "lb-0003" references unknown network "net-0009"`},
		{name: "load balancer references no network", mutate: func(p *Plan) { p.LoadBalancers[2].Network = "" }, wantErr: `load balancer "lb-0003" references unknown network ""`},
		{name: "listener references unknown load balancer", mutate: func(p *Plan) { p.Listeners[3].LoadBalancer = "lb-0009" }, wantErr: `listener "lsn-0004" references unknown load balancer "lb-0009"`},
		{name: "pool references unknown listener", mutate: func(p *Plan) { p.Pools[3].Listener = "lsn-0009" }, wantErr: `pool "pool-0004" references unknown listener "lsn-0009"`},
		{name: "member references unknown pool", mutate: func(p *Plan) { p.Members[3].Pool = "pool-0009" }, wantErr: `member "pool-0004-member-01" references unknown pool "pool-0009"`},
		{name: "health monitor references unknown pool", mutate: func(p *Plan) { p.HealthMonitors[1].Pool = "pool-0009" }, wantErr: `health monitor "hm-0002" references unknown pool "pool-0009"`},

		// Ports.
		{name: "listener port 0", mutate: func(p *Plan) { p.Listeners[1].Port = 0 }, wantErr: `listener "lsn-0002" has port 0, want 1 to 65535`},
		{name: "listener port 65536", mutate: func(p *Plan) { p.Listeners[1].Port = 65536 }, wantErr: `listener "lsn-0002" has port 65536, want 1 to 65535`},
		{name: "member port 0", mutate: func(p *Plan) { p.Members[1].Port = 0 }, wantErr: `member "pool-0002-member-01" has port 0, want 1 to 65535`},
		{name: "member port 65536", mutate: func(p *Plan) { p.Members[1].Port = 65536 }, wantErr: `member "pool-0002-member-01" has port 65536, want 1 to 65535`},
		{name: "duplicate listener port on one load balancer", mutate: func(p *Plan) { p.Listeners[2].Port = 80 }, wantErr: `listener "lsn-0003" uses port 80 of load balancer "lb-0001", already taken by listener "lsn-0001"`},

		// One pool per listener, one monitor per pool.
		{name: "second pool on one listener", mutate: func(p *Plan) { p.Pools[2].Listener = "lsn-0001" }, wantErr: `pool "pool-0003" is a second pool on listener "lsn-0001", which already has pool "pool-0001"`},
		{name: "second monitor on one pool", mutate: func(p *Plan) { p.HealthMonitors[1].Pool = "pool-0001" }, wantErr: `health monitor "hm-0002" is a second health monitor on pool "pool-0001", which already has health monitor "hm-0001"`},

		// Member names.
		{name: "member named for another pool", mutate: func(p *Plan) { p.Members[1].Pool = "pool-0001" }, wantErr: `member "pool-0002-member-01" is named for pool "pool-0002", not its pool "pool-0001"`},
		{name: "member name without member separator", mutate: func(p *Plan) { p.Members[1].Name = "mem-0001" }, wantErr: `member "mem-0001" of pool "pool-0002" is not named pool-0002-member-<nn>`},
		{name: "member name with nothing before the separator", mutate: func(p *Plan) { p.Members[1].Name = "-member-01" }, wantErr: `member "-member-01" of pool "pool-0002" is not named pool-0002-member-<nn>`},
		{name: "member number of one digit", mutate: func(p *Plan) { p.Members[1].Name = "pool-0002-member-1" }, wantErr: `member "pool-0002-member-1" of pool "pool-0002" is not named pool-0002-member-<nn>`},
		{name: "member number with a leading zero too many", mutate: func(p *Plan) { p.Members[1].Name = "pool-0002-member-001" }, wantErr: `member "pool-0002-member-001" of pool "pool-0002" is not named pool-0002-member-<nn>`},
		{name: "member number zero", mutate: func(p *Plan) { p.Members[1].Name = "pool-0002-member-00" }, wantErr: `member "pool-0002-member-00" of pool "pool-0002" is not named pool-0002-member-<nn>`},
		{name: "member number not a number", mutate: func(p *Plan) { p.Members[1].Name = "pool-0002-member-ab" }, wantErr: `member "pool-0002-member-ab" of pool "pool-0002" is not named pool-0002-member-<nn>`},
		{name: "member name without number", mutate: func(p *Plan) { p.Members[1].Name = "pool-0002-member-" }, wantErr: `member "pool-0002-member-" of pool "pool-0002" is not named pool-0002-member-<nn>`},

		// Member addresses.
		{name: "member address below .128", mutate: func(p *Plan) { p.Members[0].Address = "10.0.1.127" }, wantErr: `member "pool-0001-member-01" has address 10.0.1.127, want .128 to .254 of network "net-0001" (10.0.1.0/24)`},
		{name: "member address on the gateway", mutate: func(p *Plan) { p.Members[0].Address = "10.0.1.1" }, wantErr: `member "pool-0001-member-01" has address 10.0.1.1, want .128 to .254 of network "net-0001" (10.0.1.0/24)`},
		{name: "member address on the broadcast address", mutate: func(p *Plan) { p.Members[0].Address = "10.0.1.255" }, wantErr: `member "pool-0001-member-01" has address 10.0.1.255, want .128 to .254 of network "net-0001" (10.0.1.0/24)`},
		{
			// pool-0004 reaches net-0002 through lsn-0004 and lb-0003, so an
			// address in the upper half of net-0001 is off its network.
			name:    "member address on another network",
			mutate:  func(p *Plan) { p.Members[3].Address = "10.0.1.200" },
			wantErr: `member "pool-0004-member-01" has address 10.0.1.200, want .128 to .254 of network "net-0002" (10.0.2.0/24)`,
		},
		{name: "member address that does not parse", mutate: func(p *Plan) { p.Members[0].Address = "10.0.1" }, wantErr: `member "pool-0001-member-01" has address "10.0.1", want an IPv4 address`},
		{name: "empty member address", mutate: func(p *Plan) { p.Members[0].Address = "" }, wantErr: `member "pool-0001-member-01" has address "", want an IPv4 address`},
		{name: "IPv6 member address", mutate: func(p *Plan) { p.Members[0].Address = "fd00::80" }, wantErr: `member "pool-0001-member-01" has address "fd00::80", want an IPv4 address`},
		{name: "IPv4-mapped IPv6 member address", mutate: func(p *Plan) { p.Members[0].Address = "::ffff:10.0.1.128" }, wantErr: `member "pool-0001-member-01" has address "::ffff:10.0.1.128", want an IPv4 address`},
		{
			// pool-0001 and pool-0002 sit on different load balancers of the
			// same network, so their members share its address space.
			name:    "member address used twice on one network",
			mutate:  func(p *Plan) { p.Members[1].Address = "10.0.1.128" },
			wantErr: `member "pool-0002-member-01" has address 10.0.1.128 on network "net-0001", already used by member "pool-0001-member-01"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := wellFormed()
			tc.mutate(p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want %q", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("Validate() = %q, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateNamesFirstOffender asserts that of several offenders Validate
// names the first, both within one collection and across collections, where
// the networks come before everything else.
func TestValidateNamesFirstOffender(t *testing.T) {
	t.Run("within a collection", func(t *testing.T) {
		p := wellFormed()
		p.Listeners[1].LoadBalancer = "lb-0008"
		p.Listeners[3].LoadBalancer = "lb-0009"
		want := `listener "lsn-0002" references unknown load balancer "lb-0008"`
		if err := p.Validate(); err == nil || err.Error() != want {
			t.Errorf("Validate() = %v, want %q", err, want)
		}
	})
	t.Run("across collections", func(t *testing.T) {
		p := wellFormed()
		p.HealthMonitors[0].Pool = "pool-0009"
		p.Members[0].Address = "10.0.1.1"
		p.Networks[1].CIDR = "10.0.2.0/23"
		want := `network "net-0002" has CIDR "10.0.2.0/23", want a masked IPv4 /24 such as 10.0.1.0/24`
		if err := p.Validate(); err == nil || err.Error() != want {
			t.Errorf("Validate() = %v, want %q", err, want)
		}
	})
}

func TestMemberName(t *testing.T) {
	for _, tc := range []struct {
		pool string
		n    int
		want string
	}{
		{"pool-0001", 1, "pool-0001-member-01"},
		{"pool-0001", 9, "pool-0001-member-09"},
		{"pool-0030", 12, "pool-0030-member-12"},
		{"pool-0002", 100, "pool-0002-member-100"},
	} {
		if got := MemberName(tc.pool, tc.n); got != tc.want {
			t.Errorf("MemberName(%q, %d) = %q, want %q", tc.pool, tc.n, got, tc.want)
		}
	}
}

// TestMemberPool asserts MemberPool returns the part before the last
// "-member-" and false when there is none or it is empty.
func TestMemberPool(t *testing.T) {
	for _, tc := range []struct {
		member   string
		wantPool string
		wantOK   bool
	}{
		{"pool-0001-member-01", "pool-0001", true},
		{"pool-0030-member-120", "pool-0030", true},
		{"pool-member-0004-member-01", "pool-member-0004", true},
		{"x-member-", "x", true},
		{"pool-0001", "", false},
		{"pool-0001-member01", "", false},
		{"-member-01", "", false},
		{"", "", false},
	} {
		pool, ok := MemberPool(tc.member)
		if pool != tc.wantPool || ok != tc.wantOK {
			t.Errorf("MemberPool(%q) = (%q, %v), want (%q, %v)", tc.member, pool, ok, tc.wantPool, tc.wantOK)
		}
	}
}

// TestMemberPoolInvertsMemberName asserts every member name MemberName builds
// returns its pool through MemberPool, the property status relies on to find a
// recorded member's pool.
func TestMemberPoolInvertsMemberName(t *testing.T) {
	for _, pool := range []string{"pool-0001", "pool-0030", "pool-member-0004"} {
		for n := 1; n <= 120; n++ {
			name := MemberName(pool, n)
			if got, ok := MemberPool(name); !ok || got != pool {
				t.Fatalf("MemberPool(%q) = (%q, %v), want (%q, true)", name, got, ok, pool)
			}
		}
	}
}

// TestSummary asserts Summary renders the header and the seven aligned lines
// exactly, the bytes "octavia apply --dry-run" prints, and renders the same
// bytes twice for one plan.
func TestSummary(t *testing.T) {
	p := wellFormed()
	want := `Plan for scenario "test" (seed 42)
  provider:         ovn
  networks:         2
  load balancers:   3
  listeners:        4
  pools:            4
  members:          5
  health monitors:  2
`
	first, second := p.Summary(), p.Summary()
	if first != want {
		t.Errorf("Summary() =\n%s\nwant\n%s", first, want)
	}
	if first != second {
		t.Errorf("Summary() not deterministic:\n%s\n---\n%s", first, second)
	}
}

// TestSummaryNamesCloudDefaultProvider asserts a plan without a provider reads
// "cloud default" on the provider line, also when it has no resources at all.
func TestSummaryNamesCloudDefaultProvider(t *testing.T) {
	p := &Plan{Scenario: "empty", Seed: 7}
	want := `Plan for scenario "empty" (seed 7)
  provider:         cloud default
  networks:         0
  load balancers:   0
  listeners:        0
  pools:            0
  members:          0
  health monitors:  0
`
	if got := p.Summary(); got != want {
		t.Errorf("Summary() =\n%s\nwant\n%s", got, want)
	}
}

// TestJSONEncoding pins the JSON field names and their order, the shape of the
// plan file and of the golden plan.
func TestJSONEncoding(t *testing.T) {
	p := &Plan{
		Scenario:       "t",
		Seed:           1,
		Provider:       "ovn",
		Networks:       []Network{{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"}},
		LoadBalancers:  []LoadBalancer{{Name: "lb-0001", Network: "net-0001"}},
		Listeners:      []Listener{{Name: "lsn-0001", LoadBalancer: "lb-0001", Port: 80}},
		Pools:          []Pool{{Name: "pool-0001", Listener: "lsn-0001"}},
		Members:        []Member{{Name: "pool-0001-member-01", Pool: "pool-0001", Address: "10.0.1.128", Port: 8080}},
		HealthMonitors: []HealthMonitor{{Name: "hm-0001", Pool: "pool-0001"}},
	}
	got, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	want := `{"scenario":"t","seed":1,"provider":"ovn",` +
		`"networks":[{"name":"net-0001","subnet":"sub-0001","cidr":"10.0.1.0/24"}],` +
		`"loadBalancers":[{"name":"lb-0001","network":"net-0001"}],` +
		`"listeners":[{"name":"lsn-0001","loadBalancer":"lb-0001","port":80}],` +
		`"pools":[{"name":"pool-0001","listener":"lsn-0001"}],` +
		`"members":[{"name":"pool-0001-member-01","pool":"pool-0001","address":"10.0.1.128","port":8080}],` +
		`"healthMonitors":[{"name":"hm-0001","pool":"pool-0001"}]}`
	if string(got) != want {
		t.Errorf("json.Marshal =\n%s\nwant\n%s", got, want)
	}

	var back Plan
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("Validate() of the decoded plan = %v, want nil", err)
	}
}

// TestJSONEncodingOmitsEmptyProvider asserts a plan without a provider encodes
// without a provider key, and that empty collections encode as [].
func TestJSONEncodingOmitsEmptyProvider(t *testing.T) {
	p := &Plan{
		Scenario:       "t",
		Seed:           1,
		Networks:       []Network{},
		LoadBalancers:  []LoadBalancer{},
		Listeners:      []Listener{},
		Pools:          []Pool{},
		Members:        []Member{},
		HealthMonitors: []HealthMonitor{},
	}
	got, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	want := `{"scenario":"t","seed":1,"networks":[],"loadBalancers":[],"listeners":[],"pools":[],"members":[],"healthMonitors":[]}`
	if string(got) != want {
		t.Errorf("json.Marshal =\n%s\nwant\n%s", got, want)
	}
}
