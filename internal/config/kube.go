package config

import (
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewKubeClient returns a Kubernetes dynamic client. It reads kubeconfig when
// the path is non-empty and otherwise $KUBECONFIG or ~/.kube/config, and falls
// back to the pod's service account when none of them exists. kubeContext,
// when non-empty, replaces the kubeconfig's current context. The client does
// not throttle its own requests; see kubeRESTConfig. Building the client sends
// no request.
func NewKubeClient(kubeconfig, kubeContext string) (dynamic.Interface, error) {
	restConfig, err := kubeRESTConfig(kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}
	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}
	return client, nil
}

// kubeRESTConfig loads the client configuration NewKubeClient builds its
// client from, and turns off client-go's client-side rate limiter. Left at
// its default, that limiter holds the client to 5 requests per second after a
// burst of 10, and the wait it adds before a request counts towards the
// duration the c5c3 client records for the operation, which would then show
// dizzy's own throttling instead of the API server's latency. A negative QPS
// with no RateLimiter set is client-go's switch for no limiter; the API
// server's own priority and fairness still applies.
func kubeRESTConfig(kubeconfig, kubeContext string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	restConfig.QPS = -1
	restConfig.RateLimiter = nil
	return restConfig, nil
}
