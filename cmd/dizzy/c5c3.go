package main

import (
	"crypto/tls"
	"log/slog"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/client-go/dynamic"

	"github.com/B42Labs/dizzy/internal/c5c3"
	c5c3plan "github.com/B42Labs/dizzy/internal/c5c3/plan"
	"github.com/B42Labs/dizzy/internal/config"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/telemetry"
)

// newC5C3Cmd builds the "c5c3" command namespace and attaches its
// subcommands: generate, apply, monitor, status, report, and cleanup. It has
// no chaos: an order takes minutes to converge, so the namespace has no churn
// graph. report is the same service-agnostic builder the other namespaces use.
func newC5C3Cmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "c5c3",
		Short: "Keystone orders through the c5c3-operator, checked with the credentials it delivers",
	}

	cmd.AddCommand(
		newC5C3GenerateCmd(opts),
		newC5C3ApplyCmd(opts),
		newC5C3MonitorCmd(opts),
		newC5C3StatusCmd(opts),
		newReportCmd(opts),
		newC5C3CleanupCmd(opts),
	)

	return cmd
}

// c5c3KubeFlags are the apply, monitor, status and cleanup flags that select
// the Kubernetes cluster the orders live in.
type c5c3KubeFlags struct {
	kubeconfig  string
	kubeContext string
}

// register wires the Kubernetes flags onto a command's flag set.
func (f *c5c3KubeFlags) register(flags *pflag.FlagSet) {
	flags.StringVar(&f.kubeconfig, "kubeconfig", "", "kubeconfig file to read (default: $KUBECONFIG or ~/.kube/config, then the pod's service account)")
	flags.StringVar(&f.kubeContext, "kube-context", "", "kubeconfig context to use instead of the current one")
}

// c5c3KeystoneEndpoint returns the Keystone URL and TLS configuration that
// replace those of every document the operator delivers: the auth_url and TLS
// settings of the clouds.yaml entry --os-cloud or $OS_CLOUD names. With
// neither set it returns "", nil, nil without reading any clouds.yaml, so each
// document is used as delivered. It authenticates nothing, and the entry's
// credentials stay unused. An entry without auth_url is an error.
func c5c3KeystoneEndpoint(opts *globalOptions) (string, *tls.Config, error) {
	name := opts.cloudName()
	if name == "" {
		return "", nil, nil
	}
	return config.IdentityEndpoint(name)
}

// c5c3Cluster is what apply, monitor and cleanup resolve once at startup and
// every client they build shares: the Kubernetes client, the Keystone
// endpoint override, whose authURL is empty when no clouds.yaml entry is
// named, and requestTimeout, the --timeout bound on each request.
type c5c3Cluster struct {
	dyn       dynamic.Interface
	authURL   string
	tlsConfig *tls.Config
	// requestTimeout bounds each request a client sends outside the executor,
	// each list of its pre-check and each GET of Observe, through
	// SetRequestTimeout. The executor bounds the requests it sends itself.
	requestTimeout time.Duration
}

// connectC5C3 resolves the Keystone endpoint override and then builds the
// Kubernetes client the flags select, in that order, so a missing clouds.yaml
// or an entry without auth_url fails before the kubeconfig is read. Neither
// step sends a request. Every client of the cluster bounds each request it
// sends outside the executor by --timeout.
func connectC5C3(opts *globalOptions, kube c5c3KubeFlags) (c5c3Cluster, error) {
	authURL, tlsConfig, err := c5c3KeystoneEndpoint(opts)
	if err != nil {
		return c5c3Cluster{}, err
	}
	if authURL != "" {
		slog.Info("logging in at the Keystone endpoint of the clouds.yaml entry", "cloud", opts.cloudName(), "authURL", authURL)
	}
	dyn, err := config.NewKubeClient(kube.kubeconfig, kube.kubeContext)
	if err != nil {
		return c5c3Cluster{}, err
	}
	return c5c3Cluster{dyn: dyn, authURL: authURL, tlsConfig: tlsConfig, requestTimeout: opts.timeout}, nil
}

// client returns a client for runID's orders in namespace, recording into m
// and mirroring into tel (nil for no export), whose Keystone checks log in at
// the override endpoint when the cluster resolved one. Each list of its
// pre-check and each GET of Observe is bounded by the cluster's request
// timeout on its own, so an API server that does not answer cannot hang a
// command before its first order, nor c5c3 status.
func (k c5c3Cluster) client(namespace, runID string, m *metrics.Collector, tel *telemetry.Telemetry) *c5c3.Client {
	c := c5c3.New(k.dyn, namespace, runID, m)
	c.SetTelemetry(tel)
	c.SetRequestTimeout(k.requestTimeout)
	if k.authURL != "" {
		c.SetKeystoneEndpoint(k.authURL, k.tlsConfig)
	}
	return c
}

// c5c3TelemetryConfig is the telemetry configuration of c5c3 apply and
// monitor running plan p: the cloud --os-cloud or $OS_CLOUD names, p's
// scenario, and service c5c3, the service label every exported sample of the
// namespace carries.
func c5c3TelemetryConfig(opts *globalOptions, p *c5c3plan.Plan) telemetry.Config {
	return telemetry.Config{Cloud: opts.cloudName(), Scenario: p.Scenario, Service: "c5c3"}
}
