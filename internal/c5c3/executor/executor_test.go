package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/B42Labs/dizzy/internal/c5c3"
	"github.com/B42Labs/dizzy/internal/c5c3/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// The names the fake reports for the project and the catalog entry's service,
// and the namespace its orders live in.
const (
	testNamespace = "dizzy-orders"
	testProject   = "dizzy-project"
	testDomain    = "dizzy-domain"
	testService   = "dizzy-probe-service"
)

// The prefixes of the secret values the fake delivers. No log line or error
// text may carry either.
const (
	userPasswordPrefix  = "fake-user-password-"
	appcredSecretPrefix = "fake-appcred-secret-"
)

// The bounds testPlan and the tests' opTimeout give the calls, each distinct so
// a test can tell from a deadline which bound a call ran under.
const (
	testOpTimeout    = 1 * time.Second
	testReadyTimeout = 2 * time.Second
	testGoneTimeout  = 3 * time.Second
	testGracePeriod  = 1500 * time.Millisecond
)

// fakeOrders records every call the executor drives, so tests can assert on
// call order and coverage. It is the in-test implementation of the executor's
// Orders seam. Each call is recorded as one event: its key, verb:<logical> for
// a call on an order and verb:<tag> for a login, where the tag is the
// credential's resourceVersion, followed by the call's arguments, all joined by
// colons. A call answers with the context's error once its context has ended,
// otherwise with the next error queued for its key in fail, otherwise with the
// error of the hook installed for its key, otherwise with success.
//
// The fake keeps the credential generation each Secret carries: a user's
// Secret carries the password fake-user-password-<gen> under the tag
// user-<gen>, an application credential's the id appcred-id-<gen> and the
// secret fake-appcred-secret-<gen> under the tag appcred-<gen>. A deleted
// Secret is written again at once and carries the tag suffix r until the next
// rotation, so a test can tell the restored credential from the one before.
type fakeOrders struct {
	mu     sync.Mutex
	events []string
	// changed is closed and replaced on every event, so waitFor can block
	// until the next one.
	changed chan struct{}
	// deadlines maps an event key to the time left on the context of its
	// first call, or -1 for a context without a deadline.
	deadlines map[string]time.Duration
	fail      map[string][]error
	hooks     map[string]func(context.Context) error

	gen      map[string]int64
	restored map[string]bool
	// made holds the logical name of every order a CreateOrder call created.
	made map[string]bool

	// graceLeft is how far ahead of a Status call the superseded application
	// credential's deletion lies after a rotation. previousGone reports the
	// superseded credential as already deleted instead. previousID, when set,
	// is the id Status reports for the superseded credential in place of the
	// id of the generation before, as after a second rotation.
	graceLeft    time.Duration
	previousGone bool
	previousID   string
}

func newFake() *fakeOrders {
	return &fakeOrders{
		changed:   make(chan struct{}),
		deadlines: map[string]time.Duration{},
		fail:      map[string][]error{},
		hooks:     map[string]func(context.Context) error{},
		gen:       map[string]int64{},
		restored:  map[string]bool{},
		made:      map[string]bool{},
		graceLeft: time.Hour,
	}
}

// failWith queues errs as the answers of the next calls recorded under key.
func (f *fakeOrders) failWith(key string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[key] = append(f.fail[key], errs...)
}

// on installs hook as the answer of every call recorded under key that no
// queued error answers. A nil answer lets the call return its usual result.
func (f *fakeOrders) on(key string, hook func(context.Context) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks[key] = hook
}

// record appends ev to the events, under f.mu.
func (f *fakeOrders) record(ev string) {
	f.events = append(f.events, ev)
	close(f.changed)
	f.changed = make(chan struct{})
}

// mark records ev without answering a call, so a hook can note when it
// returns.
func (f *fakeOrders) mark(ev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ev)
}

// call records the call key with its arguments as one event and answers it.
func (f *fakeOrders) call(ctx context.Context, key string, args ...string) error {
	f.mu.Lock()
	f.record(strings.Join(append([]string{key}, args...), ":"))
	if _, seen := f.deadlines[key]; !seen {
		left := time.Duration(-1)
		if d, ok := ctx.Deadline(); ok {
			left = time.Until(d)
		}
		f.deadlines[key] = left
	}
	var queued error
	if q := f.fail[key]; len(q) > 0 {
		queued, f.fail[key] = q[0], q[1:]
	}
	hook := f.hooks[key]
	f.mu.Unlock()

	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case queued != nil:
		return queued
	case hook != nil:
		return hook(ctx)
	}
	return nil
}

func (f *fakeOrders) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.events)
}

// waitFor blocks until cond holds for the events recorded so far, and returns
// ctx.Err() when ctx ends first.
func (f *fakeOrders) waitFor(ctx context.Context, cond func([]string) bool) error {
	for {
		f.mu.Lock()
		ok, changed := cond(f.events), f.changed
		f.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// credential returns what the Secret beside the order logical carries now.
// The caller holds f.mu.
func (f *fakeOrders) credential(logical string) c5c3.Credential {
	gen := f.gen[logical]
	tag := logical + "-" + strconv.FormatInt(gen, 10)
	if f.restored[logical] {
		tag += "r"
	}
	if logical == "user" {
		password := userPasswordPrefix + strconv.FormatInt(gen, 10)
		return c5c3.Credential{
			CloudsYAML:      []byte("clouds:\n  openstack:\n    auth:\n      password: " + password + "\n"),
			Cloud:           "openstack",
			ResourceVersion: tag,
			Password:        password,
		}
	}
	secret := appcredSecretPrefix + strconv.FormatInt(gen, 10)
	return c5c3.Credential{
		CloudsYAML:                  []byte("clouds:\n  openstack:\n    auth:\n      application_credential_secret: " + secret + "\n"),
		Cloud:                       "openstack",
		ResourceVersion:             tag,
		ApplicationCredentialID:     appcredID(gen),
		ApplicationCredentialSecret: secret,
	}
}

// appcredID is the id of the application credential of generation gen.
func appcredID(gen int64) string {
	return "appcred-id-" + strconv.FormatInt(gen, 10)
}

// checkKind rejects a login whose kind does not match the credential: a
// password for a user, an application credential for a credential order.
func checkKind(kind resource.Kind, cred c5c3.Credential) error {
	if (cred.Password != "") != (kind == c5c3.KindUser) {
		return fmt.Errorf("fake: a %s login with %s", kind, cred)
	}
	return nil
}

func (f *fakeOrders) CreateOrder(ctx context.Context, o plan.Order) (resource.Resource, error) {
	if err := f.call(ctx, "create:"+o.Name); err != nil {
		return resource.Resource{}, err
	}
	f.mu.Lock()
	f.made[o.Name] = true
	f.mu.Unlock()
	name := "dizzyorder-run0-" + o.Name
	return resource.Resource{Kind: resource.Kind(o.Kind), Logical: o.Name, Name: name, ID: testNamespace + "/" + name}, nil
}

func (f *fakeOrders) WaitReady(ctx context.Context, r resource.Resource) error {
	return f.call(ctx, "waitready:"+r.Logical)
}

func (f *fakeOrders) WaitReason(ctx context.Context, r resource.Resource, reason string) error {
	return f.call(ctx, "waitreason:"+r.Logical, reason)
}

func (f *fakeOrders) WaitGone(ctx context.Context, r resource.Resource) error {
	return f.call(ctx, "waitgone:"+r.Logical)
}

func (f *fakeOrders) Delete(ctx context.Context, r resource.Resource) error {
	return f.call(ctx, "delete:"+r.Logical)
}

func (f *fakeOrders) Status(ctx context.Context, r resource.Resource) (c5c3.OrderStatus, error) {
	if err := f.call(ctx, "status:"+r.Logical); err != nil {
		return c5c3.OrderStatus{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st := c5c3.OrderStatus{Ready: true, Generation: f.gen[r.Logical], SpecGeneration: f.gen[r.Logical]}
	switch r.Logical {
	case "project":
		st.ProjectName, st.DomainName = testProject, testDomain
	case "catalog":
		st.ServiceName = testService
	case "appcred":
		gen := f.gen[r.Logical]
		st.CredentialID = appcredID(gen)
		if gen > 0 && !f.previousGone {
			st.PreviousCredentialID = appcredID(gen - 1)
			if f.previousID != "" {
				st.PreviousCredentialID = f.previousID
			}
			st.PreviousCredentialDeleteAt = time.Now().Add(f.graceLeft)
		}
	}
	return st, nil
}

func (f *fakeOrders) RaiseGeneration(ctx context.Context, r resource.Resource) (int64, error) {
	if err := f.call(ctx, "raise:"+r.Logical); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen[r.Logical] + 1, nil
}

func (f *fakeOrders) WaitRotated(ctx context.Context, r resource.Resource, generation int64, previous c5c3.Credential) (c5c3.Credential, error) {
	if err := f.call(ctx, "waitrotated:"+r.Logical, strconv.FormatInt(generation, 10), previous.ResourceVersion); err != nil {
		return c5c3.Credential{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gen[r.Logical] = generation
	f.restored[r.Logical] = false
	return f.credential(r.Logical), nil
}

func (f *fakeOrders) WaitPreviousGone(ctx context.Context, r resource.Resource) error {
	return f.call(ctx, "waitprevgone:"+r.Logical)
}

func (f *fakeOrders) ReadCredential(ctx context.Context, order resource.Resource) (c5c3.Credential, error) {
	if err := f.call(ctx, "read:"+order.Logical); err != nil {
		return c5c3.Credential{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.credential(order.Logical), nil
}

func (f *fakeOrders) EditSecret(ctx context.Context, order resource.Resource) error {
	return f.call(ctx, "editsecret:"+order.Logical)
}

func (f *fakeOrders) DeleteSecret(ctx context.Context, order resource.Resource) error {
	if err := f.call(ctx, "deletesecret:"+order.Logical); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restored[order.Logical] = true
	return nil
}

func (f *fakeOrders) WaitSecretRestored(ctx context.Context, order resource.Resource, want c5c3.Credential) error {
	return f.call(ctx, "waitrestored:"+order.Logical, want.ResourceVersion)
}

func (f *fakeOrders) Authenticate(ctx context.Context, kind resource.Kind, cred c5c3.Credential, wantProject string) error {
	if err := f.call(ctx, "auth:"+cred.ResourceVersion, wantProject); err != nil {
		return err
	}
	return checkKind(kind, cred)
}

func (f *fakeOrders) VerifyRole(ctx context.Context, cred c5c3.Credential, projectName, domainName, role string) error {
	return f.call(ctx, "verifyrole:"+cred.ResourceVersion, projectName, domainName, role)
}

func (f *fakeOrders) VerifyCatalog(ctx context.Context, cred c5c3.Credential, projectName, domainName, serviceType, serviceName, endpointURL string) error {
	return f.call(ctx, "verifycatalog:"+cred.ResourceVersion, projectName, domainName, serviceType, serviceName, endpointURL)
}

func (f *fakeOrders) Reject(ctx context.Context, kind resource.Kind, cred c5c3.Credential) error {
	if err := f.call(ctx, "reject:"+cred.ResourceVersion); err != nil {
		return err
	}
	return checkKind(kind, cred)
}

// testPlan is the plan scenario.Generate emits for the small profile, with
// bounds short enough for a test.
func testPlan() *plan.Plan {
	return &plan.Plan{
		Scenario:     "small",
		Namespace:    testNamespace,
		ControlPlane: plan.ControlPlane{Name: "controlplane", Namespace: "openstack"},
		Orders: []plan.Order{
			{Kind: plan.KindUser, Name: "user"},
			{Kind: plan.KindProject, Name: "project"},
			{Kind: plan.KindRoleAssignment, Name: "role", User: "user", Project: "project", Role: "member"},
			{Kind: plan.KindCatalogEntry, Name: "catalog", ServiceType: "dizzy-probe", EndpointURL: "https://dizzy-probe.invalid/v1"},
			{Kind: plan.KindApplicationCredential, Name: "appcred", User: "user", Project: "project"},
		},
		Refusals: []plan.Refusal{
			{
				Order:  plan.Order{Kind: plan.KindApplicationCredential, Name: "refused-appcred", User: "user", Project: "project"},
				Reason: "NoRoleOnProject",
			},
			{
				Order:  plan.Order{Kind: plan.KindRoleAssignment, Name: "refused-role", User: "user", Project: "project", Role: "dizzy-refused"},
				Reason: "RoleNotAllowed",
			},
		},
		GracePeriod:  testGracePeriod,
		ReadyTimeout: testReadyTimeout,
		GoneTimeout:  testGoneTimeout,
		SecretRepair: true,
	}
}

// fullRun is every call a successful Apply of testPlan makes at concurrency 1,
// stage by stage.
var fullRun = [][]string{
	{
		"create:user", "waitready:user", "create:project", "waitready:project",
		"read:user", "auth:user-0:",
	},
	{
		"create:refused-appcred", "waitreason:refused-appcred:NoRoleOnProject",
		"create:refused-role", "waitreason:refused-role:RoleNotAllowed",
		"delete:refused-appcred", "delete:refused-role",
		"waitgone:refused-appcred", "waitgone:refused-role",
	},
	{
		"create:role", "waitready:role", "create:catalog", "waitready:catalog",
		"status:project", "status:catalog",
		"verifyrole:user-0:dizzy-project:dizzy-domain:member",
		"verifycatalog:user-0:dizzy-project:dizzy-domain:dizzy-probe:dizzy-probe-service:https://dizzy-probe.invalid/v1",
	},
	{
		"create:appcred", "waitready:appcred", "read:appcred", "auth:appcred-0:dizzy-project",
	},
	{
		"editsecret:user", "waitrestored:user:user-0", "deletesecret:user", "waitrestored:user:user-0",
		"read:user", "auth:user-0r:",
		"editsecret:appcred", "waitrestored:appcred:appcred-0", "deletesecret:appcred", "waitrestored:appcred:appcred-0",
		"read:appcred", "auth:appcred-0r:dizzy-project",
	},
	{
		"raise:user", "waitrotated:user:1:user-0r", "auth:user-1:", "reject:user-0r",
	},
	{
		"raise:appcred", "waitrotated:appcred:1:appcred-0r", "status:appcred",
		"auth:appcred-0r:dizzy-project", "waitprevgone:appcred", "reject:appcred-0r",
	},
}

// orderKinds maps each logical name of testPlan to its kind.
var orderKinds = map[string]string{
	"user":            plan.KindUser,
	"project":         plan.KindProject,
	"role":            plan.KindRoleAssignment,
	"catalog":         plan.KindCatalogEntry,
	"appcred":         plan.KindApplicationCredential,
	"refused-appcred": plan.KindApplicationCredential,
	"refused-role":    plan.KindRoleAssignment,
}

// orders returns the resources the fake creates for logicals, in order.
func orders(logicals ...string) []resource.Resource {
	out := make([]resource.Resource, 0, len(logicals))
	for _, l := range logicals {
		name := "dizzyorder-run0-" + l
		out = append(out, resource.Resource{Kind: resource.Kind(orderKinds[l]), Logical: l, Name: name, ID: testNamespace + "/" + name})
	}
	return out
}

// allFive is Result.Created of a run that got past stage 4.
var allFive = orders("user", "project", "role", "catalog", "appcred")

// setProberInterval sets proberInterval to d for the rest of the test.
func setProberInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := proberInterval
	proberInterval = d
	t.Cleanup(func() { proberInterval = prev })
}

// captureLogs sends the default logger's JSON output, every level included,
// to the returned buffer for the rest of the test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// syncBuffer is a bytes.Buffer safe for the concurrent writes of a stage's
// workers and the prober.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// count returns how many events equal want.
func count(events []string, want string) int {
	var n int
	for _, e := range events {
		if e == want {
			n++
		}
	}
	return n
}

// countPrefix returns how many events start with prefix.
func countPrefix(events []string, prefix string) int {
	var n int
	for _, e := range events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// noneWithPrefix fails the test for every event that starts with one of
// prefixes.
func noneWithPrefix(t *testing.T, events []string, prefixes ...string) {
	t.Helper()
	for _, e := range events {
		for _, p := range prefixes {
			if strings.HasPrefix(e, p) {
				t.Errorf("unexpected call %s in %v", e, events)
			}
		}
	}
}

// index returns the position of the first event equal to want, failing the
// test when there is none.
func index(t *testing.T, events []string, want string) int {
	t.Helper()
	i := slices.Index(events, want)
	if i < 0 {
		t.Fatalf("event %q not found in %v", want, events)
	}
	return i
}

// mustFailIn checks that err names stage n and wraps target.
func mustFailIn(t *testing.T, err error, n int, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("Apply() = %v, want an error wrapping %v", err, target)
	}
	if want := fmt.Sprintf("stage %d ", n); !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Apply() = %q, want it to start with %q", err, want)
	}
}

// mismatch returns an error wrapping c5c3.ErrMismatch.
func mismatch(what string) error {
	return fmt.Errorf("%w: %s", c5c3.ErrMismatch, what)
}

// TestProberIntervalDefault pins the interval boundary 7 gives the prober in
// production. Every test that shortens it restores it through
// setProberInterval.
func TestProberIntervalDefault(t *testing.T) {
	if proberInterval != 5*time.Second {
		t.Errorf("proberInterval = %s, want 5s", proberInterval)
	}
}

func TestApplyRunsTheSevenStagesInOrder(t *testing.T) {
	setProberInterval(t, time.Hour)
	f := newFake()
	result, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	if got, want := f.snapshot(), slices.Concat(fullRun...); !slices.Equal(got, want) {
		t.Errorf("calls =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Equal(result.Created, allFive) {
		t.Errorf("Created = %+v, want %+v", result.Created, allFive)
	}
}

// TestApplyCreatesInParallel confirms stage 1 creates the user and the
// project, and stage 3 the role assignment and the catalog entry, at the same
// time: each first order's wait for readiness only returns once the second
// order has been created.
func TestApplyCreatesInParallel(t *testing.T) {
	setProberInterval(t, time.Hour)
	f := newFake()
	created := func(logical string) func(context.Context) error {
		return func(ctx context.Context) error {
			return f.waitFor(ctx, func(e []string) bool { return slices.Contains(e, "create:"+logical) })
		}
	}
	f.on("waitready:user", created("project"))
	f.on("waitready:role", created("catalog"))

	result, err := Apply(context.Background(), f, testPlan(), 2, testOpTimeout)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	if !slices.Equal(result.Created, allFive) {
		t.Errorf("Created = %+v, want %+v", result.Created, allFive)
	}

	// Each stage's calls form one block of the run, in any order within it.
	e := f.snapshot()
	start := 0
	for n, stage := range fullRun {
		end := start + len(stage)
		if end > len(e) {
			t.Fatalf("stage %d: the run has only %d calls: %v", n+1, len(e), e)
		}
		got, want := slices.Sorted(slices.Values(e[start:end])), slices.Sorted(slices.Values(stage))
		if !slices.Equal(got, want) {
			t.Errorf("stage %d calls = %v, want %v", n+1, got, want)
		}
		start = end
	}
	if start != len(e) {
		t.Errorf("the run made %d calls beyond the seven stages: %v", len(e)-start, e[start:])
	}
}

// TestApplyNoLoginDuringPasswordRotation confirms no login with the user's
// credential is sent between the raise of spec.passwordGeneration and the
// order delivering the new password, while the wait for it takes a while and
// the prober would have fired many times.
func TestApplyNoLoginDuringPasswordRotation(t *testing.T) {
	setProberInterval(t, time.Millisecond)
	f := newFake()
	f.on("waitrotated:user", func(ctx context.Context) error {
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		f.mark("returned:waitrotated:user")
		return nil
	})
	if _, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}

	e := f.snapshot()
	raised, returned := index(t, e, "raise:user"), index(t, e, "returned:waitrotated:user")
	if raised >= returned {
		t.Fatalf("the raise at %d does not precede the wait's return at %d: %v", raised, returned, e)
	}
	for _, ev := range e[raised:returned] {
		for _, login := range []string{"auth:user-", "verifyrole:user-", "verifycatalog:user-", "reject:user-"} {
			if strings.HasPrefix(ev, login) {
				t.Errorf("login %s with the user's credential during the password rotation: %v", ev, e[raised:returned+1])
			}
		}
	}
}

// TestApplyPasswordBeforeCredentialRotation confirms the raise of
// spec.credentialGeneration waits for stage 6 to finish, including the
// rejection of the old password.
func TestApplyPasswordBeforeCredentialRotation(t *testing.T) {
	setProberInterval(t, time.Hour)
	f := newFake()
	f.on("reject:user-0r", func(ctx context.Context) error {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		f.mark("returned:reject:user-0r")
		return nil
	})
	if _, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}

	e := f.snapshot()
	raised := index(t, e, "raise:appcred")
	for _, ev := range append(slices.Clone(fullRun[5]), "returned:reject:user-0r") {
		if i := index(t, e, ev); i > raised {
			t.Errorf("stage 6 call %s at %d comes after the credential's raise at %d: %v", ev, i, raised, e)
		}
	}
}

// TestApplyProbesDuringCredentialRotation confirms the prober reads the
// delivered Secret and logs in with it, scoped to the project, while the
// credential rotates, and stops when the stage ends.
func TestApplyProbesDuringCredentialRotation(t *testing.T) {
	setProberInterval(t, 2*time.Millisecond)
	const login = "auth:appcred-0r:dizzy-project"
	f := newFake()
	f.on("waitrotated:appcred", func(ctx context.Context) error {
		before := count(f.snapshot(), login)
		if err := f.waitFor(ctx, func(e []string) bool { return count(e, login) >= before+2 }); err != nil {
			return err
		}
		f.mark("returned:waitrotated:appcred")
		return nil
	})
	result, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	if !slices.Equal(result.Created, allFive) {
		t.Errorf("Created = %+v, want %+v", result.Created, allFive)
	}

	// While the stage waits for the rotation, only the prober calls: each probe
	// reads the Secret and logs in with it. The last probe may not have logged
	// in yet when the wait returned.
	e := f.snapshot()
	window := e[index(t, e, "waitrotated:appcred:1:appcred-0r")+1 : index(t, e, "returned:waitrotated:appcred")]
	if len(window) < 4 {
		t.Errorf("the prober made %d calls during the rotation, want at least two probes: %v", len(window), window)
	}
	for i, ev := range window {
		want := "read:appcred"
		if i%2 == 1 {
			want = login
		}
		if ev != want {
			t.Errorf("call %d during the rotation = %s, want %s: %v", i, ev, want, window)
		}
	}

	// The prober has stopped: no call follows the end of Apply.
	time.Sleep(20 * proberInterval)
	if after := f.snapshot(); len(after) != len(e) {
		t.Errorf("calls after Apply returned: %v", after[len(e):])
	}
}

// slowProbe makes the prober's logins with the rotated credential take 50 ms
// from the rotation's last call on, holds that call until one such login is in
// flight, and then answers the call with last. A slow login marks
// started:probe when it begins and canceled:probe when its context ends first;
// otherwise it marks finished:probe and answers with probeErr. The hold waits
// for the login's own started:probe, since the prober can enter the hook at
// any moment after its install. The tests assume the stage reaches its stop
// within those 50 ms.
func slowProbe(f *fakeOrders, probeErr, last error) {
	f.on("reject:appcred-0r", func(ctx context.Context) error {
		f.on("auth:appcred-1", func(ctx context.Context) error {
			f.mark("started:probe")
			select {
			case <-ctx.Done():
				f.mark("canceled:probe")
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
				f.mark("finished:probe")
				return probeErr
			}
		})
		if err := f.waitFor(ctx, func(e []string) bool { return slices.Contains(e, "started:probe") }); err != nil {
			return err
		}
		return last
	})
}

// TestApplyLetsInFlightProbeFinish confirms stage 7 lets a probe that is still
// logging in when the rotation's last call returns run to its end: nothing
// cancels the login, and Apply returns after it.
func TestApplyLetsInFlightProbeFinish(t *testing.T) {
	setProberInterval(t, time.Millisecond)
	f := newFake()
	slowProbe(f, nil, nil)

	if _, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	e := f.snapshot()
	noneWithPrefix(t, e, "canceled:probe")
	if n := count(e, "finished:probe"); n != 1 {
		t.Errorf("%d probes finished, want 1: %v", n, e)
	}
	if got := e[len(e)-1]; got != "finished:probe" {
		t.Errorf("last event before Apply returned = %s, want finished:probe: %v", got, e)
	}
}

// TestApplyInFlightProbeFailureFailsStage confirms a probe that fails for a
// reason of its own after the rotation's checks passed fails stage 7 with that
// reason.
func TestApplyInFlightProbeFailureFailsStage(t *testing.T) {
	t.Run("the login is rejected", func(t *testing.T) {
		setProberInterval(t, time.Millisecond)
		f := newFake()
		slowProbe(f, mismatch("keystone rejects the credential of a ready order"), nil)
		_, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
		mustFailIn(t, err, 7, c5c3.ErrMismatch)
		noneWithPrefix(t, f.snapshot(), "canceled:probe")
	})

	t.Run("the Secret read outlasts opTimeout", func(t *testing.T) {
		setProberInterval(t, time.Millisecond)
		f := newFake()
		f.on("reject:appcred-0r", func(ctx context.Context) error {
			// From here on, the prober's Secret read never answers.
			f.on("read:appcred", func(ctx context.Context) error {
				f.mark("started:probe")
				<-ctx.Done()
				return ctx.Err()
			})
			return f.waitFor(ctx, func(e []string) bool { return slices.Contains(e, "started:probe") })
		})
		const opTimeout = 100 * time.Millisecond
		start := time.Now()
		_, err := Apply(context.Background(), f, testPlan(), 1, opTimeout)
		mustFailIn(t, err, 7, context.DeadlineExceeded)
		if errors.Is(err, context.Canceled) {
			t.Errorf("Apply() = %v, want the read's deadline rather than a cancellation", err)
		}
		// The deadline is the probe's, not the rotation's own wait running out.
		if msg := err.Error(); !strings.Contains(msg, "probing the credential of order") || strings.Contains(msg, "the prober failed as well") {
			t.Errorf("Apply() = %q, want the prober's error alone", msg)
		}
		if took := time.Since(start); took >= testReadyTimeout/2 {
			t.Errorf("Apply took %s, want the read bounded by opTimeout %s", took, opTimeout)
		}
	})
}

// TestApplyRotationFailureLetsProbeFinish confirms stage 7 lets a probe in
// flight run to its end when the rotation's last check fails, and fails with
// the rotation's error, naming the probe's failure beside it.
func TestApplyRotationFailureLetsProbeFinish(t *testing.T) {
	accepted := fmt.Errorf("%w: keystone issued a token for the deleted credential", c5c3.ErrAccepted)

	t.Run("the probe passes", func(t *testing.T) {
		setProberInterval(t, time.Millisecond)
		f := newFake()
		slowProbe(f, nil, accepted)
		_, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
		mustFailIn(t, err, 7, c5c3.ErrAccepted)
		e := f.snapshot()
		noneWithPrefix(t, e, "canceled:probe")
		if got := e[len(e)-1]; got != "finished:probe" {
			t.Errorf("last event before Apply returned = %s, want finished:probe: %v", got, e)
		}
		if strings.Contains(err.Error(), "the prober failed as well") {
			t.Errorf("Apply() = %q, want no prober failure named", err)
		}
	})

	t.Run("the probe fails too", func(t *testing.T) {
		setProberInterval(t, time.Millisecond)
		f := newFake()
		slowProbe(f, mismatch("keystone rejects the credential of a ready order"), accepted)
		_, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
		mustFailIn(t, err, 7, c5c3.ErrAccepted)
		if errors.Is(err, c5c3.ErrMismatch) {
			t.Errorf("Apply() = %v, want only the rotation's error wrapped", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, "the prober failed as well") {
			t.Errorf("Apply() = %q, want it to name the prober's failure", msg)
		}
		for _, secret := range []string{userPasswordPrefix, appcredSecretPrefix} {
			if strings.Contains(msg, secret) {
				t.Errorf("Apply() = %q carries a secret with prefix %s", msg, secret)
			}
		}
		noneWithPrefix(t, f.snapshot(), "canceled:probe")
	})
}

func TestApplyProberMismatchFailsStage(t *testing.T) {
	setProberInterval(t, 5*time.Millisecond)
	f := newFake()
	// Once the rotation has begun, Keystone rejects the delivered credential
	// while its order is ready, which the client classifies as a mismatch.
	f.on("raise:appcred", func(context.Context) error {
		f.failWith("auth:appcred-0r", mismatch("keystone rejects the credential of a ready order"))
		return nil
	})
	f.on("waitrotated:appcred", func(ctx context.Context) error {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.Canceled) {
			f.mark("canceled:waitrotated:appcred")
		}
		return ctx.Err()
	})

	start := time.Now()
	result, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
	took := time.Since(start)
	mustFailIn(t, err, 7, c5c3.ErrMismatch)
	if errors.Is(err, context.Canceled) {
		t.Errorf("Apply() = %v, want the prober's mismatch rather than the cancellation it caused", err)
	}
	if !slices.Equal(result.Created, allFive) {
		t.Errorf("Created = %+v, want %+v", result.Created, allFive)
	}
	// The prober's error ended the stage at once: it cancelled the wait for
	// the rotation, which did not run into the ready timeout.
	if took >= testReadyTimeout/2 {
		t.Errorf("Apply took %s, want the prober's error to end the stage well before the ready timeout %s", took, testReadyTimeout)
	}
	e := f.snapshot()
	index(t, e, "canceled:waitrotated:appcred")
	noneWithPrefix(t, e, "status:appcred", "waitprevgone:", "reject:appcred-")

	// The prober has stopped: no call follows the end of Apply.
	time.Sleep(20 * proberInterval)
	if after := f.snapshot(); len(after) != len(e) {
		t.Errorf("calls after Apply returned: %v", after[len(e):])
	}
}

func TestApplySkipsGraceCheckWhenOver(t *testing.T) {
	const skipped = "the grace period of the superseded application credential was already over"
	tests := []struct {
		name         string
		graceLeft    time.Duration
		previousGone bool
		previousID   string
		wantLogin    bool
	}{
		{name: "deletion 2s ahead", graceLeft: 2 * time.Second},
		{name: "deletion 4s ahead", graceLeft: 4 * time.Second},
		{name: "deletion 6s ahead", graceLeft: 6 * time.Second, wantLogin: true},
		{name: "superseded credential already deleted", graceLeft: time.Hour, previousGone: true},
		{name: "another credential superseded", graceLeft: time.Hour, previousID: "appcred-id-other"},
		{name: "deletion far ahead", graceLeft: time.Hour, wantLogin: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setProberInterval(t, time.Hour)
			logs := captureLogs(t)
			f := newFake()
			f.graceLeft, f.previousGone, f.previousID = tt.graceLeft, tt.previousGone, tt.previousID

			result, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
			if err != nil {
				t.Fatalf("Apply() = %v, want nil", err)
			}
			if !slices.Equal(result.Created, allFive) {
				t.Errorf("Created = %+v, want %+v", result.Created, allFive)
			}

			e := f.snapshot()
			want := []string{"waitprevgone:appcred", "reject:appcred-0r"}
			if tt.wantLogin {
				want = append([]string{"auth:appcred-0r:dizzy-project"}, want...)
			}
			if got := e[index(t, e, "status:appcred")+1:]; !slices.Equal(got, want) {
				t.Errorf("calls after the status read = %v, want %v", got, want)
			}
			if logged := strings.Contains(logs.String(), skipped); logged == tt.wantLogin {
				t.Errorf("log carries %q: %t, want %t:\n%s", skipped, logged, !tt.wantLogin, logs)
			}
			if want := `"previousCredentialID":"` + tt.previousID + `"`; tt.previousID != "" && !strings.Contains(logs.String(), want) {
				t.Errorf("log lacks %s:\n%s", want, logs)
			}
		})
	}
}

// TestApplyFailedWaitKeepsCreated confirms an order whose wait for readiness
// fails stays in Result.Created, after the orders created before it, so the
// cleanup of a run that failed in stage 1 or stage 4 still finds it.
func TestApplyFailedWaitKeepsCreated(t *testing.T) {
	tests := []struct {
		key   string
		stage int
		want  []resource.Resource
		later []string
	}{
		{"waitready:project", 1, orders("user", "project"), []string{"read:", "auth:", "create:refused-"}},
		{"waitready:appcred", 4, allFive, []string{"read:appcred", "auth:appcred-", "editsecret:"}},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			f := newFake()
			f.failWith(tt.key, context.DeadlineExceeded)
			result, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
			mustFailIn(t, err, tt.stage, context.DeadlineExceeded)
			if !slices.Equal(result.Created, tt.want) {
				t.Errorf("Created = %+v, want %+v", result.Created, tt.want)
			}
			noneWithPrefix(t, f.snapshot(), tt.later...)
		})
	}
}

func TestApplySecretRepairOff(t *testing.T) {
	setProberInterval(t, time.Hour)
	f := newFake()
	p := testPlan()
	p.SecretRepair = false
	result, err := Apply(context.Background(), f, p, 1, testOpTimeout)
	if err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	if !slices.Equal(result.Created, allFive) {
		t.Errorf("Created = %+v, want %+v", result.Created, allFive)
	}
	e := f.snapshot()
	noneWithPrefix(t, e, "editsecret:", "deletesecret:", "waitrestored:")
	// Stage 6 starts from the credential stage 1 read.
	stage4End := index(t, e, "auth:appcred-0:dizzy-project")
	if got, want := e[stage4End+1:stage4End+5], []string{"raise:user", "waitrotated:user:1:user-0", "auth:user-1:", "reject:user-0"}; !slices.Equal(got, want) {
		t.Errorf("calls after stage 4 = %v, want %v", got, want)
	}
}

func TestApplySecretRepairForbidden(t *testing.T) {
	setProberInterval(t, time.Hour)
	f := newFake()
	secret := testNamespace + "/dizzyorder-run0-user-credentials"
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "dizzyorder-run0-user-credentials", errors.New("cannot patch"))
	f.failWith("editsecret:user", fmt.Errorf("%w: editing key password of secret %s needs patch on secrets; "+
		"set checks.secret_repair: false for a deployment that grants only get on Secrets: %w", c5c3.ErrForbidden, secret, denied))

	result, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
	mustFailIn(t, err, 5, c5c3.ErrForbidden)
	if !apierrors.IsForbidden(err) {
		t.Errorf("Apply() = %v, want it to wrap the 403", err)
	}
	if !strings.Contains(err.Error(), "checks.secret_repair") {
		t.Errorf("Apply() = %q, want it to name checks.secret_repair", err)
	}
	if !slices.Equal(result.Created, allFive) {
		t.Errorf("Created = %+v, want %+v", result.Created, allFive)
	}
	e := f.snapshot()
	if n := count(e, "editsecret:user"); n != 1 {
		t.Errorf("the forbidden edit was sent %d times, want 1", n)
	}
	noneWithPrefix(t, e, "waitrestored:", "deletesecret:", "raise:")
}

func TestApplyRefusalBecomesReady(t *testing.T) {
	f := newFake()
	f.failWith("waitreason:refused-role", mismatch("order turned ready instead of reporting RoleNotAllowed"))

	result, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
	mustFailIn(t, err, 2, c5c3.ErrMismatch)
	if want := orders("user", "project", "refused-appcred", "refused-role"); !slices.Equal(result.Created, want) {
		t.Errorf("Created = %+v, want %+v", result.Created, want)
	}
	e := f.snapshot()
	if n := count(e, "waitreason:refused-role:RoleNotAllowed"); n != 1 {
		t.Errorf("the refusal was waited on %d times, want 1", n)
	}
	noneWithPrefix(t, e, "create:role", "create:appcred", "delete:", "waitgone:")
}

// TestApplyRefusalKeptUntilGone confirms a refusal leaves Result.Created only
// once its wait for the order to be gone succeeds.
func TestApplyRefusalKeptUntilGone(t *testing.T) {
	f := newFake()
	f.failWith("waitgone:refused-role", context.DeadlineExceeded)

	result, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
	mustFailIn(t, err, 2, context.DeadlineExceeded)
	if want := orders("user", "project", "refused-role"); !slices.Equal(result.Created, want) {
		t.Errorf("Created = %+v, want %+v", result.Created, want)
	}
	noneWithPrefix(t, f.snapshot(), "create:role", "create:catalog")
}

func TestApplyPartialFailureReturnsCreated(t *testing.T) {
	f := newFake()
	exists := apierrors.NewAlreadyExists(schema.GroupResource{Group: "c5c3.io", Resource: "keystonecatalogentries"}, "dizzyorder-run0-catalog")
	f.failWith("create:catalog", exists)

	result, err := Apply(context.Background(), f, testPlan(), 2, testOpTimeout)
	if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("Apply() = %v, want the create's 409", err)
	}
	if !strings.HasPrefix(err.Error(), "stage 3 ") {
		t.Errorf("Apply() = %q, want it to name stage 3", err)
	}
	e := f.snapshot()
	want := orders("user", "project")
	f.mu.Lock()
	if f.made["role"] {
		want = orders("user", "project", "role")
	}
	f.mu.Unlock()
	if !slices.Equal(result.Created, want) {
		t.Errorf("Created = %+v, want %+v", result.Created, want)
	}
	if n := count(e, "create:catalog"); n != 1 {
		t.Errorf("the failed create was sent %d times, want 1", n)
	}
	noneWithPrefix(t, e, "status:", "verifyrole:", "verifycatalog:", "create:appcred")
}

func TestApplyCancelled(t *testing.T) {
	t.Run("before the first stage", func(t *testing.T) {
		f := newFake()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := Apply(ctx, f, testPlan(), 2, testOpTimeout)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Apply() = %v, want context.Canceled", err)
		}
		if len(result.Created) != 0 {
			t.Errorf("Created = %+v, want none", result.Created)
		}
		if e := f.snapshot(); len(e) != 0 {
			t.Errorf("a cancelled apply made calls: %v", e)
		}
	})

	t.Run("in stage 3", func(t *testing.T) {
		f := newFake()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.on("waitready:role", func(waitCtx context.Context) error {
			cancel()
			<-waitCtx.Done()
			return waitCtx.Err()
		})
		result, err := Apply(ctx, f, testPlan(), 1, testOpTimeout)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Apply() = %v, want context.Canceled", err)
		}
		if want := orders("user", "project", "role"); !slices.Equal(result.Created, want) {
			t.Errorf("Created = %+v, want %+v", result.Created, want)
		}
		noneWithPrefix(t, f.snapshot(), "create:catalog", "status:")
	})

	t.Run("a call answers with another error after the interrupt", func(t *testing.T) {
		f := newFake()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.on("verifyrole:user-0", func(context.Context) error {
			cancel()
			return mismatch("the token scoped to dizzy-project lacks role member")
		})
		result, err := Apply(ctx, f, testPlan(), 1, testOpTimeout)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Apply() = %v, want context.Canceled", err)
		}
		if want := orders("user", "project", "role", "catalog"); !slices.Equal(result.Created, want) {
			t.Errorf("Created = %+v, want %+v", result.Created, want)
		}
	})

	t.Run("in stage 7 while the prober runs", func(t *testing.T) {
		setProberInterval(t, time.Millisecond)
		f := newFake()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.on("waitrotated:appcred", func(waitCtx context.Context) error {
			// Let the prober log in once, then interrupt the run.
			if err := f.waitFor(waitCtx, func(e []string) bool { return count(e, "auth:appcred-0r:dizzy-project") >= 2 }); err != nil {
				return err
			}
			cancel()
			<-waitCtx.Done()
			return waitCtx.Err()
		})
		result, err := Apply(ctx, f, testPlan(), 1, testOpTimeout)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Apply() = %v, want context.Canceled", err)
		}
		if !slices.Equal(result.Created, allFive) {
			t.Errorf("Created = %+v, want %+v", result.Created, allFive)
		}
		noneWithPrefix(t, f.snapshot(), "status:appcred", "waitprevgone:", "reject:appcred-")
	})

	t.Run("in stage 7 while the last probe runs", func(t *testing.T) {
		setProberInterval(t, time.Millisecond)
		f := newFake()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.on("reject:appcred-0r", func(rejectCtx context.Context) error {
			// The rotation's checks pass; the interrupt then cuts the probe
			// still logging in.
			f.on("auth:appcred-1", func(authCtx context.Context) error {
				f.mark("started:probe")
				<-authCtx.Done()
				f.mark("canceled:probe")
				return authCtx.Err()
			})
			if err := f.waitFor(rejectCtx, func(e []string) bool { return slices.Contains(e, "started:probe") }); err != nil {
				return err
			}
			cancel()
			return nil
		})
		result, err := Apply(ctx, f, testPlan(), 1, testOpTimeout)
		mustFailIn(t, err, 7, context.Canceled)
		if !slices.Equal(result.Created, allFive) {
			t.Errorf("Created = %+v, want %+v", result.Created, allFive)
		}
		index(t, f.snapshot(), "canceled:probe")
	})
}

func TestApplyRoleAndCatalogMismatch(t *testing.T) {
	t.Run("the scoped token lacks the role", func(t *testing.T) {
		f := newFake()
		f.failWith("verifyrole:user-0", mismatch("the token scoped to dizzy-project lacks role member"))
		_, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
		mustFailIn(t, err, 3, c5c3.ErrMismatch)
		e := f.snapshot()
		if n := countPrefix(e, "verifyrole:"); n != 1 {
			t.Errorf("the role check was sent %d times, want 1", n)
		}
		noneWithPrefix(t, e, "verifycatalog:", "create:appcred")
	})

	t.Run("the catalog lacks the entry until opTimeout", func(t *testing.T) {
		f := newFake()
		f.on("verifycatalog:user-0", func(ctx context.Context) error {
			<-ctx.Done()
			return fmt.Errorf("%w: the catalog lists no dizzy-probe service: %w", c5c3.ErrMismatch, ctx.Err())
		})
		const opTimeout = 100 * time.Millisecond
		start := time.Now()
		_, err := Apply(context.Background(), f, testPlan(), 1, opTimeout)
		mustFailIn(t, err, 3, c5c3.ErrMismatch)
		if took := time.Since(start); took >= testReadyTimeout/2 {
			t.Errorf("Apply took %s, want the catalog check bounded by opTimeout %s", took, opTimeout)
		}
		e := f.snapshot()
		if n := countPrefix(e, "verifycatalog:"); n != 1 {
			t.Errorf("the catalog check was sent %d times, want 1", n)
		}
		noneWithPrefix(t, e, "create:appcred")
	})
}

func TestApplyPasswordRotationFailures(t *testing.T) {
	t.Run("password unchanged", func(t *testing.T) {
		f := newFake()
		f.failWith("waitrotated:user", mismatch("the secret still carries the password of before generation 1"))
		_, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
		mustFailIn(t, err, 6, c5c3.ErrMismatch)
		noneWithPrefix(t, f.snapshot(), "auth:user-1", "reject:", "raise:appcred")
	})

	t.Run("old password accepted", func(t *testing.T) {
		f := newFake()
		f.failWith("reject:user-0r", fmt.Errorf("%w: keystone issued a token for the old password", c5c3.ErrAccepted))
		_, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
		mustFailIn(t, err, 6, c5c3.ErrAccepted)
		e := f.snapshot()
		if n := count(e, "reject:user-0r"); n != 1 {
			t.Errorf("the old password was sent %d times, want 1", n)
		}
		noneWithPrefix(t, e, "raise:appcred")
	})
}

func TestApplySupersededCredentialAccepted(t *testing.T) {
	setProberInterval(t, time.Hour)
	f := newFake()
	f.failWith("reject:appcred-0r", fmt.Errorf("%w: keystone issued a token for the deleted credential", c5c3.ErrAccepted))
	result, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
	mustFailIn(t, err, 7, c5c3.ErrAccepted)
	if n := count(f.snapshot(), "reject:appcred-0r"); n != 1 {
		t.Errorf("the superseded credential was sent %d times, want 1", n)
	}
	if !slices.Equal(result.Created, allFive) {
		t.Errorf("Created = %+v, want %+v", result.Created, allFive)
	}
}

// TestApplyRetriesTransient confirms each single request that runs through
// WithRetry is sent again after a 503 from Kubernetes or Keystone.
func TestApplyRetriesTransient(t *testing.T) {
	kube503 := apierrors.NewServiceUnavailable("the API server is restarting")
	keystone503 := gophercloud.ErrUnexpectedResponseCode{Actual: 503, Body: []byte("Service Unavailable")}
	tests := []struct {
		key string
		err error
	}{
		{"create:user", kube503},
		{"delete:refused-role", kube503},
		{"status:project", kube503},
		{"raise:user", kube503},
		{"read:user", kube503},
		{"editsecret:user", kube503},
		{"deletesecret:appcred", kube503},
		{"auth:user-0", keystone503},
		{"verifyrole:user-0", keystone503},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()
			f := newFake()
			f.failWith(tt.key, tt.err)
			result, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
			if err != nil {
				t.Fatalf("Apply() = %v, want nil after the retry", err)
			}
			if !slices.Equal(result.Created, allFive) {
				t.Errorf("Created = %+v, want %+v", result.Created, allFive)
			}
			// The first call under the key answered 503 and the next succeeded.
			e := f.snapshot()
			first := slices.IndexFunc(e, func(ev string) bool { return ev == tt.key || strings.HasPrefix(ev, tt.key+":") })
			if first < 0 || first+1 >= len(e) || e[first+1] != e[first] {
				t.Errorf("the call after the 503 to %s is not the same call: %v", tt.key, e)
			}
		})
	}
}

// TestApplyDoesNotRetry confirms a sentinel is never sent again, and that
// VerifyCatalog and Reject are sent once even on a transient error.
func TestApplyDoesNotRetry(t *testing.T) {
	keystone503 := gophercloud.ErrUnexpectedResponseCode{Actual: 503, Body: []byte("Service Unavailable")}
	is503 := func(err error) bool { return gophercloud.ResponseCodeIs(err, 503) }
	isSentinel := func(target error) func(error) bool {
		return func(err error) bool { return errors.Is(err, target) }
	}
	tests := []struct {
		key   string
		event string
		stage int
		err   error
		match func(error) bool
	}{
		{"read:user", "read:user", 1, mismatch("the secret is missing"), isSentinel(c5c3.ErrMismatch)},
		{"auth:user-0", "auth:user-0:", 1, mismatch("keystone rejects the password of a ready order"), isSentinel(c5c3.ErrMismatch)},
		{"status:catalog", "status:catalog", 3, fmt.Errorf("%w: wrapped %w", c5c3.ErrMismatch, apierrors.NewServiceUnavailable("x")), isSentinel(c5c3.ErrMismatch)},
		{"verifycatalog:user-0", "verifycatalog:user-0:dizzy-project:dizzy-domain:dizzy-probe:dizzy-probe-service:https://dizzy-probe.invalid/v1", 3, keystone503, is503},
		{"reject:user-0r", "reject:user-0r", 6, keystone503, is503},
		{"reject:appcred-0r", "reject:appcred-0r", 7, keystone503, is503},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			setProberInterval(t, time.Hour)
			f := newFake()
			f.failWith(tt.key, tt.err)
			_, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout)
			if err == nil {
				t.Fatal("Apply() = nil, want the error")
			}
			if want := fmt.Sprintf("stage %d ", tt.stage); !strings.HasPrefix(err.Error(), want) {
				t.Errorf("Apply() = %q, want it to start with %q", err, want)
			}
			if !tt.match(err) {
				t.Errorf("Apply() = %v, want it to wrap %v", err, tt.err)
			}
			if n := count(f.snapshot(), tt.event); n != 1 {
				t.Errorf("%s was sent %d times, want 1", tt.event, n)
			}
		})
	}
}

// TestApplyBoundsEachCall confirms each call runs under the bound boundary 7
// names for it: a single request under opTimeout, a wait for an order to be
// gone under the gone timeout, the wait for the superseded credential under
// the grace period plus the gone timeout, and every other wait under the ready
// timeout.
func TestApplyBoundsEachCall(t *testing.T) {
	setProberInterval(t, time.Hour)
	f := newFake()
	if _, err := Apply(context.Background(), f, testPlan(), 1, testOpTimeout); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}

	bounds := map[string]time.Duration{
		"create":        testOpTimeout,
		"delete":        testOpTimeout,
		"status":        testOpTimeout,
		"raise":         testOpTimeout,
		"read":          testOpTimeout,
		"editsecret":    testOpTimeout,
		"deletesecret":  testOpTimeout,
		"auth":          testOpTimeout,
		"verifyrole":    testOpTimeout,
		"verifycatalog": testOpTimeout,
		"reject":        testOpTimeout,
		"waitready":     testReadyTimeout,
		"waitreason":    testReadyTimeout,
		"waitrotated":   testReadyTimeout,
		"waitrestored":  testReadyTimeout,
		"waitgone":      testGoneTimeout,
		"waitprevgone":  testGracePeriod + testGoneTimeout,
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	for key, left := range f.deadlines {
		verb, _, _ := strings.Cut(key, ":")
		want, ok := bounds[verb]
		if !ok {
			t.Errorf("call %s has no expected bound", key)
			continue
		}
		seen[verb] = true
		if left > want || left < want-500*time.Millisecond {
			t.Errorf("%s ran with %s left on its context, want just under %s", key, left, want)
		}
	}
	for verb := range bounds {
		if !seen[verb] {
			t.Errorf("no %s call was made", verb)
		}
	}
}

// TestApplyKeepsSecretsOutOfLogsAndErrors runs a successful apply that logs
// the skipped grace check and three failing ones, and greps every log line and
// error text for the passwords and the credential secrets the fake delivers.
func TestApplyKeepsSecretsOutOfLogsAndErrors(t *testing.T) {
	setProberInterval(t, 2*time.Millisecond)
	logs := captureLogs(t)
	var texts []string

	f := newFake()
	f.graceLeft = 2 * time.Second
	if _, err := Apply(context.Background(), f, testPlan(), 2, testOpTimeout); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}

	for _, fail := range []struct {
		key string
		err error
	}{
		{"reject:user-0r", fmt.Errorf("%w: keystone issued a token", c5c3.ErrAccepted)},
		{"reject:appcred-0r", fmt.Errorf("%w: keystone issued a token", c5c3.ErrAccepted)},
		{"auth:appcred-0r", mismatch("keystone rejects the credential")},
	} {
		f := newFake()
		f.failWith(fail.key, fail.err)
		if _, err := Apply(context.Background(), f, testPlan(), 2, testOpTimeout); err == nil {
			t.Errorf("Apply() with %s failing = nil, want an error", fail.key)
		} else {
			texts = append(texts, err.Error())
		}
	}

	out := logs.String() + strings.Join(texts, "\n")
	if !strings.Contains(out, "starting stage") {
		t.Fatalf("captured no log line:\n%s", out)
	}
	for _, secret := range []string{userPasswordPrefix, appcredSecretPrefix} {
		if strings.Contains(out, secret) {
			t.Errorf("a log line or error text carries a %s value:\n%s", secret, out)
		}
	}
}

func TestApplyRejectsMalformedPlan(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*plan.Plan)
		want   string
	}{
		{
			name:   "no catalog entry",
			mutate: func(p *plan.Plan) { p.Orders = slices.Delete(p.Orders, 3, 4) },
			want:   "plan holds 0 keystone_catalog_entry orders, want 1",
		},
		{
			name: "two users",
			mutate: func(p *plan.Plan) {
				p.Orders = append(p.Orders, plan.Order{Kind: plan.KindUser, Name: "user2"})
			},
			want: "plan holds 2 keystone_user orders, want 1",
		},
		{
			name:   "credential of another user",
			mutate: func(p *plan.Plan) { p.Orders[4].User = "someone" },
			want:   `order "appcred" references user "someone"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			p := testPlan()
			tt.mutate(p)
			result, err := Apply(context.Background(), f, p, 1, testOpTimeout)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Apply() = %v, want an error containing %q", err, tt.want)
			}
			if len(result.Created) != 0 {
				t.Errorf("Created = %+v, want none", result.Created)
			}
			if e := f.snapshot(); len(e) != 0 {
				t.Errorf("a malformed plan drove calls: %v", e)
			}
		})
	}
}

// TestWithRetry confirms WithRetry sends a call again after a Kubernetes or
// Keystone 503 and returns a sentinel after the first attempt.
func TestWithRetry(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		attempts int
	}{
		{"kubernetes 503", apierrors.NewServiceUnavailable("the API server is restarting"), 2},
		{"keystone 503", gophercloud.ErrUnexpectedResponseCode{Actual: 503}, 2},
		{"mismatch", mismatch("a mismatch"), 1},
		{"forbidden", fmt.Errorf("%w: %w", c5c3.ErrForbidden, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "s", errors.New("no"))), 1},
		{"refused", fmt.Errorf("%w: %w", c5c3.ErrRefused, apierrors.NewServiceUnavailable("x")), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var attempts int
			err := WithRetry(context.Background(), testOpTimeout, func(context.Context) error {
				attempts++
				if attempts == 1 {
					return tt.err
				}
				return nil
			})
			if attempts != tt.attempts {
				t.Errorf("attempts = %d, want %d", attempts, tt.attempts)
			}
			if wantErr := tt.attempts == 1; (err != nil) != wantErr {
				t.Errorf("WithRetry() = %v, want an error: %t", err, wantErr)
			}
		})
	}
}
