package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/client-go/dynamic"

	"github.com/B42Labs/dizzy/internal/c5c3"
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
// every client they build shares: the Kubernetes client, and the Keystone
// endpoint override, whose authURL is empty when no clouds.yaml entry is
// named.
type c5c3Cluster struct {
	dyn       dynamic.Interface
	authURL   string
	tlsConfig *tls.Config
}

// connectC5C3 resolves the Keystone endpoint override and then builds the
// Kubernetes client the flags select, in that order, so a missing clouds.yaml
// or an entry without auth_url fails before the kubeconfig is read. Neither
// step sends a request.
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
	return c5c3Cluster{dyn: dyn, authURL: authURL, tlsConfig: tlsConfig}, nil
}

// client returns a client for runID's orders in namespace, recording into m
// and mirroring into tel (nil for no export), whose Keystone checks log in at
// the override endpoint when the cluster resolved one.
func (k c5c3Cluster) client(namespace, runID string, m *metrics.Collector, tel *telemetry.Telemetry) *c5c3.Client {
	c := c5c3.New(k.dyn, namespace, runID, m)
	c.SetTelemetry(tel)
	if k.authURL != "" {
		c.SetKeystoneEndpoint(k.authURL, k.tlsConfig)
	}
	return c
}

// precheckC5C3 runs c's Precheck under opTimeout, the bound --timeout puts on
// every Kubernetes request a c5c3 command sends, so an API server that does
// not answer cannot hang the command before its first order.
func precheckC5C3(ctx context.Context, c *c5c3.Client, opTimeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return c.Precheck(ctx)
}
