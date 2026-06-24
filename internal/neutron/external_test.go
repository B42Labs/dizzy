package neutron

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

// externalNetworksServer serves a fixed network list (two external, one
// internal) at /networks and asserts the router:external filter is sent.
func externalNetworksServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/networks" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("router:external"); got != "true" {
			t.Errorf("router:external filter = %q, want true", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"networks":[
			{"id":"ext-b","name":"public-b","router:external":true},
			{"id":"ext-a","name":"public-a","router:external":true},
			{"id":"int-1","name":"internal","router:external":false}
		]}`)
	}))
}

func gcForServer(ts *httptest.Server) *gophercloud.ServiceClient {
	return &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
}

// TestFindExternalNetworkAutoDetect confirms auto-detection (empty name) picks
// the first external network in name order and ignores internal networks.
func TestFindExternalNetworkAutoDetect(t *testing.T) {
	ts := externalNetworksServer(t)
	defer ts.Close()

	got, ok, err := FindExternalNetwork(context.Background(), gcForServer(ts), "")
	if err != nil {
		t.Fatalf("FindExternalNetwork: %v", err)
	}
	if !ok {
		t.Fatal("FindExternalNetwork ok = false, want an external network")
	}
	if got.ID != "ext-a" || got.Name != "public-a" {
		t.Errorf("auto-detected %+v, want the name-sorted first external (ext-a/public-a)", got)
	}
}

// TestFindExternalNetworkByName confirms a named lookup returns that network,
// and that a name with no match is a hard error.
func TestFindExternalNetworkByName(t *testing.T) {
	ts := externalNetworksServer(t)
	defer ts.Close()
	gc := gcForServer(ts)

	got, ok, err := FindExternalNetwork(context.Background(), gc, "public-b")
	if err != nil || !ok {
		t.Fatalf("FindExternalNetwork(public-b) = (%+v, %v, %v), want it found", got, ok, err)
	}
	if got.ID != "ext-b" {
		t.Errorf("named lookup got %+v, want ext-b", got)
	}

	if _, _, err := FindExternalNetwork(context.Background(), gc, "does-not-exist"); err == nil {
		t.Error("FindExternalNetwork with an unknown name: expected an error, got nil")
	}
}

// TestFindExternalNetworkNoneAvailable confirms a cloud with no external network
// returns ok=false without an error, so the caller can proceed without external
// connectivity.
func TestFindExternalNetworkNoneAvailable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"networks":[]}`)
	}))
	defer ts.Close()

	_, ok, err := FindExternalNetwork(context.Background(), gcForServer(ts), "")
	if err != nil {
		t.Fatalf("FindExternalNetwork with no external networks returned an error: %v", err)
	}
	if ok {
		t.Error("FindExternalNetwork ok = true with no external networks, want false")
	}
}
