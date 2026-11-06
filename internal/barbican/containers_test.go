package barbican

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// TestCreateContainer confirms CreateContainer POSTs a generic container with
// the deterministic name whose secret_refs carry each referenced secret's
// logical name and href in plan order, and returns the container with the id
// from container_ref.
func TestCreateContainer(t *testing.T) {
	type secretRef struct {
		Name      string `json:"name"`
		SecretRef string `json:"secret_ref"`
	}
	var body struct {
		Type       string      `json:"type"`
		Name       string      `json:"name"`
		SecretRefs []secretRef `json:"secret_refs"`
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/containers" {
			t.Errorf("request %s %s, want POST /v1/containers", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding container body: %v", err)
		}
		writeJSON(w, http.StatusCreated, `{"container_ref":"https://kv/v1/containers/ctr-id"}`)
	}))
	defer ts.Close()
	c, m := testClient(ts)

	stored := map[string]resource.Resource{
		"sec-0001": {Kind: KindSecret, Logical: "sec-0001", Name: "dizzy-run0-sec-0001", ID: "s1"},
		"sec-0002": {Kind: KindSecret, Logical: "sec-0002", Name: "dizzy-run0-sec-0002", ID: "s2"},
		"sec-0003": {Kind: KindSecret, Logical: "sec-0003", Name: "dizzy-run0-sec-0003", ID: "s3"},
	}
	ct := barbicanplan.Container{Name: "ctr-0001", Secrets: []string{"sec-0003", "sec-0001"}}
	got, err := c.CreateContainer(context.Background(), ct, stored)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	want := resource.Resource{Kind: KindContainer, Logical: "ctr-0001", Name: "dizzy-run0-ctr-0001", ID: "ctr-id"}
	if got != want {
		t.Errorf("CreateContainer = %+v, want %+v", got, want)
	}
	if body.Type != "generic" || body.Name != "dizzy-run0-ctr-0001" {
		t.Errorf("container = %s named %q, want generic named dizzy-run0-ctr-0001", body.Type, body.Name)
	}
	wantRefs := []secretRef{
		{Name: "sec-0003", SecretRef: ts.URL + "/v1/secrets/s3"},
		{Name: "sec-0001", SecretRef: ts.URL + "/v1/secrets/s1"},
	}
	if fmt.Sprint(body.SecretRefs) != fmt.Sprint(wantRefs) {
		t.Errorf("secret_refs = %+v, want %+v", body.SecretRefs, wantRefs)
	}
	if st := statsOf(m.Aggregate(time.Second), "container"); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("container samples = %+v, want 1 succeeded", st)
	}
}

// TestCreateContainerRefusesUnstoredReference confirms a reference to a secret
// that was not stored is refused before any request or sample.
func TestCreateContainerRefusesUnstoredReference(t *testing.T) {
	var requests int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer ts.Close()
	c, m := testClient(ts)

	stored := map[string]resource.Resource{
		"sec-0001": {Kind: KindSecret, Logical: "sec-0001", Name: "dizzy-run0-sec-0001", ID: "s1"},
	}
	ct := barbicanplan.Container{Name: "ctr-0001", Secrets: []string{"sec-0001", "sec-0009"}}
	_, err := c.CreateContainer(context.Background(), ct, stored)
	if want := `container "ctr-0001" references secret "sec-0009" that was not stored`; err == nil || err.Error() != want {
		t.Errorf("CreateContainer error = %v, want %q", err, want)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0", requests)
	}
	if n := m.Aggregate(time.Second).Overall.Attempted; n != 0 {
		t.Errorf("samples = %d, want 0", n)
	}
}

// containerEntry renders one entry of a container list page.
func containerEntry(name, id string) string {
	return fmt.Sprintf(`{"type":"generic","status":"ACTIVE","name":%q,"container_ref":"https://kv/v1/containers/%s","secret_refs":[]}`, name, id)
}

// TestListContainersByRun confirms discovery walks both pages and keeps only
// the containers whose name carries the run prefix, with ids from
// container_ref.
func TestListContainersByRun(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/containers" {
			t.Errorf("request %s %s, want GET /v1/containers", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("list limit = %q, want 100", got)
		}
		if r.URL.Query().Get("offset") == "100" {
			writeJSON(w, http.StatusOK, `{"containers":[`+strings.Join([]string{
				containerEntry("dizzy-run0-ctr-0002", "c3"),
				containerEntry("dizzy-run01-ctr-0001", "c4"),
			}, ",")+`],"total":4}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"containers":[`+strings.Join([]string{
			containerEntry("dizzy-run0-ctr-0001", "c1"),
			containerEntry("someone-elses", "c2"),
		}, ",")+`],"total":4,"next":"http://`+r.Host+`/v1/containers?limit=100&offset=100"}`)
	}))
	defer ts.Close()
	c, m := testClient(ts)

	got, err := c.ListContainersByRun(context.Background(), "run0")
	if err != nil {
		t.Fatalf("ListContainersByRun: %v", err)
	}
	want := []resource.Resource{
		{Kind: KindContainer, Name: "dizzy-run0-ctr-0001", ID: "c1"},
		{Kind: KindContainer, Name: "dizzy-run0-ctr-0002", ID: "c3"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ListContainersByRun = %+v, want %+v", got, want)
	}
	if st := statsOf(m.Aggregate(time.Second), "container"); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("container samples = %+v, want one list", st)
	}
}

// TestListContainersByRunEmptyAndError confirms an empty project lists nothing
// without error and a failed list is returned with its context.
func TestListContainersByRunEmptyAndError(t *testing.T) {
	t.Run("empty project", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `{"containers":[],"total":0}`)
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		got, err := c.ListContainersByRun(context.Background(), "run0")
		if err != nil || len(got) != 0 {
			t.Errorf("ListContainersByRun = (%v, %v), want an empty result and nil", got, err)
		}
	})

	t.Run("list 500", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{"code":500}`)
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		_, err := c.ListContainersByRun(context.Background(), "run0")
		if err == nil || !strings.Contains(err.Error(), "listing containers by run") {
			t.Errorf("ListContainersByRun error = %v, want one containing %q", err, "listing containers by run")
		}
	})
}

// TestListContainersByAnyRun confirms the sweep's discovery walks both pages
// and keeps every container whose name carries the dizzy- prefix, whatever its
// run id, and never one without it.
func TestListContainersByAnyRun(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/containers" {
			t.Errorf("request %s %s, want GET /v1/containers", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("list limit = %q, want 100", got)
		}
		if r.URL.Query().Get("offset") == "100" {
			writeJSON(w, http.StatusOK, `{"containers":[`+strings.Join([]string{
				containerEntry("dizzy-run9-ctr-0001", "c3"),
				containerEntry("dizzy-run01-ctr-0001", "c4"),
			}, ",")+`],"total":4}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"containers":[`+strings.Join([]string{
			containerEntry("dizzy-run0-ctr-0001", "c1"),
			containerEntry("someone-elses", "c2"),
		}, ",")+`],"total":4,"next":"http://`+r.Host+`/v1/containers?limit=100&offset=100"}`)
	}))
	defer ts.Close()
	c, m := testClient(ts)

	got, err := c.ListContainersByAnyRun(context.Background())
	if err != nil {
		t.Fatalf("ListContainersByAnyRun: %v", err)
	}
	want := []resource.Resource{
		{Kind: KindContainer, Name: "dizzy-run0-ctr-0001", ID: "c1"},
		{Kind: KindContainer, Name: "dizzy-run9-ctr-0001", ID: "c3"},
		{Kind: KindContainer, Name: "dizzy-run01-ctr-0001", ID: "c4"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ListContainersByAnyRun = %+v, want %+v", got, want)
	}
	if st := statsOf(m.Aggregate(time.Second), "container"); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("container samples = %+v, want one list", st)
	}
}

// TestListContainersByAnyRunEmptyAndError confirms an empty project lists
// nothing without error and a failed list is returned with its context.
func TestListContainersByAnyRunEmptyAndError(t *testing.T) {
	t.Run("empty project", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `{"containers":[],"total":0}`)
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		got, err := c.ListContainersByAnyRun(context.Background())
		if err != nil || len(got) != 0 {
			t.Errorf("ListContainersByAnyRun = (%v, %v), want an empty result and nil", got, err)
		}
	})

	t.Run("list 500", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{"code":500}`)
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		_, err := c.ListContainersByAnyRun(context.Background())
		if err == nil || !strings.Contains(err.Error(), "listing containers by any run") {
			t.Errorf("ListContainersByAnyRun error = %v, want one containing %q", err, "listing containers by any run")
		}
	})
}
