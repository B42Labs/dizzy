package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/openstack-tester/internal/keystone"
	keystoneplan "github.com/B42Labs/openstack-tester/internal/keystone/plan"
	"github.com/B42Labs/openstack-tester/internal/resource"
	"github.com/B42Labs/openstack-tester/internal/run"
)

// sampleKeystoneScenarioYAML is a small but complete Keystone scenario used by
// the keystone command tests.
const sampleKeystoneScenarioYAML = `
name: cli
seed: 5
resources:
  domains:  1
  roles:    2
  projects: 3
  users:    4
distribution:
  projects_per_domain:            { min: 1, max: 2 }
  assignments_per_user:           { min: 1, max: 2 }
  domain_scoped_assignment_ratio: 0.2
  users_issuing_tokens_ratio:     0.5
`

// recordingKeystoneCleaner is a fake keystoneexec.Cleaner that records what it
// deletes (and disables) and whether the contexts it is handed were cancelled.
// Its listings return nothing, so the only deletes are the recorded resources
// passed through the created list — letting a test assert reverse-order
// teardown and disable-before-delete.
type recordingKeystoneCleaner struct {
	recordWritten      *bool
	firstCallSawRecord bool
	calls              int
	sawCancelled       bool
	events             []string
	deleted            []resource.Resource
}

func (c *recordingKeystoneCleaner) observe(ctx context.Context) {
	c.calls++
	if c.calls == 1 && c.recordWritten != nil {
		c.firstCallSawRecord = *c.recordWritten
	}
	if ctx.Err() != nil {
		c.sawCancelled = true
	}
}

func (c *recordingKeystoneCleaner) ListProjectsByTag(ctx context.Context, _ string) ([]resource.Resource, error) {
	c.observe(ctx)
	return nil, nil
}
func (c *recordingKeystoneCleaner) ListUsersByPrefix(ctx context.Context, _ string) ([]resource.Resource, error) {
	c.observe(ctx)
	return nil, nil
}
func (c *recordingKeystoneCleaner) ListRolesByPrefix(ctx context.Context, _ string) ([]resource.Resource, error) {
	c.observe(ctx)
	return nil, nil
}
func (c *recordingKeystoneCleaner) ListDomainsByPrefix(ctx context.Context, _ string) ([]resource.Resource, error) {
	c.observe(ctx)
	return nil, nil
}
func (c *recordingKeystoneCleaner) ListAssignmentsForUser(ctx context.Context, _ string) ([]resource.Resource, error) {
	c.observe(ctx)
	return nil, nil
}
func (c *recordingKeystoneCleaner) DisableDomain(ctx context.Context, r resource.Resource) error {
	c.observe(ctx)
	c.events = append(c.events, "disable:"+r.ID)
	return nil
}
func (c *recordingKeystoneCleaner) Delete(ctx context.Context, r resource.Resource) error {
	c.observe(ctx)
	c.deleted = append(c.deleted, r)
	c.events = append(c.events, "del:"+string(r.Kind))
	return nil
}

// blockingKeystoneCleaner mimics a wedged Keystone: every operation blocks until
// its context is cancelled, so a per-op timeout is the only thing that unblocks
// it.
type blockingKeystoneCleaner struct{}

func (blockingKeystoneCleaner) ListProjectsByTag(ctx context.Context, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingKeystoneCleaner) ListUsersByPrefix(ctx context.Context, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingKeystoneCleaner) ListRolesByPrefix(ctx context.Context, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingKeystoneCleaner) ListDomainsByPrefix(ctx context.Context, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingKeystoneCleaner) ListAssignmentsForUser(ctx context.Context, _ string) ([]resource.Resource, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingKeystoneCleaner) DisableDomain(ctx context.Context, _ resource.Resource) error {
	<-ctx.Done()
	return ctx.Err()
}
func (blockingKeystoneCleaner) Delete(ctx context.Context, _ resource.Resource) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestKeystoneSubcommandsRegistered(t *testing.T) {
	root := newRootCmd()
	ks := findSubcommand(root, "keystone")
	if ks == nil {
		t.Fatal("keystone command not registered on root")
	}
	for _, name := range []string{"generate", "apply", "status", "report", "cleanup"} {
		t.Run(name, func(t *testing.T) {
			if findSubcommand(ks, name) == nil {
				t.Errorf("keystone subcommand %q not registered", name)
			}
		})
	}
}

func TestKeystoneStatusRequiresRunFlag(t *testing.T) {
	if _, err := execRoot(t, "keystone", "status"); err == nil {
		t.Fatal("status without --run: expected error, got nil")
	}
}

func TestKeystoneCleanupRequiresRunOrRunID(t *testing.T) {
	if _, err := execRoot(t, "keystone", "cleanup"); err == nil {
		t.Fatal("cleanup with neither --run nor --run-id: expected error, got nil")
	}
}

// TestKeystoneRejectsCinderRecord confirms the service guard stops a keystone
// command from operating on a cinder run record (whose resource kinds the
// keystone client cannot handle), and vice versa.
func TestKeystoneRejectsCinderRecord(t *testing.T) {
	dir := t.TempDir()
	if _, err := run.Write(dir, &run.Record{RunID: "cin00001", Service: "cinder"}); err != nil {
		t.Fatalf("seeding cinder record: %v", err)
	}
	if _, err := run.Write(dir, &run.Record{RunID: "key00001", Service: "keystone"}); err != nil {
		t.Fatalf("seeding keystone record: %v", err)
	}
	cinderRec := filepath.Join(dir, "run-cin00001.json")
	keystoneRec := filepath.Join(dir, "run-key00001.json")

	// keystone status/cleanup must reject a cinder record before touching a cloud.
	if _, err := execRoot(t, "keystone", "status", "--run", cinderRec); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("keystone status on a cinder record: err = %v, want a service mismatch error", err)
	}
	if _, err := execRoot(t, "keystone", "cleanup", "--run", cinderRec); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("keystone cleanup on a cinder record: err = %v, want a service mismatch error", err)
	}
	// cinder status must reject a keystone record symmetrically.
	if _, err := execRoot(t, "cinder", "status", "--run", keystoneRec); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("cinder status on a keystone record: err = %v, want a service mismatch error", err)
	}
}

func TestKeystoneGenerateStdout(t *testing.T) {
	path := writeScenario(t, sampleKeystoneScenarioYAML)

	out, err := execRoot(t, "keystone", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var p keystoneplan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if len(p.Users) != 4 {
		t.Errorf("users = %d, want 4", len(p.Users))
	}
	// A generated plan must never carry a password anywhere.
	if strings.Contains(out, "password") {
		t.Error("generated plan JSON contains a password field")
	}
}

func TestKeystoneGenerateSetOverride(t *testing.T) {
	path := writeScenario(t, sampleKeystoneScenarioYAML)
	out, err := execRoot(t, "keystone", "generate", "--scenario", path, "--set", "resources.users=2")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var p keystoneplan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not valid plan JSON: %v", err)
	}
	if len(p.Users) != 2 {
		t.Errorf("users = %d, want 2 after override", len(p.Users))
	}
}

func TestKeystoneGenerateSeedOverride(t *testing.T) {
	path := writeScenario(t, sampleKeystoneScenarioYAML)
	base, err := execRoot(t, "keystone", "generate", "--scenario", path)
	if err != nil {
		t.Fatalf("generate (file seed): %v", err)
	}
	overridden, err := execRoot(t, "keystone", "generate", "--scenario", path, "--seed", "999")
	if err != nil {
		t.Fatalf("generate (--seed): %v", err)
	}
	if base == overridden {
		t.Error("global --seed did not change the generated plan")
	}
}

func TestKeystoneGenerateErrors(t *testing.T) {
	valid := writeScenario(t, sampleKeystoneScenarioYAML)
	// A cinder key in a keystone scenario must fail strict parse.
	wrongSchema := writeScenario(t, "name: bad\nresources:\n  volumes: 1\n")

	tests := []struct {
		name string
		args []string
	}{
		{"missing scenario flag", []string{"keystone", "generate"}},
		{"nonexistent file", []string{"keystone", "generate", "--scenario", filepath.Join(t.TempDir(), "nope.yaml")}},
		{"wrong-schema scenario", []string{"keystone", "generate", "--scenario", wrongSchema}},
		{"unknown set key", []string{"keystone", "generate", "--scenario", valid, "--set", "resources.nope=1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := execRoot(t, tc.args...); err == nil {
				t.Errorf("expected error for args %v, got nil", tc.args)
			}
		})
	}
}

func TestKeystoneApplyDryRunSummaryNoAPICall(t *testing.T) {
	// Point cloud configuration at nothing: succeeding without a reachable cloud
	// proves the dry-run path makes zero auth/API calls.
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleKeystoneScenarioYAML)

	out, err := execRoot(t, "keystone", "apply", "--scenario", path, "--dry-run")
	if err != nil {
		t.Fatalf("apply --dry-run: %v", err)
	}
	for _, want := range []string{`scenario "cli"`, "domains:", "roles:", "projects:", "users:", "assignments:", "token issues:"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestKeystoneApplyWithoutDryRunRequiresCloud(t *testing.T) {
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")

	path := writeScenario(t, sampleKeystoneScenarioYAML)

	_, err := execRoot(t, "keystone", "apply", "--scenario", path)
	if err == nil {
		t.Fatal("apply without --dry-run: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "identity client") {
		t.Errorf("error %q does not mention identity client creation", err.Error())
	}
}

func TestKeystoneApplyPrivilegeFlagsParse(t *testing.T) {
	// --dry-run returns before any client or privilege check, so a successful
	// parse proves the privilege flags are registered without needing a cloud.
	path := writeScenario(t, sampleKeystoneScenarioYAML)
	if _, err := execRoot(t, "keystone", "apply", "--scenario", path, "--dry-run",
		"--privilege", "domain-manager", "--domain", "d", "--roles", "member,reader"); err != nil {
		t.Fatalf("apply --dry-run with privilege flags: %v", err)
	}
}

func TestResolveKeystonePrivilegeRejectsBadValue(t *testing.T) {
	p := &keystoneplan.Plan{Domains: []keystoneplan.Domain{{Name: "dom-0001"}}}
	_, _, err := resolveKeystonePrivilege(context.Background(), nil, keystonePrivilegeFlags{privilege: "root"}, p)
	if err == nil {
		t.Fatal("resolveKeystonePrivilege with --privilege root: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "privilege") {
		t.Errorf("error %q does not name the bad privilege value", err)
	}
}

func TestFinishAbortedKeystoneApplyTearsDownOnLiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a first signal cancelled the run

	recordWritten := false
	c := &recordingKeystoneCleaner{recordWritten: &recordWritten}
	// Reverse dependency order: assignment, user, project, role, domain.
	created := []resource.Resource{
		{Kind: keystone.KindDomain, ID: "d1", Name: "ostester-abcd1234-dom-0001"},
		{Kind: keystone.KindRole, ID: "r1", Name: "ostester-abcd1234-role-0001"},
		{Kind: keystone.KindProject, ID: "p1", Name: "ostester-abcd1234-proj-0001"},
		{Kind: keystone.KindUser, ID: "u1", Name: "ostester-abcd1234-user-0001"},
		{Kind: keystone.KindAssignment, ID: "u1:project:p1:r1"},
	}
	var out bytes.Buffer
	writeRecord := func() (string, error) {
		recordWritten = true
		return "run-abcd1234.json", nil
	}

	err := finishAbortedKeystoneApply(ctx, &out, c, "abcd1234", created, false, errors.New("apply boom"), time.Second, writeRecord)
	if err == nil {
		t.Fatal("finishAbortedKeystoneApply returned nil; an interrupted apply must exit non-zero")
	}
	if !strings.Contains(err.Error(), "abcd1234") || !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("error %q does not name the run id or signal interruption", err)
	}
	if c.sawCancelled {
		t.Error("teardown ran with a cancelled context; it must run on context.WithoutCancel")
	}
	if !c.firstCallSawRecord {
		t.Error("the run record was not written before teardown began")
	}
	// The deleted kinds must appear in reverse dependency order.
	wantOrder := []resource.Kind{keystone.KindAssignment, keystone.KindUser, keystone.KindProject, keystone.KindRole, keystone.KindDomain}
	if len(c.deleted) != len(wantOrder) {
		t.Fatalf("deleted %d resources, want %d: %+v", len(c.deleted), len(wantOrder), c.deleted)
	}
	for i, want := range wantOrder {
		if c.deleted[i].Kind != want {
			t.Errorf("deleted[%d] kind = %q, want %q (order %+v)", i, c.deleted[i].Kind, want, c.deleted)
		}
	}
	// The domain must be disabled before it is deleted.
	var disabledSeen, domainDeleteSeen bool
	for _, e := range c.events {
		if e == "disable:d1" {
			disabledSeen = true
		}
		if e == "del:"+string(keystone.KindDomain) {
			if !disabledSeen {
				t.Error("the domain was deleted before it was disabled")
			}
			domainDeleteSeen = true
		}
	}
	if !domainDeleteSeen {
		t.Error("the domain was never deleted")
	}
	if !strings.Contains(out.String(), "deleted 5 resource(s)") {
		t.Errorf("output %q missing the deletion count", out.String())
	}
}

func TestFinishAbortedKeystoneApplyBoundsWedgedTeardown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the signal context is already cancelled at teardown time

	writeRecord := func() (string, error) { return "run-wedged.json", nil }

	done := make(chan error, 1)
	go func() {
		done <- finishAbortedKeystoneApply(ctx, io.Discard, blockingKeystoneCleaner{}, "wedged", nil, false, errors.New("apply boom"), 10*time.Millisecond, writeRecord)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("finishAbortedKeystoneApply did not return; teardown was not bounded")
	}
}

func TestFinishAbortedKeystoneApplyKeepOnAbortLeavesResources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name       string
		recordPath string
		wantHint   string
	}{
		{"with record path", "run-keep.json", "keystone cleanup --run run-keep.json"},
		{"empty record path falls back to run-id", "", "keystone cleanup --run-id keep1234"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingKeystoneCleaner{}
			var out bytes.Buffer
			writeRecord := func() (string, error) { return tc.recordPath, nil }

			err := finishAbortedKeystoneApply(ctx, &out, c, "keep1234", nil, true, errors.New("apply boom"), time.Second, writeRecord)
			if err == nil || !strings.Contains(err.Error(), "keep1234") {
				t.Errorf("error = %v, want a non-nil error naming the run id", err)
			}
			if c.calls != 0 {
				t.Errorf("cleaner was called %d times; --keep-on-abort must not tear anything down", c.calls)
			}
			if !strings.Contains(out.String(), tc.wantHint) {
				t.Errorf("output %q missing hint %q", out.String(), tc.wantHint)
			}
		})
	}
}
