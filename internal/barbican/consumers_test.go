package barbican

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// testContainer is the created container the consumer tests target.
var testContainer = resource.Resource{Kind: KindContainer, Logical: "ctr-0001", Name: "dizzy-run0-ctr-0001", ID: "c1"}

// consumerRes is the resource RegisterConsumer returns for the consumer with
// cloud name name on testContainer.
func consumerRes(logical string) resource.Resource {
	name := "dizzy-run0-" + logical
	return resource.Resource{Kind: KindConsumer, Logical: logical, Name: name, ID: "c1:" + name}
}

// consumerEntry renders one entry of a consumer list page as Barbican answers
// it, with the URL under the key URL.
func consumerEntry(name, url string) string {
	return fmt.Sprintf(`{"name":%q,"URL":%q,"status":"ACTIVE","created":"2026-11-18T10:00:00","updated":"2026-11-18T10:00:00"}`, name, url)
}

// listedConsumer renders the list entry of a consumer dizzy registered under
// the logical name logical.
func listedConsumer(logical string) string {
	name := "dizzy-run0-" + logical
	return consumerEntry(name, "https://dizzy.invalid/consumers/"+name)
}

// consumerPage renders one page of a container's consumer list.
func consumerPage(next string, entries ...string) string {
	page := `{"consumers":[` + strings.Join(entries, ",") + `],"total":` + fmt.Sprint(len(entries))
	if next != "" {
		page += `,"next":"` + next + `"`
	}
	return page + "}"
}

// TestRegisterConsumer confirms RegisterConsumer POSTs to the container's
// consumers the deterministic name and the URL derived from it, records one
// succeeded consumer/create, and returns a consumer resource with that name
// and the id <containerID>:<name>.
func TestRegisterConsumer(t *testing.T) {
	var requests []string
	var body map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding consumer body: %v", err)
		}
		writeJSON(w, http.StatusOK, `{"type":"generic","status":"ACTIVE","name":"dizzy-run0-ctr-0001","container_ref":"https://kv/v1/containers/c1","consumers":[{"name":"dizzy-run0-con-0001","URL":"https://dizzy.invalid/consumers/dizzy-run0-con-0001"}]}`)
	}))
	defer ts.Close()
	c, m, reader := telemetryTestClient(t, ts)

	got, err := c.RegisterConsumer(context.Background(), barbicanplan.Consumer{Name: "con-0001", Container: "ctr-0001"}, testContainer)
	if err != nil {
		t.Fatalf("RegisterConsumer: %v", err)
	}
	want := resource.Resource{Kind: KindConsumer, Logical: "con-0001", Name: "dizzy-run0-con-0001", ID: "c1:dizzy-run0-con-0001"}
	if got != want {
		t.Errorf("RegisterConsumer = %+v, want %+v", got, want)
	}
	if want := []string{"POST /v1/containers/c1/consumers"}; fmt.Sprint(requests) != fmt.Sprint(want) {
		t.Errorf("requests = %v, want %v", requests, want)
	}
	wantBody := map[string]string{"name": "dizzy-run0-con-0001", "URL": "https://dizzy.invalid/consumers/dizzy-run0-con-0001"}
	if fmt.Sprint(body) != fmt.Sprint(wantBody) {
		t.Errorf("request body = %v, want %v", body, wantBody)
	}
	if st := statsOf(m.Aggregate(time.Second), "consumer"); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("consumer samples = %+v, want 1 succeeded", st)
	}
	if got := metricPoints(t, reader, "dizzy.operation.duration"); !pointsEqual(got,
		map[string]string{"kind": "consumer", "operation": "create", "outcome": "success"}) {
		t.Errorf("operation.duration points = %v, want one consumer/create success", got)
	}
}

// TestRegisterConsumerRefusesUncreatedContainer confirms a container without an
// id is refused before any request or sample.
func TestRegisterConsumerRefusesUncreatedContainer(t *testing.T) {
	var requests int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer ts.Close()
	c, m := testClient(ts)

	_, err := c.RegisterConsumer(context.Background(), barbicanplan.Consumer{Name: "con-0001", Container: "ctr-0001"},
		resource.Resource{Kind: KindContainer, Logical: "ctr-0001"})
	if want := `consumer "con-0001" targets container "ctr-0001" that was not created`; err == nil || err.Error() != want {
		t.Errorf("RegisterConsumer error = %v, want %q", err, want)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0", requests)
	}
	if n := m.Aggregate(time.Second).Overall.Attempted; n != 0 {
		t.Errorf("samples = %d, want 0", n)
	}
}

// TestRegisterConsumerQuota confirms a 403 whose body mentions a quota returns
// a create error wrapping ErrQuota, so the executor fails fast.
func TestRegisterConsumerQuota(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, `{"code":403,"title":"Forbidden","description":"Creation not allowed because a quota has been reached"}`)
	}))
	defer ts.Close()
	c, _ := testClient(ts)

	_, err := c.RegisterConsumer(context.Background(), barbicanplan.Consumer{Name: "con-0001", Container: "ctr-0001"}, testContainer)
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("RegisterConsumer error = %v, want an error wrapping ErrQuota", err)
	}
	if !strings.HasPrefix(err.Error(), `creating consumer "con-0001"`) {
		t.Errorf("RegisterConsumer error = %v, want one starting %q", err, `creating consumer "con-0001"`)
	}
}

// TestVerifyConsumersExactOverTwoPages confirms an exact check passes when the
// container lists exactly the wanted consumers spread over two pages, paged
// 100 at a time, as one succeeded consumer/list.
func TestVerifyConsumersExactOverTwoPages(t *testing.T) {
	var requests []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.URL.Path != "/v1/containers/c1/consumers" {
			t.Errorf("request %s %s, want GET /v1/containers/c1/consumers", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("list limit = %q, want 100", got)
		}
		if r.URL.Query().Get("offset") == "100" {
			writeJSON(w, http.StatusOK, consumerPage("", listedConsumer("con-0003")))
			return
		}
		writeJSON(w, http.StatusOK, consumerPage("http://"+r.Host+"/v1/containers/c1/consumers?limit=100&offset=100",
			listedConsumer("con-0002"), listedConsumer("con-0001")))
	}))
	defer ts.Close()
	c, m, reader := telemetryTestClient(t, ts)

	want := []resource.Resource{consumerRes("con-0001"), consumerRes("con-0002"), consumerRes("con-0003")}
	if err := c.VerifyConsumers(context.Background(), testContainer, want, true); err != nil {
		t.Fatalf("VerifyConsumers = %v, want nil", err)
	}
	if len(requests) != 2 {
		t.Errorf("requests = %v, want two pages", requests)
	}
	if st := statsOf(m.Aggregate(time.Second), "consumer"); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("consumer samples = %+v, want one succeeded list", st)
	}
	if got := metricPoints(t, reader, "dizzy.operation.duration"); !pointsEqual(got,
		map[string]string{"kind": "consumer", "operation": "list", "outcome": "success"}) {
		t.Errorf("operation.duration points = %v, want one consumer/list success", got)
	}
}

// consumerListServer answers every request with a single page holding entries.
func consumerListServer(t *testing.T, requests *int, entries ...string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests++
		writeJSON(w, http.StatusOK, consumerPage("", entries...))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestVerifyConsumersLacksWanted confirms a list that lacks a wanted consumer
// (or holds it under another URL), or is empty, is a mismatch naming the first
// wanted consumer missing, never retryable, recorded with error kind mismatch.
func TestVerifyConsumersLacksWanted(t *testing.T) {
	want := []resource.Resource{consumerRes("con-0001"), consumerRes("con-0002")}
	for _, tc := range []struct {
		name    string
		entries []string
		wantErr string
	}{
		{"one missing", []string{listedConsumer("con-0001")},
			`barbican: consumer mismatch: container c1 lacks consumer "dizzy-run0-con-0002"`},
		{"empty list", nil,
			`barbican: consumer mismatch: container c1 lacks consumer "dizzy-run0-con-0001"`},
		{"another URL", []string{listedConsumer("con-0001"), consumerEntry("dizzy-run0-con-0002", "https://elsewhere.invalid/x")},
			`barbican: consumer mismatch: container c1 lacks consumer "dizzy-run0-con-0002"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests int
			c, m := testClient(consumerListServer(t, &requests, tc.entries...))

			err := c.VerifyConsumers(context.Background(), testContainer, want, true)
			if !errors.Is(err, ErrConsumerMismatch) {
				t.Fatalf("VerifyConsumers = %v, want an error wrapping ErrConsumerMismatch", err)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("VerifyConsumers error = %q, want %q", err, tc.wantErr)
			}
			if IsRetryable(err) {
				t.Errorf("IsRetryable(%v) = true, want false", err)
			}
			agg := m.Aggregate(time.Second)
			if len(agg.Errors) != 1 || agg.Errors[0] != (metrics.ErrorCount{Kind: ErrKindMismatch, Count: 1}) {
				t.Errorf("errors = %+v, want one mismatch", agg.Errors)
			}
		})
	}
}

// TestVerifyConsumersUnexpected confirms a list holding one consumer more than
// wanted is a mismatch naming it for an exact check and passes otherwise.
func TestVerifyConsumersUnexpected(t *testing.T) {
	want := []resource.Resource{consumerRes("con-0001")}
	entries := []string{listedConsumer("con-0001"), listedConsumer("con-0009")}

	var requests int
	c, _ := testClient(consumerListServer(t, &requests, entries...))
	err := c.VerifyConsumers(context.Background(), testContainer, want, true)
	if want := `barbican: consumer mismatch: container c1 lists unexpected consumer "dizzy-run0-con-0009"`; err == nil || err.Error() != want {
		t.Errorf("VerifyConsumers(exact) = %v, want %q", err, want)
	}
	if !errors.Is(err, ErrConsumerMismatch) {
		t.Errorf("VerifyConsumers(exact) error %v does not wrap ErrConsumerMismatch", err)
	}

	if err := c.VerifyConsumers(context.Background(), testContainer, want, false); err != nil {
		t.Errorf("VerifyConsumers(not exact) = %v, want nil", err)
	}
}

// TestVerifyConsumersEmptyWant confirms an empty want without exact checks
// nothing and sends no request, while with exact it lists the container and
// passes on an empty list.
func TestVerifyConsumersEmptyWant(t *testing.T) {
	var requests int
	c, m := testClient(consumerListServer(t, &requests))

	if err := c.VerifyConsumers(context.Background(), testContainer, nil, false); err != nil {
		t.Errorf("VerifyConsumers(no want, not exact) = %v, want nil", err)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0 for no want without exact", requests)
	}
	if n := m.Aggregate(time.Second).Overall.Attempted; n != 0 {
		t.Errorf("samples = %d, want 0", n)
	}

	if err := c.VerifyConsumers(context.Background(), testContainer, nil, true); err != nil {
		t.Errorf("VerifyConsumers(no want, exact) on an empty list = %v, want nil", err)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1 for an exact check", requests)
	}
}

// TestVerifyConsumersListError confirms a failed listing returns an error naming
// the container that stays retryable for a 500.
func TestVerifyConsumersListError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, `{"code":500}`)
	}))
	defer ts.Close()
	c, _ := testClient(ts)

	err := c.VerifyConsumers(context.Background(), testContainer, []resource.Resource{consumerRes("con-0001")}, true)
	if err == nil || !strings.HasPrefix(err.Error(), "listing consumers of container c1") {
		t.Fatalf("VerifyConsumers error = %v, want one starting %q", err, "listing consumers of container c1")
	}
	if !IsRetryable(err) {
		t.Errorf("IsRetryable(%v) = false, want true", err)
	}
	if errors.Is(err, ErrConsumerMismatch) {
		t.Errorf("VerifyConsumers error %v is a mismatch; a failed listing is not", err)
	}
}

func TestConsumerIDRoundTrip(t *testing.T) {
	if got, want := consumerURL("dizzy-run0-con-0001"), "https://dizzy.invalid/consumers/dizzy-run0-con-0001"; got != want {
		t.Errorf("consumerURL = %q, want %q", got, want)
	}
	id := consumerID("c1", "dizzy-run0-con-0001")
	if id != "c1:dizzy-run0-con-0001" {
		t.Errorf("consumerID = %q, want %q", id, "c1:dizzy-run0-con-0001")
	}
	for _, tc := range []struct {
		id, container, name string
		wantErr             bool
	}{
		{id: id, container: "c1", name: "dizzy-run0-con-0001"},
		{id: "c1:a:b", container: "c1", name: "a:b"},
		{id: "no-colon", wantErr: true},
		{id: ":dizzy-run0-con-0001", wantErr: true},
		{id: "c1:", wantErr: true},
	} {
		t.Run(tc.id, func(t *testing.T) {
			container, name, err := parseConsumerID(tc.id)
			if tc.wantErr {
				if want := fmt.Sprintf("malformed consumer id %q", tc.id); err == nil || err.Error() != want {
					t.Errorf("parseConsumerID(%q) error = %v, want %q", tc.id, err, want)
				}
				return
			}
			if err != nil || container != tc.container || name != tc.name {
				t.Errorf("parseConsumerID(%q) = (%q, %q, %v), want (%q, %q, nil)", tc.id, container, name, err, tc.container, tc.name)
			}
		})
	}
}
