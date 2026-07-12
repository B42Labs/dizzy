package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/nova"
	"github.com/B42Labs/dizzy/internal/nova/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// fakeNova records every operation the executor drives, so tests can assert on
// call order and coverage. It is the in-test implementation of the executor's
// Nova seam. failCreate names a logical resource whose create returns an error.
type fakeNova struct {
	mu     sync.Mutex
	events []string

	failCreate string // logical name whose create returns err
	err        error
	quota      bool // when set, failCreate's error is nova.ErrQuota

	serverStatusErrWant string // the WaitForServerStatus want that returns serverStatusErr
	serverStatusErr     error
}

func (f *fakeNova) record(ev string) {
	f.mu.Lock()
	f.events = append(f.events, ev)
	f.mu.Unlock()
}

func (f *fakeNova) createErr(logical string) error {
	if logical != f.failCreate {
		return nil
	}
	if f.quota {
		return nova.ErrQuota
	}
	if f.err != nil {
		return f.err
	}
	return context.DeadlineExceeded
}

func res(kind resource.Kind, logical string) resource.Resource {
	return resource.Resource{Kind: kind, Logical: logical, Name: "dizzy-run0-" + logical, ID: string(kind) + "-" + logical}
}

func (f *fakeNova) CreateNetwork(_ context.Context, n plan.Network) (resource.Resource, error) {
	f.record("createNetwork:" + n.Name)
	if err := f.createErr(n.Name); err != nil {
		return resource.Resource{}, err
	}
	return res(nova.KindNetwork, n.Name), nil
}

func (f *fakeNova) CreateSubnet(_ context.Context, n plan.Network, _ string) (resource.Resource, error) {
	f.record("createSubnet:" + n.Subnet)
	return res(nova.KindSubnet, n.Subnet), nil
}

func (f *fakeNova) CreateVolume(_ context.Context, v plan.Volume) (resource.Resource, error) {
	f.record("createVolume:" + v.Name)
	if err := f.createErr(v.Name); err != nil {
		return resource.Resource{}, err
	}
	return res(nova.KindVolume, v.Name), nil
}

func (f *fakeNova) CreatePort(_ context.Context, pt plan.Port, _ string) (resource.Resource, error) {
	f.record("createPort:" + pt.Name)
	return res(nova.KindPort, pt.Name), nil
}

func (f *fakeNova) CreateServer(_ context.Context, s plan.Server, _ nova.BootSpec) (resource.Resource, error) {
	f.record("createServer:" + s.Name)
	if err := f.createErr(s.Name); err != nil {
		return resource.Resource{}, err
	}
	return res(nova.KindServer, s.Name), nil
}

func (f *fakeNova) StopServer(_ context.Context, r resource.Resource) error {
	f.record("stop:" + r.Logical)
	return nil
}
func (f *fakeNova) StartServer(_ context.Context, r resource.Resource) error {
	f.record("start:" + r.Logical)
	return nil
}
func (f *fakeNova) RebootServerHard(_ context.Context, r resource.Resource) error {
	f.record("reboot:" + r.Logical)
	return nil
}
func (f *fakeNova) ResizeServer(_ context.Context, r resource.Resource, _ string) error {
	f.record("resize:" + r.Logical)
	return nil
}
func (f *fakeNova) ConfirmResizeServer(_ context.Context, r resource.Resource) error {
	f.record("confirm-resize:" + r.Logical)
	return nil
}
func (f *fakeNova) LiveMigrateServer(_ context.Context, r resource.Resource) error {
	f.record("live-migrate:" + r.Logical)
	return nil
}
func (f *fakeNova) AttachVolume(_ context.Context, s, v resource.Resource) error {
	f.record("attachVolume:" + s.Logical + "/" + v.Logical)
	return nil
}
func (f *fakeNova) DetachVolume(_ context.Context, s, v resource.Resource) error {
	f.record("detachVolume:" + s.Logical + "/" + v.Logical)
	return nil
}
func (f *fakeNova) AttachPort(_ context.Context, s, p resource.Resource) error {
	f.record("attachPort:" + s.Logical + "/" + p.Logical)
	return nil
}
func (f *fakeNova) DetachPort(_ context.Context, s, p resource.Resource) error {
	f.record("detachPort:" + s.Logical + "/" + p.Logical)
	return nil
}
func (f *fakeNova) Delete(_ context.Context, r resource.Resource) error {
	f.record("delete:" + r.Logical)
	return nil
}
func (f *fakeNova) WaitForReady(context.Context, resource.Resource) error { return nil }
func (f *fakeNova) WaitForServerStatus(_ context.Context, _ resource.Resource, want string) error {
	if f.serverStatusErr != nil && want == f.serverStatusErrWant {
		return f.serverStatusErr
	}
	return nil
}
func (f *fakeNova) WaitForVolumeStatus(context.Context, resource.Resource, string) error {
	return nil
}
func (f *fakeNova) WaitForGone(context.Context, resource.Resource) error { return nil }

func (f *fakeNova) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// indexOf returns the position of the first event whose prefix matches, or -1.
func indexOf(events []string, prefix string) int {
	for i, e := range events {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			return i
		}
	}
	return -1
}

// singleServerPlan builds a one-server, one-network plan carrying the given
// lifecycle operations, one data volume and one port both attached and detached.
func singleServerPlan(mutate func(*plan.Server)) *plan.Plan {
	s := plan.Server{Name: "srv-0001", Networks: []string{"net-0001"}}
	if mutate != nil {
		mutate(&s)
	}
	return &plan.Plan{
		Scenario:     "t",
		Image:        "cirros",
		Flavor:       "m1.tiny",
		ResizeFlavor: "m1.small",
		Networks:     []plan.Network{{Name: "net-0001", Subnet: "sub-0001", CIDR: "10.0.1.0/24"}},
		Servers:      []plan.Server{s},
		Volumes:      []plan.Volume{{Name: "vol-0001", SizeGiB: 1, Server: "srv-0001", Detach: true}},
		Ports:        []plan.Port{{Name: "port-0001", Network: "net-0001", Server: "srv-0001", Detach: true}},
	}
}

func TestApplyStageOrdering(t *testing.T) {
	f := &fakeNova{}
	p := singleServerPlan(func(s *plan.Server) { s.Delete = true })
	if _, err := Apply(context.Background(), f, p, 4, time.Second, Resolved{}); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	e := f.snapshot()
	order := []string{"createNetwork:", "createVolume:", "createPort:", "createServer:", "attachVolume:", "delete:"}
	prev := -1
	for _, prefix := range order {
		i := indexOf(e, prefix)
		if i < 0 {
			t.Fatalf("event %q not found in %v", prefix, e)
		}
		if i <= prev {
			t.Errorf("event %q at %d is not after the previous stage (%d): %v", prefix, i, prev, e)
		}
		prev = i
	}
}

func TestApplyPerServerSoftSequence(t *testing.T) {
	f := &fakeNova{}
	p := singleServerPlan(func(s *plan.Server) {
		s.StopStart = plan.StopStartSoft
		s.Resize = true
		s.LiveMigrate = true
	})
	if _, err := Apply(context.Background(), f, p, 1, time.Second, Resolved{LiveMigration: true}); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	e := f.snapshot()
	// attach volume/port → stop → start → resize → confirm → live-migrate → detach.
	want := []string{
		"attachVolume:srv-0001/vol-0001",
		"attachPort:srv-0001/port-0001",
		"stop:srv-0001",
		"start:srv-0001",
		"resize:srv-0001",
		"confirm-resize:srv-0001",
		"live-migrate:srv-0001",
		"detachPort:srv-0001/port-0001",
		"detachVolume:srv-0001/vol-0001",
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

func TestApplyHardStopStartUsesReboot(t *testing.T) {
	f := &fakeNova{}
	p := singleServerPlan(func(s *plan.Server) { s.StopStart = plan.StopStartHard })
	if _, err := Apply(context.Background(), f, p, 1, time.Second, Resolved{}); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	e := f.snapshot()
	if indexOf(e, "reboot:srv-0001") < 0 {
		t.Errorf("hard stop/start did not use reboot: %v", e)
	}
	if indexOf(e, "stop:srv-0001") >= 0 {
		t.Errorf("hard stop/start unexpectedly used stop: %v", e)
	}
}

func TestApplySkipsLiveMigrationWhenDisabled(t *testing.T) {
	f := &fakeNova{}
	p := singleServerPlan(func(s *plan.Server) { s.LiveMigrate = true })
	if _, err := Apply(context.Background(), f, p, 1, time.Second, Resolved{LiveMigration: false}); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	if indexOf(f.snapshot(), "live-migrate:") >= 0 {
		t.Error("live migration was attempted despite being disabled for the run")
	}
}

func TestApplyBootActiveDeadlineIsFatal(t *testing.T) {
	// The boot ACTIVE wait times out. A server that never reaches ACTIVE cannot
	// be driven, so the run must fail here rather than tolerate the deadline and
	// issue lifecycle ops against a still-BUILDING instance.
	f := &fakeNova{serverStatusErrWant: statusServerActive, serverStatusErr: context.DeadlineExceeded}
	p := singleServerPlan(nil)
	if _, err := Apply(context.Background(), f, p, 1, time.Second, Resolved{}); err == nil {
		t.Fatal("Apply() = nil, want a boot-timeout error when the server never reaches ACTIVE")
	}
	if i := indexOf(f.snapshot(), "attachVolume:"); i >= 0 {
		t.Errorf("lifecycle sequence ran against a server that never reached ACTIVE: %v", f.snapshot())
	}
}

func TestRunStagePrefersRealFailureOverCanceledSibling(t *testing.T) {
	// One item fails with a real error, cancelling the stage; a sibling still in
	// flight then returns context.Canceled at an earlier index. runStage must
	// surface the real failure, not the arbitrary cancelled sibling.
	realErr := errors.New("invalid flavor")
	started := make(chan struct{})
	_, err := runStage(context.Background(), []int{0, 1}, 2, func(ctx context.Context, i int) (int, error) {
		if i == 0 {
			close(started) // signal the sibling is running before the failure fires
			<-ctx.Done()   // block until the real failure cancels the stage
			return 0, ctx.Err()
		}
		<-started // ensure the sibling is mid-flight so its error is context.Canceled
		return 0, realErr
	})
	if !errors.Is(err, realErr) {
		t.Fatalf("runStage() = %v, want the real failure %v masked by a cancelled sibling", err, realErr)
	}
}

func TestApplyQuotaFailsFast(t *testing.T) {
	f := &fakeNova{failCreate: "srv-0001", quota: true}
	p := singleServerPlan(nil)
	result, err := Apply(context.Background(), f, p, 1, time.Second, Resolved{})
	if err == nil {
		t.Fatal("Apply() = nil, want a quota error")
	}
	// The networks, subnet, volume, and port were created before the server boot
	// failed on quota; those are recorded so cleanup can reclaim them.
	if len(result.Created) == 0 {
		t.Error("Created is empty; a partially-applied plan must record what it made")
	}
}

func TestApplyPartialCreatedHonesty(t *testing.T) {
	f := &fakeNova{failCreate: "vol-0001", err: context.DeadlineExceeded}
	// A non-retryable create failure on the volume stops the run at stage 2; the
	// network and subnet from stage 1 are already recorded.
	p := singleServerPlan(nil)
	result, err := Apply(context.Background(), f, p, 1, time.Second, Resolved{})
	if err == nil {
		t.Fatal("Apply() = nil, want the volume create error")
	}
	// The network resource (stage 1) is present; the failed volume is not, and no
	// zero-id placeholder leaks into the record.
	var haveNet, haveVol bool
	for _, rr := range result.Created {
		if rr.ID == "" {
			t.Error("Created must not contain zero-id placeholder resources")
		}
		if rr.Kind == nova.KindNetwork {
			haveNet = true
		}
		if rr.Kind == nova.KindVolume {
			haveVol = true
		}
	}
	if !haveNet {
		t.Error("stage-1 network missing from Created")
	}
	if haveVol {
		t.Error("failed volume should not be in Created")
	}
}
