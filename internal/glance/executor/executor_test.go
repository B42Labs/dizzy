package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"

	"github.com/B42Labs/dizzy/internal/glance"
	"github.com/B42Labs/dizzy/internal/glance/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeGlance records every operation the executor drives, so tests can assert on
// call order and coverage. It is the in-test implementation of the executor's
// Glance seam. failCreate names a logical image whose create returns an error.
type fakeGlance struct {
	mu     sync.Mutex
	events []string

	failCreate string // logical name whose create returns err
	err        error
	quota      bool // when set, failCreate's error is glance.ErrQuota

	uploadSeeds map[string]int64 // logical name -> the seed the upload was handed
}

func newFakeGlance() *fakeGlance {
	return &fakeGlance{uploadSeeds: map[string]int64{}}
}

func (f *fakeGlance) record(ev string) {
	f.mu.Lock()
	f.events = append(f.events, ev)
	f.mu.Unlock()
}

func (f *fakeGlance) createErr(logical string) error {
	if logical != f.failCreate {
		return nil
	}
	if f.quota {
		return glance.ErrQuota
	}
	if f.err != nil {
		return f.err
	}
	return context.DeadlineExceeded
}

func res(logical string) resource.Resource {
	return resource.Resource{Kind: glance.KindImage, Logical: logical, Name: "dizzy-run0-" + logical, ID: "image-" + logical}
}

func (f *fakeGlance) CreateImage(_ context.Context, img plan.Image) (resource.Resource, error) {
	f.record("create:" + img.Name)
	if err := f.createErr(img.Name); err != nil {
		return resource.Resource{}, err
	}
	return res(img.Name), nil
}

func (f *fakeGlance) UploadImageData(_ context.Context, r resource.Resource, _ int, seed int64) error {
	f.mu.Lock()
	f.uploadSeeds[r.Logical] = seed
	f.mu.Unlock()
	f.record("upload:" + r.Logical)
	return nil
}

func (f *fakeGlance) ImageOwner(_ context.Context, r resource.Resource) (string, error) {
	f.record("owner:" + r.Logical)
	return "project-owner", nil
}
func (f *fakeGlance) AddImageProperties(_ context.Context, r resource.Resource) error {
	f.record("meta-add:" + r.Logical)
	return nil
}
func (f *fakeGlance) ChurnImageProperties(_ context.Context, r resource.Resource) error {
	f.record("meta-churn:" + r.Logical)
	return nil
}
func (f *fakeGlance) SetImageVisibility(_ context.Context, r resource.Resource, v images.ImageVisibility) error {
	f.record("visibility:" + r.Logical + "/" + string(v))
	return nil
}
func (f *fakeGlance) AddImageMember(_ context.Context, r resource.Resource, member string) error {
	f.record("member-add:" + r.Logical + "/" + member)
	return nil
}
func (f *fakeGlance) AcceptImageMember(_ context.Context, r resource.Resource, member string) error {
	f.record("member-accept:" + r.Logical + "/" + member)
	return nil
}
func (f *fakeGlance) RemoveImageMember(_ context.Context, r resource.Resource, member string) error {
	f.record("member-remove:" + r.Logical + "/" + member)
	return nil
}
func (f *fakeGlance) DeactivateImage(_ context.Context, r resource.Resource) error {
	f.record("deactivate:" + r.Logical)
	return nil
}
func (f *fakeGlance) ReactivateImage(_ context.Context, r resource.Resource) error {
	f.record("reactivate:" + r.Logical)
	return nil
}
func (f *fakeGlance) Delete(_ context.Context, r resource.Resource) error {
	f.record("delete:" + r.Logical)
	return nil
}
func (f *fakeGlance) WaitForReady(context.Context, resource.Resource) error { return nil }
func (f *fakeGlance) WaitForGone(context.Context, resource.Resource) error  { return nil }

func (f *fakeGlance) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// indexOf returns the position of the first event equal to want, or -1.
func indexOf(events []string, want string) int {
	for i, e := range events {
		if e == want {
			return i
		}
	}
	return -1
}

// indexOfPrefix returns the position of the first event whose prefix matches, or
// -1.
func indexOfPrefix(events []string, prefix string) int {
	for i, e := range events {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			return i
		}
	}
	return -1
}

// singleImagePlan builds a one-image plan carrying the given lifecycle flags.
func singleImagePlan(mutate func(*plan.Image)) *plan.Plan {
	img := plan.Image{Name: "img-0001", SizeMiB: 4}
	if mutate != nil {
		mutate(&img)
	}
	return &plan.Plan{Scenario: "t", Seed: 5, Images: []plan.Image{img}}
}

func TestApplyStageOrdering(t *testing.T) {
	f := newFakeGlance()
	p := singleImagePlan(func(img *plan.Image) { img.MetadataUpdate = true; img.Delete = true })
	if _, err := Apply(context.Background(), f, p, 4, time.Second, p.Seed); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	e := f.snapshot()
	order := []string{"create:img-0001", "upload:img-0001", "meta-add:img-0001", "delete:img-0001"}
	prev := -1
	for _, ev := range order {
		i := indexOf(e, ev)
		if i < 0 {
			t.Fatalf("event %q not found in %v", ev, e)
		}
		if i <= prev {
			t.Errorf("event %q at %d is not after the previous stage (%d): %v", ev, i, prev, e)
		}
		prev = i
	}
}

func TestApplyPerImageSequence(t *testing.T) {
	f := newFakeGlance()
	p := singleImagePlan(func(img *plan.Image) {
		img.MetadataUpdate = true
		img.Shared = true
		img.MemberAccept = true
		img.MemberRemove = true
		img.Deactivate = true
		img.Community = true
		img.Public = true
	})
	if _, err := Apply(context.Background(), f, p, 1, time.Second, p.Seed); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	e := f.snapshot()
	// metadata add/churn -> shared visibility + member add/accept/remove (all while
	// still shared) -> deactivate/reactivate -> community -> public.
	want := []string{
		"meta-add:img-0001",
		"meta-churn:img-0001",
		"visibility:img-0001/shared",
		"member-add:img-0001/project-owner",
		"member-accept:img-0001/project-owner",
		"member-remove:img-0001/project-owner",
		"deactivate:img-0001",
		"reactivate:img-0001",
		"visibility:img-0001/community",
		"visibility:img-0001/public",
	}
	prev := -1
	for _, ev := range want {
		i := indexOf(e, ev)
		if i < 0 {
			t.Fatalf("event %q not found in %v", ev, e)
		}
		if i <= prev {
			t.Errorf("event %q at %d is out of order: %v", ev, i, e)
		}
		prev = i
	}
}

func TestApplySkipsMemberOpsOnUnsharedImages(t *testing.T) {
	f := newFakeGlance()
	// Community without Shared: member operations must never run, since Glance
	// rejects members on a non-shared image.
	p := singleImagePlan(func(img *plan.Image) { img.Community = true })
	if _, err := Apply(context.Background(), f, p, 1, time.Second, p.Seed); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	e := f.snapshot()
	if indexOfPrefix(e, "member-") >= 0 {
		t.Errorf("member operation ran on an unshared image: %v", e)
	}
	if indexOf(e, "visibility:img-0001/community") < 0 {
		t.Errorf("community flip did not run: %v", e)
	}
}

func TestApplyQuotaFailsFast(t *testing.T) {
	f := &fakeGlance{failCreate: "img-0002", quota: true, uploadSeeds: map[string]int64{}}
	p := &plan.Plan{Scenario: "t", Seed: 1, Images: []plan.Image{
		{Name: "img-0001", SizeMiB: 1},
		{Name: "img-0002", SizeMiB: 1},
	}}
	result, err := Apply(context.Background(), f, p, 1, time.Second, p.Seed)
	if err == nil {
		t.Fatal("Apply() = nil, want a quota error")
	}
	// img-0001 was created before img-0002's create hit quota; it is recorded so
	// cleanup can reclaim it.
	if len(result.Created) == 0 {
		t.Error("Created is empty; a partially-applied plan must record what it made")
	}
}

func TestApplyPartialCreatedHonesty(t *testing.T) {
	f := &fakeGlance{failCreate: "img-0002", err: context.DeadlineExceeded, uploadSeeds: map[string]int64{}}
	p := &plan.Plan{Scenario: "t", Seed: 1, Images: []plan.Image{
		{Name: "img-0001", SizeMiB: 1},
		{Name: "img-0002", SizeMiB: 1},
	}}
	result, err := Apply(context.Background(), f, p, 1, time.Second, p.Seed)
	if err == nil {
		t.Fatal("Apply() = nil, want the create error")
	}
	for _, rr := range result.Created {
		if rr.ID == "" {
			t.Error("Created must not contain zero-id placeholder resources")
		}
		if rr.Logical == "img-0002" {
			t.Error("failed image should not be in Created")
		}
	}
}

// TestApplyUploadUsesDeterministicSeed confirms the payload seed the upload is
// handed is derived from the plan seed and the logical name, so two runs of the
// same plan push byte-identical data.
func TestApplyUploadUsesDeterministicSeed(t *testing.T) {
	p := singleImagePlan(nil)
	f1 := newFakeGlance()
	if _, err := Apply(context.Background(), f1, p, 1, time.Second, p.Seed); err != nil {
		t.Fatalf("Apply() #1 = %v", err)
	}
	f2 := newFakeGlance()
	if _, err := Apply(context.Background(), f2, p, 1, time.Second, p.Seed); err != nil {
		t.Fatalf("Apply() #2 = %v", err)
	}
	got1, got2 := f1.uploadSeeds["img-0001"], f2.uploadSeeds["img-0001"]
	if got1 != got2 {
		t.Errorf("upload seed differed across runs: %d vs %d", got1, got2)
	}
	if want := glance.PayloadSeed(p.Seed, "img-0001"); got1 != want {
		t.Errorf("upload seed = %d, want PayloadSeed(seed, name) = %d", got1, want)
	}
}
