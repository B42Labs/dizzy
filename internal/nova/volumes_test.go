package nova

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// testBlockStorageClient builds a Client whose Cinder (block-storage) service
// calls hit ts. Only the data volume calls are exercised with it, so the compute
// and network clients are left nil.
func testBlockStorageClient(ts *httptest.Server) *Client {
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	return New(nil, nil, gc, "run0", metrics.NewCollector())
}

// cinderVolumes is the createAPI of Cinder volumes. Cinder writes its times
// without a zone, the form gophercloud parses.
var cinderVolumes = createAPI{
	create: "/volumes", list: "/volumes/detail", one: "volume", many: "volumes",
	prefix: "vol", status: "creating", createdKey: "created_at", createdAt: "2026-01-01T00:00:00.000000",
}

// TestCreateVolumeAdoptsLostAnswer verifies that a data volume create whose
// answer may have been lost looks the volume up by name, run and create token,
// right after the failure and before the next create of the name, and adopts a
// match without posting again; that otherwise the POST's error is returned; and
// that a quota refusal looks nothing up. The fake names each create by the
// token it carries.
func TestCreateVolumeAdoptsLostAnswer(t *testing.T) {
	const name = "dizzy-run0-vol-0001"
	vol := novaplan.Volume{Name: "vol-0001", SizeGiB: 1}
	adopted := func(id string) resource.Resource {
		return resource.Resource{Kind: "volume", Logical: "vol-0001", Name: name, ID: id}
	}
	create := func(c *Client) (resource.Resource, error) {
		return c.CreateVolume(context.Background(), vol)
	}

	t.Run("answer lost after commit", func(t *testing.T) {
		f := newLostAnswerFake(t, cinderVolumes, map[string]createAnswer{"1": answerLostAfterCommit})
		got, err := create(testBlockStorageClient(f.ts))
		if err != nil || got != adopted("vol-1") {
			t.Fatalf("CreateVolume = %+v, %v, want %+v, nil", got, err, adopted("vol-1"))
		}
		f.wantCalls(t, "create 1", "list")
		if names := f.listNames(); !slices.Equal(names, []string{name}) {
			t.Errorf("list name queries = %q, want [%s]", names, name)
		}
	})

	t.Run("answer lost, nothing created", func(t *testing.T) {
		f := newLostAnswerFake(t, cinderVolumes, map[string]createAnswer{"1": answerLost})
		c := testBlockStorageClient(f.ts)
		_, err := create(c)
		if want := `creating volume "vol-0001":`; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("CreateVolume = %v, want an error starting with %q", err, want)
		}
		if !errors.Is(err, io.EOF) {
			t.Errorf("CreateVolume = %v, want it to wrap io.EOF", err)
		}
		f.wantCalls(t, "create 1", "list")

		if _, err := create(c); err != nil {
			t.Fatalf("second CreateVolume = %v, want nil", err)
		}
		f.wantCalls(t, "list", "create 2")
	})

	t.Run("late commit", func(t *testing.T) {
		f := newLostAnswerFake(t, cinderVolumes, map[string]createAnswer{"1": answerLost})
		c := testBlockStorageClient(f.ts)
		if _, err := create(c); err == nil {
			t.Fatal("CreateVolume = nil, want the lost answer's error")
		}
		f.wantCalls(t, "create 1", "list")

		f.add(cinderVolumes.rec("vol-late", name, f.sent(0)))
		got, err := create(c)
		if err != nil || got != adopted("vol-late") {
			t.Fatalf("second CreateVolume = %+v, %v, want %+v, nil", got, err, adopted("vol-late"))
		}
		f.wantCalls(t, "list")
	})

	t.Run("pending lookup fails", func(t *testing.T) {
		f := newLostAnswerFake(t, cinderVolumes, map[string]createAnswer{"1": answerLost},
			http.StatusOK, http.StatusServiceUnavailable)
		c := testBlockStorageClient(f.ts)
		if _, err := create(c); err == nil {
			t.Fatal("CreateVolume = nil, want the lost answer's error")
		}
		f.wantCalls(t, "create 1", "list")

		_, err := create(c)
		if want := "looking up an earlier create whose answer was lost"; err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("second CreateVolume = %v, want an error containing %q", err, want)
		}
		f.wantCalls(t, "list")
	})

	t.Run("the token tells instances apart", func(t *testing.T) {
		f := newLostAnswerFake(t, cinderVolumes, map[string]createAnswer{"1": answerLost})
		f.add(
			cinderVolumes.rec("vol-other-token", name, map[string]string{"dizzy:run": "run0", "dizzy:type": "volume", "dizzy:create": "7"}),
			cinderVolumes.rec("vol-no-metadata", name, nil),
		)
		got, err := create(testBlockStorageClient(f.ts))
		if !errors.Is(err, io.EOF) || got != (resource.Resource{}) {
			t.Fatalf("CreateVolume = %+v, %v, want no adoption and the POST's io.EOF", got, err)
		}
		f.wantCalls(t, "create 1", "list")
	})

	t.Run("a 413 over-limit answer", func(t *testing.T) {
		f := newLostAnswerFake(t, cinderVolumes, map[string]createAnswer{"1": {
			status: http.StatusRequestEntityTooLarge,
			body:   `{"overLimit":{"code":413,"message":"VolumeLimitExceeded: Maximum number of volumes allowed (10) exceeded"}}`,
		}})
		c := testBlockStorageClient(f.ts)
		if _, err := create(c); !errors.Is(err, ErrQuota) {
			t.Fatalf("CreateVolume = %v, want it to wrap ErrQuota", err)
		}
		f.wantCalls(t, "create 1")

		if _, err := create(c); err != nil {
			t.Fatalf("second CreateVolume = %v, want nil", err)
		}
		f.wantCalls(t, "create 2")
	})
}

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
