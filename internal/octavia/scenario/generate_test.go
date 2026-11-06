package scenario

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/B42Labs/dizzy/internal/octavia/plan"
)

var update = flag.Bool("update", false, "update golden files")

// goldenPath is the golden plan of the small profile, regenerated only with
// go test ./internal/octavia/scenario/ -update.
var goldenPath = filepath.Join("testdata", "golden", "small.plan.json")

// marshal renders a plan exactly as the generate command does: indented JSON
// with a trailing newline.
func marshal(t *testing.T, p *plan.Plan) []byte {
	t.Helper()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("marshalling plan: %v", err)
	}
	return append(data, '\n')
}

// generate expands s, failing the test on an error.
func generate(t *testing.T, s Scenario) *plan.Plan {
	t.Helper()
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}
	return p
}

func TestGenerateInvalidScenario(t *testing.T) {
	s := smallScenario()
	s.Name = "" // fails Scenario.Validate

	_, err := s.Generate()
	if want := "invalid scenario: name must not be empty"; err == nil || err.Error() != want {
		t.Fatalf("Generate() = %v, want %q", err, want)
	}
}

func TestGenerateDeterministic(t *testing.T) {
	s := smallScenario()
	if first, second := marshal(t, generate(t, s)), marshal(t, generate(t, s)); !bytes.Equal(first, second) {
		t.Errorf("two generations of the same scenario+seed differ:\n%s\n---\n%s", first, second)
	}
}

func TestGenerateGolden(t *testing.T) {
	got := marshal(t, generate(t, smallScenario()))

	if *update {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden file (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("generated plan differs from golden file %s; run with -update if the change is intended", goldenPath)
	}
}

// TestGenerateSeedChangesPortsOnly confirms that overriding the seed (as
// --seed 7 does) changes listener and member ports but no name, no assignment
// of a load balancer to a network, a listener to a load balancer, a pool to a
// listener, or a member to a pool, and no member address: the assignment draws
// nothing. The monitored pool subset is drawn too, so only the monitor names
// and count are compared.
func TestGenerateSeedChangesPortsOnly(t *testing.T) {
	base := generate(t, smallScenario())
	reseeded := smallScenario()
	if err := reseeded.Set("seed", "7"); err != nil {
		t.Fatalf(`Set("seed", "7") = %v, want nil`, err)
	}
	other := generate(t, reseeded)

	if !slices.Equal(base.Networks, other.Networks) {
		t.Errorf("networks differ across seeds: %+v vs %+v", base.Networks, other.Networks)
	}
	if !slices.Equal(base.LoadBalancers, other.LoadBalancers) {
		t.Errorf("load balancers differ across seeds: %+v vs %+v", base.LoadBalancers, other.LoadBalancers)
	}
	if !slices.Equal(base.Pools, other.Pools) {
		t.Errorf("pools differ across seeds: %+v vs %+v", base.Pools, other.Pools)
	}

	// The shapes strip the drawn port, leaving name, assignment, and address.
	type listenerShape struct{ Name, LoadBalancer string }
	type memberShape struct{ Name, Pool, Address string }
	listenerShapes := func(p *plan.Plan) []listenerShape {
		out := make([]listenerShape, 0, len(p.Listeners))
		for _, l := range p.Listeners {
			out = append(out, listenerShape{l.Name, l.LoadBalancer})
		}
		return out
	}
	memberShapes := func(p *plan.Plan) []memberShape {
		out := make([]memberShape, 0, len(p.Members))
		for _, m := range p.Members {
			out = append(out, memberShape{m.Name, m.Pool, m.Address})
		}
		return out
	}
	if got, want := listenerShapes(other), listenerShapes(base); !slices.Equal(got, want) {
		t.Errorf("listener names or load balancers differ across seeds: %+v vs %+v", got, want)
	}
	if got, want := memberShapes(other), memberShapes(base); !slices.Equal(got, want) {
		t.Errorf("member names, pools, or addresses differ across seeds: %+v vs %+v", got, want)
	}

	monitorNames := func(p *plan.Plan) []string {
		out := make([]string, 0, len(p.HealthMonitors))
		for _, h := range p.HealthMonitors {
			out = append(out, h.Name)
		}
		return out
	}
	if got, want := monitorNames(other), monitorNames(base); !slices.Equal(got, want) {
		t.Errorf("health monitor names differ across seeds: %v vs %v", got, want)
	}

	listenerPorts := func(p *plan.Plan) []int {
		out := make([]int, 0, len(p.Listeners))
		for _, l := range p.Listeners {
			out = append(out, l.Port)
		}
		return out
	}
	memberPorts := func(p *plan.Plan) []int {
		out := make([]int, 0, len(p.Members))
		for _, m := range p.Members {
			out = append(out, m.Port)
		}
		return out
	}
	if a, b := listenerPorts(base), listenerPorts(other); slices.Equal(a, b) {
		t.Errorf("listener ports %v are the same for seeds 42 and 7, want them redrawn", a)
	}
	if a, b := memberPorts(base), memberPorts(other); slices.Equal(a, b) {
		t.Errorf("member ports %v are the same for seeds 42 and 7, want them redrawn", a)
	}
}

// TestGenerateSmallAssignment pins the round-robin assignment of the small
// plan: its one network carries both load balancers, the four listeners
// alternate between them, and pool i is the default pool of listener i.
func TestGenerateSmallAssignment(t *testing.T) {
	p := generate(t, smallScenario())

	wantNetworks := []plan.Network{{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"}}
	if !slices.Equal(p.Networks, wantNetworks) {
		t.Errorf("networks = %+v, want %+v", p.Networks, wantNetworks)
	}
	wantLBs := []plan.LoadBalancer{{Name: "lb-0001", Network: "net-0001"}, {Name: "lb-0002", Network: "net-0001"}}
	if !slices.Equal(p.LoadBalancers, wantLBs) {
		t.Errorf("load balancers = %+v, want %+v", p.LoadBalancers, wantLBs)
	}

	wantListenerLB := []struct{ listener, lb string }{
		{"lsn-0001", "lb-0001"},
		{"lsn-0002", "lb-0002"},
		{"lsn-0003", "lb-0001"},
		{"lsn-0004", "lb-0002"},
	}
	if len(p.Listeners) != len(wantListenerLB) {
		t.Fatalf("listeners = %d, want %d", len(p.Listeners), len(wantListenerLB))
	}
	for i, want := range wantListenerLB {
		if l := p.Listeners[i]; l.Name != want.listener || l.LoadBalancer != want.lb {
			t.Errorf("listener %d = %s on %s, want %s on %s", i, l.Name, l.LoadBalancer, want.listener, want.lb)
		}
	}

	if len(p.Pools) != 4 {
		t.Fatalf("pools = %d, want 4", len(p.Pools))
	}
	for i, pl := range p.Pools {
		want := plan.Pool{Name: fmt.Sprintf("pool-%04d", i+1), Listener: fmt.Sprintf("lsn-%04d", i+1)}
		if pl != want {
			t.Errorf("pool %d = %+v, want %+v", i, pl, want)
		}
	}
}

// memberNamePattern is the shape of every generated member's logical name.
var memberNamePattern = regexp.MustCompile(`^pool-\d{4}-member-\d{2}$`)

// TestGenerateMemberNamesAndAddresses confirms every member of the small plan
// is named pool-<nnnn>-member-<nn>, numbered from 1 within its pool, belongs to
// pool m % pools, recovers its pool through MemberPool, and takes the next
// address of net-0001 counting up from 10.0.1.128 without a repeat.
func TestGenerateMemberNamesAndAddresses(t *testing.T) {
	p := generate(t, smallScenario())
	if len(p.Members) != 8 {
		t.Fatalf("members = %d, want 8", len(p.Members))
	}

	seen := make(map[string]string, len(p.Members))
	for m, mem := range p.Members {
		if !memberNamePattern.MatchString(mem.Name) {
			t.Errorf("member %d name %q, want pool-<nnnn>-member-<nn>", m, mem.Name)
		}
		wantPool := p.Pools[m%len(p.Pools)].Name
		if mem.Pool != wantPool {
			t.Errorf("member %q pool = %q, want %q", mem.Name, mem.Pool, wantPool)
		}
		if want := plan.MemberName(wantPool, m/len(p.Pools)+1); mem.Name != want {
			t.Errorf("member %d name = %q, want %q", m, mem.Name, want)
		}
		if pool, ok := plan.MemberPool(mem.Name); !ok || pool != mem.Pool {
			t.Errorf("MemberPool(%q) = %q, %v, want %q, true", mem.Name, pool, ok, mem.Pool)
		}
		if want := fmt.Sprintf("10.0.1.%d", 128+m); mem.Address != want {
			t.Errorf("member %q address = %q, want %q", mem.Name, mem.Address, want)
		}
		if other, ok := seen[mem.Address]; ok {
			t.Errorf("members %q and %q share address %s", other, mem.Name, mem.Address)
		}
		seen[mem.Address] = mem.Name
	}
}

// TestGenerateMemberAddressesPerNetwork confirms each VIP network hands out
// its own member addresses in member order, counting up from .128 of its /24
// without a gap or a repeat, and that 127 members fill one network from .128
// to .254.
func TestGenerateMemberAddressesPerNetwork(t *testing.T) {
	spread := smallScenario()
	spread.Resources = Resources{Networks: 3, LoadBalancers: 5, Listeners: 10, Members: 47, HealthMonitors: 3}

	full := smallScenario()
	full.Resources = Resources{Networks: 1, LoadBalancers: 1, Listeners: 3, Members: 127}

	for name, s := range map[string]Scenario{"spread": spread, "full": full} {
		t.Run(name, func(t *testing.T) {
			p := generate(t, s)
			cidr := make(map[string]netip.Prefix, len(p.Networks))
			for _, n := range p.Networks {
				cidr[n.Name] = netip.MustParsePrefix(n.CIDR)
			}
			lbNetwork := make(map[string]string, len(p.LoadBalancers))
			for _, lb := range p.LoadBalancers {
				lbNetwork[lb.Name] = lb.Network
			}
			listenerLB := make(map[string]string, len(p.Listeners))
			for _, l := range p.Listeners {
				listenerLB[l.Name] = l.LoadBalancer
			}
			poolListener := make(map[string]string, len(p.Pools))
			for _, pl := range p.Pools {
				poolListener[pl.Name] = pl.Listener
			}

			next := make(map[string]int, len(p.Networks)) // network to the next expected octet offset
			for _, mem := range p.Members {
				network := lbNetwork[listenerLB[poolListener[mem.Pool]]]
				want := cidr[network].Addr().As4()
				want[3] = byte(128 + next[network])
				next[network]++
				if got := netip.AddrFrom4(want).String(); mem.Address != got {
					t.Errorf("member %q on %s address = %s, want %s", mem.Name, network, mem.Address, got)
				}
			}
			if name == "full" {
				if last := p.Members[len(p.Members)-1].Address; last != "10.0.1.254" {
					t.Errorf("127th member address = %s, want 10.0.1.254", last)
				}
			}
		})
	}
}

// TestGenerateHealthMonitors confirms the health monitors cover exactly
// health_monitors distinct pools, in ascending pool order, named hm-0001 on,
// for none, some, and every pool.
func TestGenerateHealthMonitors(t *testing.T) {
	none := smallScenario()
	none.Resources.HealthMonitors = 0

	every := smallScenario()
	every.Resources.HealthMonitors = every.Resources.Listeners

	wide := smallScenario()
	wide.Resources = Resources{Networks: 4, LoadBalancers: 12, Listeners: 60, Members: 120, HealthMonitors: 25}

	for name, s := range map[string]Scenario{"small": smallScenario(), "none": none, "every": every, "wide": wide} {
		t.Run(name, func(t *testing.T) {
			p := generate(t, s)
			if got, want := len(p.HealthMonitors), s.Resources.HealthMonitors; got != want {
				t.Fatalf("health monitors = %d, want %d", got, want)
			}
			index := make(map[string]int, len(p.Pools))
			for i, pl := range p.Pools {
				index[pl.Name] = i
			}
			prev := -1
			for i, h := range p.HealthMonitors {
				if want := fmt.Sprintf("hm-%04d", i+1); h.Name != want {
					t.Errorf("health monitor %d name = %q, want %q", i, h.Name, want)
				}
				j, ok := index[h.Pool]
				if !ok {
					t.Errorf("health monitor %q is on %q, which is not a pool", h.Name, h.Pool)
					continue
				}
				if j <= prev {
					t.Errorf("health monitor %q is on pool index %d after %d, want distinct pools in ascending order", h.Name, j, prev)
				}
				prev = j
			}
		})
	}
}

// TestGenerateListenerPortCollision confirms a three-port listener range with
// three listeners on one load balancer yields three distinct ports even when
// the second draw collides with the first: the colliding port steps up to the
// next free one, wrapping from max to min. Each case first proves from a fresh
// source with the same seed that the raw draws do collide, so the test cannot
// pass vacuously.
func TestGenerateListenerPortCollision(t *testing.T) {
	ports := Range{Min: 1024, Max: 1026}
	tests := []struct {
		name string
		seed int64
		// draws are the raw offsets the seed's first three Intn(3) calls return.
		draws []int
		want  []int
	}{
		{
			// 1024, then 1024 again steps up to 1025, then 1025 steps up to 1026.
			name:  "steps up",
			seed:  0,
			draws: []int{0, 0, 1},
			want:  []int{1024, 1025, 1026},
		},
		{
			// 1026, then 1026 again wraps to 1024, then 1026 wraps past the taken
			// 1024 to 1025.
			name:  "wraps from max to min",
			seed:  42,
			draws: []int{2, 2, 2},
			want:  []int{1026, 1024, 1025},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := rand.New(rand.NewSource(tc.seed))
			draws := []int{src.Intn(3), src.Intn(3), src.Intn(3)}
			if !slices.Equal(draws, tc.draws) {
				t.Fatalf("seed %d draws %v, want %v", tc.seed, draws, tc.draws)
			}
			if draws[1] != draws[0] {
				t.Fatalf("seed %d: second draw %d does not collide with the first %d", tc.seed, draws[1], draws[0])
			}

			s := smallScenario()
			s.Seed = tc.seed
			s.Resources = Resources{Networks: 1, LoadBalancers: 1, Listeners: 3, Members: 3}
			s.Distribution.ListenerPort = ports

			p := generate(t, s)
			got := make([]int, 0, len(p.Listeners))
			for _, l := range p.Listeners {
				got = append(got, l.Port)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("listener ports = %v, want %v", got, tc.want)
			}
			distinct := slices.Clone(got)
			slices.Sort(distinct)
			if !slices.Equal(slices.Compact(distinct), []int{1024, 1025, 1026}) {
				t.Errorf("listener ports = %v, want 1024, 1025, and 1026 once each", got)
			}
		})
	}
}

// TestGenerateListenerPortsFillTightRange confirms that for a listener range
// exactly as wide as the listeners one load balancer gets, every seed yields a
// valid plan whose listeners use every port of the range once per load
// balancer.
func TestGenerateListenerPortsFillTightRange(t *testing.T) {
	s := smallScenario()
	s.Resources = Resources{Networks: 2, LoadBalancers: 3, Listeners: 12, Members: 12}
	s.Distribution.ListenerPort = Range{Min: 8000, Max: 8003}

	for seed := int64(0); seed < 50; seed++ {
		s.Seed = seed
		p := generate(t, s)
		byLB := make(map[string][]int, len(p.LoadBalancers))
		for _, l := range p.Listeners {
			byLB[l.LoadBalancer] = append(byLB[l.LoadBalancer], l.Port)
		}
		for lb, ports := range byLB {
			slices.Sort(ports)
			if !slices.Equal(ports, []int{8000, 8001, 8002, 8003}) {
				t.Errorf("seed %d: %s listener ports = %v, want 8000 to 8003 once each", seed, lb, ports)
			}
		}
	}
}

// TestGenerateDrawOrder pins the documented draw order against a fresh source
// with the same seed: one draw per listener from listener_port in listener
// order, then one per member from member_port in member order, then the pool
// permutation whose first health_monitors indices, sorted, name the monitored
// pools. The networks, load balancers, and pools draw nothing in between. The
// listener draws of the seed are distinct per load balancer, which the test
// checks first, so no port steps and each equals its raw draw.
func TestGenerateDrawOrder(t *testing.T) {
	s := smallScenario()
	s.Resources = Resources{Networks: 2, LoadBalancers: 3, Listeners: 7, Members: 20, HealthMonitors: 4}
	s.Distribution.MemberPort = Range{Min: 30000, Max: 30999}

	src := rand.New(rand.NewSource(s.Seed))
	lp, mp := s.Distribution.ListenerPort, s.Distribution.MemberPort
	wantListener := make([]int, s.Resources.Listeners)
	for i := range wantListener {
		wantListener[i] = lp.Min + src.Intn(lp.Max-lp.Min+1)
	}
	wantMember := make([]int, s.Resources.Members)
	for i := range wantMember {
		wantMember[i] = mp.Min + src.Intn(mp.Max-mp.Min+1)
	}
	idx := src.Perm(s.Resources.Listeners)[:s.Resources.HealthMonitors]
	slices.Sort(idx)

	perLB := make(map[int]map[int]bool)
	for i, port := range wantListener {
		lb := i % s.Resources.LoadBalancers
		if perLB[lb][port] {
			t.Fatalf("seed %d draws port %d twice for load balancer %d; pick a seed without a collision", s.Seed, port, lb)
		}
		if perLB[lb] == nil {
			perLB[lb] = make(map[int]bool)
		}
		perLB[lb][port] = true
	}

	p := generate(t, s)
	for i, l := range p.Listeners {
		if l.Port != wantListener[i] {
			t.Errorf("listener %q port = %d, want %d", l.Name, l.Port, wantListener[i])
		}
	}
	for i, m := range p.Members {
		if m.Port != wantMember[i] {
			t.Errorf("member %q port = %d, want %d", m.Name, m.Port, wantMember[i])
		}
	}
	gotPools := make([]string, 0, len(p.HealthMonitors))
	for _, h := range p.HealthMonitors {
		gotPools = append(gotPools, h.Pool)
	}
	wantPools := make([]string, 0, len(idx))
	for _, j := range idx {
		wantPools = append(wantPools, fmt.Sprintf("pool-%04d", j+1))
	}
	if !slices.Equal(gotPools, wantPools) {
		t.Errorf("monitored pools = %v, want %v", gotPools, wantPools)
	}
}

// TestGenerateCountersAndBounds confirms each collection is emitted with
// contiguous logical names and the exact count, every network with its subnet
// and the next /24, and every port inside its drawn range.
func TestGenerateCountersAndBounds(t *testing.T) {
	s := smallScenario()
	s.Resources = Resources{Networks: 12, LoadBalancers: 15, Listeners: 31, Members: 70, HealthMonitors: 9}
	s.Distribution.ListenerPort = Range{Min: 80, Max: 120}
	s.Distribution.MemberPort = Range{Min: 9000, Max: 9100}

	p := generate(t, s)
	if len(p.Networks) != 12 || len(p.LoadBalancers) != 15 || len(p.Listeners) != 31 || len(p.Pools) != 31 || len(p.Members) != 70 || len(p.HealthMonitors) != 9 {
		t.Fatalf("counts = %d/%d/%d/%d/%d/%d, want 12/15/31/31/70/9",
			len(p.Networks), len(p.LoadBalancers), len(p.Listeners), len(p.Pools), len(p.Members), len(p.HealthMonitors))
	}
	for i, n := range p.Networks {
		want := plan.Network{Name: fmt.Sprintf("net-%04d", i+1), Subnet: fmt.Sprintf("sub-%04d", i+1), CIDR: fmt.Sprintf("10.0.%d.0/24", i+1)}
		if n != want {
			t.Errorf("network %d = %+v, want %+v", i, n, want)
		}
	}
	for i, lb := range p.LoadBalancers {
		want := plan.LoadBalancer{Name: fmt.Sprintf("lb-%04d", i+1), Network: fmt.Sprintf("net-%04d", i%12+1)}
		if lb != want {
			t.Errorf("load balancer %d = %+v, want %+v", i, lb, want)
		}
	}
	for i, l := range p.Listeners {
		if want := fmt.Sprintf("lsn-%04d", i+1); l.Name != want {
			t.Errorf("listener %d name = %q, want %q", i, l.Name, want)
		}
		if want := fmt.Sprintf("lb-%04d", i%15+1); l.LoadBalancer != want {
			t.Errorf("listener %q load balancer = %q, want %q", l.Name, l.LoadBalancer, want)
		}
		if l.Port < 80 || l.Port > 120 {
			t.Errorf("listener %q port %d out of range [80,120]", l.Name, l.Port)
		}
	}
	for _, m := range p.Members {
		if m.Port < 9000 || m.Port > 9100 {
			t.Errorf("member %q port %d out of range [9000,9100]", m.Name, m.Port)
		}
	}
}

// TestGenerateZeroResourcesEncodeEmptyArrays confirms an all-zero resources
// block yields a valid plan whose six collections encode as [], never null,
// and whose Summary reads "cloud default" for the empty provider.
func TestGenerateZeroResourcesEncodeEmptyArrays(t *testing.T) {
	s := smallScenario()
	s.Resources = Resources{}

	p := generate(t, s)
	got := string(marshal(t, p))
	for _, want := range []string{
		`"networks": []`,
		`"loadBalancers": []`,
		`"listeners": []`,
		`"pools": []`,
		`"members": []`,
		`"healthMonitors": []`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan JSON lacks %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "null") {
		t.Errorf("plan JSON carries null:\n%s", got)
	}

	wantSummary := `Plan for scenario "small" (seed 42)
  provider:         cloud default
  networks:         0
  load balancers:   0
  listeners:        0
  pools:            0
  members:          0
  health monitors:  0
`
	if got := p.Summary(); got != wantSummary {
		t.Errorf("Summary() =\n%s\nwant\n%s", got, wantSummary)
	}
}

// TestGenerateProvider confirms a provider override (as --set provider=ovn
// does) is copied into the plan and its JSON, and that the empty provider
// omits the key altogether.
func TestGenerateProvider(t *testing.T) {
	s := smallScenario()
	if err := s.Set("provider", "ovn"); err != nil {
		t.Fatalf(`Set("provider", "ovn") = %v, want nil`, err)
	}
	p := generate(t, s)
	if p.Provider != "ovn" {
		t.Errorf("plan provider = %q, want %q", p.Provider, "ovn")
	}
	if got := string(marshal(t, p)); !strings.Contains(got, `"provider": "ovn"`) {
		t.Errorf("plan JSON lacks \"provider\": \"ovn\":\n%s", got)
	}

	if got := string(marshal(t, generate(t, smallScenario()))); strings.Contains(got, `"provider"`) {
		t.Errorf("plan JSON of the empty provider carries the provider key:\n%s", got)
	}
}
