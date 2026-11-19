package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// kubeconfigYAML defines one cluster, one user and the single context "dizzy",
// which is also the current one. The server is never contacted: building a
// dynamic client sends no request.
const kubeconfigYAML = `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
users:
- name: test
  user:
    token: not-a-secret
contexts:
- name: dizzy
  context:
    cluster: test
    user: test
current-context: dizzy
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(kubeconfigYAML), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}

func TestNewKubeClientExplicitPath(t *testing.T) {
	// $KUBECONFIG names a missing file, so only the explicit path can make the
	// call succeed.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	path := writeKubeconfig(t)

	for _, kubeContext := range []string{"", "dizzy"} {
		client, err := NewKubeClient(path, kubeContext)
		if err != nil {
			t.Fatalf("NewKubeClient(%q, %q): %v", path, kubeContext, err)
		}
		if client == nil {
			t.Fatalf("NewKubeClient(%q, %q) returned a nil client", path, kubeContext)
		}
	}
}

// TestKubeRESTConfigDisablesRateLimiter asserts the configuration
// NewKubeClient builds its client from turns off client-go's client-side rate
// limiter, whose default of 5 requests per second would add its waits to the
// durations dizzy records, and that the REST client built from it the way
// dynamic.NewForConfig builds one indeed has no limiter.
func TestKubeRESTConfigDisablesRateLimiter(t *testing.T) {
	restConfig, err := kubeRESTConfig(writeKubeconfig(t), "")
	if err != nil {
		t.Fatalf("kubeRESTConfig: %v", err)
	}
	if restConfig.QPS >= 0 {
		t.Errorf("QPS = %v, want it negative, which turns client-side rate limiting off", restConfig.QPS)
	}
	if restConfig.RateLimiter != nil {
		t.Errorf("RateLimiter = %v, want nil, since a set limiter overrides a negative QPS", restConfig.RateLimiter)
	}

	restClient, err := rest.UnversionedRESTClientFor(dynamic.ConfigFor(restConfig))
	if err != nil {
		t.Fatalf("building the REST client: %v", err)
	}
	if limiter := restClient.GetRateLimiter(); limiter != nil {
		t.Errorf("REST client rate limiter = %v, want none", limiter)
	}
}

func TestNewKubeClientMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	client, err := NewKubeClient(path, "")
	if err == nil {
		t.Fatalf("NewKubeClient(%q) = %v, want an error", path, client)
	}
	if !strings.HasPrefix(err.Error(), "loading kubeconfig: ") {
		t.Errorf("error = %q, want it to wrap loading kubeconfig", err)
	}
}

func TestNewKubeClientUnknownContext(t *testing.T) {
	path := writeKubeconfig(t)
	client, err := NewKubeClient(path, "nope")
	if err == nil {
		t.Fatalf("NewKubeClient with an undefined context = %v, want an error", client)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %q, want it to name the context", err)
	}
}
