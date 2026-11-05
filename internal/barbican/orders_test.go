package barbican

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

func orderRes(id string) resource.Resource {
	return resource.Resource{Kind: KindOrder, Logical: "ord-0001", Name: "dizzy-run0-ord-0001", ID: id}
}

// TestCreateOrder confirms CreateOrder POSTs a key order for a 256-bit AES-CBC
// key whose meta carries the deterministic name, and returns the order with the
// id from order_ref.
func TestCreateOrder(t *testing.T) {
	var body struct {
		Type string `json:"type"`
		Meta struct {
			Algorithm          string `json:"algorithm"`
			BitLength          int    `json:"bit_length"`
			Mode               string `json:"mode"`
			Name               string `json:"name"`
			PayloadContentType string `json:"payload_content_type"`
		} `json:"meta"`
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/orders" {
			t.Errorf("request %s %s, want POST /v1/orders", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding order body: %v", err)
		}
		writeJSON(w, http.StatusAccepted, `{"order_ref":"https://kv/v1/orders/ord-id"}`)
	}))
	defer ts.Close()
	c, m := testClient(ts)

	got, err := c.CreateOrder(context.Background(), barbicanplan.Order{Name: "ord-0001"})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if want := orderRes("ord-id"); got != want {
		t.Errorf("CreateOrder = %+v, want %+v", got, want)
	}
	if body.Type != "key" {
		t.Errorf("type = %q, want key", body.Type)
	}
	meta := body.Meta
	if meta.Algorithm != "aes" || meta.BitLength != 256 || meta.Mode != "cbc" ||
		meta.Name != "dizzy-run0-ord-0001" || meta.PayloadContentType != "application/octet-stream" {
		t.Errorf("meta = %+v, want aes/256/cbc named dizzy-run0-ord-0001 with application/octet-stream", meta)
	}
	if st := statsOf(m.Aggregate(time.Second), "order"); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("order samples = %+v, want 1 succeeded", st)
	}
}

// TestCreateOrderQuota confirms a 403 quota rejection of an order threads
// ErrQuota into the error and logs the order's name as a possible orphan.
func TestCreateOrderQuota(t *testing.T) {
	logs := captureLog(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, `{"code":403,"description":"Creation not allowed because a quota has been reached"}`)
	}))
	defer ts.Close()
	c, _ := testClient(ts)

	_, err := c.CreateOrder(context.Background(), barbicanplan.Order{Name: "ord-0001"})
	if !errors.Is(err, ErrQuota) {
		t.Errorf("errors.Is(%v, ErrQuota) = false, want true", err)
	}
	if err == nil || !strings.Contains(err.Error(), `creating order "ord-0001"`) {
		t.Errorf("error %v does not name the order", err)
	}
	if !strings.Contains(logs.String(), "name=dizzy-run0-ord-0001") {
		t.Errorf("log %q does not name dizzy-run0-ord-0001", logs.String())
	}
}

// orderPolls serves GET /v1/orders/ord-id with the given answers in turn,
// repeating the last one, and counts the polls.
type orderPolls struct {
	t       *testing.T
	answers []string // a JSON body, or "404" or "500" for that error status

	mu    sync.Mutex
	polls int
}

func (f *orderPolls) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/v1/orders/ord-id" {
		f.t.Errorf("request %s %s, want GET /v1/orders/ord-id", r.Method, r.URL.Path)
	}
	f.mu.Lock()
	answer := f.answers[min(f.polls, len(f.answers)-1)]
	f.polls++
	f.mu.Unlock()
	if answer == "404" {
		writeJSON(w, http.StatusNotFound, `{"code":404,"title":"Not Found"}`)
		return
	}
	if answer == "500" {
		writeJSON(w, http.StatusInternalServerError, `{"code":500}`)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// count returns how many polls the fake has answered.
func (f *orderPolls) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

const (
	orderPending = `{"status":"PENDING","order_ref":"https://kv/v1/orders/ord-id"}`
	orderActive  = `{"status":"ACTIVE","order_ref":"https://kv/v1/orders/ord-id","secret_ref":"https://kv/v1/secrets/gen-id"}`
)

// TestWaitForOrderActive confirms the wait polls through PENDING to ACTIVE,
// returns the generated secret under the order's logical and cloud name, and
// records one successful order readiness and no operation sample for the polls.
func TestWaitForOrderActive(t *testing.T) {
	f := &orderPolls{t: t, answers: []string{orderPending, orderPending, orderActive}}
	ts := httptest.NewServer(f)
	defer ts.Close()
	c, m, reader := telemetryTestClient(t, ts)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := c.WaitForOrder(ctx, orderRes("ord-id"))
	if err != nil {
		t.Fatalf("WaitForOrder: %v", err)
	}
	want := resource.Resource{Kind: KindSecret, Logical: "ord-0001", Name: "dizzy-run0-ord-0001", ID: "gen-id"}
	if got != want {
		t.Errorf("WaitForOrder = %+v, want %+v", got, want)
	}
	if n := f.count(); n != 3 {
		t.Errorf("polls = %d, want 3", n)
	}
	agg := m.Aggregate(time.Second)
	if len(agg.Readiness) != 1 || agg.Readiness[0].Type != "order" || agg.Readiness[0].Count != 1 || agg.Readiness[0].OK != 1 {
		t.Errorf("readiness = %+v, want one OK order", agg.Readiness)
	}
	if agg.Overall.Attempted != 0 {
		t.Errorf("operation samples = %d, want 0 for the polls", agg.Overall.Attempted)
	}
	if got := metricPoints(t, reader, "dizzy.resource.time_to_ready"); !pointsEqual(got,
		map[string]string{"kind": "order", "outcome": "success"}) {
		t.Errorf("time_to_ready points = %v, want one order success", got)
	}
	if got := metricPoints(t, reader, "dizzy.operation.duration"); len(got) != 0 {
		t.Errorf("operation points = %v, want none for the polls", got)
	}
}

// TestWaitForOrderPollsThroughTransientErrors confirms a poll answered with a
// 500 is retried on the next tick instead of ending the wait.
func TestWaitForOrderPollsThroughTransientErrors(t *testing.T) {
	f := &orderPolls{t: t, answers: []string{"500", orderActive}}
	ts := httptest.NewServer(f)
	defer ts.Close()
	c, _ := testClient(ts)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := c.WaitForOrder(ctx, orderRes("ord-id"))
	if err != nil || got.ID != "gen-id" {
		t.Errorf("WaitForOrder = (%+v, %v), want the generated secret gen-id", got, err)
	}
}

// TestWaitForOrderFailures confirms the wait's terminal outcomes: an order in
// ERROR and an ACTIVE order without secret_ref fail with ErrOrderFailed and one
// failed order sample of error kind order_error, a 404 ends the wait as gone,
// and context expiry returns the context's error. Each records one readiness
// sample that is not OK.
func TestWaitForOrderFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		answers     []string
		timeout     time.Duration
		check       func(t *testing.T, err error)
		orderErrors int // failed order samples, all of error kind order_error
	}{
		{
			name:    "error",
			answers: []string{orderPending, `{"status":"ERROR","error_reason":"boom","error_status_code":"500"}`},
			timeout: 10 * time.Second,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrOrderFailed) {
					t.Errorf("WaitForOrder = %v, want ErrOrderFailed", err)
				}
				if want := "barbican: order failed: order ord-id: boom (status 500)"; err == nil || err.Error() != want {
					t.Errorf("error = %v, want %q", err, want)
				}
			},
			orderErrors: 1,
		},
		{
			name:    "active without secret_ref",
			answers: []string{`{"status":"ACTIVE","order_ref":"https://kv/v1/orders/ord-id"}`},
			timeout: 10 * time.Second,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrOrderFailed) {
					t.Errorf("WaitForOrder = %v, want ErrOrderFailed", err)
				}
				if err == nil || !strings.Contains(err.Error(), "active without secret_ref") {
					t.Errorf("error %v does not name the missing secret_ref", err)
				}
			},
			orderErrors: 1,
		},
		{
			name:    "404",
			answers: []string{orderPending, "404"},
			timeout: 10 * time.Second,
			check: func(t *testing.T, err error) {
				if err == nil || !IsNotFound(err) || !strings.Contains(err.Error(), "is gone while waiting for readiness") {
					t.Errorf("WaitForOrder = %v, want a gone-while-waiting 404", err)
				}
				if errors.Is(err, ErrOrderFailed) {
					t.Errorf("error %v is ErrOrderFailed, want a plain 404", err)
				}
			},
		},
		{
			name:    "context expiry",
			answers: []string{orderPending},
			timeout: 300 * time.Millisecond,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("WaitForOrder = %v, want context.DeadlineExceeded", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(&orderPolls{t: t, answers: tc.answers})
			defer ts.Close()
			c, m, reader := telemetryTestClient(t, ts)

			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			got, err := c.WaitForOrder(ctx, orderRes("ord-id"))
			tc.check(t, err)
			if got != (resource.Resource{}) {
				t.Errorf("WaitForOrder resource = %+v, want none", got)
			}

			agg := m.Aggregate(time.Second)
			if r := readinessOf(agg, "order"); r.Count != 1 || r.OK != 0 {
				t.Errorf("order readiness = %+v, want one not OK", r)
			}
			if got := metricPoints(t, reader, "dizzy.resource.time_to_ready"); !pointsEqual(got,
				map[string]string{"kind": "order", "outcome": "timeout"}) {
				t.Errorf("time_to_ready points = %v, want one order timeout", got)
			}
			st := statsOf(agg, "order")
			if agg.Overall.Attempted != tc.orderErrors || st.Attempted != tc.orderErrors || st.Failed != tc.orderErrors {
				t.Errorf("samples = %d, order samples = %+v, want %d failed order samples and nothing else",
					agg.Overall.Attempted, st, tc.orderErrors)
			}
			if tc.orderErrors == 0 {
				return
			}
			if len(agg.Errors) != 1 || agg.Errors[0] != (metrics.ErrorCount{Kind: "order_error", Count: tc.orderErrors}) {
				t.Errorf("sample errors = %+v, want %d order_error", agg.Errors, tc.orderErrors)
			}
			if got := metricPoints(t, reader, "dizzy.operation.errors"); !pointsEqual(got,
				map[string]string{"kind": "order", "operation": "get", "error.kind": "order_error"}) {
				t.Errorf("operation.errors points = %v, want one order/get order_error", got)
			}
		})
	}
}

// orderEntry renders one entry of an order list page in the given status. An
// empty secretRef leaves secret_ref out, as Barbican does for an order that has
// not generated its secret (PENDING) or never will (ERROR).
func orderEntry(metaName, id, status, secretRef string) string {
	ref := ""
	if secretRef != "" {
		ref = fmt.Sprintf(`,"secret_ref":%q`, secretRef)
	}
	return fmt.Sprintf(`{"type":"key","status":%q,"order_ref":"https://kv/v1/orders/%s"%s,"meta":{"name":%q,"algorithm":"aes","bit_length":256,"mode":"cbc"}}`, status, id, ref, metaName)
}

// TestListOrdersByRun confirms discovery walks both pages and keeps only the
// orders whose meta.name carries the run prefix, with ids from order_ref, and
// returns the generated secret of each kept order that has a secret_ref, named
// after the order: the ACTIVE order's secret, while a PENDING and an ERROR
// order contribute none.
func TestListOrdersByRun(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/orders" {
			t.Errorf("request %s %s, want GET /v1/orders", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("list limit = %q, want 100", got)
		}
		if r.URL.Query().Get("offset") == "100" {
			writeJSON(w, http.StatusOK, `{"orders":[`+strings.Join([]string{
				orderEntry("dizzy-run0-ord-0002", "o4", "PENDING", ""),
				orderEntry("dizzy-run0-ord-0003", "o6", "ERROR", ""),
				orderEntry("dizzy-run01-ord-0001", "o5", "ACTIVE", "https://kv/v1/secrets/g5"),
			}, ",")+`],"total":6}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"orders":[`+strings.Join([]string{
			orderEntry("dizzy-run0-ord-0001", "o1", "ACTIVE", "https://kv/v1/secrets/g1"),
			orderEntry("dizzy-other-ord-0001", "o2", "ACTIVE", "https://kv/v1/secrets/g2"),
			`{"type":"key","status":"ACTIVE","order_ref":"https://kv/v1/orders/o3","secret_ref":"https://kv/v1/secrets/g3","meta":{}}`,
		}, ",")+`],"total":6,"next":"http://`+r.Host+`/v1/orders?limit=100&offset=100"}`)
	}))
	defer ts.Close()
	c, m := testClient(ts)

	gotOrders, gotSecrets, err := c.ListOrdersByRun(context.Background(), "run0")
	if err != nil {
		t.Fatalf("ListOrdersByRun: %v", err)
	}
	wantOrders := []resource.Resource{
		{Kind: KindOrder, Name: "dizzy-run0-ord-0001", ID: "o1"},
		{Kind: KindOrder, Name: "dizzy-run0-ord-0002", ID: "o4"},
		{Kind: KindOrder, Name: "dizzy-run0-ord-0003", ID: "o6"},
	}
	if fmt.Sprint(gotOrders) != fmt.Sprint(wantOrders) {
		t.Errorf("ListOrdersByRun orders = %+v, want %+v", gotOrders, wantOrders)
	}
	wantSecrets := []resource.Resource{
		{Kind: KindSecret, Name: "dizzy-run0-ord-0001", ID: "g1"},
	}
	if fmt.Sprint(gotSecrets) != fmt.Sprint(wantSecrets) {
		t.Errorf("ListOrdersByRun secrets = %+v, want %+v (the ACTIVE order's only)", gotSecrets, wantSecrets)
	}
	if st := statsOf(m.Aggregate(time.Second), "order"); st.Attempted != 1 || st.Succeeded != 1 {
		t.Errorf("order samples = %+v, want one list", st)
	}
}

// TestListOrdersByRunEmptyAndError confirms an empty project lists nothing
// without error, and that a failed list or a kept order whose secret_ref has no
// id is returned as an error with its context.
func TestListOrdersByRunEmptyAndError(t *testing.T) {
	t.Run("empty project", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `{"orders":[],"total":0}`)
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		gotOrders, gotSecrets, err := c.ListOrdersByRun(context.Background(), "run0")
		if err != nil || len(gotOrders) != 0 || len(gotSecrets) != 0 {
			t.Errorf("ListOrdersByRun = (%v, %v, %v), want empty results and nil", gotOrders, gotSecrets, err)
		}
	})

	t.Run("list 500", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{"code":500}`)
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		_, _, err := c.ListOrdersByRun(context.Background(), "run0")
		if err == nil || !strings.Contains(err.Error(), "listing orders by run") {
			t.Errorf("ListOrdersByRun error = %v, want one containing %q", err, "listing orders by run")
		}
	})

	t.Run("unusable secret_ref", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `{"orders":[`+orderEntry("dizzy-run0-ord-0001", "o1", "ACTIVE", "https://kv/")+`],"total":1}`)
		}))
		defer ts.Close()
		c, _ := testClient(ts)
		gotOrders, gotSecrets, err := c.ListOrdersByRun(context.Background(), "run0")
		const want = `listing orders by run: order "dizzy-run0-ord-0001" secret_ref: reference "https://kv/" has no id segment`
		if err == nil || err.Error() != want {
			t.Errorf("ListOrdersByRun error = %v, want %q", err, want)
		}
		if gotOrders != nil || gotSecrets != nil {
			t.Errorf("ListOrdersByRun = (%v, %v) with an error, want no results", gotOrders, gotSecrets)
		}
	})
}
