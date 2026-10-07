package config

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
)

// TestNewNetworkClient_Errors exercises the failure paths that do not require a
// reachable cloud: an unresolvable cloud name and a missing clouds.yaml. Both
// must surface a wrapped error rather than a nil client.
func TestNewNetworkClient_Errors(t *testing.T) {
	tests := []struct {
		name        string
		cloudName   string
		osCloud     string
		configFile  string
		wantErrPart string
	}{
		{
			name:        "empty cloud name and no OS_CLOUD",
			cloudName:   "",
			osCloud:     "",
			wantErrPart: "parsing clouds.yaml",
		},
		{
			name:        "clouds.yaml file not found",
			cloudName:   "does-not-exist",
			configFile:  "/nonexistent/path/clouds.yaml",
			wantErrPart: "parsing clouds.yaml",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OS_CLOUD", tc.osCloud)
			t.Setenv("OS_CLIENT_CONFIG_FILE", tc.configFile)

			client, err := NewNetworkClient(t.Context(), tc.cloudName)
			if err == nil {
				t.Fatalf("expected an error, got nil (client=%v)", client)
			}
			if client != nil {
				t.Errorf("expected nil client on error, got %v", client)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}

// TestNewBlockStorageClient_Errors mirrors TestNewNetworkClient_Errors for the
// Cinder client: both failure paths that need no reachable cloud must surface a
// wrapped error rather than a nil client.
func TestNewBlockStorageClient_Errors(t *testing.T) {
	tests := []struct {
		name        string
		cloudName   string
		osCloud     string
		configFile  string
		wantErrPart string
	}{
		{
			name:        "empty cloud name and no OS_CLOUD",
			cloudName:   "",
			osCloud:     "",
			wantErrPart: "parsing clouds.yaml",
		},
		{
			name:        "clouds.yaml file not found",
			cloudName:   "does-not-exist",
			configFile:  "/nonexistent/path/clouds.yaml",
			wantErrPart: "parsing clouds.yaml",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OS_CLOUD", tc.osCloud)
			t.Setenv("OS_CLIENT_CONFIG_FILE", tc.configFile)

			client, err := NewBlockStorageClient(t.Context(), tc.cloudName)
			if err == nil {
				t.Fatalf("expected an error, got nil (client=%v)", client)
			}
			if client != nil {
				t.Errorf("expected nil client on error, got %v", client)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}

// TestNewIdentityClient_Errors mirrors the sibling client tests for the Keystone
// client: both failure paths that need no reachable cloud must surface a wrapped
// error rather than a nil client.
func TestNewIdentityClient_Errors(t *testing.T) {
	tests := []struct {
		name        string
		cloudName   string
		osCloud     string
		configFile  string
		wantErrPart string
	}{
		{
			name:        "empty cloud name and no OS_CLOUD",
			cloudName:   "",
			osCloud:     "",
			wantErrPart: "parsing clouds.yaml",
		},
		{
			name:        "clouds.yaml file not found",
			cloudName:   "does-not-exist",
			configFile:  "/nonexistent/path/clouds.yaml",
			wantErrPart: "parsing clouds.yaml",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OS_CLOUD", tc.osCloud)
			t.Setenv("OS_CLIENT_CONFIG_FILE", tc.configFile)

			client, err := NewIdentityClient(t.Context(), tc.cloudName)
			if err == nil {
				t.Fatalf("expected an error, got nil (client=%v)", client)
			}
			if client != nil {
				t.Errorf("expected nil client on error, got %v", client)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}

// unauthorizedBody is the body Keystone and the services answer a 401 with.
const unauthorizedBody = `{"error":{"code":401,"title":"Unauthorized","message":"The request you have made requires authentication."}}`

// writeClouds points clouds.yaml discovery at a file whose only cloud, fake,
// carries the auth block entry, given as unindented "key: value" lines.
func writeClouds(t *testing.T, entry string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("clouds:\n  fake:\n    auth:\n")
	for line := range strings.Lines(entry) {
		b.WriteString("      " + strings.TrimSuffix(line, "\n") + "\n")
	}
	path := filepath.Join(t.TempDir(), "clouds.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing clouds.yaml: %v", err)
	}
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", path)
}

// credentialAuth is a username-and-password auth block for authURL. It names no
// region, so the endpoint matcher accepts the fake's single region.
func credentialAuth(authURL string) string {
	return "auth_url: " + authURL + "\n" +
		"username: u\npassword: p\nproject_name: p\nuser_domain_name: Default\nproject_domain_name: Default\n"
}

// fakeCloud is a Keystone whose catalog points every service at the server
// itself, with a Neutron network list the test answers through networks. The
// endpoints carry their major version, so the client skips discovery, and
// Neutron's client appends v2.0 once more.
type fakeCloud struct {
	srv *httptest.Server

	// maxTokens, when above zero, is the number of token requests Keystone
	// grants; every later one answers 401.
	maxTokens int
	// networks decides the status and body of the network list for the
	// X-Auth-Token the request carries. A test that lists sets it first.
	networks func(token string) (status int, body string)

	mu            sync.Mutex
	tokenPosts    int
	networkTokens []string
}

// newFakeCloud starts a fakeCloud. Its k-th granted token is "tok-<k>", and a
// GET of a token echoes the X-Subject-Token it is asked about, which answers a
// clouds.yaml entry that authenticates with a pre-issued token.
func newFakeCloud(t *testing.T) *fakeCloud {
	t.Helper()
	fc := &fakeCloud{}
	mux := http.NewServeMux()
	fc.srv = httptest.NewServer(mux)
	t.Cleanup(fc.srv.Close)

	var catalog []string
	for _, svc := range [][2]string{{"compute", "v2.1"}, {"network", "v2.0"}, {"block-storage", "v3"}, {"image", "v2"}, {"identity", "v3"}} {
		catalog = append(catalog, fmt.Sprintf(`{"type":%q,"endpoints":[{"interface":"public","region":"RegionOne","url":%q}]}`,
			svc[0], fc.srv.URL+"/"+svc[0]+"/"+svc[1]+"/"))
	}
	token := `{"token":{"expires_at":"2030-01-01T00:00:00Z","project":{"id":"pid"},"catalog":[` + strings.Join(catalog, ",") + `]}}`

	mux.HandleFunc("POST /v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
		fc.mu.Lock()
		fc.tokenPosts++
		k := fc.tokenPosts
		fc.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fc.maxTokens > 0 && k > fc.maxTokens {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, unauthorizedBody)
			return
		}
		w.Header().Set("X-Subject-Token", fmt.Sprintf("tok-%d", k))
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, token)
	})
	mux.HandleFunc("GET /v3/auth/tokens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Subject-Token", r.Header.Get("X-Subject-Token"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, token)
	})
	mux.HandleFunc("GET /network/v2.0/v2.0/networks", func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Auth-Token")
		fc.mu.Lock()
		fc.networkTokens = append(fc.networkTokens, tok)
		fc.mu.Unlock()
		status, body := fc.networks(tok)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	})
	return fc
}

// authURL is the fake's versioned identity endpoint, so the client skips
// version discovery.
func (fc *fakeCloud) authURL() string { return fc.srv.URL + "/v3" }

// tokenCount is the number of token requests Keystone received.
func (fc *fakeCloud) tokenCount() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.tokenPosts
}

// seenTokens lists the X-Auth-Token of every network list, in order.
func (fc *fakeCloud) seenTokens() []string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return slices.Clone(fc.networkTokens)
}

// networkClient authenticates a Neutron client against fc with credentials.
func networkClient(t *testing.T, fc *fakeCloud) *gophercloud.ServiceClient {
	t.Helper()
	writeClouds(t, credentialAuth(fc.authURL()))
	client, err := NewNetworkClient(t.Context(), "fake")
	if err != nil {
		t.Fatalf("NewNetworkClient: %v", err)
	}
	return client
}

// listNetworks lists every network through client.
func listNetworks(ctx context.Context, client *gophercloud.ServiceClient) error {
	_, err := networks.List(client, networks.ListOpts{}).AllPages(ctx)
	return err
}

// TestConstructorsEnableReauth asserts every constructor builds its clients on
// a provider client that re-authenticates when a credential entry's token
// expires.
func TestConstructorsEnableReauth(t *testing.T) {
	computeClient := func(ctx context.Context, cloudName string) (*gophercloud.ServiceClient, error) {
		cs, err := NewComputeStack(ctx, cloudName)
		if err != nil {
			return nil, err
		}
		return cs.Compute, nil
	}
	tests := []struct {
		name  string
		build func(context.Context, string) (*gophercloud.ServiceClient, error)
	}{
		{"NewNetworkClient", NewNetworkClient},
		{"NewBlockStorageClient", NewBlockStorageClient},
		{"NewIdentityClient", NewIdentityClient},
		{"NewImageClient", NewImageClient},
		{"NewComputeStack", computeClient},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeCloud(t)
			writeClouds(t, credentialAuth(fc.authURL()))

			client, err := tc.build(t.Context(), "fake")
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if client.ReauthFunc == nil {
				t.Errorf("%s built a provider client that never re-authenticates", tc.name)
			}
		})
	}
}

// TestExpiredTokenIsRenewedAndRequestRetried asserts a request rejected with
// 401 gets a fresh token and is sent again with it, so the caller sees success.
func TestExpiredTokenIsRenewedAndRequestRetried(t *testing.T) {
	fc := newFakeCloud(t)
	fc.networks = func(token string) (int, string) {
		if token == "tok-1" {
			return http.StatusUnauthorized, unauthorizedBody
		}
		return http.StatusOK, `{"networks":[]}`
	}
	client := networkClient(t, fc)

	if err := listNetworks(t.Context(), client); err != nil {
		t.Fatalf("listing networks after the token expired: %v", err)
	}
	if got := fc.tokenCount(); got != 2 {
		t.Errorf("token requests = %d, want 2", got)
	}
	if got, want := fc.seenTokens(), []string{"tok-1", "tok-2"}; !slices.Equal(got, want) {
		t.Errorf("network list tokens = %q, want %q", got, want)
	}
}

// TestConcurrentExpiryReauthenticatesOnce asserts that eight requests failing
// on the same expired token share one re-authentication.
func TestConcurrentExpiryReauthenticatesOnce(t *testing.T) {
	fc := newFakeCloud(t)
	var expired atomic.Bool
	fc.networks = func(token string) (int, string) {
		if token == "tok-1" && expired.Load() {
			return http.StatusUnauthorized, unauthorizedBody
		}
		return http.StatusOK, `{"networks":[]}`
	}
	client := networkClient(t, fc)

	if err := listNetworks(t.Context(), client); err != nil {
		t.Fatalf("listing networks before the token expired: %v", err)
	}
	expired.Store(true)

	var (
		wg   sync.WaitGroup
		errs [8]error
	)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = listNetworks(t.Context(), client)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("list %d after the token expired: %v", i, err)
		}
	}
	if got := fc.tokenCount(); got != 2 {
		t.Errorf("token requests = %d, want 2", got)
	}
}

// TestPersistent401AfterReauth asserts a request still rejected after one
// re-authentication fails as a 401, without a second re-authentication.
func TestPersistent401AfterReauth(t *testing.T) {
	fc := newFakeCloud(t)
	fc.networks = func(string) (int, string) { return http.StatusUnauthorized, unauthorizedBody }
	client := networkClient(t, fc)

	err := listNetworks(t.Context(), client)
	if !errors.As(err, new(*gophercloud.ErrErrorAfterReauthentication)) {
		t.Errorf("list error = %v, want an ErrErrorAfterReauthentication", err)
	}
	var code gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &code) || code.Actual != http.StatusUnauthorized {
		t.Errorf("list error = %v, want it to unwrap to a 401 response", err)
	}
	if got := fc.tokenCount(); got != 2 {
		t.Errorf("token requests = %d, want 2", got)
	}
}

// TestFailedReauthIsReported asserts a re-authentication Keystone rejects fails
// the request with the re-authentication error, which is not a response code.
func TestFailedReauthIsReported(t *testing.T) {
	fc := newFakeCloud(t)
	fc.maxTokens = 1
	fc.networks = func(token string) (int, string) {
		if token == "tok-1" {
			return http.StatusUnauthorized, unauthorizedBody
		}
		return http.StatusOK, `{"networks":[]}`
	}
	client := networkClient(t, fc)

	err := listNetworks(t.Context(), client)
	if !errors.As(err, new(*gophercloud.ErrUnableToReauthenticate)) {
		t.Errorf("list error = %v, want an ErrUnableToReauthenticate", err)
	}
	if errors.As(err, new(gophercloud.ErrUnexpectedResponseCode)) {
		t.Errorf("list error = %v unwraps to a response code, want none", err)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "Unable to re-authenticate:") {
		t.Errorf("list error = %v, want it to start with %q", err, "Unable to re-authenticate:")
	}
	if got := fc.tokenCount(); got != 2 {
		t.Errorf("token requests = %d, want 2", got)
	}
}

// TestPreIssuedTokenDisablesReauth asserts an entry that authenticates with a
// pre-issued token still authenticates, without re-authentication, which
// gophercloud refuses for an unscoped token.
func TestPreIssuedTokenDisablesReauth(t *testing.T) {
	fc := newFakeCloud(t)
	writeClouds(t, "auth_url: "+fc.authURL()+"\ntoken: pre-issued\n")

	client, err := NewNetworkClient(t.Context(), "fake")
	if err != nil {
		t.Fatalf("NewNetworkClient: %v", err)
	}
	if client.ReauthFunc != nil {
		t.Error("a pre-issued token built a provider client that re-authenticates")
	}
}

// TestUnreachableKeystoneFailsProviderCreation asserts a Keystone nobody answers
// fails the constructor with the wrapped dial error and no client.
func TestUnreachableKeystoneFailsProviderCreation(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	writeClouds(t, credentialAuth(dead.URL+"/v3"))

	client, err := NewNetworkClient(t.Context(), "fake")
	if client != nil {
		t.Errorf("expected nil client on error, got %v", client)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "creating provider client:") {
		t.Fatalf("error = %v, want it to start with %q", err, "creating provider client:")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" {
		t.Errorf("error = %v, want it to wrap the dial error", err)
	}
}
