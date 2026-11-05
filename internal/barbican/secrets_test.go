package barbican

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/secrets"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/payload"
	"github.com/B42Labs/dizzy/internal/resource"
)

func secretRes(id string) resource.Resource {
	return resource.Resource{Kind: KindSecret, Logical: "sec-0001", Name: "dizzy-run0-sec-0001", ID: id}
}

// TestStoreSecret confirms StoreSecret POSTs an opaque secret with the
// deterministic name and a base64 payload of the derived bytes, parses the id
// from the secret_ref Barbican answers with, and records one secret sample.
func TestStoreSecret(t *testing.T) {
	const seed, n = 7, 100
	var body map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/secrets" {
			t.Errorf("request %s %s, want POST /v1/secrets", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding store body: %v", err)
		}
		writeJSON(w, http.StatusCreated, `{"secret_ref":"https://kv/v1/secrets/sec-id"}`)
	}))
	defer ts.Close()
	c, m := testClient(ts)

	got, err := c.StoreSecret(context.Background(), barbicanplan.Secret{Name: "sec-0001", PayloadBytes: n}, seed)
	if err != nil {
		t.Fatalf("StoreSecret: %v", err)
	}
	if want := secretRes("sec-id"); got != want {
		t.Errorf("StoreSecret = %+v, want %+v", got, want)
	}

	for key, want := range map[string]string{
		"name":                     "dizzy-run0-sec-0001",
		"secret_type":              "opaque",
		"payload_content_type":     "application/octet-stream",
		"payload_content_encoding": "base64",
	} {
		if body[key] != want {
			t.Errorf("store body %s = %v, want %q", key, body[key], want)
		}
	}
	encoded, _ := body["payload"].(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("payload %q is not standard base64: %v", encoded, err)
	}
	if !bytes.Equal(decoded, payload.Bytes(seed, n)) {
		t.Errorf("decoded payload (%d bytes) differs from payload.Bytes(%d, %d)", len(decoded), seed, n)
	}

	if st := statsOf(m.Aggregate(time.Second), "secret"); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("secret samples = %+v, want 1 succeeded", st)
	}
}

// TestStoreSecretFailureWarnsOfOrphan confirms a failed store logs the
// deterministic name as a possible orphan, since a secret left behind by a lost
// answer carries no run metadata for discovery to find.
func TestStoreSecretFailureWarnsOfOrphan(t *testing.T) {
	logs := captureLog(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, `{"code":500,"title":"Internal Server Error"}`)
	}))
	defer ts.Close()
	c, _ := testClient(ts)

	_, err := c.StoreSecret(context.Background(), barbicanplan.Secret{Name: "sec-0001", PayloadBytes: 64}, 7)
	if err == nil || !strings.Contains(err.Error(), `creating secret "sec-0001"`) {
		t.Fatalf("StoreSecret error = %v, want one naming the secret", err)
	}
	out := logs.String()
	if !strings.Contains(out, "create failed; a secret with this name may be orphaned in the cloud") {
		t.Errorf("log %q lacks the orphan warning", out)
	}
	if !strings.Contains(out, "name=dizzy-run0-sec-0001") {
		t.Errorf("log %q does not name dizzy-run0-sec-0001", out)
	}
}

// TestStoreSecretQuota confirms a 403 quota rejection threads ErrQuota into the
// error and classifies as quota, while a 413 oversized-payload rejection does
// neither.
func TestStoreSecretQuota(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantQuota bool
		wantKind  string
	}{
		{"403 quota reached", http.StatusForbidden,
			`{"code":403,"title":"Forbidden","description":"Creation not allowed because a quota has been reached"}`, true, "quota"},
		{"413 too large", http.StatusRequestEntityTooLarge,
			`{"code":413,"title":"Request Entity Too Large","description":"Provided information too large to process"}`, false, "http_413"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, tc.status, tc.body)
			}))
			defer ts.Close()
			c, m := testClient(ts)

			_, err := c.StoreSecret(context.Background(), barbicanplan.Secret{Name: "sec-0001", PayloadBytes: 64}, 7)
			if got := errors.Is(err, ErrQuota); got != tc.wantQuota {
				t.Errorf("errors.Is(%v, ErrQuota) = %v, want %v", err, got, tc.wantQuota)
			}
			if got := errKind(err); got != tc.wantKind {
				t.Errorf("errKind(%v) = %q, want %q", err, got, tc.wantKind)
			}
			if agg := m.Aggregate(time.Second); len(agg.Errors) != 1 || agg.Errors[0].Kind != tc.wantKind {
				t.Errorf("sample errors = %+v, want one %s", agg.Errors, tc.wantKind)
			}
		})
	}
}

// urlSafeKey is the pattern Barbican's secret metadata validator
// (NewSecretMetadataValidator._check_string_url_safe) requires of every key.
var urlSafeKey = regexp.MustCompile(`^[A-Za-z0-9_-]*$`)

// barbicanMetadata serves PUT /v1/secrets/<id>/metadata the way Barbican does:
// like NewSecretMetadataValidator._extract_metadata, it answers a key outside
// ^[A-Za-z0-9_-]*$ with Barbican's 400 "Invalid Key. Key must be URL safe."
// and stores every accepted key lowercased, replacing the secret's whole set.
// A stamp whose keys break either rule then fails, or stores keys discovery
// does not read back.
type barbicanMetadata struct {
	t      *testing.T
	method string
	path   string
	body   string
	stored map[string]map[string]string // secret id -> metadata as Barbican keeps it
}

func (f *barbicanMetadata) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.method, f.path, f.body = r.Method, r.URL.Path, string(b)
	id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/v1/secrets/"), "/metadata")
	if r.Method != http.MethodPut || !ok {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	var req struct {
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(b, &req); err != nil {
		f.t.Errorf("decoding metadata body %s: %v", b, err)
		writeJSON(w, http.StatusBadRequest, `{"code": 400, "title": "Bad Request"}`)
		return
	}
	md := make(map[string]string, len(req.Metadata))
	for k, v := range req.Metadata {
		if !urlSafeKey.MatchString(k) {
			writeJSON(w, http.StatusBadRequest, `{"code": 400, "description": "Invalid Key. Key must be URL safe.", "title": "Bad Request"}`)
			return
		}
		md[strings.ToLower(k)] = v
	}
	if f.stored == nil {
		f.stored = map[string]map[string]string{}
	}
	f.stored[id] = md
	writeJSON(w, http.StatusCreated, `{"metadata_ref":"https://kv/v1/secrets/`+id+`/metadata"}`)
}

// TestBarbicanMetadataFakeFollowsValidator confirms the fake behind
// TestStampSecret enforces Barbican's key rules: a key with a colon is refused
// with Barbican's 400, and an uppercase key is stored lowercased.
func TestBarbicanMetadataFakeFollowsValidator(t *testing.T) {
	f := &barbicanMetadata{t: t}
	ts := httptest.NewServer(f)
	defer ts.Close()
	kv := testKV(ts)

	err := secrets.CreateMetadata(context.Background(), kv, "s1", secrets.MetadataOpts{"run:id": "run0"}).Err
	if !gophercloud.ResponseCodeIs(err, http.StatusBadRequest) {
		t.Fatalf("PUT of a colon key = %v, want a 400", err)
	}
	var code gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &code) || !strings.Contains(string(code.Body), "Invalid Key. Key must be URL safe.") {
		t.Errorf("400 body = %q, want Barbican's invalid key description", code.Body)
	}
	if _, ok := f.stored["s1"]; ok {
		t.Errorf("a refused PUT stored metadata %v", f.stored["s1"])
	}

	if err := secrets.CreateMetadata(context.Background(), kv, "s1", secrets.MetadataOpts{"Dizzy_Run": "run0"}).Err; err != nil {
		t.Fatalf("PUT of a URL-safe uppercase key: %v", err)
	}
	if want := map[string]string{"dizzy_run": "run0"}; fmt.Sprint(f.stored["s1"]) != fmt.Sprint(want) {
		t.Errorf("stored metadata = %v, want %v (key lowercased)", f.stored["s1"], want)
	}
}

// TestStampSecret confirms StampSecret PUTs exactly the run identity as the
// secret's metadata, that Barbican accepts its keys and keeps them unchanged so
// discovery reads back the key it compares, and that the stamp records one
// secret sample under the update operation.
func TestStampSecret(t *testing.T) {
	f := &barbicanMetadata{t: t}
	ts := httptest.NewServer(f)
	defer ts.Close()
	c, m, reader := telemetryTestClient(t, ts)

	if err := c.StampSecret(context.Background(), secretRes("sec-id")); err != nil {
		t.Fatalf("StampSecret: %v", err)
	}
	if f.method != http.MethodPut || f.path != "/v1/secrets/sec-id/metadata" {
		t.Errorf("request %s %s, want PUT /v1/secrets/sec-id/metadata", f.method, f.path)
	}
	if want := `{"metadata":{"dizzy_run":"run0","dizzy_type":"secret"}}`; f.body != want {
		t.Errorf("stamp body = %s, want %s", f.body, want)
	}
	stored := f.stored["sec-id"]
	if want := map[string]string{"dizzy_run": "run0", "dizzy_type": "secret"}; fmt.Sprint(stored) != fmt.Sprint(want) {
		t.Errorf("metadata Barbican stored = %v, want %v", stored, want)
	}
	if stored[metaRun] != "run0" {
		t.Errorf("stored metadata %v has no %s=run0, the key ListSecretsByRun compares", stored, metaRun)
	}
	if st := statsOf(m.Aggregate(time.Second), "secret"); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("secret samples = %+v, want 1 succeeded", st)
	}
	if got := metricPoints(t, reader, "dizzy.operation.duration"); !pointsEqual(got,
		map[string]string{"kind": "secret", "operation": "update", "outcome": "success"}) {
		t.Errorf("operation points = %v, want one secret/update success", got)
	}
}

// TestRetrievePayloadMatches confirms a read-back asks for the raw bytes and
// returns nil when they equal the stored payload, recording one successful
// secret sample under the download operation.
func TestRetrievePayloadMatches(t *testing.T) {
	const seed, n = 7, 512
	var accept, path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept, path = r.Header.Get("Accept"), r.URL.Path
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload.Bytes(seed, n))
	}))
	defer ts.Close()
	c, m, reader := telemetryTestClient(t, ts)

	if err := c.RetrievePayload(context.Background(), secretRes("sec-id"), seed, n); err != nil {
		t.Fatalf("RetrievePayload: %v", err)
	}
	if accept != "application/octet-stream" {
		t.Errorf("Accept = %q, want application/octet-stream", accept)
	}
	if path != "/v1/secrets/sec-id/payload" {
		t.Errorf("path = %q, want /v1/secrets/sec-id/payload", path)
	}
	agg := m.Aggregate(time.Second)
	if agg.Overall.Attempted != 1 {
		t.Errorf("samples = %d, want 1", agg.Overall.Attempted)
	}
	if st := statsOf(agg, "secret"); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("secret samples = %+v, want 1 succeeded", st)
	}
	if got := metricPoints(t, reader, "dizzy.operation.duration"); !pointsEqual(got,
		map[string]string{"kind": "secret", "operation": "download", "outcome": "success"}) {
		t.Errorf("operation points = %v, want one secret/download success", got)
	}
}

// TestRetrievePayloadMismatch confirms a read-back that returns other bytes
// fails with ErrPayloadMismatch, marks its sample failed with error kind
// mismatch, and names both lengths and the first differing offset without any
// payload bytes in the message.
func TestRetrievePayloadMismatch(t *testing.T) {
	const seed, n = 7, 64
	want := payload.Bytes(seed, n)
	flipped := bytes.Clone(want)
	flipped[10] ^= 0xff
	for _, tc := range []struct {
		name    string
		body    []byte
		wantMsg string
	}{
		{"one byte flipped", flipped,
			`barbican: payload mismatch: secret "sec-0001" returned 64 bytes, want 64, first difference at byte 10`},
		{"truncated", want[:32],
			`barbican: payload mismatch: secret "sec-0001" returned 32 bytes, want 64, first difference at byte 32`},
		{"longer", append(bytes.Clone(want), 0),
			`barbican: payload mismatch: secret "sec-0001" returned 65 bytes, want 64, first difference at byte 64`},
		{"empty body", nil,
			`barbican: payload mismatch: secret "sec-0001" returned 0 bytes, want 64, first difference at byte 0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(tc.body)
			}))
			defer ts.Close()
			c, m, reader := telemetryTestClient(t, ts)

			err := c.RetrievePayload(context.Background(), secretRes("sec-id"), seed, n)
			if !errors.Is(err, ErrPayloadMismatch) {
				t.Fatalf("RetrievePayload = %v, want ErrPayloadMismatch", err)
			}
			if err.Error() != tc.wantMsg {
				t.Errorf("error = %q, want %q", err, tc.wantMsg)
			}
			agg := m.Aggregate(time.Second)
			if st := statsOf(agg, "secret"); st.Attempted != 1 || st.Failed != 1 {
				t.Errorf("secret samples = %+v, want 1 failed", st)
			}
			if len(agg.Errors) != 1 || agg.Errors[0] != (metrics.ErrorCount{Kind: "mismatch", Count: 1}) {
				t.Errorf("sample errors = %+v, want one mismatch", agg.Errors)
			}
			if got := metricPoints(t, reader, "dizzy.operation.errors"); !pointsEqual(got,
				map[string]string{"kind": "secret", "operation": "download", "error.kind": "mismatch"}) {
				t.Errorf("operation.errors points = %v, want one secret/download mismatch", got)
			}
		})
	}
}

// TestRetrievePayloadNotFound confirms a read-back of a secret that is gone
// reports a 404, not a payload mismatch.
func TestRetrievePayloadNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, `{"code":404,"title":"Not Found"}`)
	}))
	defer ts.Close()
	c, _ := testClient(ts)

	err := c.RetrievePayload(context.Background(), secretRes("sec-id"), 7, 64)
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false, want true", err)
	}
	if errors.Is(err, ErrPayloadMismatch) {
		t.Errorf("error %v is a payload mismatch, want a plain 404", err)
	}
}

// TestRetrievePayloadDeadline confirms a read-back that outlives its context
// classifies as a timeout, in the returned error and in its sample.
func TestRetrievePayloadDeadline(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer ts.Close()
	defer close(release)
	c, m := testClient(ts)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.RetrievePayload(ctx, secretRes("sec-id"), 7, 64)
	if got := errKind(err); got != "timeout" {
		t.Errorf("errKind(%v) = %q, want timeout", err, got)
	}
	if agg := m.Aggregate(time.Second); len(agg.Errors) != 1 || agg.Errors[0].Kind != "timeout" {
		t.Errorf("sample errors = %+v, want one timeout", agg.Errors)
	}
}

// secretsFake serves a two-page secret list linked by next, and the metadata
// of each listed secret, recording which secrets' metadata was read.
type secretsFake struct {
	t        *testing.T
	pages    [2]string
	metadata map[string]string // id -> metadata JSON; an id absent here answers 404
	reads    []string
}

func (f *secretsFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/secrets":
		if got := r.URL.Query().Get("limit"); got != "100" {
			f.t.Errorf("list limit = %q, want 100", got)
		}
		if r.URL.Query().Get("offset") == "100" {
			writeJSON(w, http.StatusOK, f.pages[1])
			return
		}
		writeJSON(w, http.StatusOK, strings.ReplaceAll(f.pages[0], "NEXT", "http://"+r.Host+"/v1/secrets?limit=100&offset=100"))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/metadata"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/secrets/"), "/metadata")
		f.reads = append(f.reads, id)
		md, ok := f.metadata[id]
		if !ok {
			writeJSON(w, http.StatusNotFound, `{"code":404,"title":"Not Found"}`)
			return
		}
		writeJSON(w, http.StatusOK, md)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// secretEntry renders one entry of a secret list page.
func secretEntry(name, id string) string {
	return fmt.Sprintf(`{"name":%q,"secret_ref":"https://kv/v1/secrets/%s","status":"ACTIVE"}`, name, id)
}

// TestListSecretsByRun confirms discovery walks both pages, reads metadata only
// for names with the run prefix, and keeps only the secrets whose metadata
// carries this run: a prefix match stamped for another run, one without
// metadata, and one whose metadata read answers 404 are dropped.
func TestListSecretsByRun(t *testing.T) {
	f := &secretsFake{
		t: t,
		pages: [2]string{
			`{"secrets":[` + strings.Join([]string{
				secretEntry("dizzy-run0-sec-0001", "s1"),
				secretEntry("unrelated", "s2"),
				secretEntry("dizzy-run0-sec-0002", "s3"),
			}, ",") + `],"total":7,"next":"NEXT"}`,
			`{"secrets":[` + strings.Join([]string{
				secretEntry("dizzy-run0-sec-0003", "s4"),
				secretEntry("dizzy-run0-sec-0004", "s5"),
				secretEntry("dizzy-run0-ord-0001", "s6"),
				secretEntry("dizzy-run01-sec-0001", "s7"),
			}, ",") + `],"total":7}`,
		},
		metadata: map[string]string{
			"s1": `{"metadata":{"dizzy_run":"run0","dizzy_type":"secret"}}`,
			"s2": `{"metadata":{"dizzy_run":"run0","dizzy_type":"secret"}}`,
			"s3": `{"metadata":{"dizzy_run":"other","dizzy_type":"secret"}}`,
			"s4": `{"metadata":{}}`,
			"s6": `{"metadata":{"dizzy_run":"run0","dizzy_type":"secret"}}`,
			"s7": `{"metadata":{"dizzy_run":"run0","dizzy_type":"secret"}}`,
		},
	}
	ts := httptest.NewServer(f)
	defer ts.Close()
	c, m := testClient(ts)

	got, err := c.ListSecretsByRun(context.Background(), "run0")
	if err != nil {
		t.Fatalf("ListSecretsByRun: %v", err)
	}
	want := []resource.Resource{
		{Kind: KindSecret, Name: "dizzy-run0-sec-0001", ID: "s1"},
		{Kind: KindSecret, Name: "dizzy-run0-ord-0001", ID: "s6"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ListSecretsByRun = %+v, want %+v", got, want)
	}
	if wantReads := []string{"s1", "s3", "s4", "s5", "s6"}; fmt.Sprint(f.reads) != fmt.Sprint(wantReads) {
		t.Errorf("metadata reads = %v, want %v (prefix matches only)", f.reads, wantReads)
	}
	// One list sample plus one get per metadata read, the 404 among them failed.
	if st := statsOf(m.Aggregate(time.Second), "secret"); st.Attempted != 6 || st.Failed != 1 {
		t.Errorf("secret samples = %+v, want 6 attempted and 1 failed", st)
	}
}

// TestListSecretsByRunEmptyAndErrors confirms an empty project lists nothing
// without error, and that a failed list or a failed metadata read is returned
// with its context.
func TestListSecretsByRunEmptyAndErrors(t *testing.T) {
	t.Run("empty project", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `{"secrets":[],"total":0}`)
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		got, err := c.ListSecretsByRun(context.Background(), "run0")
		if err != nil || len(got) != 0 {
			t.Errorf("ListSecretsByRun = (%v, %v), want an empty result and nil", got, err)
		}
	})

	t.Run("list 500", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{"code":500}`)
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		_, err := c.ListSecretsByRun(context.Background(), "run0")
		if err == nil || !strings.Contains(err.Error(), "listing secrets by run") {
			t.Errorf("ListSecretsByRun error = %v, want one containing %q", err, "listing secrets by run")
		}
	})

	t.Run("metadata 500", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/metadata") {
				writeJSON(w, http.StatusInternalServerError, `{"code":500}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"secrets":[`+secretEntry("dizzy-run0-sec-0001", "s1")+`],"total":1}`)
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		_, err := c.ListSecretsByRun(context.Background(), "run0")
		if err == nil || !strings.Contains(err.Error(), "reading metadata of secret s1") {
			t.Errorf("ListSecretsByRun error = %v, want one containing %q", err, "reading metadata of secret s1")
		}
	})
}

// TestListSecretsByRunRequestTimeout confirms SetRequestTimeout bounds each
// discovery request on its own rather than the discovery as a whole: ten
// metadata reads of about 60ms each outlast the 300ms timeout together and
// still succeed, while a single read or the listing outliving the timeout fails
// with the deadline error, though the caller's context carries no deadline.
func TestListSecretsByRunRequestTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	const stamped = `{"metadata":{"dizzy_run":"run0","dizzy_type":"secret"}}`

	t.Run("each read within the timeout", func(t *testing.T) {
		entries := make([]string, 10)
		for i := range entries {
			entries[i] = secretEntry(fmt.Sprintf("dizzy-run0-sec-%04d", i+1), fmt.Sprintf("s%d", i+1))
		}
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/metadata") {
				time.Sleep(60 * time.Millisecond)
				writeJSON(w, http.StatusOK, stamped)
				return
			}
			writeJSON(w, http.StatusOK, `{"secrets":[`+strings.Join(entries, ",")+`],"total":10}`)
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		c.SetRequestTimeout(timeout)

		start := time.Now()
		got, err := c.ListSecretsByRun(context.Background(), "run0")
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("ListSecretsByRun: %v", err)
		}
		if len(got) != len(entries) {
			t.Errorf("ListSecretsByRun found %d secrets, want %d", len(got), len(entries))
		}
		// The reads together outlast the timeout, so a single bound over the
		// whole discovery would have failed it.
		if elapsed <= timeout {
			t.Errorf("discovery took %v, want longer than the %v request timeout", elapsed, timeout)
		}
	})

	for _, tc := range []struct {
		name    string
		wedged  func(r *http.Request) bool
		wantErr string
	}{
		{"one read over the timeout", func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/metadata") },
			"reading metadata of secret s1"},
		{"listing over the timeout", func(r *http.Request) bool { return r.URL.Path == "/v1/secrets" },
			"listing secrets by run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.wedged(r) {
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				if strings.HasSuffix(r.URL.Path, "/metadata") {
					writeJSON(w, http.StatusOK, stamped)
					return
				}
				writeJSON(w, http.StatusOK, `{"secrets":[`+secretEntry("dizzy-run0-sec-0001", "s1")+`],"total":1}`)
			}))
			defer ts.Close()
			defer close(release)
			c, _ := testClient(ts)
			c.SetRequestTimeout(timeout)

			_, err := c.ListSecretsByRun(context.Background(), "run0")
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("ListSecretsByRun = %v, want an error wrapping context.DeadlineExceeded", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ListSecretsByRun error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}
