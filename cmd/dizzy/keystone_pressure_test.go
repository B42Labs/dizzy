package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/spf13/cobra"

	"github.com/B42Labs/dizzy/internal/keystone"
	keystoneplan "github.com/B42Labs/dizzy/internal/keystone/plan"
	keystonescenario "github.com/B42Labs/dizzy/internal/keystone/scenario"
	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/pressure"
	"github.com/B42Labs/dizzy/internal/pressure/keystoneops"
	"github.com/B42Labs/dizzy/internal/resource"
	"github.com/B42Labs/dizzy/internal/run"
	"github.com/B42Labs/dizzy/scenarios"
)

// keystonePressureScenarioYAML is sampleKeystoneScenarioYAML extended with a
// pressure block that supplies a duration, used where a test needs the
// command to get past the duration check.
const keystonePressureScenarioYAML = sampleKeystoneScenarioYAML + `
pressure:
  duration: 1m
  workers: 4
  tokens: 10
`

// fanScenarioYAML is a Keystone scenario of five projects, three regular
// users whose grants are all project-scoped, and one fan user, fan-0001, with
// grants on two of the projects: the plan whose fan user the staged runs
// against fakePressureKeystone grow.
const fanScenarioYAML = `
name: fan
seed: 7
resources:
  domains:  1
  roles:    2
  projects: 5
  users:    3
distribution:
  projects_per_domain:            { min: 5, max: 5 }
  assignments_per_user:           { min: 1, max: 2 }
  domain_scoped_assignment_ratio: 0
  users_issuing_tokens_ratio:     0.5
  fan_users:                      1
  fan_assignments:                { min: 2, max: 2 }
`

// fanStagesScenarioYAML is a Keystone scenario whose fan user holds 500 of its
// 1000 projects' grants, with a pressure block of two stages without a
// duration that grow the fan user to 500 and then 1000 grants. It sets no
// duration, so the command needs --duration.
const fanStagesScenarioYAML = `
name: fan-stages
seed: 7
resources:
  domains:  1
  roles:    2
  projects: 1000
  users:    3
distribution:
  projects_per_domain:  { min: 1000, max: 1000 }
  assignments_per_user: { min: 1, max: 2 }
  fan_users:            1
  fan_assignments:      { min: 500, max: 500 }
pressure:
  stages:
    - fan_assignments: 500
    - fan_assignments: 1000
`

// adminTierMessage is the exact rejection of a caller below the admin tier.
const adminTierMessage = `keystone pressure needs the admin tier: validating and revoking other users' tokens and listing their role assignments are admin operations; the caller resolved to tier "domain-manager"`

// noIdentityCloud points the cloud configuration at a file that does not
// exist, so a command that gets past its pre-cloud checks fails at the
// identity client and never reaches a real API.
func noIdentityCloud(t *testing.T) {
	t.Helper()
	t.Setenv("OS_CLOUD", "")
	t.Setenv("OS_CLIENT_CONFIG_FILE", "/nonexistent/clouds.yaml")
}

// execPressure runs "keystone pressure --scenario <body>" with extra args
// against an unreachable cloud and returns the command error.
func execPressure(t *testing.T, body string, args ...string) error {
	t.Helper()
	noIdentityCloud(t)
	path := writeScenario(t, body)
	_, err := execRoot(t, append([]string{"keystone", "pressure", "--scenario", path}, args...)...)
	return err
}

// wantIdentityClient fails the test unless err is the identity client failure
// of a run that passed every pre-cloud check.
func wantIdentityClient(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a client-creation failure, got nil")
	}
	if !strings.Contains(err.Error(), "identity client") {
		t.Errorf("error %q failed before reaching the identity client", err.Error())
	}
}

// wantExactError fails the test unless err's text is exactly want.
func wantExactError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %q, got nil", want)
	}
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// setPressureFlags sets flags and each --mix value on cmd as a command line
// would and returns the pressureFlags value the command's flag variables then
// hold, the value mergePressureConfig receives.
func setPressureFlags(t *testing.T, cmd *cobra.Command, flags map[string]string, mix ...string) pressureFlags {
	t.Helper()
	fs := cmd.Flags()
	for name, value := range flags {
		if err := fs.Set(name, value); err != nil {
			t.Fatalf("setting --%s %s: %v", name, value, err)
		}
	}
	for _, v := range mix {
		if err := fs.Set("mix", v); err != nil {
			t.Fatalf("setting --mix %s: %v", v, err)
		}
	}
	var (
		f   pressureFlags
		err error
	)
	if f.duration, err = fs.GetDuration("duration"); err != nil {
		t.Fatalf("reading --duration: %v", err)
	}
	if f.bucketWidth, err = fs.GetDuration("bucket-width"); err != nil {
		t.Fatalf("reading --bucket-width: %v", err)
	}
	if f.workers, err = fs.GetInt("workers"); err != nil {
		t.Fatalf("reading --workers: %v", err)
	}
	if f.tokens, err = fs.GetInt("tokens"); err != nil {
		t.Fatalf("reading --tokens: %v", err)
	}
	if f.churnUsers, err = fs.GetInt("churn-users"); err != nil {
		t.Fatalf("reading --churn-users: %v", err)
	}
	if f.mix, err = fs.GetStringArray("mix"); err != nil {
		t.Fatalf("reading --mix: %v", err)
	}
	return f
}

// mergePressure is mergePressureScenario over the sample scenario with block
// appended, without the stage targets.
func mergePressure(t *testing.T, block string, flags map[string]string, mix ...string) (pressure.Config, keystoneops.Options, error) {
	t.Helper()
	cfg, o, _, err := mergePressureScenario(t, sampleKeystoneScenarioYAML+"\n"+block, flags, mix...)
	return cfg, o, err
}

// mergePressureScenario parses the scenario body, sets flags and mix on a
// fresh pressure command, and returns mergePressureConfig's result with
// opts.timeout 42s.
func mergePressureScenario(t *testing.T, body string, flags map[string]string, mix ...string) (pressure.Config, keystoneops.Options, []int, error) {
	t.Helper()
	s, err := keystonescenario.Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	opts := &globalOptions{concurrency: 8, timeout: 42 * time.Second}
	cmd := newKeystonePressureCmd(opts)
	return mergePressureConfig(cmd, opts, s, setPressureFlags(t, cmd, flags, mix...))
}

func TestKeystonePressureRequiresScenario(t *testing.T) {
	if _, err := execRoot(t, "keystone", "pressure"); err == nil {
		t.Fatal("keystone pressure without --scenario: expected error, got nil")
	}
}

func TestKeystonePressureRequiresDuration(t *testing.T) {
	// No pressure block and no --duration: the merged config has no duration
	// and is rejected before the identity client.
	err := execPressure(t, sampleKeystoneScenarioYAML)
	if err == nil {
		t.Fatal("keystone pressure without a duration: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "pressure duration must be set and positive") {
		t.Errorf("error %q does not name the missing duration", err.Error())
	}
	if strings.Contains(err.Error(), "identity client") {
		t.Errorf("error %q reached the identity client; the duration check must come first", err.Error())
	}
}

func TestKeystonePressureShippedProfilesRunWithoutFlags(t *testing.T) {
	// Each built-in Keystone profile ships a pressure block, so the command
	// needs no flag: it passes every pre-cloud check and fails only at the
	// identity client.
	for _, name := range []string{"small", "medium", "large"} {
		t.Run(name, func(t *testing.T) {
			data, err := scenarios.Files.ReadFile("keystone/" + name + ".yaml")
			if err != nil {
				t.Fatalf("reading shipped profile %s.yaml: %v", name, err)
			}
			wantIdentityClient(t, execPressure(t, string(data)))
		})
	}
}

func TestKeystonePressureDurationZero(t *testing.T) {
	t.Run("unbounded run reaches the identity client", func(t *testing.T) {
		wantIdentityClient(t, execPressure(t, sampleKeystoneScenarioYAML, "--duration", "0"))
	})
	t.Run("bucket width below a minute is rejected", func(t *testing.T) {
		err := execPressure(t, sampleKeystoneScenarioYAML, "--duration", "0", "--bucket-width", "30s")
		wantExactError(t, err, "pressure bucket-width must be at least 1m0s, got 30s")
	})
}

func TestParseMixFlag(t *testing.T) {
	tests := []struct {
		name    string
		values  []string
		want    map[string]int
		wantErr string
	}{
		{"no values", nil, map[string]int{}, ""},
		{"valid entries", []string{"validate=10", "revoke=0", "probe=0", "bad_token=3"}, map[string]int{"validate": 10, "revoke": 0, "probe": 0, "bad_token": 3}, ""},
		{"every operation", []string{"validate=1", "probe=2", "effective=3", "revoke=4", "grant_churn=5", "issue=6", "bad_token=7"},
			map[string]int{"validate": 1, "probe": 2, "effective": 3, "revoke": 4, "grant_churn": 5, "issue": 6, "bad_token": 7}, ""},
		{"a later value wins", []string{"validate=1", "issue=4", "validate=2"}, map[string]int{"validate": 2, "issue": 4}, ""},
		{"no equals sign", []string{"validate"}, nil, `--mix: want <operation>=<weight>, got "validate"`},
		{"unknown operation", []string{"nope=1"}, nil, `--mix: unknown operation "nope"`},
		{"negative weight", []string{"validate=-1"}, nil, `--mix: weight for "validate" must be a non-negative integer, got "-1"`},
		{"weight not a number", []string{"revoke=x"}, nil, `--mix: weight for "revoke" must be a non-negative integer, got "x"`},
		{"empty weight", []string{"issue="}, nil, `--mix: weight for "issue" must be a non-negative integer, got ""`},
		{"error after a valid entry", []string{"validate=1", "nope=1"}, nil, `--mix: unknown operation "nope"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMixFlag(tc.values)
			if tc.wantErr != "" {
				wantExactError(t, err, tc.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("parseMixFlag: %v", err)
			}
			if !maps.Equal(got, tc.want) {
				t.Errorf("parseMixFlag(%q) = %v, want %v", tc.values, got, tc.want)
			}
		})
	}
}

func TestKeystonePressureMixFlagErrors(t *testing.T) {
	tests := []struct {
		mix  string
		want string
	}{
		{"validate", `--mix: want <operation>=<weight>, got "validate"`},
		{"nope=1", `--mix: unknown operation "nope"`},
		{"validate=-1", `--mix: weight for "validate" must be a non-negative integer, got "-1"`},
	}
	for _, tc := range tests {
		t.Run(tc.mix, func(t *testing.T) {
			wantExactError(t, execPressure(t, keystonePressureScenarioYAML, "--mix", tc.mix), tc.want)
		})
	}
}

func TestKeystonePressureAllZeroMixFails(t *testing.T) {
	err := execPressure(t, keystonePressureScenarioYAML,
		"--mix", "validate=0", "--mix", "effective=0", "--mix", "revoke=0", "--mix", "grant_churn=0", "--mix", "issue=0")
	wantExactError(t, err, pressure.ErrNoWeight.Error())
	if !errors.Is(err, pressure.ErrNoWeight) {
		t.Errorf("error %v does not match pressure.ErrNoWeight", err)
	}
}

func TestKeystonePressureRejectsUnsupportedOperations(t *testing.T) {
	const probeMessage = `pressure mix weight "probe" is not supported by this build; supported operations: validate, effective, revoke, grant_churn, issue`
	const badTokenMessage = `pressure mix weight "bad_token" is not supported by this build; supported operations: validate, effective, revoke, grant_churn, issue`

	t.Run("--mix probe=1", func(t *testing.T) {
		wantExactError(t, execPressure(t, keystonePressureScenarioYAML, "--mix", "probe=1"), probeMessage)
	})
	t.Run("--mix bad_token=1", func(t *testing.T) {
		wantExactError(t, execPressure(t, keystonePressureScenarioYAML, "--mix", "bad_token=1"), badTokenMessage)
	})
	t.Run("block probe: 1", func(t *testing.T) {
		body := keystonePressureScenarioYAML + "  mix: { probe: 1 }\n"
		wantExactError(t, execPressure(t, body), probeMessage)
	})
	t.Run("block bad_token: 1", func(t *testing.T) {
		body := keystonePressureScenarioYAML + "  mix: { bad_token: 1 }\n"
		wantExactError(t, execPressure(t, body), badTokenMessage)
	})
	t.Run("block propagation every 30s", func(t *testing.T) {
		body := keystonePressureScenarioYAML + "  propagation: { every: 30s }\n"
		wantExactError(t, execPressure(t, body), "pressure.propagation is not supported by this build")
	})
	t.Run("block probe: 0 and bad_token: 0 without propagation is accepted", func(t *testing.T) {
		body := keystonePressureScenarioYAML + "  mix: { probe: 0, bad_token: 0 }\n"
		wantIdentityClient(t, execPressure(t, body))
	})
}

func TestCheckPressureSupportedChecksProbeFirst(t *testing.T) {
	o := keystoneops.Options{Weights: map[string]int{opProbe: 1, opBadToken: 1}}
	err := checkPressureSupported(o, keystonescenario.Scenario{})
	if err == nil || !strings.Contains(err.Error(), `"probe"`) {
		t.Errorf("checkPressureSupported with probe and bad_token positive = %v, want the probe message", err)
	}
}

func TestMergePressureConfigDefaults(t *testing.T) {
	cfg, o, err := mergePressure(t, "", nil)
	if err != nil {
		t.Fatalf("mergePressureConfig: %v", err)
	}
	if cfg.Workers != 16 || cfg.BucketWidth != time.Hour || cfg.OpTimeout != 42*time.Second {
		t.Errorf("Workers/BucketWidth/OpTimeout = %d/%s/%s, want 16/1h0m0s/42s", cfg.Workers, cfg.BucketWidth, cfg.OpTimeout)
	}
	if cfg.Duration != 0 || cfg.Unbounded {
		t.Errorf("Duration/Unbounded = %s/%v, want 0s/false: there is no default duration", cfg.Duration, cfg.Unbounded)
	}
	if cfg.Seed != 0 || cfg.Classify != nil || cfg.CheckpointInterval != 0 || cfg.OnCheckpoint != nil {
		t.Error("mergePressureConfig set the seed, the classifier or the checkpoint; the command adds them")
	}
	if o.Tokens != 100 || o.ChurnUsers != 1 || o.OpTimeout != 42*time.Second {
		t.Errorf("Tokens/ChurnUsers/OpTimeout = %d/%d/%s, want 100/1/42s", o.Tokens, o.ChurnUsers, o.OpTimeout)
	}
	want := map[string]int{"validate": 60, "effective": 5, "revoke": 2, "grant_churn": 2, "issue": 1, "probe": 0, "bad_token": 0}
	if !maps.Equal(o.Weights, want) {
		t.Errorf("Weights = %v, want %v", o.Weights, want)
	}
}

func TestMergePressureConfigLayers(t *testing.T) {
	const block = `pressure:
  duration: 5m
  workers: 32
  tokens: 20
  churn_users: 2
  bucket_width: 30m
  mix: { validate: 10, revoke: 0 }
`
	tests := []struct {
		name           string
		block          string
		flags          map[string]string
		mix            []string
		wantDuration   time.Duration
		wantWorkers    int
		wantWidth      time.Duration
		wantTokens     int
		wantChurnUsers int
		wantWeights    map[string]int
	}{
		{
			name: "block values override the defaults", block: block,
			wantDuration: 5 * time.Minute, wantWorkers: 32, wantWidth: 30 * time.Minute, wantTokens: 20, wantChurnUsers: 2,
			wantWeights: map[string]int{"validate": 10, "effective": 5, "revoke": 0, "grant_churn": 2, "issue": 1, "probe": 0, "bad_token": 0},
		},
		{
			name: "flags override the block", block: block,
			flags:        map[string]string{"duration": "10m", "workers": "64", "tokens": "50", "churn-users": "3", "bucket-width": "10m"},
			mix:          []string{"validate=7", "revoke=4"},
			wantDuration: 10 * time.Minute, wantWorkers: 64, wantWidth: 10 * time.Minute, wantTokens: 50, wantChurnUsers: 3,
			wantWeights: map[string]int{"validate": 7, "effective": 5, "revoke": 4, "grant_churn": 2, "issue": 1, "probe": 0, "bad_token": 0},
		},
		{
			name:        "--workers 0 and --tokens 0 without block values take the defaults",
			flags:       map[string]string{"workers": "0", "tokens": "0"},
			wantWorkers: 16, wantWidth: time.Hour, wantTokens: 100, wantChurnUsers: 1,
			wantWeights: defaultPressureMix(),
		},
		{
			name: "--workers 0 and --tokens 0 take the block values", block: "pressure:\n  workers: 32\n  tokens: 20\n",
			flags:       map[string]string{"workers": "0", "tokens": "0"},
			wantWorkers: 32, wantWidth: time.Hour, wantTokens: 20, wantChurnUsers: 1,
			wantWeights: defaultPressureMix(),
		},
		{
			name: "--churn-users 0 reserves no user", block: block,
			flags:        map[string]string{"churn-users": "0"},
			wantDuration: 5 * time.Minute, wantWorkers: 32, wantWidth: 30 * time.Minute, wantTokens: 20, wantChurnUsers: 0,
			wantWeights: map[string]int{"validate": 10, "effective": 5, "revoke": 0, "grant_churn": 2, "issue": 1, "probe": 0, "bad_token": 0},
		},
		{
			name: "block churn_users: 0 reserves no user", block: "pressure:\n  churn_users: 0\n",
			wantWorkers: 16, wantWidth: time.Hour, wantTokens: 100, wantChurnUsers: 0,
			wantWeights: defaultPressureMix(),
		},
		{
			name:        "a block weight 0 switches an operation off, an omitted key keeps the default",
			block:       "pressure:\n  mix:\n    grant_churn: 0\n",
			wantWorkers: 16, wantWidth: time.Hour, wantTokens: 100, wantChurnUsers: 1,
			wantWeights: map[string]int{"validate": 60, "effective": 5, "revoke": 2, "grant_churn": 0, "issue": 1, "probe": 0, "bad_token": 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, o, err := mergePressure(t, tc.block, tc.flags, tc.mix...)
			if err != nil {
				t.Fatalf("mergePressureConfig: %v", err)
			}
			if cfg.Duration != tc.wantDuration || cfg.Workers != tc.wantWorkers || cfg.BucketWidth != tc.wantWidth {
				t.Errorf("Duration/Workers/BucketWidth = %s/%d/%s, want %s/%d/%s",
					cfg.Duration, cfg.Workers, cfg.BucketWidth, tc.wantDuration, tc.wantWorkers, tc.wantWidth)
			}
			if o.Tokens != tc.wantTokens || o.ChurnUsers != tc.wantChurnUsers {
				t.Errorf("Tokens/ChurnUsers = %d/%d, want %d/%d", o.Tokens, o.ChurnUsers, tc.wantTokens, tc.wantChurnUsers)
			}
			if !maps.Equal(o.Weights, tc.wantWeights) {
				t.Errorf("Weights = %v, want %v", o.Weights, tc.wantWeights)
			}
		})
	}
}

func TestMergePressureConfigRejects(t *testing.T) {
	tests := []struct {
		name  string
		block string
		flags map[string]string
		want  string
	}{
		{"negative --churn-users", "", map[string]string{"churn-users": "-1"}, "--churn-users must not be negative, got -1"},
		{"negative --tokens", "", map[string]string{"tokens": "-1"}, "pressure tokens must be at least 1, got -1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := mergePressure(t, tc.block, tc.flags)
			wantExactError(t, err, tc.want)
		})
	}
}

// pressureModeCases are chaosModeCases adapted to the pressure block: --duration
// 0 given as a flag selects the unbounded run while the block's duration never
// does, and the bucket width falls back to 1h unless the block or the flag
// sets it.
var pressureModeCases = []struct {
	name          string
	block         string
	flags         map[string]string
	wantUnbounded bool
	wantDuration  time.Duration
	wantWidth     time.Duration
}{
	{"--duration 0 over a block duration", "pressure:\n  duration: 1m\n", map[string]string{"duration": "0"}, true, 0, time.Hour},
	{"--duration 2h over a block duration", "pressure:\n  duration: 1m\n", map[string]string{"duration": "2h"}, false, 2 * time.Hour, time.Hour},
	{"no duration anywhere", "pressure:\n  workers: 4\n", nil, false, 0, time.Hour},
	{"block duration 0 is unset, not unbounded", "pressure:\n  duration: 0\n", nil, false, 0, time.Hour},
	{"block bucket width", "pressure:\n  duration: 1m\n  bucket_width: 30m\n", nil, false, time.Minute, 30 * time.Minute},
	{"--bucket-width over the block", "pressure:\n  duration: 1m\n  bucket_width: 30m\n", map[string]string{"bucket-width": "10m"}, false, time.Minute, 10 * time.Minute},
	{"zero block bucket width is unset", "pressure:\n  duration: 1m\n  bucket_width: 0\n", nil, false, time.Minute, time.Hour},
	{"no pressure block", "", nil, false, 0, time.Hour},
}

func TestMergePressureConfigDurationAndBucketWidth(t *testing.T) {
	for _, tc := range pressureModeCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := mergePressure(t, tc.block, tc.flags)
			if err != nil {
				t.Fatalf("mergePressureConfig: %v", err)
			}
			if cfg.Unbounded != tc.wantUnbounded || cfg.Duration != tc.wantDuration || cfg.BucketWidth != tc.wantWidth {
				t.Errorf("merged Unbounded/Duration/BucketWidth = %v/%s/%s, want %v/%s/%s",
					cfg.Unbounded, cfg.Duration, cfg.BucketWidth, tc.wantUnbounded, tc.wantDuration, tc.wantWidth)
			}
		})
	}
}

func TestStageWindows(t *testing.T) {
	tests := []struct {
		name     string
		stages   []time.Duration // 0 for a stage without a duration
		duration time.Duration
		want     []time.Duration
		wantErr  string
	}{
		{"stages without a duration share the run equally", []time.Duration{0, 0, 0}, 3 * time.Minute,
			[]time.Duration{time.Minute, time.Minute, time.Minute}, ""},
		{"stages without a duration share what the others leave", []time.Duration{2 * time.Minute, 0, 0}, 5 * time.Minute,
			[]time.Duration{2 * time.Minute, 90 * time.Second, 90 * time.Second}, ""},
		{"the last stage without a duration takes the remainder", []time.Duration{0, 0}, time.Second + 1,
			[]time.Duration{500 * time.Millisecond, 500*time.Millisecond + 1}, ""},
		{"the last stage runs to the end", []time.Duration{time.Minute, time.Minute}, 3 * time.Minute,
			[]time.Duration{time.Minute, 2 * time.Minute}, ""},
		{"durations past the run", []time.Duration{2 * time.Minute, 2 * time.Minute}, 3 * time.Minute, nil,
			"pressure.stages: the stage durations add up to 4m0s, more than the run duration 3m0s"},
		{"durations that fill the run", []time.Duration{3 * time.Minute, 0}, 3 * time.Minute, nil,
			"pressure.stages: the stages with a duration fill the run duration 3m0s, leaving nothing for the 1 stage(s) without one"},
		{"a rest too short to share", []time.Duration{time.Minute, 0, 0}, time.Minute + 1, nil,
			"pressure.stages: the stages with a duration fill the run duration 1m0.000000001s, leaving nothing for the 2 stage(s) without one"},
		{"durations whose sum overflows", []time.Duration{math.MaxInt64, 1}, 3 * time.Minute, nil,
			"pressure.stages: the stage durations add up to 2562047h47m16.854775807s, more than the run duration 3m0s"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stages := make([]keystonescenario.Stage, len(tc.stages))
			for i, d := range tc.stages {
				stages[i].Duration = keystonescenario.Duration(d)
			}
			got, err := stageWindows(stages, tc.duration)
			if tc.wantErr != "" {
				wantExactError(t, err, tc.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("stageWindows: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("stageWindows(%v, %s) = %v, want %v", tc.stages, tc.duration, got, tc.want)
			}
			var sum time.Duration
			for _, d := range got {
				sum += d
			}
			if sum != tc.duration {
				t.Errorf("the windows add up to %s, want the run duration %s", sum, tc.duration)
			}
		})
	}
}

func TestMergePressureConfigStages(t *testing.T) {
	t.Run("--duration 4m splits into the stages", func(t *testing.T) {
		cfg, _, targets, err := mergePressureScenario(t, fanStagesScenarioYAML, map[string]string{"duration": "4m"})
		if err != nil {
			t.Fatalf("mergePressureConfig: %v", err)
		}
		if len(cfg.Stages) != 2 {
			t.Fatalf("got %d stages, want 2: %+v", len(cfg.Stages), cfg.Stages)
		}
		for i, st := range cfg.Stages {
			if st.Duration != 2*time.Minute {
				t.Errorf("stage %d duration = %s, want 2m0s", i+1, st.Duration)
			}
			if st.Enter != nil {
				t.Errorf("stage %d has an Enter; runKeystonePressure sets it", i+1)
			}
		}
		if want := []int{500, 1000}; !slices.Equal(targets, want) {
			t.Errorf("targets = %v, want %v", targets, want)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("the merged staged config is invalid: %v", err)
		}
	})
	t.Run("no duration anywhere leaves the stages to the duration check", func(t *testing.T) {
		cfg, _, targets, err := mergePressureScenario(t, fanStagesScenarioYAML, nil)
		if err != nil {
			t.Fatalf("mergePressureConfig: %v", err)
		}
		if cfg.Stages != nil || targets != nil {
			t.Errorf("Stages/targets = %+v/%v, want none without a duration", cfg.Stages, targets)
		}
		wantExactError(t, cfg.Validate(), "pressure duration must be set and positive, got 0s")
	})
}

func TestKeystonePressureStageScenarioErrors(t *testing.T) {
	t.Run("--duration 0 with stages", func(t *testing.T) {
		err := execPressure(t, fanStagesScenarioYAML, "--duration", "0")
		wantExactError(t, err, "pressure.stages need a bounded run; --duration 0 selects a run without an end")
	})
	t.Run("a stage below distribution.fan_assignments.max", func(t *testing.T) {
		err := execPressure(t, fanStagesScenarioYAML, "--duration", "4m", "--set", "distribution.fan_assignments.max=600")
		wantExactError(t, err, "invalid scenario: pressure.stages[0].fan_assignments (500) is below distribution.fan_assignments.max (600), which a fan user may already hold")
	})
	t.Run("a bounded staged run reaches the identity client", func(t *testing.T) {
		wantIdentityClient(t, execPressure(t, fanStagesScenarioYAML, "--duration", "4m"))
	})
}

func TestKeystonePressureWorkersCeiling(t *testing.T) {
	err := execPressure(t, keystonePressureScenarioYAML, "--workers", "2000")
	wantExactError(t, err, "pressure workers must be between 1 and 1024, got 2000")
}

func TestKeystonePressureRequiresAdminTier(t *testing.T) {
	t.Run("--privilege domain-manager", func(t *testing.T) {
		wantExactError(t, execPressure(t, keystonePressureScenarioYAML, "--privilege", "domain-manager"), adminTierMessage)
	})
	t.Run("--privilege bogus", func(t *testing.T) {
		err := execPressure(t, keystonePressureScenarioYAML, "--privilege", "bogus")
		wantExactError(t, err, `invalid --privilege "bogus": want auto, admin, or domain-manager`)
	})
}

// identityServer serves GET /auth/tokens with body and counts every request.
func identityServer(t *testing.T, body string) (*gophercloud.ServiceClient, *int) {
	t.Helper()
	var requests int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/auth/tokens") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	gc := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{TokenID: "caller-token"},
		Endpoint:       ts.URL + "/",
	}
	return gc, &requests
}

func TestResolvePressureTier(t *testing.T) {
	const managerToken = `{"token":{"roles":[{"id":"r2","name":"manager"}],"domain":{"id":"d9","name":"managed"}}}`
	const adminToken = `{"token":{"roles":[{"id":"r1","name":"admin"}],"project":{"id":"p1","name":"proj","domain":{"id":"d1","name":"dom"}}}}`

	t.Run("an auto-detected domain manager gets the admin-tier message", func(t *testing.T) {
		gc, requests := identityServer(t, managerToken)
		_, err := resolvePressureTier(context.Background(), gc, keystonePrivilegeFlags{privilege: "auto", roles: defaultKeystoneRoles})
		wantExactError(t, err, adminTierMessage)
		if *requests != 1 {
			t.Errorf("classification made %d requests, want 1", *requests)
		}
	})
	t.Run("an auto-detected admin is accepted", func(t *testing.T) {
		gc, _ := identityServer(t, adminToken)
		tier, err := resolvePressureTier(context.Background(), gc, keystonePrivilegeFlags{privilege: "auto", roles: defaultKeystoneRoles})
		if err != nil || tier != keystone.TierAdmin {
			t.Errorf("resolvePressureTier = %q, %v; want %q, nil", tier, err, keystone.TierAdmin)
		}
	})
	t.Run("--privilege admin makes no request", func(t *testing.T) {
		gc, requests := identityServer(t, managerToken)
		tier, err := resolvePressureTier(context.Background(), gc, keystonePrivilegeFlags{privilege: "admin", domain: "ignored", roles: defaultKeystoneRoles})
		if err != nil || tier != keystone.TierAdmin {
			t.Errorf("resolvePressureTier = %q, %v; want %q, nil", tier, err, keystone.TierAdmin)
		}
		if *requests != 0 {
			t.Errorf("--privilege admin made %d identity requests, want none", *requests)
		}
	})
	t.Run("--privilege domain-manager makes no request", func(t *testing.T) {
		gc, requests := identityServer(t, adminToken)
		_, err := resolvePressureTier(context.Background(), gc, keystonePrivilegeFlags{privilege: "domain-manager", roles: defaultKeystoneRoles})
		wantExactError(t, err, adminTierMessage)
		if *requests != 0 {
			t.Errorf("--privilege domain-manager made %d identity requests, want none", *requests)
		}
	})
}

func TestRequirePressureAdminTier(t *testing.T) {
	if err := requirePressureAdminTier(keystone.TierAdmin); err != nil {
		t.Errorf("requirePressureAdminTier(admin) = %v, want nil", err)
	}
	wantExactError(t, requirePressureAdminTier(keystone.TierDomainManager), adminTierMessage)
}

// pressureCreated is a provisioned run's resources in dependency order.
var pressureCreated = []resource.Resource{
	{Kind: keystone.KindDomain, ID: "d1", Name: "dizzy-run1234-dom-0001"},
	{Kind: keystone.KindRole, ID: "r1", Name: "dizzy-run1234-role-0001"},
	{Kind: keystone.KindProject, ID: "p1", Name: "dizzy-run1234-proj-0001"},
	{Kind: keystone.KindUser, ID: "u1", Name: "dizzy-run1234-user-0001"},
	{Kind: keystone.KindAssignment, ID: "u1:project:p1:r1"},
}

// failingUserDeleteCleaner is a recordingKeystoneCleaner whose user deletes
// fail, so a teardown stops after the assignments.
type failingUserDeleteCleaner struct {
	*recordingKeystoneCleaner
	err error
}

func (c failingUserDeleteCleaner) Delete(ctx context.Context, r resource.Resource) error {
	if r.Kind == keystone.KindUser {
		c.observe(ctx)
		return c.err
	}
	return c.recordingKeystoneCleaner.Delete(ctx, r)
}

func TestFailPressureSetup(t *testing.T) {
	cause := fmt.Errorf("creating project: %w", keystone.ErrForbidden)
	const wantPrefix = "provisioning plan (run run1234): "

	t.Run("tears down after writing the record", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // a signal may have ended the setup; the teardown must still run
		recordWritten := false
		c := &recordingKeystoneCleaner{recordWritten: &recordWritten}
		writeRecord := func() (string, error) {
			recordWritten = true
			return "run-run1234.json", nil
		}
		var out bytes.Buffer

		err := failPressureSetup(ctx, &out, c, "run1234", pressureCreated, false, time.Second, "provisioning plan", cause, writeRecord)
		if err == nil {
			t.Fatal("failPressureSetup returned nil; a failed setup must exit non-zero")
		}
		if err.Error() != wantPrefix+cause.Error() {
			t.Errorf("error = %q, want %q", err.Error(), wantPrefix+cause.Error())
		}
		if !errors.Is(err, keystone.ErrForbidden) {
			t.Errorf("error %v does not wrap keystone.ErrForbidden", err)
		}
		if !c.firstCallSawRecord {
			t.Error("the run record was not written before teardown began")
		}
		if c.sawCancelled {
			t.Error("teardown ran with a cancelled context; it must run on context.WithoutCancel")
		}
		wantOrder := []resource.Kind{keystone.KindAssignment, keystone.KindUser, keystone.KindProject, keystone.KindRole, keystone.KindDomain}
		if len(c.deleted) != len(wantOrder) {
			t.Fatalf("deleted %d resources, want %d: %+v", len(c.deleted), len(wantOrder), c.deleted)
		}
		for i, want := range wantOrder {
			if c.deleted[i].Kind != want {
				t.Errorf("deleted[%d] kind = %q, want %q", i, c.deleted[i].Kind, want)
			}
		}
		if !strings.Contains(out.String(), "deleted 5 resource(s) for run run1234") {
			t.Errorf("output %q missing the deletion count", out.String())
		}
	})

	t.Run("--no-cleanup leaves the resources", func(t *testing.T) {
		c := &recordingKeystoneCleaner{}
		writeRecord := func() (string, error) { return "run-run1234.json", nil }
		var out bytes.Buffer

		err := failPressureSetup(context.Background(), &out, c, "run1234", pressureCreated, true, time.Second, "provisioning plan", cause, writeRecord)
		if err == nil || err.Error() != wantPrefix+cause.Error() {
			t.Errorf("error = %v, want %q", err, wantPrefix+cause.Error())
		}
		if !errors.Is(err, keystone.ErrForbidden) {
			t.Errorf("error %v does not wrap keystone.ErrForbidden", err)
		}
		if c.calls != 0 {
			t.Errorf("cleaner was called %d times; --no-cleanup must leave everything in place", c.calls)
		}
		want := "pressure setup failed; resources left in place — reclaim with: keystone cleanup --run run-run1234.json"
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q missing %q", out.String(), want)
		}
	})

	t.Run("--no-cleanup without a record falls back to the run id", func(t *testing.T) {
		writeRecord := func() (string, error) { return "", errors.New("disk full") }
		var out bytes.Buffer
		_ = failPressureSetup(context.Background(), &out, &recordingKeystoneCleaner{}, "run1234", nil, true, time.Second, "provisioning plan", cause, writeRecord)
		if !strings.Contains(out.String(), "keystone cleanup --run-id run1234") {
			t.Errorf("output %q missing the run-id hint", out.String())
		}
	})

	t.Run("an incomplete teardown names both errors", func(t *testing.T) {
		deleteErr := errors.New("delete boom")
		c := failingUserDeleteCleaner{&recordingKeystoneCleaner{}, deleteErr}
		writeRecord := func() (string, error) { return "run-run1234.json", nil }
		var out bytes.Buffer

		err := failPressureSetup(context.Background(), &out, c, "run1234", pressureCreated, false, time.Second, "provisioning plan", cause, writeRecord)
		if err == nil {
			t.Fatal("failPressureSetup returned nil")
		}
		if !strings.HasPrefix(err.Error(), wantPrefix) {
			t.Errorf("error %q does not start with %q", err.Error(), wantPrefix)
		}
		if !errors.Is(err, keystone.ErrForbidden) || !errors.Is(err, deleteErr) {
			t.Errorf("error %v does not wrap both the cause and the teardown error", err)
		}
		for _, want := range []string{"teardown incomplete after deleting 1 resource(s)", "keystone cleanup --run run-run1234.json"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err.Error(), want)
			}
		}
	})
}

// opStats returns an aggregate with one ByType entry per name, each with the
// given number of succeeded samples.
func opStats(succeeded map[string]int) metrics.Aggregate {
	var agg metrics.Aggregate
	for _, name := range []string{"effective", "grant_churn", "issue", "revoke", "validate"} {
		if n, ok := succeeded[name]; ok {
			agg.ByType = append(agg.ByType, metrics.Stats{Type: name, Attempted: n + 1, Succeeded: n, Failed: 1})
		}
	}
	return agg
}

func TestPressureStats(t *testing.T) {
	r := &pressure.Result{
		Workers:    8,
		Operations: opStats(map[string]int{"validate": 50, "revoke": 3}),
		Buckets: []pressure.Bucket{
			{Start: 0, Operations: opStats(map[string]int{"validate": 20, "revoke": 1})},
			{Start: time.Minute, Operations: opStats(map[string]int{"validate": 10})},
			{Start: 2 * time.Minute, Operations: opStats(map[string]int{"validate": 20, "revoke": 2})},
		},
		BucketWidth: time.Minute,
	}

	t.Run("grant_churn off", func(t *testing.T) {
		o := keystoneops.Options{
			Tokens:     20,
			ChurnUsers: 3,
			Weights:    map[string]int{"validate": 60, "effective": 5, "revoke": 2, "grant_churn": 0, "issue": 1, "probe": 0, "bad_token": 0},
		}
		ps := pressureStats(r, o, nil)
		if ps.Workers != 8 || ps.Tokens != 20 || ps.BucketWidth != time.Minute {
			t.Errorf("Workers/Tokens/BucketWidth = %d/%d/%s, want 8/20/1m0s", ps.Workers, ps.Tokens, ps.BucketWidth)
		}
		if ps.ChurnUsers != 0 {
			t.Errorf("ChurnUsers = %d, want 0 while grant_churn is off", ps.ChurnUsers)
		}
		if ps.Revocations != 3 {
			t.Errorf("Revocations = %d, want 3, the run's succeeded revokes", ps.Revocations)
		}
		if !reflect.DeepEqual(ps.Operations, r.Operations) {
			t.Error("Operations is not the run's aggregate")
		}
		wantMix := []run.PressureWeight{{Operation: "effective", Weight: 5}, {Operation: "issue", Weight: 1}, {Operation: "revoke", Weight: 2}, {Operation: "validate", Weight: 60}}
		if !reflect.DeepEqual(ps.Mix, wantMix) {
			t.Errorf("Mix = %+v, want %+v", ps.Mix, wantMix)
		}
		wantRevocations := []int{1, 1, 3}
		if len(ps.Buckets) != len(wantRevocations) {
			t.Fatalf("got %d buckets, want %d", len(ps.Buckets), len(wantRevocations))
		}
		for i, want := range wantRevocations {
			b := ps.Buckets[i]
			if b.Revocations != want {
				t.Errorf("bucket %d cumulative revocations = %d, want %d", i, b.Revocations, want)
			}
			if b.Start != r.Buckets[i].Start || !reflect.DeepEqual(b.Operations, r.Buckets[i].Operations) {
				t.Errorf("bucket %d = start %s, ops %+v; want the engine bucket", i, b.Start, b.Operations)
			}
		}
	})

	t.Run("grant_churn on", func(t *testing.T) {
		o := keystoneops.Options{Tokens: 20, ChurnUsers: 3, Weights: map[string]int{"validate": 60, "grant_churn": 2}}
		ps := pressureStats(r, o, nil)
		if ps.ChurnUsers != 3 {
			t.Errorf("ChurnUsers = %d, want 3 while grant_churn is on", ps.ChurnUsers)
		}
		wantMix := []run.PressureWeight{{Operation: "grant_churn", Weight: 2}, {Operation: "validate", Weight: 60}}
		if !reflect.DeepEqual(ps.Mix, wantMix) {
			t.Errorf("Mix = %+v, want %+v", ps.Mix, wantMix)
		}
	})

	t.Run("no revocations and no buckets", func(t *testing.T) {
		ps := pressureStats(&pressure.Result{Workers: 1, Operations: opStats(map[string]int{"validate": 4})}, keystoneops.Options{Tokens: 1, Weights: map[string]int{"validate": 1}}, nil)
		if ps.Revocations != 0 || ps.Buckets != nil {
			t.Errorf("Revocations/Buckets = %d/%v, want 0/nil", ps.Revocations, ps.Buckets)
		}
	})

	t.Run("stages", func(t *testing.T) {
		o := keystoneops.Options{Tokens: 20, Weights: map[string]int{"validate": 60}}
		if ps := pressureStats(r, o, nil); ps.Stages != nil {
			t.Errorf("Stages = %+v for a run without stages, want nil", ps.Stages)
		}

		staged := *r
		staged.Stages = []pressure.StageResult{
			{Start: 0, End: time.Minute, Operations: opStats(map[string]int{"validate": 30})},
			{Start: time.Minute + time.Second, End: 3 * time.Minute, Operations: opStats(map[string]int{"validate": 20, "effective": 2})},
		}
		ps := pressureStats(&staged, o, []int{0, 500})
		want := []run.PressureStage{
			{Index: 1, FanAssignments: 0, Start: 0, End: time.Minute, Operations: staged.Stages[0].Operations},
			{Index: 2, FanAssignments: 500, Start: time.Minute + time.Second, End: 3 * time.Minute, Operations: staged.Stages[1].Operations},
		}
		if !reflect.DeepEqual(ps.Stages, want) {
			t.Errorf("Stages = %+v, want %+v", ps.Stages, want)
		}

		// A stage past the end of the targets has none.
		if ps := pressureStats(&staged, o, []int{0}); len(ps.Stages) != 2 || ps.Stages[1].FanAssignments != 0 {
			t.Errorf("Stages = %+v with one target, want two stages, the second without a target", ps.Stages)
		}
	})
}

func TestFinishKeystoneTeardownPressureNoCleanup(t *testing.T) {
	tests := []struct {
		name        string
		interrupted bool
		want        string
	}{
		{"completed run", false, "pressure complete; resources left in place — reclaim with: keystone cleanup --run run-run1234.json"},
		{"interrupted run", true, "pressure interrupted; resources left in place — reclaim with: keystone cleanup --run run-run1234.json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingKeystoneCleaner{}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)

			err := finishKeystoneTeardown(context.Background(), "pressure", cmd, c, "run1234", "run-run1234.json", nil, tc.interrupted, true, time.Second)
			if err != nil {
				t.Fatalf("finishKeystoneTeardown: %v", err)
			}
			if c.calls != 0 {
				t.Errorf("cleaner was called %d times; --no-cleanup must leave everything in place", c.calls)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("output %q missing %q", out.String(), tc.want)
			}
		})
	}
}

func TestKeystonePressureHelpCarriesLabWarning(t *testing.T) {
	out, err := execRoot(t, "keystone", "pressure", "--help")
	if err != nil {
		t.Fatalf("keystone pressure --help: %v", err)
	}
	for _, want := range []string{"never on a shared cloud", "real revocation", "flushes the token validation cache", "--mix", "--churn-users"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q", want)
		}
	}
	// The churn users come from the plan's regular users; a fan user, which
	// sits at the end of the plan, is never one.
	churn := newKeystonePressureCmd(&globalOptions{}).Flags().Lookup("churn-users")
	if churn == nil {
		t.Fatal("keystone pressure has no --churn-users flag")
	}
	if !strings.Contains(churn.Usage, "regular users") || !strings.Contains(churn.Usage, "never a fan user") {
		t.Errorf("--churn-users usage = %q, want it to name the plan's regular users and exclude the fan users", churn.Usage)
	}
}

// TestKeystonePressureRejectsOverflowingMix checks that a --mix or block weight
// whose sum overflows int fails before the identity client, with the engine's
// own message, instead of after a full provision and prime.
func TestKeystonePressureRejectsOverflowingMix(t *testing.T) {
	// Weights that sum past the largest int would be refused by pressure.Run
	// only after the plan is provisioned and the pool primed; the command
	// rejects them before the identity client.
	want := fmt.Sprintf("pressure mix: the weights sum past %d", math.MaxInt)
	t.Run("--mix", func(t *testing.T) {
		err := execPressure(t, keystonePressureScenarioYAML, "--mix", fmt.Sprintf("validate=%d", math.MaxInt))
		wantExactError(t, err, want)
	})
	t.Run("block mix", func(t *testing.T) {
		body := keystonePressureScenarioYAML + fmt.Sprintf("  mix: { validate: %d }\n", math.MaxInt)
		wantExactError(t, execPressure(t, body), want)
	})
}

// TestCheckPressureMix covers the pre-cloud mix check: a usable mix passes, an
// all-zero one is pressure.ErrNoWeight and an overflowing sum is rejected.
func TestCheckPressureMix(t *testing.T) {
	tests := []struct {
		name    string
		weights map[string]int
		wantErr string
	}{
		{"the default mix", defaultPressureMix(), ""},
		{"one positive weight", map[string]int{"revoke": 1}, ""},
		{"a sum of exactly the largest int", map[string]int{"validate": math.MaxInt - 1, "issue": 1}, ""},
		{"every weight 0", map[string]int{"validate": 0, "effective": 0, "revoke": 0, "grant_churn": 0, "issue": 0}, pressure.ErrNoWeight.Error()},
		{"a sum past the largest int", map[string]int{"validate": math.MaxInt, "issue": 1}, fmt.Sprintf("pressure mix: the weights sum past %d", math.MaxInt)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPressureMix(tc.weights)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("checkPressureMix(%v) = %v, want nil", tc.weights, err)
				}
				return
			}
			wantExactError(t, err, tc.wantErr)
		})
	}
	if err := checkPressureMix(map[string]int{"validate": 0}); !errors.Is(err, pressure.ErrNoWeight) {
		t.Errorf("checkPressureMix with no positive weight = %v, want pressure.ErrNoWeight", err)
	}
}

// pressureTestRunID is the run id runKeystonePressure runs under in the tests
// that drive it with fakePressureKeystone; its record is run-run1234.json.
const pressureTestRunID = "run1234"

// fakePressureKeystone is an in-memory pressureKeystone for driving
// runKeystonePressure without a cloud. It creates every resource with an id
// derived from its logical name, answers every pressure operation with
// success, lists nothing for the teardown's discovery, and records each
// Delete. assignErr, issueErr and validateErr, when set, fail every role
// assignment, every scoped token issue or every token validation; with
// assignOK positive, assignErr spares the first assignOK role assignments,
// so a test whose assignOK is the plan's assignment count fails only the
// grants made after provisioning. It counts the role assignments per user
// id. At its first Delete it notes whether the run record exists in the
// working directory, so a test can tell the record was written before the
// teardown began. The provisioning stages and the workers call it
// concurrently, so mu guards the fields below it.
type fakePressureKeystone struct {
	assignErr   error
	assignOK    int
	issueErr    error
	validateErr error

	// stopAfter, when positive, makes the fake record in drawn, in call
	// order, each validate, effective and revoke the workers run, and call
	// stop once it has recorded stopAfter of them, so a test that cancels
	// its run's context with stop ends the run after exactly that many. The
	// three are told apart by a call no other stage makes: ValidateToken,
	// ListEffectiveAssignments and RevokeToken, each the operation's first
	// Keystone call. grant_churn and issue share their calls with
	// provisioning, priming and teardown and are not recorded.
	stopAfter int
	stop      context.CancelFunc

	mu                 sync.Mutex
	issued             int
	assigns            int            // every AssignRole call
	assigned           map[string]int // AssignRole calls by user id
	deleted            []resource.Resource
	recordBeforeDelete bool
	drawn              []string
}

// drew records op as the next operation a worker ran and calls stop once
// stopAfter operations are recorded. It records nothing while stopAfter is 0
// or once stopAfter operations are recorded.
func (f *fakePressureKeystone) drew(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.drawn) >= f.stopAfter {
		return
	}
	f.drawn = append(f.drawn, op)
	if len(f.drawn) == f.stopAfter {
		f.stop()
	}
}

// drawnOps returns a copy of the operations drew recorded.
func (f *fakePressureKeystone) drawnOps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.drawn)
}

// created returns the resource the fake creates for logical.
func (f *fakePressureKeystone) created(kind resource.Kind, logical string) resource.Resource {
	return resource.Resource{Kind: kind, Logical: logical, ID: "id-" + logical, Name: "dizzy-" + pressureTestRunID + "-" + logical}
}

// CreateDomain creates the domain d with an id derived from its name.
func (f *fakePressureKeystone) CreateDomain(_ context.Context, d keystoneplan.Domain) (resource.Resource, error) {
	return f.created(keystone.KindDomain, d.Name), nil
}

// CreateRole creates the role r with an id derived from its name.
func (f *fakePressureKeystone) CreateRole(_ context.Context, r keystoneplan.Role) (resource.Resource, error) {
	return f.created(keystone.KindRole, r.Name), nil
}

// CreateProject creates the project p with an id derived from its name.
func (f *fakePressureKeystone) CreateProject(_ context.Context, p keystoneplan.Project, _ string) (resource.Resource, error) {
	return f.created(keystone.KindProject, p.Name), nil
}

// CreateUser creates the user u with an id derived from its name.
func (f *fakePressureKeystone) CreateUser(_ context.Context, u keystoneplan.User, _, _ string) (resource.Resource, error) {
	return f.created(keystone.KindUser, u.Name), nil
}

// AssignRole counts the call for userID and grants a role as keystone.Client
// does, or fails with assignErr once the first assignOK calls are made.
func (f *fakePressureKeystone) AssignRole(_ context.Context, a keystoneplan.Assignment, userID, projectID, domainID, roleID string) (resource.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assigns++
	if f.assigned == nil {
		f.assigned = make(map[string]int)
	}
	f.assigned[userID]++
	if f.assignErr != nil && f.assigns > f.assignOK {
		return resource.Resource{}, f.assignErr
	}
	return keystone.AssignmentResource(a, userID, projectID, domainID, roleID), nil
}

// assignCalls returns how many AssignRole calls the fake saw for userID.
func (f *fakePressureKeystone) assignCalls(userID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.assigned[userID]
}

// IssueToken completes the executor seam; Provision never calls it.
func (f *fakePressureKeystone) IssueToken(context.Context, keystoneplan.TokenIssue, string, string, string) error {
	return nil
}

// IssueScopedToken returns a fresh token valid for an hour, or fails with
// issueErr.
func (f *fakePressureKeystone) IssueScopedToken(context.Context, keystoneplan.TokenIssue, string, string, string) (keystone.IssuedToken, error) {
	if f.issueErr != nil {
		return keystone.IssuedToken{}, f.issueErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued++
	return keystone.IssuedToken{ID: fmt.Sprintf("token-%d", f.issued), ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// ValidateToken records a validate and accepts every token, or fails with
// validateErr.
func (f *fakePressureKeystone) ValidateToken(context.Context, string) error {
	f.drew(keystoneops.OpValidate)
	return f.validateErr
}

// RevokeToken records a revoke and revokes every token.
func (f *fakePressureKeystone) RevokeToken(context.Context, string) error {
	f.drew(keystoneops.OpRevoke)
	return nil
}

// ListEffectiveAssignments records an effective and lists one assignment for
// every user.
func (f *fakePressureKeystone) ListEffectiveAssignments(context.Context, string) (int, error) {
	f.drew(keystoneops.OpEffective)
	return 1, nil
}

// ListUserProjects lists one project for every user.
func (f *fakePressureKeystone) ListUserProjects(context.Context, string) (int, error) { return 1, nil }

// ListAvailableProjects lists one project for every token.
func (f *fakePressureKeystone) ListAvailableProjects(context.Context, string) (int, error) {
	return 1, nil
}

// ListProjectsByTag discovers no project, so the teardown deletes the
// recorded ones and the leak check finds none.
func (f *fakePressureKeystone) ListProjectsByTag(context.Context, string) ([]resource.Resource, error) {
	return nil, nil
}

// ListUsersByPrefix discovers no user.
func (f *fakePressureKeystone) ListUsersByPrefix(context.Context, string) ([]resource.Resource, error) {
	return nil, nil
}

// ListRolesByPrefix discovers no role.
func (f *fakePressureKeystone) ListRolesByPrefix(context.Context, string) ([]resource.Resource, error) {
	return nil, nil
}

// ListDomainsByPrefix discovers no domain.
func (f *fakePressureKeystone) ListDomainsByPrefix(context.Context, string) ([]resource.Resource, error) {
	return nil, nil
}

// ListAssignmentsForUser discovers no assignment.
func (f *fakePressureKeystone) ListAssignmentsForUser(context.Context, string) ([]resource.Resource, error) {
	return nil, nil
}

// DisableDomain disables every domain.
func (f *fakePressureKeystone) DisableDomain(context.Context, resource.Resource) error { return nil }

// Delete records r and, at the first call, whether the run record exists.
func (f *fakePressureKeystone) Delete(_ context.Context, r resource.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deleted) == 0 {
		_, err := os.Stat("run-" + pressureTestRunID + ".json")
		f.recordBeforeDelete = err == nil
	}
	f.deleted = append(f.deleted, r)
	return nil
}

// deletedIDs returns how often the fake deleted each resource id.
func (f *fakePressureKeystone) deletedIDs() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make(map[string]int, len(f.deleted))
	for _, r := range f.deleted {
		ids[r.ID]++
	}
	return ids
}

// pressureTestPlan generates the plan of sampleKeystoneScenarioYAML.
func pressureTestPlan(t *testing.T) *keystoneplan.Plan {
	t.Helper()
	return generateTestPlan(t, sampleKeystoneScenarioYAML)
}

// generateTestPlan generates the plan of the scenario body.
func generateTestPlan(t *testing.T, body string) *keystoneplan.Plan {
	t.Helper()
	s, err := keystonescenario.Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p, err := s.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return p
}

// fanTestUser is the fan user of fanScenarioYAML, and fanTestUserID the id
// fakePressureKeystone creates it with.
const (
	fanTestUser   = "fan-0001"
	fanTestUserID = "id-" + fanTestUser
)

// fanPressureTestPlan generates the plan of fanScenarioYAML and fails the test
// unless it has the shape the staged runs rely on: five projects, three
// regular users and fanTestUser holding two grants.
func fanPressureTestPlan(t *testing.T) *keystoneplan.Plan {
	t.Helper()
	p := generateTestPlan(t, fanScenarioYAML)
	if len(p.Projects) != 5 || len(p.Users) != 4 || p.FanUsers() != 1 || p.Users[3].Name != fanTestUser || fanTestGrants(p) != 2 {
		t.Fatalf("the fan plan has %d projects, users %+v and %d grants of %s; want 5 projects, 3 regular users and %s with 2 grants",
			len(p.Projects), p.Users, fanTestGrants(p), fanTestUser, fanTestUser)
	}
	return p
}

// fanTestGrants returns the number of plan assignments of fanTestUser.
func fanTestGrants(p *keystoneplan.Plan) int {
	n := 0
	for _, a := range p.Assignments {
		if a.User == fanTestUser {
			n++
		}
	}
	return n
}

// pressureTestRun is a pressureRun over p that passes every pre-cloud check: a
// 20ms bounded run of two workers over a pool of four tokens, one churn user
// and the default mix, under pressureTestRunID. Its cfg leaves the seed, the
// classifier and the checkpoint interval zero, as mergePressureConfig does,
// so a run completes, draws from the plan's seed and labels its errors as
// keystoneops does only if runKeystonePressure sets them.
func pressureTestRun(t *testing.T, p *keystoneplan.Plan) pressureRun {
	t.Helper()
	r := pressureRun{
		runID: pressureTestRunID,
		plan:  p,
		cfg: pressure.Config{
			Duration:    20 * time.Millisecond,
			BucketWidth: defaultChaosBucketWidth,
			Workers:     2,
			OpTimeout:   time.Second,
		},
		o:           keystoneops.Options{Tokens: 4, ChurnUsers: 1, Weights: defaultPressureMix(), OpTimeout: time.Second},
		concurrency: 2,
		opTimeout:   time.Second,
		collector:   metrics.NewCollector(),
	}
	if err := r.cfg.Validate(); err != nil {
		t.Fatalf("the test run's config is invalid: %v", err)
	}
	if err := checkPressureMix(r.o.Weights); err != nil {
		t.Fatalf("the test run's mix is invalid: %v", err)
	}
	return r
}

// stagedPressureTestRun is pressureTestRun over p as a 60ms run of two 30ms
// stages: the first without a fan_assignments target, the second growing the
// fan user to four grants.
func stagedPressureTestRun(t *testing.T, p *keystoneplan.Plan) pressureRun {
	t.Helper()
	r := pressureTestRun(t, p)
	r.cfg.Duration = 60 * time.Millisecond
	r.cfg.Stages = []pressure.Stage{{Duration: 30 * time.Millisecond}, {Duration: 30 * time.Millisecond}}
	r.stages = []int{0, 4}
	if err := r.cfg.Validate(); err != nil {
		t.Fatalf("the staged test run's config is invalid: %v", err)
	}
	return r
}

// loadPressureTestRecord loads the run record runKeystonePressure wrote to
// the working directory.
func loadPressureTestRecord(t *testing.T) *run.Record {
	t.Helper()
	rec, err := run.Load("run-" + pressureTestRunID + ".json")
	if err != nil {
		t.Fatalf("loading the run record: %v", err)
	}
	return rec
}

// TestRunKeystonePressureCompletes drives a short run against the fake: it
// writes a record with pressure statistics, prints the Pressure table, tears
// down every created resource and reports no leak. The run's config carries no
// checkpoint interval, so the engine would refuse it unless
// runKeystonePressure sets one.
func TestRunKeystonePressureCompletes(t *testing.T) {
	t.Chdir(t.TempDir())
	p := pressureTestPlan(t)
	f := &fakePressureKeystone{}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runKeystonePressure(context.Background(), cmd, f, pressureTestRun(t, p)); err != nil {
		t.Fatalf("runKeystonePressure: %v", err)
	}

	rec := loadPressureTestRecord(t)
	if rec.RunID != pressureTestRunID || rec.Service != "keystone" || rec.Scenario != p.Scenario || rec.Seed != p.Seed {
		t.Errorf("record RunID/Service/Scenario/Seed = %q/%q/%q/%d, want %q/keystone/%q/%d",
			rec.RunID, rec.Service, rec.Scenario, rec.Seed, pressureTestRunID, p.Scenario, p.Seed)
	}
	if rec.Error != "" || rec.Incomplete {
		t.Errorf("record Error/Incomplete = %q/%v, want empty/false for a completed run", rec.Error, rec.Incomplete)
	}
	wantCreated := len(p.Domains) + len(p.Roles) + len(p.Projects) + len(p.Users) + len(p.Assignments)
	if len(rec.Created) != wantCreated {
		t.Errorf("record lists %d created resources, want %d", len(rec.Created), wantCreated)
	}
	ps := rec.Pressure
	if ps == nil {
		t.Fatal("record has no pressure statistics")
	}
	if ps.Workers != 2 || ps.Tokens != 4 || ps.ChurnUsers != 1 {
		t.Errorf("pressure Workers/Tokens/ChurnUsers = %d/%d/%d, want 2/4/1", ps.Workers, ps.Tokens, ps.ChurnUsers)
	}
	if want := pressureMix(defaultPressureMix()); !reflect.DeepEqual(ps.Mix, want) {
		t.Errorf("pressure Mix = %+v, want %+v", ps.Mix, want)
	}
	if len(ps.Buckets) != 10 {
		t.Errorf("a bounded run recorded %d buckets, want 10", len(ps.Buckets))
	}

	// The teardown deleted every created resource; grant_churn may have
	// deleted assignments during the run as well.
	deleted := f.deletedIDs()
	for _, r := range rec.Created {
		if deleted[r.ID] == 0 {
			t.Errorf("created %s %q was not deleted", r.Kind, r.ID)
		}
	}
	for _, want := range []string{
		"\nPressure\n",
		"run record written to run-" + pressureTestRunID + ".json",
		fmt.Sprintf("deleted %d resource(s) for run %s", wantCreated, pressureTestRunID),
		"leak check: no run-named resources remain",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q missing %q", out.String(), want)
		}
	}
}

// TestRunKeystonePressureSetupFailures proves that every failure after the
// first write, in provisioning, Build, Prime or the engine, writes a record
// whose error names the failure and lists what was created, tears that down
// after the record exists (or leaves it with --no-cleanup), and returns
// "<what> (run <id>): <cause>".
func TestRunKeystonePressureSetupFailures(t *testing.T) {
	p := pressureTestPlan(t)
	roots := len(p.Domains) + len(p.Roles) + len(p.Projects) + len(p.Users)
	all := roots + len(p.Assignments)
	forbidden := fmt.Errorf("assigning role: %w", keystone.ErrForbidden)
	issueErr := errors.New("issue boom")

	tests := []struct {
		name        string
		assignErr   error
		issueErr    error
		adjust      func(*pressureRun)
		noCleanup   bool
		what        string
		wantCause   string // the start of the cause the record and the error carry
		wantIs      error
		wantCreated int
	}{
		{
			name: "a 403 while provisioning", assignErr: forbidden,
			what: "provisioning plan", wantCause: "", wantIs: keystone.ErrForbidden, wantCreated: roots,
		},
		{
			name:   "a Build error",
			adjust: func(r *pressureRun) { r.o.ChurnUsers = len(r.plan.Users) },
			what:   "building pressure set",
			wantCause: fmt.Sprintf("pressure.churn_users (%d) must leave at least one user outside the churn set; the plan has %d users",
				len(p.Users), len(p.Users)),
			wantCreated: all,
		},
		{
			name: "a Prime error", issueErr: issueErr,
			what: "starting pressure run", wantCause: "priming token pool (", wantIs: issueErr, wantCreated: all,
		},
		{
			name:   "an engine error",
			adjust: func(r *pressureRun) { r.cfg.Workers = 0 },
			what:   "running pressure", wantCause: "pressure workers must be between 1 and 1024, got 0", wantCreated: all,
		},
		{
			name: "a 403 while provisioning with --no-cleanup", assignErr: forbidden, noCleanup: true,
			what: "provisioning plan", wantCause: "", wantIs: keystone.ErrForbidden, wantCreated: roots,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			f := &fakePressureKeystone{assignErr: tc.assignErr, issueErr: tc.issueErr}
			r := pressureTestRun(t, p)
			r.noCleanup = tc.noCleanup
			if tc.adjust != nil {
				tc.adjust(&r)
			}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)

			err := runKeystonePressure(context.Background(), cmd, f, r)
			if err == nil {
				t.Fatal("runKeystonePressure returned nil; a failed setup must exit non-zero")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tc.wantIs)
			}

			// The record names the failure: its error is the cause the
			// returned error carries after "<what> (run <id>): ".
			rec := loadPressureTestRecord(t)
			prefix := tc.what + " (run " + pressureTestRunID + "): "
			if err.Error() != prefix+rec.Error {
				t.Errorf("error = %q, want %q followed by the record's error %q", err.Error(), prefix, rec.Error)
			}
			if rec.Error == "" || !strings.HasPrefix(rec.Error, tc.wantCause) {
				t.Errorf("record error = %q, want one starting with %q", rec.Error, tc.wantCause)
			}
			if rec.RunID != pressureTestRunID || rec.Service != "keystone" || rec.Scenario != p.Scenario || rec.Seed != p.Seed {
				t.Errorf("record RunID/Service/Scenario/Seed = %q/%q/%q/%d, want %q/keystone/%q/%d",
					rec.RunID, rec.Service, rec.Scenario, rec.Seed, pressureTestRunID, p.Scenario, p.Seed)
			}
			if rec.Pressure != nil {
				t.Error("a failed setup recorded pressure statistics")
			}
			if len(rec.Created) != tc.wantCreated {
				t.Errorf("record lists %d created resources, want %d", len(rec.Created), tc.wantCreated)
			}

			deleted := f.deletedIDs()
			if tc.noCleanup {
				if len(deleted) != 0 {
					t.Errorf("--no-cleanup deleted %d resources, want none", len(deleted))
				}
				want := "pressure setup failed; resources left in place — reclaim with: keystone cleanup --run run-" + pressureTestRunID + ".json"
				if !strings.Contains(out.String(), want) {
					t.Errorf("output %q missing %q", out.String(), want)
				}
				return
			}
			if !f.recordBeforeDelete {
				t.Error("the run record was not written before teardown began")
			}
			if len(deleted) != len(rec.Created) {
				t.Errorf("teardown deleted %d resources, want the %d the record lists", len(deleted), len(rec.Created))
			}
			for _, r := range rec.Created {
				if deleted[r.ID] != 1 {
					t.Errorf("created %s %q deleted %d times, want once", r.Kind, r.ID, deleted[r.ID])
				}
			}
			if want := fmt.Sprintf("deleted %d resource(s) for run %s", tc.wantCreated, pressureTestRunID); !strings.Contains(out.String(), want) {
				t.Errorf("output %q missing %q", out.String(), want)
			}
		})
	}
}

// TestRunKeystonePressureStages drives a staged run against the fake: the
// second stage grows the fan user from its two plan grants to four, the
// record lists both stages and the two stage grants among the created
// resources, the output carries the Stages table, and the teardown removes
// the stage grants with everything else. The caller's stages keep their nil
// Enter.
func TestRunKeystonePressureStages(t *testing.T) {
	t.Chdir(t.TempDir())
	p := fanPressureTestPlan(t)
	f := &fakePressureKeystone{}
	r := stagedPressureTestRun(t, p)
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runKeystonePressure(context.Background(), cmd, f, r); err != nil {
		t.Fatalf("runKeystonePressure: %v", err)
	}
	for i, st := range r.cfg.Stages {
		if st.Enter != nil {
			t.Errorf("the caller's stage %d got an Enter; runKeystonePressure must set it on a copy", i+1)
		}
	}

	rec := loadPressureTestRecord(t)
	if rec.Error != "" || rec.Incomplete {
		t.Errorf("record Error/Incomplete = %q/%v, want empty/false for a completed run", rec.Error, rec.Incomplete)
	}
	if rec.Pressure == nil {
		t.Fatal("record has no pressure statistics")
	}
	stages := rec.Pressure.Stages
	if len(stages) != 2 {
		t.Fatalf("record has %d stages, want 2: %+v", len(stages), stages)
	}
	for i, st := range stages {
		if st.Index != i+1 {
			t.Errorf("stage %d index = %d, want %d", i+1, st.Index, i+1)
		}
		if st.Start >= st.End {
			t.Errorf("stage %d start %s is not before its end %s", i+1, st.Start, st.End)
		}
	}
	if stages[0].FanAssignments != 0 || stages[1].FanAssignments != 4 {
		t.Errorf("stage fanAssignments = %d, %d; want 0, 4", stages[0].FanAssignments, stages[1].FanAssignments)
	}

	// In the record file the stage without a target has no fanAssignments.
	data, err := os.ReadFile("run-" + pressureTestRunID + ".json")
	if err != nil {
		t.Fatalf("reading the run record: %v", err)
	}
	var raw struct {
		Pressure struct {
			Stages []map[string]json.RawMessage `json:"stages"`
		} `json:"pressure"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decoding the run record: %v", err)
	}
	if len(raw.Pressure.Stages) != 2 {
		t.Fatalf("the record file has %d stages, want 2", len(raw.Pressure.Stages))
	}
	if fan, ok := raw.Pressure.Stages[0]["fanAssignments"]; ok {
		t.Errorf("stage 1 has fanAssignments %s in the record file, want it omitted", fan)
	}
	if fan := string(raw.Pressure.Stages[1]["fanAssignments"]); fan != "4" {
		t.Errorf("stage 2 has fanAssignments %q in the record file, want 4", fan)
	}

	// The record lists the plan's resources and the two stage grants.
	roots := len(p.Domains) + len(p.Roles) + len(p.Projects) + len(p.Users)
	wantCreated := roots + len(p.Assignments) + 2
	assignments := 0
	for _, res := range rec.Created {
		if res.Kind == keystone.KindAssignment {
			assignments++
		}
	}
	if len(rec.Created) != wantCreated || assignments != len(p.Assignments)+2 {
		t.Errorf("record lists %d created resources, %d of them role assignments; want %d and %d",
			len(rec.Created), assignments, wantCreated, len(p.Assignments)+2)
	}
	// Provisioning made one AssignRole call per plan grant of the fan user;
	// every later call for it is a stage grant.
	if got := f.assignCalls(fanTestUserID) - fanTestGrants(p); got != 2 {
		t.Errorf("the fake saw %d AssignRole calls for %s after provisioning, want 2", got, fanTestUser)
	}

	// The teardown deleted every created resource; grant_churn may have
	// deleted the churn user's assignments during the run as well.
	deleted := f.deletedIDs()
	for _, res := range rec.Created {
		if deleted[res.ID] == 0 {
			t.Errorf("created %s %q was not deleted", res.Kind, res.ID)
		}
	}
	if want := fmt.Sprintf("deleted %d resource(s) for run %s", wantCreated, pressureTestRunID); !strings.Contains(out.String(), want) {
		t.Errorf("output %q missing %q", out.String(), want)
	}

	// STAGE and FAN are right-aligned, so the second stage's rows are found
	// by their fields.
	if !strings.Contains(out.String(), "\nStages\n") {
		t.Fatalf("output %q has no Stages table", out.String())
	}
	found := false
	for _, line := range strings.Split(out.String(), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "2" && fields[1] == "4" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("output %q has no row of stage 2 with fan assignments 4", out.String())
	}
}

// TestRunKeystonePressureStageGrantFails proves that a failed stage entry
// ends the run as a setup failure: the record's error names the stage, the
// fan user and the grants made before the failure, the record has no
// pressure statistics but lists those grants, the command returns "running
// pressure (run <id>): " and the record's error, and the teardown removes
// everything created, those grants included. grant_churn is off, so the
// only role assignments after provisioning are the stage's and assignOK
// counts them alone.
func TestRunKeystonePressureStageGrantFails(t *testing.T) {
	p := fanPressureTestPlan(t)
	grantErr := errors.New("grant boom")
	roots := len(p.Domains) + len(p.Roles) + len(p.Projects) + len(p.Users)

	for _, granted := range []int{0, 1} {
		t.Run(fmt.Sprintf("%d stage grant(s) before the failure", granted), func(t *testing.T) {
			t.Chdir(t.TempDir())
			f := &fakePressureKeystone{assignErr: grantErr, assignOK: len(p.Assignments) + granted}
			r := stagedPressureTestRun(t, p)
			r.o.Weights[keystoneops.OpGrantChurn] = 0
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)

			err := runKeystonePressure(context.Background(), cmd, f, r)
			if err == nil {
				t.Fatal("runKeystonePressure returned nil; a failed stage entry must exit non-zero")
			}
			if !errors.Is(err, grantErr) {
				t.Errorf("error %v does not wrap the grant error", err)
			}

			rec := loadPressureTestRecord(t)
			wantCause := fmt.Sprintf("entering stage 2: growing fan user %q to 4 assignments (%d granted): ", fanTestUser, granted)
			if !strings.HasPrefix(rec.Error, wantCause) {
				t.Errorf("record error = %q, want one starting with %q", rec.Error, wantCause)
			}
			if want := "running pressure (run " + pressureTestRunID + "): " + rec.Error; err.Error() != want {
				t.Errorf("error = %q, want %q", err.Error(), want)
			}
			if rec.Pressure != nil {
				t.Error("a failed stage entry recorded pressure statistics")
			}
			wantCreated := roots + len(p.Assignments) + granted
			if len(rec.Created) != wantCreated {
				t.Errorf("record lists %d created resources, want %d", len(rec.Created), wantCreated)
			}

			if !f.recordBeforeDelete {
				t.Error("the run record was not written before teardown began")
			}
			deleted := f.deletedIDs()
			if len(deleted) != len(rec.Created) {
				t.Errorf("teardown deleted %d resources, want the %d the record lists", len(deleted), len(rec.Created))
			}
			for _, res := range rec.Created {
				if deleted[res.ID] != 1 {
					t.Errorf("created %s %q deleted %d times, want once", res.Kind, res.ID, deleted[res.ID])
				}
			}
			if want := fmt.Sprintf("deleted %d resource(s) for run %s", wantCreated, pressureTestRunID); !strings.Contains(out.String(), want) {
				t.Errorf("output %q missing %q", out.String(), want)
			}
		})
	}
}

// pressureDrawLen is how many validate, effective and revoke operations a run
// of runPressureDraws records before the fake ends it.
const pressureDrawLen = 24

// runPressureDraws drives an unbounded run of one worker over p with weights
// against f, which records the operations the worker runs and cancels the
// run's context once it has recorded pressureDrawLen of them, so the run ends
// after the same operations however fast the machine is. It runs in a fresh
// working directory and returns the recorded operations in order and the run
// record.
func runPressureDraws(t *testing.T, p *keystoneplan.Plan, f *fakePressureKeystone, weights map[string]int) ([]string, *run.Record) {
	t.Helper()
	t.Chdir(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.stopAfter, f.stop = pressureDrawLen, cancel

	r := pressureTestRun(t, p)
	r.cfg.Workers = 1
	r.cfg.Duration, r.cfg.Unbounded = 0, true
	r.o.Weights = weights
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)

	if err := runKeystonePressure(ctx, cmd, f, r); err != nil {
		t.Fatalf("runKeystonePressure: %v", err)
	}
	drawn := f.drawnOps()
	if len(drawn) != pressureDrawLen {
		t.Fatalf("the run recorded %d operations, want %d", len(drawn), pressureDrawLen)
	}
	return drawn, loadPressureTestRecord(t)
}

// TestRunKeystonePressureDrawsFromThePlanSeed proves the engine draws from the
// plan's seed: with one worker, two runs of the same plan run the same
// operations in the same order, and a run of the plan under another seed runs
// another order. Only validate, effective and revoke carry a weight, the three
// the fake records, so the recorded order is the order the worker drew.
func TestRunKeystonePressureDrawsFromThePlanSeed(t *testing.T) {
	p := pressureTestPlan(t)
	reseeded := *p
	reseeded.Seed = p.Seed + 1
	weights := map[string]int{keystoneops.OpValidate: 1, keystoneops.OpEffective: 1, keystoneops.OpRevoke: 1}

	first, rec := runPressureDraws(t, p, &fakePressureKeystone{}, weights)
	if rec.Seed != p.Seed {
		t.Errorf("record seed = %d, want the plan seed %d", rec.Seed, p.Seed)
	}
	again, _ := runPressureDraws(t, p, &fakePressureKeystone{}, weights)
	other, _ := runPressureDraws(t, &reseeded, &fakePressureKeystone{}, weights)

	if !slices.Equal(first, again) {
		t.Errorf("two runs with plan seed %d ran\n%v\nand\n%v\nwant the same order", p.Seed, first, again)
	}
	if slices.Equal(first, other) {
		t.Errorf("runs with plan seeds %d and %d both ran %v; the workers must draw from the plan seed", p.Seed, reseeded.Seed, first)
	}
}

// TestRunKeystonePressureClassifiesWithKeystoneops proves the engine labels
// operation errors with keystoneops.Classify: a 404 on every token validation
// shows up in the record's pressure errors as "not-found", where the engine's
// default classifier would have said "other".
func TestRunKeystonePressureClassifiesWithKeystoneops(t *testing.T) {
	p := pressureTestPlan(t)
	notFound := gophercloud.ErrUnexpectedResponseCode{Method: http.MethodGet, URL: "auth/tokens", Expected: []int{http.StatusOK}, Actual: http.StatusNotFound}
	if got := keystoneops.Classify(notFound); got != "not-found" {
		t.Fatalf("keystoneops.Classify(404) = %q, want not-found", got)
	}
	f := &fakePressureKeystone{validateErr: notFound}

	drawn, rec := runPressureDraws(t, p, f, defaultPressureMix())
	if !slices.Contains(drawn, keystoneops.OpValidate) {
		t.Fatalf("the run ran no validate: %v", drawn)
	}
	if rec.Pressure == nil {
		t.Fatal("record has no pressure statistics")
	}
	kinds := make(map[string]int, len(rec.Pressure.Operations.Errors))
	for _, e := range rec.Pressure.Operations.Errors {
		kinds[e.Kind] = e.Count
	}
	if kinds["not-found"] == 0 {
		t.Errorf("record pressure errors = %+v, want the failed validations under not-found", rec.Pressure.Operations.Errors)
	}
	if kinds["other"] != 0 {
		t.Errorf("record pressure errors = %+v; %d error(s) carry the engine's default label other", rec.Pressure.Operations.Errors, kinds["other"])
	}
}

// stubRoundTripper is an http.RoundTripper that is not an *http.Transport,
// the kind of transport sizeConnectionPool cannot size.
type stubRoundTripper struct{ name string }

// RoundTrip fails every request; sizeConnectionPool sends none.
func (s *stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("%s: unexpected request", s.name)
}

// identityClientWith returns a service client whose HTTP client uses rt, the
// default transport when rt is nil.
func identityClientWith(rt http.RoundTripper) *gophercloud.ServiceClient {
	return &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: rt}}}
}

// TestSizeConnectionPool proves sizeConnectionPool sizes a clone of the
// transport it finds and never the transport itself: http.DefaultTransport
// for a nil transport, or an *http.Transport another client may share.
func TestSizeConnectionPool(t *testing.T) {
	const conns = 128

	t.Run("a nil transport gets a sized clone of the default transport", func(t *testing.T) {
		def, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			t.Fatalf("http.DefaultTransport is a %T", http.DefaultTransport)
		}
		perHost, idle := def.MaxIdleConnsPerHost, def.MaxIdleConns
		gc := identityClientWith(nil)

		sizeConnectionPool(gc, conns)

		got, ok := gc.HTTPClient.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("transport = %T, want *http.Transport", gc.HTTPClient.Transport)
		}
		if got == def {
			t.Error("sizeConnectionPool installed http.DefaultTransport itself, want a clone")
		}
		if got.MaxIdleConnsPerHost != conns {
			t.Errorf("MaxIdleConnsPerHost = %d, want %d", got.MaxIdleConnsPerHost, conns)
		}
		if got.MaxIdleConns != 0 && got.MaxIdleConns < conns {
			t.Errorf("MaxIdleConns = %d, want at least %d", got.MaxIdleConns, conns)
		}
		if got.ForceAttemptHTTP2 != def.ForceAttemptHTTP2 {
			t.Errorf("ForceAttemptHTTP2 = %v, want the default transport's %v", got.ForceAttemptHTTP2, def.ForceAttemptHTTP2)
		}
		if def.MaxIdleConnsPerHost != perHost || def.MaxIdleConns != idle {
			t.Errorf("http.DefaultTransport MaxIdleConnsPerHost/MaxIdleConns = %d/%d, want them unchanged at %d/%d",
				def.MaxIdleConnsPerHost, def.MaxIdleConns, perHost, idle)
		}
	})

	t.Run("an *http.Transport gets a sized clone with its TLS settings", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			idle     int
			wantIdle int
		}{
			{"MaxIdleConns below conns is raised", 100, conns},
			{"a larger MaxIdleConns is kept", 500, 500},
			{"MaxIdleConns 0, no limit, is kept", 0, 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tlsConfig := &tls.Config{ServerName: "keystone.example", RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}
				orig := &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true, MaxIdleConns: tc.idle}
				gc := identityClientWith(orig)

				sizeConnectionPool(gc, conns)

				got, ok := gc.HTTPClient.Transport.(*http.Transport)
				if !ok {
					t.Fatalf("transport = %T, want *http.Transport", gc.HTTPClient.Transport)
				}
				if got == orig {
					t.Error("the client kept the original transport, want a sized clone of it")
				}
				if got.MaxIdleConnsPerHost != conns || got.MaxIdleConns != tc.wantIdle {
					t.Errorf("MaxIdleConnsPerHost/MaxIdleConns = %d/%d, want %d/%d", got.MaxIdleConnsPerHost, got.MaxIdleConns, conns, tc.wantIdle)
				}
				c := got.TLSClientConfig
				if c == nil || c.ServerName != tlsConfig.ServerName || c.RootCAs != tlsConfig.RootCAs || c.MinVersion != tlsConfig.MinVersion {
					t.Errorf("TLSClientConfig = %+v, want the original's server name, root CAs and minimum version", c)
				}
				if !got.ForceAttemptHTTP2 {
					t.Error("the clone lost ForceAttemptHTTP2")
				}
				if orig.MaxIdleConnsPerHost != 0 || orig.MaxIdleConns != tc.idle || orig.TLSClientConfig != tlsConfig {
					t.Errorf("original MaxIdleConnsPerHost/MaxIdleConns = %d/%d, want it unchanged at 0/%d", orig.MaxIdleConnsPerHost, orig.MaxIdleConns, tc.idle)
				}
			})
		}
	})

	t.Run("another RoundTripper is left alone", func(t *testing.T) {
		rt := &stubRoundTripper{name: "stub"}
		gc := identityClientWith(rt)

		sizeConnectionPool(gc, conns)

		if got, ok := gc.HTTPClient.Transport.(*stubRoundTripper); !ok || got != rt {
			t.Errorf("transport = %T %p, want the original %p", gc.HTTPClient.Transport, gc.HTTPClient.Transport, rt)
		}
	})
}
