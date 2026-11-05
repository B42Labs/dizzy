package barbican

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	barbicanplan "github.com/B42Labs/dizzy/internal/barbican/plan"
)

// quotaPlan builds a plan with the given resource counts; only the counts
// matter to the quota pre-check.
func quotaPlan(secrets, orders, containers int) *barbicanplan.Plan {
	p := &barbicanplan.Plan{Scenario: "quota", Seed: 42}
	for i := range secrets {
		p.Secrets = append(p.Secrets, barbicanplan.Secret{Name: fmt.Sprintf("sec-%04d", i+1), PayloadBytes: 64})
	}
	for i := range orders {
		p.Orders = append(p.Orders, barbicanplan.Order{Name: fmt.Sprintf("ord-%04d", i+1)})
	}
	for i := range containers {
		p.Containers = append(p.Containers, barbicanplan.Container{Name: fmt.Sprintf("ctr-%04d", i+1), Secrets: []string{}})
	}
	return p
}

// limit returns a pointer to n, a quota limit that is present in the answer.
func limit(n int) *int { return &n }

func TestPlanNeedsCountsGeneratedSecrets(t *testing.T) {
	got := planNeeds(quotaPlan(10, 5, 3))
	if want := (needs{secrets: 15, orders: 5, containers: 3}); got != want {
		t.Errorf("planNeeds = %+v, want %+v (every order generates a secret)", got, want)
	}
}

func TestCheckQuota(t *testing.T) {
	small := planNeeds(quotaPlan(10, 5, 3))
	for _, tc := range []struct {
		name string
		need needs
		q    quotaSet
		want string // "" for no error
	}{
		{"unlimited", small, quotaSet{Secrets: limit(-1), Orders: limit(-1), Containers: limit(-1)}, ""},
		{"exactly at the limit", small, quotaSet{Secrets: limit(15), Orders: limit(5), Containers: limit(3)}, ""},
		{"secrets over", small, quotaSet{Secrets: limit(12), Orders: limit(-1), Containers: limit(-1)},
			"plan exceeds project quota; raise these quotas before applying: secrets need 15, quota 12"},
		{"several over", small, quotaSet{Secrets: limit(12), Orders: limit(4), Containers: limit(2)},
			"plan exceeds project quota; raise these quotas before applying: secrets need 15, quota 12; orders need 5, quota 4; containers need 3, quota 2"},
		{"absent secrets key unchecked", small, quotaSet{Orders: limit(5), Containers: limit(3)}, ""},
		{"absent keys leave the rest checked", small, quotaSet{Orders: limit(4)},
			"plan exceeds project quota; raise these quotas before applying: orders need 5, quota 4"},
		{"empty plan against zero quotas", planNeeds(quotaPlan(0, 0, 0)), quotaSet{Secrets: limit(0), Orders: limit(0), Containers: limit(0)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkQuota(tc.need, tc.q)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("checkQuota = %v, want nil", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("checkQuota = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestPrecheckQuota confirms the pre-check reads GET /v1/quotas, checks the plan
// against the answer, skips with a warning on a 403, and aborts on any other
// read failure, including an answer that is not JSON or carries no quotas
// object.
func TestPrecheckQuota(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantErr  string // substring of the error; "" for nil
		wantWarn bool
	}{
		{"within quota", http.StatusOK,
			`{"quotas":{"secrets":-1,"orders":-1,"containers":-1,"consumers":-1,"cas":-1}}`, "", false},
		{"over quota", http.StatusOK,
			`{"quotas":{"secrets":12,"orders":-1,"containers":-1}}`,
			"plan exceeds project quota; raise these quotas before applying: secrets need 15, quota 12", false},
		{"null limit unchecked", http.StatusOK,
			`{"quotas":{"secrets":null,"orders":5,"containers":3}}`, "", false},
		{"403 skips", http.StatusForbidden,
			`{"code":403,"title":"Forbidden"}`, "", true},
		{"500 aborts", http.StatusInternalServerError,
			`{"code":500}`, "reading Barbican quotas for pre-check", false},
		{"malformed JSON aborts", http.StatusOK,
			`{"quotas":`, "reading Barbican quotas for pre-check", false},
		{"no quotas object aborts", http.StatusOK,
			`{}`, "reading Barbican quotas for pre-check: response carries no quotas object", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			var requests []string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				writeJSON(w, tc.status, tc.body)
			}))
			defer ts.Close()

			err := PrecheckQuota(context.Background(), testKV(ts), quotaPlan(10, 5, 3))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("PrecheckQuota = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("PrecheckQuota = %v, want an error containing %q", err, tc.wantErr)
			}
			if want := []string{"GET /v1/quotas"}; fmt.Sprint(requests) != fmt.Sprint(want) {
				t.Errorf("requests = %v, want %v", requests, want)
			}
			warned := strings.Contains(logs.String(), "quota pre-check skipped: quota read denied")
			if warned != tc.wantWarn {
				t.Errorf("skip warning logged = %v, want %v; log %q", warned, tc.wantWarn, logs.String())
			}
		})
	}
}
