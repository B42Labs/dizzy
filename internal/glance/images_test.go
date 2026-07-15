package glance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/resource"
)

// testClient builds a Client whose Glance service calls hit ts.
func testClient(ts *httptest.Server) *Client {
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       ts.URL + "/",
	}
	return New(gc, "run0", metrics.NewCollector())
}

func imageRes(id string) resource.Resource {
	return resource.Resource{Kind: KindImage, Logical: "img-0001", Name: "dizzy-run0-img-0001", ID: id}
}

// TestDeactivateReactivateHitTheActionEndpoints confirms the hand-rolled raw
// POSTs target the correct actions endpoints with the POST method and tolerate a
// 204 — the riskiest code in the client, since gophercloud has no typed helper
// for these verbs.
func TestDeactivateReactivateHitTheActionEndpoints(t *testing.T) {
	var gotMethod, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	c := testClient(ts)

	if err := c.DeactivateImage(context.Background(), imageRes("img1")); err != nil {
		t.Fatalf("DeactivateImage: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/images/img1/actions/deactivate" {
		t.Errorf("deactivate hit %s %s, want POST /images/img1/actions/deactivate", gotMethod, gotPath)
	}

	if err := c.ReactivateImage(context.Background(), imageRes("img1")); err != nil {
		t.Fatalf("ReactivateImage: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/images/img1/actions/reactivate" {
		t.Errorf("reactivate hit %s %s, want POST /images/img1/actions/reactivate", gotMethod, gotPath)
	}
}

// TestMemberCallsHitTheMemberEndpoints confirms the typed member wrappers target
// the correct per-image member endpoints with the right methods, and that the
// accept step sends status "accepted" — the self-share status the whole design
// hinges on, otherwise only ever exercised against a live cloud.
func TestMemberCallsHitTheMemberEndpoints(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, string(body)
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"member_id":"proj1","image_id":"img1","status":"accepted"}`)
	}))
	defer ts.Close()
	c := testClient(ts)

	if err := c.AddImageMember(context.Background(), imageRes("img1"), "proj1"); err != nil {
		t.Fatalf("AddImageMember: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/images/img1/members" {
		t.Errorf("add member hit %s %s, want POST /images/img1/members", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"member":"proj1"`) {
		t.Errorf("add member body = %s, want it to carry the member id", gotBody)
	}

	if err := c.AcceptImageMember(context.Background(), imageRes("img1"), "proj1"); err != nil {
		t.Fatalf("AcceptImageMember: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/images/img1/members/proj1" {
		t.Errorf("accept member hit %s %s, want PUT /images/img1/members/proj1", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"status":"accepted"`) {
		t.Errorf("accept member body = %s, want it to set status accepted", gotBody)
	}

	if err := c.RemoveImageMember(context.Background(), imageRes("img1"), "proj1"); err != nil {
		t.Fatalf("RemoveImageMember: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/images/img1/members/proj1" {
		t.Errorf("remove member hit %s %s, want DELETE /images/img1/members/proj1", gotMethod, gotPath)
	}
}

// TestImageActionSurfacesError confirms a non-204 from the action endpoint
// surfaces as an error rather than being silently swallowed.
func TestImageActionSurfacesError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer ts.Close()
	c := testClient(ts)

	if err := c.DeactivateImage(context.Background(), imageRes("img1")); err == nil {
		t.Fatal("DeactivateImage on a 409 = nil, want an error")
	}
}

// TestListImagesByTagFiltersServerSide confirms the list request carries the
// run's dizzy:run tag as the server-side tag query parameter, so only this run's
// images come back.
func TestListImagesByTagFiltersServerSide(t *testing.T) {
	var gotTags []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		gotTags = r.URL.Query()["tag"]
		_, _ = io.WriteString(w, `{"images":[{"id":"i1","name":"dizzy-run0-img-0001","status":"active"}]}`)
	}))
	defer ts.Close()
	c := testClient(ts)

	found, err := c.ListImagesByTag(context.Background(), "run0")
	if err != nil {
		t.Fatalf("ListImagesByTag: %v", err)
	}
	if len(gotTags) != 1 || gotTags[0] != "dizzy:run=run0" {
		t.Errorf("tag query = %v, want [dizzy:run=run0]", gotTags)
	}
	if len(found) != 1 || found[0].ID != "i1" {
		t.Errorf("found = %+v, want the single tagged image", found)
	}
}

// TestWaitForGoneTreatsPendingDeleteAsGone confirms a delayed_delete deployment,
// which parks a deleted image in pending_delete rather than removing it, does not
// spin WaitForGone to its deadline.
func TestWaitForGoneTreatsPendingDeleteAsGone(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"i1","name":"n","status":"pending_delete"}`)
	}))
	defer ts.Close()
	c := testClient(ts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WaitForGone(ctx, imageRes("i1")); err != nil {
		t.Errorf("WaitForGone on pending_delete = %v, want nil (gone)", err)
	}
}

// TestWaitForReadyAbortsOnKilled confirms a failed upload (status killed) aborts
// the readiness poll with a terminal error rather than spinning to the deadline.
func TestWaitForReadyAbortsOnKilled(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"i1","name":"n","status":"killed"}`)
	}))
	defer ts.Close()
	c := testClient(ts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.WaitForReady(ctx, imageRes("i1"))
	if err == nil {
		t.Fatal("WaitForReady on a killed image = nil, want a terminal error")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("WaitForReady spun to the deadline instead of aborting on killed")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("error %q does not name the terminal status", err)
	}
}
