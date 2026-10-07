package nova

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// testComputeNetworkClient builds a Client whose Nova (compute) and Neutron
// (network) service calls both hit ts and record into m. The port detach wait
// reads both services, so its interface, server, and port paths land on one
// handler.
func testComputeNetworkClient(ts *httptest.Server, m *metrics.Collector) *Client {
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	return New(gc, gc, nil, "run0", m)
}

// TestWaitForPortDetached verifies the wait polls the interface attachment
// until a 404, then the server until its addresses no longer carry the port's
// MAC or any of its fixed IPs read from Neutron; that a port gone from Neutron
// or a server gone ends it, that errors are polled again, that it gives up
// with the context's error, and that it records no metrics sample. A path
// whose script is empty must not be requested. The cases run serially because
// one of them reads the process-wide default logger.
func TestWaitForPortDetached(t *testing.T) {
	srv := resource.Resource{Kind: KindServer, ID: "srv-1", Logical: "srv-0001"}
	port := resource.Resource{Kind: KindPort, ID: "port-1", Logical: "port-0001"}
	attached := `{"interfaceAttachment":{"port_id":"port-1","port_state":"ACTIVE"}}`
	neutronPort := `{"port":{"id":"port-1","mac_address":"fa:16:3e:aa:bb:cc","fixed_ips":[{"subnet_id":"sub-1","ip_address":"10.0.0.5"},{"subnet_id":"sub-6","ip_address":"fd00::5"}]}}`
	addresses := map[string]string{
		"carrying":     `{"net":[{"addr":"10.0.0.5","version":4,"OS-EXT-IPS-MAC:mac_addr":"fa:16:3e:aa:bb:cc","OS-EXT-IPS:type":"fixed"}]}`,
		"by-ip":        `{"net":[{"addr":"10.0.0.5","version":4,"OS-EXT-IPS:type":"fixed"}]}`,
		"by-second-ip": `{"net":[{"addr":"fd00::5","version":6,"OS-EXT-IPS:type":"fixed"}]}`,
		"unrelated":    `{"net":[{"addr":"10.0.0.9","version":4,"OS-EXT-IPS-MAC:mac_addr":"fa:16:3e:00:00:01","OS-EXT-IPS:type":"fixed"}]}`,
		"empty":        `{}`,
	}
	ok := []int{http.StatusOK}
	notFound := []int{http.StatusNotFound}

	tests := []struct {
		name    string
		iface   []int    // status per interface GET; the last one repeats
		neutron []int    // status per Neutron port GET; the last one repeats
		server  []string // per server GET, a status or a key of addresses; the last one repeats
		timeout time.Duration
		wantErr error
		// calls per path; 0 skips the check
		wantIface, wantNeutron, wantServer int32
		wantLog                            string // a line the wait logs, "" for none
	}{
		{name: "first answer is a 404", iface: notFound, neutron: ok, server: []string{"empty"}, timeout: 5 * time.Second, wantIface: 1, wantNeutron: 1, wantServer: 1},
		{name: "polls through a 200 and a 500", iface: []int{http.StatusOK, http.StatusInternalServerError, http.StatusNotFound}, neutron: ok, server: []string{"empty"}, timeout: 5 * time.Second, wantIface: 3},
		{name: "still attached at the deadline", iface: ok, timeout: 300 * time.Millisecond, wantErr: context.DeadlineExceeded},
		{name: "addresses still carry the port by MAC", iface: notFound, neutron: ok, server: []string{"carrying", "carrying", "empty"}, timeout: 5 * time.Second, wantServer: 3},
		{name: "addresses carry the port by fixed IP only", iface: notFound, neutron: ok, server: []string{"by-ip", "empty"}, timeout: 5 * time.Second, wantServer: 2},
		{name: "addresses carry the port by its second fixed IP", iface: notFound, neutron: ok, server: []string{"by-second-ip", "empty"}, timeout: 5 * time.Second, wantServer: 2},
		{name: "another port's address does not hold the wait", iface: notFound, neutron: ok, server: []string{"unrelated"}, timeout: 5 * time.Second, wantServer: 1},
		{name: "addresses never drop the port", iface: notFound, neutron: ok, server: []string{"carrying"}, timeout: 300 * time.Millisecond, wantErr: context.DeadlineExceeded},
		{name: "the Neutron port is gone", iface: notFound, neutron: notFound, timeout: 5 * time.Second, wantNeutron: 1,
			wantLog: `level=INFO msg="port gone from Neutron; nothing left to wait for" port=port-0001 id=port-1`},
		{name: "a Neutron error is polled again", iface: notFound, neutron: []int{http.StatusInternalServerError, http.StatusOK}, server: []string{"empty"}, timeout: 5 * time.Second, wantNeutron: 2, wantServer: 1},
		{name: "the server is gone", iface: notFound, neutron: ok, server: []string{"404"}, timeout: 5 * time.Second, wantServer: 1},
		{name: "a server error is polled again", iface: notFound, neutron: ok, server: []string{"500", "empty"}, timeout: 5 * time.Second, wantServer: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			if tc.wantLog != "" {
				prev := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(prev) })
			}

			var ifaceCalls, neutronCalls, serverCalls atomic.Int32
			// next counts a call and returns its index into a script of n
			// entries, the last entry repeating.
			next := func(calls *atomic.Int32, n int) int { return min(int(calls.Add(1))-1, n-1) }
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/servers/srv-1/os-interface/port-1" && len(tc.iface) > 0:
					status := tc.iface[next(&ifaceCalls, len(tc.iface))]
					w.WriteHeader(status)
					if status == http.StatusOK {
						_, _ = w.Write([]byte(attached))
					}
				case r.Method == http.MethodGet && r.URL.Path == "/ports/port-1" && len(tc.neutron) > 0:
					status := tc.neutron[next(&neutronCalls, len(tc.neutron))]
					w.WriteHeader(status)
					if status == http.StatusOK {
						_, _ = w.Write([]byte(neutronPort))
					}
				case r.Method == http.MethodGet && r.URL.Path == "/servers/srv-1" && len(tc.server) > 0:
					answer := tc.server[next(&serverCalls, len(tc.server))]
					if status, err := strconv.Atoi(answer); err == nil {
						w.WriteHeader(status)
						return
					}
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"server":{"id":"srv-1","status":"ACTIVE","addresses":` + addresses[answer] + `}}`))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			t.Cleanup(ts.Close)

			m := metrics.NewCollector()
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			err := testComputeNetworkClient(ts, m).WaitForPortDetached(ctx, srv, port)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("WaitForPortDetached = %v, want %v", err, tc.wantErr)
			}
			for _, c := range []struct {
				path        string
				calls, want int32
			}{
				{"interface", ifaceCalls.Load(), tc.wantIface},
				{"Neutron port", neutronCalls.Load(), tc.wantNeutron},
				{"server", serverCalls.Load(), tc.wantServer},
			} {
				if c.want != 0 && c.calls != c.want {
					t.Errorf("polled the %s %d times, want %d", c.path, c.calls, c.want)
				}
			}
			if attempted, _, _ := m.Snapshot(); attempted != 0 {
				t.Errorf("recorded %d samples, want none", attempted)
			}
			if tc.wantLog != "" && !strings.Contains(logs.String(), tc.wantLog) {
				t.Errorf("logs lack %s:\n%s", tc.wantLog, logs.String())
			}
		})
	}
}

// TestAttachPort verifies an accepted attach succeeds, a repeated attach that
// Nova rejects with a 409 is success when the server has the port's interface
// attachment, and that the 409 surfaces wrapped, still retryable, when it does
// not.
func TestAttachPort(t *testing.T) {
	srv := resource.Resource{Kind: KindServer, ID: "srv-1", Logical: "srv-0001"}
	port := resource.Resource{Kind: KindPort, ID: "port-1", Logical: "port-0001"}

	tests := []struct {
		name       string
		postStatus int
		getStatus  int // the answer to the interface lookup after a 409
		wantErr    bool
	}{
		{name: "accepted", postStatus: http.StatusOK},
		{name: "already attached to the server", postStatus: http.StatusConflict, getStatus: http.StatusOK},
		{name: "in use without the server having it", postStatus: http.StatusConflict, getStatus: http.StatusNotFound, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/servers/srv-1/os-interface":
					w.WriteHeader(tc.postStatus)
					if tc.postStatus == http.StatusConflict {
						_, _ = w.Write([]byte(`{"conflictingRequest":{"code":409,"message":"Port port-1 is still in use."}}`))
						return
					}
					_, _ = w.Write([]byte(`{"interfaceAttachment":{"port_id":"port-1","port_state":"ACTIVE"}}`))
				case r.Method == http.MethodGet && r.URL.Path == "/servers/srv-1/os-interface/port-1":
					w.WriteHeader(tc.getStatus)
					if tc.getStatus == http.StatusOK {
						_, _ = w.Write([]byte(`{"interfaceAttachment":{"port_id":"port-1","port_state":"ACTIVE"}}`))
					}
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer ts.Close()
			err := testComputeClient(ts).AttachPort(context.Background(), srv, port)
			if !tc.wantErr {
				if err != nil {
					t.Errorf("AttachPort = %v, want nil", err)
				}
				return
			}
			if want := `attaching port "port-0001" to server "srv-0001":`; err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("AttachPort = %v, want an error starting with %q", err, want)
			}
			if !IsRetryable(err) {
				t.Errorf("AttachPort = %v, want the retryable 409", err)
			}
		})
	}
}

// TestAddressesCarry verifies a server's addresses carry a port by its MAC in
// any case or by one of its fixed IPs, and that unrelated entries, entries of
// another shape, and an empty MAC without fixed IPs match nothing.
func TestAddressesCarry(t *testing.T) {
	const mac = "fa:16:3e:aa:bb:cc"
	ips := []string{"10.0.0.5"}
	carrying := map[string]any{"addr": "10.0.0.5", "version": float64(4), "OS-EXT-IPS-MAC:mac_addr": mac, "OS-EXT-IPS:type": "fixed"}
	byMAC := map[string]any{"addr": "10.0.0.7", "version": float64(4), "OS-EXT-IPS-MAC:mac_addr": mac, "OS-EXT-IPS:type": "fixed"}
	upper := map[string]any{"addr": "10.0.0.7", "version": float64(4), "OS-EXT-IPS-MAC:mac_addr": "FA:16:3E:AA:BB:CC", "OS-EXT-IPS:type": "fixed"}
	byIP := map[string]any{"addr": "10.0.0.5", "version": float64(4)}
	unrelated := map[string]any{"addr": "10.0.0.9", "version": float64(4), "OS-EXT-IPS-MAC:mac_addr": "fa:16:3e:00:00:01", "OS-EXT-IPS:type": "fixed"}
	macOnly := map[string]any{"OS-EXT-IPS-MAC:mac_addr": mac}
	emptyMAC := map[string]any{"OS-EXT-IPS-MAC:mac_addr": ""}

	tests := []struct {
		name      string
		addresses map[string]any
		mac       string
		ips       []string
		want      bool
	}{
		{name: "nil map", mac: mac, ips: ips},
		{name: "empty map", addresses: map[string]any{}, mac: mac, ips: ips},
		{name: "an entry matching by MAC", addresses: map[string]any{"net": []any{byMAC}}, mac: mac, ips: ips, want: true},
		{name: "an entry matching by MAC in upper case", addresses: map[string]any{"net": []any{upper}}, mac: mac, ips: ips, want: true},
		{name: "an entry matching by fixed IP without a MAC key", addresses: map[string]any{"net": []any{byIP}}, mac: mac, ips: ips, want: true},
		{name: "an unrelated entry", addresses: map[string]any{"net": []any{unrelated}}, mac: mac, ips: ips},
		{name: "a network value that is not a slice", addresses: map[string]any{"net": "10.0.0.5"}, mac: mac, ips: ips},
		{name: "an entry that is not a map", addresses: map[string]any{"net": []any{"10.0.0.5"}}, mac: mac, ips: ips},
		{name: "an empty mac against an entry carrying only a MAC", addresses: map[string]any{"net": []any{macOnly}}, ips: ips},
		{name: "an empty mac against an entry with an empty MAC", addresses: map[string]any{"net": []any{emptyMAC}}, ips: ips},
		{name: "an empty mac and no ips against a carrying entry", addresses: map[string]any{"net": []any{carrying}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := addressesCarry(tc.addresses, tc.mac, tc.ips); got != tc.want {
				t.Errorf("addressesCarry = %v, want %v", got, tc.want)
			}
		})
	}
}
