package config

import (
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// NewKubeClient returns a Kubernetes dynamic client. It reads kubeconfig when
// the path is non-empty and otherwise $KUBECONFIG or ~/.kube/config, and falls
// back to the pod's service account when none of them exists. kubeContext,
// when non-empty, replaces the kubeconfig's current context. Building the
// client sends no request.
func NewKubeClient(kubeconfig, kubeContext string) (dynamic.Interface, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}
	return client, nil
}
