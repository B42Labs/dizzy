package cinder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// respErr builds a gophercloud unexpected-response error for the given status
// and body, the shape the classifiers branch on.
func respErr(status int, body string) error {
	return gophercloud.ErrUnexpectedResponseCode{Actual: status, Body: []byte(body)}
}

// testServiceClient builds a Client whose gophercloud block-storage calls hit ts.
func testServiceClient(ts *httptest.Server) *Client {
	return testServiceClientWith(ts, metrics.NewCollector())
}

// testServiceClientWith builds a Client whose gophercloud block-storage calls
// hit ts and whose samples go to m, so a test can read back what a call
// recorded.
func testServiceClientWith(ts *httptest.Server, m *metrics.Collector) *Client {
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	return New(gc, "run0", m)
}

// wantOneSample fails the test unless m holds exactly one sample, of type typ,
// that succeeded when errKind is empty and otherwise failed and classified to
// errKind, so the error tally is empty or holds errKind once.
func wantOneSample(t *testing.T, m *metrics.Collector, typ, errKind string) {
	t.Helper()
	succeeded := 1
	var errs []metrics.ErrorCount
	if errKind != "" {
		succeeded = 0
		errs = []metrics.ErrorCount{{Kind: errKind, Count: 1}}
	}
	agg := m.Aggregate(0)
	if o := agg.Overall; o.Attempted != 1 || o.Succeeded != succeeded || o.Failed != 1-succeeded {
		t.Errorf("overall attempted/succeeded/failed = %d/%d/%d, want 1/%d/%d",
			o.Attempted, o.Succeeded, o.Failed, succeeded, 1-succeeded)
	}
	if len(agg.ByType) != 1 || agg.ByType[0].Type != typ ||
		agg.ByType[0].Attempted != 1 || agg.ByType[0].Succeeded != succeeded {
		t.Errorf("byType = %+v, want one %s sample with %d succeeded", agg.ByType, typ, succeeded)
	}
	if !slices.Equal(agg.Errors, errs) {
		t.Errorf("errors = %v, want %v", agg.Errors, errs)
	}
}

// TestExtendVolumeAlreadyApplied confirms a retried extend whose first request
// already committed — Cinder then rejects the re-issue with a 400 "new size ...
// must be greater than current size" — is treated as success, so an idempotent
// retry does not fail an otherwise-correct run. A 400 for any other reason still
// fails. Each case also checks the one sample the call records: the confirmed
// 400 and an accepted extend record a succeeded volume sample with no error
// kind, another 400 keeps its http_400, and a 413 records as a quota rejection
// and wraps ErrQuota.
func TestExtendVolumeAlreadyApplied(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantErr   bool
		wantQuota bool   // whether errors.Is(err, ErrQuota) must hold
		wantKind  string // the recorded error kind, "" for a success
	}{
		{
			name:    "size not greater than current is success",
			status:  http.StatusBadRequest,
			body:    `{"badRequest":{"code":400,"message":"New size for extend must be greater than current size."}}`,
			wantErr: false,
		},
		{
			name:     "other 400 still fails",
			status:   http.StatusBadRequest,
			body:     `{"badRequest":{"code":400,"message":"Invalid volume: some other reason"}}`,
			wantErr:  true,
			wantKind: "http_400",
		},
		{
			name:    "accepted extend is success",
			status:  http.StatusAccepted,
			wantErr: false,
		},
		{
			name:      "over quota fails with ErrQuota",
			status:    http.StatusRequestEntityTooLarge,
			body:      `{"overLimit":{"code":413,"message":"VolumeSizeExceedsAvailableQuota: Requested volume or snapshot exceeds allowed gigabytes quota."}}`,
			wantErr:   true,
			wantQuota: true,
			wantKind:  "quota",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer ts.Close()

			m := metrics.NewCollector()
			c := testServiceClientWith(ts, m)
			err := c.ExtendVolume(context.Background(),
				resource.Resource{Kind: KindVolume, Logical: "vol-0001", ID: "vol-1"}, 5)
			if tc.wantErr && err == nil {
				t.Fatal("ExtendVolume: expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ExtendVolume: expected success for an already-applied extend, got %v", err)
			}
			if got := errors.Is(err, ErrQuota); got != tc.wantQuota {
				t.Errorf("errors.Is(err, ErrQuota) = %v, want %v (err %v)", got, tc.wantQuota, err)
			}
			wantOneSample(t, m, string(KindVolume), tc.wantKind)
		})
	}
}

// TestListVolumesByMetadataClientSideBackstop simulates a backend that ignores
// the server-side metadata filter and returns every volume in the project (an
// older release, a proxy stripping the query param, a microversion mismatch).
// The client must still drop the volumes that do not carry this run's metadata,
// so cleanup never deletes a volume the tool did not tag.
func TestListVolumesByMetadataClientSideBackstop(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"volumes":[
			{"id":"v-mine","name":"dizzy-run0-vol-0001","metadata":{"dizzy:run":"run0"}},
			{"id":"v-theirs","name":"another-run","metadata":{"dizzy:run":"run9"}},
			{"id":"v-untagged","name":"untagged","metadata":{}}
		]}`)
	}))
	defer ts.Close()

	got, err := testServiceClient(ts).ListVolumesByMetadata(context.Background(), "run0")
	if err != nil {
		t.Fatalf("ListVolumesByMetadata: %v", err)
	}
	if len(got) != 1 || got[0].ID != "v-mine" {
		t.Fatalf("ListVolumesByMetadata returned %+v, want only the v-mine volume this run tagged", got)
	}
}

// TestListSnapshotsByMetadataFiltersClientSide confirms the streamed, unfiltered
// snapshot listing keeps only this run's snapshots — the client-side filter the
// cleanup path relies on, since gophercloud's snapshot ListOpts has no metadata
// field to filter on server-side.
func TestListSnapshotsByMetadataFiltersClientSide(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"snapshots":[
			{"id":"s-mine","name":"dizzy-run0-snap-0001","metadata":{"dizzy:run":"run0"}},
			{"id":"s-theirs","name":"another-run","metadata":{"dizzy:run":"run9"}}
		]}`)
	}))
	defer ts.Close()

	got, err := testServiceClient(ts).ListSnapshotsByMetadata(context.Background(), "run0")
	if err != nil {
		t.Fatalf("ListSnapshotsByMetadata: %v", err)
	}
	if len(got) != 1 || got[0].ID != "s-mine" {
		t.Fatalf("ListSnapshotsByMetadata returned %+v, want only the s-mine snapshot this run tagged", got)
	}
}

// TestListByTypeMetadataVolumesAcrossRuns confirms the type-metadata sweep
// discovers this tool's volumes regardless of their run id — the crash-recovery
// contract, since the sweep runs before a new iteration and must reclaim
// leftovers whose run id is no longer known — and that the client-side backstop
// still drops volumes the tool did not tag with the type even when the server
// ignores the metadata filter.
func TestListByTypeMetadataVolumesAcrossRuns(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"volumes":[
			{"id":"v-run0","name":"dizzy-run0-vol-0001","metadata":{"dizzy:run":"run0","dizzy:type":"volume"}},
			{"id":"v-run9","name":"dizzy-run9-vol-0001","metadata":{"dizzy:run":"run9","dizzy:type":"volume"}},
			{"id":"v-untagged","name":"untagged","metadata":{}},
			{"id":"v-othertype","name":"dizzy-run0-snap","metadata":{"dizzy:type":"snapshot"}}
		]}`)
	}))
	defer ts.Close()

	got, err := testServiceClient(ts).ListByTypeMetadata(context.Background(), KindVolume)
	if err != nil {
		t.Fatalf("ListByTypeMetadata(volume): %v", err)
	}
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.ID] = true
	}
	if len(got) != 2 || !ids["v-run0"] || !ids["v-run9"] {
		t.Fatalf("ListByTypeMetadata(volume) = %+v, want the two type-tagged volumes across both runs", got)
	}
}

// TestListByTypeMetadataSnapshotsFiltersClientSide confirms the snapshot sweep
// keeps only the type-tagged snapshots via the unfiltered ListDetail path, the
// client-side filter cleanup relies on since snapshots.ListOpts has no metadata
// field to filter on server-side.
func TestListByTypeMetadataSnapshotsFiltersClientSide(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"snapshots":[
			{"id":"s-run0","name":"dizzy-run0-snap-0001","metadata":{"dizzy:run":"run0","dizzy:type":"snapshot"}},
			{"id":"s-run9","name":"dizzy-run9-snap-0001","metadata":{"dizzy:run":"run9","dizzy:type":"snapshot"}},
			{"id":"s-foreign","name":"someone-elses","metadata":{}}
		]}`)
	}))
	defer ts.Close()

	got, err := testServiceClient(ts).ListByTypeMetadata(context.Background(), KindSnapshot)
	if err != nil {
		t.Fatalf("ListByTypeMetadata(snapshot): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByTypeMetadata(snapshot) = %+v, want the two type-tagged snapshots, dropping the untagged one", got)
	}
}

// TestListByTypeMetadataUnsupportedKind confirms a kind Cinder does not create is
// rejected rather than silently returning nothing, mirroring the status/delete
// switches.
func TestListByTypeMetadataUnsupportedKind(t *testing.T) {
	c := New(&gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}}, "run0", metrics.NewCollector())
	if _, err := c.ListByTypeMetadata(context.Background(), resource.Kind("router")); err == nil {
		t.Fatal("ListByTypeMetadata for an unsupported kind: expected an error, got nil")
	}
}

// TestResourceNameAndMetadata checks names and metadata are deterministic and
// exact.
func TestResourceNameAndMetadata(t *testing.T) {
	if got := resourceName("abcd1234", "vol-0001"); got != "dizzy-abcd1234-vol-0001" {
		t.Errorf("resourceName = %q", got)
	}

	meta := runMetadata("abcd1234", KindVolume)
	if meta[metaRun] != "abcd1234" {
		t.Errorf("metadata[%q] = %q, want abcd1234", metaRun, meta[metaRun])
	}
	if meta[metaType] != "volume" {
		t.Errorf("metadata[%q] = %q, want volume", metaType, meta[metaType])
	}
	if len(meta) != 2 {
		t.Errorf("metadata has %d keys, want 2", len(meta))
	}
}

// TestIsErrorStatus covers the terminal-status predicate: available is ready,
// anything with the error prefix is terminal, transient states are not.
func TestIsErrorStatus(t *testing.T) {
	cases := map[string]bool{
		"available":       false,
		"creating":        false,
		"extending":       false,
		"error":           true,
		"error_extending": true,
		"error_deleting":  true,
	}
	for status, want := range cases {
		if got := isErrorStatus(status); got != want {
			t.Errorf("isErrorStatus(%q) = %v, want %v", status, got, want)
		}
	}
}

// TestClassification pins the retry and quota classification that drives the
// executor's retry/fail-fast decisions, plus the metrics error-kind labels.
// Cinder signals over-quota with HTTP 413, unlike Neutron's 409/403.
func TestClassification(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		retryable   bool
		quota       bool
		wantErrKind string
	}{
		{"nil", nil, false, false, ""},
		{"500", respErr(500, ""), true, false, "http_500"},
		{"503", respErr(503, ""), true, false, "http_503"},
		{"429", respErr(429, ""), true, false, "http_429"},
		{"409 conflict", respErr(409, "in use"), true, false, "http_409"},
		{"413 over limit", respErr(413, "VolumeSizeExceedsAvailableQuota"), false, true, "quota"},
		{"413 empty body still quota", respErr(413, ""), false, true, "quota"},
		{"403 quota body", respErr(403, "Quota exceeded for volumes"), false, true, "quota"},
		{"403 non-quota", respErr(403, "not authorized"), false, false, "http_403"},
		{"400 quota body", respErr(400, "exceeds allowed gigabytes"), false, true, "quota"},
		{"404", respErr(404, ""), false, false, "http_404"},
		{"418 collapses to http_other", respErr(418, ""), false, false, "http_other"},
		{"timeout", context.DeadlineExceeded, false, false, "timeout"},
		{"canceled", context.Canceled, false, false, "canceled"},
		{"transport", errors.New("connection refused"), false, false, "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.retryable {
				t.Errorf("IsRetryable = %v, want %v", got, tc.retryable)
			}
			if got := isQuota(tc.err); got != tc.quota {
				t.Errorf("isQuota = %v, want %v", got, tc.quota)
			}
			if got := errKind(tc.err); got != tc.wantErrKind {
				t.Errorf("errKind = %q, want %q", got, tc.wantErrKind)
			}
		})
	}
}

// TestIsNotFound confirms a 404 (and only a 404) is treated as not-found.
func TestIsNotFound(t *testing.T) {
	if !IsNotFound(respErr(404, "")) {
		t.Error("IsNotFound(404) = false, want true")
	}
	if IsNotFound(respErr(500, "")) {
		t.Error("IsNotFound(500) = true, want false")
	}
	if IsNotFound(nil) {
		t.Error("IsNotFound(nil) = true, want false")
	}
}

// TestWrapCreateQuota verifies a 413 quota error is wrapped so errors.Is finds
// ErrQuota (for the executor's fail-fast) while the underlying response error
// stays in the chain (so classification still sees the 413), and that a
// non-quota error is neither.
func TestWrapCreateQuota(t *testing.T) {
	wrapped := wrapCreate(KindVolume, "vol-0001", respErr(413, "VolumeLimitExceeded"))
	if !errors.Is(wrapped, ErrQuota) {
		t.Errorf("wrapped quota error does not match ErrQuota: %v", wrapped)
	}
	if IsRetryable(wrapped) {
		t.Error("wrapped quota error must not be retryable")
	}

	plain := wrapCreate(KindVolume, "vol-0002", respErr(503, ""))
	if errors.Is(plain, ErrQuota) {
		t.Error("non-quota error must not match ErrQuota")
	}
	if !IsRetryable(plain) {
		t.Error("wrapped 503 error should remain retryable")
	}
}
