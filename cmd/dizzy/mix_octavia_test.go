package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/chaos/novagraph"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/mix"
	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	mixscenario "github.com/B42Labs/dizzy/internal/mix/scenario"
	"github.com/B42Labs/dizzy/internal/nova"
	novaexec "github.com/B42Labs/dizzy/internal/nova/executor"
	"github.com/B42Labs/dizzy/internal/octavia"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/scenarios"
)

// TestMixServicesMatchSupported confirms every opt-in service CheckServices
// accepts has a binding and every binding is a service CheckServices accepts,
// each bound to a persona this build defines.
func TestMixServicesMatchSupported(t *testing.T) {
	supported := mix.SupportedServices()
	for _, name := range supported {
		if _, ok := mixServices[name]; !ok {
			t.Errorf("supported opt-in service %q has no mixServices entry", name)
		}
	}
	for name, svc := range mixServices {
		if !slices.Contains(supported, name) {
			t.Errorf("mixServices binds %q, which mix.SupportedServices() %v does not list", name, supported)
		}
		if svc.bind == nil {
			t.Errorf("mixServices entry %q has no binder", name)
		}
		if _, err := mixPersonaCloud(mixscenario.Scenario{}, svc.persona); err != nil {
			t.Errorf("mixServices entry %q binds to persona %q: %v", name, svc.persona, err)
		}
	}
	if got := mixServices[mixplan.ServiceOctavia].persona; got != "gardener" {
		t.Errorf("octavia binds to persona %q, want gardener", got)
	}
}

func TestBindMixServices(t *testing.T) {
	boom := errors.New("boom")
	var bound []string
	kinds := func(err error) map[string]mixService {
		return map[string]mixService{"octavia": {persona: "gardener", bind: func(_ context.Context, _ *globalOptions, in mixLaneInput, cloud string, l *mix.Lane) error {
			bound = append(bound, in.name+" "+cloud+" "+l.Name)
			return err
		}}}
	}
	bind := func(name string, services []string, kinds map[string]mixService) error {
		in := mixLaneInput{name: name, runID: "run1234-" + name, services: services}
		return bindMixServices(context.Background(), &globalOptions{}, in, "tenant-"+name, &mix.Lane{Name: name}, kinds)
	}

	t.Run("gardener only", func(t *testing.T) {
		bound = nil
		for _, name := range []string{"ci", "gardener", "legacy"} {
			if err := bind(name, []string{"octavia"}, kinds(nil)); err != nil {
				t.Errorf("bindMixServices(%s) = %v, want nil", name, err)
			}
		}
		if want := []string{"gardener tenant-gardener gardener"}; !slices.Equal(bound, want) {
			t.Errorf("bound = %q, want %q", bound, want)
		}
	})

	t.Run("no services", func(t *testing.T) {
		bound = nil
		for _, services := range [][]string{nil, {}} {
			if err := bind("gardener", services, kinds(nil)); err != nil {
				t.Errorf("bindMixServices(%v) = %v, want nil", services, err)
			}
		}
		if len(bound) != 0 {
			t.Errorf("bound = %q, want nothing", bound)
		}
	})

	t.Run("undefined service", func(t *testing.T) {
		bound = nil
		for _, name := range []string{"ci", "gardener"} {
			err := bind(name, []string{"nope"}, kinds(nil))
			if want := `opt-in service "nope" is not defined by this build of dizzy`; err == nil || err.Error() != want {
				t.Errorf("bindMixServices(%s) = %v, want %q", name, err, want)
			}
		}
		if len(bound) != 0 {
			t.Errorf("bound = %q, want nothing", bound)
		}
	})

	t.Run("binder fails", func(t *testing.T) {
		err := bind("gardener", []string{"octavia"}, kinds(boom))
		if want := `persona "gardener": opt-in service "octavia": boom`; !errors.Is(err, boom) || err.Error() != want {
			t.Errorf("bindMixServices = %v, want %q", err, want)
		}
	})
}

// TestBuildMixLaneBindsServices confirms buildMixLane binds the opt-in
// services also for a cleanup-only lane: the gardener persona's lane reaches
// the Octavia binding, which fails on a catalog without a load-balancer
// endpoint, while the ci persona's lane and a gardener lane without services
// bind with their compute client alone.
func TestBuildMixLaneBindsServices(t *testing.T) {
	opts := &globalOptions{timeout: time.Second, concurrency: 2}
	build := func(name string, services []string) (*mix.Lane, error) {
		in := mixLaneInput{name: name, cloud: "tenant-ci", runID: "mix00001-" + name, overall: metrics.NewCollector(), services: services}
		return buildMixLane(context.Background(), opts, in)
	}

	t.Run("gardener", func(t *testing.T) {
		tenantClouds(t, fakeKeystone(t, "p1"))
		l, err := build("gardener", []string{"octavia"})
		if want := `persona "gardener": opt-in service "octavia": creating load-balancer clients:`; l != nil || err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("buildMixLane = %v, %v, want no lane and an error starting with %q", l, err, want)
		}
	})

	for name, services := range map[string][]string{"ci": {"octavia"}, "gardener": nil} {
		t.Run(name+" without a binding", func(t *testing.T) {
			url, requests := fakeKeystoneLog(t, "p1", nil)
			tenantClouds(t, url)
			l, err := build(name, services)
			if err != nil {
				t.Fatalf("buildMixLane: %v", err)
			}
			if l.Name != name || l.Cleanup == nil || l.Leaked == nil || l.Observe == nil || l.Nodes != nil {
				t.Errorf("lane = %+v, want the %s lane with its handles and no nodes", l, name)
			}
			if got := requests(); !slices.Equal(got, []string{"POST /v3/auth/tokens"}) {
				t.Errorf("requests = %q, want only the compute authentication", got)
			}
		})
	}
}

// octaviaCallLog is the call log the fakes of the extendOctaviaLane tests
// share, so a test asserts the order of the Octavia stage and the lane's
// previous handles instead of assuming it. CleanupLoadBalancers deletes from a
// worker pool, so it is safe for concurrent use.
type octaviaCallLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *octaviaCallLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *octaviaCallLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// loggingOctaviaCleaner is a fake octaviaexec.Cleaner that logs every call
// into log as "<op> <kind> <id or run>". Its load balancer listing returns
// lbs, and every listing fails with listErr when it is set. Its deletes and
// waits succeed.
type loggingOctaviaCleaner struct {
	log     *octaviaCallLog
	lbs     []resource.Resource
	listErr error
}

func (c *loggingOctaviaCleaner) ListByTag(_ context.Context, kind resource.Kind, runID string) ([]resource.Resource, error) {
	c.log.add("list " + string(kind) + " " + runID)
	if c.listErr != nil {
		return nil, c.listErr
	}
	if kind != octavia.KindLoadBalancer {
		return nil, nil
	}
	return c.lbs, nil
}

func (c *loggingOctaviaCleaner) Delete(_ context.Context, r resource.Resource) error {
	c.log.add("delete " + string(r.Kind) + " " + r.ID)
	return nil
}

func (c *loggingOctaviaCleaner) WaitForGone(_ context.Context, r resource.Resource) error {
	c.log.add("wait " + string(r.Kind) + " " + r.ID)
	return nil
}

// previousHandles are the handles the compute binding gave a lane before
// extendOctaviaLane wraps them: each logs into log, Cleanup as "previous
// cleanup <ids>" returning cleaned and cleanupErr, Leaked as "previous leaked"
// returning leaked and leakErr, and Observe as "previous observe <kind>"
// returning SHUTOFF.
type previousHandles struct {
	log                 *octaviaCallLog
	cleaned, leaked     int
	cleanupErr, leakErr error
}

// lane returns the gardener lane carrying the previous handles.
func (p *previousHandles) lane() *mix.Lane {
	return &mix.Lane{
		Name: "gardener", RunID: "run1234-gardener",
		Cleanup: func(_ context.Context, recorded []resource.Resource) (int, error) {
			ids := make([]string, 0, len(recorded))
			for _, r := range recorded {
				ids = append(ids, r.ID)
			}
			p.log.add("previous cleanup " + strings.Join(ids, ","))
			return p.cleaned, p.cleanupErr
		},
		Leaked: func(context.Context) (int, error) {
			p.log.add("previous leaked")
			return p.leaked, p.leakErr
		},
		Observe: func(_ context.Context, r resource.Resource) (string, bool, error) {
			p.log.add("previous observe " + string(r.Kind))
			return "SHUTOFF", true, nil
		},
	}
}

// octaviaObserveLogger returns an observe function that logs each call into
// log as "octavia observe <kind>" and reports ACTIVE.
func octaviaObserveLogger(log *octaviaCallLog) func(context.Context, resource.Resource) (string, bool, error) {
	return func(_ context.Context, r resource.Resource) (string, bool, error) {
		log.add("octavia observe " + string(r.Kind))
		return "ACTIVE", true, nil
	}
}

// TestExtendOctaviaLaneCleanup confirms the wrapped Cleanup deletes the load
// balancers, discovered and recorded, before the first call of the previous
// Cleanup, which gets the recorded entries unchanged, and counts both stages;
// that a failing load balancer stage still runs the previous Cleanup; and that
// both errors come back joined.
func TestExtendOctaviaLaneCleanup(t *testing.T) {
	recorded := []resource.Resource{
		{Kind: octavia.KindLoadBalancer, Logical: "lb-0003", ID: "lb3", Persona: "gardener"},
		{Kind: nova.KindServer, Logical: "srv-0001", ID: "s1", Persona: "gardener"},
	}
	discovered := []resource.Resource{
		{Kind: octavia.KindLoadBalancer, Logical: "lb-0001", ID: "lb1"},
		{Kind: octavia.KindLoadBalancer, Logical: "lb-0002", ID: "lb2"},
	}
	lbBoom, novaBoom := errors.New("lb boom"), errors.New("nova boom")

	t.Run("load balancers first", func(t *testing.T) {
		log := &octaviaCallLog{}
		l := (&previousHandles{log: log, cleaned: 3}).lane()
		extendOctaviaLane(l, &loggingOctaviaCleaner{log: log, lbs: discovered}, octaviaObserveLogger(log), "run1234-gardener", 2, time.Second)

		n, err := l.Cleanup(context.Background(), recorded)
		if n != 6 || err != nil {
			t.Fatalf("Cleanup = %d, %v, want 6 (3 load balancers and 3 of the previous cleanup), nil", n, err)
		}
		calls := log.list()
		last := len(calls) - 1
		if calls[last] != "previous cleanup lb3,s1" {
			t.Fatalf("calls = %q, want the previous cleanup last, with the recorded entries", calls)
		}
		before := slices.Clone(calls[:last])
		slices.Sort(before)
		want := []string{
			"delete loadbalancer lb1", "delete loadbalancer lb2", "delete loadbalancer lb3",
			"list loadbalancer run1234-gardener",
			"wait loadbalancer lb1", "wait loadbalancer lb2", "wait loadbalancer lb3",
		}
		if !slices.Equal(before, want) {
			t.Errorf("calls before the previous cleanup = %q, want %q in any order", before, want)
		}
	})

	t.Run("load balancer stage fails", func(t *testing.T) {
		log := &octaviaCallLog{}
		l := (&previousHandles{log: log, cleaned: 2}).lane()
		extendOctaviaLane(l, &loggingOctaviaCleaner{log: log, listErr: lbBoom}, octaviaObserveLogger(log), "run1234-gardener", 2, time.Second)

		n, err := l.Cleanup(context.Background(), recorded)
		if n != 2 || !errors.Is(err, lbBoom) {
			t.Errorf("Cleanup = %d, %v, want 2 (the previous cleanup) and the listing error", n, err)
		}
		if want := []string{"list loadbalancer run1234-gardener", "previous cleanup lb3,s1"}; !slices.Equal(log.list(), want) {
			t.Errorf("calls = %q, want %q", log.list(), want)
		}
	})

	t.Run("both fail", func(t *testing.T) {
		log := &octaviaCallLog{}
		l := (&previousHandles{log: log, cleaned: 1, cleanupErr: novaBoom}).lane()
		extendOctaviaLane(l, &loggingOctaviaCleaner{log: log, listErr: lbBoom}, octaviaObserveLogger(log), "run1234-gardener", 2, time.Second)

		n, err := l.Cleanup(context.Background(), recorded)
		if n != 1 || !errors.Is(err, lbBoom) || !errors.Is(err, novaBoom) || err.Error() != "lb boom\nnova boom" {
			t.Errorf("Cleanup = %d, %v, want 1 and both errors joined", n, err)
		}
	})
}

// TestExtendOctaviaLaneLeaked confirms the wrapped Leaked adds the listed load
// balancers to the previous count and returns the previous error unchanged,
// and that a failing listing returns 0 and its wrapped error without calling
// the previous Leaked.
func TestExtendOctaviaLaneLeaked(t *testing.T) {
	lbs := []resource.Resource{{Kind: octavia.KindLoadBalancer, ID: "lb1"}, {Kind: octavia.KindLoadBalancer, ID: "lb2"}}

	t.Run("load balancers left", func(t *testing.T) {
		log := &octaviaCallLog{}
		prevErr := errors.New("leak check listing server groups: boom")
		l := (&previousHandles{log: log, leaked: 1, leakErr: prevErr}).lane()
		extendOctaviaLane(l, &loggingOctaviaCleaner{log: log, lbs: lbs}, octaviaObserveLogger(log), "run1234-gardener", 2, time.Second)

		// The previous error must come back as it is, not wrapped, so the
		// comparison is by identity.
		n, err := l.Leaked(context.Background())
		if n != 3 || err != prevErr {
			t.Errorf("Leaked = %d, %v, want 3 and the previous error unchanged", n, err)
		}
		if want := []string{"list loadbalancer run1234-gardener", "previous leaked"}; !slices.Equal(log.list(), want) {
			t.Errorf("calls = %q, want %q", log.list(), want)
		}
	})

	t.Run("listing fails", func(t *testing.T) {
		log := &octaviaCallLog{}
		boom := errors.New("boom")
		l := (&previousHandles{log: log, leaked: 1}).lane()
		extendOctaviaLane(l, &loggingOctaviaCleaner{log: log, listErr: boom}, octaviaObserveLogger(log), "run1234-gardener", 2, time.Second)

		n, err := l.Leaked(context.Background())
		if want := "leak check listing load balancers: boom"; n != 0 || !errors.Is(err, boom) || err.Error() != want {
			t.Errorf("Leaked = %d, %v, want 0 and %q", n, err, want)
		}
		if slices.Contains(log.list(), "previous leaked") {
			t.Errorf("calls = %q, want no previous leak check", log.list())
		}
	})
}

// TestExtendOctaviaLaneObserve confirms the wrapped Observe sends the five
// Octavia kinds to the Octavia observer and every other kind to the previous
// Observe, and that, with the observer the binding builds, a member whose pool
// is not among the recorded entries comes back as an error.
func TestExtendOctaviaLaneObserve(t *testing.T) {
	log := &octaviaCallLog{}
	l := (&previousHandles{log: log}).lane()
	extendOctaviaLane(l, &loggingOctaviaCleaner{log: log}, octaviaObserveLogger(log), "run1234-gardener", 2, time.Second)

	// The Octavia observer reports ACTIVE and the previous one SHUTOFF, so the
	// state shows which one answered.
	for _, tc := range []struct {
		kinds []resource.Kind
		state string
	}{
		{[]resource.Kind{octavia.KindLoadBalancer, octavia.KindListener, octavia.KindPool, octavia.KindMember, octavia.KindHealthMonitor}, "ACTIVE"},
		{[]resource.Kind{nova.KindServer, nova.KindNetwork, nova.KindVolume}, "SHUTOFF"},
	} {
		for _, kind := range tc.kinds {
			state, exists, err := l.Observe(context.Background(), resource.Resource{Kind: kind, ID: "x"})
			if state != tc.state || !exists || err != nil {
				t.Errorf("Observe(%s) = %q, %v, %v, want %q, true, nil", kind, state, exists, err, tc.state)
			}
		}
	}
	want := []string{
		"octavia observe loadbalancer", "octavia observe listener", "octavia observe pool", "octavia observe member", "octavia observe healthmonitor",
		"previous observe server", "previous observe network", "previous observe volume",
	}
	if !slices.Equal(log.list(), want) {
		t.Errorf("calls = %q, want %q", log.list(), want)
	}

	t.Run("member of an unrecorded pool", func(t *testing.T) {
		log := &octaviaCallLog{}
		l := (&previousHandles{log: log}).lane()
		recorded := []resource.Resource{{Kind: octavia.KindPool, Logical: "pool-0001", ID: "pool1", Persona: "gardener"}}
		// The observer fails before any request, so it needs no client.
		extendOctaviaLane(l, &loggingOctaviaCleaner{log: log}, octaviaObserver{poolIDs: recordedPoolIDs(recorded)}.Observe, "run1234-gardener", 2, time.Second)

		_, _, err := l.Observe(context.Background(), resource.Resource{Kind: octavia.KindMember, Logical: "pool-0002-member-01", ID: "mem2"})
		if want := `member "pool-0002-member-01": its pool "pool-0002" is not in the run record`; err == nil || err.Error() != want {
			t.Errorf("Observe = %v, want %q", err, want)
		}
		if calls := log.list(); len(calls) != 0 {
			t.Errorf("calls = %q, want none", calls)
		}
	})
}

func TestAppendOctaviaNodes(t *testing.T) {
	lane := func() *mix.Lane {
		return &mix.Lane{Name: "gardener", Nodes: []chaos.Node{{Key: "net-0001"}, {Key: "srv-0001", Parents: []string{"net-0001"}}}}
	}
	keys := func(nodes []chaos.Node) []string {
		out := make([]string, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, n.Key)
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		nodes []chaos.Node
		want  string
		keys  []string
	}{
		{"collides", []chaos.Node{{Key: "lb-0001", Parents: []string{"net-0001"}}, {Key: "net-0001"}},
			`octavia node "net-0001" collides with a node of persona "gardener"`, []string{"net-0001", "srv-0001"}},
		{"unresolved parent", []chaos.Node{{Key: "lb-0001", Parents: []string{"net-0009"}}},
			`octavia node "lb-0001" has parent "net-0009", which no node of persona "gardener" carries`, []string{"net-0001", "srv-0001"}},
		{"appends", []chaos.Node{
			{Key: "lb-0001", Parents: []string{"net-0001"}},
			{Key: "lsn-0001", Parents: []string{"lb-0001"}},
			{Key: "pool-0001-member-01", Parents: []string{"lb-0001", "srv-0001", "net-0001"}},
		}, "", []string{"net-0001", "srv-0001", "lb-0001", "lsn-0001", "pool-0001-member-01"}},
		{"no nodes", nil, "", []string{"net-0001", "srv-0001"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := lane()
			err := appendOctaviaNodes(l, tc.nodes)
			if (tc.want == "" && err != nil) || (tc.want != "" && (err == nil || err.Error() != tc.want)) {
				t.Errorf("appendOctaviaNodes = %v, want %q", err, tc.want)
			}
			if got := keys(l.Nodes); !slices.Equal(got, tc.keys) {
				t.Errorf("lane nodes = %q, want %q", got, tc.keys)
			}
		})
	}
}

func TestRollValues(t *testing.T) {
	nodes := []chaos.Node{
		{Key: "net-0001"}, {Key: "grp-0001"},
		{Key: "srv-0001", Roll: "grp-0001"}, {Key: "vol-0001", Roll: "grp-0001"}, {Key: "srv-0002", Roll: "grp-0002"},
	}
	want := map[string]string{"srv-0001": "grp-0001", "vol-0001": "grp-0001", "srv-0002": "grp-0002"}
	if got := rollValues(nodes); !reflect.DeepEqual(got, want) {
		t.Errorf("rollValues = %v, want %v", got, want)
	}
	if got := rollValues(nil); got == nil || len(got) != 0 {
		t.Errorf("rollValues(nil) = %#v, want an empty map", got)
	}
}

func TestMixOctaviaClassify(t *testing.T) {
	classify := mixOctaviaClassify(novagraph.Classify)
	novaQuota := fmt.Errorf("creating server %q: %w", "srv-0001", nova.ErrQuota)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"octavia provisioning", fmt.Errorf("waiting for load balancer lb1: %w", octavia.ErrProvisioning), "provisioning_error"},
		{"octavia quota", fmt.Errorf("creating member %q: %w", "pool-0001-member-01", octavia.ErrQuota), "quota"},
		{"nova quota", novaQuota, novagraph.Classify(novaQuota)},
		{"deadline", context.DeadlineExceeded, novagraph.Classify(context.DeadlineExceeded)},
		{"nil", nil, novagraph.Classify(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err); got != tc.want {
				t.Errorf("classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// gardenerOctaviaPersona generates the shipped small profile opted in to
// Octavia and returns its gardener persona, which carries a frontend of one
// load balancer, two listeners and pools, four members and two health
// monitors, and the persona's rolling compute graph.
func gardenerOctaviaPersona(t *testing.T) (*mixplan.Persona, []chaos.Node) {
	t.Helper()
	data, err := scenarios.Files.ReadFile("mix/small.yaml")
	if err != nil {
		t.Fatalf("reading shipped profile: %v", err)
	}
	p, err := parseMix(t, string(data), "services=octavia").Generate(mixscenario.LaneScenarios{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for i := range p.Personas {
		ps := &p.Personas[i]
		if ps.Name != "gardener" {
			continue
		}
		var c *nova.Client // the builder only captures the client in its closures
		nodes, err := buildPersonaNodes(ps, c, novaexec.Resolved{}, time.Minute)
		if err != nil {
			t.Fatalf("buildPersonaNodes: %v", err)
		}
		return ps, nodes
	}
	t.Fatal("the small profile with services=octavia has no gardener persona")
	return nil, nil
}

// TestBindMixOctavia drives bindMixOctavia against the fake Octavia cloud. A
// cleanup-only lane gets its handles wrapped, observing a member below the
// pool id of the recorded entries, and sends nothing past authentication. A
// persona's lane without a frontend is refused before the quota read, a
// frontend the quota cannot hold and one whose members have no rolling set
// are refused leaving the lane's graph as it was, and a frontend that passes
// joins the rolling graph, adds the Octavia labels to the classifier and logs
// the bound frontend. No path writes to the cloud.
func TestBindMixOctavia(t *testing.T) {
	const memberPath = "/load-balancer/v2.0/lbaas/pools/pool1/members/mem1"
	opts := &globalOptions{timeout: time.Second, concurrency: 2}
	recorded := []resource.Resource{{Kind: octavia.KindPool, Logical: "pool-0001", ID: "pool1", Persona: "gardener"}}
	ps, rolling := gardenerOctaviaPersona(t)
	f := ps.Octavia
	frontendNodes := len(f.LoadBalancers) + len(f.Listeners) + len(f.Pools) + len(f.Members) + len(f.HealthMonitors)
	octaviaQuota := fmt.Errorf("creating load balancer: %w", octavia.ErrQuota)

	// lane returns the gardener lane the compute binding left, with the
	// previous handles logging into log, nodes as its graph and the compute
	// classifier.
	lane := func(log *octaviaCallLog, nodes []chaos.Node) *mix.Lane {
		l := (&previousHandles{log: log}).lane()
		l.Nodes, l.Config, l.Collector = slices.Clone(nodes), chaos.Config{Classify: novagraph.Classify}, metrics.NewCollector()
		return l
	}

	t.Run("cleanup-only lane", func(t *testing.T) {
		fc := newFakeOctaviaCloud(t, map[string]octaviaReply{
			"GET " + memberPath: {http.StatusOK, `{"member":{"id":"mem1","provisioning_status":"ACTIVE"}}`},
		})
		log := &octaviaCallLog{}
		l := lane(log, nil)
		in := mixLaneInput{name: "gardener", runID: "run1234-gardener", recorded: recorded}
		if err := bindMixOctavia(context.Background(), opts, in, "oct", l); err != nil {
			t.Fatalf("bindMixOctavia: %v", err)
		}
		if l.Nodes != nil {
			t.Errorf("cleanup-only lane has %d nodes, want none", len(l.Nodes))
		}
		if got := fc.requests(); !slices.Equal(got, octaviaStackRequests) {
			t.Errorf("requests = %q, want %q", got, octaviaStackRequests)
		}

		state, exists, err := l.Observe(context.Background(), resource.Resource{Kind: octavia.KindMember, Logical: "pool-0001-member-01", ID: "mem1"})
		if state != "ACTIVE" || !exists || err != nil {
			t.Errorf("Observe(member) = %q, %v, %v, want ACTIVE, true, nil", state, exists, err)
		}
		if got := fc.requests(); got[len(got)-1] != "GET "+memberPath {
			t.Errorf("requests = %q, want the member read below its recorded pool last", got)
		}
		if _, _, err := l.Observe(context.Background(), resource.Resource{Kind: nova.KindServer, ID: "s1"}); err != nil || !slices.Equal(log.list(), []string{"previous observe server"}) {
			t.Errorf("Observe(server) = %v, calls %q, want the previous observe", err, log.list())
		}
	})

	for _, tc := range []struct {
		name    string
		persona *mixplan.Persona
		nodes   []chaos.Node
		replies map[string]octaviaReply
		want    string
		prefix  bool
		quota   bool
	}{
		{"no frontend", &mixplan.Persona{Name: "gardener", Rolling: true, Nova: ps.Nova}, rolling, nil,
			"the persona's plan has no octavia part", false, false},
		{"quota exceeded", ps, rolling,
			map[string]octaviaReply{"GET " + octaviaQuotaPath: {http.StatusOK, `{"quota":{"loadbalancer":0,"listener":-1,"pool":-1,"member":-1,"healthmonitor":-1}}`}},
			"plan exceeds project quota; raise these quotas before applying: loadbalancer need 1, quota 0", false, true},
		{"no rolling set", ps, nil, map[string]octaviaReply{"GET " + octaviaQuotaPath: {http.StatusOK, octaviaTightQuota}},
			`building load-balancer graph: member "pool-0001-member-01" references server`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeOctaviaCloud(t, tc.replies)
			l := lane(&octaviaCallLog{}, tc.nodes)
			in := mixLaneInput{name: "gardener", runID: "run1234-gardener", persona: tc.persona}
			err := bindMixOctavia(context.Background(), opts, in, "oct", l)
			if err == nil || (tc.prefix && !strings.HasPrefix(err.Error(), tc.want)) || (!tc.prefix && err.Error() != tc.want) {
				t.Errorf("bindMixOctavia = %v, want %q", err, tc.want)
			}
			if len(l.Nodes) != len(tc.nodes) || l.Config.Classify(octaviaQuota) != novagraph.Classify(octaviaQuota) {
				t.Errorf("lane has %d nodes and classifies an Octavia quota error %q, want its %d nodes and the compute classifier",
					len(l.Nodes), l.Config.Classify(octaviaQuota), len(tc.nodes))
			}
			want := slices.Clone(octaviaStackRequests)
			if tc.quota {
				want = append(want, "GET "+octaviaQuotaPath)
			}
			if got := fc.requests(); !slices.Equal(got, want) {
				t.Errorf("requests = %q, want %q", got, want)
			}
		})
	}

	t.Run("frontend bound", func(t *testing.T) {
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })

		fc := newFakeOctaviaCloud(t, map[string]octaviaReply{"GET " + octaviaQuotaPath: {http.StatusOK, octaviaTightQuota}})
		l := lane(&octaviaCallLog{}, rolling)
		in := mixLaneInput{name: "gardener", runID: "run1234-gardener", persona: ps}
		if err := bindMixOctavia(context.Background(), opts, in, "oct", l); err != nil {
			t.Fatalf("bindMixOctavia: %v", err)
		}
		if len(l.Nodes) != len(rolling)+frontendNodes {
			t.Errorf("lane has %d nodes, want the %d rolling nodes and the %d frontend nodes", len(l.Nodes), len(rolling), frontendNodes)
		}
		roll := rollValues(rolling)
		for _, n := range l.Nodes[len(rolling):] {
			if n.Kind == octavia.KindMember && (n.Roll == "" || n.Roll != roll[n.Parents[2]]) {
				t.Errorf("member node %q rolls with %q, want the rolling set of its worker %q", n.Key, n.Roll, n.Parents[2])
			}
		}
		if got := l.Config.Classify(octaviaQuota); got != "quota" {
			t.Errorf("classifier labels an Octavia quota error %q, want quota", got)
		}
		if got := l.Config.Classify(context.DeadlineExceeded); got != "timeout" {
			t.Errorf("classifier labels a deadline %q, want the compute classifier's timeout", got)
		}
		if got, want := fc.requests(), append(slices.Clone(octaviaStackRequests), "GET "+octaviaQuotaPath); !slices.Equal(got, want) {
			t.Errorf("requests = %q, want %q", got, want)
		}
		wantLog := `msg="opt-in service bound" service=octavia persona=gardener run=run1234-gardener provider="cloud default" loadbalancers=1 listeners=2 members=4 healthMonitors=2`
		if !strings.Contains(logs.String(), wantLog) {
			t.Errorf("logs = %q, want a line containing %q", logs.String(), wantLog)
		}
	})
}
