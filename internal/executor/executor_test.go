package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/openstack-tester/internal/neutron"
	"github.com/B42Labs/openstack-tester/internal/plan"
)

// fakeNeutron is an in-process Neutron implementation that records call order
// and concurrency, can inject transient and quota failures, and can block until
// cancelled. It lets the executor's ordering, concurrency, retry, fail-fast, and
// cancellation logic be exercised without a cloud.
type fakeNeutron struct {
	mu          sync.Mutex
	nextID      int
	exists      map[string]bool // ids that have been created so far
	creates     []record        // successful creates, in completion order
	waits       []neutron.Resource
	attempts    map[string]int // create attempts per logical name
	badRefs     []string       // dependency-order violations: a ref that did not exist
	inFlight    int
	maxInFlight int

	failuresLeft    map[string]int   // logical name -> remaining transient failures
	permanentFail   map[string]error // logical name -> error returned on every attempt
	quotaKind       neutron.Kind     // kind to reject with a quota error ("" = none)
	workDelay       time.Duration    // sleep inside each create to expose concurrency
	holdUntilCancel bool             // block each create until ctx is cancelled
	waitErr         error            // error WaitForReady returns ("" = ready)

	started     chan struct{} // closed when the first create begins
	startedOnce sync.Once
}

type record struct {
	kind    neutron.Kind
	logical string
}

func newFake() *fakeNeutron {
	return &fakeNeutron{
		exists:        make(map[string]bool),
		attempts:      make(map[string]int),
		failuresLeft:  make(map[string]int),
		permanentFail: make(map[string]error),
	}
}

// do runs the shared create bookkeeping: it records the attempt, tracks
// concurrency, checks that every required reference already exists, applies any
// injected failure, and otherwise records a new resource.
func (f *fakeNeutron) do(ctx context.Context, kind neutron.Kind, logical string, refs ...string) (neutron.Resource, error) {
	if f.started != nil {
		f.startedOnce.Do(func() { close(f.started) })
	}

	f.mu.Lock()
	f.attempts[logical]++
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	for _, ref := range refs {
		if !f.exists[ref] {
			f.badRefs = append(f.badRefs, fmt.Sprintf("%s %s: ref %q not yet created", kind, logical, ref))
		}
	}
	quota := f.quotaKind == kind
	permErr := f.permanentFail[logical]
	fail := f.failuresLeft[logical] > 0
	if fail {
		f.failuresLeft[logical]--
	}
	f.mu.Unlock()

	switch {
	case quota:
		f.dec()
		return neutron.Resource{}, fmt.Errorf("fake quota for %s: %w", kind, neutron.ErrQuota)
	case permErr != nil:
		f.dec()
		return neutron.Resource{}, permErr
	case fail:
		f.dec()
		return neutron.Resource{}, gophercloud.ErrUnexpectedResponseCode{Actual: 503}
	case f.holdUntilCancel:
		<-ctx.Done()
		f.dec()
		return neutron.Resource{}, ctx.Err()
	}

	if f.workDelay > 0 {
		time.Sleep(f.workDelay)
	}
	f.dec()
	return f.recordCreate(kind, logical), nil
}

func (f *fakeNeutron) dec() {
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
}

func (f *fakeNeutron) recordCreate(kind neutron.Kind, logical string) neutron.Resource {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("id-%d", f.nextID)
	f.exists[id] = true
	f.creates = append(f.creates, record{kind, logical})
	return neutron.Resource{Kind: kind, Logical: logical, ID: id}
}

func (f *fakeNeutron) CreateAddressScope(ctx context.Context, as plan.AddressScope) (neutron.Resource, error) {
	return f.do(ctx, neutron.KindAddressScope, as.Name)
}

func (f *fakeNeutron) CreateSubnetPool(ctx context.Context, sp plan.SubnetPool, addressScopeID string) (neutron.Resource, error) {
	var refs []string
	if sp.AddressScope != "" {
		refs = append(refs, addressScopeID)
	}
	return f.do(ctx, neutron.KindSubnetPool, sp.Name, refs...)
}

func (f *fakeNeutron) CreateNetwork(ctx context.Context, n plan.Network) (neutron.Resource, error) {
	return f.do(ctx, neutron.KindNetwork, n.Name)
}

func (f *fakeNeutron) CreateSubnet(ctx context.Context, s plan.Subnet, networkID, subnetPoolID string) (neutron.Resource, error) {
	refs := []string{networkID}
	if s.SubnetPool != "" {
		refs = append(refs, subnetPoolID)
	}
	return f.do(ctx, neutron.KindSubnet, s.Name, refs...)
}

func (f *fakeNeutron) CreateRouter(ctx context.Context, r plan.Router) (neutron.Resource, error) {
	return f.do(ctx, neutron.KindRouter, r.Name)
}

func (f *fakeNeutron) CreateRouterInterface(ctx context.Context, ri plan.RouterInterface, routerID, subnetID string) (neutron.Resource, error) {
	return f.do(ctx, neutron.KindRouterInterface, ri.Name, routerID, subnetID)
}

func (f *fakeNeutron) CreateSecurityGroup(ctx context.Context, sg plan.SecurityGroup) (neutron.Resource, error) {
	return f.do(ctx, neutron.KindSecurityGroup, sg.Name)
}

func (f *fakeNeutron) CreateSecurityGroupRule(ctx context.Context, rule plan.SecurityGroupRule, sgID, remoteGroupID string) (neutron.Resource, error) {
	refs := []string{sgID}
	if rule.RemoteGroup != "" {
		refs = append(refs, remoteGroupID)
	}
	return f.do(ctx, neutron.KindSecurityGroupRule, "rule@"+sgID, refs...)
}

func (f *fakeNeutron) CreatePort(ctx context.Context, p plan.Port, networkID string, subnetIDByLogical map[string]string, sgIDs []string) (neutron.Resource, error) {
	refs := []string{networkID}
	refs = append(refs, sgIDs...)
	for _, id := range subnetIDByLogical {
		refs = append(refs, id)
	}
	return f.do(ctx, neutron.KindPort, p.Name, refs...)
}

func (f *fakeNeutron) WaitForReady(ctx context.Context, r neutron.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waits = append(f.waits, r)
	return f.waitErr
}

// fullPlan is a small plan exercising every kind and cross-reference.
func fullPlan() *plan.Plan {
	return &plan.Plan{
		Scenario:    "test",
		Seed:        1,
		SubnetPools: []plan.SubnetPool{{Name: "pool-1", IPVersion: 4, Prefixes: []string{"172.16.0.0/16"}, DefaultPrefixLen: 26}},
		Networks:    []plan.Network{{Name: "net-1"}, {Name: "net-2"}},
		Subnets: []plan.Subnet{
			{Name: "subnet-1", Network: "net-1", IPVersion: 4, CIDR: "10.0.0.0/24"},
			{Name: "subnet-2", Network: "net-2", IPVersion: 4, SubnetPool: "pool-1", PrefixLen: 26},
		},
		Routers:          []plan.Router{{Name: "router-1"}},
		RouterInterfaces: []plan.RouterInterface{{Name: "rif-1", Router: "router-1", Subnet: "subnet-1"}},
		SecurityGroups: []plan.SecurityGroup{{Name: "sg-1", Rules: []plan.SecurityGroupRule{
			{Direction: "ingress", EtherType: "IPv4", Protocol: "tcp", RemoteGroup: "sg-1"},
		}}},
		Ports: []plan.Port{{Name: "port-1", Network: "net-1", FixedIPs: []plan.FixedIP{{Subnet: "subnet-1"}}, SecurityGroups: []string{"sg-1"}}},
	}
}

// TestApplyDependencyOrder confirms every cross-reference is resolved to an
// already-created id (so the executor created kinds in dependency order) and
// that each created resource is waited on for readiness.
func TestApplyDependencyOrder(t *testing.T) {
	f := newFake()
	res, err := Apply(context.Background(), "run0", f, fullPlan(), 4, time.Minute)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.badRefs) != 0 {
		t.Errorf("dependency-order violations: %v", f.badRefs)
	}
	if len(res.Created) != 10 {
		t.Errorf("created %d resources, want 10", len(res.Created))
	}
	if len(f.waits) != len(res.Created) {
		t.Errorf("WaitForReady called %d times, want one per created resource (%d)", len(f.waits), len(res.Created))
	}
}

// TestApplyConcurrencyBound confirms no more than --concurrency creates run at
// once, while still proving real concurrency by saturating the limit.
func TestApplyConcurrencyBound(t *testing.T) {
	const concurrency = 5
	nets := make([]plan.Network, 30)
	for i := range nets {
		nets[i] = plan.Network{Name: fmt.Sprintf("net-%d", i)}
	}
	f := newFake()
	f.workDelay = 10 * time.Millisecond

	if _, err := Apply(context.Background(), "run0", f, &plan.Plan{Networks: nets}, concurrency, time.Minute); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.maxInFlight > concurrency {
		t.Errorf("max in-flight %d exceeded concurrency %d", f.maxInFlight, concurrency)
	}
	if f.maxInFlight != concurrency {
		t.Errorf("max in-flight %d did not reach concurrency %d", f.maxInFlight, concurrency)
	}
}

// TestApplyRetriesTransient confirms a transient error is retried with backoff
// and the create ultimately succeeds.
func TestApplyRetriesTransient(t *testing.T) {
	f := newFake()
	f.failuresLeft["net-1"] = 2 // fail twice, succeed on the third attempt

	res, err := Apply(context.Background(), "run0", f, &plan.Plan{Networks: []plan.Network{{Name: "net-1"}}}, 1, time.Minute)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Created) != 1 {
		t.Fatalf("created %d resources, want 1", len(res.Created))
	}
	if f.attempts["net-1"] != 3 {
		t.Errorf("net-1 attempted %d times, want 3", f.attempts["net-1"])
	}
}

// TestApplyFailFastQuota confirms a quota error stops the run immediately with a
// quota-mentioning error and no later kinds are created.
func TestApplyFailFastQuota(t *testing.T) {
	f := newFake()
	f.quotaKind = neutron.KindNetwork

	_, err := Apply(context.Background(), "run0", f, fullPlan(), 4, time.Minute)
	if err == nil {
		t.Fatal("expected a quota error, got nil")
	}
	if !errors.Is(err, neutron.ErrQuota) {
		t.Errorf("error %v does not match ErrQuota", err)
	}
	// Networks failed, so no dependent kind should have been created.
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.creates {
		if c.kind == neutron.KindSubnet || c.kind == neutron.KindPort {
			t.Errorf("kind %s was created after the quota failure", c.kind)
		}
	}
	// A quota error must not be retried.
	if f.attempts["net-1"] > 1 || f.attempts["net-2"] > 1 {
		t.Errorf("quota error was retried: attempts=%v", f.attempts)
	}
}

// TestApplyCancellation confirms cancelling mid-run stops promptly and returns a
// context error.
func TestApplyCancellation(t *testing.T) {
	f := newFake()
	f.holdUntilCancel = true
	f.started = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := Apply(ctx, "run0", f, &plan.Plan{Networks: []plan.Network{{Name: "net-1"}}}, 1, time.Minute)
		done <- result{err}
	}()

	<-f.started
	cancel()

	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("Apply returned %v, want context.Canceled", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Apply did not return after cancellation")
	}
}

// TestApplyPartialStageRecordsCreated confirms that when a stage fails partway,
// the resources it already created are still reported in Result.Created so the
// run record does not under-report what exists in the cloud.
func TestApplyPartialStageRecordsCreated(t *testing.T) {
	f := newFake()
	// net-1 fails permanently with a non-retryable 400. With concurrency 1 the
	// stage creates net-0 before net-1 fails and cancels the rest.
	f.permanentFail["net-1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 400}

	p := &plan.Plan{Networks: []plan.Network{{Name: "net-0"}, {Name: "net-1"}}}
	res, err := Apply(context.Background(), "run0", f, p, 1, time.Minute)
	if err == nil {
		t.Fatal("expected an error from the failing create")
	}
	if len(res.Created) != 1 || res.Created[0].Logical != "net-0" {
		t.Errorf("Created = %v, want exactly net-0 (the resource created before the failure)", res.Created)
	}
}

// TestApplyReadinessTimeoutWarns confirms a readiness deadline (the resource
// never reaching ready while the run is still live) is surfaced as a warning
// rather than being silently treated as success.
func TestApplyReadinessTimeoutWarns(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(old)

	f := newFake()
	f.waitErr = errors.New("still BUILD") // a readiness failure, not a context error

	res, err := Apply(context.Background(), "run0", f, &plan.Plan{Networks: []plan.Network{{Name: "net-1"}}}, 1, time.Minute)
	if err != nil {
		t.Fatalf("Apply: %v (a readiness deadline must not fail the run)", err)
	}
	if len(res.Created) != 1 {
		t.Fatalf("created %d resources, want 1", len(res.Created))
	}
	if logged := buf.String(); !strings.Contains(logged, "ready state") {
		t.Errorf("expected a readiness warning, got log=%q", logged)
	}
}

// TestApplyConflictRetriesCapped confirms a permanent 409 conflict fails after
// conflictMaxAttempts rather than spending the full maxAttempts retry budget.
func TestApplyConflictRetriesCapped(t *testing.T) {
	f := newFake()
	f.permanentFail["net-1"] = gophercloud.ErrUnexpectedResponseCode{Actual: 409, Body: []byte("overlapping cidr")}

	_, err := Apply(context.Background(), "run0", f, &plan.Plan{Networks: []plan.Network{{Name: "net-1"}}}, 1, time.Minute)
	if err == nil {
		t.Fatal("expected the conflict to fail the run")
	}
	if got := f.attempts["net-1"]; got != conflictMaxAttempts {
		t.Errorf("net-1 attempted %d times, want %d (a permanent 409 must fail fast)", got, conflictMaxAttempts)
	}
}
