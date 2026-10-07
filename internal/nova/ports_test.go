package nova

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/resource"
)

// TestWaitForPortDetached verifies the wait returns on the first 404 of the
// interface attachment, polls through a 200 and a 500, and gives up with the
// context's error while the port stays attached.
func TestWaitForPortDetached(t *testing.T) {
	srv := resource.Resource{Kind: KindServer, ID: "srv-1", Logical: "srv-0001"}
	port := resource.Resource{Kind: KindPort, ID: "port-1", Logical: "port-0001"}
	attached := `{"interfaceAttachment":{"port_id":"port-1","port_state":"ACTIVE"}}`

	tests := []struct {
		name      string
		answers   []int // status per GET; the last one repeats
		timeout   time.Duration
		wantErr   error
		wantCalls int32 // 0 skips the check
	}{
		{name: "first answer is a 404", answers: []int{http.StatusNotFound}, timeout: 5 * time.Second, wantCalls: 1},
		{name: "polls through a 200 and a 500", answers: []int{http.StatusOK, http.StatusInternalServerError, http.StatusNotFound}, timeout: 5 * time.Second, wantCalls: 3},
		{name: "still attached at the deadline", answers: []int{http.StatusOK}, timeout: 300 * time.Millisecond, wantErr: context.DeadlineExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/servers/srv-1/os-interface/port-1" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				n := int(calls.Add(1)) - 1
				status := tc.answers[min(n, len(tc.answers)-1)]
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status == http.StatusOK {
					_, _ = w.Write([]byte(attached))
				}
			}))
			t.Cleanup(ts.Close)

			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			err := testComputeClient(ts).WaitForPortDetached(ctx, srv, port)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("WaitForPortDetached = %v, want %v", err, tc.wantErr)
			}
			if tc.wantCalls != 0 && calls.Load() != tc.wantCalls {
				t.Errorf("polled %d times, want %d", calls.Load(), tc.wantCalls)
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
