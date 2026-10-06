package nova

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	novaplan "github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// testGroupClient builds a compute client for run1 whose calls hit a server
// answering with handler.
func testGroupClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	c := testComputeClient(ts)
	c.runID = "run1"
	return c
}

// respond writes status and body as a JSON response.
func respond(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// TestCreateServerGroup verifies the create request carries the deterministic
// name and the policy as a one-element list, and that the returned resource
// carries the kind, the logical and cloud names, and the id of the response.
func TestCreateServerGroup(t *testing.T) {
	var body string
	c := testGroupClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/os-server-groups" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		respond(w, http.StatusOK, `{"server_group":{"id":"grp-id-1","name":"dizzy-run1-grp-0001","policies":["soft-anti-affinity"]}}`)
	})

	got, err := c.CreateServerGroup(context.Background(), novaplan.ServerGroup{Name: "grp-0001", Policy: novaplan.PolicySoftAntiAffinity})
	if err != nil {
		t.Fatalf("CreateServerGroup = %v, want nil", err)
	}
	if want := `{"server_group":{"name":"dizzy-run1-grp-0001","policies":["soft-anti-affinity"]}}`; body != want {
		t.Errorf("posted body = %s, want %s", body, want)
	}
	want := resource.Resource{Kind: KindServerGroup, Logical: "grp-0001", Name: "dizzy-run1-grp-0001", ID: "grp-id-1"}
	if got != want {
		t.Errorf("CreateServerGroup = %+v, want %+v", got, want)
	}
}

// TestCreateServerGroupErrors verifies a quota rejection carries ErrQuota and
// any other failure is wrapped with the logical name.
func TestCreateServerGroupErrors(t *testing.T) {
	g := novaplan.ServerGroup{Name: "grp-0001", Policy: novaplan.PolicyAntiAffinity}

	t.Run("quota", func(t *testing.T) {
		c := testGroupClient(t, func(w http.ResponseWriter, _ *http.Request) {
			respond(w, http.StatusForbidden, `{"forbidden":{"code":403,"message":"Quota exceeded, too many server groups."}}`)
		})
		if _, err := c.CreateServerGroup(context.Background(), g); !errors.Is(err, ErrQuota) {
			t.Errorf("CreateServerGroup = %v, want an error carrying ErrQuota", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		c := testGroupClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, err := c.CreateServerGroup(context.Background(), g)
		if want := `creating server_group "grp-0001":`; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("CreateServerGroup = %v, want an error starting with %q", err, want)
		}
		var code gophercloud.ErrUnexpectedResponseCode
		if !errors.As(err, &code) || code.Actual != http.StatusInternalServerError {
			t.Errorf("CreateServerGroup = %v, want it to wrap the gophercloud 500", err)
		}
		if errors.Is(err, ErrQuota) {
			t.Errorf("CreateServerGroup = %v, want no ErrQuota for a 500", err)
		}
	})
}

// TestListServerGroupsByName verifies discovery keeps only the groups whose
// name carries the run's dizzy-<id>- prefix, so run1 never matches run10, and
// that an empty listing yields no group and no error.
func TestListServerGroupsByName(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []resource.Resource
	}{
		{
			name: "prefix match",
			body: `{"server_groups":[` +
				`{"id":"g1","name":"dizzy-run1-grp-0001"},` +
				`{"id":"g2","name":"dizzy-run10-grp-0001"},` +
				`{"id":"g3","name":"other"}]}`,
			want: []resource.Resource{{Kind: KindServerGroup, Name: "dizzy-run1-grp-0001", ID: "g1"}},
		},
		{name: "empty listing", body: `{"server_groups":[]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := testGroupClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/os-server-groups" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				respond(w, http.StatusOK, tc.body)
			})
			got, err := c.ListServerGroupsByName(context.Background(), "run1")
			if err != nil {
				t.Fatalf("ListServerGroupsByName = %v, want nil", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ListServerGroupsByName = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestListServerGroupsByNameErrors verifies a denied listing fails open with
// one warning, while any other failure is returned wrapped.
func TestListServerGroupsByNameErrors(t *testing.T) {
	t.Run("forbidden", func(t *testing.T) {
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })

		c := testGroupClient(t, func(w http.ResponseWriter, _ *http.Request) {
			respond(w, http.StatusForbidden, `{"forbidden":{"code":403,"message":"Policy doesn't allow os_compute_api:os-server-groups:index to be performed."}}`)
		})
		got, err := c.ListServerGroupsByName(context.Background(), "run1")
		if got != nil || err != nil {
			t.Errorf("ListServerGroupsByName = %v, %v, want nil, nil", got, err)
		}
		if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
			t.Errorf("got %d warnings, want 1:\n%s", n, logs.String())
		}
		if want := `msg="listing server groups denied; skipping server group discovery"`; !strings.Contains(logs.String(), want) {
			t.Errorf("warning lacks %s:\n%s", want, logs.String())
		}
	})

	t.Run("server error", func(t *testing.T) {
		c := testGroupClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, err := c.ListServerGroupsByName(context.Background(), "run1")
		if want := "listing server groups by name:"; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("ListServerGroupsByName = %v, want an error starting with %q", err, want)
		}
	})
}

// TestServerGroupObserveDeleteReady verifies a server group reads as existing
// without a status and as gone on a 404, is deleted by id, and is ready at
// once without a request.
func TestServerGroupObserveDeleteReady(t *testing.T) {
	grp := resource.Resource{Kind: KindServerGroup, Logical: "grp-0001", ID: "g1"}

	t.Run("observe exists", func(t *testing.T) {
		c := testGroupClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/os-server-groups/g1" {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			respond(w, http.StatusOK, `{"server_group":{"id":"g1","name":"dizzy-run1-grp-0001"}}`)
		})
		status, exists, err := c.Observe(context.Background(), grp)
		if status != "" || !exists || err != nil {
			t.Errorf("Observe = %q, %v, %v, want \"\", true, nil", status, exists, err)
		}
	})

	t.Run("observe gone", func(t *testing.T) {
		c := testGroupClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		status, exists, err := c.Observe(context.Background(), grp)
		if status != "" || exists || err != nil {
			t.Errorf("Observe = %q, %v, %v, want \"\", false, nil", status, exists, err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		var got string
		c := testGroupClient(t, func(w http.ResponseWriter, r *http.Request) {
			got = r.Method + " " + r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		})
		if err := c.Delete(context.Background(), grp); err != nil {
			t.Fatalf("Delete = %v, want nil", err)
		}
		if want := "DELETE /os-server-groups/g1"; got != want {
			t.Errorf("request = %q, want %q", got, want)
		}
	})

	t.Run("ready without a request", func(t *testing.T) {
		c := testGroupClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		})
		if err := c.WaitForReady(context.Background(), grp); err != nil {
			t.Errorf("WaitForReady = %v, want nil", err)
		}
	})
}
