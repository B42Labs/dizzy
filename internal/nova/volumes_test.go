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

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// TestAttachVolume verifies an accepted attach succeeds, a repeated attach that
// Nova rejects because the volume is already attached is success only when the
// server has the volume's attachment, a failed lookup surfaces its own error so
// a transient one stays retryable, and every other rejection surfaces wrapped
// with the logical names.
func TestAttachVolume(t *testing.T) {
	srv := resource.Resource{Kind: KindServer, ID: "srv-1", Logical: "srv-0001"}
	vol := resource.Resource{Kind: KindVolume, ID: "vol-1", Logical: "vol-0001"}

	tests := []struct {
		name       string
		postStatus int
		postBody   string
		getStatus  int // the answer to the attachment lookup after an "already attached" 400
		wantErr    bool
		retryable  bool
	}{
		{name: "accepted", postStatus: http.StatusOK, postBody: `{"volumeAttachment":{"id":"vol-1","volumeId":"vol-1","serverId":"srv-1","device":"/dev/vdb"}}`},
		{name: "already attached to the server", postStatus: http.StatusBadRequest, postBody: `{"badRequest":{"code":400,"message":"Invalid volume: volume vol-1 already attached"}}`, getStatus: http.StatusOK},
		{name: "already attached to another server", postStatus: http.StatusBadRequest, postBody: `{"badRequest":{"code":400,"message":"Invalid volume: volume vol-1 is already attached to instances: srv-2"}}`, getStatus: http.StatusNotFound, wantErr: true},
		{name: "already attached, lookup unavailable", postStatus: http.StatusBadRequest, postBody: `{"badRequest":{"code":400,"message":"Invalid volume: volume vol-1 already attached"}}`, getStatus: http.StatusServiceUnavailable, wantErr: true, retryable: true},
		{name: "another bad request", postStatus: http.StatusBadRequest, postBody: `{"badRequest":{"code":400,"message":"Invalid volume: volume vol-1 status must be available"}}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/servers/srv-1/os-volume_attachments":
					w.WriteHeader(tc.postStatus)
					_, _ = w.Write([]byte(tc.postBody))
				case r.Method == http.MethodGet && r.URL.Path == "/servers/srv-1/os-volume_attachments/vol-1" && tc.getStatus != 0:
					w.WriteHeader(tc.getStatus)
					if tc.getStatus == http.StatusOK {
						_, _ = w.Write([]byte(`{"volumeAttachment":{"id":"vol-1","volumeId":"vol-1","serverId":"srv-1","device":"/dev/vdb"}}`))
					}
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer ts.Close()
			err := testComputeClient(ts).AttachVolume(context.Background(), srv, vol)
			if !tc.wantErr {
				if err != nil {
					t.Errorf("AttachVolume = %v, want nil", err)
				}
				return
			}
			if want := `attaching volume "vol-0001" to server "srv-0001":`; err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("AttachVolume = %v, want an error starting with %q", err, want)
			}
			if got := IsRetryable(err); got != tc.retryable {
				t.Errorf("IsRetryable(%v) = %v, want %v", err, got, tc.retryable)
			}
		})
	}
}

// TestWaitForVolumeDetached verifies the wait polls the volume's attachment
// until a 404, that a 200 and an error are polled again, that it gives up with
// the context's error, and that it records no metrics sample.
func TestWaitForVolumeDetached(t *testing.T) {
	srv := resource.Resource{Kind: KindServer, ID: "srv-1", Logical: "srv-0001"}
	vol := resource.Resource{Kind: KindVolume, ID: "vol-1", Logical: "vol-0001"}

	tests := []struct {
		name      string
		statuses  []int // status per attachment GET; the last one repeats
		timeout   time.Duration
		cancelled bool // cancel the context before the call
		wantErr   error
		wantCalls int32 // 0 skips the check
	}{
		{name: "first answer is a 404", statuses: []int{http.StatusNotFound}, timeout: 5 * time.Second, wantCalls: 1},
		{name: "polls through a 200 and a 500", statuses: []int{http.StatusOK, http.StatusInternalServerError, http.StatusNotFound}, timeout: 5 * time.Second, wantCalls: 3},
		{name: "still attached at the deadline", statuses: []int{http.StatusOK}, timeout: 300 * time.Millisecond, wantErr: context.DeadlineExceeded},
		{name: "a cancelled context ends it", statuses: []int{http.StatusOK}, timeout: 5 * time.Second, cancelled: true, wantErr: context.Canceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/servers/srv-1/os-volume_attachments/vol-1" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				status := tc.statuses[min(int(calls.Add(1))-1, len(tc.statuses)-1)]
				w.WriteHeader(status)
				if status == http.StatusOK {
					_, _ = w.Write([]byte(`{"volumeAttachment":{"id":"vol-1","volumeId":"vol-1","serverId":"srv-1","device":"/dev/vdb"}}`))
				}
			}))
			t.Cleanup(ts.Close)

			m := metrics.NewCollector()
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			err := testComputeNetworkClient(ts, m).WaitForVolumeDetached(ctx, srv, vol)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("WaitForVolumeDetached = %v, want %v", err, tc.wantErr)
			}
			if tc.wantCalls != 0 && calls.Load() != tc.wantCalls {
				t.Errorf("polled the attachment %d times, want %d", calls.Load(), tc.wantCalls)
			}
			if attempted, _, _ := m.Snapshot(); attempted != 0 {
				t.Errorf("recorded %d samples, want none", attempted)
			}
		})
	}
}
