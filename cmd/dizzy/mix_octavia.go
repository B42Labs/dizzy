package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/chaos/octaviagraph"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/mix"
	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
	"github.com/B42Labs/dizzy/internal/octavia"
	octaviaexec "github.com/B42Labs/dizzy/internal/octavia/executor"
	"github.com/B42Labs/dizzy/internal/resource"
)

// mixService binds an opt-in service of a mix run to the lane of the one
// persona that uses it: persona names that persona, and bind extends its lane
// with the service, under the lane's effective cloud. bind always wraps the
// lane's cleanup, leak-check and observe handles, also for a cleanup-only
// lane, and for a persona's lane it also runs the service's read-only
// pre-checks and adds the service's nodes to the lane's churn graph. It
// creates nothing in the cloud.
type mixService struct {
	persona string // the one persona whose lane the service extends
	bind    func(ctx context.Context, opts *globalOptions, in mixLaneInput, cloud string, l *mix.Lane) error
}

// mixServices holds the binding of every opt-in service this build supports,
// by name, and must name exactly the services of mix.SupportedServices. It is
// a map in this package rather than a registry in internal/mix, as
// serviceLaneKinds is for the background lanes, because a binder needs the
// globalOptions of the command, the cloud, --timeout and --concurrency, which
// internal/mix cannot see. Octavia binds to the gardener persona alone, as #71
// gives load balancers to the Gardener persona only.
var mixServices = map[string]mixService{mixplan.ServiceOctavia: {persona: "gardener", bind: bindMixOctavia}}

// bindMixServices binds every opt-in service of in.services, in order, to the
// lane l of the persona in.name, looking each up in kinds. A service this
// build does not define fails the lane before any call, and a service of
// another persona is skipped, so the lanes of the personas that use no
// service stay as they are. A binder's error comes back naming the persona and
// the service. It takes kinds as a parameter, where buildMixLane passes
// mixServices, so tests can bind fake services.
func bindMixServices(ctx context.Context, opts *globalOptions, in mixLaneInput, cloud string, l *mix.Lane, kinds map[string]mixService) error {
	for _, name := range in.services {
		svc, ok := kinds[name]
		if !ok {
			return fmt.Errorf("opt-in service %q is not defined by this build of dizzy", name)
		}
		if svc.persona != in.name {
			continue
		}
		if err := svc.bind(ctx, opts, in, cloud, l); err != nil {
			return fmt.Errorf("persona %q: opt-in service %q: %w", in.name, name, err)
		}
	}
	return nil
}

// bindMixOctavia binds Octavia to the Gardener persona's lane l under cloud,
// the lane's effective cloud. It authenticates the load-balancer and network
// clients and creates an Octavia client under the lane identity that records
// into the persona's collector and exports through its telemetry, so the load
// balancer samples land in the persona's metrics and OTLP resource. It wraps
// the lane's handles with extendOctaviaLane, every call bounded by --timeout,
// and observes a member below the pool id in.recorded holds for it. A
// cleanup-only lane needs no more.
//
// For a persona's lane it then refuses a persona without a frontend, runs the
// Octavia quota pre-check of the project against the frontend, and builds the
// frontend's nodes, each member in the rolling set of its worker, and adds
// them to the lane's rolling graph. The engine's classifier gains the Octavia
// labels, and one info line names the frontend it bound.
func bindMixOctavia(ctx context.Context, opts *globalOptions, in mixLaneInput, cloud string, l *mix.Lane) error {
	ls, err := config.NewLoadBalancerStack(ctx, cloud)
	if err != nil {
		return fmt.Errorf("creating load-balancer clients: %w", err)
	}
	client := octavia.New(ls.LoadBalancer, ls.Network, in.runID, l.Collector)
	client.SetTelemetry(l.Telemetry)
	extendOctaviaLane(l, octaviaTimeoutCleaner{client, opts.timeout},
		octaviaObserver{c: client, poolIDs: recordedPoolIDs(in.recorded)}.Observe, in.runID, opts.concurrency, opts.timeout)
	if in.persona == nil {
		return nil
	}

	f := in.persona.Octavia
	if f == nil {
		return errors.New("the persona's plan has no octavia part")
	}
	if err := octavia.PrecheckFrontendQuota(ctx, ls.LoadBalancer, f); err != nil {
		return err
	}
	nodes, err := octaviagraph.BuildFrontend(f, client, rollValues(l.Nodes), opts.timeout)
	if err != nil {
		return fmt.Errorf("building load-balancer graph: %w", err)
	}
	if err := appendOctaviaNodes(l, nodes); err != nil {
		return err
	}
	l.Config.Classify = mixOctaviaClassify(l.Config.Classify)

	provider := f.Provider
	if provider == "" {
		provider = "cloud default"
	}
	slog.Info("opt-in service bound", "service", "octavia", "persona", l.Name, "run", l.RunID, "provider", provider,
		"loadbalancers", len(f.LoadBalancers), "listeners", len(f.Listeners), "members", len(f.Members), "healthMonitors", len(f.HealthMonitors))
	return nil
}

// extendOctaviaLane wraps the cleanup, leak-check and observe handles of the
// lane l, which the compute binding set, with the load balancers of the run
// runID that c discovers and deletes and observe re-queries.
//
// Cleanup deletes the load balancers first, with cascade and waiting until each
// is gone, through octaviaexec.CleanupLoadBalancers with the recorded entries,
// up to concurrency at once and every wait bounded by opTimeout, and then runs
// the previous Cleanup whatever the first stage returned. A load balancer holds
// its VIP port on a Nova cluster network, so while one is left behind the Nova
// network delete fails too, and both errors show, joined; the count is the sum
// of both stages. Octavia's own Cleanup is not used, since it also deletes every
// subnet and network carrying the run tag, which here are the Nova teardown's.
//
// Leaked adds the load balancers still carrying the run tag to the previous
// count. A failing load balancer listing returns 0 and its error without the
// previous leak check; otherwise the previous Leaked's error comes back
// unchanged. Observe sends the five Octavia kinds to observe and every other
// kind to the previous Observe.
func extendOctaviaLane(l *mix.Lane, c octaviaexec.Cleaner, observe func(ctx context.Context, r resource.Resource) (string, bool, error), runID string, concurrency int, opTimeout time.Duration) {
	cleanup, leaked, prevObserve := l.Cleanup, l.Leaked, l.Observe
	l.Cleanup = func(ctx context.Context, recorded []resource.Resource) (int, error) {
		lbs, lbErr := octaviaexec.CleanupLoadBalancers(ctx, c, runID, recorded, concurrency, opTimeout)
		rest, err := cleanup(ctx, recorded)
		return lbs + rest, errors.Join(lbErr, err)
	}
	l.Leaked = func(ctx context.Context) (int, error) {
		found, err := c.ListByTag(ctx, octavia.KindLoadBalancer, runID)
		if err != nil {
			return 0, fmt.Errorf("leak check listing load balancers: %w", err)
		}
		n, err := leaked(ctx)
		return len(found) + n, err
	}
	l.Observe = func(ctx context.Context, r resource.Resource) (string, bool, error) {
		switch r.Kind {
		case octavia.KindLoadBalancer, octavia.KindListener, octavia.KindPool, octavia.KindMember, octavia.KindHealthMonitor:
			return observe(ctx, r)
		default:
			return prevObserve(ctx, r)
		}
	}
}

// appendOctaviaNodes adds the Octavia nodes to the churn graph of the lane l.
// The engine skips a parent key no node carries, so a node whose parent the
// lane lacks would never wait for it; such a node, and one whose key a node of
// the lane already has, is refused, and l.Nodes is left as it was. A parent may
// be a node of the lane or another of nodes. No nodes leave l.Nodes unchanged.
func appendOctaviaNodes(l *mix.Lane, nodes []chaos.Node) error {
	keys := make(map[string]bool, len(l.Nodes)+len(nodes))
	for _, n := range l.Nodes {
		keys[n.Key] = true
	}
	for _, n := range nodes {
		if keys[n.Key] {
			return fmt.Errorf("octavia node %q collides with a node of persona %q", n.Key, l.Name)
		}
	}
	for _, n := range nodes {
		keys[n.Key] = true
	}
	for _, n := range nodes {
		for _, p := range n.Parents {
			if !keys[p] {
				return fmt.Errorf("octavia node %q has parent %q, which no node of persona %q carries", n.Key, p, l.Name)
			}
		}
	}
	l.Nodes = append(l.Nodes, nodes...)
	return nil
}

// rollValues maps the key of every node with a rolling set to its Roll value,
// the lookup octaviagraph.BuildFrontend puts each member into its worker's
// rolling set with.
func rollValues(nodes []chaos.Node) map[string]string {
	roll := make(map[string]string)
	for _, n := range nodes {
		if n.Roll != "" {
			roll[n.Key] = n.Roll
		}
	}
	return roll
}

// mixOctaviaClassify returns a classifier for a lane whose graph holds Octavia
// nodes next to its compute nodes: it labels a load balancer that reached
// ERROR provisioning_error and an Octavia quota rejection quota, the labels
// octaviagraph.Classify gives them, and leaves every other error to next, the
// lane's compute classifier.
func mixOctaviaClassify(next func(error) string) func(error) string {
	return func(err error) string {
		switch {
		case errors.Is(err, octavia.ErrProvisioning):
			return "provisioning_error"
		case errors.Is(err, octavia.ErrQuota):
			return "quota"
		default:
			return next(err)
		}
	}
}
